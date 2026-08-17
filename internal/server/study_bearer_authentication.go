package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"lexinet/internal/model"
)

type studyPrincipalContextKey struct{}

// requireStudyBearerAuthentication is the dedicated study.v1 authentication
// boundary. Unlike legacy loopback authentication, it always fails closed when
// the configured credential is empty and never passes credential bytes inward.
func requireStudyBearerAuthentication(configuredToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authorizationValues := r.Header.Values("Authorization")
			if strings.TrimSpace(configuredToken) == "" || len(authorizationValues) != 1 ||
				!constantTimeStudyBearerMatch(authorizationValues[0], configuredToken) {
				writeStudyError(w, &model.StudyError{Code: model.StudyErrorCodeUnauthorized})
				return
			}

			principal := model.OperatorStudyPrincipalID()
			ctx := context.WithValue(r.Context(), studyPrincipalContextKey{}, principal)
			detached := r.Clone(ctx)
			detached.Header = r.Header.Clone()
			detached.Header.Del("Authorization")
			next.ServeHTTP(w, detached)
		})
	}
}

func constantTimeStudyBearerMatch(actualAuthorization, configuredToken string) bool {
	expectedAuthorization := "Bearer " + configuredToken
	actualSHA256 := sha256.Sum256([]byte(actualAuthorization))
	expectedSHA256 := sha256.Sum256([]byte(expectedAuthorization))
	return subtle.ConstantTimeCompare(actualSHA256[:], expectedSHA256[:]) == 1 &&
		subtle.ConstantTimeEq(int32(len(actualAuthorization)), int32(len(expectedAuthorization))) == 1
}

func studyPrincipalFromContext(ctx context.Context) (model.StudyPrincipalID, bool) {
	if ctx == nil {
		return model.StudyPrincipalID{}, false
	}
	principal, ok := ctx.Value(studyPrincipalContextKey{}).(model.StudyPrincipalID)
	return principal, ok && principal.Valid()
}
