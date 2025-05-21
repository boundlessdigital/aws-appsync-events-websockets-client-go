package appsyncwsclient

import (
	"context"       // For operation struct
	"encoding/json" // Added for Message.ToJSONString
	"testing"       // For TestLogger
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// ClientOptions holds configuration for the AppSync WebSocket client.
type ClientOptions struct {
	AppSyncAPIHost        string        // The hostname of the AppSync Events API (e.g., <id>.appsync-api.<region>.amazonaws.com)
	AppSyncRealtimeHost   string        // The hostname of the AppSync Realtime service (e.g., <id>.appsync-realtime-api.<region>.amazonaws.com)
	AWSRegion             string        // The AWS region where the AppSync API is deployed (e.g., us-west-1)
	AWSCfg                aws.Config    // AWS SDK v2 Config, used for signing requests.
	ConnectionInitPayload interface{}   // Payload for the 'connection_init' message, typically an empty object {}.
	Debug                 bool          // Enable debug logging.
	ReadTimeout           time.Duration // Max time to wait for a read from the WebSocket connection
	KeepAliveInterval     time.Duration // Interval for sending keep-alive messages
	TestLogger            *testing.T    // Optional logger for tests
	OperationTimeout    time.Duration // Default timeout for operations like subscribe, publish

	// Optional callbacks
	OnConnectionError func(msg Message)                             // Called for AppSync 'connection_error' messages. For other WS errors, use OnConnectionClose or log directly.
	OnConnectionClose func(code int, reason string)                 // Called when the WebSocket connection closes.
	OnConnectionAck   func(msg Message)                             // Called when 'connection_ack' is received.
	OnKeepAlive       func()                                        // Called when a 'ka' (keep-alive) message is received.
	OnGenericError    func(errMessage MessageError)                 // Called for generic AppSync error messages (e.g., 'connection_error', 'error').
	OnSubscriptionError func(subscriptionID string, errMessage MessageError) // Called for subscription-specific errors.
}

// MessageError represents an error structure often found in AppSync messages.
type MessageError struct {
	ErrorType string      `json:"errorType,omitempty"` // Specific type of error (e.g., "ThrottlingException")
	Message   string      `json:"message"`
	ErrorCode interface{} `json:"errorCode,omitempty"` // Can be int or string, more generic or for older/other error types
}

// PublishEventErrorDetail represents an error for a specific event within a batch publish.
// As per AppSync documentation for publish_success messages with failed events.
// e.g., {"event_index": 0, "error_type": "PublishEventLimitExceeded", "message": "Publish event limit exceeded."}
type PublishEventErrorDetail struct {
	EventIndex int    `json:"event_index"`
	ErrorType  string `json:"error_type"`
	Message    string `json:"message"`
}

// Message represents a WebSocket message exchanged with AppSync.
// Fields are pointers or use omitempty to be excluded from JSON if nil/empty.
type Message struct {
	Type                string             `json:"type"`
	ID                  *string            `json:"id,omitempty"`
	Channel             *string            `json:"channel,omitempty"`         // For client-sent subscribe/publish
	Authorization       map[string]string  `json:"authorization,omitempty"`  // For client-sent subscribe/publish
	Events              []string           `json:"events,omitempty"`         // For client-sent publish (stringified JSON events)
	Payload             *PayloadContainer  `json:"payload,omitempty"`        // For 'data' messages, connection_init, etc.
	ConnectionTimeoutMs *int               `json:"connectionTimeoutMs,omitempty"` // For 'connection_ack'
	Errors              []MessageError     `json:"errors,omitempty"`         // For 'error', 'connection_error'
}

// ToJSONString marshals the message to a JSON string for logging or other purposes.
// Returns an empty string and logs an error if marshalling fails.
func (m *Message) ToJSONString() string {
	bytes, err := json.Marshal(m)
	if err != nil {
		// In a real app, you might want a logger here.
		// log.Printf("Error marshalling message to JSON: %v", err)
		return "" // Or perhaps return err.Error() or a formatted error string
	}
	return string(bytes)
}

// PayloadContainer is used because AppSync often nests actual data under a 'data' key within the payload.
// For connection_init, it might be an empty object. For data, it contains the event data.
// For publish_success, it can contain a list of failed events if some events in a batch publish failed.
type PayloadContainer struct {
	Data   interface{}             `json:"data,omitempty"`   // Actual event data for 'data' messages
	Failed []PublishEventErrorDetail `json:"failed,omitempty"` // For 'publish_success' if some events failed
	// Other fields that might appear in a payload from AppSync can be added here.
	// Using a flexible interface{} for Data allows unmarshalling various AppSync payloads.
	// For connection_init, it might be an empty object or omitted.
}

// operation represents an active client-initiated operation (e.g., subscribe, publish, unsubscribe)
// that is waiting for an acknowledgment or error from the server.
// It also manages the lifecycle of a subscription's data flow if the operation is a subscription.
type operation struct {
	ID                 string
	MessageType        string // Type of message that initiated this operation (e.g., subscribe, publish)
	subscriptionCtx    context.Context // Context for the operation's lifecycle, especially for subscriptions
	subscriptionCancel context.CancelFunc // Function to cancel the operation's context
	ackCh              chan Message // Receives ack messages (e.g., subscribe_success, publish_success)
	errorCh            chan Message // Receives error messages (e.g., subscribe_error, publish_error)
	dataChannel        chan Message // Internal channel for routing incoming data messages (for subscriptions)
	timeoutDuration    time.Duration // Duration for the operation to timeout
	timeoutTimer       *time.Timer // Timer for operation timeout
}

// Constants for AppSync WebSocket message types
const (
	MsgTypeConnectionInit     = "connection_init"
	MsgTypeConnectionAck      = "connection_ack"
	MsgTypeConnectionError    = "connection_error"
	MsgTypeError              = "error"
	MsgTypeKeepAlive          = "ka"
	MsgTypeSubscribe          = "subscribe"
	MsgTypeSubscribeSuccess   = "subscribe_success"
	MsgTypeSubscribeError     = "subscribe_error"
	MsgTypeData               = "data"
	MsgTypePublish            = "publish"
	MsgTypePublishSuccess     = "publish_success"
	MsgTypePublishFail        = "publish_fail"
	MsgTypeServerPublishError = "publish_error" // Server response to client publish
	MsgTypeBroadcastError     = "broadcast_error" // Server-sent error during data broadcast for a subscription
	MsgTypeUnsubscribe        = "unsubscribe"
	MsgTypeUnsubscribeSuccess = "unsubscribe_success"
	MsgTypeUnsubscribeError   = "unsubscribe_error"
)
