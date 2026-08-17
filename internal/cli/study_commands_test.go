package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"lexinet/internal/config"
	"lexinet/internal/studysession"
)

func TestStudyCommandsFreezeGrammarAndValidateArguments(t *testing.T) {
	setupClientConfig(t)
	testCases := []struct {
		name string
		args []string
	}{
		{name: "session_requires_request", args: []string{"study", "session"}},
		{name: "session_rejects_argument", args: []string{"study", "session", "inline", "--request", "-"}},
		{name: "sync_requires_session", args: []string{"study", "sync", "--request", "-"}},
		{name: "sync_rejects_invalid_uuid", args: []string{"study", "sync", "not-a-uuid", "--request", "-"}},
		{name: "sync_rejects_zero_uuid", args: []string{"study", "sync", "00000000-0000-0000-0000-000000000000", "--request", "-"}},
		{name: "sync_requires_request", args: []string{"study", "sync", studyClientTestSessionID}},
		{name: "state_requires_session", args: []string{"study", "state"}},
		{name: "state_rejects_invalid_uuid", args: []string{"study", "state", "not-a-uuid"}},
		{name: "state_rejects_extra_argument", args: []string{"study", "state", studyClientTestSessionID, "extra"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := executeTestCommand(t, testCase.args, strings.NewReader(`{"request":true}`))
			if err == nil {
				t.Fatal("invalid grammar unexpectedly succeeded")
			}
			if exitCode(err) != ExitUsage {
				t.Fatalf("exit code = %d, want %d: %v", exitCode(err), ExitUsage, err)
			}
		})
	}
}

func TestStudySessionAndSyncCommandsPreserveRawRequestAndResponse(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	configured := config.Default()
	configured.Server.Token = "config-token"
	configured.Server.TokenFile = ""
	if err := config.SaveFile(configPath, configured); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEXINET_CONFIG", configPath)
	t.Setenv("LEXINET_TOKEN", "environment-token")

	tokenPath := filepath.Join(t.TempDir(), "study-token")
	if err := os.WriteFile(tokenPath, []byte(studyClientTestToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionRequest := []byte("  {\n  \"operation\": \"session\"\n}\n")
	sessionRequestPath := filepath.Join(t.TempDir(), "session-request.json")
	if err := os.WriteFile(sessionRequestPath, sessionRequest, 0o600); err != nil {
		t.Fatal(err)
	}
	syncRequest := []byte("{\"operation\":\"sync\"}\n")

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Header.Get("Authorization") != "Bearer "+studyClientTestToken {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		if request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Accept") != "application/json" {
			t.Fatalf("content negotiation headers = %#v", request.Header)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		switch request.URL.Path {
		case "/api/v1/study/sessions":
			if !bytes.Equal(body, sessionRequest) {
				t.Fatalf("session request changed: %q", body)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"protocol_version":"study.v1","schema_version":1,"session_id":"11111111-1111-1111-1111-111111111111","operation":"session","raw":true}`))
		case "/api/v1/study/sessions/" + studyClientTestSessionID + "/sync":
			if !bytes.Equal(body, syncRequest) {
				t.Fatalf("sync request changed: %q", body)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte("{\"protocol_version\":\"study.v1\",\"schema_version\":1,\"sync_id\":\"22222222-2222-2222-2222-222222222222\",\"session\":{\"session_id\":\"11111111-1111-1111-1111-111111111111\"},\"operation\":\"sync\",\"raw\":true}\n"))
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
	}))
	defer server.Close()

	sessionOutput, _, err := executeTestCommand(t, []string{
		"--endpoint", server.URL,
		"--token-file", tokenPath,
		"study", "session", "--request", sessionRequestPath,
	}, nil)
	if err != nil {
		t.Fatalf("study session: %v", err)
	}
	if sessionOutput != "{\"protocol_version\":\"study.v1\",\"schema_version\":1,\"session_id\":\"11111111-1111-1111-1111-111111111111\",\"operation\":\"session\",\"raw\":true}\n" {
		t.Fatalf("session output = %q", sessionOutput)
	}

	syncOutput, _, err := executeTestCommand(t, []string{
		"--endpoint", server.URL,
		"--token-file", tokenPath,
		"study", "sync", strings.ToUpper(studyClientTestSessionID), "--request", "-",
	}, strings.NewReader(string(syncRequest)))
	if err != nil {
		t.Fatalf("study sync: %v", err)
	}
	if syncOutput != "{\"protocol_version\":\"study.v1\",\"schema_version\":1,\"sync_id\":\"22222222-2222-2222-2222-222222222222\",\"session\":{\"session_id\":\"11111111-1111-1111-1111-111111111111\"},\"operation\":\"sync\",\"raw\":true}\n" {
		t.Fatalf("sync output = %q", syncOutput)
	}
	if calls.Load() != 2 {
		t.Fatalf("HTTP calls = %d, want 2", calls.Load())
	}
}

func TestStudyStateCommandBuildsCanonicalPathAndValidatedQuery(t *testing.T) {
	setupClientConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/study/sessions/"+studyClientTestSessionID {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.URL.Query().Get("page_after_position") != "11" || request.URL.Query().Get("page_limit") != "23" || len(request.URL.Query()) != 2 {
			t.Fatalf("query = %q", request.URL.RawQuery)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"protocol_version":"study.v1","schema_version":1,"session_id":"11111111-1111-1111-1111-111111111111","state":true}`))
	}))
	defer server.Close()

	output, _, err := executeTestCommand(t, []string{
		"--endpoint", server.URL,
		"study", "state", strings.ToUpper(studyClientTestSessionID),
		"--after-position", "11", "--limit", "23",
	}, nil)
	if err != nil {
		t.Fatalf("study state: %v", err)
	}
	if output != "{\"protocol_version\":\"study.v1\",\"schema_version\":1,\"session_id\":\"11111111-1111-1111-1111-111111111111\",\"state\":true}\n" {
		t.Fatalf("output = %q", output)
	}
}

func TestStudyStateCommandRejectsInvalidPageFlagsBeforeHTTP(t *testing.T) {
	setupClientConfig(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()

	testCases := [][]string{
		{"--after-position", "-2"},
		{"--after-position", "2147483648"},
		{"--limit", "0"},
		{"--limit", "65"},
	}
	for _, flags := range testCases {
		args := []string{"--endpoint", server.URL, "study", "state", studyClientTestSessionID}
		args = append(args, flags...)
		_, _, err := executeTestCommand(t, args, nil)
		if err == nil {
			t.Fatalf("flags %v unexpectedly succeeded", flags)
		}
		if exitCode(err) != ExitUsage {
			t.Fatalf("flags %v exit code = %d, want %d", flags, exitCode(err), ExitUsage)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("HTTP calls = %d, want 0", calls.Load())
	}
}

func TestStudyCommandRequestFileAndStdinMaxPlusOneBounds(t *testing.T) {
	setupClientConfig(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) != studysession.StudyMaximumRequestBodyBytes {
			t.Fatalf("request bytes = %d, want %d", len(body), studysession.StudyMaximumRequestBodyBytes)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(studyClientTestSessionResource))
	}))
	defer server.Close()

	exactRequest := strings.Repeat("x", studysession.StudyMaximumRequestBodyBytes)
	overRequest := exactRequest + "x"
	exactPath := filepath.Join(t.TempDir(), "exact-request.json")
	overPath := filepath.Join(t.TempDir(), "over-request.json")
	if err := os.WriteFile(exactPath, []byte(exactRequest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overPath, []byte(overRequest), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "study", "session", "--request", exactPath}, nil); err != nil {
		t.Fatalf("exact file: %v", err)
	}
	if _, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "study", "session", "--request", "-"}, strings.NewReader(exactRequest)); err != nil {
		t.Fatalf("exact stdin: %v", err)
	}
	if _, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "study", "session", "--request", overPath}, nil); err == nil {
		t.Fatal("over-limit file unexpectedly succeeded")
	}
	if _, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "study", "session", "--request", "-"}, strings.NewReader(overRequest)); err == nil {
		t.Fatal("over-limit stdin unexpectedly succeeded")
	}
	if calls.Load() != 2 {
		t.Fatalf("HTTP calls = %d, want 2", calls.Load())
	}
}

func TestStudySessionCommandDoesNotAcceptInlineJSON(t *testing.T) {
	setupClientConfig(t)
	inlineRequest := `{"protocol_version":"study.v1"}`
	_, _, err := executeTestCommand(t, []string{"study", "session", "--request", inlineRequest}, nil)
	if err == nil {
		t.Fatal("inline JSON unexpectedly succeeded")
	}
	if exitCode(err) != ExitValidation {
		t.Fatalf("exit code = %d, want %d", exitCode(err), ExitValidation)
	}
	if strings.Contains(err.Error(), inlineRequest) {
		t.Fatalf("request content leaked in error %q", err)
	}
}

func TestStudyCommandSanitizesConfigurationAndTokenFileFailures(t *testing.T) {
	requestPath := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(requestPath, []byte(`{"request":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("configuration", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "configuration-path-canary.toml")
		configCanary := `server = "https://user:password@secret.example" broken`
		if err := os.WriteFile(configPath, []byte(configCanary), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("LEXINET_CONFIG", configPath)
		_, _, err := executeTestCommand(t, []string{"study", "session", "--request", requestPath}, nil)
		assertSanitizedStudyConfigurationError(t, err, configPath, configCanary)
	})

	t.Run("token_file", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "config.toml")
		configured := config.Default()
		configured.Server.TokenFile = filepath.Join(t.TempDir(), "token-path-canary")
		if err := os.Mkdir(configured.Server.TokenFile, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := config.SaveFile(configPath, configured); err != nil {
			t.Fatal(err)
		}
		t.Setenv("LEXINET_CONFIG", configPath)
		_, _, err := executeTestCommand(t, []string{"study", "session", "--request", requestPath}, nil)
		assertSanitizedStudyConfigurationError(t, err, configured.Server.TokenFile, "is a directory")
	})
}

func TestStudyCommandRejectsOverLimitResponseWithoutOutput(t *testing.T) {
	setupClientConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(append(studyJSONWithExactSize(t, studysession.StudyMaximumResponseBodyBytes), ' '))
	}))
	defer server.Close()
	requestPath := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(requestPath, []byte(`{"request":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "study", "session", "--request", requestPath}, nil)
	if err == nil {
		t.Fatal("over-limit response unexpectedly succeeded")
	}
	if stdout != "" {
		t.Fatalf("partial stdout = %q", stdout)
	}
	clientErr := requireStudyClientError(t, err)
	if clientErr.failure != studyClientFailureResponseLarge {
		t.Fatalf("failure = %q, want response_too_large", clientErr.failure)
	}
}

func assertSanitizedStudyConfigurationError(t *testing.T, err error, canaries ...string) {
	t.Helper()
	clientErr := requireStudyClientError(t, err)
	if clientErr.failure != studyClientFailureConfiguration || clientErr.StatusCode != 0 || clientErr.Code != "" || clientErr.Retryable {
		t.Fatalf("configuration error = %#v", clientErr)
	}
	var jsonOutput bytes.Buffer
	writeCLIError(&jsonOutput, true, exitCode(err), err)
	var textOutput bytes.Buffer
	writeCLIError(&textOutput, false, exitCode(err), err)
	for _, canary := range canaries {
		if canary == "" {
			continue
		}
		if strings.Contains(err.Error(), canary) || strings.Contains(jsonOutput.String(), canary) || strings.Contains(textOutput.String(), canary) {
			t.Fatalf("configuration error leaked %q: err=%q json=%q text=%q", canary, err, jsonOutput.String(), textOutput.String())
		}
	}
}
