package main

import (
	"context"
	"fmt" // Added for shutdown_reason formatting
	"log"
	"os"
	"os/signal"
	"syscall"
	"time" // Added for timestamp in publish

	"github.com/aws/aws-sdk-go-v2/config"
	appsync "github.com/boundless-exports/aws-appsync-events-websockets-client-go"
)

// --- Configuration: Replace with your actual AppSync API details and AWS profile ---
const (
	// AppSyncAPIURL is the HTTP URL of your AppSync Events API (e.g., https://<id>.appsync-api.<region>.amazonaws.com/event)
	appSyncAPIURLConfig = "https://gk3gfluct5azlhi5d3rnumxoq4.appsync-api.us-west-1.amazonaws.com/event"
	// AppSyncRealtimeURL is the WebSocket URL of your AppSync Events API (e.g., wss://<id>.appsync-realtime-api.<region>.amazonaws.com/event/realtime)
	appSyncRealtimeURLConfig = "wss://gk3gfluct5azlhi5d3rnumxoq4.appsync-realtime-api.us-west-1.amazonaws.com/event/realtime"
	// AWSRegion is the AWS region where your AppSync API is deployed.
	awsRegionConfig = "us-west-1"
	// AWSProfile is the AWS shared configuration profile to use for credentials.
	awsProfileConfig = "boundless-development"
	// TestChannel is an example channel name to use for subscribe/publish operations.
	testChannelConfig = "/live-lambda/goclient-publish-test"
)

func main() {
	log.Println("Starting AppSync WebSocket client test application...")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Load AWS SDK config
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(awsRegionConfig),
		config.WithSharedConfigProfile(awsProfileConfig),
	)
	if err != nil {
		log.Fatalf("Failed to load AWS configuration: %v", err)
	}

	log.Printf("AWS Config loaded successfully using profile: %s, region: %s", awsProfileConfig, awsRegionConfig)

	clientOpts := appsync.ClientOptions{
		AppSyncAPIURL:      appSyncAPIURLConfig,
		RealtimeServiceURL: appSyncRealtimeURLConfig,
		AWSCfg:             awsCfg,
		Debug:              true, // Enable debug logging from the client
		OnConnectionAck: func(msg appsync.Message) {
			if msg.ConnectionTimeoutMs != nil {
				log.Printf("[CALLBACK] Connection ACK received. Timeout hint: %dms", *msg.ConnectionTimeoutMs)
			} else {
				log.Printf("[CALLBACK] Connection ACK received. No timeout hint provided.")
			}
		},
		OnKeepAlive: func() {
			log.Println("[CALLBACK] Keep-alive received.")
		},
		OnConnectionError: func(msg appsync.Message) {
			log.Printf("[CALLBACK] Connection Error Message: %+v", msg)
			if len(msg.Errors) > 0 {
				log.Printf("[CALLBACK] Connection Error details: %s - %s", msg.Errors[0].ErrorType, msg.Errors[0].Message)
			}
		},
		OnConnectionClose: func(code int, reason string) {
			log.Printf("[CALLBACK] Connection Closed. Code: %d, Reason: %s", code, reason)
		},
		OnGenericError: func(errMsg appsync.MessageError) {
			log.Printf("[CALLBACK] Generic AppSync Error: %s - %s", errMsg.ErrorType, errMsg.Message)
		},
		OnSubscriptionError: func(subscriptionID string, errMsg appsync.MessageError) {
			log.Printf("[CALLBACK] Subscription Error for %s: %s - %s", subscriptionID, errMsg.ErrorType, errMsg.Message)
		},
	}

	client, err := appsync.NewClient(clientOpts)
	if err != nil {
		log.Fatalf("Failed to create AppSync client: %v", err)
	}

	log.Println("AppSync client created. Attempting to connect...")
	if err := client.Connect(ctx); err != nil {
		log.Fatalf("Failed to connect to AppSync: %v", err)
	}
	log.Println("Successfully connected to AppSync.")

	// Channel to signal that a message has been received by the subscription
	message_received_signal := make(chan bool, 1)

	// Subscribe to a test channel
	log.Printf("Attempting to subscribe to channel: %s", testChannelConfig)
	subscription, err := client.Subscribe(ctx, testChannelConfig, func(data_payload interface{}) {
		log.Printf("[CALLBACK] Data received for subscription to '%s': %+v", testChannelConfig, data_payload)
		// Signal that message was received
		select {
		case message_received_signal <- true:
		default: // Avoid blocking if channel is already full or closed
		}
	})

	if err != nil {
		log.Printf("Failed to subscribe to channel '%s': %v", testChannelConfig, err)
		// We might not want to proceed if subscription fails.
		cancel() // Trigger shutdown
	} else {
		log.Printf("Successfully subscribed to channel '%s' with ID: %s", testChannelConfig, subscription.ID)

		// Publish a test message after successful subscription
		log.Printf("Attempting to publish to channel: %s", testChannelConfig)
		publish_payload := []interface{}{
			map[string]interface{}{
				"greeting":  "Hello from test app!",
				"timestamp": time.Now().Format(time.RFC3339Nano),
				"source":    "GoTestApp-Publish",
			},
		}
		err = client.Publish(ctx, testChannelConfig, publish_payload)
		if err != nil {
			log.Printf("Failed to publish to channel '%s': %v", testChannelConfig, err)
			// Consider if this should also trigger shutdown
		} else {
			log.Printf("Successfully sent publish request to channel '%s'", testChannelConfig)
		}
	}

	// Wait for termination signal or for the message to be received
	sig_chan := make(chan os.Signal, 1)
	signal.Notify(sig_chan, syscall.SIGINT, syscall.SIGTERM)

	log.Println("Application running. Waiting for published message or Ctrl+C to exit.")

	shutdown_reason := "unknown"

	select {
	case <-message_received_signal:
		log.Println("Published message successfully received by subscription. Proceeding to shutdown.")
		shutdown_reason = "Message received"
	case s := <-sig_chan:
		log.Printf("Received signal: %s. Shutting down...", s)
		shutdown_reason = fmt.Sprintf("Signal %s", s)
	case <-ctx.Done(): // If main context is cancelled for other reasons
		log.Println("Main context cancelled. Shutting down...")
		shutdown_reason = "Main context cancelled"
	case <-time.After(30 * time.Second): // Timeout if no message received or signal
		log.Println("Timeout waiting for message or signal. Shutting down.")
		shutdown_reason = "Timeout"
	}

	log.Printf("Initiating shutdown sequence. Reason: %s", shutdown_reason)
	cancel() // Ensure main context is cancelled to stop other goroutines

	// Graceful shutdown
	if subscription != nil && subscription.ID != "" { // Check if subscription was successful
		log.Printf("Unsubscribing from channel %s (ID: %s)...", testChannelConfig, subscription.ID)
		if err_unsub := subscription.Unsubscribe(); err_unsub != nil {
			log.Printf("Error during unsubscribe for %s: %v", subscription.ID, err_unsub)
		} else {
			log.Printf("Successfully unsubscribed from %s.", subscription.ID)
		}
	}

	log.Println("Closing AppSync client connection...")
	if err_close := client.Close(); err_close != nil {
		log.Printf("Error closing AppSync client: %v", err_close)
	}

	log.Println("Shutdown complete.")
}
