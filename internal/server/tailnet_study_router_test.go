package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"

	"lexinet/internal/model"
	"lexinet/internal/studyauth"
	"lexinet/internal/studysession"
)

const (
	tailnetStudyRouterCapability = tailcfg.PeerCapability("example.com/cap/lexinet-study-v1")
	tailnetStudyRouterProfileRef = "learner.primary"
	tailnetStudyRouterNodeTag    = "tag:embedded-house-panel"
)

func TestTailnetStudyRouterAuthenticatesFreshAndExposesOnlyCreateAndSync(t *testing.T) {
	boundary := newTailnetStudyBoundaryForServerTest(t)
	whoIs := &tailnetStudyWhoIsRecorder{responses: []*apitype.WhoIsResponse{
		validTailnetStudyWhoIsResponse(),
		validTailnetStudyWhoIsResponse(),
		validTailnetStudyWhoIsResponse(),
		validTailnetStudyWhoIsResponse(),
		validTailnetStudyWhoIsResponse(),
	}}
	service := &fakeStudySessionService{
		createOrResume: func(request *http.Request, principal model.StudyPrincipalID, _ studysession.StudyCreateSessionRequest) (*studysession.StudyCreateSessionResult, error) {
			if !principal.Valid() || !strings.HasPrefix(principal.String(), "tailnet-node:") {
				t.Fatalf("principal = %q, want derived tailnet-node principal", principal.String())
			}
			if err := boundary.AuthorizeStudy(request.Context(), studysession.StudyAuthorizationRequest{
				Principal:  principal,
				Operation:  studysession.StudyOperationCreateOrResume,
				Stage:      studysession.StudyAuthorizationStageResolvedTarget,
				ProfileRef: tailnetStudyRouterProfileRef,
			}); err != nil {
				t.Fatalf("authorize derived request: %v", err)
			}
			return studyCreateResult(false), nil
		},
		sync: func(request *http.Request, principal model.StudyPrincipalID, _ string, _ studysession.StudySyncRequest) (*studysession.StudySyncResult, error) {
			if err := boundary.AuthorizeStudy(request.Context(), studysession.StudyAuthorizationRequest{
				Principal:  principal,
				Operation:  studysession.StudyOperationSync,
				Stage:      studysession.StudyAuthorizationStageResolvedTarget,
				ProfileRef: tailnetStudyRouterProfileRef,
			}); err != nil {
				t.Fatalf("authorize derived sync request: %v", err)
			}
			return studySyncResult(), nil
		},
	}
	router, err := NewTailnetStudyRouter(service, boundary, whoIs)
	if err != nil {
		t.Fatal(err)
	}

	create := httptest.NewRequest(http.MethodPost, "/api/v1/study/sessions", strings.NewReader(`{}`))
	create.Header.Set("Content-Type", "application/json")
	createResponse := httptest.NewRecorder()
	router.ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200: %s", createResponse.Code, createResponse.Body.String())
	}
	sync := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/study/sessions/11111111-1111-1111-1111-111111111111/sync",
		strings.NewReader(`{}`),
	)
	sync.Header.Set("Content-Type", "application/json")
	syncResponse := httptest.NewRecorder()
	router.ServeHTTP(syncResponse, sync)
	if syncResponse.Code != http.StatusOK {
		t.Fatalf("sync status = %d, want 200: %s", syncResponse.Code, syncResponse.Body.String())
	}

	for _, blocked := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/study/sessions/11111111-1111-1111-1111-111111111111", nil),
		httptest.NewRequest(http.MethodPost, "/ideas", strings.NewReader(`{"names":[]}`)),
		httptest.NewRequest(http.MethodGet, "/healthz", nil),
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, blocked)
		assertStudyHTTPError(t, response, http.StatusBadRequest, model.StudyErrorCodeSchemaInvalid)
	}
	if got := service.callCount(); got != 2 {
		t.Fatalf("service calls = %d, want only create and sync", got)
	}
	if got := whoIs.callCount(); got != 5 {
		t.Fatalf("WhoIs calls = %d, want one per request", got)
	}
	for _, remoteAddr := range whoIs.remoteAddresses() {
		if remoteAddr != "192.0.2.1:1234" {
			t.Fatalf("WhoIs remote address = %q, want exact request RemoteAddr", remoteAddr)
		}
	}
}

func TestTailnetStudyRouterDoesNotCacheCapabilityOrTrustSpoofedHeaders(t *testing.T) {
	boundary := newTailnetStudyBoundaryForServerTest(t)
	revoked := validTailnetStudyWhoIsResponse()
	revoked.CapMap = nil
	whoIs := &tailnetStudyWhoIsRecorder{responses: []*apitype.WhoIsResponse{
		validTailnetStudyWhoIsResponse(),
		revoked,
	}}
	service := &fakeStudySessionService{}
	router, err := NewTailnetStudyRouter(service, boundary, whoIs)
	if err != nil {
		t.Fatal(err)
	}

	for index := 0; index < 2; index++ {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/study/sessions", strings.NewReader(`{}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Tailscale-User", "operator")
		request.Header.Set("X-Tailscale-Capability", string(tailnetStudyRouterCapability))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if index == 0 {
			if response.Code != http.StatusOK {
				t.Fatalf("first status = %d, want 200: %s", response.Code, response.Body.String())
			}
			continue
		}
		assertStudyHTTPError(t, response, http.StatusForbidden, model.StudyErrorCodeAuthorizationDenied)
	}
	if got := service.callCount(); got != 1 {
		t.Fatalf("service calls = %d, want 1", got)
	}
	if got := whoIs.callCount(); got != 2 {
		t.Fatalf("WhoIs calls = %d, want 2", got)
	}
}

func TestTailnetStudyRouterNodeModeRejectsBearerBeforeService(t *testing.T) {
	boundary := newTailnetStudyBoundaryForServerTest(t)
	whoIs := &tailnetStudyWhoIsRecorder{responses: []*apitype.WhoIsResponse{validTailnetStudyWhoIsResponse()}}
	service := &fakeStudySessionService{}
	router, err := NewTailnetStudyRouter(service, boundary, whoIs)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/study/sessions", strings.NewReader(`not-json`))
	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set("Authorization", "Bearer forged-operator-token")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	assertStudyHTTPError(t, response, http.StatusForbidden, model.StudyErrorCodeAuthorizationDenied)
	if got := service.callCount(); got != 0 {
		t.Fatalf("service calls = %d, want 0", got)
	}
}

func TestNewTailnetStudyRouterRejectsMissingCapabilities(t *testing.T) {
	boundary := newTailnetStudyBoundaryForServerTest(t)
	service := &fakeStudySessionService{}
	whoIs := &tailnetStudyWhoIsRecorder{responses: []*apitype.WhoIsResponse{validTailnetStudyWhoIsResponse()}}
	for _, testCase := range []struct {
		name     string
		service  TailnetStudySessionService
		boundary *studyauth.Boundary
		whoIs    studyauth.WhoIsClient
	}{
		{name: "service", boundary: boundary, whoIs: whoIs},
		{name: "boundary", service: service, whoIs: whoIs},
		{name: "WhoIs", service: service, boundary: boundary},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			router, err := NewTailnetStudyRouter(testCase.service, testCase.boundary, testCase.whoIs)
			if router != nil || err == nil {
				t.Fatalf("router=%v err=%v, want configuration failure", router, err)
			}
		})
	}
}

func newTailnetStudyBoundaryForServerTest(t *testing.T) *studyauth.Boundary {
	t.Helper()
	boundary, err := studyauth.NewBoundary(studyauth.Policy{
		Mode:               studyauth.AuthenticationModeTailnetNode,
		RequiredNodeTag:    tailnetStudyRouterNodeTag,
		AppCapability:      tailnetStudyRouterCapability,
		AllowedProfileRefs: []string{tailnetStudyRouterProfileRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	return boundary
}

func validTailnetStudyWhoIsResponse() *apitype.WhoIsResponse {
	return &apitype.WhoIsResponse{
		Node: &tailcfg.Node{
			StableID: tailcfg.StableNodeID("nodeid:embedded-house-panel"),
			Tags:     []string{tailnetStudyRouterNodeTag},
		},
		CapMap: tailcfg.PeerCapMap{
			tailnetStudyRouterCapability: {
				tailcfg.RawMessage(`{"operations":["create_or_resume","sync"],"profile_refs":["learner.primary"]}`),
			},
		},
	}
}

type tailnetStudyWhoIsRecorder struct {
	mu        sync.Mutex
	responses []*apitype.WhoIsResponse
	remotes   []string
}

func (recorder *tailnetStudyWhoIsRecorder) WhoIs(_ context.Context, remoteAddr string) (*apitype.WhoIsResponse, error) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.remotes = append(recorder.remotes, remoteAddr)
	if len(recorder.responses) == 0 {
		return nil, nil
	}
	response := recorder.responses[0]
	recorder.responses = recorder.responses[1:]
	return response, nil
}

func (recorder *tailnetStudyWhoIsRecorder) callCount() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return len(recorder.remotes)
}

func (recorder *tailnetStudyWhoIsRecorder) remoteAddresses() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]string(nil), recorder.remotes...)
}
