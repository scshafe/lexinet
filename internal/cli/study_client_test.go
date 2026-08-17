package cli

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lexinet/internal/model"
	"lexinet/internal/studysession"
)

const (
	studyClientTestSessionID       = "11111111-1111-1111-1111-111111111111"
	studyClientTestToken           = "study-test-bearer"
	studyClientTestSessionResource = `{"protocol_version":"study.v1","schema_version":1,"session_id":"11111111-1111-1111-1111-111111111111"}`
	studyClientTestSyncResource    = `{"protocol_version":"study.v1","schema_version":1,"sync_id":"22222222-2222-2222-2222-222222222222","session":{"session_id":"11111111-1111-1111-1111-111111111111"}}`
)

type studyClientRoundTripFunc func(*http.Request) (*http.Response, error)

func (function studyClientRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestStudyClientEndpointPolicy(t *testing.T) {
	accepted := []string{
		"https://lexinet.example",
		"https://lexinet.example/proxy-prefix",
		"http://localhost:17892",
		"http://127.0.0.1:17892",
		"http://[::1]:17892",
	}
	for _, endpoint := range accepted {
		t.Run("accept_"+strings.NewReplacer(":", "_", "/", "_").Replace(endpoint), func(t *testing.T) {
			if _, err := newStudyHTTPClient(endpoint, ""); err != nil {
				t.Fatalf("newStudyHTTPClient(%q): %v", endpoint, err)
			}
		})
	}

	rejected := []string{
		"",
		"lexinet.example",
		"ftp://lexinet.example",
		"http://lexinet.example",
		"http://127.0.0.2.example",
		"http://127.1:17892",
		"http://user:password@localhost:17892",
		"http://localhost:17892?token=secret",
		"http://localhost:17892#fragment",
		"http://localhost:17892#",
		"http://localhost:17892?",
		" http://localhost:17892",
	}
	for _, endpoint := range rejected {
		t.Run("reject_"+strings.NewReplacer(":", "_", "/", "_").Replace(endpoint), func(t *testing.T) {
			_, err := newStudyHTTPClient(endpoint, studyClientTestToken)
			if err == nil {
				t.Fatalf("newStudyHTTPClient(%q) unexpectedly succeeded", endpoint)
			}
			if exitCode(err) != ExitUsage {
				t.Fatalf("exit code = %d, want %d", exitCode(err), ExitUsage)
			}
			if strings.Contains(err.Error(), studyClientTestToken) || (endpoint != "" && strings.Contains(err.Error(), endpoint)) {
				t.Fatalf("unsafe endpoint error %q", err)
			}
		})
	}
}

func TestStudyClientBuildsPrefixedPathQueryAndBearerHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", request.Method)
		}
		if request.URL.Path != "/proxy-prefix/api/v1/study/sessions/"+studyClientTestSessionID {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if request.URL.Query().Get("page_after_position") != "7" || request.URL.Query().Get("page_limit") != "9" {
			t.Fatalf("query = %q", request.URL.RawQuery)
		}
		if request.URL.Query().Get("token") != "" {
			t.Fatal("bearer appeared in query")
		}
		if request.Header.Get("Authorization") != "Bearer "+studyClientTestToken {
			t.Fatalf("authorization header = %q", request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(studyClientTestSessionResource))
	}))
	defer server.Close()

	client := mustStudyHTTPClient(t, server.URL+"/proxy-prefix", studyClientTestToken)
	responseJSON, err := client.get(context.Background(), "/api/v1/study/sessions/"+studyClientTestSessionID, url.Values{
		"page_after_position": []string{"7"},
		"page_limit":          []string{"9"},
	}, studyClientSuccessSessionState)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(responseJSON) != studyClientTestSessionResource {
		t.Fatalf("response = %q", responseJSON)
	}
}

func TestStudyClientUsesCertificateValidatedHTTPS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(studyClientTestSessionResource))
	}))
	defer server.Close()

	untrustedClient := mustStudyHTTPClient(t, server.URL, "")
	if _, err := untrustedClient.get(context.Background(), "/api/v1/study/sessions/"+studyClientTestSessionID, nil, studyClientSuccessSessionState); err == nil {
		t.Fatal("untrusted TLS certificate unexpectedly succeeded")
	}

	trustedClient := mustStudyHTTPClient(t, server.URL, "")
	transport := trustedClient.httpClient.Transport.(*http.Transport).Clone()
	certificatePool := x509.NewCertPool()
	certificatePool.AddCert(server.Certificate())
	transport.TLSClientConfig.RootCAs = certificatePool
	trustedClient.httpClient.Transport = transport
	if _, err := trustedClient.get(context.Background(), "/api/v1/study/sessions/"+studyClientTestSessionID, nil, studyClientSuccessSessionState); err != nil {
		t.Fatalf("certificate-validated HTTPS get: %v", err)
	}
}

func TestStudyClientRefusesRedirectsWithoutForwardingBearer(t *testing.T) {
	var redirectedCalls atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		redirectedCalls.Add(1)
		if request.Header.Get("Authorization") != "" {
			t.Fatal("redirect target received bearer")
		}
	}))
	defer redirectTarget.Close()

	redirectSource := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", redirectTarget.URL+"/capture")
		writer.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = writer.Write([]byte(`{"redirect":true}`))
	}))
	defer redirectSource.Close()

	client := mustStudyHTTPClient(t, redirectSource.URL, studyClientTestToken)
	_, err := client.post(context.Background(), "/api/v1/study/sessions", []byte(`{"request":true}`), studyClientSuccessCreateSession)
	if err == nil {
		t.Fatal("redirect unexpectedly succeeded")
	}
	if redirectedCalls.Load() != 0 {
		t.Fatalf("redirect target calls = %d, want 0", redirectedCalls.Load())
	}
	clientErr := requireStudyClientError(t, err)
	if clientErr.StatusCode != http.StatusTemporaryRedirect || clientErr.Retryable {
		t.Fatalf("redirect error = %#v", clientErr)
	}
}

func TestStudyClientNeverOffersAutomaticPOSTReplay(t *testing.T) {
	client := mustStudyHTTPClient(t, "http://127.0.0.1:17892", studyClientTestToken)
	var calls atomic.Int32
	client.httpClient.Transport = studyClientRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.GetBody != nil {
			t.Fatal("POST request exposed a replay body")
		}
		return nil, errors.New("transport secret: https://credential@example.invalid")
	})

	_, err := client.post(context.Background(), "/api/v1/study/sessions", []byte(`{"request":true}`), studyClientSuccessCreateSession)
	if err == nil {
		t.Fatal("transport failure unexpectedly succeeded")
	}
	if calls.Load() != 1 {
		t.Fatalf("round trips = %d, want 1", calls.Load())
	}
	if strings.Contains(err.Error(), "transport secret") || strings.Contains(err.Error(), "credential") || strings.Contains(err.Error(), studyClientTestToken) {
		t.Fatalf("unsafe transport error %q", err)
	}
	clientErr := requireStudyClientError(t, err)
	if !clientErr.Retryable || clientErr.StatusCode != 0 || clientErr.Code != "" {
		t.Fatalf("transport metadata = %#v", clientErr)
	}
}

func TestStudyClientResponseMaxPlusOneBounds(t *testing.T) {
	exactResponse := studyJSONWithExactSize(t, studysession.StudyMaximumResponseBodyBytes)
	overResponse := append(bytes.Clone(exactResponse), ' ')

	for _, testCase := range []struct {
		name       string
		statusCode int
		body       []byte
		wantError  bool
	}{
		{name: "exact_success", statusCode: http.StatusOK, body: exactResponse},
		{name: "over_success", statusCode: http.StatusOK, body: overResponse, wantError: true},
		{name: "over_error", statusCode: http.StatusServiceUnavailable, body: overResponse, wantError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(testCase.statusCode)
				_, _ = writer.Write(testCase.body)
			}))
			defer server.Close()

			client := mustStudyHTTPClient(t, server.URL, "")
			responseJSON, err := client.get(context.Background(), "/api/v1/study/sessions/"+studyClientTestSessionID, nil, studyClientSuccessSessionState)
			if testCase.wantError {
				if err == nil {
					t.Fatal("over-limit response unexpectedly succeeded")
				}
				clientErr := requireStudyClientError(t, err)
				if clientErr.failure != studyClientFailureResponseLarge {
					t.Fatalf("failure = %q, want %q", clientErr.failure, studyClientFailureResponseLarge)
				}
				return
			}
			if err != nil {
				t.Fatalf("exact-limit response: %v", err)
			}
			if !bytes.Equal(responseJSON, exactResponse) {
				t.Fatal("exact-limit response bytes changed")
			}
		})
	}
}

func TestStudyClientErrorRetainsOnlyAllowlistedBoundedMetadata(t *testing.T) {
	const responseSecret = "body-secret https://user:password@example.invalid/token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+studyClientTestToken {
			t.Fatal("missing bearer header")
		}
		writer.Header().Set("X-Request-ID", "request-123")
		writer.Header().Set("Retry-After", "17")
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte(`{"error":{"protocol_version":"study.v1","schema_version":1,"code":"STUDY_SERVICE_UNAVAILABLE","message":"` + responseSecret + `","retryable":false}}`))
	}))
	defer server.Close()

	client := mustStudyHTTPClient(t, server.URL, studyClientTestToken)
	_, err := client.post(context.Background(), "/api/v1/study/sessions", []byte(`{"request":true}`), studyClientSuccessCreateSession)
	clientErr := requireStudyClientError(t, err)
	if clientErr.StatusCode != http.StatusServiceUnavailable || clientErr.Code != model.StudyErrorCodeServiceUnavailable ||
		clientErr.RequestID != "request-123" || clientErr.RetryAfter != "17" || !clientErr.Retryable {
		t.Fatalf("metadata = %#v", clientErr)
	}
	encoded, marshalErr := json.Marshal(clientErr)
	if marshalErr != nil {
		t.Fatalf("marshal safe error: %v", marshalErr)
	}
	for _, forbidden := range []string{responseSecret, "password", studyClientTestToken, server.URL, "503 Service Unavailable"} {
		if strings.Contains(err.Error(), forbidden) || bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("safe error leaked %q: text=%q json=%s", forbidden, err, encoded)
		}
	}

	var jsonCLIError bytes.Buffer
	writeCLIError(&jsonCLIError, true, exitCode(err), err)
	var rendered struct {
		Error struct {
			Status     int                  `json:"status"`
			Code       model.StudyErrorCode `json:"code"`
			RequestID  string               `json:"request_id"`
			RetryAfter string               `json:"retry_after"`
			Retryable  bool                 `json:"retryable"`
			ExitCode   int                  `json:"exit_code"`
		} `json:"error"`
	}
	if unmarshalErr := json.Unmarshal(jsonCLIError.Bytes(), &rendered); unmarshalErr != nil {
		t.Fatalf("decode CLI JSON error: %v", unmarshalErr)
	}
	if rendered.Error.Status != http.StatusServiceUnavailable || rendered.Error.Code != model.StudyErrorCodeServiceUnavailable ||
		rendered.Error.RequestID != "request-123" || rendered.Error.RetryAfter != "17" || !rendered.Error.Retryable ||
		rendered.Error.ExitCode != ExitUnreachable {
		t.Fatalf("CLI JSON metadata = %#v", rendered.Error)
	}
	var textCLIError bytes.Buffer
	writeCLIError(&textCLIError, false, exitCode(err), err)
	for _, output := range []string{jsonCLIError.String(), textCLIError.String()} {
		for _, forbidden := range []string{responseSecret, "password", studyClientTestToken, server.URL, "503 Service Unavailable"} {
			if strings.Contains(output, forbidden) {
				t.Fatalf("CLI rendering leaked %q: %q", forbidden, output)
			}
		}
	}
}

func TestStudyClientDropsUntrustedErrorMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Request-ID", strings.Repeat("x", studyClientMaximumRequestIDBytes+1))
		writer.Header().Set("Retry-After", "unbounded-later")
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"error":{"protocol_version":"study.v1","schema_version":1,"code":"SERVER_PRIVATE_CODE","message":"private"}}`))
	}))
	defer server.Close()

	client := mustStudyHTTPClient(t, server.URL, "")
	_, err := client.get(context.Background(), "/api/v1/study/sessions/"+studyClientTestSessionID, nil, studyClientSuccessSessionState)
	clientErr := requireStudyClientError(t, err)
	if clientErr.Code != "" || clientErr.RequestID != "" || clientErr.RetryAfter != "" || clientErr.Retryable {
		t.Fatalf("untrusted metadata retained: %#v", clientErr)
	}
}

func TestStudyClientResponseHeaderTimeoutIsFiniteAndSafe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := mustStudyHTTPClient(t, server.URL, "")
	transport := client.httpClient.Transport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 20 * time.Millisecond
	client.httpClient.Transport = transport
	client.httpClient.Timeout = 100 * time.Millisecond
	startedAt := time.Now()
	_, err := client.get(context.Background(), "/api/v1/study/sessions/"+studyClientTestSessionID, nil, studyClientSuccessSessionState)
	if err == nil {
		t.Fatal("header timeout unexpectedly succeeded")
	}
	if time.Since(startedAt) > time.Second {
		t.Fatal("header timeout was not finite")
	}
	clientErr := requireStudyClientError(t, err)
	if !clientErr.Retryable || clientErr.StatusCode != 0 {
		t.Fatalf("timeout metadata = %#v", clientErr)
	}
}

func TestStudyClientRequestMaxPlusOneBounds(t *testing.T) {
	client := mustStudyHTTPClient(t, "http://127.0.0.1:17892", "")
	client.httpClient.Transport = studyClientRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		if len(body) != studysession.StudyMaximumRequestBodyBytes {
			t.Fatalf("request bytes = %d", len(body))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(studyClientTestSessionResource)),
		}, nil
	})

	exactRequest := bytes.Repeat([]byte{'x'}, studysession.StudyMaximumRequestBodyBytes)
	if _, err := client.post(context.Background(), "/api/v1/study/sessions", exactRequest, studyClientSuccessCreateSession); err != nil {
		t.Fatalf("exact-limit request: %v", err)
	}
	if _, err := client.post(context.Background(), "/api/v1/study/sessions", append(exactRequest, 'x'), studyClientSuccessCreateSession); err == nil {
		t.Fatal("over-limit request unexpectedly succeeded")
	}
}

func TestStudyClientRejectsUnexpectedSuccessStatusMediaAndResourceShape(t *testing.T) {
	testCases := []struct {
		name            string
		statusCode      int
		contentType     string
		contentEncoding string
		body            []byte
		successKind     studyClientSuccessKind
	}{
		{name: "create_202", statusCode: http.StatusAccepted, contentType: "application/json", body: []byte(studyClientTestSessionResource), successKind: studyClientSuccessCreateSession},
		{name: "state_201", statusCode: http.StatusCreated, contentType: "application/json", body: []byte(studyClientTestSessionResource), successKind: studyClientSuccessSessionState},
		{name: "partial_206", statusCode: http.StatusPartialContent, contentType: "application/json", body: []byte(studyClientTestSessionResource), successKind: studyClientSuccessSessionState},
		{name: "scalar", statusCode: http.StatusOK, contentType: "application/json", body: []byte(`"study.v1"`), successKind: studyClientSuccessSessionState},
		{name: "null", statusCode: http.StatusOK, contentType: "application/json", body: []byte(`null`), successKind: studyClientSuccessSessionState},
		{name: "array", statusCode: http.StatusOK, contentType: "application/json", body: []byte(`[]`), successKind: studyClientSuccessSessionState},
		{name: "wrong_protocol", statusCode: http.StatusOK, contentType: "application/json", body: []byte(`{"protocol_version":"study.v2","schema_version":1,"session_id":"11111111-1111-1111-1111-111111111111"}`), successKind: studyClientSuccessSessionState},
		{name: "wrong_schema", statusCode: http.StatusOK, contentType: "application/json", body: []byte(`{"protocol_version":"study.v1","schema_version":2,"session_id":"11111111-1111-1111-1111-111111111111"}`), successKind: studyClientSuccessSessionState},
		{name: "duplicate_protocol", statusCode: http.StatusOK, contentType: "application/json", body: []byte(`{"protocol_version":"study.v1","protocol_version":"study.v1","schema_version":1,"session_id":"11111111-1111-1111-1111-111111111111"}`), successKind: studyClientSuccessSessionState},
		{name: "wrong_session_kind", statusCode: http.StatusOK, contentType: "application/json", body: []byte(studyClientTestSyncResource), successKind: studyClientSuccessSessionState},
		{name: "wrong_sync_kind", statusCode: http.StatusOK, contentType: "application/json", body: []byte(studyClientTestSessionResource), successKind: studyClientSuccessSync},
		{name: "missing_content_type", statusCode: http.StatusOK, body: []byte(studyClientTestSessionResource), successKind: studyClientSuccessSessionState},
		{name: "non_json_content_type", statusCode: http.StatusOK, contentType: "text/plain", body: []byte(studyClientTestSessionResource), successKind: studyClientSuccessSessionState},
		{name: "unsupported_content_encoding", statusCode: http.StatusOK, contentType: "application/json", contentEncoding: "gzip", body: []byte(studyClientTestSessionResource), successKind: studyClientSuccessSessionState},
		{name: "invalid_utf8", statusCode: http.StatusOK, contentType: "application/json", body: append([]byte(`{"protocol_version":"study.v1","schema_version":1,"session_id":"11111111-1111-1111-1111-111111111111","value":"`), 0xff, '"', '}'), successKind: studyClientSuccessSessionState},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if testCase.contentType != "" {
					writer.Header().Set("Content-Type", testCase.contentType)
				}
				if testCase.contentEncoding != "" {
					writer.Header().Set("Content-Encoding", testCase.contentEncoding)
				}
				writer.WriteHeader(testCase.statusCode)
				_, _ = writer.Write(testCase.body)
			}))
			defer server.Close()

			client := mustStudyHTTPClient(t, server.URL, "")
			var err error
			if testCase.successKind == studyClientSuccessCreateSession || testCase.successKind == studyClientSuccessSync {
				_, err = client.post(context.Background(), "/api/v1/study/sessions", []byte(`{"request":true}`), testCase.successKind)
			} else {
				_, err = client.get(context.Background(), "/api/v1/study/sessions/"+studyClientTestSessionID, nil, testCase.successKind)
			}
			clientErr := requireStudyClientError(t, err)
			if clientErr.failure != studyClientFailureResponseInvalid {
				t.Fatalf("failure = %q, want response_invalid", clientErr.failure)
			}
		})
	}
}

func TestStudyClientAcceptsBothCreateStatusesAndExactSyncKind(t *testing.T) {
	for _, statusCode := range []int{http.StatusOK, http.StatusCreated} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json; charset=utf-8")
				writer.WriteHeader(statusCode)
				_, _ = writer.Write([]byte(studyClientTestSessionResource))
			}))
			defer server.Close()
			client := mustStudyHTTPClient(t, server.URL, "")
			if _, err := client.post(context.Background(), "/api/v1/study/sessions", []byte(`{"request":true}`), studyClientSuccessCreateSession); err != nil {
				t.Fatalf("create status %d: %v", statusCode, err)
			}
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Content-Encoding", "identity")
		_, _ = writer.Write([]byte(studyClientTestSyncResource))
	}))
	defer server.Close()
	client := mustStudyHTTPClient(t, server.URL, "")
	if _, err := client.post(context.Background(), "/api/v1/study/sessions/"+studyClientTestSessionID+"/sync", []byte(`{"request":true}`), studyClientSuccessSync); err != nil {
		t.Fatalf("sync success: %v", err)
	}
}

func TestStudyClientRefusesHTTPDowngradeRedirect(t *testing.T) {
	var downgradeCalls atomic.Int32
	downgradeTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		downgradeCalls.Add(1)
	}))
	defer downgradeTarget.Close()
	secureSource := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", downgradeTarget.URL+"/downgrade")
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer secureSource.Close()

	client := mustStudyHTTPClient(t, secureSource.URL, studyClientTestToken)
	transport := client.httpClient.Transport.(*http.Transport).Clone()
	certificatePool := x509.NewCertPool()
	certificatePool.AddCert(secureSource.Certificate())
	transport.TLSClientConfig.RootCAs = certificatePool
	client.httpClient.Transport = transport
	if _, err := client.post(context.Background(), "/api/v1/study/sessions", []byte(`{"request":true}`), studyClientSuccessCreateSession); err == nil {
		t.Fatal("HTTPS downgrade redirect unexpectedly succeeded")
	}
	if downgradeCalls.Load() != 0 {
		t.Fatalf("downgrade target calls = %d, want 0", downgradeCalls.Load())
	}
}

func TestStudyRawResponseWriterErrorIsSanitized(t *testing.T) {
	writer := studyClientErrorWriter{err: errors.New("writer-secret /private/output/path")}
	err := writeStudyRawResponse(writer, []byte(studyClientTestSessionResource))
	clientErr := requireStudyClientError(t, err)
	if clientErr.failure != studyClientFailureOutput {
		t.Fatalf("failure = %q, want output_failed", clientErr.failure)
	}
	if strings.Contains(err.Error(), "writer-secret") || strings.Contains(err.Error(), "/private/output/path") {
		t.Fatalf("writer failure leaked: %q", err)
	}
}

type studyClientErrorWriter struct {
	err error
}

func (writer studyClientErrorWriter) Write([]byte) (int, error) {
	return 0, writer.err
}

func mustStudyHTTPClient(t *testing.T, endpoint, token string) *studyHTTPClient {
	t.Helper()
	client, err := newStudyHTTPClient(endpoint, token)
	if err != nil {
		t.Fatalf("newStudyHTTPClient: %v", err)
	}
	return client
}

func requireStudyClientError(t *testing.T, err error) *StudyClientError {
	t.Helper()
	if err == nil {
		t.Fatal("expected study client error")
	}
	var clientErr *StudyClientError
	if !errors.As(err, &clientErr) {
		t.Fatalf("error type = %T, want *StudyClientError", err)
	}
	return clientErr
}

func studyJSONWithExactSize(t *testing.T, size int) []byte {
	t.Helper()
	const prefix = `{"protocol_version":"study.v1","schema_version":1,"session_id":"11111111-1111-1111-1111-111111111111","value":"`
	const suffix = `"}`
	if size < len(prefix)+len(suffix) {
		t.Fatalf("JSON size %d is too small", size)
	}
	result := []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
	if len(result) != size || !json.Valid(result) {
		t.Fatalf("failed to build valid JSON with size %d", size)
	}
	return result
}
