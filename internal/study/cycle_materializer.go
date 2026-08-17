package study

import (
	"bytes"
	"context"
	"encoding/binary"
	"sort"
	"time"

	"github.com/google/uuid"

	"lexinet/internal/model"
	studydigest "lexinet/internal/study/digest"
)

const (
	studyCycleMaterializationOperation = "study_cycle_materialize"
	// StudyMaximumCanonicalSelectorBytes mirrors the durable study.v1 selector
	// bound and protects neutral store crossings before any deep copy.
	StudyMaximumCanonicalSelectorBytes = 8192
)

// cycleMaterializerConfiguration binds the one scheduler namespace whose
// output this materializer may persist. The scheduler remains an untrusted
// extension point: every returned byte is validated against this identity and
// an untouched authoritative snapshot before storage sees it.
type cycleMaterializerConfiguration struct {
	SchedulerName                string
	SchedulerVersion             string
	SchedulerConfigurationSHA256 string
	ProjectionSchemaVersion      int
}

// CycleMaterializer composes locked persistence evidence with one pure
// scheduler. It owns no database, clock, random source, or UUID generator.
//
// The reviewed production scheduler is bounded and invoked in-process. A
// scheduler that may never terminate requires process isolation and is outside
// this interface; panics and invalid returns are converted into structured
// failures before any plan insert is attempted.
//
// @canonical study_cycle_materializer -- all production cycle creation and
// successor planning must cross this transaction-bound composition root.
type CycleMaterializer struct {
	scheduler       StudyScheduler
	itemIDAllocator StudyItemIDAllocator
	configuration   cycleMaterializerConfiguration
}

// StudyItemIDAllocator allocates one bounded batch of fresh persistence
// identities after locked corpus capture proves the exact required count.
// Implementations must return exactly requestedCount distinct nonzero UUIDs.
//
// @extension-point study_item_id_allocator -- composition roots may bind a
// cryptographically random or deterministic test allocator without granting
// either allocator corpus, profile, request, or persistence authority.
type StudyItemIDAllocator interface {
	AllocateStudyItemIDs(context.Context, int) ([]uuid.UUID, error)
}

// StudyItemIDAllocatorFunc adapts one function to StudyItemIDAllocator.
type StudyItemIDAllocatorFunc func(context.Context, int) ([]uuid.UUID, error)

// AllocateStudyItemIDs invokes the adapted item identity allocator.
func (function StudyItemIDAllocatorFunc) AllocateStudyItemIDs(
	ctx context.Context,
	requestedCount int,
) ([]uuid.UUID, error) {
	return function(ctx, requestedCount)
}

// NewCanonicalCycleMaterializer binds the exact reviewed variety-first-v1
// scheduler and projection namespace. Callers cannot inject or copy the
// canonical configuration digest: it is derived from the scheduler package's
// own immutable identity.
func NewCanonicalCycleMaterializer(
	scheduler StudyScheduler,
	itemIDAllocator StudyItemIDAllocator,
) (*CycleMaterializer, error) {
	canonicalScheduler, err := NewVarietyFirstV1()
	if err != nil {
		return nil, newCycleMaterializationError(
			model.StudyErrorCodeInternal,
			"canonical study scheduler identity is unavailable",
			"configuration",
			"canonical_scheduler_unavailable",
			err,
		)
	}
	return newCycleMaterializer(scheduler, itemIDAllocator, cycleMaterializerConfiguration{
		SchedulerName:                VarietyFirstV1Name,
		SchedulerVersion:             VarietyFirstV1Version,
		SchedulerConfigurationSHA256: canonicalScheduler.identity.configurationSHA256,
		ProjectionSchemaVersion:      StudyProjectionSchemaVersionV1,
	})
}

// newCycleMaterializer is package-private so only study-owned tests can probe
// configuration drift. Production packages must use the canonical constructor.
func newCycleMaterializer(
	scheduler StudyScheduler,
	itemIDAllocator StudyItemIDAllocator,
	configuration cycleMaterializerConfiguration,
) (*CycleMaterializer, error) {
	if scheduler == nil || itemIDAllocator == nil {
		return nil, newCycleMaterializationError(
			model.StudyErrorCodeSchemaInvalid,
			"study cycle materializer dependencies are required",
			"dependencies",
			"invalid_configuration",
			nil,
		)
	}
	canonicalScheduler, err := NewVarietyFirstV1()
	if err != nil {
		return nil, newCycleMaterializationError(
			model.StudyErrorCodeInternal,
			"canonical study scheduler identity is unavailable",
			"configuration",
			"canonical_scheduler_unavailable",
			err,
		)
	}
	if configuration.SchedulerName != VarietyFirstV1Name ||
		configuration.SchedulerVersion != VarietyFirstV1Version ||
		configuration.SchedulerConfigurationSHA256 != canonicalScheduler.identity.configurationSHA256 ||
		configuration.ProjectionSchemaVersion != StudyProjectionSchemaVersionV1 {
		return nil, newCycleMaterializationError(
			model.StudyErrorCodeSchemaInvalid,
			"study cycle materializer configuration is invalid",
			"configuration",
			"invalid_configuration",
			nil,
		)
	}
	return &CycleMaterializer{
		scheduler:       scheduler,
		itemIDAllocator: itemIDAllocator,
		configuration:   configuration,
	}, nil
}

// CycleMaterializationInput contains every retry-stable authority value that
// is not obtained from the locked store. Item identities are deliberately not
// request authority: the materializer allocates them only for a captured,
// nonempty corpus after durable receipt replay has been ruled out.
type CycleMaterializationInput struct {
	SessionID                     uuid.UUID
	Principal                     model.StudyPrincipalID
	CreationRequestID             uuid.UUID
	NormalizedRequestSHA256       string
	PlannedAt                     time.Time
	Seed                          string
	PredecessorSessionID          *uuid.UUID
	PredecessorFlushReceiptID     uuid.UUID
	LifecycleReasonCode           string
	PredecessorTerminalReasonCode string
}

// CapturedEligibleCorpus is exposed only after storage has locked and fully
// materialized the eligible source rows. NonterminalSession is the exact
// locked active/waiting authority observed in the same transaction.
type CapturedEligibleCorpus struct {
	LearnerProfile     model.StudyLearnerProfile
	Scope              model.StudyNormalizedScope
	CorpusSHA256       string
	NonterminalSession *model.StudySession
	Sources            []MaterializationSource
}

// MaterializationSource is one locked current card plus the complete frozen
// bytes storage will bind again at insertion. Current and historical
// identities are separate even though both identify the live idea today.
type MaterializationSource struct {
	CardIdentity             uuid.UUID
	LiveSourceIdeaID         uuid.UUID
	HistoricalSourceKind     string
	HistoricalSourceIdentity uuid.UUID
	SourceContentRevision    string
	SourceContentSHA256      string
	FrontCanonicalName       string
	BackDefinition           string
	BackExample              *string
}

// MaterializationProjectionKey names one complete rebuild target.
type MaterializationProjectionKey struct {
	LearnerProfileID        uuid.UUID
	CardIdentity            uuid.UUID
	SchedulerName           string
	SchedulerVersion        string
	ProjectionSchemaVersion int
}

// CyclePersistenceInput is the storage-neutral root/successor insert DTO.
type CyclePersistenceInput struct {
	SessionID                     uuid.UUID
	Principal                     model.StudyPrincipalID
	CreationRequestID             uuid.UUID
	NormalizedRequestSHA256       string
	Lifecycle                     model.StudySessionLifecycle
	CorpusSHA256                  string
	Scheduler                     *model.StudySchedulerProvenance
	PredecessorSessionID          *uuid.UUID
	Items                         []model.StudySessionItem
	LifecycleReasonCode           string
	PredecessorFlushReceiptID     uuid.UUID
	PredecessorTerminalReasonCode string
}

// CyclePersistenceResult is the committed-or-converged authority returned by
// the transaction-bound store. It is reconstructed rather than trusting the
// scheduler candidate when an existing issued cycle wins convergence.
type CyclePersistenceResult struct {
	Session model.StudySession
	Items   []model.StudySessionItem
	Receipt model.StudyCreationReceipt
	Created bool
}

// ExistingCycleResolutionInput contains only stable request identity. Storage
// uses it to adjudicate an already-bound creation receipt before the
// materializer reads mutable corpus, projection, merge, or scheduler state.
type ExistingCycleResolutionInput struct {
	Principal               model.StudyPrincipalID
	CreationRequestID       uuid.UUID
	NormalizedRequestSHA256 string
	PredecessorSessionID    *uuid.UUID
}

// WaitingReplacementEvidence is the complete durable predecessor authority
// needed to prove that a root-shaped request implicitly replaced an empty
// waiting sentinel. It is validated independently inside the materializer.
type WaitingReplacementEvidence struct {
	Predecessor model.StudySession
	Items       []model.StudySessionItem
}

// ExistingCycleResolution binds durable receipt authority to the profile and
// scope locked by the surrounding transaction without exposing mutable corpus
// state.
type ExistingCycleResolution struct {
	LearnerProfile             model.StudyLearnerProfile
	Scope                      model.StudyNormalizedScope
	Persistence                CyclePersistenceResult
	ImplicitWaitingReplacement *WaitingReplacementEvidence
}

// CycleMaterializationResult contains only persisted authority. Waiting
// sentinels have nil SchedulingSnapshot and AuthoritativePlan because no
// scheduler was invoked or persisted for an empty corpus.
type CycleMaterializationResult struct {
	Persistence        CyclePersistenceResult
	SchedulingSnapshot *StudySchedulingSnapshot
	AuthoritativePlan  *StudyPlan
}

// CommittedCycle is one independently readable session/item/receipt snapshot
// used by a post-commit caller before optional shadow evaluation. Storage must
// read all three authorities from one consistent committed snapshot and bind
// Receipt to the exact principal, creation request, and canonical session.
type CommittedCycle struct {
	Session model.StudySession
	Items   []model.StudySessionItem
	Receipt model.StudyCreationReceipt
}

// CycleMaterializationStore is the narrow transaction capability required by
// the materializer. It deliberately contains no pgx type.
type CycleMaterializationStore interface {
	ResolveExistingCycle(context.Context, ExistingCycleResolutionInput) (*ExistingCycleResolution, error)
	CaptureEligibleCorpus(context.Context) (CapturedEligibleCorpus, error)
	ReadAcceptedPrefix(context.Context, int64) ([]model.StudyEvent, error)
	ResolveCardIdentity(context.Context, uuid.UUID) (uuid.UUID, error)
	GetProjection(context.Context, MaterializationProjectionKey) (*model.StudySchedulerCardProjection, error)
	ReplaceProjection(context.Context, model.StudySchedulerCardProjection, int64) (*model.StudySchedulerCardProjection, error)
	ListSessionItems(context.Context, uuid.UUID) ([]model.StudySessionItem, error)
	InsertCycleOnce(context.Context, CyclePersistenceInput) (*CyclePersistenceResult, error)
}

// Materialize rebuilds the exact accepted prefix, plans one finite cycle, and
// inserts or converges it inside the caller's existing storage transaction.
func (materializer *CycleMaterializer) Materialize(
	ctx context.Context,
	store CycleMaterializationStore,
	input CycleMaterializationInput,
) (*CycleMaterializationResult, error) {
	if materializer == nil || materializer.scheduler == nil || materializer.itemIDAllocator == nil {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "study cycle materializer is not constructed", "materializer", "invalid_configuration", nil)
	}
	if ctx == nil || store == nil {
		return nil, newCycleMaterializationError(model.StudyErrorCodeSchemaInvalid, "study cycle materialization boundary is invalid", "boundary", "invalid_input", nil)
	}
	if err := validateCycleMaterializationInput(input); err != nil {
		return nil, err
	}
	existing, err := store.ResolveExistingCycle(ctx, ExistingCycleResolutionInput{
		Principal:               input.Principal,
		CreationRequestID:       input.CreationRequestID,
		NormalizedRequestSHA256: input.NormalizedRequestSHA256,
		PredecessorSessionID:    cloneCycleUUIDPointer(input.PredecessorSessionID),
	})
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if err := validateCyclePersistenceCloneBounds(&existing.Persistence, existing.ImplicitWaitingReplacement); err != nil {
			return nil, err
		}
		if err := validateCycleRawScopeBounds(existing.Scope); err != nil {
			return nil, err
		}
		if existing.Persistence.Created {
			return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "resolved study creation receipt was reported as newly created", "persistence", "invalid_authority", nil)
		}
		return reconstructExistingCycleAuthority(
			ExistingCycleResolution{
				LearnerProfile:             existing.LearnerProfile,
				Scope:                      cloneCycleNormalizedScope(existing.Scope),
				Persistence:                cloneCyclePersistenceResult(existing.Persistence),
				ImplicitWaitingReplacement: cloneWaitingReplacementEvidence(existing.ImplicitWaitingReplacement),
			},
			materializer.configuration,
			input,
		)
	}

	captured, err := store.CaptureEligibleCorpus(ctx)
	if err != nil {
		return nil, err
	}
	captured, err = validateAndCloneCapturedEligibleCorpus(captured)
	if err != nil {
		return nil, err
	}
	if err := validateCapturedMaterializationPath(input, captured); err != nil {
		return nil, err
	}
	effectivePredecessor := effectiveMaterializationPredecessor(input.PredecessorSessionID, captured)
	persistenceInput := CyclePersistenceInput{
		SessionID:                     input.SessionID,
		Principal:                     input.Principal,
		CreationRequestID:             input.CreationRequestID,
		NormalizedRequestSHA256:       input.NormalizedRequestSHA256,
		CorpusSHA256:                  captured.CorpusSHA256,
		PredecessorSessionID:          cloneCycleUUIDPointer(effectivePredecessor),
		LifecycleReasonCode:           input.LifecycleReasonCode,
		PredecessorFlushReceiptID:     input.PredecessorFlushReceiptID,
		PredecessorTerminalReasonCode: input.PredecessorTerminalReasonCode,
	}
	if effectivePredecessor == nil && input.PredecessorFlushReceiptID != uuid.Nil {
		return nil, newCycleMaterializationError(model.StudyErrorCodeSchemaInvalid, "study predecessor flush receipt has no predecessor", "predecessor_flush_receipt_id", "invalid_input", nil)
	}
	if effectivePredecessor != nil && !boundedSchedulerText(input.PredecessorTerminalReasonCode, 128) {
		return nil, newCycleMaterializationError(model.StudyErrorCodeSchemaInvalid, "study predecessor terminal reason is invalid", "predecessor_terminal_reason_code", "invalid_input", nil)
	}
	if len(captured.Sources) == 0 {
		persistenceInput.Lifecycle = model.StudySessionLifecycleWaitingForCorpus
		persisted, err := store.InsertCycleOnce(ctx, cloneCyclePersistenceInput(persistenceInput))
		if err != nil {
			return nil, err
		}
		if persisted == nil {
			return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "study cycle persistence returned no authority", "persistence", "missing_result", nil)
		}
		if err := validateCyclePersistenceCloneBounds(persisted, nil); err != nil {
			return nil, err
		}
		return reconstructMaterializedAuthority(
			cloneCyclePersistenceResult(*persisted),
			materializer.configuration,
			captured,
			input,
			nil,
			nil,
			nil,
		)
	}
	itemIDs, err := allocateMaterializationItemIDs(
		ctx,
		materializer.itemIDAllocator,
		len(captured.Sources),
	)
	if err != nil {
		return nil, err
	}
	predecessorTail, err := readResolvedPredecessorTail(
		ctx,
		store,
		materializer.configuration,
		effectivePredecessor,
		captured,
		input.PredecessorSessionID != nil,
	)
	if err != nil {
		return nil, err
	}

	acceptedPrefix, err := store.ReadAcceptedPrefix(ctx, captured.LearnerProfile.ProfileIngestionHighWater)
	if err != nil {
		return nil, err
	}
	resolvedEvents, err := resolveAcceptedMaterializationEvents(ctx, store, acceptedPrefix)
	if err != nil {
		return nil, err
	}
	projections, err := RebuildStudyCardProjections(StudyProjectionRebuildInput{
		LearnerProfileID:          captured.LearnerProfile.ID,
		SchedulerName:             materializer.configuration.SchedulerName,
		SchedulerVersion:          materializer.configuration.SchedulerVersion,
		ProjectionSchemaVersion:   materializer.configuration.ProjectionSchemaVersion,
		ProfileIngestionHighWater: captured.LearnerProfile.ProfileIngestionHighWater,
		Targets:                   materializationProjectionTargets(captured.Sources),
		AcceptedEvents:            resolvedEvents,
	})
	if err != nil {
		return nil, err
	}
	if err := replaceMaterializationProjections(ctx, store, materializer.configuration, projections); err != nil {
		return nil, err
	}

	snapshot, err := materializationSchedulingSnapshot(input, captured, projections, predecessorTail)
	if err != nil {
		return nil, err
	}

	var stagedPlan *StudyPlan
	var stagedSnapshot *StudySchedulingSnapshot
	plan, planErr := invokeAndValidateMaterializationScheduler(
		materializer.scheduler,
		materializer.configuration,
		snapshot,
	)
	if planErr != nil {
		return nil, planErr
	}
	persistenceInput.Lifecycle = model.StudySessionLifecycleActive
	persistenceInput.Scheduler = cloneCycleSchedulerProvenancePointer(&plan.Scheduler)
	persistenceInput.Items, err = bindMaterializationItems(input.SessionID, itemIDs, captured, plan)
	if err != nil {
		return nil, err
	}
	planClone := cloneCyclePlan(plan)
	snapshotClone := cloneCycleSchedulingSnapshot(snapshot)
	stagedPlan = &planClone
	stagedSnapshot = &snapshotClone

	persisted, err := store.InsertCycleOnce(ctx, cloneCyclePersistenceInput(persistenceInput))
	if err != nil {
		return nil, err
	}
	if persisted == nil {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "study cycle persistence returned no authority", "persistence", "missing_result", nil)
	}
	if err := validateCyclePersistenceCloneBounds(persisted, nil); err != nil {
		return nil, err
	}
	return reconstructMaterializedAuthority(
		cloneCyclePersistenceResult(*persisted),
		materializer.configuration,
		captured,
		input,
		stagedSnapshot,
		stagedPlan,
		itemIDs,
	)
}

// ValidateCommittedCycleAuthority proves that a fresh committed snapshot
// contains the exact frozen cycle and creation receipt returned by
// Materialize. It permits only the documented mutable authority to advance:
// session lifecycle/high-water fields and each item's live source link. The
// returned snapshot is deeply detached from both caller-owned inputs.
func (materializer *CycleMaterializer) ValidateCommittedCycleAuthority(
	expected CyclePersistenceResult,
	committed CommittedCycle,
) (*CommittedCycle, error) {
	if materializer == nil || materializer.scheduler == nil || materializer.itemIDAllocator == nil {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "study cycle materializer is not constructed", "materializer", "invalid_configuration", nil)
	}
	committedPersistence := CyclePersistenceResult{
		Session: committed.Session,
		Items:   committed.Items,
		Receipt: committed.Receipt,
		Created: expected.Created,
	}
	if err := validateCyclePersistenceCloneBounds(&expected, nil); err != nil {
		return nil, err
	}
	if err := validateCyclePersistenceCloneBounds(&committedPersistence, nil); err != nil {
		return nil, err
	}
	if err := validateCommittedCycleReceiptAuthority(expected.Receipt, expected.Session); err != nil {
		return nil, err
	}
	if err := validateCommittedCycleReceiptAuthority(committed.Receipt, committed.Session); err != nil {
		return nil, err
	}

	_, expectedItems, err := reconstructPersistedCycleAuthority(
		cloneCyclePersistenceResult(expected),
		materializer.configuration,
	)
	if err != nil {
		return nil, err
	}
	_, committedItems, err := reconstructPersistedCycleAuthority(
		cloneCyclePersistenceResult(committedPersistence),
		materializer.configuration,
	)
	if err != nil {
		return nil, err
	}
	if !equalCycleCreationReceipts(expected.Receipt, committed.Receipt) {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "committed study creation receipt differs from materialized authority", "committed_cycle", "receipt_mismatch", nil)
	}
	if !equalCycleImmutableSessionAuthority(expected.Session, committed.Session) ||
		!sameCommittedSessionExactScope(expected.Session.Scope, committed.Session.Scope) {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "committed study session differs from materialized authority", "committed_cycle", "session_mismatch", nil)
	}
	if err := validateCommittedSessionMutableAdvance(expected.Session, committed.Session); err != nil {
		return nil, err
	}
	if !equalCycleImmutableSessionItems(expectedItems, committedItems) {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "committed study items differ from materialized authority", "committed_cycle", "items_mismatch", nil)
	}

	validated := &CommittedCycle{
		Session: cloneCycleSession(committed.Session),
		Items:   make([]model.StudySessionItem, len(committedItems)),
		Receipt: cloneCycleCreationReceipt(committed.Receipt),
	}
	for index := range committedItems {
		validated.Items[index] = cloneCycleSessionItem(committedItems[index])
	}
	return validated, nil
}

func validateCommittedCycleReceiptAuthority(
	receipt model.StudyCreationReceipt,
	session model.StudySession,
) error {
	if !receipt.StudyPrincipalID.Valid() || receipt.CreationRequestID == uuid.Nil ||
		!model.IsStudySHA256(receipt.NormalizedRequestSHA256) || !receipt.Disposition.Valid() ||
		receipt.CanonicalStudySessionID == uuid.Nil || receipt.LearnerProfileID == uuid.Nil ||
		receipt.CreationReceiptAdjudicatedAt.IsZero() ||
		receipt.CanonicalStudySessionID != session.ID || receipt.LearnerProfileID != session.LearnerProfileID ||
		!sameCycleScope(receipt.Scope, session.Scope) || !sameCommittedSessionExactScope(receipt.Scope, session.Scope) {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "committed study creation receipt authority is invalid", "committed_receipt", "invalid_authority", nil)
	}
	if err := validateCycleScopeAuthority(receipt.Scope, "committed_receipt"); err != nil {
		return err
	}
	return nil
}

func equalCycleCreationReceipts(left, right model.StudyCreationReceipt) bool {
	return left.StudyPrincipalID == right.StudyPrincipalID &&
		left.CreationRequestID == right.CreationRequestID &&
		left.NormalizedRequestSHA256 == right.NormalizedRequestSHA256 &&
		left.Disposition == right.Disposition &&
		left.CanonicalStudySessionID == right.CanonicalStudySessionID &&
		left.LearnerProfileID == right.LearnerProfileID &&
		sameCycleScope(left.Scope, right.Scope) && sameCommittedSessionExactScope(left.Scope, right.Scope) &&
		left.CreationReceiptAdjudicatedAt.Equal(right.CreationReceiptAdjudicatedAt)
}

func equalCycleImmutableSessionAuthority(left, right model.StudySession) bool {
	return left.ID == right.ID &&
		left.LearnerProfileID == right.LearnerProfileID &&
		sameCycleScope(left.Scope, right.Scope) &&
		left.CorpusSHA256 == right.CorpusSHA256 &&
		equalCycleSchedulerProvenance(left.Scheduler, right.Scheduler) &&
		sameCycleUUIDPointer(left.PredecessorStudySessionID, right.PredecessorStudySessionID) &&
		left.CreatedAt.Equal(right.CreatedAt)
}

func equalCycleSchedulerProvenance(
	left *model.StudySchedulerProvenance,
	right *model.StudySchedulerProvenance,
) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Name == right.Name && left.Version == right.Version &&
		left.ConfigurationSHA256 == right.ConfigurationSHA256 &&
		left.InputProfileIngestionHighWater == right.InputProfileIngestionHighWater &&
		left.PlannedAt.Equal(right.PlannedAt) && left.Seed == right.Seed
}

func equalCycleImmutableSessionItems(left, right []model.StudySessionItem) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		leftItem := left[index]
		rightItem := right[index]
		if leftItem.ID != rightItem.ID || leftItem.StudySessionID != rightItem.StudySessionID ||
			leftItem.Position != rightItem.Position || leftItem.CardKind != rightItem.CardKind ||
			leftItem.CardIdentity != rightItem.CardIdentity ||
			leftItem.HistoricalSourceKind != rightItem.HistoricalSourceKind ||
			leftItem.HistoricalSourceIdentity != rightItem.HistoricalSourceIdentity ||
			leftItem.SourceContentRevision != rightItem.SourceContentRevision ||
			leftItem.SourceContentSHA256 != rightItem.SourceContentSHA256 ||
			leftItem.FrontCanonicalName != rightItem.FrontCanonicalName ||
			leftItem.BackDefinition != rightItem.BackDefinition ||
			!sameCycleStringPointer(leftItem.BackExample, rightItem.BackExample) ||
			leftItem.SelectionReason.RatingRank != rightItem.SelectionReason.RatingRank ||
			!sameCycleInt64Pointer(leftItem.SelectionReason.LatestPresentedIngestionOrdinal, rightItem.SelectionReason.LatestPresentedIngestionOrdinal) ||
			leftItem.SelectionReason.TieBreakerSHA256 != rightItem.SelectionReason.TieBreakerSHA256 ||
			leftItem.SelectionReason.BoundaryAdjustment != rightItem.SelectionReason.BoundaryAdjustment ||
			!leftItem.CreatedAt.Equal(rightItem.CreatedAt) {
			return false
		}
	}
	return true
}

func validateCycleMaterializationInput(input CycleMaterializationInput) error {
	if input.SessionID == uuid.Nil || input.CreationRequestID == uuid.Nil || !input.Principal.Valid() {
		return newCycleMaterializationError(model.StudyErrorCodeSchemaInvalid, "study cycle materialization identities are invalid", "identity", "invalid_input", nil)
	}
	if !model.IsStudySHA256(input.NormalizedRequestSHA256) || input.PlannedAt.IsZero() || !boundedSchedulerText(input.Seed, 128) || !boundedSchedulerText(input.LifecycleReasonCode, 128) {
		return newCycleMaterializationError(model.StudyErrorCodeSchemaInvalid, "study cycle materialization provenance is invalid", "provenance", "invalid_input", nil)
	}
	if input.PredecessorSessionID != nil {
		if *input.PredecessorSessionID == uuid.Nil || *input.PredecessorSessionID == input.SessionID {
			return newCycleMaterializationError(model.StudyErrorCodeSchemaInvalid, "study cycle predecessor identity is invalid", "predecessor_session_id", "invalid_input", nil)
		}
		if !boundedSchedulerText(input.PredecessorTerminalReasonCode, 128) {
			return newCycleMaterializationError(model.StudyErrorCodeSchemaInvalid, "study predecessor terminal reason is invalid", "predecessor_terminal_reason_code", "invalid_input", nil)
		}
		if input.PredecessorFlushReceiptID == uuid.Nil {
			return newCycleMaterializationError(model.StudyErrorCodeSchemaInvalid, "explicit study predecessor requires a flush receipt", "predecessor_flush_receipt_id", "invalid_input", nil)
		}
	} else if input.PredecessorFlushReceiptID != uuid.Nil {
		return newCycleMaterializationError(model.StudyErrorCodeSchemaInvalid, "study predecessor flush receipt has no predecessor", "predecessor_flush_receipt_id", "invalid_input", nil)
	}
	if input.PredecessorTerminalReasonCode != "" && !boundedSchedulerText(input.PredecessorTerminalReasonCode, 128) {
		return newCycleMaterializationError(model.StudyErrorCodeSchemaInvalid, "study predecessor terminal reason is invalid", "predecessor_terminal_reason_code", "invalid_input", nil)
	}
	return nil
}

func allocateMaterializationItemIDs(
	ctx context.Context,
	allocator StudyItemIDAllocator,
	requestedCount int,
) (itemIDs []uuid.UUID, err error) {
	if allocator == nil || requestedCount < 1 {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "study item identity allocation boundary is invalid", "item_id_allocator", "invalid_configuration", nil)
	}
	if requestedCount > StudyMaximumSchedulingCards {
		return nil, newCycleMaterializationError(model.StudyErrorCodeRequestTooLarge, "study item identity request exceeds the work bound", "item_id_allocator", "work_bound_exceeded", nil)
	}

	completed := false
	defer func() {
		if recovered := recover(); recovered != nil || !completed {
			itemIDs = nil
			err = newCycleMaterializationError(model.StudyErrorCodeInternal, "study item identity allocator panicked", "item_id_allocator", "allocator_panic", nil)
		}
	}()
	allocated, allocationErr := allocator.AllocateStudyItemIDs(ctx, requestedCount)
	completed = true
	if allocationErr != nil {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "study item identity allocation failed", "item_id_allocator", "allocator_error", nil)
	}
	if allocated == nil {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "study item identity allocator returned no identities", "item_id_allocator", "missing_result", nil)
	}
	if len(allocated) > StudyMaximumSchedulingCards {
		return nil, newCycleMaterializationError(model.StudyErrorCodeRequestTooLarge, "allocated study item identities exceed the work bound", "item_id_allocator", "work_bound_exceeded", nil)
	}
	if len(allocated) != requestedCount {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "allocated study item identity count is inconsistent", "item_id_allocator", "count_mismatch", nil)
	}

	detached := append([]uuid.UUID(nil), allocated...)
	seen := make(map[uuid.UUID]struct{}, len(detached))
	for _, itemID := range detached {
		if itemID == uuid.Nil {
			return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "allocated study item identity is invalid", "item_id_allocator", "invalid_identity", nil)
		}
		if _, duplicate := seen[itemID]; duplicate {
			return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "allocated study item identity is duplicated", "item_id_allocator", "duplicate_identity", nil)
		}
		seen[itemID] = struct{}{}
	}
	return detached, nil
}

func validateCyclePersistenceCloneBounds(
	persistence *CyclePersistenceResult,
	waitingEvidence *WaitingReplacementEvidence,
) error {
	if persistence == nil {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "study cycle persistence returned no authority", "persistence", "missing_result", nil)
	}
	if len(persistence.Items) > StudyMaximumSchedulingCards {
		return newCycleMaterializationError(model.StudyErrorCodeRequestTooLarge, "persisted study cycle exceeds the work bound", "persistence_items", "work_bound_exceeded", nil)
	}
	for _, scope := range []model.StudyNormalizedScope{persistence.Session.Scope, persistence.Receipt.Scope} {
		if err := validateCycleRawScopeBounds(scope); err != nil {
			return err
		}
	}
	if waitingEvidence != nil && len(waitingEvidence.Items) != 0 {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "implicit waiting replacement contains predecessor items", "waiting_replacement", "invalid_authority", nil)
	}
	if waitingEvidence != nil {
		if err := validateCycleRawScopeBounds(waitingEvidence.Predecessor.Scope); err != nil {
			return err
		}
	}
	return nil
}

func validateCycleRawScopeBounds(scope model.StudyNormalizedScope) error {
	if len(scope.Scope.TopicIDs) > model.StudyMaximumTopics ||
		len(scope.Scope.TopicRefs) > model.StudyMaximumTopics ||
		len(scope.Scope.TopicIDs) != len(scope.Scope.TopicRefs) ||
		len(scope.CanonicalSelector) > StudyMaximumCanonicalSelectorBytes {
		return newCycleMaterializationError(model.StudyErrorCodeRequestTooLarge, "study scope authority exceeds the work bound", "scope", "work_bound_exceeded", nil)
	}
	return nil
}

func validateCycleScopeAuthority(scope model.StudyNormalizedScope, field string) error {
	if err := validateCycleRawScopeBounds(scope); err != nil {
		return err
	}
	normalized, err := model.NormalizeStudyScope(scope.Scope)
	if err != nil || normalized.CanonicalSelectorHash != scope.CanonicalSelectorHash ||
		!bytes.Equal(normalized.CanonicalSelector, scope.CanonicalSelector) ||
		!equalCycleUUIDs(normalized.Scope.TopicIDs, scope.Scope.TopicIDs) ||
		!equalCycleStrings(normalized.Scope.TopicRefs, scope.Scope.TopicRefs) {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "study scope authority is invalid", field, "invalid_authority", err)
	}
	return nil
}

func validateAndCloneCapturedEligibleCorpus(captured CapturedEligibleCorpus) (CapturedEligibleCorpus, error) {
	if err := validateCycleRawScopeBounds(captured.Scope); err != nil {
		return CapturedEligibleCorpus{}, err
	}
	if captured.NonterminalSession != nil {
		if err := validateCycleRawScopeBounds(captured.NonterminalSession.Scope); err != nil {
			return CapturedEligibleCorpus{}, err
		}
	}
	if captured.LearnerProfile.ID == uuid.Nil || captured.LearnerProfile.ProfileIngestionHighWater < 0 || !model.IsStudySHA256(captured.CorpusSHA256) {
		return CapturedEligibleCorpus{}, newCycleMaterializationError(model.StudyErrorCodeInternal, "captured study corpus authority is invalid", "captured_corpus", "invalid_authority", nil)
	}
	if err := validateCycleScopeAuthority(captured.Scope, "captured_scope"); err != nil {
		return CapturedEligibleCorpus{}, err
	}
	if len(captured.Sources) > StudyMaximumSchedulingCards {
		return CapturedEligibleCorpus{}, newCycleMaterializationError(model.StudyErrorCodeRequestTooLarge, "captured study corpus exceeds the work bound", "captured_sources", "work_bound_exceeded", nil)
	}
	clone := CapturedEligibleCorpus{
		LearnerProfile: captured.LearnerProfile,
		Scope:          cloneCycleNormalizedScope(captured.Scope),
		CorpusSHA256:   captured.CorpusSHA256,
		Sources:        make([]MaterializationSource, len(captured.Sources)),
	}
	if captured.NonterminalSession != nil {
		session := cloneCycleSession(*captured.NonterminalSession)
		if err := validateCycleScopeAuthority(session.Scope, "nonterminal_session"); err != nil {
			return CapturedEligibleCorpus{}, err
		}
		if session.LearnerProfileID != captured.LearnerProfile.ID || !sameCycleScope(session.Scope, captured.Scope) ||
			session.ProfileIngestionHighWater != captured.LearnerProfile.ProfileIngestionHighWater ||
			(session.Lifecycle != model.StudySessionLifecycleActive && session.Lifecycle != model.StudySessionLifecycleWaitingForCorpus) {
			return CapturedEligibleCorpus{}, newCycleMaterializationError(model.StudyErrorCodeInternal, "captured nonterminal session is inconsistent", "nonterminal_session", "invalid_authority", nil)
		}
		clone.NonterminalSession = &session
	}
	seen := make(map[uuid.UUID]struct{}, len(captured.Sources))
	for index, source := range captured.Sources {
		if source.CardIdentity == uuid.Nil || source.LiveSourceIdeaID != source.CardIdentity ||
			source.HistoricalSourceIdentity != source.CardIdentity || source.HistoricalSourceKind != string(model.StudyCardKindIdea) ||
			!boundedSchedulerText(source.SourceContentRevision, 128) || !model.IsStudySHA256(source.SourceContentSHA256) ||
			!boundedSchedulerText(source.FrontCanonicalName, 512) || !boundedSchedulerText(source.BackDefinition, 16384) ||
			(source.BackExample != nil && !boundedSchedulerText(*source.BackExample, 16384)) {
			return CapturedEligibleCorpus{}, newCycleMaterializationError(model.StudyErrorCodeInternal, "captured study source is invalid", "captured_source", "invalid_authority", nil)
		}
		if _, duplicate := seen[source.CardIdentity]; duplicate {
			return CapturedEligibleCorpus{}, newCycleMaterializationError(model.StudyErrorCodeInternal, "captured study corpus repeats a source", "captured_source", "duplicate_identity", nil)
		}
		exampleMarker := []byte{0}
		exampleValue := []byte(nil)
		if source.BackExample != nil {
			exampleMarker[0] = 1
			exampleValue = []byte(*source.BackExample)
		}
		contentSHA256, err := studydigest.CardContent(
			[]byte(captured.Scope.Scope.CardKind),
			[]byte(source.CardIdentity.String()),
			[]byte(source.HistoricalSourceIdentity.String()),
			[]byte(source.FrontCanonicalName),
			[]byte(source.BackDefinition),
			exampleMarker,
			exampleValue,
		)
		if err != nil {
			return CapturedEligibleCorpus{}, newCycleMaterializationError(model.StudyErrorCodeInternal, "captured study source content could not be verified", "captured_source", "digest_failed", err)
		}
		if contentSHA256 != source.SourceContentSHA256 {
			return CapturedEligibleCorpus{}, newCycleMaterializationError(model.StudyErrorCodeInternal, "captured study source content digest is inconsistent", "captured_source", "content_digest_mismatch", nil)
		}
		seen[source.CardIdentity] = struct{}{}
		clone.Sources[index] = cloneMaterializationSource(source)
	}
	sort.Slice(clone.Sources, func(left, right int) bool {
		return clone.Sources[left].CardIdentity.String() < clone.Sources[right].CardIdentity.String()
	})
	computedCorpusSHA256, err := materializationCorpusSHA256(clone.Scope, clone.Sources)
	if err != nil {
		return CapturedEligibleCorpus{}, err
	}
	if computedCorpusSHA256 != clone.CorpusSHA256 {
		return CapturedEligibleCorpus{}, newCycleMaterializationError(model.StudyErrorCodeInternal, "captured study corpus digest is inconsistent", "captured_corpus", "digest_mismatch", nil)
	}
	return clone, nil
}

func resolveAcceptedMaterializationEvents(
	ctx context.Context,
	store CycleMaterializationStore,
	events []model.StudyEvent,
) ([]StudyResolvedAcceptedEvent, error) {
	if len(events) > StudyMaximumSchedulingCards*4 {
		return nil, newCycleMaterializationError(model.StudyErrorCodeRequestTooLarge, "accepted study prefix exceeds the materialization work bound", "accepted_prefix", "work_bound_exceeded", nil)
	}
	resolved := make([]StudyResolvedAcceptedEvent, len(events))
	cache := make(map[uuid.UUID]uuid.UUID)
	for index, event := range events {
		identity, exists := cache[event.CardIdentity]
		if !exists {
			var err error
			identity, err = store.ResolveCardIdentity(ctx, event.CardIdentity)
			if err != nil {
				return nil, err
			}
			if identity == uuid.Nil {
				return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "accepted study event has no current card identity", "resolved_card_identity", "invalid_authority", nil)
			}
			cache[event.CardIdentity] = identity
		}
		resolved[index] = StudyResolvedAcceptedEvent{Event: cloneCycleStudyEvent(event), CurrentCardIdentity: identity}
	}
	return resolved, nil
}

func materializationProjectionTargets(sources []MaterializationSource) []StudyProjectionTarget {
	targets := make([]StudyProjectionTarget, len(sources))
	for index, source := range sources {
		liveSource := source.LiveSourceIdeaID
		targets[index] = StudyProjectionTarget{CurrentCardIdentity: source.CardIdentity, LiveSourceIdeaID: &liveSource}
	}
	return targets
}

func replaceMaterializationProjections(
	ctx context.Context,
	store CycleMaterializationStore,
	configuration cycleMaterializerConfiguration,
	projections []model.StudySchedulerCardProjection,
) error {
	for _, projection := range projections {
		key := MaterializationProjectionKey{
			LearnerProfileID:        projection.LearnerProfileID,
			CardIdentity:            projection.ProjectedCardIdentity,
			SchedulerName:           configuration.SchedulerName,
			SchedulerVersion:        configuration.SchedulerVersion,
			ProjectionSchemaVersion: configuration.ProjectionSchemaVersion,
		}
		existing, err := store.GetProjection(ctx, key)
		if err != nil {
			return err
		}
		expectedRevision := int64(0)
		if existing != nil {
			expectedRevision = existing.ProjectionRevision
		}
		replaced, err := store.ReplaceProjection(ctx, cloneCycleProjection(projection), expectedRevision)
		if err != nil {
			return err
		}
		if replaced == nil || replaced.CanonicalSHA256 != projection.CanonicalSHA256 ||
			!bytes.Equal(replaced.CanonicalPayload, projection.CanonicalPayload) || replaced.ProjectionRevision != expectedRevision+1 {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "study projection replacement returned inconsistent authority", "projection", "invalid_authority", nil)
		}
	}
	return nil
}

func effectiveMaterializationPredecessor(
	requested *uuid.UUID,
	captured CapturedEligibleCorpus,
) *uuid.UUID {
	if requested != nil {
		return cloneCycleUUIDPointer(requested)
	}
	if captured.NonterminalSession != nil &&
		captured.NonterminalSession.Lifecycle == model.StudySessionLifecycleWaitingForCorpus &&
		len(captured.Sources) > 0 {
		predecessor := captured.NonterminalSession.ID
		return &predecessor
	}
	return nil
}

func validateCapturedMaterializationPath(
	input CycleMaterializationInput,
	captured CapturedEligibleCorpus,
) error {
	nonterminal := captured.NonterminalSession
	if input.PredecessorSessionID == nil {
		if nonterminal == nil || nonterminal.Lifecycle == model.StudySessionLifecycleWaitingForCorpus {
			return nil
		}
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "active study cycle was not resolved before mutable materialization", "nonterminal_session", "unresolved_preflight", nil)
	}
	if nonterminal == nil || nonterminal.ID != *input.PredecessorSessionID {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "explicit study predecessor is not the locked nonterminal authority", "nonterminal_session", "invalid_authority", nil)
	}
	if nonterminal.Lifecycle == model.StudySessionLifecycleWaitingForCorpus {
		return newCycleMaterializationError(model.StudyErrorCodeSchemaInvalid, "waiting study sentinel cannot be named as an explicit successor predecessor", "predecessor_session", "invalid_input", nil)
	}
	if nonterminal.Lifecycle != model.StudySessionLifecycleActive {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "explicit study predecessor lifecycle is invalid", "nonterminal_session", "invalid_authority", nil)
	}
	return nil
}

func readResolvedPredecessorTail(
	ctx context.Context,
	store CycleMaterializationStore,
	configuration cycleMaterializerConfiguration,
	predecessorID *uuid.UUID,
	captured CapturedEligibleCorpus,
	explicit bool,
) (*uuid.UUID, error) {
	if predecessorID == nil {
		return nil, nil
	}
	if captured.NonterminalSession == nil || captured.NonterminalSession.ID != *predecessorID ||
		captured.NonterminalSession.LearnerProfileID != captured.LearnerProfile.ID ||
		!sameCycleScope(captured.NonterminalSession.Scope, captured.Scope) {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "study predecessor is not the locked nonterminal authority", "predecessor_session", "invalid_authority", nil)
	}
	predecessor := cloneCycleSession(*captured.NonterminalSession)
	expectedLifecycle := model.StudySessionLifecycleWaitingForCorpus
	if explicit {
		expectedLifecycle = model.StudySessionLifecycleActive
	}
	if predecessor.Lifecycle != expectedLifecycle {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "study predecessor lifecycle does not match the materialization path", "predecessor_session", "invalid_authority", nil)
	}
	items, err := store.ListSessionItems(ctx, *predecessorID)
	if err != nil {
		return nil, err
	}
	if len(items) > StudyMaximumSchedulingCards {
		return nil, newCycleMaterializationError(model.StudyErrorCodeRequestTooLarge, "study predecessor items exceed the work bound", "predecessor_items", "work_bound_exceeded", nil)
	}
	_, verifiedItems, err := reconstructPersistedCycleAuthority(CyclePersistenceResult{
		Session: predecessor,
		Items:   items,
	}, configuration)
	if err != nil {
		return nil, err
	}
	if predecessor.Lifecycle == model.StudySessionLifecycleWaitingForCorpus {
		if len(verifiedItems) != 0 {
			return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "waiting study predecessor contains issued items", "predecessor_items", "invalid_authority", nil)
		}
		return nil, nil
	}
	if len(verifiedItems) == 0 {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "active study predecessor has no issued item tail", "predecessor_items", "invalid_authority", nil)
	}
	resolved, err := store.ResolveCardIdentity(ctx, verifiedItems[len(verifiedItems)-1].CardIdentity)
	if err != nil {
		return nil, err
	}
	if resolved == uuid.Nil {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "study predecessor tail has no current identity", "predecessor_tail", "invalid_authority", nil)
	}
	return &resolved, nil
}

func materializationSchedulingSnapshot(
	input CycleMaterializationInput,
	captured CapturedEligibleCorpus,
	projections []model.StudySchedulerCardProjection,
	predecessorTail *uuid.UUID,
) (StudySchedulingSnapshot, error) {
	projectionByIdentity := make(map[uuid.UUID]model.StudySchedulerCardProjection, len(projections))
	for _, projection := range projections {
		projectionByIdentity[projection.ProjectedCardIdentity] = projection
	}
	snapshot := StudySchedulingSnapshot{
		LearnerProfileID:               captured.LearnerProfile.ID,
		CardKind:                       captured.Scope.Scope.CardKind,
		CorpusSHA256:                   captured.CorpusSHA256,
		InputProfileIngestionHighWater: captured.LearnerProfile.ProfileIngestionHighWater,
		PlannedAt:                      input.PlannedAt.UTC(),
		Seed:                           input.Seed,
		Cards:                          make([]StudySchedulingCard, len(captured.Sources)),
		PredecessorTailCardIdentity:    cloneCycleUUIDPointer(predecessorTail),
	}
	for index, source := range captured.Sources {
		projection, exists := projectionByIdentity[source.CardIdentity]
		if !exists {
			return StudySchedulingSnapshot{}, newCycleMaterializationError(model.StudyErrorCodeInternal, "rebuilt study projection is missing an eligible card", "projection", "missing_identity", nil)
		}
		snapshot.Cards[index] = StudySchedulingCard{
			CardIdentity: source.CardIdentity,
			Projection: StudyProjectionTail{
				LatestEffectiveRating:           cloneCycleRatingPointer(projection.LatestEffectiveRating),
				LatestPresentedIngestionOrdinal: cloneCycleInt64Pointer(projection.LatestPresentedIngestionOrdinal),
			},
		}
	}
	return snapshot, nil
}

func invokeAndValidateMaterializationScheduler(
	scheduler StudyScheduler,
	configuration cycleMaterializerConfiguration,
	snapshot StudySchedulingSnapshot,
) (plan StudyPlan, err error) {
	privateSnapshot := cloneCycleSchedulingSnapshot(snapshot)
	completed := false
	defer func() {
		if recovered := recover(); recovered != nil || !completed {
			plan = StudyPlan{}
			err = newCycleMaterializationError(model.StudyErrorCodeInternal, "study scheduler panicked", "scheduler", "scheduler_panic", nil)
		}
	}()
	plan, err = scheduler.Plan(privateSnapshot)
	completed = true
	if err != nil {
		return StudyPlan{}, newCycleMaterializationError(
			model.StudyErrorCodeInternal,
			"study scheduler failed",
			"scheduler",
			"scheduler_error",
			nil,
		)
	}
	if err := validateMaterializationPlan(configuration, snapshot, plan); err != nil {
		return StudyPlan{}, err
	}
	return cloneCyclePlan(plan), nil
}

func validateMaterializationPlan(
	configuration cycleMaterializerConfiguration,
	snapshot StudySchedulingSnapshot,
	plan StudyPlan,
) error {
	if len(plan.Cards) > StudyMaximumSchedulingCards {
		return newCycleMaterializationError(model.StudyErrorCodeRequestTooLarge, "study scheduler output exceeds the work bound", "scheduler_cards", "work_bound_exceeded", nil)
	}
	if plan.Scheduler.Name != configuration.SchedulerName ||
		plan.Scheduler.Version != configuration.SchedulerVersion ||
		plan.Scheduler.ConfigurationSHA256 != configuration.SchedulerConfigurationSHA256 ||
		plan.Scheduler.InputProfileIngestionHighWater != snapshot.InputProfileIngestionHighWater ||
		!plan.Scheduler.PlannedAt.Equal(snapshot.PlannedAt) ||
		plan.Scheduler.Seed != snapshot.Seed || plan.CorpusSHA256 != snapshot.CorpusSHA256 {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "study scheduler provenance does not match its authoritative input", "scheduler_provenance", "invalid_scheduler_output", nil)
	}
	if len(plan.Cards) != len(snapshot.Cards) {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "study scheduler output does not cover the captured corpus", "scheduler_cards", "count_mismatch", nil)
	}
	snapshotCards := make(map[uuid.UUID]StudySchedulingCard, len(snapshot.Cards))
	for _, card := range snapshot.Cards {
		snapshotCards[card.CardIdentity] = card
	}
	seen := make(map[uuid.UUID]struct{}, len(plan.Cards))
	for index, planned := range plan.Cards {
		inputCard, exists := snapshotCards[planned.CardIdentity]
		if !exists {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "study scheduler output contains an unknown card", "scheduler_cards", "identity_mismatch", nil)
		}
		if _, duplicate := seen[planned.CardIdentity]; duplicate {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "study scheduler output repeats a card", "scheduler_cards", "duplicate_identity", nil)
		}
		seen[planned.CardIdentity] = struct{}{}
		reason := planned.SelectionReason
		if !reason.RatingRank.Valid() || !reason.BoundaryAdjustment.Valid() ||
			reason.RatingRank != ratingRank(inputCard.Projection.LatestEffectiveRating) ||
			!sameCycleInt64Pointer(reason.LatestPresentedIngestionOrdinal, inputCard.Projection.LatestPresentedIngestionOrdinal) {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "study scheduler selection reason is inconsistent", "selection_reason", "invalid_scheduler_output", nil)
		}
		expectedTie, err := studydigest.Tie(
			[]byte(configuration.SchedulerName),
			[]byte(configuration.SchedulerVersion),
			[]byte(configuration.SchedulerConfigurationSHA256),
			[]byte(snapshot.Seed),
			[]byte(snapshot.CardKind),
			[]byte(planned.CardIdentity.String()),
		)
		if err != nil {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "study scheduler selection identity could not be verified", "selection_reason", "digest_failed", err)
		}
		if reason.TieBreakerSHA256 != expectedTie {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "study scheduler tie provenance is inconsistent", "selection_reason", "invalid_scheduler_output", nil)
		}
		if reason.BoundaryAdjustment != model.StudyBoundaryAdjustmentNone && index != 0 {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "study scheduler boundary provenance is misplaced", "selection_reason", "invalid_scheduler_output", nil)
		}
	}
	canonicalScheduler, err := NewVarietyFirstV1()
	if err != nil {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "canonical study scheduler is unavailable", "scheduler", "canonical_scheduler_unavailable", err)
	}
	canonicalPlan, err := canonicalScheduler.Plan(cloneCycleSchedulingSnapshot(snapshot))
	if err != nil {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "canonical study scheduler rejected an authoritative snapshot", "scheduler", "canonical_scheduler_failed", err)
	}
	if !equalCyclePlans(plan, canonicalPlan) {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "study scheduler output differs from canonical variety-first order", "scheduler_cards", "noncanonical_scheduler_output", nil)
	}
	return nil
}

func equalCyclePlans(left, right StudyPlan) bool {
	if left.Scheduler.Name != right.Scheduler.Name || left.Scheduler.Version != right.Scheduler.Version ||
		left.Scheduler.ConfigurationSHA256 != right.Scheduler.ConfigurationSHA256 ||
		left.Scheduler.InputProfileIngestionHighWater != right.Scheduler.InputProfileIngestionHighWater ||
		!left.Scheduler.PlannedAt.Equal(right.Scheduler.PlannedAt) || left.Scheduler.Seed != right.Scheduler.Seed ||
		left.CorpusSHA256 != right.CorpusSHA256 || len(left.Cards) != len(right.Cards) {
		return false
	}
	for index := range left.Cards {
		leftCard := left.Cards[index]
		rightCard := right.Cards[index]
		if leftCard.CardIdentity != rightCard.CardIdentity ||
			leftCard.SelectionReason.RatingRank != rightCard.SelectionReason.RatingRank ||
			!sameCycleInt64Pointer(leftCard.SelectionReason.LatestPresentedIngestionOrdinal, rightCard.SelectionReason.LatestPresentedIngestionOrdinal) ||
			leftCard.SelectionReason.TieBreakerSHA256 != rightCard.SelectionReason.TieBreakerSHA256 ||
			leftCard.SelectionReason.BoundaryAdjustment != rightCard.SelectionReason.BoundaryAdjustment {
			return false
		}
	}
	return true
}

func bindMaterializationItems(
	sessionID uuid.UUID,
	itemIDs []uuid.UUID,
	captured CapturedEligibleCorpus,
	plan StudyPlan,
) ([]model.StudySessionItem, error) {
	if sessionID == uuid.Nil || len(itemIDs) != len(captured.Sources) || len(itemIDs) != len(plan.Cards) {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "study item identities do not match the staged plan", "item_ids", "count_mismatch", nil)
	}
	sources := make(map[uuid.UUID]MaterializationSource, len(captured.Sources))
	for _, source := range captured.Sources {
		sources[source.CardIdentity] = source
	}
	items := make([]model.StudySessionItem, len(plan.Cards))
	for index, planned := range plan.Cards {
		source, exists := sources[planned.CardIdentity]
		if !exists {
			return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "study scheduler card cannot be bound to a captured source", "scheduler_cards", "identity_mismatch", nil)
		}
		liveSource := source.LiveSourceIdeaID
		items[index] = model.StudySessionItem{
			ID:                       itemIDs[index],
			StudySessionID:           sessionID,
			Position:                 index,
			CardKind:                 captured.Scope.Scope.CardKind,
			CardIdentity:             source.CardIdentity,
			HistoricalSourceKind:     source.HistoricalSourceKind,
			HistoricalSourceIdentity: source.HistoricalSourceIdentity,
			LiveSourceIdeaID:         &liveSource,
			SourceContentRevision:    source.SourceContentRevision,
			SourceContentSHA256:      source.SourceContentSHA256,
			FrontCanonicalName:       source.FrontCanonicalName,
			BackDefinition:           source.BackDefinition,
			BackExample:              cloneCycleStringPointer(source.BackExample),
			SelectionReason:          cloneCycleSelectionReason(planned.SelectionReason),
		}
		if err := model.ValidateStudySessionItem(items[index]); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func reconstructMaterializedAuthority(
	persistence CyclePersistenceResult,
	configuration cycleMaterializerConfiguration,
	captured CapturedEligibleCorpus,
	input CycleMaterializationInput,
	stagedSnapshot *StudySchedulingSnapshot,
	stagedPlan *StudyPlan,
	stagedItemIDs []uuid.UUID,
) (*CycleMaterializationResult, error) {
	if err := validateCyclePersistenceEnvelope(persistence, captured, input); err != nil {
		return nil, err
	}
	result, items, err := reconstructPersistedCycleAuthority(persistence, configuration)
	if err != nil {
		return nil, err
	}
	if !persistence.Created {
		if !validCycleReceiptPredecessor(persistence, input.PredecessorSessionID, false) {
			return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "converged study cycle has different predecessor semantics", "persistence_session", "predecessor_mismatch", nil)
		}
		// Historical plans stay authoritative without being re-adjudicated
		// against today's redirects, projections, predecessor tail, or HWM.
		// The exact original scheduling snapshot is not durably available, so
		// a converged result is deliberately ineligible for shadow evaluation.
		return result, nil
	}

	switch persistence.Session.Lifecycle {
	case model.StudySessionLifecycleWaitingForCorpus:
		if persistence.Session.ID != input.SessionID || len(captured.Sources) != 0 ||
			persistence.Session.CorpusSHA256 != captured.CorpusSHA256 || stagedPlan != nil || stagedSnapshot != nil {
			return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "created waiting cycle differs from staged authority", "persistence", "staged_authority_mismatch", nil)
		}
		return result, nil
	case model.StudySessionLifecycleActive:
		if persistence.Session.ID != input.SessionID || persistence.Session.CorpusSHA256 != captured.CorpusSHA256 ||
			stagedSnapshot == nil || stagedPlan == nil || result.AuthoritativePlan == nil ||
			!equalCyclePlans(*result.AuthoritativePlan, *stagedPlan) {
			return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "created study cycle differs from staged authority", "persistence", "staged_authority_mismatch", nil)
		}
		if err := validateMaterializationPlan(configuration, *stagedSnapshot, *stagedPlan); err != nil {
			return nil, err
		}
		expectedItems, err := bindMaterializationItems(input.SessionID, stagedItemIDs, captured, *stagedPlan)
		if err != nil {
			return nil, err
		}
		if !equalCycleSessionItems(items, expectedItems) {
			return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "created study items differ from staged authority", "persistence_items", "staged_authority_mismatch", nil)
		}
		snapshotClone := cloneCycleSchedulingSnapshot(*stagedSnapshot)
		result.SchedulingSnapshot = &snapshotClone
		return result, nil
	default:
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "newly created study cycle has an invalid lifecycle", "persistence", "staged_authority_mismatch", nil)
	}
}

func reconstructExistingCycleAuthority(
	resolution ExistingCycleResolution,
	configuration cycleMaterializerConfiguration,
	input CycleMaterializationInput,
) (*CycleMaterializationResult, error) {
	persistence := resolution.Persistence
	if persistence.Created {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "existing study cycle was reported as newly created", "persistence", "invalid_authority", nil)
	}
	if err := validateCyclePersistenceRequestEnvelope(persistence, input); err != nil {
		return nil, err
	}
	implicitWaitingReplacement, err := validateWaitingReplacementEvidence(resolution, input)
	if err != nil {
		return nil, err
	}
	if !validCycleReceiptPredecessor(persistence, input.PredecessorSessionID, implicitWaitingReplacement) {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "resolved study creation receipt has different predecessor semantics", "persistence_session", "predecessor_mismatch", nil)
	}
	if err := validateCycleScopeAuthority(resolution.Scope, "persistence_session"); err != nil {
		return nil, err
	}
	if resolution.LearnerProfile.ID == uuid.Nil || resolution.LearnerProfile.ProfileIngestionHighWater < 0 ||
		persistence.Session.LearnerProfileID != resolution.LearnerProfile.ID ||
		persistence.Session.ProfileIngestionHighWater != resolution.LearnerProfile.ProfileIngestionHighWater ||
		!sameCycleScope(persistence.Session.Scope, resolution.Scope) {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "resolved study cycle does not match the locked profile and scope", "persistence_session", "authority_mismatch", nil)
	}
	result, _, err := reconstructPersistedCycleAuthority(persistence, configuration)
	return result, err
}

func validateWaitingReplacementEvidence(
	resolution ExistingCycleResolution,
	input CycleMaterializationInput,
) (bool, error) {
	evidence := resolution.ImplicitWaitingReplacement
	if evidence == nil {
		return false, nil
	}
	persistence := resolution.Persistence
	predecessor := evidence.Predecessor
	invalid := input.PredecessorSessionID != nil || input.PredecessorFlushReceiptID != uuid.Nil ||
		persistence.Receipt.Disposition != model.StudyCreationDispositionSuccessor ||
		persistence.Session.PredecessorStudySessionID == nil ||
		predecessor.ID == uuid.Nil || predecessor.ID != *persistence.Session.PredecessorStudySessionID ||
		predecessor.Lifecycle != model.StudySessionLifecycleSupersededEmpty ||
		predecessor.SuccessorStudySessionID == nil || *predecessor.SuccessorStudySessionID != persistence.Session.ID ||
		predecessor.LearnerProfileID != resolution.LearnerProfile.ID ||
		predecessor.LearnerProfileID != persistence.Session.LearnerProfileID ||
		predecessor.ProfileIngestionHighWater != resolution.LearnerProfile.ProfileIngestionHighWater ||
		!sameCycleScope(predecessor.Scope, resolution.Scope) || !sameCycleScope(predecessor.Scope, persistence.Session.Scope) ||
		predecessor.Scheduler != nil || len(evidence.Items) != 0 || predecessor.StateVersion < 2 ||
		predecessor.TerminalizedAt == nil || predecessor.TerminalProfileIngestionHighWater == nil ||
		predecessor.ProfileIngestionHighWater < 0 || *predecessor.TerminalProfileIngestionHighWater < 0 ||
		(predecessor.PredecessorStudySessionID != nil &&
			(*predecessor.PredecessorStudySessionID == uuid.Nil || *predecessor.PredecessorStudySessionID == predecessor.ID))
	if invalid {
		return false, newCycleMaterializationError(model.StudyErrorCodeInternal, "implicit waiting replacement evidence is invalid", "waiting_replacement", "invalid_authority", nil)
	}
	if err := validateCycleScopeAuthority(predecessor.Scope, "waiting_replacement"); err != nil {
		return false, err
	}
	if err := validateCycleSessionLifecycleAuthority(predecessor, len(evidence.Items)); err != nil {
		return false, err
	}
	emptyCorpusSHA256, err := materializationCorpusSHA256(predecessor.Scope, nil)
	if err != nil {
		return false, err
	}
	if predecessor.CorpusSHA256 != emptyCorpusSHA256 {
		return false, newCycleMaterializationError(model.StudyErrorCodeInternal, "implicit waiting replacement corpus is not empty", "waiting_replacement", "corpus_mismatch", nil)
	}
	return true, nil
}

func validCycleReceiptPredecessor(
	persistence CyclePersistenceResult,
	requested *uuid.UUID,
	implicitWaitingReplacement bool,
) bool {
	switch persistence.Receipt.Disposition {
	case model.StudyCreationDispositionCreated:
		return requested == nil && persistence.Session.PredecessorStudySessionID == nil && !implicitWaitingReplacement
	case model.StudyCreationDispositionConvergedToExisting:
		return requested == nil && !implicitWaitingReplacement
	case model.StudyCreationDispositionSuccessor:
		if persistence.Session.PredecessorStudySessionID == nil {
			return false
		}
		if requested != nil {
			return !implicitWaitingReplacement && *requested == *persistence.Session.PredecessorStudySessionID
		}
		return implicitWaitingReplacement
	default:
		return false
	}
}

func reconstructPersistedCycleAuthority(
	persistence CyclePersistenceResult,
	configuration cycleMaterializerConfiguration,
) (*CycleMaterializationResult, []model.StudySessionItem, error) {
	if len(persistence.Items) > StudyMaximumSchedulingCards {
		return nil, nil, newCycleMaterializationError(model.StudyErrorCodeRequestTooLarge, "persisted study cycle exceeds the work bound", "persistence_items", "work_bound_exceeded", nil)
	}
	if err := validateCycleRawScopeBounds(persistence.Session.Scope); err != nil {
		return nil, nil, err
	}
	if err := validateCycleRawScopeBounds(persistence.Receipt.Scope); err != nil {
		return nil, nil, err
	}
	if err := validateCycleScopeAuthority(persistence.Session.Scope, "persistence_session"); err != nil {
		return nil, nil, err
	}
	if err := validateCycleSessionLifecycleAuthority(persistence.Session, len(persistence.Items)); err != nil {
		return nil, nil, err
	}
	result := &CycleMaterializationResult{Persistence: cloneCyclePersistenceResult(persistence)}
	session := persistence.Session
	emptyLifecycle := session.Lifecycle == model.StudySessionLifecycleWaitingForCorpus ||
		session.Lifecycle == model.StudySessionLifecycleSupersededEmpty ||
		(session.Lifecycle == model.StudySessionLifecycleAbandoned && session.Scheduler == nil && len(persistence.Items) == 0)
	if emptyLifecycle {
		if session.Scheduler != nil || len(persistence.Items) != 0 {
			return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted empty cycle contains scheduler authority", "persistence", "invalid_authority", nil)
		}
		emptyCorpusSHA256, err := materializationCorpusSHA256(session.Scope, nil)
		if err != nil {
			return nil, nil, err
		}
		if session.CorpusSHA256 != emptyCorpusSHA256 {
			return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted empty cycle corpus is inconsistent", "persistence", "corpus_mismatch", nil)
		}
		return result, []model.StudySessionItem{}, nil
	}
	if session.Lifecycle != model.StudySessionLifecycleActive &&
		session.Lifecycle != model.StudySessionLifecycleCompleted &&
		session.Lifecycle != model.StudySessionLifecycleAbandoned {
		return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study cycle lifecycle is invalid", "persistence", "invalid_authority", nil)
	}
	if session.Scheduler == nil || len(persistence.Items) == 0 {
		return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study cycle authority is incomplete", "persistence", "invalid_authority", nil)
	}
	if session.Scheduler.Name != configuration.SchedulerName ||
		session.Scheduler.Version != configuration.SchedulerVersion ||
		session.Scheduler.ConfigurationSHA256 != configuration.SchedulerConfigurationSHA256 ||
		session.Scheduler.InputProfileIngestionHighWater < 0 || session.Scheduler.PlannedAt.IsZero() ||
		!boundedSchedulerText(session.Scheduler.Seed, 128) {
		return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study scheduler namespace is not the configured authority", "persistence_scheduler", "authority_mismatch", nil)
	}
	items := make([]model.StudySessionItem, len(persistence.Items))
	for index := range persistence.Items {
		items[index] = cloneCycleSessionItem(persistence.Items[index])
	}
	sort.Slice(items, func(left, right int) bool { return items[left].Position < items[right].Position })
	corpusSources := make([]MaterializationSource, len(items))
	seenItemIDs := make(map[uuid.UUID]struct{}, len(items))
	seenCardIdentities := make(map[uuid.UUID]struct{}, len(items))
	seenPresentedOrdinals := make(map[int64]struct{}, len(items))
	plan := StudyPlan{
		Scheduler:    *cloneCycleSchedulerProvenancePointer(session.Scheduler),
		CorpusSHA256: session.CorpusSHA256,
		Cards:        make([]StudyPlannedCard, len(items)),
	}
	for index, item := range items {
		if item.Position != index || item.StudySessionID != session.ID || item.CardKind != session.Scope.Scope.CardKind {
			return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study items are not a complete owned cycle", "persistence_items", "invalid_authority", nil)
		}
		if _, duplicate := seenItemIDs[item.ID]; duplicate {
			return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study cycle repeats an item identity", "persistence_items", "duplicate_identity", nil)
		}
		if _, duplicate := seenCardIdentities[item.CardIdentity]; duplicate {
			return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study cycle repeats a card identity", "persistence_items", "duplicate_card", nil)
		}
		seenItemIDs[item.ID] = struct{}{}
		seenCardIdentities[item.CardIdentity] = struct{}{}
		if err := model.ValidateStudySessionItem(item); err != nil {
			return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study item is invalid", "persistence_items", "invalid_authority", err)
		}
		exampleMarker := []byte{0}
		exampleValue := []byte(nil)
		if item.BackExample != nil {
			exampleMarker[0] = 1
			exampleValue = []byte(*item.BackExample)
		}
		contentSHA256, err := studydigest.CardContent(
			[]byte(item.CardKind),
			[]byte(item.CardIdentity.String()),
			[]byte(item.HistoricalSourceIdentity.String()),
			[]byte(item.FrontCanonicalName),
			[]byte(item.BackDefinition),
			exampleMarker,
			exampleValue,
		)
		if err != nil {
			return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study item content could not be verified", "persistence_items", "digest_failed", err)
		}
		if contentSHA256 != item.SourceContentSHA256 {
			return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study item content digest is inconsistent", "persistence_items", "content_digest_mismatch", nil)
		}
		if ordinal := item.SelectionReason.LatestPresentedIngestionOrdinal; ordinal != nil && *ordinal > session.Scheduler.InputProfileIngestionHighWater {
			return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study selection ordinal exceeds its scheduler high-water", "persistence_items", "invalid_authority", nil)
		} else if ordinal != nil {
			if _, duplicate := seenPresentedOrdinals[*ordinal]; duplicate {
				return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study cycle repeats a presented ordinal", "persistence_items", "duplicate_presented_ordinal", nil)
			}
			seenPresentedOrdinals[*ordinal] = struct{}{}
		}
		if item.SelectionReason.BoundaryAdjustment != model.StudyBoundaryAdjustmentNone && index != 0 {
			return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study boundary provenance is misplaced", "persistence_items", "invalid_authority", nil)
		}
		tieSHA256, err := studydigest.Tie(
			[]byte(session.Scheduler.Name),
			[]byte(session.Scheduler.Version),
			[]byte(session.Scheduler.ConfigurationSHA256),
			[]byte(session.Scheduler.Seed),
			[]byte(item.CardKind),
			[]byte(item.CardIdentity.String()),
		)
		if err != nil {
			return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study selection tie could not be verified", "persistence_items", "digest_failed", err)
		}
		if tieSHA256 != item.SelectionReason.TieBreakerSHA256 {
			return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study selection tie digest is inconsistent", "persistence_items", "tie_digest_mismatch", nil)
		}
		corpusSources[index] = MaterializationSource{
			CardIdentity:          item.CardIdentity,
			SourceContentRevision: item.SourceContentRevision,
			SourceContentSHA256:   item.SourceContentSHA256,
		}
		plan.Cards[index] = StudyPlannedCard{
			CardIdentity:    item.CardIdentity,
			SelectionReason: cloneCycleSelectionReason(item.SelectionReason),
		}
	}
	if err := validatePersistedMaterializationOrder(plan, session.PredecessorStudySessionID); err != nil {
		return nil, nil, err
	}
	persistedCorpusSHA256, err := materializationCorpusSHA256(session.Scope, corpusSources)
	if err != nil {
		return nil, nil, err
	}
	if session.CorpusSHA256 != persistedCorpusSHA256 {
		return nil, nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study cycle corpus is inconsistent", "persistence", "corpus_mismatch", nil)
	}
	planClone := cloneCyclePlan(plan)
	result.AuthoritativePlan = &planClone
	return result, items, nil
}

func validateCycleSessionLifecycleAuthority(session model.StudySession, itemCount int) error {
	invalidPointer := func(candidate *uuid.UUID) bool {
		return candidate != nil && (*candidate == uuid.Nil || *candidate == session.ID)
	}
	if session.ID == uuid.Nil || session.LearnerProfileID == uuid.Nil || session.ProfileIngestionHighWater < 0 ||
		session.StateVersion < 1 || !session.Lifecycle.Valid() || !model.IsStudySHA256(session.CorpusSHA256) ||
		invalidPointer(session.PredecessorStudySessionID) || invalidPointer(session.SuccessorStudySessionID) ||
		(session.PredecessorStudySessionID != nil && session.SuccessorStudySessionID != nil &&
			*session.PredecessorStudySessionID == *session.SuccessorStudySessionID) {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study session lifecycle authority is invalid", "persistence_session", "invalid_authority", nil)
	}
	terminal := session.Lifecycle.Terminal()
	if terminal {
		if session.StateVersion < 2 || session.TerminalizedAt == nil || session.TerminalizedAt.IsZero() ||
			session.TerminalProfileIngestionHighWater == nil || *session.TerminalProfileIngestionHighWater < 0 ||
			*session.TerminalProfileIngestionHighWater > session.ProfileIngestionHighWater {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted terminal study session shape is invalid", "persistence_session", "invalid_authority", nil)
		}
	} else if session.TerminalizedAt != nil || session.TerminalProfileIngestionHighWater != nil {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted nonterminal study session contains terminal authority", "persistence_session", "invalid_authority", nil)
	}
	if session.Scheduler != nil && (session.Scheduler.InputProfileIngestionHighWater < 0 ||
		session.Scheduler.InputProfileIngestionHighWater > session.ProfileIngestionHighWater) {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study scheduler high-water exceeds current profile authority", "persistence_scheduler", "invalid_authority", nil)
	}
	if terminal && session.Scheduler != nil &&
		*session.TerminalProfileIngestionHighWater < session.Scheduler.InputProfileIngestionHighWater {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted terminal study session predates its scheduler prefix", "persistence_scheduler", "invalid_authority", nil)
	}
	emptyCorpusSHA256, err := materializationCorpusSHA256(session.Scope, nil)
	if err != nil {
		return err
	}
	emptyShape := session.Scheduler == nil && itemCount == 0 && session.CorpusSHA256 == emptyCorpusSHA256
	scheduledShape := session.Scheduler != nil && itemCount > 0
	switch session.Lifecycle {
	case model.StudySessionLifecycleActive:
		if !scheduledShape || session.SuccessorStudySessionID != nil {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted active study session shape is invalid", "persistence_session", "invalid_authority", nil)
		}
	case model.StudySessionLifecycleWaitingForCorpus:
		if !emptyShape || session.SuccessorStudySessionID != nil {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted waiting study session shape is invalid", "persistence_session", "invalid_authority", nil)
		}
	case model.StudySessionLifecycleSupersededEmpty:
		if !emptyShape || session.SuccessorStudySessionID == nil {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted superseded-empty study session shape is invalid", "persistence_session", "invalid_authority", nil)
		}
	case model.StudySessionLifecycleCompleted:
		if !scheduledShape || session.SuccessorStudySessionID == nil {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted completed study session shape is invalid", "persistence_session", "invalid_authority", nil)
		}
	case model.StudySessionLifecycleAbandoned:
		if (!emptyShape && !scheduledShape) || session.SuccessorStudySessionID != nil {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted abandoned study session shape is invalid", "persistence_session", "invalid_authority", nil)
		}
	}
	return nil
}

func validatePersistedMaterializationOrder(plan StudyPlan, predecessorSessionID *uuid.UUID) error {
	if len(plan.Cards) == 0 {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study plan order is empty", "persistence_items", "invalid_authority", nil)
	}
	if predecessorSessionID == nil && plan.Cards[0].SelectionReason.BoundaryAdjustment != model.StudyBoundaryAdjustmentNone {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted root study plan contains successor-only boundary provenance", "persistence_items", "invalid_boundary_shape", nil)
	}
	base := make([]varietyScheduledCard, len(plan.Cards))
	for index, card := range plan.Cards {
		card = cloneCyclePlannedCard(card)
		card.SelectionReason.BoundaryAdjustment = model.StudyBoundaryAdjustmentNone
		base[index] = varietyScheduledCard{planned: card, rankOrder: varietyRankOrder(card.SelectionReason.RatingRank)}
	}
	sort.Slice(base, func(left, right int) bool { return varietyScheduledCardLess(base[left], base[right]) })
	leadingRankEnd := 1
	for leadingRankEnd < len(base) && base[leadingRankEnd].rankOrder == base[0].rankOrder {
		leadingRankEnd++
	}
	expected := make([]StudyPlannedCard, len(base))
	for index := range base {
		expected[index] = cloneCyclePlannedCard(base[index].planned)
	}
	switch plan.Cards[0].SelectionReason.BoundaryAdjustment {
	case model.StudyBoundaryAdjustmentNone:
	case model.StudyBoundaryAdjustmentPriorityBound:
		if leadingRankEnd != 1 {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted priority boundary has an avoidable same-rank alternative", "persistence_items", "invalid_boundary_shape", nil)
		}
	case model.StudyBoundaryAdjustmentRotated:
		if leadingRankEnd < 2 {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted rotated boundary has no same-rank alternative", "persistence_items", "invalid_boundary_shape", nil)
		}
		first := expected[0]
		copy(expected[0:leadingRankEnd-1], expected[1:leadingRankEnd])
		expected[leadingRankEnd-1] = first
	default:
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study boundary marker is invalid", "persistence_items", "invalid_boundary_shape", nil)
	}
	for index, card := range plan.Cards {
		if card.CardIdentity != expected[index].CardIdentity ||
			card.SelectionReason.RatingRank != expected[index].SelectionReason.RatingRank ||
			!sameCycleInt64Pointer(card.SelectionReason.LatestPresentedIngestionOrdinal, expected[index].SelectionReason.LatestPresentedIngestionOrdinal) ||
			card.SelectionReason.TieBreakerSHA256 != expected[index].SelectionReason.TieBreakerSHA256 ||
			(index > 0 && card.SelectionReason.BoundaryAdjustment != model.StudyBoundaryAdjustmentNone) {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study plan order is not a legal variety-first order", "persistence_items", "invalid_persisted_order", nil)
		}
	}
	return nil
}

func validateCyclePersistenceEnvelope(
	persistence CyclePersistenceResult,
	captured CapturedEligibleCorpus,
	input CycleMaterializationInput,
) error {
	if err := validateCyclePersistenceRequestEnvelope(persistence, input); err != nil {
		return err
	}
	session := persistence.Session
	if session.LearnerProfileID != captured.LearnerProfile.ID || !sameCycleScope(session.Scope, captured.Scope) ||
		session.Scope.Scope.CardKind != captured.Scope.Scope.CardKind ||
		session.ProfileIngestionHighWater != captured.LearnerProfile.ProfileIngestionHighWater {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study session does not match the locked authority", "persistence_session", "authority_mismatch", nil)
	}
	if persistence.Created {
		effectivePredecessor := effectiveMaterializationPredecessor(input.PredecessorSessionID, captured)
		if !sameCycleUUIDPointer(session.PredecessorStudySessionID, effectivePredecessor) {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "created study predecessor does not match the staged authority", "persistence_session", "predecessor_mismatch", nil)
		}
		expectedDisposition := model.StudyCreationDispositionCreated
		if effectivePredecessor != nil {
			expectedDisposition = model.StudyCreationDispositionSuccessor
		}
		if persistence.Receipt.Disposition != expectedDisposition {
			return newCycleMaterializationError(model.StudyErrorCodeInternal, "created study receipt disposition does not match its predecessor shape", "persistence_receipt", "invalid_authority", nil)
		}
	}
	return nil
}

func validateCyclePersistenceRequestEnvelope(
	persistence CyclePersistenceResult,
	input CycleMaterializationInput,
) error {
	session := persistence.Session
	receipt := persistence.Receipt
	if err := validateCycleScopeAuthority(session.Scope, "persistence_session"); err != nil {
		return err
	}
	if session.ID == uuid.Nil || session.LearnerProfileID == uuid.Nil || session.StateVersion < 1 ||
		!session.Lifecycle.Valid() || !model.IsStudySHA256(session.CorpusSHA256) {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study session authority is invalid", "persistence_session", "invalid_authority", nil)
	}
	if receipt.StudyPrincipalID != input.Principal || receipt.CreationRequestID != input.CreationRequestID ||
		receipt.NormalizedRequestSHA256 != input.NormalizedRequestSHA256 || receipt.CanonicalStudySessionID != session.ID ||
		receipt.LearnerProfileID != session.LearnerProfileID || !sameCycleScope(receipt.Scope, session.Scope) || !receipt.Disposition.Valid() {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study creation receipt does not match the request authority", "persistence_receipt", "authority_mismatch", nil)
	}
	if err := validateCycleScopeAuthority(receipt.Scope, "persistence_receipt"); err != nil {
		return err
	}
	return nil
}

func equalCycleSchedulingSnapshots(left, right StudySchedulingSnapshot) bool {
	if left.LearnerProfileID != right.LearnerProfileID || left.CardKind != right.CardKind || left.CorpusSHA256 != right.CorpusSHA256 ||
		left.InputProfileIngestionHighWater != right.InputProfileIngestionHighWater || !left.PlannedAt.Equal(right.PlannedAt) ||
		left.Seed != right.Seed || !sameCycleUUIDPointer(left.PredecessorTailCardIdentity, right.PredecessorTailCardIdentity) || len(left.Cards) != len(right.Cards) {
		return false
	}
	rightCards := make(map[uuid.UUID]StudyProjectionTail, len(right.Cards))
	for _, card := range right.Cards {
		rightCards[card.CardIdentity] = card.Projection
	}
	for _, leftCard := range left.Cards {
		rightProjection, exists := rightCards[leftCard.CardIdentity]
		if !exists ||
			!sameCycleRatingPointer(leftCard.Projection.LatestEffectiveRating, rightProjection.LatestEffectiveRating) ||
			!sameCycleInt64Pointer(leftCard.Projection.LatestPresentedIngestionOrdinal, rightProjection.LatestPresentedIngestionOrdinal) {
			return false
		}
	}
	return true
}

func equalCycleSessionItems(left, right []model.StudySessionItem) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		leftItem := left[index]
		rightItem := right[index]
		if leftItem.ID != rightItem.ID || leftItem.StudySessionID != rightItem.StudySessionID || leftItem.Position != rightItem.Position ||
			leftItem.CardKind != rightItem.CardKind || leftItem.CardIdentity != rightItem.CardIdentity ||
			leftItem.HistoricalSourceKind != rightItem.HistoricalSourceKind || leftItem.HistoricalSourceIdentity != rightItem.HistoricalSourceIdentity ||
			!sameCycleUUIDPointer(leftItem.LiveSourceIdeaID, rightItem.LiveSourceIdeaID) || leftItem.SourceContentRevision != rightItem.SourceContentRevision ||
			leftItem.SourceContentSHA256 != rightItem.SourceContentSHA256 || leftItem.FrontCanonicalName != rightItem.FrontCanonicalName ||
			leftItem.BackDefinition != rightItem.BackDefinition || !sameCycleStringPointer(leftItem.BackExample, rightItem.BackExample) ||
			leftItem.SelectionReason.RatingRank != rightItem.SelectionReason.RatingRank ||
			!sameCycleInt64Pointer(leftItem.SelectionReason.LatestPresentedIngestionOrdinal, rightItem.SelectionReason.LatestPresentedIngestionOrdinal) ||
			leftItem.SelectionReason.TieBreakerSHA256 != rightItem.SelectionReason.TieBreakerSHA256 ||
			leftItem.SelectionReason.BoundaryAdjustment != rightItem.SelectionReason.BoundaryAdjustment {
			return false
		}
	}
	return true
}

func ratingForMaterializedRank(rank model.StudyRatingRank) (*model.StudyRating, error) {
	var rating model.StudyRating
	switch rank {
	case model.StudyRatingRankUnseenUnrated:
		return nil, nil
	case model.StudyRatingRankAgain:
		rating = model.StudyRatingAgain
	case model.StudyRatingRankHard:
		rating = model.StudyRatingHard
	case model.StudyRatingRankGood:
		rating = model.StudyRatingGood
	case model.StudyRatingRankEasy:
		rating = model.StudyRatingEasy
	default:
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "persisted study rating rank is invalid", "selection_reason", "invalid_authority", nil)
	}
	return &rating, nil
}

func sameCycleScope(left, right model.StudyNormalizedScope) bool {
	return left.CanonicalSelectorHash == right.CanonicalSelectorHash &&
		bytes.Equal(left.CanonicalSelector, right.CanonicalSelector) && left.Scope.CardKind == right.Scope.CardKind &&
		equalCycleUUIDs(left.Scope.TopicIDs, right.Scope.TopicIDs)
}

func equalCycleUUIDs(left, right []uuid.UUID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalCycleStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func materializationCorpusSHA256(
	scope model.StudyNormalizedScope,
	sources []MaterializationSource,
) (string, error) {
	canonicalSources := append([]MaterializationSource(nil), sources...)
	sort.Slice(canonicalSources, func(left, right int) bool {
		return canonicalSources[left].CardIdentity.String() < canonicalSources[right].CardIdentity.String()
	})
	parts := make([][]byte, 0, 3+len(scope.Scope.TopicIDs)+len(canonicalSources)*4)
	parts = append(parts, cycleDigestCount(len(scope.Scope.TopicIDs)))
	for _, topicID := range scope.Scope.TopicIDs {
		parts = append(parts, []byte(topicID.String()))
	}
	parts = append(parts, []byte(scope.Scope.CardKind), cycleDigestCount(len(canonicalSources)))
	for _, source := range canonicalSources {
		parts = append(parts,
			[]byte(source.CardIdentity.String()),
			[]byte(source.CardIdentity.String()),
			[]byte(source.SourceContentRevision),
			[]byte(source.SourceContentSHA256),
		)
	}
	digest, err := studydigest.Corpus(parts...)
	if err != nil {
		return "", newCycleMaterializationError(model.StudyErrorCodeInternal, "study corpus digest could not be verified", "corpus_sha256", "digest_failed", err)
	}
	return digest, nil
}

func cycleDigestCount(count int) []byte {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], uint32(count))
	return encoded[:]
}

func sameCycleInt64Pointer(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameCycleUUIDPointer(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameCycleRatingPointer(left, right *model.StudyRating) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameCycleStringPointer(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func cloneMaterializationSource(source MaterializationSource) MaterializationSource {
	clone := source
	clone.BackExample = cloneCycleStringPointer(source.BackExample)
	return clone
}

func cloneCyclePersistenceInput(input CyclePersistenceInput) CyclePersistenceInput {
	clone := input
	clone.Scheduler = cloneCycleSchedulerProvenancePointer(input.Scheduler)
	clone.PredecessorSessionID = cloneCycleUUIDPointer(input.PredecessorSessionID)
	clone.Items = make([]model.StudySessionItem, len(input.Items))
	for index := range input.Items {
		clone.Items[index] = cloneCycleSessionItem(input.Items[index])
	}
	return clone
}

func cloneCyclePersistenceResult(result CyclePersistenceResult) CyclePersistenceResult {
	clone := result
	clone.Session = cloneCycleSession(result.Session)
	clone.Items = make([]model.StudySessionItem, len(result.Items))
	for index := range result.Items {
		clone.Items[index] = cloneCycleSessionItem(result.Items[index])
	}
	clone.Receipt = cloneCycleCreationReceipt(result.Receipt)
	return clone
}

func cloneCycleCreationReceipt(receipt model.StudyCreationReceipt) model.StudyCreationReceipt {
	clone := receipt
	clone.Scope = cloneCycleNormalizedScope(receipt.Scope)
	return clone
}

func cloneWaitingReplacementEvidence(evidence *WaitingReplacementEvidence) *WaitingReplacementEvidence {
	if evidence == nil {
		return nil
	}
	clone := &WaitingReplacementEvidence{
		Predecessor: cloneCycleSession(evidence.Predecessor),
		Items:       make([]model.StudySessionItem, len(evidence.Items)),
	}
	for index := range evidence.Items {
		clone.Items[index] = cloneCycleSessionItem(evidence.Items[index])
	}
	return clone
}

func cloneCycleSession(session model.StudySession) model.StudySession {
	clone := session
	clone.Scope = cloneCycleNormalizedScope(session.Scope)
	clone.Scheduler = cloneCycleSchedulerProvenancePointer(session.Scheduler)
	clone.PredecessorStudySessionID = cloneCycleUUIDPointer(session.PredecessorStudySessionID)
	clone.SuccessorStudySessionID = cloneCycleUUIDPointer(session.SuccessorStudySessionID)
	clone.TerminalizedAt = cloneCycleTimePointer(session.TerminalizedAt)
	clone.TerminalProfileIngestionHighWater = cloneCycleInt64Pointer(session.TerminalProfileIngestionHighWater)
	return clone
}

func cloneCycleSessionItem(item model.StudySessionItem) model.StudySessionItem {
	clone := item
	clone.LiveSourceIdeaID = cloneCycleUUIDPointer(item.LiveSourceIdeaID)
	clone.BackExample = cloneCycleStringPointer(item.BackExample)
	clone.SelectionReason = cloneCycleSelectionReason(item.SelectionReason)
	return clone
}

func cloneCycleStudyEvent(event model.StudyEvent) model.StudyEvent {
	clone := event
	clone.Rating = cloneCycleRatingPointer(event.Rating)
	clone.RatingEventID = cloneCycleUUIDPointer(event.RatingEventID)
	clone.ClientObservedAt = cloneCycleTimePointer(event.ClientObservedAt)
	clone.ProfileIngestionOrdinal = cloneCycleInt64Pointer(event.ProfileIngestionOrdinal)
	clone.ReceivedAt = cloneCycleTimePointer(event.ReceivedAt)
	return clone
}

func cloneCycleProjection(projection model.StudySchedulerCardProjection) model.StudySchedulerCardProjection {
	clone := projection
	clone.LiveSourceIdeaID = cloneCycleUUIDPointer(projection.LiveSourceIdeaID)
	clone.LatestEffectiveRatingEventID = cloneCycleUUIDPointer(projection.LatestEffectiveRatingEventID)
	clone.LatestEffectiveRatingEvidenceCardIdentity = cloneCycleUUIDPointer(projection.LatestEffectiveRatingEvidenceCardIdentity)
	clone.LatestEffectiveRating = cloneCycleRatingPointer(projection.LatestEffectiveRating)
	clone.LatestEffectiveRatingIngestionOrdinal = cloneCycleInt64Pointer(projection.LatestEffectiveRatingIngestionOrdinal)
	clone.LatestPresentedEventID = cloneCycleUUIDPointer(projection.LatestPresentedEventID)
	clone.LatestPresentedEvidenceCardIdentity = cloneCycleUUIDPointer(projection.LatestPresentedEvidenceCardIdentity)
	clone.LatestPresentedIngestionOrdinal = cloneCycleInt64Pointer(projection.LatestPresentedIngestionOrdinal)
	clone.CanonicalPayload = append([]byte(nil), projection.CanonicalPayload...)
	return clone
}

func cloneCycleSchedulingSnapshot(snapshot StudySchedulingSnapshot) StudySchedulingSnapshot {
	clone := snapshot
	clone.PredecessorTailCardIdentity = cloneCycleUUIDPointer(snapshot.PredecessorTailCardIdentity)
	clone.Cards = make([]StudySchedulingCard, len(snapshot.Cards))
	for index, card := range snapshot.Cards {
		clone.Cards[index] = StudySchedulingCard{
			CardIdentity: card.CardIdentity,
			Projection: StudyProjectionTail{
				LatestEffectiveRating:           cloneCycleRatingPointer(card.Projection.LatestEffectiveRating),
				LatestPresentedIngestionOrdinal: cloneCycleInt64Pointer(card.Projection.LatestPresentedIngestionOrdinal),
			},
		}
	}
	return clone
}

func cloneCyclePlan(plan StudyPlan) StudyPlan {
	clone := plan
	clone.Cards = make([]StudyPlannedCard, len(plan.Cards))
	for index, card := range plan.Cards {
		clone.Cards[index] = cloneCyclePlannedCard(card)
	}
	return clone
}

func cloneCyclePlannedCard(card StudyPlannedCard) StudyPlannedCard {
	return StudyPlannedCard{CardIdentity: card.CardIdentity, SelectionReason: cloneCycleSelectionReason(card.SelectionReason)}
}

func cloneCycleSelectionReason(reason model.StudySelectionReason) model.StudySelectionReason {
	clone := reason
	clone.LatestPresentedIngestionOrdinal = cloneCycleInt64Pointer(reason.LatestPresentedIngestionOrdinal)
	return clone
}

func cloneCycleNormalizedScope(scope model.StudyNormalizedScope) model.StudyNormalizedScope {
	clone := scope
	clone.CanonicalSelector = append([]byte(nil), scope.CanonicalSelector...)
	clone.Scope.TopicIDs = append([]uuid.UUID(nil), scope.Scope.TopicIDs...)
	clone.Scope.TopicRefs = append([]string(nil), scope.Scope.TopicRefs...)
	return clone
}

func cloneCycleSchedulerProvenancePointer(value *model.StudySchedulerProvenance) *model.StudySchedulerProvenance {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneCycleUUIDPointer(value *uuid.UUID) *uuid.UUID {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneCycleInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneCycleRatingPointer(value *model.StudyRating) *model.StudyRating {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneCycleStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneCycleTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func newCycleMaterializationError(
	code model.StudyErrorCode,
	message string,
	field string,
	reason string,
	cause error,
) *model.StudyError {
	return &model.StudyError{
		Code:    code,
		Message: message,
		Context: map[string]string{
			"operation": studyCycleMaterializationOperation,
			"field":     field,
			"reason":    reason,
		},
		Retryable: false,
		Cause:     cause,
	}
}
