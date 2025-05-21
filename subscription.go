package appsyncwsclient

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Subscription represents an active subscription to an AppSync channel.
// It provides a method to unsubscribe and a channel to receive data.
// The Data() channel will be closed when the subscription is terminated (either by Unsubscribe() or by the client closing).
type Subscription struct {
	ID          string
	client      *Client // Reference to the main client for unsubscribing
	dataChannel <-chan interface{} // User-facing channel to receive deserialized event data
	ctx         context.Context    // Context for this specific subscription's lifetime
	cancel      context.CancelFunc // Function to cancel this specific subscription's context
}

// Unsubscribe terminates an active subscription.
// It calls the client's internal unsubscribe method.
func (sub *Subscription) Unsubscribe() error {
	return sub.client.unsubscribe(sub.ID)
}

// Done returns a channel that's closed when the subscription is terminated (unsubscribed or client closed).
func (sub *Subscription) Done() <-chan struct{} {
	return sub.ctx.Done()
}

// Data returns a read-only channel from which to receive subscription data.
// This is used when the on_data callback is not provided to Subscribe.
func (sub *Subscription) Data() <-chan interface{} {
	return sub.dataChannel
}

// Subscribe initiates a subscription to the given AppSync channel.
// The on_data callback will be invoked for each data message received for this subscription.
func (c *Client) Subscribe(ctx context.Context, channel string, on_data func(data_payload interface{})) (*Subscription, error) {
	c.mu.RLock()
	if !c.isConnected {
		c.mu.RUnlock()
		return nil, fmt.Errorf("client not connected")
	}
	c.mu.RUnlock()

	formatted_channel := channel
	if !strings.HasPrefix(channel, "/") {
		formatted_channel = "/" + channel
		c.logf("Channel name auto-prefixed with '/': %s", formatted_channel)
	}

	subscription_id := c.generate_operation_id()
	c.logf("Initiating subscription %s to channel: %s", subscription_id, formatted_channel)

	subscribe_auth_payload := map[string]interface{}{"channel": formatted_channel}
	auth_headers, err := c.create_signed_headers_for_operation(ctx, subscribe_auth_payload)
	if err != nil {
		return nil, fmt.Errorf("failed to create signed headers for subscription: %w", err)
	}

	sub_msg := Message{
		Type:          MsgTypeSubscribe,
		ID:            &subscription_id,
		Channel:       &formatted_channel,
		Authorization: auth_headers,
	}

	// Create the context and cancel function for the Subscription object.
	// This context will also govern the data handling goroutine's lifetime.
	sub_specific_ctx, sub_specific_cancel := context.WithCancel(c.connCtx)
	
	// Ensure sub_specific_cancel is called if Subscribe returns an error path after this point,
	// or if it panics. This is a safeguard. Successful return paths will return the sub object,
	// and its sub_specific_cancel will be managed by sub.Unsubscribe() or client close.
	// Error paths before returning sub must cancel it to prevent goroutine/context leaks.
	var returned_sub_successfully bool = false
	defer func() {
		if !returned_sub_successfully {
			c.logf("Subscribe for %s returning error or panicking, ensuring its context is cancelled.", subscription_id)
			sub_specific_cancel()
		}
	}()

	user_facing_data_channel := make(chan interface{}, 10)

	sub := &Subscription{
		ID:          subscription_id,
		client:      c,
		dataChannel: user_facing_data_channel,
		ctx:         sub_specific_ctx,
		cancel:      sub_specific_cancel,
	}

	op := &operation{
		ID:                 subscription_id,
		MessageType:        MsgTypeSubscribe,
		subscriptionCtx:    sub.ctx,    // Use the Subscription's context
		subscriptionCancel: sub.cancel, // Use the Subscription's cancel
		ackCh:              make(chan Message, 1),
		errorCh:           make(chan Message, 1),
		dataChannel:        make(chan Message, 10), 
		timeoutDuration:    defaultOperationTimeout,
		// timeoutTimer is set below
	}

	c.mu.Lock()
	c.operations[subscription_id] = op
	c.mu.Unlock()

	// Data handling goroutine
	go func() {
		defer close(user_facing_data_channel)
		// Use op.ID in logs as subscription_id might be shadowed if this func took it as param
		defer c.logf("Subscription %s data handler goroutine stopped.", op.ID) 
		for {
			select {
			case <-op.subscriptionCtx.Done(): // Tied to sub.ctx.Done()
				c.logf("op.subscriptionCtx.Done() selected in data handler for op %s. Returning.", op.ID)
				return
			case data_msg, ok := <-op.dataChannel: // data_msg is the *Message struct*
				c.logf("Read from op.dataChannel for op %s. ok: %t. Message Type: %s", op.ID, ok, data_msg.Type)
				if !ok { 
					c.logf("op.dataChannel closed for op %s. Returning.", op.ID)
					return
				}

				var event_data_to_deliver interface{}
				if data_msg.Payload != nil && data_msg.Payload.Data != nil { 
					c.logf("Event data for op %s from data_msg.Payload.Data: %v", op.ID, data_msg.Payload.Data)
					event_data_to_deliver = data_msg.Payload.Data 
					if on_data != nil {
						on_data(event_data_to_deliver)
						c.logf("Called on_data callback for op %s", op.ID)
					} else {
						select {
						case user_facing_data_channel <- event_data_to_deliver:
							c.logf("Sent data to user_facing_data_channel for op %s", op.ID)
						case <-op.subscriptionCtx.Done(): // Subscription context cancelled
							c.logf("Subscription context done while trying to send to user_facing_data_channel for op %s", op.ID)
							// Do not return here yet, let the loop exit via op.subscriptionCtx.Done() select case
						}
					}
				} else {
					c.logf("No event data found in data_msg.Payload.Data for op %s (Type: %s, Payload: %v)", op.ID, data_msg.Type, data_msg.Payload)
				}
			}
		}
	}()

	if err := c.sendMessage(ctx, sub_msg); err != nil {
		c.logf("Failed to send subscribe message for %s: %v. Cleaning up operation.", subscription_id, err)
		c.mu.Lock()
		delete(c.operations, subscription_id)
		c.mu.Unlock()
		// returned_sub_successfully is false, so defer will call sub_specific_cancel()
		return nil, fmt.Errorf("failed to send subscribe message: %w", err)
	}
	
	op.timeoutTimer = time.NewTimer(op.timeoutDuration)

	select {
	case ack := <-op.ackCh:
		op.timeoutTimer.Stop()
		c.logf("Subscription %s acknowledged: %s", subscription_id, ack.Type)
		returned_sub_successfully = true // Mark successful return, defer will not cancel sub_specific_ctx
		return sub, nil
	case errMsg := <-op.errorCh:
		op.timeoutTimer.Stop()
		c.logf("Subscription %s failed: Type=%s, Errors=%v", subscription_id, errMsg.Type, errMsg.Errors)
		c.mu.Lock()
		delete(c.operations, subscription_id)
		c.mu.Unlock()
		// returned_sub_successfully is false, so defer will call sub_specific_cancel()
		return nil, fmt.Errorf("subscription %s failed: %v (type: %s)", subscription_id, errMsg.Errors, errMsg.Type)
	case <-op.timeoutTimer.C:
		c.logf("Subscription %s timed out waiting for ack/error.", subscription_id)
		c.mu.Lock()
		delete(c.operations, subscription_id)
		c.mu.Unlock()
		// returned_sub_successfully is false, so defer will call sub_specific_cancel()
		return nil, fmt.Errorf("subscription %s timed out", subscription_id)
	case <-ctx.Done(): // Overall context for the Subscribe call itself
		c.logf("Context cancelled while waiting for subscription %s ack/error: %v", subscription_id, ctx.Err())
		op.timeoutTimer.Stop() // Stop timer if running
		c.mu.Lock()
		if _, exists := c.operations[subscription_id]; exists {
			delete(c.operations, subscription_id)
		}
		c.mu.Unlock()
		// returned_sub_successfully is false, so defer will call sub_specific_cancel()
		return nil, ctx.Err()
	}
}

// unsubscribe terminates an active subscription by its ID.
func (c *Client) unsubscribe(subscription_id string) error {
	c.mu.RLock()
	op, exists := c.operations[subscription_id]
	foundAndIsSubscribeOp := exists && op.MessageType == MsgTypeSubscribe
	c.mu.RUnlock()

	if !foundAndIsSubscribeOp {
		c.logf("No active subscribe operation found for ID %s to unsubscribe (exists: %t, op type: %v).", subscription_id, exists, op.MessageType) // Use op.MessageType safely if exists is true
		// If op exists but is not a subscription, or doesn't exist, consider it a no-op or already unsubscribed.
		return nil
	}

	c.logf("Unsubscribing from ID: %s", subscription_id)

	unsubscribe_msg := Message{
		Type: MsgTypeUnsubscribe,
		ID:   &subscription_id,
	}

	// Reset ack/error channels for this new unsubscribe operation on the existing subscription_id
	// Note: The original 'op' was for the 'subscribe' message. We are now sending 'unsubscribe'.
	// For simplicity, we can reuse the subscription_id for the unsubscribe ack, AppSync supports this.
	// Or generate a new opID for the unsubscribe message itself.
	// Let's assume AppSync acks unsubscribe using the original subscription ID.
	// If we need a new op for unsubscribe, the logic for tracking would be more complex.

	op.MessageType = MsgTypeUnsubscribe // Mark the operation as an unsubscribe attempt now
	// We can reuse op.ackCh and op.errorCh if we ensure they are reset or handled correctly.
	// For now, we assume `dispatchMessage` will route `unsubscribe_success` or `unsubscribe_fail` to this `op`.

	if err := c.sendMessage(c.connCtx, unsubscribe_msg); err != nil {
		// If sending fails, we can't be sure of the server state.
		// The subscription might still be active on the server.
		// We should still clean up locally.
		c.logf("Failed to send unsubscribe message for %s: %v. Cleaning up locally.", subscription_id, err)
		// op.subscriptionCancel() // Already called via defer in data handler, or by close in dispatch
		// close(op.dataChannel) // ^^ 
		// c.mu.Lock()
		// delete(c.operations, subscription_id)
		// c.mu.Unlock()
		// The above cleanup is now handled more robustly by the op.subscriptionCancel() and client Close()
		return fmt.Errorf("failed to send unsubscribe message: %w", err)
	}

	// Timeout for unsubscribe acknowledgment
	unsubscribeTimeout := time.NewTimer(defaultOperationTimeout)
	defer unsubscribeTimeout.Stop()

	select {
	case ack := <-op.ackCh: // Expecting unsubscribe_success
		c.logf("Unsubscribe for %s acknowledged: %s", subscription_id, ack.Type)
		// Actual cleanup of operation and data channels should happen when op.subscriptionCtx is canceled.
		op.subscriptionCancel() // This will stop the data handler and trigger cleanup
		c.mu.Lock()
		delete(c.operations, subscription_id)
		c.mu.Unlock()
		return nil
	case errMsg := <-op.errorCh: // Expecting unsubscribe_fail or general error
		c.logf("Unsubscribe for %s failed: %v", subscription_id, errMsg.Errors)
		// Even if unsubscribe fails on server, clean up locally.
		op.subscriptionCancel()
		c.mu.Lock()
		delete(c.operations, subscription_id)
		c.mu.Unlock()
		return fmt.Errorf("unsubscribe %s failed on server: %v", subscription_id, errMsg.Errors)
	case <-c.connCtx.Done():
		// Connection closed before unsubscribe completed.
		c.logf("Connection closed while waiting for unsubscribe ack for %s", subscription_id)
		// Local cleanup will happen via client Close() or op.subscriptionCancel triggered by connCtx.Done()
		return fmt.Errorf("connection closed during unsubscribe")
	case <-unsubscribeTimeout.C:
		c.logf("Unsubscribe for %s timed out.", subscription_id)
		// Assume it might not have worked on server, but clean up locally.
		op.subscriptionCancel()
		c.mu.Lock()
		delete(c.operations, subscription_id)
		c.mu.Unlock()
		return fmt.Errorf("unsubscribe for %s timed out", subscription_id)
	}
}
