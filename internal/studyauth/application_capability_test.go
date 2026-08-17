package studyauth

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"tailscale.com/tailcfg"

	"lexinet/internal/model"
	"lexinet/internal/studysession"
)

func TestBoundaryIntersectsBroaderValidCapabilityWithLocalCeiling(t *testing.T) {
	t.Parallel()
	boundary := newTailnetNodeBoundaryForTest(t, []string{testPrimaryProfileRef})
	broaderCapability := capabilityValueForTest(
		[]studysession.StudyOperation{
			studysession.StudyOperationCreateOrResume,
			studysession.StudyOperationGet,
			studysession.StudyOperationSync,
		},
		[]string{testPrimaryProfileRef, testSecondaryProfileRef},
	)
	ctx, principal, err := boundary.Authenticate(
		context.Background(),
		testWhoIsRemoteAddress,
		&recordingWhoIsClient{response: validTailnetNodeWhoIsResponseForTest(
			testStableNodeID,
			[]string{testTailnetNodeTag},
			[]tailcfg.RawMessage{broaderCapability},
		)},
		nil,
	)
	if err != nil {
		t.Fatalf("authenticate broader valid capability: %v", err)
	}

	if err := boundary.AuthorizeStudy(ctx, studysession.StudyAuthorizationRequest{
		Principal:  principal,
		Operation:  studysession.StudyOperationSync,
		Stage:      studysession.StudyAuthorizationStageResolvedTarget,
		ProfileRef: testPrimaryProfileRef,
	}); err != nil {
		t.Fatalf("authorize intersected operation/profile: %v", err)
	}
	for _, request := range []studysession.StudyAuthorizationRequest{
		{
			Principal: principal,
			Operation: studysession.StudyOperationGet,
			Stage:     studysession.StudyAuthorizationStageCapability,
		},
		{
			Principal:  principal,
			Operation:  studysession.StudyOperationSync,
			Stage:      studysession.StudyAuthorizationStageResolvedTarget,
			ProfileRef: testSecondaryProfileRef,
		},
	} {
		err := boundary.AuthorizeStudy(ctx, request)
		requireStudyErrorCodeForTest(t, err, model.StudyErrorCodeAuthorizationDenied)
	}
}

func TestBoundaryRejectsClosedMalformedDuplicateAndOverboundCapabilityValues(t *testing.T) {
	t.Parallel()
	boundary := newTailnetNodeBoundaryForTest(t, []string{testPrimaryProfileRef})
	validCapability := capabilityValueForTest(
		[]studysession.StudyOperation{studysession.StudyOperationSync},
		[]string{testPrimaryProfileRef},
	)
	tooManyProfiles := make([]string, studysession.StudyMaximumScopedAuthorizationProfileRefsPerGrant+1)
	for index := range tooManyProfiles {
		tooManyProfiles[index] = fmt.Sprintf("learner.profile-%d", index)
	}
	tooManyValues := make([]tailcfg.RawMessage, maximumApplicationCapabilityValues+1)
	for index := range tooManyValues {
		tooManyValues[index] = tailcfg.RawMessage(fmt.Sprintf(
			`{"operations":["sync"],"profile_refs":["learner.profile-%d"]}`,
			index,
		))
	}

	testCases := []struct {
		name   string
		values []tailcfg.RawMessage
	}{
		{name: "no values", values: nil},
		{name: "too many values", values: tooManyValues},
		{name: "empty value", values: []tailcfg.RawMessage{""}},
		{name: "overbound value", values: []tailcfg.RawMessage{tailcfg.RawMessage(strings.Repeat(" ", maximumApplicationCapabilityValueBytes+1))}},
		{name: "primitive value", values: []tailcfg.RawMessage{`true`}},
		{name: "array value", values: []tailcfg.RawMessage{`[]`}},
		{name: "unknown member", values: []tailcfg.RawMessage{`{"operations":["sync"],"profile_refs":["learner.primary"],"admin":true}`}},
		{name: "missing operations", values: []tailcfg.RawMessage{`{"profile_refs":["learner.primary"]}`}},
		{name: "missing profiles", values: []tailcfg.RawMessage{`{"operations":["sync"]}`}},
		{name: "wrong member case", values: []tailcfg.RawMessage{`{"Operations":["sync"],"profile_refs":["learner.primary"]}`}},
		{name: "duplicate object member", values: []tailcfg.RawMessage{`{"operations":["sync"],"operations":["create_or_resume"],"profile_refs":["learner.primary"]}`}},
		{name: "empty operations", values: []tailcfg.RawMessage{`{"operations":[],"profile_refs":["learner.primary"]}`}},
		{name: "null operations", values: []tailcfg.RawMessage{`{"operations":null,"profile_refs":["learner.primary"]}`}},
		{name: "unknown operation", values: []tailcfg.RawMessage{`{"operations":["sync_all"],"profile_refs":["learner.primary"]}`}},
		{name: "duplicate filtered operation", values: []tailcfg.RawMessage{`{"operations":["get","get"],"profile_refs":["learner.primary"]}`}},
		{name: "empty profiles", values: []tailcfg.RawMessage{`{"operations":["sync"],"profile_refs":[]}`}},
		{name: "null profiles", values: []tailcfg.RawMessage{`{"operations":["sync"],"profile_refs":null}`}},
		{name: "invalid profile", values: []tailcfg.RawMessage{`{"operations":["sync"],"profile_refs":["learner profile"]}`}},
		{name: "duplicate filtered profile", values: []tailcfg.RawMessage{`{"operations":["sync"],"profile_refs":["learner.secondary","learner.secondary"]}`}},
		{name: "too many profiles", values: []tailcfg.RawMessage{capabilityValueForTest([]studysession.StudyOperation{studysession.StudyOperationSync}, tooManyProfiles)}},
		{name: "no operation intersection", values: []tailcfg.RawMessage{capabilityValueForTest([]studysession.StudyOperation{studysession.StudyOperationGet}, []string{testPrimaryProfileRef})}},
		{name: "no profile intersection", values: []tailcfg.RawMessage{capabilityValueForTest([]studysession.StudyOperation{studysession.StudyOperationSync}, []string{testSecondaryProfileRef})}},
		{name: "duplicate canonical object", values: []tailcfg.RawMessage{validCapability, tailcfg.RawMessage(` { "profile_refs" : ["learner.primary"], "operations" : ["sync"] } `)}},
		{name: "one malformed rejects valid sibling", values: []tailcfg.RawMessage{validCapability, `{"operations":["sync"],"profile_refs":["SENSITIVE-capability-canary"],"extra":true}`}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := boundary.Authenticate(
				context.Background(),
				testWhoIsRemoteAddress,
				&recordingWhoIsClient{response: validTailnetNodeWhoIsResponseForTest(
					testStableNodeID,
					[]string{testTailnetNodeTag},
					testCase.values,
				)},
				nil,
			)
			requireStudyErrorCodeForTest(t, err, model.StudyErrorCodeAuthorizationDenied)
			if strings.Contains(fmt.Sprintf("%+v", err), "SENSITIVE-capability-canary") {
				t.Fatal("capability denial exposed capability JSON")
			}
		})
	}
}

func TestBoundaryIgnoresEmptyIntersectionsWhenAnotherCapabilityObjectAuthorizes(t *testing.T) {
	t.Parallel()
	boundary := newTailnetNodeBoundaryForTest(t, []string{testPrimaryProfileRef})
	ctx, principal, err := boundary.Authenticate(
		context.Background(),
		testWhoIsRemoteAddress,
		&recordingWhoIsClient{response: validTailnetNodeWhoIsResponseForTest(
			testStableNodeID,
			[]string{testTailnetNodeTag},
			[]tailcfg.RawMessage{
				capabilityValueForTest([]studysession.StudyOperation{studysession.StudyOperationGet}, []string{testSecondaryProfileRef}),
				capabilityValueForTest([]studysession.StudyOperation{studysession.StudyOperationSync}, []string{testPrimaryProfileRef}),
			},
		)},
		nil,
	)
	if err != nil {
		t.Fatalf("authenticate with one effective capability object: %v", err)
	}
	if err := boundary.AuthorizeStudy(ctx, studysession.StudyAuthorizationRequest{
		Principal:  principal,
		Operation:  studysession.StudyOperationSync,
		Stage:      studysession.StudyAuthorizationStageResolvedTarget,
		ProfileRef: testPrimaryProfileRef,
	}); err != nil {
		t.Fatalf("authorize effective sibling capability object: %v", err)
	}
}
