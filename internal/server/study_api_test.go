package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"lexinet/internal/model"
	"lexinet/internal/studysession"
)

func TestStudyHandlersReturn200And201AndCallExactlyOneServiceMethod(t *testing.T) {
	sessionID := "11111111-1111-1111-1111-111111111111"
	testCases := []struct {
		name       string
		method     string
		target     string
		body       string
		service    *fakeStudySessionService
		wantStatus int
	}{
		{name: "create", method: http.MethodPost, target: "/api/v1/study/sessions", body: `{}`, service: &fakeStudySessionService{createResult: studyCreateResult(true)}, wantStatus: http.StatusCreated},
		{name: "resume", method: http.MethodPost, target: "/api/v1/study/sessions", body: `{}`, service: &fakeStudySessionService{createResult: studyCreateResult(false)}, wantStatus: http.StatusOK},
		{name: "get", method: http.MethodGet, target: "/api/v1/study/sessions/" + sessionID, service: &fakeStudySessionService{getResult: studyGetResult()}, wantStatus: http.StatusOK},
		{name: "sync", method: http.MethodPost, target: "/api/v1/study/sessions/" + sessionID + "/sync", body: `{}`, service: &fakeStudySessionService{syncResult: studySyncResult()}, wantStatus: http.StatusOK},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			router := newStudyTestRouter("secret", testCase.service)
			request := newAuthorizedStudyRequest(testCase.method, testCase.target, testCase.body, "secret")
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			if response.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, testCase.wantStatus, response.Body.String())
			}
			if got := testCase.service.callCount(); got != 1 {
				t.Fatalf("service calls = %d, want 1", got)
			}
			if got := response.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("content type = %q, want application/json", got)
			}
		})
	}
}

func TestStudyHandlerRejectsMissingTypedPrincipalWithoutServiceWork(t *testing.T) {
	service := &fakeStudySessionService{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/study/sessions", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	createOrResumeStudySessionHandler(service).ServeHTTP(response, request)

	assertStudyHTTPError(t, response, http.StatusUnauthorized, model.StudyErrorCodeUnauthorized)
	if got := service.callCount(); got != 0 {
		t.Fatalf("service calls = %d, want 0", got)
	}
}

func TestStudyGetQueryDefaultsAndStrictOverrides(t *testing.T) {
	testCases := []struct {
		name       string
		query      string
		wantPage   studysession.StudyPageRequest
		wantStatus int
		wantCalls  int64
	}{
		{name: "defaults", wantPage: studysession.StudyPageRequest{AfterPosition: -1, Limit: 16}, wantStatus: http.StatusOK, wantCalls: 1},
		{name: "overrides", query: "?page_after_position=2147483647&page_limit=64", wantPage: studysession.StudyPageRequest{AfterPosition: 2147483647, Limit: 64}, wantStatus: http.StatusOK, wantCalls: 1},
		{name: "unknown", query: "?limit=1", wantStatus: http.StatusBadRequest},
		{name: "duplicate", query: "?page_limit=1&page_limit=1", wantStatus: http.StatusBadRequest},
		{name: "empty", query: "?page_limit=", wantStatus: http.StatusBadRequest},
		{name: "after too low", query: "?page_after_position=-2", wantStatus: http.StatusBadRequest},
		{name: "after too high", query: "?page_after_position=2147483648", wantStatus: http.StatusBadRequest},
		{name: "limit zero", query: "?page_limit=0", wantStatus: http.StatusBadRequest},
		{name: "limit high", query: "?page_limit=65", wantStatus: http.StatusBadRequest},
		{name: "malformed encoding", query: "?page_limit=%zz", wantStatus: http.StatusBadRequest},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			service := &fakeStudySessionService{
				get: func(_ *http.Request, _ model.StudyPrincipalID, _ string, page studysession.StudyPageRequest) (*studysession.StudyGetSessionResult, error) {
					if page != testCase.wantPage {
						t.Fatalf("page = %#v, want %#v", page, testCase.wantPage)
					}
					return studyGetResult(), nil
				},
			}
			router := newStudyTestRouter("secret", service)
			request := newAuthorizedStudyRequest(http.MethodGet, "/api/v1/study/sessions/11111111-1111-1111-1111-111111111111"+testCase.query, "", "secret")
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			if response.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, testCase.wantStatus, response.Body.String())
			}
			if got := service.callCount(); got != testCase.wantCalls {
				t.Fatalf("service calls = %d, want %d", got, testCase.wantCalls)
			}
		})
	}
}

func TestStudyHTTPErrorMapIsClosedAndRedacted(t *testing.T) {
	testCases := []struct {
		code       model.StudyErrorCode
		wantStatus int
		wantCode   model.StudyErrorCode
	}{
		{model.StudyErrorCodeSchemaInvalid, http.StatusBadRequest, model.StudyErrorCodeSchemaInvalid},
		{model.StudyErrorCodeUnauthorized, http.StatusUnauthorized, model.StudyErrorCodeUnauthorized},
		{model.StudyErrorCodeAuthorizationDenied, http.StatusForbidden, model.StudyErrorCodeAuthorizationDenied},
		{model.StudyErrorCodeProfileNotFound, http.StatusNotFound, model.StudyErrorCodeProfileNotFound},
		{model.StudyErrorCodeSessionNotFound, http.StatusNotFound, model.StudyErrorCodeSessionNotFound},
		{model.StudyErrorCodeIdempotencyConflict, http.StatusConflict, model.StudyErrorCodeIdempotencyConflict},
		{model.StudyErrorCodeCycleNotExhausted, http.StatusConflict, model.StudyErrorCodeCycleNotExhausted},
		{model.StudyErrorCodeFlushReceiptRequired, http.StatusConflict, model.StudyErrorCodeFlushReceiptRequired},
		{model.StudyErrorCodeFlushReceiptStale, http.StatusConflict, model.StudyErrorCodeFlushReceiptStale},
		{model.StudyErrorCodeStateStale, http.StatusConflict, model.StudyErrorCodeStateStale},
		{model.StudyErrorCodeStreamSequenceGap, http.StatusConflict, model.StudyErrorCodeStreamSequenceGap},
		{model.StudyErrorCodeRequestTooLarge, http.StatusRequestEntityTooLarge, model.StudyErrorCodeRequestTooLarge},
		{model.StudyErrorCodeRateLimited, http.StatusTooManyRequests, model.StudyErrorCodeRateLimited},
		{model.StudyErrorCodeServiceUnavailable, http.StatusServiceUnavailable, model.StudyErrorCodeServiceUnavailable},
		{model.StudyErrorCodeInternal, http.StatusInternalServerError, model.StudyErrorCodeInternal},
		{model.StudyErrorCode("UNKNOWN_CANARY_CODE"), http.StatusInternalServerError, model.StudyErrorCodeInternal},
	}

	for _, testCase := range testCases {
		t.Run(string(testCase.code), func(t *testing.T) {
			const canary = "credential-profile-url-card-content-canary"
			service := &fakeStudySessionService{
				createOrResume: func(_ *http.Request, _ model.StudyPrincipalID, _ studysession.StudyCreateSessionRequest) (*studysession.StudyCreateSessionResult, error) {
					return nil, &model.StudyError{
						Code:      testCase.code,
						Message:   canary,
						Context:   map[string]string{"secret": canary},
						Retryable: true,
						Cause:     errors.New(canary),
					}
				},
			}
			router := newStudyTestRouter("secret", service)
			request := newAuthorizedStudyRequest(http.MethodPost, "/api/v1/study/sessions", `{}`, "secret")
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			assertStudyHTTPError(t, response, testCase.wantStatus, testCase.wantCode)
			if strings.Contains(response.Body.String(), canary) || strings.Contains(response.Body.String(), "UNKNOWN_CANARY_CODE") {
				t.Fatalf("error response leaked non-allowlisted data: %s", response.Body.String())
			}
			wantRetryAfter := ""
			if testCase.wantCode == model.StudyErrorCodeRateLimited || testCase.wantCode == model.StudyErrorCodeServiceUnavailable {
				wantRetryAfter = "1"
			}
			if got := response.Header().Get("Retry-After"); got != wantRetryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, wantRetryAfter)
			}
		})
	}
}

func TestStudyPanicIsSanitizedAndCarriesGeneratedCorrelation(t *testing.T) {
	const incomingRequestID = "attacker-controlled-request-id"
	service := &fakeStudySessionService{
		createOrResume: func(_ *http.Request, _ model.StudyPrincipalID, _ studysession.StudyCreateSessionRequest) (*studysession.StudyCreateSessionResult, error) {
			panic("panic-secret-canary")
		},
	}
	router := newStudyTestRouter("secret", service)
	request := newAuthorizedStudyRequest(http.MethodPost, "/api/v1/study/sessions", `{}`, "secret")
	request.Header.Set("X-Request-ID", incomingRequestID)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	assertStudyHTTPError(t, response, http.StatusInternalServerError, model.StudyErrorCodeInternal)
	requestID := response.Header().Get("X-Request-ID")
	if requestID == "" || len(requestID) > 128 || requestID == incomingRequestID {
		t.Fatalf("response request ID = %q, want bounded server-generated value", requestID)
	}
	if _, err := uuid.Parse(requestID); err != nil {
		t.Fatalf("response request ID is not UUID: %v", err)
	}
	if strings.Contains(response.Body.String(), "panic-secret-canary") {
		t.Fatal("panic value leaked")
	}
}

func TestStudyRequestCorrelationIsGeneratedBeforeAuthentication(t *testing.T) {
	router := newStudyTestRouter("secret", &fakeStudySessionService{})
	requestIDs := make(map[string]struct{}, 2)
	for index := 0; index < 2; index++ {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/study/sessions", strings.NewReader(`{}`))
		request.Header.Set("X-Request-ID", "incoming")
		response := httptest.NewRecorder()

		router.ServeHTTP(response, request)

		assertStudyHTTPError(t, response, http.StatusUnauthorized, model.StudyErrorCodeUnauthorized)
		requestID := response.Header().Get("X-Request-ID")
		if requestID == "" || requestID == "incoming" {
			t.Fatalf("response request ID = %q, want generated identity", requestID)
		}
		if _, duplicate := requestIDs[requestID]; duplicate {
			t.Fatalf("duplicate generated request ID %q", requestID)
		}
		requestIDs[requestID] = struct{}{}
	}
}

func TestStudyRequestCorrelationGenerationFailureUsesBoundedSanitizedFallback(t *testing.T) {
	service := &fakeStudySessionService{}
	router := Router(
		Config{AuthToken: "secret"},
		fakeStore{},
		WithStudySessionService(service),
		withStudyRequestIDGenerator(func() (string, error) {
			return "request-id-secret-canary", errors.New("request-id-generation-secret-canary")
		}),
	)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/study/sessions", strings.NewReader(`{}`))
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	assertStudyHTTPError(t, response, http.StatusUnauthorized, model.StudyErrorCodeUnauthorized)
	requestID := response.Header().Get("X-Request-ID")
	if !strings.HasPrefix(requestID, "study-fallback-") || len(requestID) > 128 {
		t.Fatalf("fallback request ID = %q, want bounded explicit fallback", requestID)
	}
	if strings.Contains(response.Body.String(), "canary") || strings.Contains(requestID, "canary") {
		t.Fatal("request ID generation failure leaked")
	}
	if got := service.callCount(); got != 0 {
		t.Fatalf("service calls = %d, want 0", got)
	}
}

func TestStudyNamespaceFallbacksRetainAuthenticationAndClosedEnvelope(t *testing.T) {
	testCases := []struct {
		name          string
		method        string
		target        string
		authorization string
		wantStatus    int
		wantCode      model.StudyErrorCode
	}{
		{name: "unauthenticated unknown path", method: http.MethodGet, target: "/api/v1/study/unknown", wantStatus: http.StatusUnauthorized, wantCode: model.StudyErrorCodeUnauthorized},
		{name: "unauthenticated wrong method", method: http.MethodDelete, target: "/api/v1/study/sessions", wantStatus: http.StatusUnauthorized, wantCode: model.StudyErrorCodeUnauthorized},
		{name: "authenticated unknown path", method: http.MethodGet, target: "/api/v1/study/unknown", authorization: "Bearer secret", wantStatus: http.StatusBadRequest, wantCode: model.StudyErrorCodeSchemaInvalid},
		{name: "authenticated wrong method", method: http.MethodDelete, target: "/api/v1/study/sessions", authorization: "Bearer secret", wantStatus: http.StatusBadRequest, wantCode: model.StudyErrorCodeSchemaInvalid},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			service := &fakeStudySessionService{}
			router := newStudyTestRouter("secret", service)
			request := httptest.NewRequest(testCase.method, testCase.target, nil)
			if testCase.authorization != "" {
				request.Header.Set("Authorization", testCase.authorization)
			}
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			assertStudyHTTPError(t, response, testCase.wantStatus, testCase.wantCode)
			if requestID := response.Header().Get("X-Request-ID"); requestID == "" || len(requestID) > 128 {
				t.Fatalf("X-Request-ID = %q, want bounded generated identity", requestID)
			}
			if got := service.callCount(); got != 0 {
				t.Fatalf("service calls = %d, want 0", got)
			}
		})
	}
}

func TestHTTPServerUsesFiniteReviewedBudgets(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	server := NewHTTPServer(handler)

	if server.Handler == nil || server.ReadHeaderTimeout != 5*time.Second || server.ReadTimeout != 30*time.Second ||
		server.WriteTimeout != 60*time.Second || server.IdleTimeout != 120*time.Second || server.MaxHeaderBytes != 64*1024 {
		t.Fatalf("unexpected HTTP server budgets: %#v", server)
	}
}

type fakeStudySessionService struct {
	createOrResume func(*http.Request, model.StudyPrincipalID, studysession.StudyCreateSessionRequest) (*studysession.StudyCreateSessionResult, error)
	get            func(*http.Request, model.StudyPrincipalID, string, studysession.StudyPageRequest) (*studysession.StudyGetSessionResult, error)
	sync           func(*http.Request, model.StudyPrincipalID, string, studysession.StudySyncRequest) (*studysession.StudySyncResult, error)
	createResult   *studysession.StudyCreateSessionResult
	getResult      *studysession.StudyGetSessionResult
	syncResult     *studysession.StudySyncResult
	calls          atomic.Int64
}

func (service *fakeStudySessionService) CreateOrResume(ctx context.Context, principal model.StudyPrincipalID, request studysession.StudyCreateSessionRequest) (*studysession.StudyCreateSessionResult, error) {
	service.calls.Add(1)
	if service.createOrResume != nil {
		return service.createOrResume(requestWithContext(ctx), principal, request)
	}
	if service.createResult != nil {
		return service.createResult, nil
	}
	return studyCreateResult(false), nil
}

func (service *fakeStudySessionService) Get(ctx context.Context, principal model.StudyPrincipalID, sessionID uuid.UUID, page studysession.StudyPageRequest) (*studysession.StudyGetSessionResult, error) {
	service.calls.Add(1)
	if service.get != nil {
		return service.get(requestWithContext(ctx), principal, sessionID.String(), page)
	}
	if service.getResult != nil {
		return service.getResult, nil
	}
	return studyGetResult(), nil
}

func (service *fakeStudySessionService) Sync(ctx context.Context, principal model.StudyPrincipalID, sessionID uuid.UUID, request studysession.StudySyncRequest) (*studysession.StudySyncResult, error) {
	service.calls.Add(1)
	if service.sync != nil {
		return service.sync(requestWithContext(ctx), principal, sessionID.String(), request)
	}
	if service.syncResult != nil {
		return service.syncResult, nil
	}
	return studySyncResult(), nil
}

func (service *fakeStudySessionService) callCount() int64 {
	return service.calls.Load()
}

func requestWithContext(ctx context.Context) *http.Request {
	return (&http.Request{}).WithContext(ctx)
}

func newStudyTestRouter(token string, service StudySessionService) http.Handler {
	return Router(Config{AuthToken: token}, fakeStore{}, WithStudySessionService(service))
}

func newAuthorizedStudyRequest(method, target, body, token string) *http.Request {
	return newAuthorizedStudyRequestBytes(method, target, []byte(body), token)
}

func newAuthorizedStudyRequestBytes(method, target string, body []byte, token string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer "+token)
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func studyCreateResult(created bool) *studysession.StudyCreateSessionResult {
	return &studysession.StudyCreateSessionResult{ResponseJSON: json.RawMessage(`{"protocol_version":"study.v1"}`), Created: created}
}

func studyGetResult() *studysession.StudyGetSessionResult {
	return &studysession.StudyGetSessionResult{ResponseJSON: json.RawMessage(`{"protocol_version":"study.v1"}`)}
}

func studySyncResult() *studysession.StudySyncResult {
	return &studysession.StudySyncResult{ResponseJSON: json.RawMessage(`{"protocol_version":"study.v1"}`)}
}

func assertStudyHTTPError(t *testing.T, response *httptest.ResponseRecorder, wantStatus int, wantCode model.StudyErrorCode) {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("status = %d, want %d: %s", response.Code, wantStatus, response.Body.String())
	}
	var envelope studyHTTPErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v: %s", err, response.Body.String())
	}
	if envelope.Error.ProtocolVersion != model.StudyProtocolVersion || envelope.Error.SchemaVersion != model.StudySchemaVersion ||
		envelope.Error.Code != wantCode || envelope.Error.Message == "" {
		t.Fatalf("unexpected error envelope: %#v", envelope)
	}
}
