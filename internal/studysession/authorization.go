package studysession

import (
	"context"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

// StudyOperation identifies one complete service capability.
type StudyOperation string

const (
	StudyOperationCreateOrResume StudyOperation = "create_or_resume"
	StudyOperationGet            StudyOperation = "get"
	StudyOperationSync           StudyOperation = "sync"
)

// Valid is the @canonical study_operation_validation entry point. It reports
// whether operation is one exact frozen study.v1 capability.
func (operation StudyOperation) Valid() bool {
	switch operation {
	case StudyOperationCreateOrResume, StudyOperationGet, StudyOperationSync:
		return true
	default:
		return false
	}
}

// StudyAuthorizationStage makes the two-stage opaque-session policy explicit:
// capability authorization precedes all repository work; resolved-target
// authorization follows only a narrow profile-ownership lookup and precedes
// every item/content read.
type StudyAuthorizationStage string

const (
	StudyAuthorizationStageCapability     StudyAuthorizationStage = "capability"
	StudyAuthorizationStageResolvedTarget StudyAuthorizationStage = "resolved_target"
)

func (stage StudyAuthorizationStage) valid() bool {
	return stage == StudyAuthorizationStageCapability || stage == StudyAuthorizationStageResolvedTarget
}

// StudyAuthorizationRequest contains only non-secret derived identity and the
// stable target known before repository access. Authorization adapters must
// never receive bearer bytes.
type StudyAuthorizationRequest struct {
	Principal  model.StudyPrincipalID
	Operation  StudyOperation
	Stage      StudyAuthorizationStage
	ProfileRef string
	SessionID  uuid.UUID
}

// StudyAuthorizer is the mandatory policy boundary for every service call.
// Implementations return a structured study error and perform no storage work.
type StudyAuthorizer interface {
	AuthorizeStudy(context.Context, StudyAuthorizationRequest) error
}

// OperatorStudyAuthorizer is the explicit Step 4 policy: the exact stable
// operator principal may use all three study capabilities across profiles.
type OperatorStudyAuthorizer struct{}

// AuthorizeStudy grants the frozen operations only to the stable operator
// principal. Invalid principals fail as unauthenticated; other valid principals
// fail as denied.
func (OperatorStudyAuthorizer) AuthorizeStudy(_ context.Context, request StudyAuthorizationRequest) error {
	if !request.Principal.Valid() {
		return newStudySessionError(model.StudyErrorCodeUnauthorized)
	}
	if !request.Operation.Valid() || !request.Stage.valid() {
		return newStudySessionError(model.StudyErrorCodeAuthorizationDenied)
	}
	if request.Principal.String() != model.OperatorStudyPrincipalID().String() {
		return newStudySessionError(model.StudyErrorCodeAuthorizationDenied)
	}
	return nil
}
