package appsyncwsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Publish sends an array of event messages to the specified AppSync channel.
// Each event in the 'events_payload' slice will be marshalled to a JSON string.
// The AppSync Events API expects the 'events' field in the WebSocket message
// to be an array of stringified JSONs.
func (c *Client) Publish(ctx context.Context, channel string, events_payload []interface{}) error {
	c.mu.RLock()
	if !c.isConnected {
		c.mu.RUnlock()
		return fmt.Errorf("client not connected")
	}
	c.mu.RUnlock()

	if len(events_payload) == 0 {
		return fmt.Errorf("events_payload cannot be empty")
	}
	if len(events_payload) > 5 {
		return fmt.Errorf("cannot publish more than 5 events in a single request")
	}

	// Ensure channel name starts with a "/"
	formatted_channel := channel
	if !strings.HasPrefix(channel, "/") {
		formatted_channel = "/" + channel
		c.logf("Channel name auto-prefixed with '/': %s", formatted_channel)
	}

	operation_id := c.generate_operation_id()
	c.logf("Initiating publish operation %s to channel: %s", operation_id, formatted_channel)

	// Marshal each event to a JSON string
	stringified_events := make([]string, len(events_payload))
	for i, event := range events_payload {
		event_bytes, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("failed to marshal event at index %d: %w", i, err)
		}
		stringified_events[i] = string(event_bytes)
	}

	// Prepare authorization payload for signing
	// This payload will be used by create_signed_headers_for_operation
	publish_auth_payload := map[string]interface{}{
		"channel": formatted_channel,
		"events":  stringified_events, // Pass the array of JSON strings
	}

	auth_headers, err := c.create_signed_headers_for_operation(ctx, publish_auth_payload)
	if err != nil {
		return fmt.Errorf("failed to create signed headers for publish: %w", err)
	}

	publish_msg := Message{
		Type:          MsgTypePublish,
		ID:            &operation_id,
		Channel:       &formatted_channel,
		Events:        stringified_events, // Array of stringified JSONs
		Authorization: auth_headers,
	}

	op := &operation{
		ID:              operation_id,
		MessageType:     MsgTypePublish,
		ackCh:           make(chan Message, 1),
		errorCh:         make(chan Message, 1),
		timeoutDuration: defaultOperationTimeout,
	}

	c.mu.Lock()
	c.operations[operation_id] = op
	c.mu.Unlock()

	// Ensure operation is cleaned up if publish fails to send or times out
	defer func() {
		c.mu.Lock()
		delete(c.operations, operation_id)
		c.mu.Unlock()
	}()

	if err := c.sendMessage(c.connCtx, publish_msg); err != nil {
		return fmt.Errorf("failed to send publish message: %w", err)
	}

	// Wait for acknowledgment or error
	timeout_timer := time.NewTimer(op.timeoutDuration)
	defer timeout_timer.Stop()

	select {
	case ack_msg := <-op.ackCh:
		if ack_msg.Type == MsgTypePublishSuccess {
			c.logf("Publish operation %s to channel %s succeeded.", operation_id, formatted_channel)
			// Check for individual event failures within the success message if needed
			if ack_msg.Payload != nil && len(ack_msg.Payload.Failed) > 0 {
				c.logf("Publish operation %s had %d failed events.", operation_id, len(ack_msg.Payload.Failed))
				// Depending on desired behavior, this could be an error or just a log
				// For now, we'll log and consider the top-level publish_success as overall success
			}
			return nil
		} else {
			return fmt.Errorf("received unexpected ack type %s for publish operation %s", ack_msg.Type, operation_id)
		}
	case err_msg := <-op.errorCh:
		emsg := "publish operation failed"
		if len(err_msg.Errors) > 0 {
			emsg = fmt.Sprintf("publish operation %s failed: %s - %s", operation_id, err_msg.Errors[0].ErrorType, err_msg.Errors[0].Message)
		}
		return fmt.Errorf(emsg)
	case <-timeout_timer.C:
		return fmt.Errorf("publish operation %s timed out", operation_id)
	case <-c.connCtx.Done():
		return fmt.Errorf("connection closed during publish operation %s", operation_id)
	case <-ctx.Done():
		return fmt.Errorf("publish operation %s cancelled by caller: %w", operation_id, ctx.Err())
	}
}
