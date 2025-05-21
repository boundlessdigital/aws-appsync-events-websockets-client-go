package appsyncwsclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	// "log"             // Removed unused import
	"net/http"
	"net/url"
	// "os"              // Removed unused import
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4" // Added for v4.SignerOptions
)

// mockSigner is a simple mock for the v4.Signer for predictable test outcomes.
// For more complex scenarios, a more sophisticated mock or interface might be needed.
type mockSigner struct {
	ExpectedHost             string
	ExpectedCanonicalURI     string
	ExpectedBodyHash         string
	ExpectedSecurityToken    string
	ExpectedSignature        string
	ExpectedXAmzDate         string
	ExpectedSignedHeaders    string
}

func (m *mockSigner) SignHTTP(ctx context.Context, creds aws.Credentials, r *http.Request, bodyHash string, service string, region string, t time.Time, opts ...func(*v4.SignerOptions)) error {
	if r.URL.Host != m.ExpectedHost {
		return fmt.Errorf("mockSigner: host mismatch. Expected %s, got %s", m.ExpectedHost, r.URL.Host)
	}
	if r.URL.Path != m.ExpectedCanonicalURI { // Path should be CanonicalURI for SigV4
		return fmt.Errorf("mockSigner: path (CanonicalURI) mismatch. Expected %s, got %s", m.ExpectedCanonicalURI, r.URL.Path)
	}
	if bodyHash != m.ExpectedBodyHash {
		return fmt.Errorf("mockSigner: bodyHash mismatch. Expected %s, got %s", m.ExpectedBodyHash, bodyHash)
	}
	// Simulate signing by setting expected headers
	r.Header.Set("Authorization", m.ExpectedSignature)
	r.Header.Set("X-Amz-Date", m.ExpectedXAmzDate)
	if m.ExpectedSecurityToken != "" {
		r.Header.Set("X-Amz-Security-Token", m.ExpectedSecurityToken)
	}
	// The actual SigV4 adds SignedHeaders component to the Authorization header itself, not as a separate header.
	// We'll ensure the Authorization header string contains it.
	if !strings.Contains(m.ExpectedSignature, "SignedHeaders="+m.ExpectedSignedHeaders) {
		return fmt.Errorf("mockSigner: ExpectedSignature should contain SignedHeaders=%s", m.ExpectedSignedHeaders)
	}
	return nil
}

func TestClient_create_signed_headers_for_operation_Subscribe_IAM(t *testing.T) {
	ctx := context.Background()
	apiURL := "https://example123.appsync-api.us-east-1.amazonaws.com/graphql" // Base URL, path will be overridden
	region := "us-east-1"
	accessKeyID := "AKIAIOSFODNN7EXAMPLE"
	secretAccessKey := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	sessionToken := "AQoDYXdzEPT//////////wEXAMPLEtc764bNrC9SAPBSM22wDOk4x4HIZ8j4FZTwdQWMAEXAMPLECMW2gKWTCGYufrsOFyL1jy1DakeP7t7DMXwNhOYHT3sq6K2Y2Y+lgpDT10GvCVfKjL/KSINtSiYGIj3j2Y8/P8oAYuA/c1GrmKRgL4jAYuA/c1GrmKRgL4jAYuA/c1GrmKRgL4jAYuA/c1GrmKRgL4jAYuA/c1GrmKRgL4jAYuA/c2ZpQAAAQBEXAMPLEDEZxVjGud203gojWBVOqXnOImP3P3cHiPIJGaXL2mN2Y089kjqcpE80m5cea3ryVCSyAd7a610ik7B2W3zYyORYwHrmZz2Pff2Pff2Pff2Pff2Pff2Pff2Pff2Pff2Pff2Pff2Pff2Pff2V3LzAAAAAQEABA=="

	credsProvider := credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, sessionToken)
	parsedAPIURL, _ := url.Parse(apiURL)
	expectedHost := parsedAPIURL.Host
	expectedPath := "/event"
	fakeTimestamp := "20250101T000000Z"
	fakeSignatureBase := "AWS4-HMAC-SHA256 Credential=%s/20250101/%s/appsync/aws4_request, SignedHeaders=accept;content-encoding;content-type;host;x-amz-date;x-amz-security-token, Signature=fakesignature"

	tests := []struct {
		name                 string
		inputChannelName     string
		expectedChannelForSign string // Channel name as it should appear in the signed payload JSON
	}{
		{
			name:                 "channel name without leading slash",
			inputChannelName:     "test/channel",
			expectedChannelForSign: "/test/channel",
		},
		{
			name:                 "channel name with leading slash",
			inputChannelName:     "/test/channel",
			expectedChannelForSign: "/test/channel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Body for signing depends on the expectedChannelForSign
			bodyPayload := map[string]string{"channel": tt.expectedChannelForSign}
			bodyBytes, _ := json.Marshal(bodyPayload)
			expectedBodySHA256 := fmt.Sprintf("%x", sha256.Sum256(bodyBytes))

			// The mockSigner's ExpectedBodyHash needs to be updated for each test case
			// The mockSigner will be called by Subscribe, which in turn calls create_signed_headers_for_operation.
			// The actual channel name formatting happens in Subscribe before calling create_signed_headers_for_operation.
			// So, operation_payload for create_signed_headers_for_operation will use tt.expectedChannelForSign.
			
			fakeSignatureString := fmt.Sprintf(fakeSignatureBase, accessKeyID, region)

			mockSig := &mockSigner{
				ExpectedHost:          expectedHost,
				ExpectedCanonicalURI:  expectedPath,
				ExpectedBodyHash:      expectedBodySHA256, // This is key for testing the body
				ExpectedSecurityToken: sessionToken,
				ExpectedSignature:     fakeSignatureString,
				ExpectedXAmzDate:      fakeTimestamp,
				ExpectedSignedHeaders: "accept;content-encoding;content-type;host;x-amz-date;x-amz-security-token",
			}

			c := &Client{
				options: ClientOptions{
					AppSyncAPIURL: apiURL,
					AWSCfg: aws.Config{
						Region:      region,
						Credentials: credsProvider,
					},
					// Debug: true, // Can enable for specific test debugging
				},
				signer: mockSig,
			}

			// The payload passed to create_signed_headers_for_operation for subscribe.
			// This payload should use the channel name *after* formatting (i.e., with leading slash).
			operation_payload := map[string]interface{}{"channel": tt.expectedChannelForSign}

			auth_object, err := c.create_signed_headers_for_operation(ctx, operation_payload)

			if err != nil {
				t.Fatalf("create_signed_headers_for_operation returned error: %v", err)
			}

			expectedKeys := map[string]bool{
				"host":                 true,
				"x-amz-date":           true,
				"X-Amz-Security-Token": true,
				"Authorization":        true,
			}
			if len(auth_object) != len(expectedKeys) {
				t.Errorf("auth_object key count mismatch: expected %d, got %d. Object: %v", len(expectedKeys), len(auth_object), auth_object)
			}
			for k := range expectedKeys {
				if _, exists := auth_object[k]; !exists {
					t.Errorf("auth_object missing expected key: %s. Object: %v", k, auth_object)
				}
			}

			if auth_object["host"] != expectedHost {
				t.Errorf("auth_object host mismatch: expected %s, got %s", expectedHost, auth_object["host"])
			}
			if auth_object["x-amz-date"] != fakeTimestamp {
				t.Errorf("auth_object x-amz-date mismatch: expected %s, got %s", fakeTimestamp, auth_object["x-amz-date"])
			}
			if auth_object["X-Amz-Security-Token"] != sessionToken {
				t.Errorf("auth_object X-Amz-Security-Token mismatch: expected %s, got %s", sessionToken, auth_object["X-Amz-Security-Token"])
			}
			if auth_object["Authorization"] != fakeSignatureString {
				t.Errorf("auth_object Authorization mismatch: expected %s, got %s", fakeSignatureString, auth_object["Authorization"])
			}
			t.Logf("Test case '%s' completed.", tt.name)
		})
	}
}

