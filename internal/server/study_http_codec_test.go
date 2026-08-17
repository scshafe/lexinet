package server

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lexinet/internal/model"
	"lexinet/internal/studysession"
)

func TestStrictStudyJSONAcceptsExactBodyLimitAndRejectsOneByteOver(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		bodyLength int
		wantStatus int
		wantCalls  int64
	}{
		{name: "exact limit", bodyLength: studysession.StudyMaximumRequestBodyBytes, wantStatus: http.StatusOK, wantCalls: 1},
		{name: "one byte over", bodyLength: studysession.StudyMaximumRequestBodyBytes + 1, wantStatus: http.StatusRequestEntityTooLarge},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service := &fakeStudySessionService{}
			router := newStudyTestRouter("secret", service)
			body := []byte(`{}` + strings.Repeat(" ", testCase.bodyLength-2))
			request := newAuthorizedStudyRequestBytes(http.MethodPost, "/api/v1/study/sessions", body, "secret")
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

func TestStrictStudyJSONRejectsInvalidTransportAndAmbiguousJSONBeforeService(t *testing.T) {
	deepValue := strings.Repeat("[", studyMaximumJSONNestingDepth+1) + "0" + strings.Repeat("]", studyMaximumJSONNestingDepth+1)
	testCases := []struct {
		name            string
		body            []byte
		contentType     []string
		contentEncoding []string
	}{
		{name: "missing content type", body: []byte(`{}`)},
		{name: "wrong media type", body: []byte(`{}`), contentType: []string{"text/json"}},
		{name: "unsupported media parameter", body: []byte(`{}`), contentType: []string{"application/json; version=1"}},
		{name: "unsupported charset", body: []byte(`{}`), contentType: []string{"application/json; charset=iso-8859-1"}},
		{name: "multiple content types", body: []byte(`{}`), contentType: []string{"application/json", "application/json"}},
		{name: "unsupported content encoding", body: []byte(`{}`), contentType: []string{"application/json"}, contentEncoding: []string{"gzip"}},
		{name: "explicit empty content encoding", body: []byte(`{}`), contentType: []string{"application/json"}, contentEncoding: []string{""}},
		{name: "multiple content encodings", body: []byte(`{}`), contentType: []string{"application/json"}, contentEncoding: []string{"identity", "identity"}},
		{name: "invalid utf8", body: []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}, contentType: []string{"application/json"}},
		{name: "top-level null", body: []byte(`null`), contentType: []string{"application/json"}},
		{name: "top-level scalar", body: []byte(`true`), contentType: []string{"application/json"}},
		{name: "top-level array", body: []byte(`[]`), contentType: []string{"application/json"}},
		{name: "duplicate root member", body: []byte(`{"page":{"after_position":-1,"limit":1},"page":{"after_position":-1,"limit":1}}`), contentType: []string{"application/json"}},
		{name: "duplicate nested member", body: []byte(`{"page":{"after_position":-1,"limit":1,"limit":2}}`), contentType: []string{"application/json"}},
		{name: "duplicate event member", body: []byte(`{"events":[{"client_sequence":1,"client_sequence":1}]}`), contentType: []string{"application/json"}},
		{name: "unknown root member", body: []byte(`{"extra":true}`), contentType: []string{"application/json"}},
		{name: "case variant root member", body: []byte(`{"Page":{"after_position":-1,"limit":1}}`), contentType: []string{"application/json"}},
		{name: "unknown nested member", body: []byte(`{"page":{"after_position":-1,"limit":1,"extra":true}}`), contentType: []string{"application/json"}},
		{name: "unknown event member", body: []byte(`{"events":[{"extra":true}]}`), contentType: []string{"application/json"}},
		{name: "multiple values", body: []byte(`{} {}`), contentType: []string{"application/json"}},
		{name: "trailing token", body: []byte(`{} true`), contentType: []string{"application/json"}},
		{name: "malformed", body: []byte(`{"page":`), contentType: []string{"application/json"}},
		{name: "nesting limit", body: []byte(`{"page":` + deepValue + `}`), contentType: []string{"application/json"}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			service := &fakeStudySessionService{}
			router := newStudyTestRouter("secret", service)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/study/sessions", bytes.NewReader(testCase.body))
			request.Header.Set("Authorization", "Bearer secret")
			for _, value := range testCase.contentType {
				request.Header.Add("Content-Type", value)
			}
			for _, value := range testCase.contentEncoding {
				request.Header.Add("Content-Encoding", value)
			}
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			assertStudyHTTPError(t, response, http.StatusBadRequest, model.StudyErrorCodeSchemaInvalid)
			if got := service.callCount(); got != 0 {
				t.Fatalf("service calls = %d, want 0", got)
			}
		})
	}
}

func TestStrictStudyJSONAcceptsUTF8IdentityAndFullInt64WithoutRounding(t *testing.T) {
	service := &fakeStudySessionService{
		sync: func(_ *http.Request, _ model.StudyPrincipalID, _ string, request studysession.StudySyncRequest) (*studysession.StudySyncResult, error) {
			if request.ExpectedStateVersion != math.MaxInt64 {
				t.Fatalf("expected state version = %d, want MaxInt64", request.ExpectedStateVersion)
			}
			if len(request.Events) != 1 || request.Events[0].ClientSequence != math.MaxInt64 {
				t.Fatalf("events = %#v, want full int64 sequence", request.Events)
			}
			return studySyncResult(), nil
		},
	}
	router := newStudyTestRouter("secret", service)
	body := `{"expected_state_version":9223372036854775807,"events":[{"client_sequence":9223372036854775807}]}`
	request := newAuthorizedStudyRequest(http.MethodPost, "/api/v1/study/sessions/11111111-1111-1111-1111-111111111111/sync", body, "secret")
	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	request.Header.Set("Content-Encoding", "identity")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
}

func TestStudyResponseJSONAllowsExactLimitAndRejectsOverflowBeforeHeaders(t *testing.T) {
	exact := boundedValidStudyJSON(studysession.StudyMaximumResponseBodyBytes)
	for _, testCase := range []struct {
		name       string
		response   json.RawMessage
		wantStatus int
		wantCode   model.StudyErrorCode
	}{
		{name: "exact limit", response: exact, wantStatus: http.StatusOK},
		{name: "one byte over", response: append(append(json.RawMessage(nil), exact...), ' '), wantStatus: http.StatusInternalServerError, wantCode: model.StudyErrorCodeInternal},
		{name: "invalid JSON", response: json.RawMessage(`{"secret":"response-canary"`), wantStatus: http.StatusInternalServerError, wantCode: model.StudyErrorCodeInternal},
		{name: "invalid UTF-8", response: json.RawMessage{'{', '"', 'x', '"', ':', '"', 'r', 'e', 's', 'p', 'o', 'n', 's', 'e', '-', 'c', 'a', 'n', 'a', 'r', 'y', '-', 0xff, '"', '}'}, wantStatus: http.StatusInternalServerError, wantCode: model.StudyErrorCodeInternal},
		{name: "empty JSON", response: nil, wantStatus: http.StatusInternalServerError, wantCode: model.StudyErrorCodeInternal},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service := &fakeStudySessionService{
				get: func(_ *http.Request, _ model.StudyPrincipalID, _ string, _ studysession.StudyPageRequest) (*studysession.StudyGetSessionResult, error) {
					return &studysession.StudyGetSessionResult{ResponseJSON: testCase.response}, nil
				},
			}
			router := newStudyTestRouter("secret", service)
			request := newAuthorizedStudyRequest(http.MethodGet, "/api/v1/study/sessions/11111111-1111-1111-1111-111111111111", "", "secret")
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			if response.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, testCase.wantStatus)
			}
			if testCase.wantCode != "" {
				assertStudyHTTPError(t, response, testCase.wantStatus, testCase.wantCode)
				if strings.Contains(response.Body.String(), "response-canary") {
					t.Fatal("response leaked rejected service bytes")
				}
			} else if response.Body.Len() != studysession.StudyMaximumResponseBodyBytes {
				t.Fatalf("response bytes = %d, want %d", response.Body.Len(), studysession.StudyMaximumResponseBodyBytes)
			}
		})
	}
}

func boundedValidStudyJSON(totalBytes int) json.RawMessage {
	prefix := `{"data":"`
	suffix := `"}`
	return json.RawMessage(prefix + strings.Repeat("x", totalBytes-len(prefix)-len(suffix)) + suffix)
}
