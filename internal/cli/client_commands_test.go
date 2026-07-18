package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lexinet/internal/config"
	"lexinet/internal/model"
)

func TestIdeaAddUsesTokenFileOverEnv(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	cfg := config.Default()
	cfg.Server.Token = "config-token"
	if err := config.SaveFile(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEXINET_CONFIG", configPath)
	t.Setenv("LEXINET_TOKEN", "env-token")

	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := writeTestFile(tokenPath, "file-token\n"); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/ideas" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer file-token" {
			t.Fatalf("Authorization = %q, want file token", got)
		}
		var input model.CreateIdeaInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if len(input.Names) != 1 || input.Names[0].Text != "Redis" || input.Names[0].Kind != model.NameKindCanonical {
			t.Fatalf("unexpected names: %#v", input.Names)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"idea-1"}`))
	}))
	defer server.Close()

	stdout, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "--token-file", tokenPath, "--json", "idea", "add", "Redis"}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(stdout, "idea-1") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestBodyAddReadsStdinDash(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := config.SaveFile(configPath, config.Default()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEXINET_CONFIG", configPath)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/ideas/redis/definitions" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var input model.CreateDefinitionInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if input.Body != "definition from stdin" {
			t.Fatalf("body = %q", input.Body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"def-1"}`))
	}))
	defer server.Close()

	_, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "--json", "def", "add", "redis", "-"}, strings.NewReader("definition from stdin"))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
}

func TestNonInteractiveIdeaRemoveRequiresYes(t *testing.T) {
	_, _, err := executeTestCommand(t, []string{"idea", "rm", "redis"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := exitCode(err); got != ExitUsage {
		t.Fatalf("exit code = %d, want %d", got, ExitUsage)
	}
}

func TestInvalidRelationIsUsageError(t *testing.T) {
	_, _, err := executeTestCommand(t, []string{"link", "add", "redis", "not_a_relation", "kv-store"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := exitCode(err); got != ExitUsage {
		t.Fatalf("exit code = %d, want %d", got, ExitUsage)
	}
}

func TestTopicCommandsUseHTTPClientLayer(t *testing.T) {
	setupClientConfig(t)

	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case http.MethodPost + " /topics":
			seen["add"] = true
			var input model.CreateTopicInput
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input.Name != "Software" || input.Slug != "software" || input.Description == nil || *input.Description != "systems terms" {
				t.Fatalf("unexpected topic add payload: %#v", input)
			}
			_, _ = w.Write([]byte(`{"id":"topic-1","slug":"software","name":"Software"}`))
		case http.MethodGet + " /topics":
			seen["list"] = true
			_, _ = w.Write([]byte(`{"topics":[{"id":"topic-1","slug":"software","name":"Software"}]}`))
		case http.MethodPatch + " /topics/software":
			seen["edit"] = true
			var patch map[string]string
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Fatal(err)
			}
			if patch["name"] != "Systems" || patch["description"] != "updated" {
				t.Fatalf("unexpected topic edit payload: %#v", patch)
			}
			_, _ = w.Write([]byte(`{"id":"topic-1","slug":"software","name":"Systems"}`))
		case http.MethodPost + " /ideas/redis/topics":
			seen["assign"] = true
			var input map[string]string
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input["topic"] != "software" {
				t.Fatalf("unexpected topic assign payload: %#v", input)
			}
			_, _ = w.Write([]byte(`{"id":"topic-1","slug":"software","name":"Software"}`))
		case http.MethodDelete + " /ideas/redis/topics/software":
			seen["unassign"] = true
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet + " /topics/software/ideas":
			seen["ideas"] = true
			query := r.URL.Query()
			if query.Get("q") != "red" || query.Get("limit") != "7" || query.Get("offset") != "2" {
				t.Fatalf("unexpected topic ideas query: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"ideas":[{"id":"idea-1","canonical_name":"Redis"}]}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	commands := [][]string{
		{"--endpoint", server.URL, "--json", "topic", "add", "Software", "--slug", "software", "--description", "systems terms"},
		{"--endpoint", server.URL, "topic", "list"},
		{"--endpoint", server.URL, "topic", "edit", "software", "--name", "Systems", "--description", "updated"},
		{"--endpoint", server.URL, "topic", "assign", "redis", "software"},
		{"--endpoint", server.URL, "topic", "unassign", "redis", "software"},
		{"--endpoint", server.URL, "topic", "ideas", "software", "--q", "red", "--limit", "7", "--offset", "2"},
	}
	for _, args := range commands {
		stdout, _, err := executeTestCommand(t, args, nil)
		if err != nil {
			t.Fatalf("execute %v: %v", args, err)
		}
		if stdout == "" {
			t.Fatalf("stdout empty for %v", args)
		}
	}
	for _, key := range []string{"add", "list", "edit", "assign", "unassign", "ideas"} {
		if !seen[key] {
			t.Fatalf("did not see %s request", key)
		}
	}
}

func TestIdeaListTopicFilterAndJSONL(t *testing.T) {
	setupClientConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/ideas" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("topic") != "software" || query.Get("limit") != "2" {
			t.Fatalf("unexpected query: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ideas":[{"id":"idea-1","canonical_name":"Redis"},{"id":"idea-2","canonical_name":"Postgres"}]}`))
	}))
	defer server.Close()

	stdout, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "idea", "list", "--topic", "software", "--limit", "2", "--jsonl"}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "Redis") || !strings.Contains(lines[1], "Postgres") {
		t.Fatalf("unexpected jsonl stdout: %q", stdout)
	}
}

func TestAcronymCommandsUseHTTPClientLayer(t *testing.T) {
	setupClientConfig(t)

	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case http.MethodPost + " /acronym-links":
			seen["link"] = true
			var input model.UpsertAcronymLinkInput
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input.Acronym != "TUI" || input.Expansion != "Terminal User Interface" || input.Note == nil || *input.Note != "common abbreviation" {
				t.Fatalf("unexpected acronym link payload: %#v", input)
			}
			_, _ = w.Write([]byte(`{"id":"link-1","acronym_name":"TUI","expansion_name":"Terminal User Interface"}`))
		case http.MethodGet + " /ideas/TUI/acronym-links":
			seen["links"] = true
			_, _ = w.Write([]byte(`{"acronym_links":[{"id":"link-1","acronym_name":"TUI","expansion_name":"Terminal User Interface"}]}`))
		case http.MethodDelete + " /acronym-links/11111111-1111-1111-1111-111111111111":
			seen["unlink"] = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	_, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "acronym", "link", "TUI", "Terminal User Interface", "--note", "common abbreviation"}, nil)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	stdout, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "acronym", "list", "TUI", "--jsonl"}, nil)
	if err != nil {
		t.Fatalf("links: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(stdout), "\n"); len(lines) != 1 || !strings.Contains(lines[0], "Terminal User Interface") {
		t.Fatalf("unexpected acronym jsonl stdout: %q", stdout)
	}
	_, _, err = executeTestCommand(t, []string{"--endpoint", server.URL, "acronym", "unlink", "11111111-1111-1111-1111-111111111111"}, nil)
	if err == nil || exitCode(err) != ExitUsage {
		t.Fatalf("unlink without --yes exit = %v, want usage", err)
	}
	_, _, err = executeTestCommand(t, []string{"--endpoint", server.URL, "acronym", "unlink", "11111111-1111-1111-1111-111111111111", "--yes"}, nil)
	if err != nil {
		t.Fatalf("unlink: %v", err)
	}
	for _, key := range []string{"link", "links", "unlink"} {
		if !seen[key] {
			t.Fatalf("did not see %s request", key)
		}
	}
}

func TestSubmissionCommandsUseHTTPClientLayer(t *testing.T) {
	setupClientConfig(t)

	submissionID := "77777777-7777-7777-7777-777777777777"
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case http.MethodPost + " /submissions":
			seen["submit"] = true
			if r.URL.RawQuery != "" {
				t.Fatalf("unexpected submit query: %s", r.URL.RawQuery)
			}
			var input model.CreateSubmissionInput
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input.Term != "TUI" || input.Context == nil || *input.Context != "menu bar capture" {
				t.Fatalf("unexpected submit payload: %#v", input)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"` + submissionID + `","term":"TUI","context":"menu bar capture","status":"pending","created_at":"2026-07-18T00:00:00Z","updated_at":"2026-07-18T00:00:00Z"}`))
		case http.MethodGet + " /submissions":
			seen["list"] = true
			if r.URL.RawQuery != "limit=5&offset=2&status=pending" {
				t.Fatalf("unexpected list query: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"total":2,"limit":5,"offset":2,"submissions":[{"id":"` + submissionID + `","term":"TUI","status":"pending","created_at":"2026-07-18T00:00:00Z","updated_at":"2026-07-18T00:00:00Z"},{"id":"88888888-8888-8888-8888-888888888888","term":"Redis","status":"pending","created_at":"2026-07-18T00:00:00Z","updated_at":"2026-07-18T00:00:00Z"}]}`))
		case http.MethodPost + " /submissions/" + submissionID + "/complete":
			seen["complete"] = true
			var input model.CompleteSubmissionInput
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input.Idea != "Terminal User Interface" || input.Note == nil || *input.Note != "created idea" {
				t.Fatalf("unexpected complete payload: %#v", input)
			}
			_, _ = w.Write([]byte(`{"id":"` + submissionID + `","term":"TUI","status":"enriched","note":"created idea","created_at":"2026-07-18T00:00:00Z","updated_at":"2026-07-18T00:00:00Z"}`))
		case http.MethodPost + " /submissions/" + submissionID + "/fail":
			seen["fail"] = true
			var input model.FailSubmissionInput
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input.Note == nil || *input.Note != "duplicate" {
				t.Fatalf("unexpected fail payload: %#v", input)
			}
			_, _ = w.Write([]byte(`{"id":"` + submissionID + `","term":"TUI","status":"failed","note":"duplicate","created_at":"2026-07-18T00:00:00Z","updated_at":"2026-07-18T00:00:00Z"}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	commands := [][]string{
		{"--endpoint", server.URL, "submit", " TUI ", "--context", " menu bar capture "},
		{"--endpoint", server.URL, "submission", "complete", submissionID, " Terminal User Interface ", "--note", " created idea "},
		{"--endpoint", server.URL, "submission", "fail", submissionID, "--note", " duplicate "},
	}
	for _, args := range commands {
		stdout, _, err := executeTestCommand(t, args, nil)
		if err != nil {
			t.Fatalf("execute %v: %v", args, err)
		}
		if stdout == "" {
			t.Fatalf("stdout empty for %v", args)
		}
	}

	stdout, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "submission", "list", "--status", "pending", "--limit", "5", "--offset", "2", "--jsonl"}, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "TUI") || !strings.Contains(lines[1], "Redis") {
		t.Fatalf("unexpected submission jsonl stdout: %q", stdout)
	}
	for _, key := range []string{"submit", "list", "complete", "fail"} {
		if !seen[key] {
			t.Fatalf("did not see %s request", key)
		}
	}
}

func TestSubmissionServiceErrorUsesTypedExit(t *testing.T) {
	setupClientConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/submissions/77777777-7777-7777-7777-777777777777/complete" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"SUBMISSION_INVALID_TRANSITION","message":"submission is not pending"}}`))
	}))
	defer server.Close()

	_, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "submission", "complete", "77777777-7777-7777-7777-777777777777", "redis"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := exitCode(err); got != ExitValidation {
		t.Fatalf("exit code = %d, want %d", got, ExitValidation)
	}
	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) || serviceErr.Code != "SUBMISSION_INVALID_TRANSITION" {
		t.Fatalf("service error = %#v, want submission transition error", serviceErr)
	}
}

func TestSubmissionLocalValidationFailures(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	for _, args := range [][]string{
		{"--endpoint", server.URL, "submit", "   "},
		{"--endpoint", server.URL, "submission", "list", "--status", "done"},
		{"--endpoint", server.URL, "submission", "complete", "77777777-7777-7777-7777-777777777777", "   "},
	} {
		_, _, err := executeTestCommand(t, args, nil)
		if err == nil {
			t.Fatalf("expected error for %v", args)
		}
		if got := exitCode(err); got != ExitUsage {
			t.Fatalf("exit code for %v = %d, want %d", args, got, ExitUsage)
		}
	}
	if calls != 0 {
		t.Fatalf("expected no HTTP calls for local validation failures, got %d", calls)
	}
}

func TestClientHTTPErrorExitCodeMapping(t *testing.T) {
	setupClientConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"TOPIC_NOT_FOUND","message":"topic not found"}}`))
	}))
	defer server.Close()

	_, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "topic", "list"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := exitCode(err); got != ExitNotFound {
		t.Fatalf("exit code = %d, want %d", got, ExitNotFound)
	}
}

func TestSearchCommandSendsStrategyAndLegacyFlags(t *testing.T) {
	setupClientConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/search" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("q") != "redos" || query.Get("strategy") != "legacy" || query.Get("fuzzy") != "false" || query.Get("threshold") != "0.45" || query.Get("scope") != "names" || query.Get("kind") != "alias" || query.Get("limit") != "7" || query.Get("offset") != "3" {
			t.Fatalf("unexpected query: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"redos","results":[]}`))
	}))
	defer server.Close()

	_, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "search", "redos", "--strategy", "legacy", "--no-fuzzy", "--threshold", "0.45", "--scope", "names", "--name-kind", "alias", "--limit", "7", "--offset", "3"}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
}

func TestSearchCommandFuzzyAndMinScoreAlias(t *testing.T) {
	setupClientConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/search" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("q") != "redos" || query.Get("fuzzy") != "true" || query.Get("threshold") != "0.62" {
			t.Fatalf("unexpected query: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"redos","results":[]}`))
	}))
	defer server.Close()

	_, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "search", "redos", "--fuzzy", "--min-score", "0.62"}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
}

func TestSearchCommandJSONLEmitsOnlyResults(t *testing.T) {
	setupClientConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/search" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"redos","strategy":"daily-driver","total":2,"results":[{"id":"idea-1","canonical_name":"Redis"},{"id":"idea-2","canonical_name":"Postgres"}]}`))
	}))
	defer server.Close()

	stdout, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "search", "redos", "--jsonl"}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "Redis") || !strings.Contains(lines[1], "Postgres") || strings.Contains(stdout, "daily-driver") {
		t.Fatalf("unexpected search jsonl stdout: %q", stdout)
	}
}

func TestSearchCommandInvalidNameKindIsUsageError(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	_, _, err := executeTestCommand(t, []string{"--endpoint", server.URL, "search", "redos", "--name-kind", "nickname"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := exitCode(err); got != ExitUsage {
		t.Fatalf("exit code = %d, want %d", got, ExitUsage)
	}
	if calls != 0 {
		t.Fatalf("expected no HTTP calls for invalid name kind, got %d", calls)
	}
}

func executeTestCommand(t *testing.T, args []string, stdin *strings.Reader) (string, string, error) {
	t.Helper()
	opts := &Options{}
	cmd := NewRoot(opts)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if stdin != nil {
		cmd.SetIn(stdin)
	}
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func setupClientConfig(t *testing.T) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := config.SaveFile(configPath, config.Default()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEXINET_CONFIG", configPath)
}

func writeTestFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
