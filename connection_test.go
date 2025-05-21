package appsyncwsclient

import (
	"context"
	"testing"
	"time"
	// "github.com/aws/aws-sdk-go-v2/aws" // Not directly used in this function block, but ClientOptions.AWSCfg is aws.Config
)

// safeString dereferences a string pointer or returns "<nil>" if it's nil.
func safeString(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestIntegration_ConnectDisconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // Increased timeout
	defer cancel()

	awsCfg, err := loadTestAWSConfig(ctx)
	if err != nil {
		t.Fatalf("Failed to load AWS config for test: %v", err)
	}

	client, clientErr := NewClient(ClientOptions{
		AppSyncAPIHost:      testAppSyncAPIHost,
		AppSyncRealtimeHost: testAppSyncRealtimeHost,
		AWSRegion:           testAWSRegion,
		AWSCfg:             awsCfg,
		TestLogger:        t,
		OnConnectionAck: func(msg Message) {
			t.Logf("Connection ACK: type=%s, id=%s, payload=%v", msg.Type, safeString(msg.ID), msg.Payload)
		},
		// Debug: true,
	})
	if clientErr != nil {
		t.Fatalf("NewClient failed: %v", clientErr)
	}

	// Test Connect
	t.Logf("Connecting...")
	connectErr := client.Connect(ctx)
	if connectErr != nil {
		t.Fatalf("Connect failed: %v", connectErr)
	}
	t.Logf("Connected successfully.")

	// Check if client.conn is not nil after successful connection
	if client.conn == nil {
		t.Errorf("client.conn is nil after successful Connect()")
	}
	client.mu.RLock()
	isConn := client.isConnected
	client.mu.RUnlock()
	if !isConn {
		t.Errorf("client.isConnected is false after successful Connect()")
	}


	// Test Disconnect (via Close)
	t.Logf("Closing client (which should disconnect)...")
	client.Close() // Close also handles disconnection
	t.Logf("Client closed.")

	// Verify state after close
	client.mu.RLock()
	isConnAfterClose := client.isConnected
	client.mu.RUnlock()
	if isConnAfterClose {
		t.Errorf("client.isConnected is true after Close()")
	}
	// After Close, client.conn might be set to nil by the closeWsConnection method.
	// Depending on the exact implementation, it might be useful to check if it's nil
	// or if specific cleanup has occurred. For now, isConnected is the primary check.
}
