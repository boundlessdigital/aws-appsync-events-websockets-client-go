# AWS AppSync Events WebSocket Client for Go

[![Go Reference](https://pkg.go.dev/badge/github.com/boundlessdigital/aws-appsync-events-websockets-client-go.svg)](https://pkg.go.dev/github.com/boundlessdigital/aws-appsync-events-websockets-client-go)

A Go client library for interacting with the AWS AppSync Events API via WebSockets. This library provides a convenient way to connect to your AppSync Events API, subscribe to channels, publish messages, and manage the WebSocket connection lifecycle with IAM-based authentication.

This client is specifically for the **AWS AppSync Events API**. Ensure you are using the correct service endpoints for the Events protocol.

## Features

*   Connect to AppSync Events API with IAM authentication (AWS Signature Version 4).
*   Subscribe to event channels.
*   Publish messages to event channels.
*   Unsubscribe from event channels.
*   Automatic keep-alive handling.
*   Graceful connection closure.
*   Configurable timeouts and callbacks for various events.
*   Debug logging option.

## Prerequisites

1.  **AWS Account:** You need an active AWS account.
2.  **AppSync Events API:** An AppSync API configured for the Events protocol.
    *   You will need the **AppSync API Host** (e.g., `<api-id>.appsync-api.<region>.amazonaws.com`), the **AppSync Realtime Host** (e.g., `<api-id>.appsync-realtime-api.<region>.amazonaws.com`), and the **AWS Region** (e.g., `us-west-1`) where your API is deployed.
3.  **IAM Permissions:** The AWS credentials used by the client must have an IAM policy attached that grants permissions to interact with your AppSync Events API. This client handles the AWS Signature Version 4 (SigV4) signing process, which targets the `/event` path of your AppSync HTTP endpoint as detailed in the [AWS AppSync Events API WebSocket protocol documentation](https://docs.aws.amazon.com/appsync/latest/eventapi/event-api-websocket-protocol.html).

    A minimal IAM policy should grant the following actions:
    *   `appsync:Connect`: Required to establish the WebSocket connection.
    *   `appsync:Subscribe`: Required to subscribe to specific event channels.
    *   `appsync:PublishEvents`: Required to publish messages to event channels.

    Here is an example policy structure:

    ```json
    {
        "Version": "2012-10-17",
        "Statement": [
            {
                "Effect": "Allow",
                "Action": [
                    "appsync:Connect"
                ],
                "Resource": [
                    "arn:aws:appsync:<region>:<account-id>:apis/<api-id>"
                ]
            },
            {
                "Effect": "Allow",
                "Action": [
                    "appsync:Subscribe",
                    "appsync:PublishEvents"
                ],
                "Resource": [
                    "arn:aws:appsync:<region>:<account-id>:apis/<api-id>/channels/*"
                ]
            }
        ]
    }
    ```
    **Important:**
    *   Replace `<region>`, `<account-id>`, and `<api-id>` with your specific values.
    *   The `appsync:Connect` action typically targets the API ARN itself.
    *   The `appsync:Subscribe` and `appsync:PublishEvents` actions target channel ARNs. You can use wildcards as shown, or restrict permissions to specific channels for finer-grained control.
    *   Always follow the principle of least privilege in your IAM policies.

## Installation

To use this library in your Go project, you can install it using `go get`:

```sh
go get github.com/boundlessdigital/aws-appsync-events-websockets-client-go
```

## Basic Usage

Here's a basic example demonstrating how to use the client:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	appsyncwsclient "github.com/boundlessdigital/aws-appsync-events-websockets-client-go"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Configuration ---
	appsyncAPIHost := "<your-appsync-api-id>.appsync-api.<your-region>.amazonaws.com"
	appsyncRealtimeHost := "<your-appsync-realtime-id>.appsync-realtime-api.<your-region>.amazonaws.com"
	awsRegion := "<your-region>" // e.g., "us-west-1"
	myChannel := "/example/channel"

	// Load AWS configuration (ensure your environment is set up for AWS credentials)
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("Failed to load AWS config: %v", err)
	}

	// --- Client Options ---
	clientOptions := appsyncwsclient.ClientOptions{
		AppSyncAPIHost:      appsyncAPIHost,
		AppSyncRealtimeHost: appsyncRealtimeHost,
		AWSRegion:           awsRegion,
		AWSCfg:             awsCfg,
		Debug:              true, // Enable for detailed logging
		KeepAliveInterval:  2 * time.Minute,
		ReadTimeout:        3 * time.Minute,
		OperationTimeout:   10 * time.Second,

		// --- Callbacks (optional but recommended) ---
		OnConnectionAck: func(msg appsyncwsclient.Message) {
			log.Printf("[CALLBACK] Connection Acknowledged. Timeout: %dms", *msg.ConnectionTimeoutMs)
		},
		OnConnectionError: func(msg appsyncwsclient.Message) {
			log.Printf("[CALLBACK] Connection Error: %s", msg.ToJSONString())
		},
		OnConnectionClose: func(code int, reason string) {
			log.Printf("[CALLBACK] Connection Closed. Code: %d, Reason: %s", code, reason)
		},
		OnKeepAlive: func() {
			log.Println("[CALLBACK] Keep-alive received.")
		},
		OnGenericError: func(errMsg appsyncwsclient.MessageError) {
			log.Printf("[CALLBACK] Generic Error: Type=%s, Message=%s, Code=%v", errMsg.ErrorType, errMsg.Message, errMsg.ErrorCode)
		},
		OnSubscriptionError: func(subscriptionID string, errMsg appsyncwsclient.MessageError) {
			log.Printf("[CALLBACK] Subscription Error for ID '%s': Type=%s, Message=%s, Code=%v",
				subscriptionID, errMsg.ErrorType, errMsg.Message, errMsg.ErrorCode)
		},
	}

	// --- Create Client ---
	client, err := appsyncwsclient.NewClient(clientOptions)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	// --- Connect ---
	log.Println("Connecting to AppSync Events API...")
	if err := client.Connect(ctx); err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	log.Println("Successfully connected!")

	// --- Subscribe ---
	log.Println("Subscribing to channel:", myChannel)
	sub, err := client.Subscribe(ctx, myChannel, func(dataPayload interface{}) {
		// This callback is invoked for each data message received for this subscription.
		log.Printf("[SUBSCRIPTION DATA] Received for '%s': %+v", myChannel, dataPayload)
		// Process the dataPayload here
	})
	if err != nil {
		log.Fatalf("Failed to subscribe to '%s': %v", myChannel, err)
	}
	log.Printf("Successfully subscribed to '%s' with ID: %s", myChannel, sub.ID)

	// The subscription will remain active and the callback will be called for incoming data
	// until sub.Unsubscribe() is called, or the client is closed.

	// --- Publish a message (after a short delay to ensure subscription is active) ---
	time.Sleep(2 * time.Second) // Give some time for subscription to fully establish
	publishPayload := []interface{}{
		map[string]string{"greeting": "Hello from Go client!", "timestamp": time.Now().Format(time.RFC3339Nano)},
	}
	log.Println("Publishing a message to channel:", myChannel)
	err = client.Publish(ctx, myChannel, publishPayload)
	if err != nil {
		log.Printf("Failed to publish: %v", err)
	} else {
		log.Println("Message publish request sent successfully.")
	}

	// --- Wait for a signal to exit or a timeout ---
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case s := <-sigCh:
		log.Printf("Received signal: %s. Shutting down...", s)
	case <-time.After(30 * time.Second): // Example: run for 30 seconds
		log.Println("Timeout reached. Shutting down...")
	}

	// --- Unsubscribe (optional, as client.Close() will also handle this) ---
	log.Printf("Unsubscribing from channel '%s' (ID: %s)...", myChannel, sub.ID)
	if err := sub.Unsubscribe(); err != nil {
		log.Printf("Failed to unsubscribe: %v", err)
	}
	log.Println("Successfully unsubscribed.")

	// --- Close Client ---
	log.Println("Closing client connection...")
	if err := client.Close(); err != nil {
		log.Fatalf("Error closing client: %v", err) // Should be nil if handled correctly
	}
	log.Println("Client closed successfully.")

	// Allow some time for final logs from callbacks if any
	time.Sleep(1 * time.Second)
	log.Println("Application finished.")
}

```

### Finding Your URLs

*   **AppSync API Host (for signing):** In the AWS AppSync console, go to your API, then select "Settings". The "API URL" listed here is typically for GraphQL. For the Events API, the hostname of the base URL (e.g., `<api-id>.appsync-api.<region>.amazonaws.com`) is used for constructing the signing URL.
*   **Realtime Service Host (for WebSocket connection):** This is often in the format `<api-id>.appsync-realtime-api.<region>.amazonaws.com`. You might also find this in the "Settings" page of your AppSync API in the console, sometimes labeled as "Realtime endpoint" or similar (ensure it's for the `/event/realtime` path).

## ClientOptions

The `ClientOptions` struct allows you to configure the client's behavior:

*   `AppSyncAPIHost` (string, required): The hostname of your AppSync Events API (e.g., `<id>.appsync-api.<region>.amazonaws.com`).
*   `AppSyncRealtimeHost` (string, required): The hostname of your AppSync Events API's realtime endpoint (e.g., `<id>.appsync-realtime-api.<region>.amazonaws.com`).
*   `AWSRegion` (string, required): The AWS region where your AppSync API is deployed (e.g., `us-west-1`).
*   `AWSCfg` (aws.Config, required): AWS SDK v2 configuration, used for IAM authentication. Ensure it's configured with credentials that have the necessary AppSync permissions.
*   `ConnectionInitPayload` (interface{}, optional): Payload for the `connection_init` message. Defaults to an empty object `{}` if nil, which is standard for IAM auth.
*   `Debug` (bool, optional): Set to `true` to enable detailed debug logging from the client.
*   `ReadTimeout` (time.Duration, optional): Maximum time to wait for a read from the WebSocket. Defaults to **15 minutes**.
*   `KeepAliveInterval` (time.Duration, optional): Interval for sending keep-alive messages. Defaults to **2 minutes**. This should be shorter than server-side idle timeouts (often around 10 minutes) and the `ReadTimeout`. AppSync expects keep-alive pings, and the connection will be closed if they are not sent.
*   `OperationTimeout` (time.Duration, optional): Default timeout for operations like subscribe, publish, etc. Defaults to **15 minutes**.
*   **Callbacks:**
    *   `OnConnectionAck func(msg Message)`: Called when a `connection_ack` is received.
    *   `OnConnectionError func(msg Message)`: Called for AppSync `connection_error` messages.
    *   `OnConnectionClose func(code int, reason string)`: Called when the WebSocket connection closes for any reason.
    *   `OnKeepAlive func()`: Called when a `ka` (keep-alive) message is received from the server.
    *   `OnGenericError func(errMsg MessageError)`: Called for generic AppSync error messages (e.g., if the server sends an `error` type message not tied to a specific operation ID).
    *   `OnSubscriptionError func(subscriptionID string, errMsg MessageError)`: Called for subscription-specific errors that are not part of the `Subscribe()` call's direct error return (e.g., a `broadcast_error` from the server).

## Working with Subscriptions

The `client.Subscribe()` method returns a `*Subscription` object and an `error`. 

*   `*Subscription`: This object represents your active subscription. You will primarily use its `Unsubscribe()` method to stop receiving messages and clean up resources associated with this specific subscription.
    *   `sub.ID() string`: Returns the unique ID of the subscription.
    *   `sub.Unsubscribe() error`: Sends an unsubscribe message to AppSync and cleans up local resources. It's good practice to call this when you no longer need the subscription, though `client.Close()` will also attempt to unsubscribe from all active subscriptions.
    *   `sub.Done() <-chan struct{}`: Returns a channel that is closed when the subscription is definitively terminated (either by `Unsubscribe()`, client closure, or an unrecoverable error causing the subscription's context to be cancelled). You can use this to wait for a subscription to fully terminate.
    *   `sub.Err() error`: If `Done()` is closed due to an error, `Err()` will return the error that caused the termination. Otherwise, it returns `nil`.
    *   `sub.Data() <-chan interface{}`: If you did *not* provide an `on_data` callback to `client.Subscribe()`, you can use this method to get a read-only channel from which to consume incoming data messages for the subscription. If an `on_data` callback *was* provided, this channel will not receive data (as it's sent to the callback instead).

*   `error`: The error returned directly by `client.Subscribe()` indicates a problem during the initial subscription attempt (e.g., client not connected, failure to send the subscribe message, or a timeout waiting for the subscribe acknowledgment from the server).

Data for an active subscription is delivered via the `on_data func(data_payload interface{})` callback you provide to `client.Subscribe()`. Ensure this callback can handle concurrent invocations if your processing is lengthy, or offload work to other goroutines.

## Error Handling

*   Methods like `Connect`, `Subscribe`, `Publish`, and `Unsubscribe` can return errors directly if the initial part of the operation fails (e.g., invalid parameters, immediate connection issues, failure to get an acknowledgment within the timeout).
*   Asynchronous errors (e.g., connection drops, server-sent errors not tied to an immediate request-response) are delivered via the callbacks defined in `ClientOptions` and `SubscriptionOptions`.
*   The `Client.Close()` method is designed to handle the common "use of closed network connection" error internally by treating it as a successful closure (returning `nil`), as the connection does end up closed. The `OnConnectionClose` callback will still reflect the underlying reason provided by the WebSocket library.

## Debug Logging

Set `ClientOptions.Debug = true` to enable verbose logging from the client, which can be helpful for troubleshooting connection and message flow issues.

## Changelog

### v0.2.0 - 2025-05-21

*   **Breaking Change:** Updated `ClientOptions` to accept `AppSyncAPIHost` (string), `AppSyncRealtimeHost` (string), and `AWSRegion` (string) instead of full `AppSyncAPIURL` and `RealtimeServiceURL`. The client now constructs the full URLs internally. This change simplifies configuration and reduces potential URL formatting errors. Refer to the "Basic Usage" and "ClientOptions" sections for updated examples.
*   Refactored subscription management logic into a separate `subscription.go` file for better code organization.
*   Updated all tests and the test application to align with the new `ClientOptions` structure.

## Contributing

Contributions are welcome! Please feel free to submit issues and pull requests.

## License

This project is licensed under the MIT License.

Copyright (c) 2025 Sidney Burks (sidney@boundlessdigital.com)

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
