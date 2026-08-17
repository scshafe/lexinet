package studyauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"tailscale.com/tailcfg"

	"lexinet/internal/model"
	"lexinet/internal/studysession"
)

func TestAuthenticationModeValidUsesClosedVocabulary(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		mode AuthenticationMode
		want bool
	}{
		{mode: AuthenticationModeTailnetNode, want: true},
		{mode: AuthenticationModeScopedToken, want: true},
		{mode: "tailnet-node"},
		{mode: "token"},
		{mode: ""},
	} {
		if got := testCase.mode.Valid(); got != testCase.want {
			t.Fatalf("mode %q validity = %t, want %t", testCase.mode, got, testCase.want)
		}
	}
}

func TestNewBoundaryRejectsInvalidTailnetNodePolicyWithRedactedStructuredError(t *testing.T) {
	t.Parallel()
	sensitiveCanary := "SENSITIVE-policy-value-canary"
	validPolicy := Policy{
		Mode:               AuthenticationModeTailnetNode,
		RequiredNodeTag:    testTailnetNodeTag,
		AppCapability:      testAppCapability,
		AllowedProfileRefs: []string{testPrimaryProfileRef},
	}
	tooManyProfiles := make([]string, studysession.StudyMaximumScopedAuthorizationProfileRefsPerGrant+1)
	for index := range tooManyProfiles {
		tooManyProfiles[index] = fmt.Sprintf("learner.profile-%d", index)
	}

	testCases := []struct {
		name   string
		mutate func(*Policy)
		field  string
	}{
		{name: "invalid mode", mutate: func(policy *Policy) { policy.Mode = "automatic" }, field: authenticationConfigurationFieldMode},
		{name: "empty profiles", mutate: func(policy *Policy) { policy.AllowedProfileRefs = nil }, field: authenticationConfigurationFieldAllowedProfileRefs},
		{name: "invalid profile", mutate: func(policy *Policy) { policy.AllowedProfileRefs = []string{"learner profile " + sensitiveCanary} }, field: authenticationConfigurationFieldAllowedProfileRefs},
		{name: "duplicate profile", mutate: func(policy *Policy) {
			policy.AllowedProfileRefs = []string{testPrimaryProfileRef, testPrimaryProfileRef}
		}, field: authenticationConfigurationFieldAllowedProfileRefs},
		{name: "too many profiles", mutate: func(policy *Policy) { policy.AllowedProfileRefs = tooManyProfiles }, field: authenticationConfigurationFieldAllowedProfileRefs},
		{name: "empty tag", mutate: func(policy *Policy) { policy.RequiredNodeTag = "" }, field: authenticationConfigurationFieldRequiredNodeTag},
		{name: "invalid tag", mutate: func(policy *Policy) { policy.RequiredNodeTag = "tag:" + sensitiveCanary + "@" }, field: authenticationConfigurationFieldRequiredNodeTag},
		{name: "overbound tag", mutate: func(policy *Policy) {
			policy.RequiredNodeTag = "tag:" + strings.Repeat("a", maximumRequiredNodeTagBytes)
		}, field: authenticationConfigurationFieldRequiredNodeTag},
		{name: "empty capability", mutate: func(policy *Policy) { policy.AppCapability = "" }, field: authenticationConfigurationFieldAppCapability},
		{name: "scheme capability", mutate: func(policy *Policy) { policy.AppCapability = "https://lexinet.example/cap/lexinet-study-v1" }, field: authenticationConfigurationFieldAppCapability},
		{name: "uncontrolled shape capability", mutate: func(policy *Policy) { policy.AppCapability = tailcfg.PeerCapability(sensitiveCanary) }, field: authenticationConfigurationFieldAppCapability},
		{name: "token subject mixed into node mode", mutate: func(policy *Policy) { policy.ScopedTokenSubjectID = uuid.New() }, field: authenticationConfigurationFieldModeSpecificFields},
		{name: "token file mixed into node mode", mutate: func(policy *Policy) { policy.ScopedTokenFile = "/" + sensitiveCanary }, field: authenticationConfigurationFieldModeSpecificFields},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			policy := validPolicy
			policy.AllowedProfileRefs = append([]string(nil), validPolicy.AllowedProfileRefs...)
			testCase.mutate(&policy)
			boundary, err := NewBoundary(policy)
			if boundary != nil {
				t.Fatal("invalid policy returned a boundary")
			}
			requireStudyErrorCodeForTest(t, err, model.StudyErrorCodeSchemaInvalid)
			var studyError *model.StudyError
			if !errors.As(err, &studyError) {
				t.Fatal("invalid policy did not return a study error")
			}
			if studyError.Context["operation"] != tailnetStudyAuthenticationConfigurationOperation ||
				studyError.Context["field"] != testCase.field || studyError.Cause != nil {
				t.Fatalf("unsafe configuration error context: %#v", studyError)
			}
			encoded, marshalErr := json.Marshal(studyError)
			if marshalErr != nil {
				t.Fatalf("marshal structured policy error: %v", marshalErr)
			}
			if strings.Contains(string(encoded), sensitiveCanary) || strings.Contains(fmt.Sprintf("%+v", err), sensitiveCanary) {
				t.Fatal("configuration error exposed supplied policy content")
			}
		})
	}
}

func TestNewBoundaryAcceptsOnlyControlledDomainCapabilityShape(t *testing.T) {
	t.Parallel()
	for _, capability := range []tailcfg.PeerCapability{
		"example.com/cap/lexinet-study-v1",
		"study.auth.example.com/cap/lexinet-study-v1",
		"a-b.example/cap/lexinet-study-v1",
	} {
		policy := Policy{
			Mode:               AuthenticationModeTailnetNode,
			RequiredNodeTag:    testTailnetNodeTag,
			AppCapability:      capability,
			AllowedProfileRefs: []string{testPrimaryProfileRef},
		}
		if _, err := NewBoundary(policy); err != nil {
			t.Fatalf("valid application capability %q rejected: %v", capability, err)
		}
	}

	for _, capability := range []tailcfg.PeerCapability{
		"example/cap/lexinet-study-v1",
		"Example.com/cap/lexinet-study-v1",
		"example.com:443/cap/lexinet-study-v1",
		"127.0.0.1/cap/lexinet-study-v1",
		"example.com./cap/lexinet-study-v1",
		"-example.com/cap/lexinet-study-v1",
		"example_.com/cap/lexinet-study-v1",
		"example.com/cap/other",
		"example.com/cap/lexinet-study-v1?grant=all",
		"example.com/cap/lexinet-study-v1#fragment",
		"example.com//cap/lexinet-study-v1",
	} {
		policy := Policy{
			Mode:               AuthenticationModeTailnetNode,
			RequiredNodeTag:    testTailnetNodeTag,
			AppCapability:      capability,
			AllowedProfileRefs: []string{testPrimaryProfileRef},
		}
		_, err := NewBoundary(policy)
		requireStudyErrorCodeForTest(t, err, model.StudyErrorCodeSchemaInvalid)
	}
}

func TestNewBoundaryDetachesAllowedProfileCeiling(t *testing.T) {
	t.Parallel()
	profileRefs := []string{testPrimaryProfileRef}
	boundary, err := NewBoundary(Policy{
		Mode:               AuthenticationModeTailnetNode,
		RequiredNodeTag:    testTailnetNodeTag,
		AppCapability:      testAppCapability,
		AllowedProfileRefs: profileRefs,
	})
	if err != nil {
		t.Fatalf("construct boundary for detachment test: %v", err)
	}
	profileRefs[0] = testSecondaryProfileRef

	ctx, principal, err := boundary.Authenticate(
		context.Background(),
		testWhoIsRemoteAddress,
		&recordingWhoIsClient{response: validTailnetNodeWhoIsResponseForTest(
			testStableNodeID,
			[]string{testTailnetNodeTag},
			[]tailcfg.RawMessage{capabilityValueForTest(
				[]studysession.StudyOperation{studysession.StudyOperationSync},
				[]string{testPrimaryProfileRef},
			)},
		)},
		nil,
	)
	if err != nil {
		t.Fatalf("authenticate detached profile policy: %v", err)
	}
	if err := boundary.AuthorizeStudy(ctx, studysession.StudyAuthorizationRequest{
		Principal:  principal,
		Operation:  studysession.StudyOperationSync,
		Stage:      studysession.StudyAuthorizationStageResolvedTarget,
		ProfileRef: testPrimaryProfileRef,
	}); err != nil {
		t.Fatalf("authorize original detached profile: %v", err)
	}
}
