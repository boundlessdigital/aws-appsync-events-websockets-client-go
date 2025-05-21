package appsyncwsclient

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	signer "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"nhooyr.io/websocket"
)

// Client manages the WebSocket connection and communication with AWS AppSync.
const (
	defaultOperationTimeout = 30 * time.Second
)

type Client struct {
	options      ClientOptions
	conn         *websocket.Conn
	connCtx      context.Context
	connCancel   context.CancelFunc
	signer       Signer // AWS SigV4 signer or mock
	mu           sync.RWMutex // Protects operations map and connection state
	operations   map[string]*operation
	isConnecting bool
	isConnected  bool
	// lastKeepAliveTime was removed as it was unused; keepAliveInterval is used by manageKeepAlive
	keepAliveInterval time.Duration
	send_message_override func(ctx context.Context, msg Message) error // For testing
	kickKeepAlive     chan struct{} // To reset keep-alive timer on any message
}

// NewClient creates a new AppSync WebSocket client.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.AppSyncAPIURL == "" || opts.RealtimeServiceURL == "" {
		return nil, fmt.Errorf("AppSyncAPIURL and RealtimeServiceURL must be provided")
	}
	if opts.ConnectionInitPayload == nil {
		opts.ConnectionInitPayload = make(map[string]interface{}) // Default to empty JSON object {}
	}

	if opts.KeepAliveInterval <= 0 {
		opts.KeepAliveInterval = 5 * time.Minute // Default keep-alive interval
	}
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = opts.KeepAliveInterval + 30*time.Second // Default read timeout slightly longer than keep-alive
	}

	c := &Client{
		options:    opts,
		signer:     signer.NewSigner(),
		operations: make(map[string]*operation),
		kickKeepAlive: make(chan struct{}, 1), // Buffered to prevent blocking on send
	}
	return c, nil
}

func (c *Client) logf(format string, v ...interface{}) {
	if c.options.Debug {
		log.Printf("[AppSyncWSClient-DEBUG] "+format, v...)
	}
}



func (c *Client) generate_operation_id() string {
	// Placeholder - In a real application, use a more robust UUID generator.
	// For now, simple timestamp-based ID.
	return fmt.Sprintf("op-%d", time.Now().UnixNano())
}


// isErrorType checks if the given message type is a known server-sent error type.
func isErrorType(msgType string) bool {
	switch msgType {
	case MsgTypeSubscribeError,
		MsgTypeUnsubscribeError,
		MsgTypeServerPublishError, // Corresponds to "publish_error" from server
		MsgTypeError,
		MsgTypeConnectionError,
		MsgTypeBroadcastError:
		return true
	default:
		return false
	}
}

// handleOperationMessage processes messages related to a specific ongoing operation.
// The client's mutex (c.mu) must be held by the caller.
func (c *Client) handleOperationMessage(op *operation, msg Message) {
	switch msg.Type {
	case MsgTypeSubscribeSuccess, MsgTypePublishSuccess, MsgTypeUnsubscribeSuccess:
		if op.ackCh != nil {
			op.ackCh <- msg
		}
		// For subscribe_success, data handling is separate via op.dataChannel.
		// For publish/unsubscribe, the operation is typically done after ack.
		if msg.Type != MsgTypeSubscribeSuccess {
			delete(c.operations, *msg.ID) // Clean up completed non-subscription operation
		}
	case MsgTypeData:
		if op.dataChannel != nil {
			op.dataChannel <- msg // Send raw data message
		}
	case MsgTypeSubscribeError, MsgTypeUnsubscribeError, MsgTypeServerPublishError, MsgTypeError, MsgTypeBroadcastError: // Errors specific to an operation
		if op.errorCh != nil {
			select { // Non-blocking send
			case op.errorCh <- msg:
			default:
				c.logf("Failed to send error to op.errorCh for %s, channel likely closed or full.", DerefString(msg.ID))
			}
		}
		// Call OnSubscriptionError callback if it's a subscription-related error
		if c.options.OnSubscriptionError != nil && len(msg.Errors) > 0 {
			isSubRelatedError := msg.Type == string(MsgTypeSubscribeError) ||
				msg.Type == string(MsgTypeBroadcastError) ||
				(msg.Type == string(MsgTypeError) && op.MessageType == MsgTypeSubscribe)
			if isSubRelatedError {
				c.options.OnSubscriptionError(*msg.ID, msg.Errors[0])
			}
		}
		delete(c.operations, *msg.ID) // Clean up failed/errored operation
	default:
		if isErrorType(msg.Type) {
			c.logf("Received unhandled ERROR message type %s for operation ID %s. Payload: %+v", msg.Type, DerefString(msg.ID), msg.Errors)
			if op.errorCh != nil {
				select { // Non-blocking send
				case op.errorCh <- msg:
				default:
					c.logf("Failed to send unhandled error to op.errorCh for %s, channel likely closed or full.", DerefString(msg.ID))
				}
			}
			if c.options.OnSubscriptionError != nil && op.MessageType == MsgTypeSubscribe && len(msg.Errors) > 0 {
				c.options.OnSubscriptionError(*msg.ID, msg.Errors[0]) // Report as subscription error if it was a subscribe op
			}
			delete(c.operations, *msg.ID) // Clean up operation
		} else {
			c.logf("Received unhandled NON-ERROR message type %s for operation ID %s", msg.Type, DerefString(msg.ID))
		}
	}
}

// handleNonOperationMessage processes messages not tied to a specific client operation.
// The client's mutex (c.mu) must be held by the caller.
func (c *Client) handleNonOperationMessage(msg Message) {
	switch msg.Type {
	case MsgTypeConnectionAck:
		c.logf("Connection Acknowledged by AppSync.")
		if msg.ConnectionTimeoutMs != nil {
			// Store the server-suggested keepAliveInterval. manageKeepAlive will use this.
			c.keepAliveInterval = time.Duration(*msg.ConnectionTimeoutMs) * time.Millisecond
			c.logf("Server suggested keep-alive interval updated to: %v. manageKeepAlive will pick this up on next activity.", c.keepAliveInterval)
		}
		if c.options.OnConnectionAck != nil {
			c.options.OnConnectionAck(msg)
		}
	case MsgTypeKeepAlive:
		c.logf("Keep-alive received.")
		// Actual timer reset for sending keep-alives is handled by manageKeepAlive via kickKeepAlive,
		// which is triggered in handleIncomingMessages for any message.
		if c.options.OnKeepAlive != nil {
			c.options.OnKeepAlive()
		}
	case MsgTypeConnectionError, MsgTypeError: // Generic errors not tied to an ID
		c.logf("Received generic error from AppSync: %v", msg.Errors)
		if c.options.OnGenericError != nil && len(msg.Errors) > 0 {
			c.options.OnGenericError(msg.Errors[0])
		}
		// A connection_error from AppSync is fatal for this connection.
		if msg.Type == MsgTypeConnectionError {
			c.Close() // Close client connection
		}
	default:
		if isErrorType(msg.Type) {
			c.logf("Received unhandled ERROR message type without ID: %s. Payload: %+v", msg.Type, msg.Errors)
			if c.options.OnGenericError != nil && len(msg.Errors) > 0 {
				c.options.OnGenericError(msg.Errors[0])
			}
		} else {
			c.logf("Received unhandled NON-ERROR message type without ID: %s", msg.Type)
		}
	}
}

// dispatchMessage routes incoming messages from AppSync to the appropriate handlers.
func (c *Client) dispatchMessage(msg Message) {
	c.mu.Lock()

	if msg.ID != nil {
		op, exists := c.operations[*msg.ID]
		if exists {
			c.handleOperationMessage(op, msg)
			c.mu.Unlock() // Unlock after operation is handled
			return        // Return as operation message path is complete
		} else {
			// Log that an operation ID was present but no operation found.
			// This can happen if the operation timed out and was removed, but a late message arrives.
			c.logf("Received message for unknown or timed-out operation ID %s: type %s", DerefString(msg.ID), msg.Type)
			// Fall through to handleNonOperationMessage in case it's a generic error with an ID we no longer track.
		}
	}

	// If we reach here, it's either a non-operation message,
	// or an operation message for an unknown/timed-out ID.
	// The mutex is still locked.
	c.handleNonOperationMessage(msg)
	c.mu.Unlock()
}
