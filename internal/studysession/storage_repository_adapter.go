package studysession

import (
	"context"

	"github.com/google/uuid"

	"lexinet/internal/model"
	"lexinet/internal/storage"
	"lexinet/internal/study"
)

// StorageRepositoryAdapter maps the public study-session composition seam to
// the storage package's transaction-owning repository methods.
type StorageRepositoryAdapter struct {
	repository *storage.Repository
}

var _ StudySessionRepository = (*StorageRepositoryAdapter)(nil)

// NewStorageRepositoryAdapter binds one concrete Postgres repository.
func NewStorageRepositoryAdapter(repository *storage.Repository) (*StorageRepositoryAdapter, error) {
	if repository == nil {
		return nil, newStudySessionError(model.StudyErrorCodeSchemaInvalid)
	}
	return &StorageRepositoryAdapter{repository: repository}, nil
}

// ResolveStudyScope resolves aliases to stable unique topic IDs.
func (adapter *StorageRepositoryAdapter) ResolveStudyScope(
	ctx context.Context,
	topicRefs []string,
	cardKind model.StudyCardKind,
) (model.StudyNormalizedScope, error) {
	if adapter == nil || adapter.repository == nil {
		return model.StudyNormalizedScope{}, newStudySessionError(model.StudyErrorCodeInternal)
	}
	return adapter.repository.ResolveStudyScope(ctx, append([]string(nil), topicRefs...), cardKind)
}

// GetStudyLearnerProfileBySessionID resolves only the narrow profile ownership
// target needed to authorize GET/sync before any item or page content is read.
func (adapter *StorageRepositoryAdapter) GetStudyLearnerProfileBySessionID(
	ctx context.Context,
	sessionID uuid.UUID,
) (*model.StudyLearnerProfile, error) {
	if adapter == nil || adapter.repository == nil {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	return adapter.repository.GetStudyLearnerProfileBySessionID(ctx, sessionID)
}

// MaterializeStudyCycle enters the storage-owned scope transaction and invokes
// the canonical materializer without exposing pgx authority.
func (adapter *StorageRepositoryAdapter) MaterializeStudyCycle(
	ctx context.Context,
	materializer *study.CycleMaterializer,
	command StudyCycleMaterializationCommand,
) (*study.CycleMaterializationResult, error) {
	if adapter == nil || adapter.repository == nil {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	return adapter.repository.MaterializeStudyCycle(ctx, materializer, storage.StudyCycleMaterializationRequest{
		Transaction: storage.StudyScopeTransactionInput{
			ProfileRef: command.ProfileRef,
			Scope:      command.Scope,
		},
		Materialization: command.Materialization,
	})
}

// ReadCommittedStudyCycle reads the exact creation receipt, issuance fields,
// and ordered items from one committed snapshot.
func (adapter *StorageRepositoryAdapter) ReadCommittedStudyCycle(
	ctx context.Context,
	command StudyCommittedCycleReadCommand,
) (*study.CommittedCycle, error) {
	if adapter == nil || adapter.repository == nil {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	return adapter.repository.ReadCommittedStudyCycle(ctx, storage.StudyCommittedCycleReadInput{
		Principal:         command.Principal,
		ProfileRef:        command.ProfileRef,
		SessionID:         command.SessionID,
		CreationRequestID: command.CreationRequestID,
	})
}

// ReadStudySessionCycle reads profile, session, and items from one consistent
// storage snapshot for GET and sync.
func (adapter *StorageRepositoryAdapter) ReadStudySessionCycle(
	ctx context.Context,
	sessionID uuid.UUID,
) (*StudySessionCycleAuthority, error) {
	if adapter == nil || adapter.repository == nil {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	result, err := adapter.repository.ReadStudySessionCycle(ctx, sessionID)
	if err != nil || result == nil {
		return nil, err
	}
	return &StudySessionCycleAuthority{
		Profile: result.Profile,
		Session: result.Session,
		Items:   cloneStudySessionItems(result.Items),
	}, nil
}

// AdjudicateStudyEvents atomically binds terminal event receipts, one flush
// barrier, and the immutable page artifact.
func (adapter *StorageRepositoryAdapter) AdjudicateStudyEvents(
	ctx context.Context,
	command StudyEventSyncCommand,
) (*StudyEventSyncAuthority, error) {
	if adapter == nil || adapter.repository == nil {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	result, err := adapter.repository.AdjudicateStudyEvents(ctx, storage.StudyEventSyncInput{
		Principal:               command.Principal,
		ProfileRef:              command.ProfileRef,
		SessionID:               command.SessionID,
		SyncRequestID:           command.SyncRequestID,
		NormalizedRequestSHA256: command.NormalizedRequestSHA256,
		ClientStreamID:          command.ClientStreamID,
		ExpectedStateVersion:    command.ExpectedStateVersion,
		Events:                  cloneStudyEvents(command.Events),
		PageArtifact:            cloneStudyPageArtifact(command.PageArtifact),
	})
	if err != nil || result == nil {
		return nil, err
	}
	return &StudyEventSyncAuthority{
		Receipts:     cloneStudyEventReceipts(result.Receipts),
		FlushReceipt: cloneStudyFlushReceipt(result.FlushReceipt),
		Session:      cloneStudySession(result.Session),
		PageArtifact: cloneStudyPageArtifact(result.PageArtifact),
		Replayed:     result.Replayed,
	}, nil
}

func cloneStudySessionItems(items []model.StudySessionItem) []model.StudySessionItem {
	clone := make([]model.StudySessionItem, len(items))
	for index := range items {
		clone[index] = cloneStudySessionItem(items[index])
	}
	return clone
}

func cloneStudyEvents(events []model.StudyEvent) []model.StudyEvent {
	clone := make([]model.StudyEvent, len(events))
	for index, event := range events {
		clone[index] = event
		clone[index].Rating = cloneStudyRating(event.Rating)
		clone[index].RatingEventID = cloneUUIDPointer(event.RatingEventID)
		clone[index].ClientObservedAt = cloneTimePointer(event.ClientObservedAt)
		clone[index].ProfileIngestionOrdinal = cloneInt64Pointer(event.ProfileIngestionOrdinal)
		clone[index].ReceivedAt = cloneTimePointer(event.ReceivedAt)
	}
	return clone
}

func cloneStudyEventReceipts(receipts []model.StudyEventReceipt) []model.StudyEventReceipt {
	clone := make([]model.StudyEventReceipt, len(receipts))
	for index, receipt := range receipts {
		clone[index] = receipt
		clone[index].TerminalFailureClientSequence = cloneInt64Pointer(receipt.TerminalFailureClientSequence)
		clone[index].CanonicalStudyEventID = cloneUUIDPointer(receipt.CanonicalStudyEventID)
		clone[index].CanonicalProfileIngestionOrdinal = cloneInt64Pointer(receipt.CanonicalProfileIngestionOrdinal)
	}
	return clone
}

func cloneStudyFlushReceipt(receipt model.StudyFlushReceipt) model.StudyFlushReceipt {
	clone := receipt
	clone.RequestFirstClientSequence = cloneInt64Pointer(receipt.RequestFirstClientSequence)
	clone.RequestLastClientSequence = cloneInt64Pointer(receipt.RequestLastClientSequence)
	clone.FirstClientSequence = cloneInt64Pointer(receipt.FirstClientSequence)
	clone.LastClientSequence = cloneInt64Pointer(receipt.LastClientSequence)
	return clone
}

func cloneStudySession(session model.StudySession) model.StudySession {
	clone := session
	clone.Scope.Scope.TopicIDs = append([]uuid.UUID(nil), session.Scope.Scope.TopicIDs...)
	clone.Scope.Scope.TopicRefs = append([]string(nil), session.Scope.Scope.TopicRefs...)
	clone.Scope.CanonicalSelector = append([]byte(nil), session.Scope.CanonicalSelector...)
	clone.Scheduler = cloneStudyScheduler(session.Scheduler)
	clone.PredecessorStudySessionID = cloneUUIDPointer(session.PredecessorStudySessionID)
	clone.SuccessorStudySessionID = cloneUUIDPointer(session.SuccessorStudySessionID)
	clone.TerminalizedAt = cloneTimePointer(session.TerminalizedAt)
	clone.TerminalProfileIngestionHighWater = cloneInt64Pointer(session.TerminalProfileIngestionHighWater)
	return clone
}

func cloneStudyScheduler(value *model.StudySchedulerProvenance) *model.StudySchedulerProvenance {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneStudyPageArtifact(artifact model.StudySessionPageArtifact) model.StudySessionPageArtifact {
	clone := artifact
	clone.CanonicalJSON = append([]byte(nil), artifact.CanonicalJSON...)
	return clone
}
