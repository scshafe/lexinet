package server

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"lexinet/internal/model"
	"lexinet/internal/studyauth"
)

// NewTailnetStudyRouter is the @canonical tailnet_study_http_boundary. Its
// constructor has no vocabulary Store capability, and its route table exposes
// only create/resume and sync. Every request performs fresh native WhoIs
// authentication before routing or body decoding.
func NewTailnetStudyRouter(
	service TailnetStudySessionService,
	boundary *studyauth.Boundary,
	whoIsClient studyauth.WhoIsClient,
) (http.Handler, error) {
	if service == nil || boundary == nil || whoIsClient == nil {
		return nil, &ConfigurationError{
			Code:    "CONFIG.TAILNET_STUDY_ROUTER_INVALID",
			Message: "Tailnet study router dependencies are unavailable",
			Context: map[string]any{"configuration_field": "tailnet_study_router"},
		}
	}

	router := chi.NewRouter()
	router.Use(assignStudyRequestID(randomStudyRequestID))
	router.Use(recoverStudyPanic)
	router.Use(requireTailnetStudyAuthentication(boundary, whoIsClient))
	router.Route("/api/v1/study", func(studyRouter chi.Router) {
		studyRouter.Post("/sessions", createOrResumeStudySessionHandler(service))
		studyRouter.Post("/sessions/{session_id}/sync", syncStudySessionHandler(service))
		studyRouter.NotFound(writeTailnetStudyRouteError)
		studyRouter.MethodNotAllowed(writeTailnetStudyRouteError)
	})
	router.NotFound(writeTailnetStudyRouteError)
	router.MethodNotAllowed(writeTailnetStudyRouteError)
	return router, nil
}

func requireTailnetStudyAuthentication(
	boundary *studyauth.Boundary,
	whoIsClient studyauth.WhoIsClient,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			authenticatedContext, principal, err := boundary.Authenticate(
				request.Context(),
				request.RemoteAddr,
				whoIsClient,
				request.Header.Values("Authorization"),
			)
			if err != nil {
				writeStudyError(w, err)
				return
			}
			contextPrincipal, ok := boundary.PrincipalFromContext(authenticatedContext)
			if !ok || contextPrincipal != principal || !principal.Valid() {
				writeStudyError(w, studyTransportError(model.StudyErrorCodeInternal))
				return
			}

			ctx := context.WithValue(authenticatedContext, studyPrincipalContextKey{}, principal)
			detached := request.Clone(ctx)
			detached.Header = request.Header.Clone()
			detached.Header.Del("Authorization")
			next.ServeHTTP(w, detached)
		})
	}
}

func writeTailnetStudyRouteError(w http.ResponseWriter, _ *http.Request) {
	writeStudyError(w, studyTransportError(model.StudyErrorCodeSchemaInvalid))
}
