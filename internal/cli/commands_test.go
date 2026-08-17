package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"lexinet/internal/config"
	"lexinet/internal/server"
)

func TestServeTailnetOnWithoutScopedAuthenticationFailsBeforeMigrationOrServerStart(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	cfg := config.Default()
	cfg.Service.AuthToken = " \t\n "
	cfg.Service.Database.DSN = ":// invalid database DSN"
	if err := config.SaveFile(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.DefaultConfigEnv, configPath)

	migrationCalls := 0
	preparationCalls := 0
	serverCalls := 0
	cmd := newServeCommand(&Options{}, serveCommandRuntime{
		migrate: func(context.Context, string) error {
			migrationCalls++
			return nil
		},
		prepare: func(cfg server.Config) (*server.PreparedConfiguration, error) {
			preparationCalls++
			return server.PrepareConfiguration(cfg)
		},
		serve: func(context.Context, *server.PreparedConfiguration) error {
			serverCalls++
			return nil
		},
	})
	cmd.SetArgs([]string{"--tailnet", "on"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected configuration error")
	}
	var configurationError *server.ConfigurationError
	if !errors.As(err, &configurationError) {
		t.Fatalf("error = %T %v, want *server.ConfigurationError", err, err)
	}
	if got := exitCode(err); got != ExitValidation {
		t.Fatalf("exit code = %d, want %d", got, ExitValidation)
	}
	if migrationCalls != 0 {
		t.Fatalf("migration calls = %d, want 0", migrationCalls)
	}
	if preparationCalls != 1 {
		t.Fatalf("preparation calls = %d, want 1", preparationCalls)
	}
	if serverCalls != 0 {
		t.Fatalf("server calls = %d, want 0", serverCalls)
	}
}

func TestTailnetAuthenticationConfigurationErrorJSONPreservesStableSafeFields(t *testing.T) {
	credentialMaterial := "credential-material-must-not-appear"
	err := server.ValidateConfiguration(server.Config{
		TailnetEnabled:              true,
		TailnetAuthenticationMode:   "scoped_token",
		TailnetAllowedProfileRefs:   []string{"learner.primary"},
		TailnetScopedTokenSubjectID: "11111111-1111-1111-1111-111111111111",
		TailnetScopedTokenFile:      credentialMaterial,
	})
	var configurationError *server.ConfigurationError
	if !errors.As(err, &configurationError) {
		t.Fatalf("error = %T %v, want *server.ConfigurationError", err, err)
	}
	var output bytes.Buffer
	writeCLIError(&output, true, ExitValidation, &ExitError{Code: ExitValidation, Err: err})

	var envelope struct {
		Error struct {
			Code     string         `json:"code"`
			Message  string         `json:"message"`
			Context  map[string]any `json:"context"`
			ExitCode int            `json:"exit_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatalf("decode configuration error JSON: %v", err)
	}
	if envelope.Error.Code != server.ErrorCodeTailnetStudyAuthenticationInvalid {
		t.Fatalf("error code = %q, want %q", envelope.Error.Code, server.ErrorCodeTailnetStudyAuthenticationInvalid)
	}
	if envelope.Error.Message != configurationError.Message {
		t.Fatalf("error message = %q, want %q", envelope.Error.Message, configurationError.Message)
	}
	if envelope.Error.Context["configuration_field"] != "service.tailnet_study_authentication" {
		t.Fatalf("configuration field context = %#v", envelope.Error.Context["configuration_field"])
	}
	if envelope.Error.ExitCode != ExitValidation {
		t.Fatalf("exit code = %d, want %d", envelope.Error.ExitCode, ExitValidation)
	}
	if bytes.Contains(output.Bytes(), []byte(credentialMaterial)) {
		t.Fatalf("configuration error JSON contains credential material: %s", output.Bytes())
	}
}

func TestServePreparesCompleteTailnetPolicyBeforeMigrationAndReusesAuthority(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	cfg := config.Default()
	cfg.Service.TailnetEnabled = true
	cfg.Service.TailnetHostname = "lexinet-study"
	cfg.Service.TailnetStateDir = "/var/lib/lexinet/tsnet-state"
	cfg.Service.TailnetStudyAuthentication = config.TailnetStudyAuthenticationConfig{
		Mode:                  "tailnet_node",
		RequiredNodeTag:       "tag:embedded-house-panel",
		ApplicationCapability: "example.com/cap/lexinet-study-v1",
		AllowedProfileRefs:    []string{"learner.primary", "learner.secondary"},
	}
	if err := config.SaveFile(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.DefaultConfigEnv, configPath)

	prepared := &server.PreparedConfiguration{}
	var calls []string
	cmd := newServeCommand(&Options{}, serveCommandRuntime{
		prepare: func(got server.Config) (*server.PreparedConfiguration, error) {
			calls = append(calls, "prepare")
			if !got.TailnetEnabled || got.TailnetHostname != "lexinet-study" ||
				got.TailnetStateDir != "/var/lib/lexinet/tsnet-state" ||
				got.TailnetAuthenticationMode != "tailnet_node" ||
				got.TailnetRequiredNodeTag != "tag:embedded-house-panel" ||
				got.TailnetApplicationCapability != "example.com/cap/lexinet-study-v1" ||
				!reflect.DeepEqual(got.TailnetAllowedProfileRefs, []string{"learner.primary", "learner.secondary"}) ||
				got.TailnetScopedTokenSubjectID != "" || got.TailnetScopedTokenFile != "" {
				t.Fatalf("prepared server config = %#v", got)
			}
			got.TailnetAllowedProfileRefs[0] = "mutated.after.prepare"
			return prepared, nil
		},
		migrate: func(context.Context, string) error {
			calls = append(calls, "migrate")
			return nil
		},
		serve: func(_ context.Context, got *server.PreparedConfiguration) error {
			calls = append(calls, "serve")
			if got != prepared {
				t.Fatal("serve did not receive the prepared authority")
			}
			return nil
		},
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute serve: %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"prepare", "migrate", "serve"}) {
		t.Fatalf("call order = %#v", calls)
	}
	if got := cfg.Service.TailnetStudyAuthentication.AllowedProfileRefs[0]; got != "learner.primary" {
		t.Fatalf("source config profile mutated: %q", got)
	}
}
