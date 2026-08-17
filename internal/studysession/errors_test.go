package studysession

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"lexinet/internal/model"
)

func TestPublicStudyErrorReturnsOnlyAllowlistedDescriptor(t *testing.T) {
	t.Parallel()
	const canary = "repository-secret-canary"
	public := PublicStudyError(&model.StudyError{
		Code:      model.StudyErrorCodeStateStale,
		Message:   canary,
		Context:   map[string]string{"raw": canary},
		Retryable: true,
		Cause:     errors.New(canary),
	})
	if public.Code != model.StudyErrorCodeStateStale ||
		public.Message != studySafeErrorMessages[model.StudyErrorCodeStateStale] || public.Retryable {
		t.Fatalf("public descriptor = %#v", public)
	}
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), canary) || strings.Contains(string(encoded), "context") || strings.Contains(string(encoded), "cause") {
		t.Fatalf("public descriptor leaked private error data: %s", encoded)
	}
}

func TestPublicStudyErrorFailsClosedAndOwnsRetryability(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name      string
		err       error
		code      model.StudyErrorCode
		retryable bool
	}{
		{name: "nil", code: model.StudyErrorCodeInternal},
		{name: "unknown", err: errors.New("unknown"), code: model.StudyErrorCodeInternal},
		{name: "rate limited", err: &model.StudyError{Code: model.StudyErrorCodeRateLimited, Message: "untrusted", Retryable: false}, code: model.StudyErrorCodeRateLimited, retryable: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			public := PublicStudyError(testCase.err)
			if public.Code != testCase.code || public.Message != studySafeErrorMessages[testCase.code] || public.Retryable != testCase.retryable {
				t.Fatalf("public descriptor = %#v", public)
			}
		})
	}
}
