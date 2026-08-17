package tailnet

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config contains only native Tailnet transport inputs. Enrollment secrets,
// control-server overrides, advertised tags, Funnel, and raw TCP are not
// configurable through this boundary.
type Config struct {
	Hostname  string
	StateDir  string
	UpTimeout time.Duration
}

type normalizedConfig struct {
	hostname  string
	stateDir  string
	upTimeout time.Duration
}

func normalizeConfig(config Config) (normalizedConfig, error) {
	if !validHostname(config.Hostname) {
		return normalizedConfig{}, configurationError("hostname")
	}
	if config.StateDir == "" || len(config.StateDir) > maximumStateDirPathBytes {
		return normalizedConfig{}, configurationError("state_dir")
	}

	stateDir, err := expandHome(config.StateDir)
	if err != nil {
		return normalizedConfig{}, newError(
			ErrorCodeConfigurationInvalid,
			"native Tailnet transport configuration is invalid",
			map[string]string{"configuration_field": "state_dir"},
			err,
		)
	}
	if stateDir == "" || len(stateDir) > maximumStateDirPathBytes || !filepath.IsAbs(stateDir) {
		return normalizedConfig{}, configurationError("state_dir")
	}

	upTimeout := config.UpTimeout
	if upTimeout == 0 {
		upTimeout = DefaultUpTimeout
	}
	if upTimeout < 0 || upTimeout > MaximumUpTimeout {
		return normalizedConfig{}, configurationError("up_timeout")
	}

	return normalizedConfig{
		hostname:  config.Hostname,
		stateDir:  filepath.Clean(stateDir),
		upTimeout: upTimeout,
	}, nil
}

func validHostname(hostname string) bool {
	if hostname == "" || len(hostname) > maximumHostnameBytes {
		return false
	}
	if hostname[0] == '-' || hostname[len(hostname)-1] == '-' {
		return false
	}
	for index := range len(hostname) {
		character := hostname[index]
		if (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') ||
			character == '-' {
			continue
		}
		return false
	}
	return true
}

func expandHome(path string) (string, error) {
	if path == "" || path[0] != '~' {
		return path, nil
	}
	homeDirectory, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return homeDirectory, nil
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(homeDirectory, path[2:]), nil
	}
	return "", fmt.Errorf("unsupported home-directory expression")
}

func configurationError(field string) *Error {
	return newError(
		ErrorCodeConfigurationInvalid,
		"native Tailnet transport configuration is invalid",
		map[string]string{"configuration_field": field},
		nil,
	)
}
