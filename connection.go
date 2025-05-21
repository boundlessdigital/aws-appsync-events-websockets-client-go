package appsyncwsclient

import (
	"context"
	"encoding/json" // Added for json.Marshal in sendMessage
	"fmt"
	"time"

	"nhooyr.io/websocket"
)

// Connect establishes a WebSocket connection to AppSync and handles authentication.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	if c.isConnecting || c.isConnected {
		c.mu.Unlock()
		c.logf("Connect called while already connecting or connected.")
		return nil
	}
	c.isConnecting = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.isConnecting = false
		c.mu.Unlock()
	}()

	c.logf("Attempting to connect to %s", c.options.RealtimeServiceURL)

	subprotocols, err := c.create_connection_auth_subprotocol(ctx)
	if err != nil {
		return fmt.Errorf("failed to create connection auth subprotocol: %w", err)
	}
	c.logf("Auth subprotocols prepared: %v", subprotocols)

	dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second) // Connection timeout
	defer dialCancel()

	wsConn, _, err := websocket.Dial(dialCtx, c.options.RealtimeServiceURL, &websocket.DialOptions{
		Subprotocols: subprotocols,
	})
	if err != nil {
		return fmt.Errorf("failed to dial WebSocket: %w", err)
	}

	c.mu.Lock()
	c.conn = wsConn
	c.connCtx, c.connCancel = context.WithCancel(context.Background()) // Independent context for connection lifecycle
	c.isConnected = true
	c.mu.Unlock()

	c.logf("WebSocket connection established. Sending connection_init...")

	initMsg := Message{
		Type:    MsgTypeConnectionInit,
		Payload: &PayloadContainer{Data: c.options.ConnectionInitPayload},
	}
	if err := c.sendMessage(c.connCtx, initMsg); err != nil {
		c.Close() // Close connection if init fails
		return fmt.Errorf("failed to send connection_init: %w", err)
	}

	go c.handleIncomingMessages()
	go c.manageKeepAlive()

	return nil
}

// Close gracefully closes the WebSocket connection and cleans up resources.
func (c *Client) Close() error {
	c.mu.Lock()
	if !c.isConnected && !c.isConnecting {
		c.mu.Unlock()
		c.logf("Close called but client is not connected or connecting.")
		return nil // Not connected, nothing to close
	}

	// Signal all goroutines that depend on connCtx to stop
	if c.connCancel != nil {
		c.connCancel()
	}

	var err error
	if c.conn != nil {
		c.logf("Closing WebSocket connection...")
		// Close with a normal closure status.
		err = c.conn.Close(websocket.StatusNormalClosure, "Client closing connection")
		c.conn = nil
	}

	c.isConnected = false
	c.isConnecting = false // Ensure this is also reset

	// Clean up operations
	for id, op := range c.operations {
		op.subscriptionCancel() // Cancel the context for this operation
		close(op.ackCh)
		close(op.errorCh)
		if op.dataChannel != nil {
			close(op.dataChannel)
		}
		delete(c.operations, id)
	}
	c.mu.Unlock()

	if c.options.OnConnectionClose != nil {
		// Convert error to code and reason if possible, or use defaults
		// This is a simplified notification as specific close codes from server are hard to get here.
		closeCode := 1000
		closeReason := "Client initiated close"
		if err != nil { // If close itself had an error
			closeCode = 1006 // Abnormal closure
			closeReason = err.Error()
		}
		c.options.OnConnectionClose(closeCode, closeReason)
	}

	c.logf("WebSocket connection closed.")
	return err // Return the error from conn.Close() if any
}

// DerefString safely dereferences a string pointer, returning an empty string if the pointer is nil.
func DerefString(s *string) string {
	if s != nil {
		return *s
	}
	return ""
}

func (c *Client) sendMessage(ctx context.Context, msg Message) error {
	if c.send_message_override != nil {
		c.logf("Sending WebSocket message (via override): %s", msg.ToJSONString())
		return c.send_message_override(ctx, msg)
	}

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("cannot send message: not connected")
	}

	c.logf("Sending WebSocket message: %s", msg.ToJSONString()) // Log the message being sent

	jsonMsg, err := json.Marshal(msg) 
	if err != nil {
		return fmt.Errorf("failed to marshal message to JSON: %w", err)
	}

	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second) // Timeout for the write operation
	defer cancel()

	return conn.Write(writeCtx, websocket.MessageText, jsonMsg)
}

// _read_and_unmarshal_message reads a single message from the WebSocket and unmarshals it.
func (c *Client) _read_and_unmarshal_message() (Message, error) {
	// Set a read timeout for each message read attempt.
	// It's important this timeout is longer than the expected keep-alive interval from the server.
	// c.connCtx is the context for the overall connection lifecycle.
	readCtx, cancelRead := context.WithTimeout(c.connCtx, c.options.ReadTimeout)
	defer cancelRead()

	msgType, data, err := c.conn.Read(readCtx)
	if err != nil {
		return Message{}, err // Return error to be handled by caller
	}

	// Log raw message if debug is enabled
	if c.options.Debug {
		c.logf("Received raw WebSocket message type %v: %s", msgType, string(data))
	}

	var msg Message
	if err := json.Unmarshal(data, &msg); err != nil {
		// Log and return the unmarshalling error, including raw data for diagnosis.
		c.logf("Error unmarshalling message: %v. Raw data: %s", err, string(data))
		return Message{}, fmt.Errorf("error unmarshalling message: %w. Raw data: %s", err, string(data))
	}
	return msg, nil
}

// handleIncomingMessages runs in a goroutine to process messages from AppSync.
func (c *Client) handleIncomingMessages() {
	defer func() {
		c.logf("handleIncomingMessages goroutine stopped.")
		// If this loop stops (e.g., due to connection error), ensure client is marked as disconnected.
		// The Close() method is idempotent and handles the lock internally.
		c.Close() // Attempt a graceful close and cleanup
	}()

	for {
		select {
		case <-c.connCtx.Done(): // Connection context cancelled (e.g., by Client.Close())
			c.logf("Connection context done, exiting message read loop.")
			return
		default:
			// Fall through to read the next message
		}

		msg, err := c._read_and_unmarshal_message()
		if err != nil {
			// Check if the error is due to overall connection context cancellation
			if c.connCtx.Err() != nil {
				c.logf("Connection context done (error path), exiting message read loop: %v", c.connCtx.Err())
				return // Exit loop, defer will handle cleanup
			}

			// Check for normal WebSocket closure
			closeStatus := websocket.CloseStatus(err)
			if closeStatus == websocket.StatusNormalClosure || closeStatus == websocket.StatusGoingAway {
				c.logf("WebSocket normally closed by peer (status %d): %v", closeStatus, err)
				return // Exit loop, defer will handle cleanup
			}

			// For other errors (e.g., timeouts, unexpected closures, unmarshal errors)
			c.logf("Error reading/unmarshalling from WebSocket: %v", err)
			// Any read/unmarshal error is considered fatal for the read loop.
			// The defer will call c.Close() to clean up the connection.
			return
		}

		// Reset keep-alive timer on any successfully processed message received
		select {
		case c.kickKeepAlive <- struct{}{}:
		default: // Non-blocking send, don't stall if manageKeepAlive isn't ready
		}

		c.logf("Received AppSync message: Type=%s, ID=%s. Dispatching...", msg.Type, DerefString(msg.ID))
		c.dispatchMessage(msg) // Delegate all message handling to dispatchMessage
	}
}

// defaultClientKeepAliveTimeout is the default keep-alive timeout if not specified by
// the client options or the server via connection_ack.
const defaultClientKeepAliveTimeout = 5 * time.Minute

// _get_effective_keep_alive_timeout determines the effective keep-alive timeout duration.
// It prioritizes server-suggested, then client-configured, then a default.
func (c *Client) _get_effective_keep_alive_timeout() time.Duration {
	c.mu.RLock()
	serverTimeout := c.keepAliveInterval // This is updated on connection_ack by dispatchMessage
	c.mu.RUnlock()

	if serverTimeout > 0 {
		return serverTimeout
	}

	if c.options.KeepAliveInterval > 0 {
		return c.options.KeepAliveInterval
	}
	return defaultClientKeepAliveTimeout
}

// manageKeepAlive sends keep-alive messages to the server.
func (c *Client) manageKeepAlive() {
	effectiveTimeout := c._get_effective_keep_alive_timeout()
	pingInterval := effectiveTimeout / 2
	if pingInterval <= 0 { // Ensure ping interval is positive to prevent ticker panic
		c.logf("Warning: Initial effective keep-alive timeout %v results in non-positive ping interval. Using default ping interval (%v).", effectiveTimeout, defaultClientKeepAliveTimeout/2)
		pingInterval = (defaultClientKeepAliveTimeout / 2)
	}

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	defer c.logf("manageKeepAlive goroutine stopped.")
	c.logf("manageKeepAlive started. Initial ping interval: %v (based on effective timeout: %v)", pingInterval, effectiveTimeout)

	for {
		select {
		case <-c.connCtx.Done(): // Connection context cancelled, e.g., by Client.Close()
			return
		case <-c.kickKeepAlive: // Activity detected on the connection, reset keep-alive timer
			c.logf("Keep-alive timer reset due to activity (kickKeepAlive received).")
			newTimeout := c._get_effective_keep_alive_timeout()
			newPingIntervalCandidate := newTimeout / 2

			if newPingIntervalCandidate <= 0 {
				c.logf("Warning: Effective keep-alive timeout %v on kick results in non-positive ping interval %v. Using previous ping interval: %v", newTimeout, newPingIntervalCandidate, pingInterval)
				// Keep the existing valid pingInterval
			} else if newPingIntervalCandidate != pingInterval {
				c.logf("Adjusting ping interval from %v to %v due to activity or config change.", pingInterval, newPingIntervalCandidate)
				pingInterval = newPingIntervalCandidate
			}
			ticker.Reset(pingInterval) // Reset with current (potentially updated, always valid) pingInterval

		case <-ticker.C: // Time to send a keep-alive ping
			// Re-evaluate interval before sending, as server might have updated c.keepAliveInterval via connection_ack
			newTimeout := c._get_effective_keep_alive_timeout()
			newPingIntervalCandidate := newTimeout / 2

			if newPingIntervalCandidate <= 0 {
				c.logf("Warning: Effective keep-alive timeout %v on tick results in non-positive ping interval %v. Using previous ping interval: %v", newTimeout, newPingIntervalCandidate, pingInterval)
			} else if newPingIntervalCandidate != pingInterval {
				c.logf("Adjusting ping interval from %v to %v before send due to config change.", pingInterval, newPingIntervalCandidate)
				pingInterval = newPingIntervalCandidate
			}
			// Reset the ticker for the *next* period with the current (potentially updated, always valid) pingInterval.
			// This ensures the next tick accurately reflects the latest interval, done *before* potential blocking on sendMessage.
			ticker.Reset(pingInterval)

			c.mu.RLock()
			connected := c.isConnected
			c.mu.RUnlock()

			if !connected {
				c.logf("manageKeepAlive: Not connected, stopping ping attempts.")
				return // Connection lost or closed
			}

			c.logf("Sending keep-alive message (ping interval: %v).", pingInterval)
			kaMsg := Message{Type: MsgTypeKeepAlive}
			if err := c.sendMessage(c.connCtx, kaMsg); err != nil {
				c.logf("Error sending keep-alive: %v. Terminating keep-alive manager.", err)
				// Error sending keep-alive implies connection issue. Client.Close() will likely be called
				// by handleIncomingMessages or another part of the system, which cancels c.connCtx.
				return
			}
		}
	}
}
