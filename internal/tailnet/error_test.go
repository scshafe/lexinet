package tailnet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestErrorSerializationAndRenderingRedactExternalCause(t *testing.T) {
	err := newError(
		ErrorCodeUpFailed,
		"native Tailnet server did not become ready",
		map[string]string{"operation": "up"},
		errTestRawTailscaleFailure,
	)
	encoded, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatalf("marshal error: %v", marshalErr)
	}
	renderings := []string{err.Error(), string(encoded)}
	for _, rendering := range renderings {
		if strings.Contains(rendering, testRawErrorCanary) || strings.Contains(rendering, testSecretCanary) {
			t.Fatalf("rendering leaked external material: %q", rendering)
		}
	}
	if !errors.Is(err, errTestRawTailscaleFailure) {
		t.Fatal("internal cause was not preserved for programmatic inspection")
	}
}

func TestErrorIsMatchesOnlyStableCode(t *testing.T) {
	err := newError(ErrorCodeCloseFailed, "safe", nil, nil)
	if !errors.Is(err, &Error{Code: ErrorCodeCloseFailed}) {
		t.Fatal("matching stable code did not match")
	}
	if errors.Is(err, &Error{Code: ErrorCodeUpFailed}) {
		t.Fatal("different stable code matched")
	}
}

func TestNewErrorClonesCallerContext(t *testing.T) {
	contextValues := map[string]string{"operation": "up"}
	err := newError(ErrorCodeUpFailed, "safe", contextValues, nil)
	contextValues["operation"] = "mutated"
	if err.Context["operation"] != "up" {
		t.Fatalf("stored context = %#v", err.Context)
	}
}
