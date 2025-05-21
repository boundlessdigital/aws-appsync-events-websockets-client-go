package appsyncwsclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	signerV4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSigner implements the Signer interface for testing.
// It allows us to control the behavior of SignHTTP and inspect its inputs.
type mockSigner struct {
	SignHTTPFunc func(ctx context.Context, creds aws.Credentials, r *http.Request, bodyHash string, service string, region string, t time.Time, opts ...func(*signerV4.SignerOptions)) error
	// Store last call arguments for verification
	LastRequest  *http.Request
	LastBodyHash string
	LastService  string
	LastRegion   string
}

func (m *mockSigner) SignHTTP(ctx context.Context, creds aws.Credentials, r *http.Request, bodyHash string, service string, region string, t time.Time, opts ...func(*signerV4.SignerOptions)) error {
	m.LastRequest = r
	m.LastBodyHash = bodyHash
	m.LastService = service
	m.LastRegion = region
	if m.SignHTTPFunc != nil {
		return m.SignHTTPFunc(ctx, creds, r, bodyHash, service, region, t, opts...)
	}
	// Default mock behavior: simulate successful signing by populating required headers
	r.Header.Set("Authorization", "mock-auth-header")
	r.Header.Set("X-Amz-Date", "mock-date-header")
	return nil
}

func TestCreateConnectionAuthSubprotocol_Success(t *testing.T) {
	mockSig := &mockSigner{}

	clientOpts := ClientOptions{
		AppSyncAPIHost:      "example123.appsync-api.us-west-1.amazonaws.com",
		AppSyncRealtimeHost: "example123.appsync-realtime-api.us-west-1.amazonaws.com",
		AWSRegion:           "us-west-1",
		AWSCfg: aws.Config{
			// Region field in AWSCfg is still useful for the SDK to know the default region for services if not overridden,
			// but our client specifically uses AWSRegion from ClientOptions for signing AppSync requests.
			Region: "us-west-1", 
			Credentials: credentials.NewStaticCredentialsProvider("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "mockSessionToken"),
		},
		Debug:      true,
		TestLogger: t, // Use testing.T for logs
	}

	client, err := NewClient(clientOpts)
	require.NoError(t, err, "NewClient should not return an error")
	client.signer = mockSig // Inject mock signer

	ctx := context.Background()
	subprotocols, err := client.create_connection_auth_subprotocol(ctx)

	assert.NoError(t, err, "create_connection_auth_subprotocol should not return an error")
	require.Len(t, subprotocols, 2, "Expected 2 subprotocols")
	assert.True(t, strings.HasPrefix(subprotocols[0], "header-"), "First subprotocol should start with 'header-'" )
	assert.Equal(t, "aws-appsync-event-ws", subprotocols[1], "Second subprotocol should be 'aws-appsync-event-ws'")

	// Verify mockSigner was called with expected parameters
	require.NotNil(t, mockSig.LastRequest, "SignHTTP should have been called")
	assert.Equal(t, "POST", mockSig.LastRequest.Method, "HTTP method for signing should be POST")
	assert.Equal(t, "/event", mockSig.LastRequest.URL.Path, "URL path for signing should be /event")
	assert.Equal(t, "example123.appsync-api.us-west-1.amazonaws.com", mockSig.LastRequest.URL.Host, "Host for signing should match AppSyncAPIURL host")
	// Expect body to be "{}", which hashes to "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
	assert.Equal(t, "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a", mockSig.LastBodyHash, "Body hash should be for '{}'")
	assert.Equal(t, "appsync", mockSig.LastService, "Service name for signing should be 'appsync'")
	assert.Equal(t, "us-west-1", mockSig.LastRegion, "Region for signing should match config")

	t.Logf("Successfully created subprotocols: %v", subprotocols)
}

func TestClient_create_signed_headers_for_operation_Subscribe_IAM(t *testing.T) {
	ctx := context.Background()
	apiURLHost := "example123.appsync-api.us-east-1.amazonaws.com" // Just the host part
	region := "us-east-1"
	accessKeyID := "AKIAIOSFODNN7EXAMPLE"
	secretAccessKey := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	sessionToken := "AQoDYXdzEPT//////////wEXAMPLEtc764bNrC9SAPBSM22wDOk4x4HIZ8j4FZTwdQWMAEXAMPLECMW2gKWTCGYufrsOFyL1jy1DakeP7t7DMXwNhOYHT3sq6K2Y2Y+lgpDT10GvCVfKjL/KSINtSiYGIj3j2Y8/P8oAYuA/c1GrmKRgL4jAYuA/c1GrmKRgL4jAYuA/c1GrmKRgL4jAYuA/c1GrmKRgL4jAYuA/c1GrmKRgL4jAYuA/c2ZpQAAAQBEXAMPLEDEZxVjGud203gojWBVOqXnOImP3P3cHiPIJGaXL2mN2Y089kjqcpE80m5cea3ryVCSyAd7a610ik7B2W3zYyORYwHrmZz2Pff2Pff2Pff2Pff2Pff2Pff2Pff2Pff2Pff2Pff2Pff2Pff2V3LzAAAAAQEABA=="

	credsProvider := credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, sessionToken)
	parsedAPIURL, _ := url.Parse("https://" + apiURLHost + "/event")
	expectedHost := parsedAPIURL.Host
	// create_signed_headers_for_operation always signs for the "/event" path.
	expectedPathForSigning := "/event"
	fakeTimestamp := "20250101T000000Z"
	fakeSignatureString := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/20250101/%s/appsync/aws4_request, SignedHeaders=accept;content-encoding;content-type;host;x-amz-date;x-amz-security-token, Signature=fakesignature", accessKeyID, region)

	testChannelName := "/test/channel/iam"
	subscribe_operation_payload := map[string]interface{}{"channel": testChannelName}
	bodyBytes, err := json.Marshal(subscribe_operation_payload)
	require.NoError(t, err, "Failed to marshal subscribe_operation_payload")
	expectedBodySHA256 := fmt.Sprintf("%x", sha256.Sum256(bodyBytes))

	// Use the mockSigner from auth_test.go
	// Provide a SignHTTPFunc to control the headers set on the request,
	// so that existing assertions on auth_object can largely remain.
	mockSig := &mockSigner{ // This now refers to the mockSigner from auth_test.go
		SignHTTPFunc: func(ctxArg context.Context, credsArg aws.Credentials, rArg *http.Request, bodyHashArg string, serviceArg string, regionArg string, tArg time.Time, optsArg ...func(*signerV4.SignerOptions)) error {
			// Set headers on rArg as the test expects them to appear in the final auth_object
			rArg.Header.Set("Authorization", fakeSignatureString)
			rArg.Header.Set("X-Amz-Date", fakeTimestamp)
			if sessionToken != "" {
				rArg.Header.Set("X-Amz-Security-Token", sessionToken)
			}
			// Other headers (Accept, Content-Type, Content-Encoding) are set by
			// create_signed_headers_for_operation *before* SignHTTP is called.
			// Host is also derived from rArg.URL.Host by the calling function.
			return nil
		},
	}

	c := &Client{
		options: ClientOptions{ // Options as they would be passed by the user
			AppSyncAPIHost:      apiURLHost, // apiURLHost should be defined as just the host for this test
			AWSRegion:           region,
			AWSCfg: aws.Config{
				Region:      region, // SDK config region
				Credentials: credsProvider,
			},
		},
		// Internal fields that NewClient would have set
		// Construct the internal API URL as NewClient would
		appsyncAPIURLInternal: "https://" + apiURLHost + "/event",
		signer: mockSig, // Inject the mock signer
	}

	// The payload passed to create_signed_headers_for_operation for a subscribe includes the channel name.
	auth_object, err := c.create_signed_headers_for_operation(ctx, subscribe_operation_payload)

	// ---- Assertions for what was passed TO the mock signer's SignHTTP method ----
	require.NotNil(t, mockSig.LastRequest, "SignHTTP should have been called on mockSigner")
	if mockSig.LastRequest.URL.Host != expectedHost {
		t.Errorf("mockSigner received incorrect host: expected %s, got %s", expectedHost, mockSig.LastRequest.URL.Host)
	}
	if mockSig.LastRequest.URL.Path != expectedPathForSigning {
		t.Errorf("mockSigner received incorrect path: expected %s, got %s", expectedPathForSigning, mockSig.LastRequest.URL.Path)
	}
	if mockSig.LastBodyHash != expectedBodySHA256 {
		t.Errorf("mockSigner received incorrect body hash: expected %s, got %s (body signed: '%s')", expectedBodySHA256, mockSig.LastBodyHash, string(bodyBytes))
	}
	if mockSig.LastService != "appsync" {
		t.Errorf("mockSigner received incorrect service: expected 'appsync', got '%s'", mockSig.LastService)
	}
	if mockSig.LastRegion != region {
		t.Errorf("mockSigner received incorrect region: expected '%s', got '%s'", region, mockSig.LastRegion)
	}

			// ---- Existing assertions on the returned auth_object (populated from request headers AFTER signing) ----
			if err != nil {
				t.Fatalf("create_signed_headers_for_operation returned error: %v", err)
			}

			expectedKeysInAuthObject := map[string]bool{
				"accept":               true,
				"content-encoding":     true,
				"content-type":         true,
				"host":                 true,
				"x-amz-date":           true,
				"Authorization":        true,
			}
			if sessionToken != "" {
				expectedKeysInAuthObject["X-Amz-Security-Token"] = true
			}

			if len(auth_object) != len(expectedKeysInAuthObject) {
				t.Errorf("auth_object key count mismatch: expected %d, got %d. Object: %v", len(expectedKeysInAuthObject), len(auth_object), auth_object)
			}
			for k := range expectedKeysInAuthObject {
				if _, exists := auth_object[k]; !exists {
					t.Errorf("auth_object missing expected key: %s. Object: %v", k, auth_object)
				}
			}

			if val, ok := auth_object["host"]; !ok || val != expectedHost {
				t.Errorf("auth_object host mismatch: expected %s, got %v", expectedHost, val)
			}
			if val, ok := auth_object["x-amz-date"]; !ok || val != fakeTimestamp {
				t.Errorf("auth_object x-amz-date mismatch: expected %s, got %v", fakeTimestamp, val)
			}
			if sessionToken != "" {
				if val, ok := auth_object["X-Amz-Security-Token"]; !ok || val != sessionToken {
					t.Errorf("auth_object X-Amz-Security-Token mismatch: expected %s, got %v", sessionToken, val)
				}
			} else {
				if _, exists := auth_object["X-Amz-Security-Token"]; exists {
					t.Errorf("auth_object should not contain X-Amz-Security-Token when no session token is provided, but got: %v", auth_object["X-Amz-Security-Token"])
				}
			}
			if val, ok := auth_object["Authorization"]; !ok || val != fakeSignatureString {
				t.Errorf("auth_object Authorization mismatch: expected %s, got %v", fakeSignatureString, val)
			}

			// Check standard headers that create_signed_headers_for_operation should ensure are in auth_object
			if val, ok := auth_object["accept"]; !ok || val != "application/json, text/javascript" {
				t.Errorf("auth_object accept header mismatch: expected 'application/json, text/javascript', got %v", val)
			}
			if val, ok := auth_object["content-encoding"]; !ok || val != "amz-1.0" {
				t.Errorf("auth_object content-encoding header mismatch: expected 'amz-1.0', got %v", val)
			}
			if val, ok := auth_object["content-type"]; !ok || val != "application/json; charset=UTF-8" {
				t.Errorf("auth_object content-type header mismatch: expected 'application/json; charset=UTF-8', got %v", val)
			}

}
