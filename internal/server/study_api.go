package server

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"lexinet/internal/model"
	"lexinet/internal/studysession"
)

type studyCreateOrResumeService interface {
	CreateOrResume(context.Context, model.StudyPrincipalID, studysession.StudyCreateSessionRequest) (*studysession.StudyCreateSessionResult, error)
}

type studyGetService interface {
	Get(context.Context, model.StudyPrincipalID, uuid.UUID, studysession.StudyPageRequest) (*studysession.StudyGetSessionResult, error)
}

type studySyncService interface {
	Sync(context.Context, model.StudyPrincipalID, uuid.UUID, studysession.StudySyncRequest) (*studysession.StudySyncResult, error)
}

// StudySessionService is the complete loopback HTTP-facing study capability.
// Handlers call exactly one method and never receive repository or transaction
// access.
type StudySessionService interface {
	studyCreateOrResumeService
	studyGetService
	studySyncService
}

// TailnetStudySessionService is the structurally narrower appliance surface.
// It intentionally omits operator-only GET/state access.
type TailnetStudySessionService interface {
	studyCreateOrResumeService
	studySyncService
}

func registerStudyAPIRoutes(router chi.Router, service StudySessionService) {
	router.Post("/sessions", createOrResumeStudySessionHandler(service))
	router.Get("/sessions/{session_id}", getStudySessionHandler(service))
	router.Post("/sessions/{session_id}/sync", syncStudySessionHandler(service))
	router.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeStudyError(w, studyTransportError(model.StudyErrorCodeSchemaInvalid))
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeStudyError(w, studyTransportError(model.StudyErrorCodeSchemaInvalid))
	})
}

func createOrResumeStudySessionHandler(service studyCreateOrResumeService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := studyPrincipalFromContext(r.Context())
		if !ok {
			writeStudyError(w, studyTransportError(model.StudyErrorCodeUnauthorized))
			return
		}
		var request studysession.StudyCreateSessionRequest
		if err := decodeStrictStudyJSON(r, &request); err != nil {
			writeStudyError(w, err)
			return
		}
		result, err := service.CreateOrResume(r.Context(), principal, request)
		if err != nil {
			writeStudyError(w, err)
			return
		}
		if result == nil {
			writeStudyError(w, studyTransportError(model.StudyErrorCodeInternal))
			return
		}
		status := http.StatusOK
		if result.Created {
			status = http.StatusCreated
		}
		writeStudyResponseJSON(w, status, result.ResponseJSON)
	}
}

func getStudySessionHandler(service studyGetService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := studyPrincipalFromContext(r.Context())
		if !ok {
			writeStudyError(w, studyTransportError(model.StudyErrorCodeUnauthorized))
			return
		}
		sessionID, err := parseStudySessionID(chi.URLParam(r, "session_id"))
		if err != nil {
			writeStudyError(w, err)
			return
		}
		page, err := decodeStudyPageQuery(r.URL.RawQuery)
		if err != nil {
			writeStudyError(w, err)
			return
		}
		result, err := service.Get(r.Context(), principal, sessionID, page)
		if err != nil {
			writeStudyError(w, err)
			return
		}
		if result == nil {
			writeStudyError(w, studyTransportError(model.StudyErrorCodeInternal))
			return
		}
		writeStudyResponseJSON(w, http.StatusOK, result.ResponseJSON)
	}
}

func syncStudySessionHandler(service studySyncService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := studyPrincipalFromContext(r.Context())
		if !ok {
			writeStudyError(w, studyTransportError(model.StudyErrorCodeUnauthorized))
			return
		}
		sessionID, err := parseStudySessionID(chi.URLParam(r, "session_id"))
		if err != nil {
			writeStudyError(w, err)
			return
		}
		var request studysession.StudySyncRequest
		if err := decodeStrictStudyJSON(r, &request); err != nil {
			writeStudyError(w, err)
			return
		}
		result, err := service.Sync(r.Context(), principal, sessionID, request)
		if err != nil {
			writeStudyError(w, err)
			return
		}
		if result == nil {
			writeStudyError(w, studyTransportError(model.StudyErrorCodeInternal))
			return
		}
		writeStudyResponseJSON(w, http.StatusOK, result.ResponseJSON)
	}
}
