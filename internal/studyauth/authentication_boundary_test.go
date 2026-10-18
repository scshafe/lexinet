package studyauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"

	"lexinet/internal/model"
	"lexinet/internal/studysession"
)

const (
	testTailnetNodeTag      = "tag:house-panel"
	testAppCapability       = tailcfg.PeerCapability("lexinet.example/cap/lexinet-study-v1")
	testPrimaryProfileRef   = "learner.primary"
	testSecondaryProfileRef = "learner.secondary"
	testWhoIsRemoteAddress  = "100.64.0.10:44321"
	testStableNodeID        = tailcfg.StableNodeID("node-123")
)

type recordingWhoIsClient struct {
	response *apitype.WhoIsResponse
	err      error
	calls    atomic.Int64
	mu       sync.Mutex
	remotes  []string
}

func (client *recordingWhoIsClient) WhoIs(_ context.Context, remoteAddr string) (*apitype.WhoIsResponse, error) {
	client.calls.Add(1)
	client.mu.Lock()
	client.remotes = append(client.remotes, remoteAddr)
	client.mu.Unlock()
	return client.response, client.err
}

func TestBoundaryAuthenticatesExactTailnetNodeAndDelegatesScopedAuthorization(t *testing.T) {
	t.Parallel()
	boundary := newTailnetNodeBoundaryForTest(t, []string{testPrimaryProfileRef})
	whoIsClient := &recordingWhoIsClient{response: validTailnetNodeWhoIsResponseForTest(
		testStableNodeID,
		[]string{testTailnetNodeTag},
		[]tailcfg.RawMessage{capabilityValueForTest(
			[]studysession.StudyOperation{studysession.StudyOperationCreateOrResume, studysession.StudyOperationSync},
			[]string{testPrimaryProfileRef},
		)},
	)}

	ctx, principal, err := boundary.Authenticate(context.Background(), testWhoIsRemoteAddress, whoIsClient, nil)
	if err != nil {
		t.Fatalf("authenticate exact tailnet node: %v", err)
	}
	if principal.String() != "tailnet-node:56f4a689-6592-520e-b45f-1b26fef1390d" {
		t.Fatalf("unexpected frozen principal: %s", principal.String())
	}
	contextPrincipal, ok := boundary.PrincipalFromContext(ctx)
	if !ok || contextPrincipal != principal {
		t.Fatal("authenticated context did not preserve the exact principal")
	}
	for _, request := range []studysession.StudyAuthorizationRequest{
		{
			Principal: principal,
			Operation: studysession.StudyOperationCreateOrResume,
			Stage:     studysession.StudyAuthorizationStageCapability,
		},
		{
			Principal:  principal,
			Operation:  studysession.StudyOperationCreateOrResume,
			Stage:      studysession.StudyAuthorizationStageResolvedTarget,
			ProfileRef: testPrimaryProfileRef,
		},
		{
			Principal:  principal,
			Operation:  studysession.StudyOperationSync,
			Stage:      studysession.StudyAuthorizationStageResolvedTarget,
			ProfileRef: testPrimaryProfileRef,
		},
	} {
		if err := boundary.AuthorizeStudy(ctx, request); err != nil {
			t.Fatalf("authorize exact scoped request %+v: %v", request, err)
		}
	}
	if whoIsClient.calls.Load() != 1 {
		t.Fatalf("WhoIs calls = %d, want 1", whoIsClient.calls.Load())
	}
	whoIsClient.mu.Lock()
	defer whoIsClient.mu.Unlock()
	if len(whoIsClient.remotes) != 1 || whoIsClient.remotes[0] != testWhoIsRemoteAddress {
		t.Fatalf("WhoIs remote addresses = %v", whoIsClient.remotes)
	}
}

// TestPlanLexinetStep5Verify1Item1UnitTestsRejectSpoofedIdentityHeadersAndWrong
// is the exact aggregate Step 5 evidence gate for production Boundary paths.
func TestPlanLexinetStep5Verify1Item1UnitTestsRejectSpoofedIdentityHeadersAndWrong(t *testing.T) {
	t.Parallel()
	boundary := newTailnetNodeBoundaryForTest(t, []string{testPrimaryProfileRef})
	validCapability := []tailcfg.RawMessage{capabilityValueForTest(
		[]studysession.StudyOperation{studysession.StudyOperationSync},
		[]string{testPrimaryProfileRef},
	)}

	for _, testCase := range []struct {
		name                string
		tags                []string
		capabilityValues    []tailcfg.RawMessage
		includeCapability   bool
		authorizationValues []string
	}{
		{name: "missing tag", tags: nil, capabilityValues: validCapability, includeCapability: true},
		{name: "wrong tag", tags: []string{"tag:other-panel"}, capabilityValues: validCapability, includeCapability: true},
		{name: "additional tag", tags: []string{testTailnetNodeTag, "tag:extra"}, capabilityValues: validCapability, includeCapability: true},
		{name: "spoofed authorization identity", tags: []string{testTailnetNodeTag}, capabilityValues: validCapability, includeCapability: true, authorizationValues: []string{"Bearer spoofed-identity-header-that-is-not-accepted"}},
		{name: "missing capability", tags: []string{testTailnetNodeTag}, includeCapability: false},
		{name: "empty capability", tags: []string{testTailnetNodeTag}, capabilityValues: nil, includeCapability: true},
		{name: "malformed capability", tags: []string{testTailnetNodeTag}, capabilityValues: []tailcfg.RawMessage{`{"operations":["sync"],"operations":["create_or_resume"],"profile_refs":["learner.primary"]}`}, includeCapability: true},
		{name: "outside local profile ceiling", tags: []string{testTailnetNodeTag}, capabilityValues: []tailcfg.RawMessage{capabilityValueForTest([]studysession.StudyOperation{studysession.StudyOperationSync}, []string{testSecondaryProfileRef})}, includeCapability: true},
		{name: "outside local operation ceiling", tags: []string{testTailnetNodeTag}, capabilityValues: []tailcfg.RawMessage{capabilityValueForTest([]studysession.StudyOperation{studysession.StudyOperationGet}, []string{testPrimaryProfileRef})}, includeCapability: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			response := validTailnetNodeWhoIsResponseForTest(
				testStableNodeID,
				testCase.tags,
				testCase.capabilityValues,
			)
			if !testCase.includeCapability {
				response.CapMap = tailcfg.PeerCapMap{}
			}
			_, _, err := boundary.Authenticate(
				context.Background(),
				testWhoIsRemoteAddress,
				&recordingWhoIsClient{response: response},
				testCase.authorizationValues,
			)
			requireStudyErrorCodeForTest(t, err, model.StudyErrorCodeAuthorizationDenied)
		})
	}

	ctx, principal, err := boundary.Authenticate(
		context.Background(),
		testWhoIsRemoteAddress,
		&recordingWhoIsClient{response: validTailnetNodeWhoIsResponseForTest(
			testStableNodeID,
			[]string{testTailnetNodeTag},
			validCapability,
		)},
		nil,
	)
	if err != nil {
		t.Fatalf("authenticate aggregate authorization fixture: %v", err)
	}
	for _, testCase := range []struct {
		name    string
		ctx     context.Context
		request studysession.StudyAuthorizationRequest
	}{
		{
			name: "forwarded identity context cannot forge claims",
			ctx: context.WithValue(
				context.WithValue(context.Background(), "X-Tailscale-Tags", testTailnetNodeTag),
				"X-Lexinet-App-Capability",
				string(testAppCapability),
			),
			request: studysession.StudyAuthorizationRequest{
				Principal: principal,
				Operation: studysession.StudyOperationSync,
				Stage:     studysession.StudyAuthorizationStageCapability,
			},
		},
		{
			name: "ungranted create operation",
			ctx:  ctx,
			request: studysession.StudyAuthorizationRequest{
				Principal: principal,
				Operation: studysession.StudyOperationCreateOrResume,
				Stage:     studysession.StudyAuthorizationStageCapability,
			},
		},
		{
			name: "get operation is never tailnet authorized",
			ctx:  ctx,
			request: studysession.StudyAuthorizationRequest{
				Principal: principal,
				Operation: studysession.StudyOperationGet,
				Stage:     studysession.StudyAuthorizationStageCapability,
			},
		},
		{
			name: "wrong resolved profile",
			ctx:  ctx,
			request: studysession.StudyAuthorizationRequest{
				Principal:  principal,
				Operation:  studysession.StudyOperationSync,
				Stage:      studysession.StudyAuthorizationStageResolvedTarget,
				ProfileRef: testSecondaryProfileRef,
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := boundary.AuthorizeStudy(testCase.ctx, testCase.request)
			requireStudyErrorCodeForTest(t, err, model.StudyErrorCodeAuthorizationDenied)
		})
	}

	otherBoundary := newTailnetNodeBoundaryForTest(t, []string{testPrimaryProfileRef})
	err = otherBoundary.AuthorizeStudy(ctx, studysession.StudyAuthorizationRequest{
		Principal: principal,
		Operation: studysession.StudyOperationSync,
		Stage:     studysession.StudyAuthorizationStageCapability,
	})
	requireStudyErrorCodeForTest(t, err, model.StudyErrorCodeAuthorizationDenied)
}

func TestBoundaryPreservesCapabilityObjectOperationProfileRelationships(t *testing.T) {
	t.Parallel()
	boundary := newTailnetNodeBoundaryForTest(t, []string{testPrimaryProfileRef, testSecondaryProfileRef})
	whoIs := validTailnetNodeWhoIsResponseForTest(
		testStableNodeID,
		[]string{testTailnetNodeTag},
		[]tailcfg.RawMessage{
			capabilityValueForTest(
				[]studysession.StudyOperation{studysession.StudyOperationSync},
				[]string{testPrimaryProfileRef},
			),
			capabilityValueForTest(
				[]studysession.StudyOperation{studysession.StudyOperationCreateOrResume},
				[]string{testSecondaryProfileRef},
			),
		},
	)
	ctx, principal, err := boundary.Authenticate(
		context.Background(),
		testWhoIsRemoteAddress,
		&recordingWhoIsClient{response: whoIs},
		nil,
	)
	if err != nil {
		t.Fatalf("authenticate relational grant fixture: %v", err)
	}

	for _, testCase := range []struct {
		name       string
		operation  studysession.StudyOperation
		profileRef string
		allowed    bool
	}{
		{name: "sync primary allowed", operation: studysession.StudyOperationSync, profileRef: testPrimaryProfileRef, allowed: true},
		{name: "create secondary allowed", operation: studysession.StudyOperationCreateOrResume, profileRef: testSecondaryProfileRef, allowed: true},
		{name: "create primary cross product denied", operation: studysession.StudyOperationCreateOrResume, profileRef: testPrimaryProfileRef},
		{name: "sync secondary cross product denied", operation: studysession.StudyOperationSync, profileRef: testSecondaryProfileRef},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := boundary.AuthorizeStudy(ctx, studysession.StudyAuthorizationRequest{
				Principal:  principal,
				Operation:  testCase.operation,
				Stage:      studysession.StudyAuthorizationStageResolvedTarget,
				ProfileRef: testCase.profileRef,
			})
			if testCase.allowed && err != nil {
				t.Fatalf("authorize exact capability relation: %v", err)
			}
			if !testCase.allowed {
				requireStudyErrorCodeForTest(t, err, model.StudyErrorCodeAuthorizationDenied)
			}
		})
	}
}

func TestBoundaryPreservesLoopbackOperatorAndRequiresClaimsForNonoperator(t *testing.T) {
	t.Parallel()
	boundary := newTailnetNodeBoundaryForTest(t, []string{testPrimaryProfileRef})
	for _, operation := range []studysession.StudyOperation{
		studysession.StudyOperationCreateOrResume,
		studysession.StudyOperationGet,
		studysession.StudyOperationSync,
	} {
		if err := boundary.AuthorizeStudy(context.Background(), studysession.StudyAuthorizationRequest{
			Principal: model.OperatorStudyPrincipalID(),
			Operation: operation,
			Stage:     studysession.StudyAuthorizationStageCapability,
		}); err != nil {
			t.Fatalf("preserve operator operation %s: %v", operation, err)
		}
	}

	nonoperator, err := deriveTailnetNodePrincipal(testStableNodeID)
	if err != nil {
		t.Fatalf("derive nonoperator fixture: %v", err)
	}
	err = boundary.AuthorizeStudy(context.Background(), studysession.StudyAuthorizationRequest{
		Principal: nonoperator,
		Operation: studysession.StudyOperationSync,
		Stage:     studysession.StudyAuthorizationStageCapability,
	})
	requireStudyErrorCodeForTest(t, err, model.StudyErrorCodeAuthorizationDenied)
}

func TestTailnetNodePrincipalDerivationUsesExactBoundedStableIDBytes(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name     string
		stableID tailcfg.StableNodeID
		want     string
	}{
		{name: "frozen vector", stableID: "node-123", want: "tailnet-node:56f4a689-6592-520e-b45f-1b26fef1390d"},
		{name: "case preserved", stableID: "Node-123", want: "tailnet-node:96a0945b-3846-5a13-9fd5-05d59b07d9b2"},
		{name: "leading space preserved", stableID: " node-123", want: "tailnet-node:9bcf324d-7838-5778-ae94-4fde986679cf"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			principal, err := deriveTailnetNodePrincipal(testCase.stableID)
			if err != nil {
				t.Fatalf("derive exact stable node ID: %v", err)
			}
			if principal.String() != testCase.want {
				t.Fatalf("principal = %s, want %s", principal.String(), testCase.want)
			}
		})
	}
}

func TestBoundaryAcceptsOpaqueStableNodeIDAtExactByteBounds(t *testing.T) {
	t.Parallel()
	boundary := newTailnetNodeBoundaryForTest(t, []string{testPrimaryProfileRef})
	for _, stableID := range []tailcfg.StableNodeID{
		tailcfg.StableNodeID(strings.Repeat("x", maximumStableNodeIDBytes)),
		tailcfg.StableNodeID(string([]byte{0xff})),
	} {
		_, principal, err := boundary.Authenticate(
			context.Background(),
			testWhoIsRemoteAddress,
			&recordingWhoIsClient{response: validTailnetNodeWhoIsResponseForTest(
				stableID,
				[]string{testTailnetNodeTag},
				[]tailcfg.RawMessage{capabilityValueForTest(
					[]studysession.StudyOperation{studysession.StudyOperationSync},
					[]string{testPrimaryProfileRef},
				)},
			)},
			nil,
		)
		if err != nil {
			t.Fatalf("authenticate opaque stable ID of %d bytes: %v", len(stableID), err)
		}
		if !principal.Valid() {
			t.Fatal("opaque stable ID did not derive a valid principal")
		}
	}
}

func TestBoundaryRejectsInvalidWhoIsAndRedactsExternalEvidence(t *testing.T) {
	t.Parallel()
	boundary := newTailnetNodeBoundaryForTest(t, []string{testPrimaryProfileRef})
	sensitiveCanary := "SENSITIVE-stable-tag-capability-token-canary"
	for _, testCase := range []struct {
		name     string
		response *apitype.WhoIsResponse
		err      error
		wantCode model.StudyErrorCode
	}{
		{name: "WhoIs failure", err: errors.New(sensitiveCanary), wantCode: model.StudyErrorCodeServiceUnavailable},
		{name: "nil response", wantCode: model.StudyErrorCodeUnauthorized},
		{name: "nil node", response: &apitype.WhoIsResponse{}, wantCode: model.StudyErrorCodeUnauthorized},
		{name: "expired node", response: func() *apitype.WhoIsResponse {
			response := validTailnetNodeWhoIsResponseForTest(testStableNodeID, []string{testTailnetNodeTag}, []tailcfg.RawMessage{capabilityValueForTest([]studysession.StudyOperation{studysession.StudyOperationSync}, []string{testPrimaryProfileRef})})
			response.Node.Expired = true
			return response
		}(), wantCode: model.StudyErrorCodeUnauthorized},
		{name: "empty stable ID", response: validTailnetNodeWhoIsResponseForTest("", []string{testTailnetNodeTag}, []tailcfg.RawMessage{capabilityValueForTest([]studysession.StudyOperation{studysession.StudyOperationSync}, []string{testPrimaryProfileRef})}), wantCode: model.StudyErrorCodeUnauthorized},
		{name: "overbound stable ID", response: validTailnetNodeWhoIsResponseForTest(tailcfg.StableNodeID(strings.Repeat("x", maximumStableNodeIDBytes+1)), []string{testTailnetNodeTag}, []tailcfg.RawMessage{capabilityValueForTest([]studysession.StudyOperation{studysession.StudyOperationSync}, []string{testPrimaryProfileRef})}), wantCode: model.StudyErrorCodeUnauthorized},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := boundary.Authenticate(
				context.Background(),
				testWhoIsRemoteAddress,
				&recordingWhoIsClient{response: testCase.response, err: testCase.err},
				nil,
			)
			requireStudyErrorCodeForTest(t, err, testCase.wantCode)
			if strings.Contains(fmt.Sprintf("%+v", err), sensitiveCanary) {
				t.Fatal("authentication error exposed external evidence")
			}
			var studyError *model.StudyError
			if !errors.As(err, &studyError) || studyError.Cause != nil {
				t.Fatal("authentication error retained an external cause")
			}
		})
	}
}

func TestBoundaryRejectsInvalidRemoteAndNilDependenciesWithoutCallingWhoIs(t *testing.T) {
	t.Parallel()
	boundary := newTailnetNodeBoundaryForTest(t, []string{testPrimaryProfileRef})
	for _, remoteAddr := range []string{
		"",
		"panel.example:443",
		"100.64.0.10:not-a-port",
		"100.64.0.10:0",
		" 100.64.0.10:443",
		strings.Repeat("1", maximumRemoteAddressBytes+1),
	} {
		whoIsClient := &recordingWhoIsClient{response: validTailnetNodeWhoIsResponseForTest(
			testStableNodeID,
			[]string{testTailnetNodeTag},
			[]tailcfg.RawMessage{capabilityValueForTest(
				[]studysession.StudyOperation{studysession.StudyOperationSync},
				[]string{testPrimaryProfileRef},
			)},
		)}
		_, _, err := boundary.Authenticate(context.Background(), remoteAddr, whoIsClient, nil)
		requireStudyErrorCodeForTest(t, err, model.StudyErrorCodeUnauthorized)
		if whoIsClient.calls.Load() != 0 {
			t.Fatalf("invalid remote %q reached WhoIs", remoteAddr)
		}
	}

	var typedNilWhoIsClient *recordingWhoIsClient
	for _, testCase := range []struct {
		name     string
		boundary *Boundary
		ctx      context.Context
		client   WhoIsClient
	}{
		{name: "nil boundary", ctx: context.Background(), client: &recordingWhoIsClient{}},
		{name: "nil context", boundary: boundary, client: &recordingWhoIsClient{}},
		{name: "nil client", boundary: boundary, ctx: context.Background()},
		{name: "typed nil client", boundary: boundary, ctx: context.Background(), client: typedNilWhoIsClient},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := testCase.boundary.Authenticate(testCase.ctx, testWhoIsRemoteAddress, testCase.client, nil)
			requireStudyErrorCodeForTest(t, err, model.StudyErrorCodeInternal)
		})
	}
}

func TestBoundaryCallsWhoIsForEveryValidRemoteRequestAndIsRaceSafe(t *testing.T) {
	boundary := newTailnetNodeBoundaryForTest(t, []string{testPrimaryProfileRef})
	whoIsClient := &recordingWhoIsClient{response: validTailnetNodeWhoIsResponseForTest(
		testStableNodeID,
		[]string{testTailnetNodeTag},
		[]tailcfg.RawMessage{capabilityValueForTest(
			[]studysession.StudyOperation{studysession.StudyOperationSync},
			[]string{testPrimaryProfileRef},
		)},
	)}

	const requestCount = 64
	var waitGroup sync.WaitGroup
	for index := 0; index < requestCount; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			ctx, principal, err := boundary.Authenticate(context.Background(), testWhoIsRemoteAddress, whoIsClient, nil)
			if err != nil {
				t.Errorf("concurrent authenticate: %v", err)
				return
			}
			if err := boundary.AuthorizeStudy(ctx, studysession.StudyAuthorizationRequest{
				Principal:  principal,
				Operation:  studysession.StudyOperationSync,
				Stage:      studysession.StudyAuthorizationStageResolvedTarget,
				ProfileRef: testPrimaryProfileRef,
			}); err != nil {
				t.Errorf("concurrent authorize: %v", err)
			}
		}()
	}
	waitGroup.Wait()
	if whoIsClient.calls.Load() != requestCount {
		t.Fatalf("WhoIs calls = %d, want %d", whoIsClient.calls.Load(), requestCount)
	}
}

func newTailnetNodeBoundaryForTest(t *testing.T, profileRefs []string) *Boundary {
	t.Helper()
	boundary, err := NewBoundary(Policy{
		Mode:               AuthenticationModeTailnetNode,
		RequiredNodeTag:    testTailnetNodeTag,
		AppCapability:      testAppCapability,
		AllowedProfileRefs: append([]string(nil), profileRefs...),
	})
	if err != nil {
		t.Fatalf("construct tailnet-node boundary: %v", err)
	}
	return boundary
}

func validTailnetNodeWhoIsResponseForTest(
	stableID tailcfg.StableNodeID,
	tags []string,
	capabilityValues []tailcfg.RawMessage,
) *apitype.WhoIsResponse {
	return &apitype.WhoIsResponse{
		Node: &tailcfg.Node{
			StableID: stableID,
			Tags:     append([]string(nil), tags...),
		},
		CapMap: tailcfg.PeerCapMap{
			testAppCapability: append([]tailcfg.RawMessage(nil), capabilityValues...),
		},
	}
}

func capabilityValueForTest(operations []studysession.StudyOperation, profileRefs []string) tailcfg.RawMessage {
	operationValues := make([]string, 0, len(operations))
	for _, operation := range operations {
		operationValues = append(operationValues, fmt.Sprintf("%q", operation))
	}
	profileValues := make([]string, 0, len(profileRefs))
	for _, profileRef := range profileRefs {
		profileValues = append(profileValues, fmt.Sprintf("%q", profileRef))
	}
	return tailcfg.RawMessage(fmt.Sprintf(
		`{"operations":[%s],"profile_refs":[%s]}`,
		strings.Join(operationValues, ","),
		strings.Join(profileValues, ","),
	))
}

func requireStudyErrorCodeForTest(t *testing.T, err error, want model.StudyErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %s", want)
	}
	var studyError *model.StudyError
	if !errors.As(err, &studyError) || studyError == nil {
		t.Fatalf("error type = %T, want *model.StudyError", err)
	}
	if studyError.Code != want {
		t.Fatalf("error code = %s, want %s", studyError.Code, want)
	}
}

func newScopedTokenPrincipalForTest(t *testing.T, subjectID uuid.UUID) model.StudyPrincipalID {
	t.Helper()
	principal, err := model.NewStudyPrincipalID(model.StudyPrincipalNamespaceScopedToken, subjectID)
	if err != nil {
		t.Fatalf("construct scoped-token principal: %v", err)
	}
	return principal
}
