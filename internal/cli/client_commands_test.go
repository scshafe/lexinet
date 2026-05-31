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

func writeTestFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
