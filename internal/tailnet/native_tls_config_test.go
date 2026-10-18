package tailnet

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeConfigAcceptsExplicitNativeTLSConfiguration(t *testing.T) {
	stateDirectory := t.TempDir()
	normalized, err := normalizeConfig(Config{
		Hostname:  testHostname,
		StateDir:  stateDirectory,
		UpTimeout: 17 * time.Second,
	})
	if err != nil {
		t.Fatalf("normalize config: %v", err)
	}
	if normalized.hostname != testHostname || normalized.stateDir != stateDirectory || normalized.upTimeout != 17*time.Second {
		t.Fatalf("normalized config = %#v", normalized)
	}
}

func TestNormalizeConfigUsesBoundedDefaultUpTimeout(t *testing.T) {
	normalized, err := normalizeConfig(Config{Hostname: testHostname, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("normalize config: %v", err)
	}
	if normalized.upTimeout != DefaultUpTimeout {
		t.Fatalf("up timeout = %s, want %s", normalized.upTimeout, DefaultUpTimeout)
	}
}

func TestNormalizeConfigExpandsCurrentUserHome(t *testing.T) {
	homeDirectory, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home: %v", err)
	}
	normalized, err := normalizeConfig(Config{
		Hostname: testHostname,
		StateDir: "~/lexinet-tailnet-state",
	})
	if err != nil {
		t.Fatalf("normalize config: %v", err)
	}
	want := filepath.Join(homeDirectory, "lexinet-tailnet-state")
	if normalized.stateDir != want {
		t.Fatalf("state dir = %q, want %q", normalized.stateDir, want)
	}
}

func TestNormalizeConfigRejectsInvalidHostnames(t *testing.T) {
	testCases := map[string]string{
		"empty":           "",
		"uppercase":       "Lexinet",
		"period":          "lexinet.example",
		"space":           "lexinet study",
		"leading hyphen":  "-lexinet",
		"trailing hyphen": "lexinet-",
		"over bound":      strings.Repeat("a", maximumHostnameBytes+1),
	}
	for name, hostname := range testCases {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeConfig(Config{Hostname: hostname, StateDir: t.TempDir()})
			assertConfigurationField(t, err, "hostname")
		})
	}
}

func TestNormalizeConfigRejectsInvalidStateDirectories(t *testing.T) {
	testCases := map[string]string{
		"empty":                   "",
		"relative":                "relative/state",
		"unsupported home syntax": "~another-user/state",
		"over bound":              "/" + strings.Repeat("a", maximumStateDirPathBytes),
	}
	for name, stateDirectory := range testCases {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeConfig(Config{Hostname: testHostname, StateDir: stateDirectory})
			assertConfigurationField(t, err, "state_dir")
		})
	}
}

func TestNormalizeConfigRejectsUnboundedUpTimeouts(t *testing.T) {
	for _, timeout := range []time.Duration{-time.Nanosecond, MaximumUpTimeout + time.Nanosecond} {
		t.Run(timeout.String(), func(t *testing.T) {
			_, err := normalizeConfig(Config{Hostname: testHostname, StateDir: t.TempDir(), UpTimeout: timeout})
			assertConfigurationField(t, err, "up_timeout")
		})
	}
}

func assertConfigurationField(t *testing.T, err error, wantField string) {
	t.Helper()
	if !errors.Is(err, &Error{Code: ErrorCodeConfigurationInvalid}) {
		t.Fatalf("error = %v, want %s", err, ErrorCodeConfigurationInvalid)
	}
	var tailnetError *Error
	if !errors.As(err, &tailnetError) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if tailnetError.Context["configuration_field"] != wantField {
		t.Fatalf("configuration field = %q, want %q", tailnetError.Context["configuration_field"], wantField)
	}
}
