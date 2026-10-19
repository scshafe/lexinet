package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func lookupFrom(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { value, ok := values[name]; return value, ok }
}

const testDSN = "postgres://lexinet@db:5432/lexinet?sslmode=disable"

func TestDatabaseDSNAppliesEnvironmentPasswordEscaped(t *testing.T) {
	got, err := databaseDSNWithPassword(testDSN, lookupFrom(map[string]string{DatabasePasswordEnv: "p@ss/w:rd"}))
	if err != nil || got != "postgres://lexinet:p%40ss%2Fw%3Ard@db:5432/lexinet?sslmode=disable" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestDatabaseDSNUnchangedWithoutPasswordOrWithExistingPassword(t *testing.T) {
	for _, dsn := range []string{testDSN, "postgres://lexinet:own@db/lexinet", "postgres://db/lexinet"} {
		env := map[string]string{DatabasePasswordEnv: "x"}
		if dsn == testDSN {
			env = nil
		}
		got, err := databaseDSNWithPassword(dsn, lookupFrom(env))
		if err != nil || got != dsn {
			t.Fatalf("dsn %q changed to %q, %v", dsn, got, err)
		}
	}
}

func TestDatabaseDSNReadsOwnerOnlyPasswordFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pw")
	if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := databaseDSNWithPassword(testDSN, lookupFrom(map[string]string{DatabasePasswordFileEnv: path}))
	if err != nil || !strings.Contains(got, ":secret@") {
		t.Fatalf("got %q, %v", got, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := databaseDSNWithPassword(testDSN, lookupFrom(map[string]string{DatabasePasswordFileEnv: path})); err == nil {
		t.Fatal("group/world readable file accepted")
	}
}

func TestDatabaseDSNRejectsBothSourcesAndNeverLeaksPassword(t *testing.T) {
	_, err := databaseDSNWithPassword(testDSN, lookupFrom(map[string]string{DatabasePasswordEnv: "topsecret", DatabasePasswordFileEnv: "/x"}))
	if err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("err = %v", err)
	}
	_, err = databaseDSNWithPassword(testDSN, lookupFrom(map[string]string{DatabasePasswordFileEnv: "/nonexistent/topsecret"}))
	if err == nil {
		t.Fatal("missing file accepted")
	}
}
