package studyauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"

	"lexinet/internal/model"
	"lexinet/internal/studysession"
)

const testScopedToken = "0123456789abcdef0123456789abcdef01234567"

var testScopedTokenSubjectID = uuid.MustParse("11111111-2222-4333-8444-555555555555")

func TestScopedTokenBoundaryAuthenticatesWhoIsPeerWithoutDerivingIdentityFromToken(t *testing.T) {
	if !scopedTokenFileOwnershipSupported {
		t.Skip("owner-only token files deliberately fail closed on this platform")
	}
	t.Parallel()
	tokenFile := writeScopedTokenFileForTest(t, testScopedToken+"\n", 0o600)
	boundary, err := NewBoundary(Policy{
		Mode:                 AuthenticationModeScopedToken,
		AllowedProfileRefs:   []string{testPrimaryProfileRef, testSecondaryProfileRef},
		ScopedTokenSubjectID: testScopedTokenSubjectID,
		ScopedTokenFile:      tokenFile,
	})
	if err != nil {
		t.Fatalf("construct scoped-token boundary: %v", err)
	}
	whoIsClient := &recordingWhoIsClient{response: &apitype.WhoIsResponse{
		Node: &tailcfg.Node{
			StableID: "unrelated-tailnet-node-id",
			Tags:     []string{"tag:not-the-configured-node-tag", "tag:extra"},
		},
		CapMap: tailcfg.PeerCapMap{
			"other.example/cap/irrelevant": {`{"admin":true}`},
		},
	}}
	ctx, principal, err := boundary.Authenticate(
		context.Background(),
		testWhoIsRemoteAddress,
		whoIsClient,
		[]string{scopedTokenAuthorizationPrefix + testScopedToken},
	)
	if err != nil {
		t.Fatalf("authenticate scoped token: %v", err)
	}
	wantPrincipal := newScopedTokenPrincipalForTest(t, testScopedTokenSubjectID)
	if principal != wantPrincipal || principal.String() != "scoped-token:"+testScopedTokenSubjectID.String() {
		t.Fatalf("scoped-token principal = %s, want %s", principal.String(), wantPrincipal.String())
	}
	for _, operation := range []studysession.StudyOperation{
		studysession.StudyOperationCreateOrResume,
		studysession.StudyOperationSync,
	} {
		if err := boundary.AuthorizeStudy(ctx, studysession.StudyAuthorizationRequest{
			Principal:  principal,
			Operation:  operation,
			Stage:      studysession.StudyAuthorizationStageResolvedTarget,
			ProfileRef: testSecondaryProfileRef,
		}); err != nil {
			t.Fatalf("authorize scoped-token %s: %v", operation, err)
		}
	}
	err = boundary.AuthorizeStudy(ctx, studysession.StudyAuthorizationRequest{
		Principal: principal,
		Operation: studysession.StudyOperationGet,
		Stage:     studysession.StudyAuthorizationStageCapability,
	})
	requireStudyErrorCodeForTest(t, err, model.StudyErrorCodeAuthorizationDenied)
	if whoIsClient.calls.Load() != 1 {
		t.Fatalf("WhoIs calls = %d, want 1", whoIsClient.calls.Load())
	}

	boundaryRepresentation := fmt.Sprintf("%#v", boundary)
	if strings.Contains(boundaryRepresentation, testScopedToken) || strings.Contains(boundaryRepresentation, tokenFile) {
		t.Fatal("boundary retained raw scoped-token material or its source path")
	}
}

func TestScopedTokenBoundaryRequiresFreshWhoIsAndExactBearerHeader(t *testing.T) {
	if !scopedTokenFileOwnershipSupported {
		t.Skip("owner-only token files deliberately fail closed on this platform")
	}
	t.Parallel()
	boundary := newScopedTokenBoundaryForTest(t, testScopedToken)
	validWhoIs := &apitype.WhoIsResponse{Node: &tailcfg.Node{StableID: testStableNodeID}}

	for _, testCase := range []struct {
		name   string
		values []string
	}{
		{name: "missing header", values: nil},
		{name: "empty header", values: []string{""}},
		{name: "multiple headers", values: []string{scopedTokenAuthorizationPrefix + testScopedToken, scopedTokenAuthorizationPrefix + testScopedToken}},
		{name: "lowercase scheme", values: []string{"bearer " + testScopedToken}},
		{name: "leading whitespace", values: []string{" " + scopedTokenAuthorizationPrefix + testScopedToken}},
		{name: "extra separator", values: []string{scopedTokenAuthorizationPrefix + " " + testScopedToken}},
		{name: "wrong token", values: []string{scopedTokenAuthorizationPrefix + strings.Repeat("x", len(testScopedToken))}},
		{name: "short token", values: []string{scopedTokenAuthorizationPrefix + "short"}},
		{name: "overbound token", values: []string{scopedTokenAuthorizationPrefix + strings.Repeat("x", maximumScopedTokenBytes+1)}},
		{name: "token with whitespace", values: []string{scopedTokenAuthorizationPrefix + strings.Repeat("x", minimumScopedTokenBytes) + " "}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			whoIsClient := &recordingWhoIsClient{response: validWhoIs}
			_, _, err := boundary.Authenticate(
				context.Background(),
				testWhoIsRemoteAddress,
				whoIsClient,
				testCase.values,
			)
			requireStudyErrorCodeForTest(t, err, model.StudyErrorCodeUnauthorized)
			if whoIsClient.calls.Load() != 1 {
				t.Fatalf("WhoIs calls = %d, want 1 before header decision", whoIsClient.calls.Load())
			}
		})
	}

	for _, testCase := range []struct {
		name     string
		response *apitype.WhoIsResponse
		err      error
		wantCode model.StudyErrorCode
	}{
		{name: "nil response", wantCode: model.StudyErrorCodeUnauthorized},
		{name: "nil node", response: &apitype.WhoIsResponse{}, wantCode: model.StudyErrorCodeUnauthorized},
		{name: "empty stable ID", response: &apitype.WhoIsResponse{Node: &tailcfg.Node{}}, wantCode: model.StudyErrorCodeUnauthorized},
		{name: "WhoIs error", err: fmt.Errorf("SENSITIVE-token-whois-cause"), wantCode: model.StudyErrorCodeServiceUnavailable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := boundary.Authenticate(
				context.Background(),
				testWhoIsRemoteAddress,
				&recordingWhoIsClient{response: testCase.response, err: testCase.err},
				[]string{scopedTokenAuthorizationPrefix + testScopedToken},
			)
			requireStudyErrorCodeForTest(t, err, testCase.wantCode)
			if strings.Contains(fmt.Sprintf("%+v", err), "SENSITIVE-token-whois-cause") {
				t.Fatal("scoped-token WhoIs error exposed its cause")
			}
		})
	}
}

func TestNewBoundaryRejectsInsecureScopedTokenPolicyAndFile(t *testing.T) {
	if !scopedTokenFileOwnershipSupported {
		t.Skip("owner-only token files deliberately fail closed on this platform")
	}
	t.Parallel()
	temporaryDirectory := t.TempDir()
	sensitivePathCanary := "SENSITIVE-token-path-canary"
	nonexistentPath := filepath.Join(temporaryDirectory, sensitivePathCanary)
	directoryPath := filepath.Join(temporaryDirectory, "token-directory")
	if err := os.Mkdir(directoryPath, 0o700); err != nil {
		t.Fatalf("create token directory fixture: %v", err)
	}
	validPath := writeScopedTokenFileForTest(t, testScopedToken, 0o600)
	insecureModePath := writeScopedTokenFileForTest(t, testScopedToken, 0o644)
	shortPath := writeScopedTokenFileForTest(t, strings.Repeat("x", minimumScopedTokenBytes-1), 0o600)
	longPath := writeScopedTokenFileForTest(t, strings.Repeat("x", maximumScopedTokenBytes+1), 0o600)
	spacePath := writeScopedTokenFileForTest(t, strings.Repeat("x", minimumScopedTokenBytes)+" ", 0o600)
	newlinePath := writeScopedTokenFileForTest(t, strings.Repeat("x", minimumScopedTokenBytes)+"\n\n", 0o600)
	nonASCIIPath := writeScopedTokenFileForTest(t, strings.Repeat("x", minimumScopedTokenBytes)+"é", 0o600)
	symlinkPath := filepath.Join(temporaryDirectory, "token-symlink")
	if err := os.Symlink(validPath, symlinkPath); err != nil {
		t.Fatalf("create token symlink fixture: %v", err)
	}

	validPolicy := Policy{
		Mode:                 AuthenticationModeScopedToken,
		AllowedProfileRefs:   []string{testPrimaryProfileRef},
		ScopedTokenSubjectID: testScopedTokenSubjectID,
		ScopedTokenFile:      validPath,
	}
	for _, testCase := range []struct {
		name   string
		mutate func(*Policy)
		field  string
	}{
		{name: "node tag mixed into token mode", mutate: func(policy *Policy) { policy.RequiredNodeTag = testTailnetNodeTag }, field: authenticationConfigurationFieldModeSpecificFields},
		{name: "app capability mixed into token mode", mutate: func(policy *Policy) { policy.AppCapability = testAppCapability }, field: authenticationConfigurationFieldModeSpecificFields},
		{name: "nil subject", mutate: func(policy *Policy) { policy.ScopedTokenSubjectID = uuid.Nil }, field: authenticationConfigurationFieldScopedTokenSubject},
		{name: "empty path", mutate: func(policy *Policy) { policy.ScopedTokenFile = "" }, field: authenticationConfigurationFieldScopedTokenFile},
		{name: "relative path", mutate: func(policy *Policy) { policy.ScopedTokenFile = sensitivePathCanary }, field: authenticationConfigurationFieldScopedTokenFile},
		{name: "nonexistent path", mutate: func(policy *Policy) { policy.ScopedTokenFile = nonexistentPath }, field: authenticationConfigurationFieldScopedTokenFile},
		{name: "directory", mutate: func(policy *Policy) { policy.ScopedTokenFile = directoryPath }, field: authenticationConfigurationFieldScopedTokenFile},
		{name: "symlink", mutate: func(policy *Policy) { policy.ScopedTokenFile = symlinkPath }, field: authenticationConfigurationFieldScopedTokenFile},
		{name: "group readable mode", mutate: func(policy *Policy) { policy.ScopedTokenFile = insecureModePath }, field: authenticationConfigurationFieldScopedTokenFile},
		{name: "short token", mutate: func(policy *Policy) { policy.ScopedTokenFile = shortPath }, field: authenticationConfigurationFieldScopedTokenFile},
		{name: "long token", mutate: func(policy *Policy) { policy.ScopedTokenFile = longPath }, field: authenticationConfigurationFieldScopedTokenFile},
		{name: "token with space", mutate: func(policy *Policy) { policy.ScopedTokenFile = spacePath }, field: authenticationConfigurationFieldScopedTokenFile},
		{name: "multiple trailing newlines", mutate: func(policy *Policy) { policy.ScopedTokenFile = newlinePath }, field: authenticationConfigurationFieldScopedTokenFile},
		{name: "non-ASCII token", mutate: func(policy *Policy) { policy.ScopedTokenFile = nonASCIIPath }, field: authenticationConfigurationFieldScopedTokenFile},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			policy := validPolicy
			testCase.mutate(&policy)
			boundary, err := NewBoundary(policy)
			if boundary != nil {
				t.Fatal("invalid scoped-token policy returned a boundary")
			}
			requireStudyErrorCodeForTest(t, err, model.StudyErrorCodeSchemaInvalid)
			var studyError *model.StudyError
			if !errors.As(err, &studyError) || studyError.Context["field"] != testCase.field ||
				studyError.Context["operation"] != tailnetStudyAuthenticationConfigurationOperation || studyError.Cause != nil {
				t.Fatalf("unsafe scoped-token configuration error: %#v", studyError)
			}
			if strings.Contains(fmt.Sprintf("%+v", err), sensitivePathCanary) || strings.Contains(fmt.Sprintf("%+v", err), testScopedToken) {
				t.Fatal("scoped-token configuration error exposed a path or token")
			}
		})
	}
}

func TestNewBoundaryAcceptsExactScopedTokenFileModesAndOptionalLF(t *testing.T) {
	if !scopedTokenFileOwnershipSupported {
		t.Skip("owner-only token files deliberately fail closed on this platform")
	}
	t.Parallel()
	for _, testCase := range []struct {
		name    string
		content string
		mode    os.FileMode
	}{
		{name: "read only", content: testScopedToken, mode: 0o400},
		{name: "owner writable", content: testScopedToken, mode: 0o600},
		{name: "optional LF", content: testScopedToken + "\n", mode: 0o600},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := writeScopedTokenFileForTest(t, testCase.content, testCase.mode)
			if _, err := NewBoundary(Policy{
				Mode:                 AuthenticationModeScopedToken,
				AllowedProfileRefs:   []string{testPrimaryProfileRef},
				ScopedTokenSubjectID: testScopedTokenSubjectID,
				ScopedTokenFile:      path,
			}); err != nil {
				t.Fatalf("valid scoped-token file rejected: %v", err)
			}
		})
	}
}

func newScopedTokenBoundaryForTest(t *testing.T, token string) *Boundary {
	t.Helper()
	path := writeScopedTokenFileForTest(t, token, 0o600)
	boundary, err := NewBoundary(Policy{
		Mode:                 AuthenticationModeScopedToken,
		AllowedProfileRefs:   []string{testPrimaryProfileRef},
		ScopedTokenSubjectID: testScopedTokenSubjectID,
		ScopedTokenFile:      path,
	})
	if err != nil {
		t.Fatalf("construct scoped-token boundary: %v", err)
	}
	return boundary
}

func writeScopedTokenFileForTest(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scoped-token")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write scoped-token fixture: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("set scoped-token fixture mode: %v", err)
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("resolve scoped-token fixture path: %v", err)
	}
	return absolutePath
}
