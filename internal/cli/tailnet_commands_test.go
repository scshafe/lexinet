package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"lexinet/internal/tailnet"
)

func runEnroll(t *testing.T, stdin string, args []string, got *string) (string, error) {
	t.Helper()
	cmd := newTailnetEnrollCommand(&Options{}, func(_ context.Context, _ tailnet.Config, key string) error {
		*got = key
		return nil
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestEnrollReadsKeyFromStdinOnly(t *testing.T) {
	var key string
	out, err := runEnroll(t, "tskey-abc\n", []string{"--state-dir", "/s", "--hostname", "h"}, &key)
	if err != nil || key != "tskey-abc" || strings.Contains(out, "tskey") {
		t.Fatalf("key %q out %q err %v", key, out, err)
	}
	if _, err := runEnroll(t, "tskey-abc", []string{"--state-dir", "/s", "--hostname", "h", "--key", "x"}, &key); err == nil {
		t.Fatal("a key flag must not exist")
	}
	key = ""
	if _, err := runEnroll(t, "  \n", []string{"--state-dir", "/s", "--hostname", "h"}, &key); err == nil || key != "" {
		t.Fatalf("empty stdin accepted: %v", err)
	}
}
