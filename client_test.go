package appsyncwsclient

import (
	"context"
	"fmt"
	"sync" // Added for WaitGroup in integration test
	"encoding/json" // Added for unmarshalling subscription data
	// "log"             // Removed unused import
	// "os"              // Removed unused import
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config" // Added for integration tests
)

// --- Integration Tests ---

// Configuration for integration tests
const (
	testAppSyncAPIURL      = "https://gk3gfluct5azlhi5d3rnumxoq4.appsync-api.us-west-1.amazonaws.com/graphql"
	testRealtimeServiceURL = "wss://gk3gfluct5azlhi5d3rnumxoq4.appsync-realtime-api.us-west-1.amazonaws.com/event/realtime"
	testAWSRegion          = "us-west-1"
	testAWSProfile         = "boundless-development" // Ensure this profile is configured for your test environment
)

// loadTestAWSConfig helper function to load AWS config for tests
func loadTestAWSConfig(ctx context.Context) (aws.Config, error) {
	return config.LoadDefaultConfig(ctx,
		config.WithRegion(testAWSRegion),
		config.WithSharedConfigProfile(testAWSProfile),
	)
}


func TestIntegration_SubscribePublishReceive(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // Temporarily reduced timeout for debugging
	defer cancel()

	awsCfg, err := loadTestAWSConfig(ctx)
	if err != nil {
		t.Fatalf("Failed to load AWS config: %v", err)
	}

	// Unique channel for this test run, using a prefix likely allowed by IAM policy
	testChannel := fmt.Sprintf("/live-lambda/integrationtest-%d", time.Now().UnixNano())
	expectedMessageData := fmt.Sprintf("Hello from integration test %d", time.Now().UnixNano())

	var receivedMsgData string
	var wg sync.WaitGroup
	wg.Add(1) // For synchronizing receipt of the message

	client, clientErr := NewClient(ClientOptions{
		AppSyncAPIURL:      testAppSyncAPIURL,
		RealtimeServiceURL: testRealtimeServiceURL,
		AWSCfg:             awsCfg,
		OnConnectionAck: func(msg Message) {
			t.Logf("SPR Test: Connection ACK: type=%s", msg.Type)
		},
		Debug:             true, // Ensure debug is enabled
		TestLogger:        t,    // Add test logger
	})
	if clientErr != nil {
		t.Fatalf("SPR Test: NewClient failed: %v", clientErr)
	}

	t.Logf("SPR Test: Connecting...")
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("SPR Test: Connect failed: %v", err)
	}
	t.Logf("SPR Test: Connected. Subscribing to %s...", testChannel)
	time.Sleep(100 * time.Millisecond) // DEBUG: Wait a bit
	t.Logf("SPR Test: Pre-Subscribe check for %s. Client still connected via IsConnected(): %t", testChannel, client.IsConnected()) // DEBUG

	// client.Subscribe takes an on_data callback. We'll pass nil and use sub.Data().
	// The Subscribe method blocks until the subscription is acknowledged or fails.
	subscription, subErr := client.Subscribe(ctx, testChannel, nil)
	if subErr != nil {
		t.Fatalf("SPR Test: client.Subscribe call failed for %s: %v", testChannel, subErr)
	}
	t.Logf("SPR Test: Subscribe call successful for %s (ID: %s). Setting up data listener...", testChannel, subscription.ID)

	go func() {
		t.Logf("SPR Test: Goroutine for subscription %s (channel %s) started. Waiting for data, sub.Done, or ctx.Done.", subscription.ID, testChannel)
		// Keep a flag to ensure wg.Done() is only called once.
		doneCalled := false
		defer func() {
			if !doneCalled { // If wg.Done() hasn't been called yet (e.g. timeout or sub.Done path)
				t.Logf("SPR Test: Goroutine for subscription %s (channel %s) exiting via defer (not data path). Calling wg.Done().", subscription.ID, testChannel)
				wg.Done()
			}
		}()

		select {
		case data, ok := <-subscription.Data(): // sub.Data() returns interface{}
			if !ok {
				t.Logf("SPR Test: Subscription data channel closed for %s.", subscription.ID)
				// wg.Done() will be called by defer
				return
			}
			t.Logf("SPR Test: Received data on %s: %v", testChannel, data)
			if rawMsg, okRaw := data.(json.RawMessage); okRaw {
				var strData string
				if err := json.Unmarshal(rawMsg, &strData); err == nil {
					receivedMsgData = strData
				} else {
					t.Errorf("SPR Test: Failed to unmarshal json.RawMessage to string: %v. Raw data: %s", err, string(rawMsg))
				}
			} else if strData, okStr := data.(string); okStr {
				receivedMsgData = strData
			} else {
				t.Errorf("SPR Test: Received data on %s is not json.RawMessage or string: %T, value: %v", testChannel, data, data)
			}
			// Call wg.Done() immediately after processing data
			t.Logf("SPR Test: Goroutine for subscription %s (channel %s) processed data. Calling wg.Done().", subscription.ID, testChannel)
			wg.Done()
			doneCalled = true // Mark that wg.Done() was called

		case <-subscription.Done(): // Subscription was closed (unsubscribed or client closed)
			t.Logf("SPR Test: Subscription %s Done() channel closed before data was received or while waiting.", subscription.ID)
			// wg.Done() will be called by defer

		case <-ctx.Done(): // Test timeout
			t.Logf("SPR Test: Test context timed out (ctx.Done) in goroutine for subscription %s (channel %s).", subscription.ID, testChannel)
			// wg.Done() will be called by defer
		}
	}()

	t.Logf("SPR Test: Publishing to %s: '%s'...", testChannel, expectedMessageData)
	// client.Publish takes channel and events_payload []interface{}. It returns only error.
	pubErr := client.Publish(ctx, testChannel, []interface{}{expectedMessageData})
	if pubErr != nil {
		// If publish fails, wg.Done() might not be called by the receiver goroutine if it's stuck waiting for data.
		// However, the test context timeout or subscription.Done() should eventually unblock it.
		t.Fatalf("SPR Test: Publish to %s failed: %v", testChannel, pubErr)
	}
	t.Logf("SPR Test: Publish call to %s successful. Now waiting on wg.Wait()...", testChannel)

	// Wait for the message to be received or timeout
	waitDoneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDoneCh)
	}()

	select {
	case <-waitDoneCh:
		t.Logf("SPR Test: wg.Wait() completed for channel %s.", testChannel)
	case <-ctx.Done():
		t.Fatalf("SPR Test: Test context timed out while wg.Wait() was blocking for channel %s. This likely means wg.Done() was not called.", testChannel)
	}

	if receivedMsgData == "" {
		t.Errorf("SPR Test: Did not receive message on channel %s", testChannel)
	} else if receivedMsgData != expectedMessageData {
		t.Errorf("SPR Test: Message mismatch on %s. Expected '%s', got '%s'", testChannel, expectedMessageData, receivedMsgData)
	}

	t.Logf("SPR Test: Unsubscribing from %s (ID: %s)...", testChannel, subscription.ID)
	// subscription.Unsubscribe takes no arguments
	if err := subscription.Unsubscribe(); err != nil {
		t.Errorf("SPR Test: Unsubscribe for %s (ID: %s) failed: %v", testChannel, subscription.ID, err)
	}
	t.Logf("SPR Test: Unsubscribed from %s (ID: %s).", testChannel, subscription.ID)

	t.Logf("SPR Test: Closing client...")
	client.Close()
	t.Logf("SPR Test: Client closed for channel %s.", testChannel)
}

