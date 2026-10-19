package tailnet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeEnrollServer struct {
	upError error
	closed  int
}

func (server *fakeEnrollServer) Up(context.Context) error { return server.upError }
func (server *fakeEnrollServer) Close() error             { server.closed++; return nil }

func noEnvironment(string) (string, bool) { return "", false }

func enrollFixture(t *testing.T, server *fakeEnrollServer, keySeen *string) (Config, enrollServerFactory) {
	t.Helper()
	return Config{Hostname: "lexinet-study", StateDir: filepath.Join(t.TempDir(), "state")},
		func(_ normalizedConfig, key string) (enrollServer, error) {
			if keySeen != nil {
				*keySeen = key
			}
			return server, nil
		}
}

func TestEnrollCreatesOwnerOnlyDirectoryAndClosesServer(t *testing.T) {
	server := &fakeEnrollServer{}
	var seen string
	config, factory := enrollFixture(t, server, &seen)
	if err := enroll(context.Background(), config, "tskey-secret", noEnvironment, factory); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(config.StateDir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("state dir mode = %v, %v", info, err)
	}
	if seen != "tskey-secret" || server.closed != 1 {
		t.Fatalf("key seen %q, closed %d", seen, server.closed)
	}
}

func TestEnrollRefusesAmbientEnrollmentAndEmptyKey(t *testing.T) {
	config, factory := enrollFixture(t, &fakeEnrollServer{}, nil)
	ambient := func(name string) (string, bool) { return "x", name == "TS_AUTHKEY" }
	if err := enroll(context.Background(), config, "k", ambient, factory); !errors.Is(err, &Error{Code: ErrorCodeAmbientConfigurationRejected}) {
		t.Fatalf("ambient err = %v", err)
	}
	if err := enroll(context.Background(), config, "", noEnvironment, factory); !errors.Is(err, &Error{Code: ErrorCodeConfigurationInvalid}) {
		t.Fatalf("empty key err = %v", err)
	}
	if _, err := os.Stat(config.StateDir); !os.IsNotExist(err) {
		t.Fatal("state dir created despite refused input")
	}
}

func TestEnrollRefusesExistingStateAndWrongMode(t *testing.T) {
	config, factory := enrollFixture(t, &fakeEnrollServer{}, nil)
	if err := os.Mkdir(config.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.StateDir, "tailscaled.state"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := enroll(context.Background(), config, "k", noEnvironment, factory); !errors.Is(err, &Error{Code: ErrorCodeStateDirectoryInvalid}) {
		t.Fatalf("existing state err = %v", err)
	}
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	config.StateDir = loose
	if err := enroll(context.Background(), config, "k", noEnvironment, factory); !errors.Is(err, &Error{Code: ErrorCodeStateDirectoryInvalid}) {
		t.Fatalf("wrong mode err = %v", err)
	}
}

func TestEnrollUpFailureClosesServerAndDoesNotLeakKey(t *testing.T) {
	server := &fakeEnrollServer{upError: errors.New("control said tskey-secret is bad")}
	config, factory := enrollFixture(t, server, nil)
	err := enroll(context.Background(), config, "tskey-secret", noEnvironment, factory)
	if !errors.Is(err, &Error{Code: ErrorCodeUpFailed}) || server.closed != 1 {
		t.Fatalf("err = %v closed = %d", err, server.closed)
	}
	if strings.Contains(err.Error(), "tskey-secret") {
		t.Fatal("error text leaks the key")
	}
}
