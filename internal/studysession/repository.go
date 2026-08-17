package studysession

import (
	"context"

	"github.com/google/uuid"

	"lexinet/internal/model"
	"lexinet/internal/study"
)

// StudyCycleMaterializationCommand binds one normalized service request to the
// storage lock domain used by the canonical cycle materializer.
type StudyCycleMaterializationCommand struct {
	ProfileRef      string
	Scope           model.StudyNormalizedScope
	Materialization study.CycleMaterializationInput
}

// StudyCommittedCycleReadCommand names the exact post-commit receipt and cycle
// authority that must be read in one consistent snapshot.
type StudyCommittedCycleReadCommand struct {
	Principal         model.StudyPrincipalID
	ProfileRef        string
	SessionID         uuid.UUID
	CreationRequestID uuid.UUID
}

// StudySessionCycleAuthority is one consistent read for GET and sync page
// composition. It contains no transaction or driver capability.
type StudySessionCycleAuthority struct {
	Profile model.StudyLearnerProfile
	Session model.StudySession
	Items   []model.StudySessionItem
}

// StudyEventSyncCommand contains normalized event authority and the exact page
// artifact that storage atomically binds to a new flush receipt.
type StudyEventSyncCommand struct {
	Principal               model.StudyPrincipalID
	ProfileRef              string
	SessionID               uuid.UUID
	SyncRequestID           uuid.UUID
	NormalizedRequestSHA256 string
	ClientStreamID          uuid.UUID
	ExpectedStateVersion    int64
	Events                  []model.StudyEvent
	PageArtifact            model.StudySessionPageArtifact
}

// StudyEventSyncAuthority contains immutable receipt/page authority plus the
// current session envelope. Replayed does not weaken artifact stability.
type StudyEventSyncAuthority struct {
	Receipts     []model.StudyEventReceipt
	FlushReceipt model.StudyFlushReceipt
	Session      model.StudySession
	PageArtifact model.StudySessionPageArtifact
	Replayed     bool
}

// StudySessionRepository is the complete persistence capability required by
// StudySessionService. HTTP handlers receive this interface only indirectly
// through the service and never orchestrate its methods.
type StudySessionRepository interface {
	ResolveStudyScope(context.Context, []string, model.StudyCardKind) (model.StudyNormalizedScope, error)
	GetStudyLearnerProfileBySessionID(context.Context, uuid.UUID) (*model.StudyLearnerProfile, error)
	MaterializeStudyCycle(context.Context, *study.CycleMaterializer, StudyCycleMaterializationCommand) (*study.CycleMaterializationResult, error)
	ReadCommittedStudyCycle(context.Context, StudyCommittedCycleReadCommand) (*study.CommittedCycle, error)
	ReadStudySessionCycle(context.Context, uuid.UUID) (*StudySessionCycleAuthority, error)
	AdjudicateStudyEvents(context.Context, StudyEventSyncCommand) (*StudyEventSyncAuthority, error)
}
