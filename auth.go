package appsyncwsclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	signerV4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// Signer defines an interface for signing HTTP requests, compatible with aws.v4.Signer.
// This allows for mocking the signer in tests.
// Note: The `opts ...func(*signerV4.SignerOptions)` parameter uses the imported package alias `signerV4`.
// If client.go uses a different alias (e.g., `signer`), ensure consistency or use the full path.
// However, since they are in the same package, the type `Signer` will be available directly.
// The method signature uses `signerV4` as per its definition in `aws-sdk-go-v2`.
// The `Client.signer` field will be of this type `Signer`.
// The `NewClient` method in `client.go` uses `signer.NewSigner()`, which should resolve to `aws/signer/v4.NewSigner()`
// if `signer` is aliased to `"github.com/aws/aws-sdk-go-v2/aws/signer/v4"` in `client.go`.
// For clarity, and since `auth.go` uses `signerV4`, the interface definition here will use `signerV4`.
type Signer interface {
	SignHTTP(ctx context.Context, creds aws.Credentials, r *http.Request, bodyHash string, service string, region string, t time.Time, opts ...func(*signerV4.SignerOptions)) error
}

func (c *Client) create_signed_headers_for_operation(ctx context.Context, payload map[string]interface{}) (map[string]string, error) {
	// For IAM authorization of operations, the body to be signed depends on the operation.
	// Subscribe: {"channel":"<channel_name>"}
	// Publish:   {"channel":"<channel_name>", "events":["<event1_json_string>", ...] }
	// Documentation (Chunk 9 for IAM Subscribe & Publish)
	// URL for signing: https://<host>/event
	// Headers for signing request: accept, content-encoding, content-type, host

	// Construct the body for signing. This is the JSON marshalled 'payload'.
	// For a subscribe operation with empty data, payload is map[string]interface{}{}, which marshals to "{}".
	// For a publish operation, payload would be map[string]interface{}{"events": ["event_data_string"]}, marshaling to {"events":["event_data_string"]}.
	body_for_signing_bytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("error marshalling payload for signing: %w", err)
	}

	parsed_url, err := url.Parse(c.appsyncAPIURLInternal)
	if err != nil {
		return nil, fmt.Errorf("error parsing AppSync API URL: %w", err)
	}
	// Signing URL path must be /event for operations as per documentation
	signing_url_str := fmt.Sprintf("%s://%s/event", parsed_url.Scheme, parsed_url.Host)

	req, err := http.NewRequest("POST", signing_url_str, bytes.NewReader(body_for_signing_bytes))
	if err != nil {
		return nil, fmt.Errorf("error creating new request for signing: %w", err)
	}

	// Set headers that are part of the SigV4 signature calculation for the HTTP request.
	// These headers must be present for the signing process as per AppSync documentation.
	req.Header.Set("Host", parsed_url.Host) // Revert to using Set for consistent Header map access
	req.Header.Set("Accept", "application/json, text/javascript")
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("Content-Encoding", "amz-1.0") // As per documentation (Chunk 9)

	// Get AWS credentials
	creds, err := c.options.AWSCfg.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve AWS credentials for operation: %w", err)
	}

	// Calculate payload hash for SigV4
	hash := sha256.Sum256(body_for_signing_bytes)
	hex_hash := hex.EncodeToString(hash[:])

	err = c.signer.SignHTTP(ctx, creds, req, hex_hash, "appsync", c.options.AWSRegion, time.Now())
	if err != nil {
		return nil, fmt.Errorf("error signing request: %w", err)
	}

	// Construct the auth_headers map for the 'authorization' object in the WebSocket message.
	// Documentation (Chunk 11 for IAM operation authorization fields) states this should contain:
	// host, x-amz-date, x-amz-security-token, and authorization.
	auth_headers := make(map[string]string)

	// Populate according to the documentation's 'Authorization header example' (Chunk 9).
	// Keys must match the JSON example for case-sensitivity.
	auth_headers["accept"] = req.Header.Get("Accept")
	auth_headers["content-encoding"] = req.Header.Get("Content-Encoding")
	auth_headers["content-type"] = req.Header.Get("Content-Type")
	auth_headers["host"] = req.Header.Get("Host") // This was parsed_url.Host
	auth_headers["x-amz-date"] = req.Header.Get("X-Amz-Date")

	if session_token := req.Header.Get("X-Amz-Security-Token"); session_token != "" {
		auth_headers["X-Amz-Security-Token"] = session_token // Case as per documentation
	}
	auth_headers["Authorization"] = req.Header.Get("Authorization") // Case as per documentation

	c.logf("Signed headers for operation authorization field: %v", auth_headers)
	return auth_headers, nil
}

func (c *Client) create_connection_auth_subprotocol(ctx context.Context) ([]string, error) {
	// Per AppSync Events API IAM documentation, sign against the HTTP API endpoint.
	parsedAPIURL, err := url.Parse(c.appsyncAPIURLInternal) // e.g., https://<id>.appsync-api.<region>.amazonaws.com/event
	if err != nil {
		return nil, fmt.Errorf("failed to parse AppSync API URL: %w", err)
	}

	// The body for signing the connection request is typically an empty object for IAM auth.
	body := "{}"
	bodyBytes := []byte(body)

	// Construct the signing URL, ensuring the path is /event for the WebSocket handshake signature
	signingURL := url.URL{
		Scheme: parsedAPIURL.Scheme,
		Host:   parsedAPIURL.Host,
		Path:   "/event",
	}
	signingURLString := signingURL.String()

	// Create the request for signing using the constructed signingURLString.
	req, err := http.NewRequestWithContext(ctx, "POST", signingURLString, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request for signing: %w", err)
	}

	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("Accept", "application/json, text/javascript")
	req.Header.Set("Content-Encoding", "amz-1.0") // As per AppSync client examples
	// Host header for signing must match the host of AppSyncAPIURL.
	req.Host = parsedAPIURL.Host
	req.Header.Set("Content-Length", strconv.Itoa(len(bodyBytes)))

	// Sign the request
	// Calculate SHA256 hash of the payload for signing
	hash := sha256.Sum256(bodyBytes)
	hexHash := fmt.Sprintf("%x", hash)

	creds, err := c.options.AWSCfg.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve AWS credentials: %w", err)
	}

	err = c.signer.SignHTTP(ctx, creds, req, hexHash, "appsync", c.options.AWSRegion, time.Now())
	if err != nil {
		return nil, fmt.Errorf("failed to sign HTTP request: %w", err)
	}

	c.logf("Signed HTTP Headers for WebSocket handshake:")
	for k, v := range req.Header {
		c.logf("  %s: %s", k, strings.Join(v, ", "))
	}

	// Construct the handshakeHeaders JSON payload with exact key casing
	handshakeHeaders := make(map[string]string)
	if val := req.Header.Get("Accept"); val != "" { handshakeHeaders["accept"] = val }
	if val := req.Header.Get("Content-Encoding"); val != "" { handshakeHeaders["content-encoding"] = val }
	if val := req.Header.Get("Content-Type"); val != "" { handshakeHeaders["content-type"] = val }
	if req.Host != "" { handshakeHeaders["host"] = req.Host } // Use req.Host, not req.Header.Get("Host")
	if val := req.Header.Get("X-Amz-Date"); val != "" { handshakeHeaders["x-amz-date"] = val }
	if val := req.Header.Get("Authorization"); val != "" { handshakeHeaders["Authorization"] = val }
	if sessionToken := req.Header.Get("X-Amz-Security-Token"); sessionToken != "" {
		handshakeHeaders["X-Amz-Security-Token"] = sessionToken
	}

	c.logf("handshakeHeaders map for JSON marshal: %v", handshakeHeaders)

	jsonHeaders, err := json.Marshal(handshakeHeaders)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal handshake headers: %w", err)
	}

	encodedHeaders := base64.RawURLEncoding.EncodeToString(jsonHeaders) // No padding
	subprotocol := fmt.Sprintf("header-%s", encodedHeaders)
	return []string{subprotocol, "aws-appsync-event-ws"}, nil
}
