package studysession

import (
	"context"

	"lexinet/internal/model"
)

// ScopedStudyAuthorizationGrant is one non-operator principal's complete
// study capability and learner-profile allowlist. Credential material never
// enters this policy boundary.
type ScopedStudyAuthorizationGrant struct {
	Principal                 model.StudyPrincipalID
	AllowedOperations         []StudyOperation
	AllowedLearnerProfileRefs []string
}

type scopedStudyAuthorizationPermissions struct {
	allowedOperations         map[StudyOperation]struct{}
	allowedLearnerProfileRefs map[string]struct{}
}

// ScopedStudyAuthorizer is the @canonical scoped_study_authorizer. It composes
// the existing all-profile operator policy with immutable, bounded grants for
// non-operator principals. Capability authorization checks only the configured
// operation; resolved-target authorization additionally checks exact learner-
// profile membership.
type ScopedStudyAuthorizer struct {
	operatorAuthorizer OperatorStudyAuthorizer
	grantsByPrincipal  map[model.StudyPrincipalID]scopedStudyAuthorizationPermissions
}

var _ StudyAuthorizer = (*ScopedStudyAuthorizer)(nil)

// NewScopedStudyAuthorizer validates and detaches all non-operator grants.
// Invalid, duplicate, empty, or overbound configuration returns a structured
// STUDY_SCHEMA_INVALID error whose context contains no supplied values.
func NewScopedStudyAuthorizer(grants []ScopedStudyAuthorizationGrant) (*ScopedStudyAuthorizer, error) {
	if len(grants) == 0 || len(grants) > StudyMaximumScopedAuthorizationGrants {
		return nil, newScopedStudyAuthorizationConfigurationError()
	}

	grantsByPrincipal := make(map[model.StudyPrincipalID]scopedStudyAuthorizationPermissions, len(grants))
	operatorPrincipal := model.OperatorStudyPrincipalID()
	for _, grant := range grants {
		if !grant.Principal.Valid() || grant.Principal == operatorPrincipal {
			return nil, newScopedStudyAuthorizationConfigurationError()
		}
		principalKey := grant.Principal
		if _, duplicate := grantsByPrincipal[principalKey]; duplicate {
			return nil, newScopedStudyAuthorizationConfigurationError()
		}
		if len(grant.AllowedOperations) == 0 ||
			len(grant.AllowedOperations) > StudyMaximumScopedAuthorizationOperationsPerGrant {
			return nil, newScopedStudyAuthorizationConfigurationError()
		}
		if len(grant.AllowedLearnerProfileRefs) == 0 ||
			len(grant.AllowedLearnerProfileRefs) > StudyMaximumScopedAuthorizationProfileRefsPerGrant {
			return nil, newScopedStudyAuthorizationConfigurationError()
		}

		allowedOperations := make(map[StudyOperation]struct{}, len(grant.AllowedOperations))
		for _, operation := range grant.AllowedOperations {
			if !operation.Valid() {
				return nil, newScopedStudyAuthorizationConfigurationError()
			}
			if _, duplicate := allowedOperations[operation]; duplicate {
				return nil, newScopedStudyAuthorizationConfigurationError()
			}
			allowedOperations[operation] = struct{}{}
		}

		allowedLearnerProfileRefs := make(map[string]struct{}, len(grant.AllowedLearnerProfileRefs))
		for _, profileRef := range grant.AllowedLearnerProfileRefs {
			if !ValidStudyProfileRef(profileRef) {
				return nil, newScopedStudyAuthorizationConfigurationError()
			}
			if _, duplicate := allowedLearnerProfileRefs[profileRef]; duplicate {
				return nil, newScopedStudyAuthorizationConfigurationError()
			}
			allowedLearnerProfileRefs[profileRef] = struct{}{}
		}

		grantsByPrincipal[principalKey] = scopedStudyAuthorizationPermissions{
			allowedOperations:         allowedOperations,
			allowedLearnerProfileRefs: allowedLearnerProfileRefs,
		}
	}

	return &ScopedStudyAuthorizer{
		operatorAuthorizer: OperatorStudyAuthorizer{},
		grantsByPrincipal:  grantsByPrincipal,
	}, nil
}

// AuthorizeStudy preserves operator authority and fails closed for every
// unconfigured principal, operation, stage, or resolved learner profile.
func (authorizer *ScopedStudyAuthorizer) AuthorizeStudy(ctx context.Context, request StudyAuthorizationRequest) error {
	if authorizer == nil || authorizer.grantsByPrincipal == nil {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	if !request.Principal.Valid() {
		return newStudySessionError(model.StudyErrorCodeUnauthorized)
	}
	if request.Principal == model.OperatorStudyPrincipalID() {
		return authorizer.operatorAuthorizer.AuthorizeStudy(ctx, request)
	}
	if !request.Operation.Valid() || !request.Stage.valid() {
		return newStudySessionError(model.StudyErrorCodeAuthorizationDenied)
	}

	permissions, configured := authorizer.grantsByPrincipal[request.Principal]
	if !configured {
		return newStudySessionError(model.StudyErrorCodeAuthorizationDenied)
	}
	if _, allowed := permissions.allowedOperations[request.Operation]; !allowed {
		return newStudySessionError(model.StudyErrorCodeAuthorizationDenied)
	}
	if request.Stage == StudyAuthorizationStageCapability {
		return nil
	}
	if _, allowed := permissions.allowedLearnerProfileRefs[request.ProfileRef]; !allowed {
		return newStudySessionError(model.StudyErrorCodeAuthorizationDenied)
	}
	return nil
}

func newScopedStudyAuthorizationConfigurationError() *model.StudyError {
	err := newStudySessionError(model.StudyErrorCodeSchemaInvalid)
	err.Context = map[string]string{
		"operation": studyScopedAuthorizationConfigurationOperation,
		"field":     studyScopedAuthorizationConfigurationField,
	}
	return err
}
