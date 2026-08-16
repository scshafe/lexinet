package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"lexinet/internal/config"
	"lexinet/internal/server"
)

func TestServeTailnetOnWithoutBearerFailsBeforeMigrationOrServerStart(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	cfg := config.Default()
	cfg.Service.AuthToken = " \t\n "
	cfg.Service.Database.DSN = ":// invalid database DSN"
	if err := config.SaveFile(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.DefaultConfigEnv, configPath)

	migrationCalls := 0
	serverCalls := 0
	cmd := newServeCommand(&Options{}, serveCommandRuntime{
		migrate: func(context.Context, string) error {
			migrationCalls++
			return nil
		},
		serve: func(context.Context, server.Config) error {
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
	if serverCalls != 0 {
		t.Fatalf("server calls = %d, want 0", serverCalls)
	}
}

func TestTailnetBearerConfigurationErrorJSONPreservesStableSafeFields(t *testing.T) {
	credentialMaterial := "\u2003\u2003"
	err := server.ValidateConfiguration(server.Config{AuthToken: credentialMaterial, TailnetEnabled: true})
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
	if envelope.Error.Code != server.ErrorCodeTailnetAuthTokenRequired {
		t.Fatalf("error code = %q, want %q", envelope.Error.Code, server.ErrorCodeTailnetAuthTokenRequired)
	}
	if envelope.Error.Message != configurationError.Message {
		t.Fatalf("error message = %q, want %q", envelope.Error.Message, configurationError.Message)
	}
	if envelope.Error.Context["configuration_field"] != "service.auth_token" {
		t.Fatalf("configuration field context = %#v, want service.auth_token", envelope.Error.Context["configuration_field"])
	}
	if envelope.Error.Context["tailnet_enabled"] != true {
		t.Fatalf("tailnet context = %#v, want true", envelope.Error.Context["tailnet_enabled"])
	}
	if envelope.Error.ExitCode != ExitValidation {
		t.Fatalf("exit code = %d, want %d", envelope.Error.ExitCode, ExitValidation)
	}
	if bytes.Contains(output.Bytes(), []byte(credentialMaterial)) {
		t.Fatalf("configuration error JSON contains credential material: %s", output.Bytes())
	}
}
