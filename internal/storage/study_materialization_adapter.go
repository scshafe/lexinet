package storage

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lexinet/internal/model"
	"lexinet/internal/study"
)

// StudyMaterializationAdapter is the sole bridge from the storage-private
// transaction seam to the storage-neutral study cycle materializer.
type StudyMaterializationAdapter struct {
	transaction StudyStoreTransaction
}

// StudyCycleMaterializationRequest binds one retry-stable materialization to
// the storage lock domain that must commit it.
type StudyCycleMaterializationRequest struct {
	Transaction     StudyScopeTransactionInput
	Materialization study.CycleMaterializationInput
}

// StudyCommittedCycleReadInput names the exact immutable creation receipt that
// must be observed with its session and items in one committed snapshot.
type StudyCommittedCycleReadInput struct {
	Principal         model.StudyPrincipalID
	ProfileRef        string
	SessionID         uuid.UUID
	CreationRequestID uuid.UUID
}

// StudySessionCycleRead is a storage-neutral consistent snapshot for GET and
// sync composition. It contains no transaction or pgx implementation detail.
type StudySessionCycleRead struct {
	Profile model.StudyLearnerProfile
	Session model.StudySession
	Items   []model.StudySessionItem
}

var _ study.CycleMaterializationStore = (*StudyMaterializationAdapter)(nil)

// NewStudyMaterializationAdapter binds an already-open study scope
// transaction. The adapter never begins or commits a transaction itself.
func NewStudyMaterializationAdapter(transaction StudyStoreTransaction) (*StudyMaterializationAdapter, error) {
	if transaction == nil {
		return nil, newStudyStorageError(
			model.StudyErrorCodeSchemaInvalid,
			"study materialization transaction is required",
			"study_materialization_adapter_construct",
			false,
			nil,
		)
	}
	return &StudyMaterializationAdapter{transaction: transaction}, nil
}

// MaterializeStudyCycle is the production transaction boundary. A staged
// materializer result is never exposed unless the enclosing database commit
// succeeds; optional shadow evaluation belongs strictly after this method.
func (repository *Repository) MaterializeStudyCycle(
	ctx context.Context,
	materializer *study.CycleMaterializer,
	request StudyCycleMaterializationRequest,
) (*study.CycleMaterializationResult, error) {
	if repository == nil || materializer == nil {
		return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study materialization boundary is invalid", "study_materialization", false, nil)
	}
	var staged *study.CycleMaterializationResult
	err := repository.WithStudyScopeTransaction(ctx, request.Transaction, func(transaction StudyStoreTransaction) error {
		adapter, err := NewStudyMaterializationAdapter(transaction)
		if err != nil {
			return err
		}
		result, err := materializer.Materialize(ctx, adapter, request.Materialization)
		if err != nil {
			return err
		}
		staged = result
		return nil
	})
	if err != nil {
		return nil, err
	}
	if staged == nil {
		return nil, newStudyStorageError(model.StudyErrorCodeInternal, "study materialization committed without authority", "study_materialization", false, nil)
	}
	return staged, nil
}

func (adapter *StudyMaterializationAdapter) ResolveExistingCycle(
	ctx context.Context,
	input study.ExistingCycleResolutionInput,
) (*study.ExistingCycleResolution, error) {
	if adapter == nil || adapter.transaction == nil {
		return nil, newStudyStorageError(model.StudyErrorCodeInternal, "study materialization adapter is not constructed", "study_materialization_resolve", false, nil)
	}
	result, err := adapter.transaction.ResolveExistingCycle(ctx, StudyExistingCycleResolutionInput{
		Principal:               input.Principal,
		CreationRequestID:       input.CreationRequestID,
		NormalizedRequestSHA256: input.NormalizedRequestSHA256,
		PredecessorSessionID:    cloneStudyUUIDPointer(input.PredecessorSessionID),
	})
	if err != nil || result == nil {
		return nil, err
	}
	lockedScope := adapter.transaction.Scope()
	if err := validateStudyMaterializationResultCloneBounds(result, lockedScope); err != nil {
		return nil, err
	}
	var waitingEvidence *study.WaitingReplacementEvidence
	if result.ImplicitWaitingReplacement != nil {
		waitingEvidence = &study.WaitingReplacementEvidence{
			Predecessor: cloneStudySession(result.ImplicitWaitingReplacement.Predecessor),
			Items:       cloneStudySessionItems(result.ImplicitWaitingReplacement.Items),
		}
	}
	return &study.ExistingCycleResolution{
		LearnerProfile:             adapter.transaction.Profile(),
		Scope:                      cloneStudyNormalizedScope(lockedScope),
		ImplicitWaitingReplacement: waitingEvidence,
		Persistence: study.CyclePersistenceResult{
			Session: cloneStudySession(result.Plan.Session),
			Items:   cloneStudySessionItems(result.Plan.Items),
			Receipt: cloneStudyCreationReceipt(result.Plan.Receipt),
			Created: false,
		},
	}, nil
}

// CaptureEligibleCorpus performs the locked source read before exposing its
// storage-owned digest or the locked nonterminal authority.
func (adapter *StudyMaterializationAdapter) CaptureEligibleCorpus(
	ctx context.Context,
) (study.CapturedEligibleCorpus, error) {
	if adapter == nil || adapter.transaction == nil {
		return study.CapturedEligibleCorpus{}, newStudyStorageError(model.StudyErrorCodeInternal, "study materialization adapter is not constructed", "study_materialization_capture", false, nil)
	}
	sources, err := adapter.transaction.ListEligibleSources(ctx)
	if err != nil {
		return study.CapturedEligibleCorpus{}, err
	}
	corpusSHA256, err := adapter.transaction.CapturedEligibleCorpusSHA256()
	if err != nil {
		return study.CapturedEligibleCorpus{}, err
	}
	captured := study.CapturedEligibleCorpus{
		LearnerProfile:     adapter.transaction.Profile(),
		Scope:              adapter.transaction.Scope(),
		CorpusSHA256:       corpusSHA256,
		NonterminalSession: adapter.transaction.NonterminalSession(),
		Sources:            make([]study.MaterializationSource, len(sources)),
	}
	for index, source := range sources {
		captured.Sources[index] = study.MaterializationSource{
			CardIdentity:             source.IdeaID,
			LiveSourceIdeaID:         source.IdeaID,
			HistoricalSourceKind:     string(model.StudyCardKindIdea),
			HistoricalSourceIdentity: source.IdeaID,
			SourceContentRevision:    source.SourceContentRevision,
			SourceContentSHA256:      source.SourceContentSHA256,
			FrontCanonicalName:       source.CanonicalName,
			BackDefinition:           source.Definition,
			BackExample:              cloneStudyStringPointer(source.Example),
		}
	}
	return captured, nil
}

func (adapter *StudyMaterializationAdapter) ReadAcceptedPrefix(
	ctx context.Context,
	highWater int64,
) ([]model.StudyEvent, error) {
	return adapter.transaction.ReadAcceptedPrefix(ctx, highWater)
}

func (adapter *StudyMaterializationAdapter) ResolveCardIdentity(
	ctx context.Context,
	identity uuid.UUID,
) (uuid.UUID, error) {
	return adapter.transaction.ResolveCardIdentity(ctx, identity)
}

func (adapter *StudyMaterializationAdapter) GetProjection(
	ctx context.Context,
	key study.MaterializationProjectionKey,
) (*model.StudySchedulerCardProjection, error) {
	return adapter.transaction.GetProjection(ctx, StudyProjectionKey{
		LearnerProfileID:        key.LearnerProfileID,
		CardIdentity:            key.CardIdentity,
		SchedulerName:           key.SchedulerName,
		SchedulerVersion:        key.SchedulerVersion,
		ProjectionSchemaVersion: key.ProjectionSchemaVersion,
	})
}

func (adapter *StudyMaterializationAdapter) ReplaceProjection(
	ctx context.Context,
	projection model.StudySchedulerCardProjection,
	expectedRevision int64,
) (*model.StudySchedulerCardProjection, error) {
	return adapter.transaction.ReplaceProjection(ctx, projection, expectedRevision)
}

func (adapter *StudyMaterializationAdapter) ListSessionItems(
	ctx context.Context,
	sessionID uuid.UUID,
) ([]model.StudySessionItem, error) {
	return adapter.transaction.ListSessionItems(ctx, sessionID)
}

func (adapter *StudyMaterializationAdapter) InsertCycleOnce(
	ctx context.Context,
	input study.CyclePersistenceInput,
) (*study.CyclePersistenceResult, error) {
	plan := StudyPlanInsert{
		SessionID:               input.SessionID,
		Principal:               input.Principal,
		CreationRequestID:       input.CreationRequestID,
		NormalizedRequestSHA256: input.NormalizedRequestSHA256,
		Lifecycle:               input.Lifecycle,
		CorpusSHA256:            input.CorpusSHA256,
		Scheduler:               cloneStudySchedulerProvenance(input.Scheduler),
		PredecessorSessionID:    cloneStudyUUIDPointer(input.PredecessorSessionID),
		Items:                   cloneStudySessionItems(input.Items),
		LifecycleReasonCode:     input.LifecycleReasonCode,
	}
	var (
		result *StudyPlanInsertResult
		err    error
	)
	if input.PredecessorSessionID == nil {
		result, err = adapter.transaction.InsertPlanOnce(ctx, plan)
	} else {
		result, err = adapter.transaction.InsertSuccessorPlanOnce(ctx, StudySuccessorPlanInsert{
			Plan:                          plan,
			PredecessorFlushReceiptID:     input.PredecessorFlushReceiptID,
			PredecessorTerminalReasonCode: input.PredecessorTerminalReasonCode,
		})
	}
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, newStudyStorageError(model.StudyErrorCodeInternal, "study persistence returned no materialized cycle", "study_materialization_insert", false, nil)
	}
	if err := validateStudyPlanInsertResultCloneBounds(result); err != nil {
		return nil, err
	}
	return &study.CyclePersistenceResult{
		Session: cloneStudySession(result.Session),
		Items:   cloneStudySessionItems(result.Items),
		Receipt: cloneStudyCreationReceipt(result.Receipt),
		Created: result.Created,
	}, nil
}

func validateStudyMaterializationResultCloneBounds(
	result *StudyExistingCycleResolution,
	lockedScope model.StudyNormalizedScope,
) error {
	if result == nil {
		return newStudyStorageError(model.StudyErrorCodeInternal, "study persistence returned no materialized cycle", "study_materialization_clone", false, nil)
	}
	if err := validateStudyPlanInsertResultCloneBounds(&result.Plan); err != nil {
		return err
	}
	if err := validateStudyMaterializationScopeCloneBounds(lockedScope); err != nil {
		return err
	}
	if result.ImplicitWaitingReplacement != nil {
		if len(result.ImplicitWaitingReplacement.Items) != 0 {
			return newStudyStorageError(model.StudyErrorCodeInternal, "study waiting replacement evidence contains items", "study_materialization_clone", false, nil)
		}
		if err := validateStudyMaterializationScopeCloneBounds(result.ImplicitWaitingReplacement.Predecessor.Scope); err != nil {
			return err
		}
	}
	return nil
}

func validateStudyPlanInsertResultCloneBounds(result *StudyPlanInsertResult) error {
	if result == nil {
		return newStudyStorageError(model.StudyErrorCodeInternal, "study persistence returned no materialized cycle", "study_materialization_clone", false, nil)
	}
	if len(result.Items) > study.StudyMaximumSchedulingCards {
		return newStudyStorageError(model.StudyErrorCodeRequestTooLarge, "study persistence exceeds the materialization work bound", "study_materialization_clone", false, nil)
	}
	if err := validateStudyMaterializationScopeCloneBounds(result.Session.Scope); err != nil {
		return err
	}
	return validateStudyMaterializationScopeCloneBounds(result.Receipt.Scope)
}

func validateStudyMaterializationScopeCloneBounds(scope model.StudyNormalizedScope) error {
	if len(scope.Scope.TopicIDs) > model.StudyMaximumTopics ||
		len(scope.Scope.TopicRefs) > model.StudyMaximumTopics ||
		len(scope.Scope.TopicIDs) != len(scope.Scope.TopicRefs) ||
		len(scope.CanonicalSelector) > study.StudyMaximumCanonicalSelectorBytes {
		return newStudyStorageError(model.StudyErrorCodeRequestTooLarge, "study scope exceeds the materialization work bound", "study_materialization_clone", false, nil)
	}
	return nil
}

// ReadCommittedStudyCycle performs a fresh repository read after the scope
// transaction has returned. A post-commit caller may use this as a visibility
// barrier before invoking the store-free shadow runner.
func (repository *Repository) ReadCommittedStudyCycle(
	ctx context.Context,
	input StudyCommittedCycleReadInput,
) (*study.CommittedCycle, error) {
	if !input.Principal.Valid() || !validStudyStorageReference(input.ProfileRef) ||
		input.SessionID == uuid.Nil || input.CreationRequestID == uuid.Nil {
		return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "committed study cycle lookup is invalid", "study_committed_cycle_read", false, nil)
	}
	tx, err := repository.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, mapStudyStorageError("study_committed_cycle_read_begin", err)
	}
	defer tx.Rollback(ctx)

	session, err := queryStudySession(ctx, tx, input.SessionID, uuid.Nil, input.ProfileRef, false)
	if err != nil {
		return nil, err
	}
	receipt, err := readStudyCreationReceipt(
		ctx,
		tx,
		session.LearnerProfileID,
		session.Scope,
		input.Principal,
		input.CreationRequestID,
	)
	if err != nil {
		return nil, err
	}
	if receipt == nil || receipt.CanonicalStudySessionID != input.SessionID {
		return nil, newStudyStorageError(model.StudyErrorCodeInternal, "exact committed study creation receipt is unavailable", "study_committed_cycle_read", false, nil)
	}
	items, err := listStudySessionItems(ctx, tx, input.SessionID)
	if err != nil {
		return nil, err
	}
	committed := &study.CommittedCycle{
		Session: cloneStudySession(*session),
		Items:   cloneStudySessionItems(items),
		Receipt: cloneStudyCreationReceipt(*receipt),
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapStudyStorageError("study_committed_cycle_read_commit", err)
	}
	return committed, nil
}

// ReadStudySessionCycle returns learner, session, and ordered issued items from
// one repeatable-read snapshot so mutable live links cannot straddle commits.
func (repository *Repository) ReadStudySessionCycle(
	ctx context.Context,
	sessionID uuid.UUID,
) (*StudySessionCycleRead, error) {
	if sessionID == uuid.Nil {
		return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study session cycle lookup is invalid", "study_session_cycle_read", false, nil)
	}
	tx, err := repository.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, mapStudyStorageError("study_session_cycle_read_begin", err)
	}
	defer tx.Rollback(ctx)
	session, err := queryStudySession(ctx, tx, sessionID, uuid.Nil, "", false)
	if err != nil {
		return nil, err
	}
	profile, err := scanStudyProfile(tx.QueryRow(ctx, `
		SELECT learner_profile_id, learner_profile_ref, profile_ingestion_high_water, created_at, updated_at
		FROM study_learner_profiles
		WHERE learner_profile_id = $1`, session.LearnerProfileID))
	if err != nil {
		return nil, mapStudyStorageError("study_session_cycle_profile_read", err)
	}
	items, err := listStudySessionItems(ctx, tx, sessionID)
	if err != nil {
		return nil, err
	}
	result := &StudySessionCycleRead{
		Profile: *profile,
		Session: cloneStudySession(*session),
		Items:   cloneStudySessionItems(items),
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapStudyStorageError("study_session_cycle_read_commit", err)
	}
	return result, nil
}

func cloneStudySchedulerProvenance(
	provenance *model.StudySchedulerProvenance,
) *model.StudySchedulerProvenance {
	if provenance == nil {
		return nil
	}
	clone := *provenance
	return &clone
}

func cloneStudySessionItems(items []model.StudySessionItem) []model.StudySessionItem {
	clone := make([]model.StudySessionItem, len(items))
	for index, item := range items {
		clone[index] = item
		clone[index].LiveSourceIdeaID = cloneStudyUUIDPointer(item.LiveSourceIdeaID)
		clone[index].BackExample = cloneStudyStringPointer(item.BackExample)
		clone[index].SelectionReason.LatestPresentedIngestionOrdinal = cloneStudyInt64Pointer(item.SelectionReason.LatestPresentedIngestionOrdinal)
	}
	return clone
}

func cloneStudyCreationReceipt(receipt model.StudyCreationReceipt) model.StudyCreationReceipt {
	clone := receipt
	clone.Scope = cloneStudyNormalizedScope(receipt.Scope)
	return clone
}
