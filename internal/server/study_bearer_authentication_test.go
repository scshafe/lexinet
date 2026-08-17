package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lexinet/internal/model"
	"lexinet/internal/studysession"
)

func TestStudyBearerAuthenticationFailsClosedBeforeDecodeAndService(t *testing.T) {
	testCases := []struct {
		name            string
		configuredToken string
		authorization   []string
	}{
		{name: "empty configured token", configuredToken: "", authorization: []string{"Bearer "}},
		{name: "whitespace configured token", configuredToken: " \t ", authorization: []string{"Bearer  \t "}},
		{name: "missing header", configuredToken: "secret"},
		{name: "empty header", configuredToken: "secret", authorization: []string{""}},
		{name: "wrong token", configuredToken: "secret", authorization: []string{"Bearer wrong"}},
		{name: "wrong scheme", configuredToken: "secret", authorization: []string{"bearer secret"}},
		{name: "missing bearer space", configuredToken: "secret", authorization: []string{"Bearersecret"}},
		{name: "multiple headers", configuredToken: "secret", authorization: []string{"Bearer secret", "Bearer secret"}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			service := &fakeStudySessionService{}
			router := Router(
				Config{AuthToken: testCase.configuredToken},
				fakeStore{},
				WithStudySessionService(service),
			)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/study/sessions", strings.NewReader(`not-json`))
			request.Header.Set("Content-Type", "text/plain")
			for _, authorization := range testCase.authorization {
				request.Header.Add("Authorization", authorization)
			}
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			assertStudyHTTPError(t, response, http.StatusUnauthorized, model.StudyErrorCodeUnauthorized)
			if got := service.callCount(); got != 0 {
				t.Fatalf("service calls = %d, want 0", got)
			}
		})
	}
}

func TestStudyBearerAuthenticationInjectsOnlyOperatorPrincipal(t *testing.T) {
	service := &fakeStudySessionService{
		createOrResume: func(_ *http.Request, principal model.StudyPrincipalID, _ studysession.StudyCreateSessionRequest) (*studysession.StudyCreateSessionResult, error) {
			if principal.String() != model.OperatorStudyPrincipalID().String() {
				t.Fatalf("principal = %q, want operator", principal.String())
			}
			return studyCreateResult(false), nil
		},
	}
	router := newStudyTestRouter("secret", service)
	request := newAuthorizedStudyRequest(http.MethodPost, "/api/v1/study/sessions", `{}`, "secret")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if got := service.callCount(); got != 1 {
		t.Fatalf("service calls = %d, want 1", got)
	}
}
