package cli

import (
	"bytes"
	"encoding/json"
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
