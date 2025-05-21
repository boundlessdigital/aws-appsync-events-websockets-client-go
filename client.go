package appsyncwsclient

import (
	"context"
	"errors" // Added for error handling
	"fmt"
	"log"
	"net"    // Added for net.ErrClosed
	"sync"
	"testing" // Added for testLogger
	"time"

	signer "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"nhooyr.io/websocket"
)

// Client manages the WebSocket connection and communication with AWS AppSync.
const (
	defaultOperationTimeout = 30 * time.Second
	// Default timeout for waiting for internal goroutines to stop during Close()
	defaultCloseGoroutineWaitTimeout = 5 * time.Second
)

type Client struct {
	internal_wg sync.WaitGroup
	options      ClientOptions
	testLogger   *testing.T // For direct test logging
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
	close_mu          sync.Mutex // Protects is_closing and Close() re-entrancy
	is_closing        bool       // Flag to indicate Close() is in progress
	on_connection_closed func(code int, reason string) // Callback for connection closed
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
		testLogger: opts.TestLogger, // Initialize test logger
		signer:     signer.NewSigner(),
		operations: make(map[string]*operation),
		kickKeepAlive: make(chan struct{}, 1), // Buffered to prevent blocking on send
		on_connection_closed: opts.OnConnectionClose, // Corrected to use OnConnectionClose from ClientOptions
	}
	return c, nil
}

func (c *Client) logf(format string, v ...interface{}) {
	prefix := "[AppSyncWSClient-DEBUG] "
	if c.testLogger != nil {
		c.testLogger.Logf(prefix+format, v...)
	} else if c.options.Debug {
		log.Printf(prefix+format, v...)
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
                msg.Type == string(MsgTypeUnsubscribeError) ||
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
            c.keepAliveInterval = time.Duration(*msg.ConnectionTimeoutMs) * time.Millisecond
            c.logf("Server suggested keep-alive interval updated to: %v. manageKeepAlive will pick this up on next activity.", c.keepAliveInterval)
        }
        if c.options.OnConnectionAck != nil {
            c.options.OnConnectionAck(msg)
        }
    case MsgTypeKeepAlive:
        c.logf("Keep-alive received.")
        if c.options.OnKeepAlive != nil {
            c.options.OnKeepAlive()
        }
    case MsgTypeConnectionError, MsgTypeError: // Generic errors not tied to an ID
        c.logf("Received generic error from AppSync: %v", msg.Errors)
        if c.options.OnGenericError != nil && len(msg.Errors) > 0 {
            c.options.OnGenericError(msg.Errors[0])
        }
        if msg.Type == MsgTypeConnectionError {
            go c.Close() // Close client connection in a new goroutine
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
    c.mu.Lock() // Lock at the beginning

    if msg.ID != nil {
        op, exists := c.operations[*msg.ID]
        if exists {
            c.handleOperationMessage(op, msg) 
            c.mu.Unlock() // Unlock after operation is handled
            return        // Return as operation message path is complete
        } else {
            c.logf("Received message for unknown or timed-out operation ID %s: type %s", DerefString(msg.ID), msg.Type)
        }
    }

    c.handleNonOperationMessage(msg) 
    c.mu.Unlock() // Ensure unlock before exiting
}


// Close gracefully shuts down the WebSocket client connection.
// It signals all internal goroutines to stop, waits for them,
// closes the WebSocket connection, and invokes the OnConnectionClosed callback.
// If the WebSocket connection is already closed when attempting to send a close frame
// (resulting in errors like 'use of closed network connection' or net.ErrClosed),
// this is treated as a successful closure, and nil is returned.
func (c *Client) Close() error {
	c.close_mu.Lock() // Lock for checking/setting is_closing
	c.logf("[AppSyncWSClient-DEBUG] Client.Close: Entered with is_closing=%v, is_connected=%v, conn_is_nil=%v", c.is_closing, c.isConnected, c.conn == nil)

	if c.is_closing {
		c.logf("[AppSyncWSClient-DEBUG] Client.Close: Close attempt while already closing.")
		c.close_mu.Unlock()
		return errors.New("close operation already in progress")
	}

	// Check connection state under general mutex, but is_closing under close_mu
	c.mu.RLock()
	is_conn_nil := c.conn == nil
	is_connected_status := c.isConnected
	is_connecting_status := c.isConnecting
	c.mu.RUnlock()

	if !is_connected_status && is_conn_nil && !is_connecting_status {
		c.logf("[AppSyncWSClient-DEBUG] Client.Close: Client is not connected, not connecting, and conn is nil.")
		c.close_mu.Unlock()
		return nil
	}

	c.is_closing = true
	c.close_mu.Unlock() // Unlock close_mu; subsequent state changes are under c.mu or are final

	c.logf("[AppSyncWSClient-DEBUG] Client.Close: Signalling internal goroutines to stop...")
	if c.connCancel != nil {
		c.connCancel() // This should trigger Done() in goroutines using connCtx
	}

	c.logf("[AppSyncWSClient-DEBUG] Client.Close: Waiting for internal goroutines to stop...")
	wait_complete_ch := make(chan struct{})
	go func() {
		c.internal_wg.Wait() // Wait for handleIncomingMessages and manageKeepAlive
		close(wait_complete_ch)
	}()

	select {
	case <-wait_complete_ch:
		c.logf("[AppSyncWSClient-DEBUG] Client.Close: Internal goroutines stopped.")
	case <-time.After(defaultCloseGoroutineWaitTimeout): // Use defined constant for internal goroutine wait
		c.logf("[AppSyncWSClient-ERROR] Client.Close: Timeout waiting for internal goroutines to stop.")
	}

	// Final state update and connection closure must be under the main mutex
	c.mu.Lock()
	defer func() {
		c.mu.Unlock()
		c.close_mu.Lock() // Lock again to reset is_closing
		c.is_closing = false
		c.close_mu.Unlock()
		c.logf("[AppSyncWSClient-DEBUG] Client.Close: Exited")
	}()

	var close_err error
	ws_code_to_report := int(websocket.StatusNormalClosure)
	reason_to_report := "client requested closure"

	if c.conn != nil {
		c.logf("[AppSyncWSClient-DEBUG] Client.Close: Calling c.conn.Close(websocket.StatusNormalClosure, ...)")
		close_err = c.conn.Close(websocket.StatusNormalClosure, "client requested closure")
		c.logf("[AppSyncWSClient-DEBUG] Client.Close: c.conn.Close() returned: %v", close_err)
		if close_err != nil {
			var ce websocket.CloseError
			if errors.As(close_err, &ce) {
				ws_code_to_report = int(ce.Code)
				reason_to_report = ce.Reason
			} else {
				ws_code_to_report = 1006 // WebSocket StatusAbnormalClosure for non-websocket specific errors like net.ErrClosed
				reason_to_report = close_err.Error()
			}
		}
	} else {
		c.logf("[AppSyncWSClient-DEBUG] Client.Close: c.conn is already nil before c.conn.Close() call.")
	}

	// Update internal state
	c.isConnected = false
	c.isConnecting = false // Should also be false if closing
	c.conn = nil
	// c.connCtx and c.connCancel are typically not reset here, but with the connection.

	if c.on_connection_closed != nil {
		c.logf("[AppSyncWSClient-DEBUG] Client.Close: Invoking OnConnectionClosed callback with code %d, reason '%s'", ws_code_to_report, reason_to_report)
		go c.on_connection_closed(ws_code_to_report, reason_to_report) // Run in goroutine to avoid deadlocks
	} else {
		c.logf("[AppSyncWSClient-DEBUG] Client.Close: OnConnectionClosed callback is nil.")
	}

	c.logf("[AppSyncWSClient-DEBUG] WebSocket connection resources released state updated.")

	if close_err != nil && (errors.Is(close_err, net.ErrClosed) || websocket.CloseStatus(close_err) == websocket.StatusAbnormalClosure || websocket.CloseStatus(close_err) == websocket.StatusGoingAway) {
		c.logf("[AppSyncWSClient-DEBUG] Client.Close: c.conn.Close() reported an acceptable closure error (%v), returning nil.", close_err)
		return nil
	}

	return close_err
}

// IsConnected returns true if the client believes it is currently connected.
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isConnected
}
