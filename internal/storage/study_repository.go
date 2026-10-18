package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"lexinet/internal/model"
	"lexinet/internal/study"
	studydigest "lexinet/internal/study/digest"
)

var studyReferencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// StudyStoreTransaction is the only write-capable persistence seam exposed to
// study orchestration. The concrete pgx transaction remains storage-private so
// materialization, lifecycle changes, and receipt binding cannot accidentally
// escape the transaction that owns their lock order.
type StudyStoreTransaction interface {
	Profile() model.StudyLearnerProfile
	Scope() model.StudyNormalizedScope
	NonterminalSession() *model.StudySession
	ResolveExistingCycle(context.Context, StudyExistingCycleResolutionInput) (*StudyExistingCycleResolution, error)
	CapturedEligibleCorpusSHA256() (string, error)
	GetSession(context.Context, uuid.UUID) (*model.StudySession, error)
	FindSuccessor(context.Context, uuid.UUID) (*model.StudySession, error)
	ListSessionItems(context.Context, uuid.UUID) ([]model.StudySessionItem, error)
	ListEligibleSources(context.Context) ([]StudyEligibleSource, error)
	ReadAcceptedPrefix(context.Context, int64) ([]model.StudyEvent, error)
	ResolveCardIdentity(context.Context, uuid.UUID) (uuid.UUID, error)
	GetProjection(context.Context, StudyProjectionKey) (*model.StudySchedulerCardProjection, error)
	ReplaceProjection(context.Context, model.StudySchedulerCardProjection, int64) (*model.StudySchedulerCardProjection, error)
	InsertPlanOnce(context.Context, StudyPlanInsert) (*StudyPlanInsertResult, error)
	InsertSuccessorPlanOnce(context.Context, StudySuccessorPlanInsert) (*StudyPlanInsertResult, error)
	TransitionSession(context.Context, StudySessionTransition) (*model.StudySession, error)
}

// StudyScopeTransactionInput identifies the single lock domain entered by
// WithStudyScopeTransaction. The profile row is locked first; an existing
// nonterminal exact-scope session is locked second.
type StudyScopeTransactionInput struct {
	ProfileRef string
	Scope      model.StudyNormalizedScope
}

// StudyPlanInsert is a fully materialized, retry-stable root or empty-sentinel
// plan. PredecessorSessionID is reserved for the purpose-specific atomic
// successor operation; InsertPlanOnce rejects it until that operation can also
// validate the event/flush barrier. All UUIDs, scheduler time, and seed are
// supplied by orchestration so a transaction retry cannot change authoritative
// bytes.
type StudyPlanInsert struct {
	SessionID               uuid.UUID
	Principal               model.StudyPrincipalID
	CreationRequestID       uuid.UUID
	NormalizedRequestSHA256 string
	Lifecycle               model.StudySessionLifecycle
	CorpusSHA256            string
	Scheduler               *model.StudySchedulerProvenance
	PredecessorSessionID    *uuid.UUID
	Items                   []model.StudySessionItem
	LifecycleReasonCode     string
}

type StudyPlanInsertResult struct {
	Session model.StudySession
	Items   []model.StudySessionItem
	Receipt model.StudyCreationReceipt
	Created bool
}

// StudyExistingCycleResolutionInput is the stable creation identity that may
// be resolved before any mutable corpus or projection state is read.
type StudyExistingCycleResolutionInput struct {
	Principal               model.StudyPrincipalID
	CreationRequestID       uuid.UUID
	NormalizedRequestSHA256 string
	PredecessorSessionID    *uuid.UUID
}

// StudyExistingCycleResolution carries the durable plan authority plus the
// one storage-proven fact that cannot be inferred from receipt bytes alone:
// a nil-predecessor request created the successor while atomically replacing
// a superseded-empty waiting sentinel.
type StudyExistingCycleResolution struct {
	Plan                       StudyPlanInsertResult
	ImplicitWaitingReplacement *StudyWaitingReplacementEvidence
}

type StudyWaitingReplacementEvidence struct {
	Predecessor model.StudySession
	Items       []model.StudySessionItem
}

type StudySessionTransition struct {
	SessionID            uuid.UUID
	ExpectedStateVersion int64
	To                   model.StudySessionLifecycle
	ReasonCode           string
	Actor                model.StudyPrincipalID
}

type StudyProjectionKey struct {
	LearnerProfileID        uuid.UUID
	CardIdentity            uuid.UUID
	SchedulerName           string
	SchedulerVersion        string
	ProjectionSchemaVersion int
}

// StudyEligibleSource is the deterministic, live vocabulary projection used
// by a scheduler. It deliberately retains every source/tie timestamp and ID
// needed to reproduce selection; mutable source text is never consulted after
// a session item is issued.
type StudyEligibleSource struct {
	IdeaID                 uuid.UUID
	IdeaSummary            *string
	IdeaCreatedAt          time.Time
	IdeaUpdatedAt          time.Time
	CanonicalNameID        uuid.UUID
	CanonicalName          string
	CanonicalNameSortKey   float64
	CanonicalNameCreatedAt time.Time
	CanonicalNameUpdatedAt time.Time
	DefinitionID           *uuid.UUID
	Definition             string
	DefinitionIsPrimary    *bool
	DefinitionSortKey      *float64
	DefinitionCreatedAt    *time.Time
	DefinitionUpdatedAt    *time.Time
	ExampleID              *uuid.UUID
	Example                *string
	ExampleSortKey         *float64
	ExampleCreatedAt       *time.Time
	ExampleUpdatedAt       *time.Time
	SourceContentRevision  string
	SourceContentSHA256    string
}

type studyStoreTx struct {
	tx                      pgx.Tx
	profile                 model.StudyLearnerProfile
	scope                   model.StudyNormalizedScope
	nonterminal             *model.StudySession
	eligibleSourcesCaptured bool
	eligibleSources         map[uuid.UUID]StudyEligibleSource
	eligibleCorpusSHA256    string
}

func (store *studyStoreTx) Profile() model.StudyLearnerProfile {
	return store.profile
}

func (store *studyStoreTx) Scope() model.StudyNormalizedScope {
	return cloneStudyNormalizedScope(store.scope)
}

func (store *studyStoreTx) NonterminalSession() *model.StudySession {
	if store.nonterminal == nil {
		return nil
	}
	copyOfSession := cloneStudySession(*store.nonterminal)
	return &copyOfSession
}

func (store *studyStoreTx) CapturedEligibleCorpusSHA256() (string, error) {
	if !store.eligibleSourcesCaptured {
		return "", newStudyStorageError(
			model.StudyErrorCodeStateStale,
			"study eligibility snapshot has not been captured",
			"study_eligible_corpus_read",
			false,
			nil,
		)
	}
	if !model.IsStudySHA256(store.eligibleCorpusSHA256) {
		return "", newStudyStorageError(
			model.StudyErrorCodeInternal,
			"captured study corpus identity is invalid",
			"study_eligible_corpus_read",
			false,
			nil,
		)
	}
	return store.eligibleCorpusSHA256, nil
}

func cloneStudyNormalizedScope(scope model.StudyNormalizedScope) model.StudyNormalizedScope {
	clone := scope
	clone.CanonicalSelector = append([]byte(nil), scope.CanonicalSelector...)
	clone.Scope.TopicIDs = append([]uuid.UUID(nil), scope.Scope.TopicIDs...)
	clone.Scope.TopicRefs = append([]string(nil), scope.Scope.TopicRefs...)
	return clone
}

func cloneStudySession(session model.StudySession) model.StudySession {
	clone := session
	clone.Scope = cloneStudyNormalizedScope(session.Scope)
	if session.Scheduler != nil {
		scheduler := *session.Scheduler
		clone.Scheduler = &scheduler
	}
	clone.PredecessorStudySessionID = cloneStudyUUIDPointer(session.PredecessorStudySessionID)
	clone.SuccessorStudySessionID = cloneStudyUUIDPointer(session.SuccessorStudySessionID)
	clone.TerminalizedAt = cloneStudyTimePointer(session.TerminalizedAt)
	if session.TerminalProfileIngestionHighWater != nil {
		highWater := *session.TerminalProfileIngestionHighWater
		clone.TerminalProfileIngestionHighWater = &highWater
	}
	return clone
}

// WithStudyScopeTransaction establishes the canonical study lock order and
// commits only when the callback succeeds. Callers never receive pgx.Tx.
func (repository *Repository) WithStudyScopeTransaction(
	ctx context.Context,
	input StudyScopeTransactionInput,
	operation func(StudyStoreTransaction) error,
) error {
	if operation == nil {
		return newStudyStorageError(model.StudyErrorCodeInternal, "study transaction operation is required", "study_transaction", false, nil)
	}
	normalizedScope, err := normalizeStudyScopeTransactionInput(input)
	if err != nil {
		return err
	}

	tx, err := repository.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return mapStudyStorageError("study_transaction_begin", err)
	}
	defer tx.Rollback(ctx)

	profile, err := lockStudyProfile(ctx, tx, input.ProfileRef)
	if err != nil {
		return mapStudyStorageError("study_profile_lock", err)
	}
	nonterminal, err := findExactNonterminalStudySession(ctx, tx, profile.ID, normalizedScope, true)
	if err != nil {
		return err
	}
	store := &studyStoreTx{tx: tx, profile: *profile, scope: normalizedScope, nonterminal: nonterminal}
	if err := operation(store); err != nil {
		return err
	}
	if repository.studyMaterializationBeforeCommit != nil {
		if err := repository.studyMaterializationBeforeCommit(ctx, tx); err != nil {
			return mapStudyStorageError("study_materialization_before_commit", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return mapStudyStorageError("study_transaction_commit", err)
	}
	return nil
}

func normalizeStudyScopeTransactionInput(input StudyScopeTransactionInput) (model.StudyNormalizedScope, error) {
	if !validStudyStorageReference(input.ProfileRef) {
		return model.StudyNormalizedScope{}, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study profile reference is invalid", "study_transaction", false, nil)
	}
	normalized, err := model.NormalizeStudyScope(input.Scope.Scope)
	if err != nil {
		return model.StudyNormalizedScope{}, err
	}
	if normalized.CanonicalSelectorHash != input.Scope.CanonicalSelectorHash ||
		!bytes.Equal(normalized.CanonicalSelector, input.Scope.CanonicalSelector) {
		return model.StudyNormalizedScope{}, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study scope canonical identity is invalid", "study_transaction", false, nil)
	}
	return normalized, nil
}

func validStudyStorageReference(value string) bool {
	return utf8.ValidString(value) && len(value) >= 1 && len(value) <= 128 && studyReferencePattern.MatchString(value)
}

func lockStudyProfile(ctx context.Context, tx pgx.Tx, profileRef string) (*model.StudyLearnerProfile, error) {
	if _, err := tx.Exec(ctx, `
		INSERT INTO study_learner_profiles (learner_profile_ref)
		VALUES ($1)
		ON CONFLICT (learner_profile_ref) DO NOTHING`, profileRef); err != nil {
		return nil, err
	}
	return scanStudyProfile(tx.QueryRow(ctx, `
		SELECT learner_profile_id, learner_profile_ref, profile_ingestion_high_water, created_at, updated_at
		FROM study_learner_profiles
		WHERE learner_profile_ref = $1
		FOR UPDATE`, profileRef))
}

func (repository *Repository) GetStudyLearnerProfile(ctx context.Context, profileRef string) (*model.StudyLearnerProfile, error) {
	if !validStudyStorageReference(profileRef) {
		return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study profile reference is invalid", "study_profile_read", false, nil)
	}
	profile, err := scanStudyProfile(repository.db.QueryRow(ctx, `
		SELECT learner_profile_id, learner_profile_ref, profile_ingestion_high_water, created_at, updated_at
		FROM study_learner_profiles
		WHERE learner_profile_ref = $1`, profileRef))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, newStudyStorageError(model.StudyErrorCodeProfileNotFound, "study learner profile was not found", "study_profile_read", false, nil)
	}
	if err != nil {
		return nil, mapStudyStorageError("study_profile_read", err)
	}
	return profile, nil
}

// GetStudyLearnerProfileBySessionID resolves the stable learner owner for an
// opaque session identity without requiring a caller-supplied profile guess.
func (repository *Repository) GetStudyLearnerProfileBySessionID(
	ctx context.Context,
	sessionID uuid.UUID,
) (*model.StudyLearnerProfile, error) {
	if sessionID == uuid.Nil {
		return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study session lookup is invalid", "study_session_profile_read", false, nil)
	}
	profile, err := scanStudyProfile(repository.db.QueryRow(ctx, `
		SELECT profile.learner_profile_id, profile.learner_profile_ref,
		       profile.profile_ingestion_high_water, profile.created_at, profile.updated_at
		FROM study_sessions session
		JOIN study_learner_profiles profile
		  ON profile.learner_profile_id = session.learner_profile_id
		WHERE session.study_session_id = $1`, sessionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, newStudyStorageError(model.StudyErrorCodeSessionNotFound, "study session was not found", "study_session_profile_read", false, nil)
	}
	if err != nil {
		return nil, mapStudyStorageError("study_session_profile_read", err)
	}
	return profile, nil
}

// ResolveStudyScope converts caller references into stable topic identities
// and canonical slugs. Duplicate identities reached through different aliases
// are rejected rather than silently narrowing the requested scope.
func (repository *Repository) ResolveStudyScope(
	ctx context.Context,
	topicRefs []string,
	cardKind model.StudyCardKind,
) (model.StudyNormalizedScope, error) {
	if !cardKind.Valid() || len(topicRefs) < 1 || len(topicRefs) > model.StudyMaximumTopics {
		return model.StudyNormalizedScope{}, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study scope is invalid", "study_scope_resolve", false, nil)
	}
	for _, ref := range topicRefs {
		if !validStudyStorageReference(ref) {
			return model.StudyNormalizedScope{}, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study scope topic reference is invalid", "study_scope_resolve", false, nil)
		}
	}

	tx, err := repository.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return model.StudyNormalizedScope{}, mapStudyStorageError("study_scope_resolve_begin", err)
	}
	defer tx.Rollback(ctx)

	resolvedIDs := make([]uuid.UUID, 0, len(topicRefs))
	canonicalRefs := make([]string, 0, len(topicRefs))
	seenIDs := make(map[uuid.UUID]struct{}, len(topicRefs))
	for _, ref := range topicRefs {
		topicID, resolveErr := resolveTopicID(ctx, tx, ref)
		if resolveErr != nil {
			if errors.Is(resolveErr, ErrTopicNotFound) {
				return model.StudyNormalizedScope{}, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study scope topic was not found unambiguously", "study_scope_resolve", false, nil)
			}
			return model.StudyNormalizedScope{}, mapStudyStorageError("study_scope_resolve", resolveErr)
		}
		if _, duplicate := seenIDs[topicID]; duplicate {
			return model.StudyNormalizedScope{}, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study scope resolves the same topic more than once", "study_scope_resolve", false, nil)
		}
		topic, topicErr := getTopicByID(ctx, tx, topicID)
		if topicErr != nil {
			if errors.Is(topicErr, ErrTopicNotFound) {
				return model.StudyNormalizedScope{}, newStudyStorageError(model.StudyErrorCodeStateStale, "study scope topic changed during resolution", "study_scope_resolve", true, nil)
			}
			return model.StudyNormalizedScope{}, mapStudyStorageError("study_scope_resolve", topicErr)
		}
		seenIDs[topicID] = struct{}{}
		resolvedIDs = append(resolvedIDs, topicID)
		canonicalRefs = append(canonicalRefs, topic.Slug)
	}

	normalized, err := model.NormalizeStudyScope(model.StudyScope{
		TopicIDs:  resolvedIDs,
		TopicRefs: canonicalRefs,
		CardKind:  cardKind,
	})
	if err != nil {
		return model.StudyNormalizedScope{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.StudyNormalizedScope{}, mapStudyStorageError("study_scope_resolve_commit", err)
	}
	return normalized, nil
}

func (repository *Repository) GetStudySession(ctx context.Context, profileRef string, sessionID uuid.UUID) (*model.StudySession, error) {
	if !validStudyStorageReference(profileRef) || sessionID == uuid.Nil {
		return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study session lookup is invalid", "study_session_read", false, nil)
	}
	session, err := queryStudySession(ctx, repository.db, sessionID, uuid.Nil, profileRef, false)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func (repository *Repository) ListStudySessionItems(ctx context.Context, profileRef string, sessionID uuid.UUID) ([]model.StudySessionItem, error) {
	if _, err := repository.GetStudySession(ctx, profileRef, sessionID); err != nil {
		return nil, err
	}
	return listStudySessionItems(ctx, repository.db, sessionID)
}

func (store *studyStoreTx) GetSession(ctx context.Context, sessionID uuid.UUID) (*model.StudySession, error) {
	return queryStudySession(ctx, store.tx, sessionID, store.profile.ID, "", false)
}

func (store *studyStoreTx) FindSuccessor(ctx context.Context, predecessorID uuid.UUID) (*model.StudySession, error) {
	if predecessorID == uuid.Nil {
		return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study predecessor identity is invalid", "study_successor_read", false, nil)
	}
	session, err := scanStudySession(store.tx.QueryRow(ctx, studySessionSelect+`
		WHERE session.predecessor_study_session_id = $1
		  AND session.learner_profile_id = $2`, predecessorID, store.profile.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapStudyStorageError("study_successor_read", err)
	}
	return session, nil
}

func (store *studyStoreTx) ListSessionItems(ctx context.Context, sessionID uuid.UUID) ([]model.StudySessionItem, error) {
	if _, err := store.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	return listStudySessionItems(ctx, store.tx, sessionID)
}

func scanStudyProfile(row pgx.Row) (*model.StudyLearnerProfile, error) {
	var profile model.StudyLearnerProfile
	if err := row.Scan(&profile.ID, &profile.Ref, &profile.ProfileIngestionHighWater, &profile.CreatedAt, &profile.UpdatedAt); err != nil {
		return nil, err
	}
	return &profile, nil
}

const studySessionSelect = `
	SELECT
		session.study_session_id,
		session.learner_profile_id,
		session.normalized_corpus_selector,
		session.normalized_corpus_selector_sha256,
		session.normalized_topic_ids,
		session.normalized_topic_refs,
		session.study_card_kind,
		session.study_session_lifecycle,
		session.study_corpus_sha256,
		session.study_scheduler_name,
		session.study_scheduler_version,
		session.study_scheduler_configuration_sha256,
		session.study_scheduler_input_ingestion_high_water,
		session.study_scheduler_planned_at,
		session.study_scheduler_seed,
		session.predecessor_study_session_id,
		(
			SELECT successor.study_session_id
			FROM study_sessions successor
			WHERE successor.predecessor_study_session_id = session.study_session_id
		),
		profile.profile_ingestion_high_water,
		session.study_session_state_version,
		session.created_at,
		session.updated_at,
		session.study_session_terminalized_at,
		session.study_session_terminal_profile_ingestion_high_water
	FROM study_sessions session
	JOIN study_learner_profiles profile ON profile.learner_profile_id = session.learner_profile_id
`

func queryStudySession(
	ctx context.Context,
	runner dbRunner,
	sessionID uuid.UUID,
	profileID uuid.UUID,
	profileRef string,
	lock bool,
) (*model.StudySession, error) {
	query := studySessionSelect + `
		WHERE session.study_session_id = $1
		  AND ($2::uuid = '00000000-0000-0000-0000-000000000000' OR session.learner_profile_id = $2)
		  AND ($3 = '' OR profile.learner_profile_ref = $3)`
	if lock {
		query += ` FOR UPDATE OF session`
	}
	session, err := scanStudySession(runner.QueryRow(ctx, query, sessionID, profileID, profileRef))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, newStudyStorageError(model.StudyErrorCodeSessionNotFound, "study session was not found", "study_session_read", false, nil)
	}
	if err != nil {
		return nil, mapStudyStorageError("study_session_read", err)
	}
	return session, nil
}

func findExactNonterminalStudySession(
	ctx context.Context,
	runner dbRunner,
	profileID uuid.UUID,
	scope model.StudyNormalizedScope,
	lock bool,
) (*model.StudySession, error) {
	query := studySessionSelect + `
		WHERE session.learner_profile_id = $1
		  AND session.normalized_corpus_selector_sha256 = $2
		  AND session.normalized_corpus_selector = $3
		  AND session.study_card_kind = $4
		  AND session.study_session_lifecycle IN ('active', 'waiting_for_corpus')`
	if lock {
		query += ` FOR UPDATE OF session`
	}
	session, err := scanStudySession(runner.QueryRow(
		ctx,
		query,
		profileID,
		scope.CanonicalSelectorHash,
		string(scope.CanonicalSelector),
		scope.Scope.CardKind,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapStudyStorageError("study_nonterminal_scope_read", err)
	}
	return session, nil
}

func scanStudySession(row pgx.Row) (*model.StudySession, error) {
	var (
		session                      model.StudySession
		canonicalSelector            string
		topicIDs                     []uuid.UUID
		topicRefs                    []string
		cardKind                     string
		lifecycle                    string
		schedulerName                *string
		schedulerVersion             *string
		schedulerConfigurationSHA256 *string
		schedulerHighWater           *int64
		schedulerPlannedAt           *time.Time
		schedulerSeed                *string
	)
	if err := row.Scan(
		&session.ID,
		&session.LearnerProfileID,
		&canonicalSelector,
		&session.Scope.CanonicalSelectorHash,
		&topicIDs,
		&topicRefs,
		&cardKind,
		&lifecycle,
		&session.CorpusSHA256,
		&schedulerName,
		&schedulerVersion,
		&schedulerConfigurationSHA256,
		&schedulerHighWater,
		&schedulerPlannedAt,
		&schedulerSeed,
		&session.PredecessorStudySessionID,
		&session.SuccessorStudySessionID,
		&session.ProfileIngestionHighWater,
		&session.StateVersion,
		&session.CreatedAt,
		&session.UpdatedAt,
		&session.TerminalizedAt,
		&session.TerminalProfileIngestionHighWater,
	); err != nil {
		return nil, err
	}
	session.Scope.CanonicalSelector = json.RawMessage(canonicalSelector)
	session.Scope.Scope = model.StudyScope{
		TopicIDs:  topicIDs,
		TopicRefs: topicRefs,
		CardKind:  model.StudyCardKind(cardKind),
	}
	session.Lifecycle = model.StudySessionLifecycle(lifecycle)
	if schedulerName != nil {
		session.Scheduler = &model.StudySchedulerProvenance{
			Name:                           *schedulerName,
			Version:                        *schedulerVersion,
			ConfigurationSHA256:            *schedulerConfigurationSHA256,
			InputProfileIngestionHighWater: *schedulerHighWater,
			PlannedAt:                      *schedulerPlannedAt,
			Seed:                           *schedulerSeed,
		}
	}
	return &session, nil
}

func listStudySessionItems(ctx context.Context, runner dbRunner, sessionID uuid.UUID) ([]model.StudySessionItem, error) {
	rows, err := runner.Query(ctx, `
		SELECT
			study_session_item_id,
			study_session_id,
			study_session_item_position,
			study_card_kind,
			study_card_identity,
			historical_source_kind,
			historical_source_identity,
			live_source_idea_id,
			source_content_revision,
			source_content_sha256,
			card_front_canonical_name,
			card_back_definition,
			card_back_example,
			selection_rating_rank,
			selection_latest_presented_ingestion_ordinal,
			selection_tie_breaker_sha256,
			selection_boundary_adjustment,
			study_session_item_created_at
		FROM study_session_items
		WHERE study_session_id = $1
		ORDER BY study_session_item_position`, sessionID)
	if err != nil {
		return nil, mapStudyStorageError("study_session_items_read", err)
	}
	defer rows.Close()
	items := make([]model.StudySessionItem, 0)
	for rows.Next() {
		var item model.StudySessionItem
		var cardKind, ratingRank, boundaryAdjustment string
		if err := rows.Scan(
			&item.ID,
			&item.StudySessionID,
			&item.Position,
			&cardKind,
			&item.CardIdentity,
			&item.HistoricalSourceKind,
			&item.HistoricalSourceIdentity,
			&item.LiveSourceIdeaID,
			&item.SourceContentRevision,
			&item.SourceContentSHA256,
			&item.FrontCanonicalName,
			&item.BackDefinition,
			&item.BackExample,
			&ratingRank,
			&item.SelectionReason.LatestPresentedIngestionOrdinal,
			&item.SelectionReason.TieBreakerSHA256,
			&boundaryAdjustment,
			&item.CreatedAt,
		); err != nil {
			return nil, mapStudyStorageError("study_session_items_read", err)
		}
		item.CardKind = model.StudyCardKind(cardKind)
		item.SelectionReason.RatingRank = model.StudyRatingRank(ratingRank)
		item.SelectionReason.BoundaryAdjustment = model.StudyBoundaryAdjustment(boundaryAdjustment)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, mapStudyStorageError("study_session_items_read", err)
	}
	return items, nil
}

func (store *studyStoreTx) ResolveExistingCycle(
	ctx context.Context,
	input StudyExistingCycleResolutionInput,
) (*StudyExistingCycleResolution, error) {
	if !input.Principal.Valid() || input.CreationRequestID == uuid.Nil || !model.IsStudySHA256(input.NormalizedRequestSHA256) ||
		(input.PredecessorSessionID != nil && *input.PredecessorSessionID == uuid.Nil) {
		return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study creation resolution identity is invalid", "study_creation_resolve", false, nil)
	}
	if input.PredecessorSessionID != nil {
		predecessor, err := store.GetSession(ctx, *input.PredecessorSessionID)
		if err != nil {
			return nil, err
		}
		if predecessor != nil && (predecessor.Lifecycle == model.StudySessionLifecycleWaitingForCorpus ||
			predecessor.Lifecycle == model.StudySessionLifecycleSupersededEmpty) {
			return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "waiting study sentinel cannot be named as an explicit successor predecessor", "study_creation_resolve", false, nil)
		}
	}
	receipt, err := readStudyCreationReceipt(
		ctx,
		store.tx,
		store.profile.ID,
		store.scope,
		input.Principal,
		input.CreationRequestID,
	)
	if err != nil {
		return nil, err
	}
	if receipt != nil {
		if receipt.NormalizedRequestSHA256 != input.NormalizedRequestSHA256 {
			return nil, newStudyStorageError(model.StudyErrorCodeIdempotencyConflict, "study creation request identity is already bound to different content", "study_creation_resolve", false, nil)
		}
		result, err := store.planInsertResult(ctx, *receipt, false)
		if err != nil {
			return nil, err
		}
		implicitWaitingReplacement, err := store.implicitWaitingReplacementEvidence(ctx, result.Session, result.Receipt, input.PredecessorSessionID)
		if err != nil {
			return nil, err
		}
		if !validStudyMaterializationReceiptPredecessor(
			result.Session,
			result.Receipt,
			input.PredecessorSessionID,
			implicitWaitingReplacement != nil,
		) {
			return nil, newStudyStorageError(model.StudyErrorCodeIdempotencyConflict, "study creation request identity is already bound to different predecessor semantics", "study_creation_resolve", false, nil)
		}
		return &StudyExistingCycleResolution{Plan: *result, ImplicitWaitingReplacement: implicitWaitingReplacement}, nil
	}

	if input.PredecessorSessionID != nil {
		successor, err := store.FindSuccessor(ctx, *input.PredecessorSessionID)
		if err != nil {
			return nil, err
		}
		if successor == nil {
			return nil, nil
		}
		if !sameStudyScope(successor.Scope, store.scope) {
			return nil, newStudyStorageError(model.StudyErrorCodeSessionNotFound, "study successor was not found in the active scope", "study_creation_resolve", false, nil)
		}
		receipt, err := insertStudyCreationReceipt(
			ctx,
			store.tx,
			store.profile.ID,
			store.scope,
			input.Principal,
			input.CreationRequestID,
			input.NormalizedRequestSHA256,
			successor.ID,
			model.StudyCreationDispositionSuccessor,
		)
		if err != nil {
			return nil, err
		}
		result, err := store.planInsertResult(ctx, *receipt, false)
		if err != nil {
			return nil, err
		}
		return &StudyExistingCycleResolution{Plan: *result}, nil
	}

	if store.nonterminal == nil || store.nonterminal.Lifecycle == model.StudySessionLifecycleWaitingForCorpus {
		// A waiting sentinel is deliberately unresolved: only a locked corpus
		// capture can distinguish unchanged waiting from atomic replacement.
		return nil, nil
	}
	if store.nonterminal.Lifecycle != model.StudySessionLifecycleActive {
		return nil, newStudyStorageError(model.StudyErrorCodeInternal, "locked study nonterminal lifecycle is invalid", "study_creation_resolve", false, nil)
	}
	receipt, err = insertStudyCreationReceipt(
		ctx,
		store.tx,
		store.profile.ID,
		store.scope,
		input.Principal,
		input.CreationRequestID,
		input.NormalizedRequestSHA256,
		store.nonterminal.ID,
		model.StudyCreationDispositionConvergedToExisting,
	)
	if err != nil {
		return nil, err
	}
	result, err := store.planInsertResult(ctx, *receipt, false)
	if err != nil {
		return nil, err
	}
	return &StudyExistingCycleResolution{Plan: *result}, nil
}

func validStudyMaterializationReceiptPredecessor(
	session model.StudySession,
	receipt model.StudyCreationReceipt,
	requested *uuid.UUID,
	implicitWaitingReplacement bool,
) bool {
	switch receipt.Disposition {
	case model.StudyCreationDispositionCreated:
		return requested == nil && session.PredecessorStudySessionID == nil && !implicitWaitingReplacement
	case model.StudyCreationDispositionConvergedToExisting:
		return requested == nil && !implicitWaitingReplacement
	case model.StudyCreationDispositionSuccessor:
		if session.PredecessorStudySessionID == nil {
			return false
		}
		if requested != nil {
			return !implicitWaitingReplacement && *requested == *session.PredecessorStudySessionID
		}
		return implicitWaitingReplacement
	default:
		return false
	}
}

func (store *studyStoreTx) implicitWaitingReplacementEvidence(
	ctx context.Context,
	session model.StudySession,
	receipt model.StudyCreationReceipt,
	requested *uuid.UUID,
) (*StudyWaitingReplacementEvidence, error) {
	if requested != nil || receipt.Disposition != model.StudyCreationDispositionSuccessor ||
		session.PredecessorStudySessionID == nil {
		return nil, nil
	}
	predecessor, err := store.GetSession(ctx, *session.PredecessorStudySessionID)
	if err != nil {
		return nil, err
	}
	if predecessor == nil {
		return nil, newStudyStorageError(model.StudyErrorCodeInternal, "study waiting replacement predecessor is unavailable", "study_creation_resolve", false, nil)
	}
	if predecessor.Lifecycle != model.StudySessionLifecycleSupersededEmpty ||
		predecessor.SuccessorStudySessionID == nil || *predecessor.SuccessorStudySessionID != session.ID ||
		predecessor.LearnerProfileID != session.LearnerProfileID || !sameStudyScope(predecessor.Scope, session.Scope) {
		return nil, nil
	}
	items, err := listStudySessionItems(ctx, store.tx, predecessor.ID)
	if err != nil {
		return nil, err
	}
	return &StudyWaitingReplacementEvidence{Predecessor: *predecessor, Items: items}, nil
}

func (store *studyStoreTx) InsertPlanOnce(ctx context.Context, input StudyPlanInsert) (*StudyPlanInsertResult, error) {
	if err := validateStudyPlanInsert(store, input); err != nil {
		return nil, err
	}

	receipt, err := readStudyCreationReceipt(ctx, store.tx, store.profile.ID, store.scope, input.Principal, input.CreationRequestID)
	if err != nil {
		return nil, err
	}
	if receipt != nil {
		if receipt.NormalizedRequestSHA256 != input.NormalizedRequestSHA256 {
			return nil, newStudyStorageError(model.StudyErrorCodeIdempotencyConflict, "study creation request identity is already bound to different content", "study_plan_insert", false, nil)
		}
		return store.planInsertResult(ctx, *receipt, false)
	}

	var authoritative *model.StudySession
	disposition := model.StudyCreationDispositionConvergedToExisting
	if store.nonterminal != nil {
		copyOfSession := *store.nonterminal
		authoritative = &copyOfSession
	}
	if authoritative != nil {
		receipt, err = insertStudyCreationReceipt(
			ctx,
			store.tx,
			store.profile.ID,
			store.scope,
			input.Principal,
			input.CreationRequestID,
			input.NormalizedRequestSHA256,
			authoritative.ID,
			disposition,
		)
		if err != nil {
			return nil, err
		}
		return store.planInsertResult(ctx, *receipt, false)
	}

	input, err = bindStudyPlanToEligibleSnapshot(store, input)
	if err != nil {
		return nil, err
	}
	if err := validateStudyPlanInsert(store, input); err != nil {
		return nil, err
	}
	if err := lockAndValidateStudyPlanSources(ctx, store.tx, store.scope, input); err != nil {
		return nil, err
	}

	terminalizedAt := (*time.Time)(nil)
	_, err = store.tx.Exec(ctx, `
		INSERT INTO study_sessions (
			study_session_id,
			learner_profile_id,
			normalized_corpus_selector,
			normalized_corpus_selector_sha256,
			normalized_topic_ids,
			normalized_topic_refs,
			study_card_kind,
			study_session_lifecycle,
			study_corpus_sha256,
			study_scheduler_name,
			study_scheduler_version,
			study_scheduler_configuration_sha256,
			study_scheduler_input_ingestion_high_water,
			study_scheduler_planned_at,
			study_scheduler_seed,
			predecessor_study_session_id,
			study_session_state_version,
			study_session_terminalized_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9,
			$10, $11, $12, $13, $14, $15, $16, 1, $17
		)`,
		input.SessionID,
		store.profile.ID,
		string(store.scope.CanonicalSelector),
		store.scope.CanonicalSelectorHash,
		store.scope.Scope.TopicIDs,
		store.scope.Scope.TopicRefs,
		store.scope.Scope.CardKind,
		input.Lifecycle,
		input.CorpusSHA256,
		studySchedulerName(input.Scheduler),
		studySchedulerVersion(input.Scheduler),
		studySchedulerConfigurationSHA(input.Scheduler),
		studySchedulerHighWater(input.Scheduler),
		studySchedulerPlannedAt(input.Scheduler),
		studySchedulerSeed(input.Scheduler),
		nil,
		terminalizedAt,
	)
	if err != nil {
		return nil, mapStudyStorageError("study_plan_insert", err)
	}
	if _, err := store.tx.Exec(ctx, `
		INSERT INTO study_session_lifecycle_events (
			study_session_id,
			lifecycle_from_state,
			lifecycle_to_state,
			lifecycle_reason_code,
			lifecycle_actor_ref,
			lifecycle_state_version
		) VALUES ($1, NULL, $2, $3, $4, 1)`,
		input.SessionID,
		input.Lifecycle,
		input.LifecycleReasonCode,
		input.Principal.String(),
	); err != nil {
		return nil, mapStudyStorageError("study_lifecycle_audit_insert", err)
	}
	for index := range input.Items {
		if err := insertStudySessionItem(ctx, store.tx, input.Items[index]); err != nil {
			return nil, err
		}
	}

	disposition = model.StudyCreationDispositionCreated
	receipt, err = insertStudyCreationReceipt(
		ctx,
		store.tx,
		store.profile.ID,
		store.scope,
		input.Principal,
		input.CreationRequestID,
		input.NormalizedRequestSHA256,
		input.SessionID,
		disposition,
	)
	if err != nil {
		return nil, err
	}
	created, err := store.GetSession(ctx, input.SessionID)
	if err != nil {
		return nil, err
	}
	store.nonterminal = created
	return store.planInsertResult(ctx, *receipt, true)
}

func bindStudyPlanToEligibleSnapshot(store *studyStoreTx, input StudyPlanInsert) (StudyPlanInsert, error) {
	if !store.eligibleSourcesCaptured {
		return StudyPlanInsert{}, newStudyStorageError(model.StudyErrorCodeStateStale, "study eligibility snapshot was not captured in this transaction", "study_plan_source_bind", false, nil)
	}
	if input.Lifecycle == model.StudySessionLifecycleWaitingForCorpus {
		if len(store.eligibleSources) != 0 {
			return StudyPlanInsert{}, newStudyStorageError(model.StudyErrorCodeStateStale, "study corpus is not empty", "study_plan_source_bind", false, nil)
		}
		input.CorpusSHA256 = store.eligibleCorpusSHA256
		return input, nil
	}
	if input.Lifecycle != model.StudySessionLifecycleActive || len(input.Items) != len(store.eligibleSources) {
		return StudyPlanInsert{}, newStudyStorageError(model.StudyErrorCodeStateStale, "study plan does not cover the captured corpus", "study_plan_source_bind", false, nil)
	}
	boundItems := append([]model.StudySessionItem(nil), input.Items...)
	seen := make(map[uuid.UUID]struct{}, len(boundItems))
	for index := range boundItems {
		item := &boundItems[index]
		if item.LiveSourceIdeaID == nil || item.CardIdentity != *item.LiveSourceIdeaID || item.HistoricalSourceIdentity != *item.LiveSourceIdeaID {
			return StudyPlanInsert{}, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study item source identities are inconsistent", "study_plan_source_bind", false, nil)
		}
		source, ok := store.eligibleSources[*item.LiveSourceIdeaID]
		if !ok {
			return StudyPlanInsert{}, newStudyStorageError(model.StudyErrorCodeStateStale, "study item source is outside the captured corpus", "study_plan_source_bind", false, nil)
		}
		if _, duplicate := seen[source.IdeaID]; duplicate {
			return StudyPlanInsert{}, newStudyStorageError(model.StudyErrorCodeStateStale, "study plan repeats a captured source", "study_plan_source_bind", false, nil)
		}
		seen[source.IdeaID] = struct{}{}
		item.SourceContentRevision = source.SourceContentRevision
		item.SourceContentSHA256 = source.SourceContentSHA256
		item.FrontCanonicalName = source.CanonicalName
		item.BackDefinition = source.Definition
		item.BackExample = cloneStudyStringPointer(source.Example)
	}
	if len(seen) != len(store.eligibleSources) {
		return StudyPlanInsert{}, newStudyStorageError(model.StudyErrorCodeStateStale, "study plan omits a captured source", "study_plan_source_bind", false, nil)
	}
	input.Items = boundItems
	input.CorpusSHA256 = store.eligibleCorpusSHA256
	return input, nil
}

func cloneStudyStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	return &copyOfValue
}

func validateStudyPlanInsert(store *studyStoreTx, input StudyPlanInsert) error {
	if input.SessionID == uuid.Nil || input.CreationRequestID == uuid.Nil || !input.Principal.Valid() {
		return newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study plan identities are invalid", "study_plan_insert", false, nil)
	}
	if !model.IsStudySHA256(input.NormalizedRequestSHA256) || !model.IsStudySHA256(input.CorpusSHA256) {
		return newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study plan digest is invalid", "study_plan_insert", false, nil)
	}
	if input.Lifecycle != model.StudySessionLifecycleActive && input.Lifecycle != model.StudySessionLifecycleWaitingForCorpus {
		return newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study plan lifecycle is invalid", "study_plan_insert", false, nil)
	}
	if input.PredecessorSessionID != nil {
		return newStudyStorageError(model.StudyErrorCodeFlushReceiptRequired, "study successors require the atomic flush-barrier operation", "study_plan_insert", false, nil)
	}
	if !boundedStudyStorageText(input.LifecycleReasonCode, 128) {
		return newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study lifecycle reason is invalid", "study_plan_insert", false, nil)
	}
	if input.Lifecycle == model.StudySessionLifecycleActive {
		if err := validateStudyScheduler(input.Scheduler, store.profile.ProfileIngestionHighWater); err != nil {
			return err
		}
		if len(input.Items) == 0 {
			return newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "active study plan requires at least one issued item", "study_plan_insert", false, nil)
		}
	} else if input.Scheduler != nil || len(input.Items) != 0 {
		return newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "waiting study plan cannot contain scheduler output", "study_plan_insert", false, nil)
	}
	for index := range input.Items {
		item := input.Items[index]
		if item.StudySessionID != input.SessionID || item.Position != index {
			return newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study item ownership or position is invalid", "study_plan_insert", false, nil)
		}
		if item.LiveSourceIdeaID == nil || *item.LiveSourceIdeaID == uuid.Nil ||
			item.CardIdentity != *item.LiveSourceIdeaID || item.HistoricalSourceIdentity != *item.LiveSourceIdeaID {
			return newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study item source identities are inconsistent", "study_plan_insert", false, nil)
		}
		if err := model.ValidateStudySessionItem(item); err != nil {
			return err
		}
	}
	return nil
}

func lockAndValidateStudyPlanSources(
	ctx context.Context,
	tx pgx.Tx,
	scope model.StudyNormalizedScope,
	input StudyPlanInsert,
) error {
	if input.Lifecycle != model.StudySessionLifecycleActive {
		return nil
	}
	sourceSet := make(map[uuid.UUID]struct{}, len(input.Items))
	for index := range input.Items {
		item := input.Items[index]
		if item.LiveSourceIdeaID == nil ||
			*item.LiveSourceIdeaID == uuid.Nil ||
			item.CardIdentity != *item.LiveSourceIdeaID ||
			item.HistoricalSourceIdentity != *item.LiveSourceIdeaID {
			return newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study item source identities are inconsistent", "study_plan_source_lock", false, nil)
		}
		if _, duplicate := sourceSet[*item.LiveSourceIdeaID]; duplicate {
			return newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study plan contains a duplicate source identity", "study_plan_source_lock", false, nil)
		}
		sourceSet[*item.LiveSourceIdeaID] = struct{}{}
	}
	sourceIDs := make([]uuid.UUID, 0, len(sourceSet))
	for sourceID := range sourceSet {
		sourceIDs = append(sourceIDs, sourceID)
	}
	sort.Slice(sourceIDs, func(left, right int) bool {
		return sourceIDs[left].String() < sourceIDs[right].String()
	})

	rows, err := tx.Query(ctx, `
		SELECT id, status
		FROM ideas
		WHERE id = ANY($1::uuid[])
		ORDER BY id
		FOR UPDATE`, sourceIDs)
	if err != nil {
		return mapStudyStorageError("study_plan_source_lock", err)
	}
	activeSources := make(map[uuid.UUID]struct{}, len(sourceIDs))
	for rows.Next() {
		var sourceID uuid.UUID
		var status string
		if err := rows.Scan(&sourceID, &status); err != nil {
			rows.Close()
			return mapStudyStorageError("study_plan_source_lock", err)
		}
		if model.IdeaStatus(status) != model.IdeaStatusActive {
			rows.Close()
			return newStudyStorageError(model.StudyErrorCodeStateStale, "study plan source is no longer active", "study_plan_source_lock", false, nil)
		}
		activeSources[sourceID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return mapStudyStorageError("study_plan_source_lock", err)
	}
	rows.Close()
	if len(activeSources) != len(sourceIDs) {
		return newStudyStorageError(model.StudyErrorCodeStateStale, "study plan source is no longer available", "study_plan_source_lock", false, nil)
	}

	rows, err = tx.Query(ctx, `
		SELECT idea_id, topic_id
		FROM idea_topics
		WHERE idea_id = ANY($1::uuid[])
		  AND topic_id = ANY($2::uuid[])
		ORDER BY idea_id, topic_id
		FOR KEY SHARE`, sourceIDs, scope.Scope.TopicIDs)
	if err != nil {
		return mapStudyStorageError("study_plan_topic_lock", err)
	}
	eligibleSources := make(map[uuid.UUID]struct{}, len(sourceIDs))
	for rows.Next() {
		var sourceID, topicID uuid.UUID
		if err := rows.Scan(&sourceID, &topicID); err != nil {
			rows.Close()
			return mapStudyStorageError("study_plan_topic_lock", err)
		}
		eligibleSources[sourceID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return mapStudyStorageError("study_plan_topic_lock", err)
	}
	rows.Close()
	if len(eligibleSources) != len(sourceIDs) {
		return newStudyStorageError(model.StudyErrorCodeStateStale, "study plan source is no longer topic-eligible", "study_plan_topic_lock", false, nil)
	}
	return nil
}

func validateStudyScheduler(scheduler *model.StudySchedulerProvenance, profileHighWater int64) error {
	if scheduler == nil ||
		!boundedStudyStorageText(scheduler.Name, 128) ||
		!boundedStudyStorageText(scheduler.Version, 64) ||
		!boundedStudyStorageText(scheduler.Seed, 128) ||
		!model.IsStudySHA256(scheduler.ConfigurationSHA256) ||
		scheduler.InputProfileIngestionHighWater < 0 ||
		scheduler.InputProfileIngestionHighWater != profileHighWater ||
		scheduler.PlannedAt.IsZero() {
		return newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study scheduler provenance is invalid", "study_plan_insert", false, nil)
	}
	return nil
}

func boundedStudyStorageText(value string, maximumRunes int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && len([]rune(value)) <= maximumRunes
}

func sameStudyScope(left, right model.StudyNormalizedScope) bool {
	return left.CanonicalSelectorHash == right.CanonicalSelectorHash &&
		bytes.Equal(left.CanonicalSelector, right.CanonicalSelector) &&
		left.Scope.CardKind == right.Scope.CardKind
}

func studySchedulerName(scheduler *model.StudySchedulerProvenance) any {
	if scheduler == nil {
		return nil
	}
	return scheduler.Name
}

func studySchedulerVersion(scheduler *model.StudySchedulerProvenance) any {
	if scheduler == nil {
		return nil
	}
	return scheduler.Version
}

func studySchedulerConfigurationSHA(scheduler *model.StudySchedulerProvenance) any {
	if scheduler == nil {
		return nil
	}
	return scheduler.ConfigurationSHA256
}

func studySchedulerHighWater(scheduler *model.StudySchedulerProvenance) any {
	if scheduler == nil {
		return nil
	}
	return scheduler.InputProfileIngestionHighWater
}

func studySchedulerPlannedAt(scheduler *model.StudySchedulerProvenance) any {
	if scheduler == nil {
		return nil
	}
	return scheduler.PlannedAt
}

func studySchedulerSeed(scheduler *model.StudySchedulerProvenance) any {
	if scheduler == nil {
		return nil
	}
	return scheduler.Seed
}

func insertStudySessionItem(ctx context.Context, tx pgx.Tx, item model.StudySessionItem) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO study_session_items (
			study_session_item_id,
			study_session_id,
			study_session_item_position,
			study_card_kind,
			study_card_identity,
			historical_source_kind,
			historical_source_identity,
			live_source_idea_id,
			source_content_revision,
			source_content_sha256,
			card_front_canonical_name,
			card_back_definition,
			card_back_example,
			selection_rating_rank,
			selection_latest_presented_ingestion_ordinal,
			selection_tie_breaker_sha256,
			selection_boundary_adjustment
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
		)`,
		item.ID,
		item.StudySessionID,
		item.Position,
		item.CardKind,
		item.CardIdentity,
		item.HistoricalSourceKind,
		item.HistoricalSourceIdentity,
		item.LiveSourceIdeaID,
		item.SourceContentRevision,
		item.SourceContentSHA256,
		item.FrontCanonicalName,
		item.BackDefinition,
		item.BackExample,
		item.SelectionReason.RatingRank,
		item.SelectionReason.LatestPresentedIngestionOrdinal,
		item.SelectionReason.TieBreakerSHA256,
		item.SelectionReason.BoundaryAdjustment,
	)
	if err != nil {
		return mapStudyStorageError("study_session_item_insert", err)
	}
	return nil
}

func readStudyCreationReceipt(
	ctx context.Context,
	runner dbRunner,
	profileID uuid.UUID,
	scope model.StudyNormalizedScope,
	principal model.StudyPrincipalID,
	requestID uuid.UUID,
) (*model.StudyCreationReceipt, error) {
	row := runner.QueryRow(ctx, `
		SELECT
			study_principal_id,
			creation_request_id,
			normalized_creation_request_sha256,
			creation_disposition,
			study_session_id,
			learner_profile_id,
			creation_receipt_created_at
		FROM study_session_creation_receipts
		WHERE study_principal_id = $1
		  AND learner_profile_id = $2
		  AND normalized_corpus_selector_sha256 = $3
		  AND normalized_corpus_selector = $4
		  AND study_card_kind = $5
		  AND creation_request_id = $6`,
		principal.String(),
		profileID,
		scope.CanonicalSelectorHash,
		string(scope.CanonicalSelector),
		scope.Scope.CardKind,
		requestID,
	)
	receipt, err := scanStudyCreationReceipt(row, scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapStudyStorageError("study_creation_receipt_read", err)
	}
	return receipt, nil
}

func insertStudyCreationReceipt(
	ctx context.Context,
	tx pgx.Tx,
	profileID uuid.UUID,
	scope model.StudyNormalizedScope,
	principal model.StudyPrincipalID,
	requestID uuid.UUID,
	requestSHA string,
	sessionID uuid.UUID,
	disposition model.StudyCreationDisposition,
) (*model.StudyCreationReceipt, error) {
	receipt, err := scanStudyCreationReceipt(tx.QueryRow(ctx, `
		INSERT INTO study_session_creation_receipts (
			study_principal_id,
			learner_profile_id,
			normalized_corpus_selector_sha256,
			normalized_corpus_selector,
			study_card_kind,
			creation_request_id,
			normalized_creation_request_sha256,
			study_session_id,
			creation_disposition
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING
			study_principal_id,
			creation_request_id,
			normalized_creation_request_sha256,
			creation_disposition,
			study_session_id,
			learner_profile_id,
			creation_receipt_created_at`,
		principal.String(),
		profileID,
		scope.CanonicalSelectorHash,
		string(scope.CanonicalSelector),
		scope.Scope.CardKind,
		requestID,
		requestSHA,
		sessionID,
		disposition,
	), scope)
	if err != nil {
		return nil, mapStudyStorageError("study_creation_receipt_insert", err)
	}
	return receipt, nil
}

func scanStudyCreationReceipt(row pgx.Row, scope model.StudyNormalizedScope) (*model.StudyCreationReceipt, error) {
	var receipt model.StudyCreationReceipt
	var principal, disposition string
	if err := row.Scan(
		&principal,
		&receipt.CreationRequestID,
		&receipt.NormalizedRequestSHA256,
		&disposition,
		&receipt.CanonicalStudySessionID,
		&receipt.LearnerProfileID,
		&receipt.CreationReceiptAdjudicatedAt,
	); err != nil {
		return nil, err
	}
	restoredPrincipal, err := model.RestoreStudyPrincipalID(principal)
	if err != nil {
		return nil, err
	}
	receipt.StudyPrincipalID = restoredPrincipal
	receipt.Disposition = model.StudyCreationDisposition(disposition)
	receipt.Scope = scope
	return &receipt, nil
}

func (store *studyStoreTx) planInsertResult(ctx context.Context, receipt model.StudyCreationReceipt, created bool) (*StudyPlanInsertResult, error) {
	session, err := store.GetSession(ctx, receipt.CanonicalStudySessionID)
	if err != nil {
		return nil, err
	}
	items, err := listStudySessionItems(ctx, store.tx, session.ID)
	if err != nil {
		return nil, err
	}
	return &StudyPlanInsertResult{Session: *session, Items: items, Receipt: receipt, Created: created}, nil
}

func (store *studyStoreTx) TransitionSession(ctx context.Context, input StudySessionTransition) (*model.StudySession, error) {
	if input.SessionID == uuid.Nil || input.ExpectedStateVersion < 1 || !input.To.Terminal() || !input.Actor.Valid() || !boundedStudyStorageText(input.ReasonCode, 128) {
		return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study lifecycle transition is invalid", "study_lifecycle_transition", false, nil)
	}
	if input.To != model.StudySessionLifecycleAbandoned {
		return nil, newStudyStorageError(model.StudyErrorCodeFlushReceiptRequired, "study successor lifecycle changes require the atomic flush-barrier operation", "study_lifecycle_transition", false, nil)
	}
	session, err := queryStudySession(ctx, store.tx, input.SessionID, store.profile.ID, "", true)
	if err != nil {
		return nil, err
	}
	if !sameStudyScope(session.Scope, store.scope) {
		return nil, newStudyStorageError(model.StudyErrorCodeSessionNotFound, "study session was not found", "study_lifecycle_transition", false, nil)
	}
	if session.StateVersion != input.ExpectedStateVersion {
		return nil, newStudyStorageError(model.StudyErrorCodeStateStale, "study session state version is stale", "study_lifecycle_transition", false, nil)
	}
	if !validStudyLifecycleTransition(session.Lifecycle, input.To) {
		return nil, newStudyStorageError(model.StudyErrorCodeStateStale, "study lifecycle transition is no longer valid", "study_lifecycle_transition", false, nil)
	}
	nextVersion := session.StateVersion + 1
	tag, err := store.tx.Exec(ctx, `
		UPDATE study_sessions
		SET study_session_lifecycle = $4,
			study_session_state_version = $5,
			study_session_terminalized_at = now(),
			study_session_terminal_profile_ingestion_high_water = (
				SELECT profile_ingestion_high_water
				FROM study_learner_profiles
				WHERE learner_profile_id = $2
			)
		WHERE study_session_id = $1
		  AND learner_profile_id = $2
		  AND study_session_state_version = $3
		  AND study_session_lifecycle = $6`,
		input.SessionID,
		store.profile.ID,
		input.ExpectedStateVersion,
		input.To,
		nextVersion,
		session.Lifecycle,
	)
	if err != nil {
		return nil, mapStudyStorageError("study_lifecycle_transition", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, newStudyStorageError(model.StudyErrorCodeStateStale, "study session state version is stale", "study_lifecycle_transition", false, nil)
	}
	if _, err := store.tx.Exec(ctx, `
		INSERT INTO study_session_lifecycle_events (
			study_session_id,
			lifecycle_from_state,
			lifecycle_to_state,
			lifecycle_reason_code,
			lifecycle_actor_ref,
			lifecycle_state_version
		) VALUES ($1, $2, $3, $4, $5, $6)`,
		input.SessionID,
		session.Lifecycle,
		input.To,
		input.ReasonCode,
		input.Actor.String(),
		nextVersion,
	); err != nil {
		return nil, mapStudyStorageError("study_lifecycle_audit_insert", err)
	}
	if store.nonterminal != nil && store.nonterminal.ID == input.SessionID {
		store.nonterminal = nil
	}
	return store.GetSession(ctx, input.SessionID)
}

func validStudyLifecycleTransition(from, to model.StudySessionLifecycle) bool {
	switch from {
	case model.StudySessionLifecycleActive, model.StudySessionLifecycleWaitingForCorpus:
		return to == model.StudySessionLifecycleAbandoned
	default:
		return false
	}
}

func (store *studyStoreTx) ListEligibleSources(ctx context.Context) ([]StudyEligibleSource, error) {
	identityRows, err := store.tx.Query(ctx, `
		SELECT idea.id
		FROM ideas idea
		JOIN names canonical_name
		  ON canonical_name.id = idea.canonical_name_id
		 AND canonical_name.idea_id = idea.id
		 AND canonical_name.kind = 'canonical'
		WHERE idea.status = 'active'
		  AND EXISTS (
			SELECT 1
			FROM idea_topics eligibility
			WHERE eligibility.idea_id = idea.id
			  AND eligibility.topic_id = ANY($1::uuid[])
		  )
		  AND btrim(canonical_name.text::text) <> ''
		  AND (
			EXISTS (
				SELECT 1 FROM definitions definition
				WHERE definition.idea_id = idea.id AND btrim(definition.body) <> ''
			)
			OR (idea.summary IS NOT NULL AND btrim(idea.summary) <> '')
		  )
		ORDER BY idea.id
		LIMIT $2
		FOR UPDATE OF idea`, store.scope.Scope.TopicIDs, study.StudyMaximumSchedulingCards+1)
	if err != nil {
		return nil, mapStudyStorageError("study_eligible_sources_lock", err)
	}
	ideaIDs, err := collectUUIDRows(identityRows)
	identityRows.Close()
	if err != nil {
		return nil, mapStudyStorageError("study_eligible_sources_lock", err)
	}
	if len(ideaIDs) > study.StudyMaximumSchedulingCards {
		return nil, newStudyStorageError(model.StudyErrorCodeRequestTooLarge, "study eligible corpus exceeds the materialization work bound", "study_eligible_sources_lock", false, nil)
	}
	if err := lockAndValidateStudyScopeTopics(ctx, store.tx, store.scope); err != nil {
		return nil, err
	}
	store.eligibleSourcesCaptured = true
	store.eligibleSources = make(map[uuid.UUID]StudyEligibleSource, len(ideaIDs))
	if len(ideaIDs) == 0 {
		store.eligibleCorpusSHA256, err = computeStudyEligibleCorpusDigest(store.scope, nil)
		if err != nil {
			return nil, mapStudyDigestStorageError("study_eligible_corpus_digest", err)
		}
		return []StudyEligibleSource{}, nil
	}

	rows, err := store.tx.Query(ctx, `
		SELECT
			idea.id,
			idea.summary,
			idea.created_at,
			idea.updated_at,
			canonical_name.id,
			canonical_name.text::text,
			canonical_name.sort_key,
			canonical_name.created_at,
			canonical_name.updated_at,
			selected_definition.id,
			coalesce(selected_definition.body, idea.summary),
			selected_definition.is_primary,
			selected_definition.sort_key,
			selected_definition.created_at,
			selected_definition.updated_at,
			selected_example.id,
			selected_example.body,
			selected_example.sort_key,
			selected_example.created_at,
			selected_example.updated_at
		FROM ideas idea
		JOIN names canonical_name
		  ON canonical_name.id = idea.canonical_name_id
		 AND canonical_name.idea_id = idea.id
		 AND canonical_name.kind = 'canonical'
		LEFT JOIN LATERAL (
			SELECT definition.id, definition.body, definition.is_primary, definition.sort_key,
				definition.created_at, definition.updated_at
			FROM definitions definition
			WHERE definition.idea_id = idea.id
			  AND btrim(definition.body) <> ''
			ORDER BY definition.is_primary DESC, definition.sort_key, definition.created_at, definition.id
			LIMIT 1
		) selected_definition ON true
		LEFT JOIN LATERAL (
			SELECT example.id, example.body, example.sort_key, example.created_at, example.updated_at
			FROM examples example
			WHERE example.idea_id = idea.id
			  AND btrim(example.body) <> ''
			ORDER BY example.sort_key, example.created_at, example.id
			LIMIT 1
		) selected_example ON true
		WHERE idea.id = ANY($1::uuid[])
		ORDER BY idea.id`, ideaIDs)
	if err != nil {
		return nil, mapStudyStorageError("study_eligible_sources_read", err)
	}
	defer rows.Close()

	sources := make([]StudyEligibleSource, 0)
	for rows.Next() {
		var source StudyEligibleSource
		if err := rows.Scan(
			&source.IdeaID,
			&source.IdeaSummary,
			&source.IdeaCreatedAt,
			&source.IdeaUpdatedAt,
			&source.CanonicalNameID,
			&source.CanonicalName,
			&source.CanonicalNameSortKey,
			&source.CanonicalNameCreatedAt,
			&source.CanonicalNameUpdatedAt,
			&source.DefinitionID,
			&source.Definition,
			&source.DefinitionIsPrimary,
			&source.DefinitionSortKey,
			&source.DefinitionCreatedAt,
			&source.DefinitionUpdatedAt,
			&source.ExampleID,
			&source.Example,
			&source.ExampleSortKey,
			&source.ExampleCreatedAt,
			&source.ExampleUpdatedAt,
		); err != nil {
			return nil, mapStudyStorageError("study_eligible_sources_read", err)
		}
		if err := materializeStudyEligibleSource(&source, store.scope.Scope.CardKind); err != nil {
			return nil, err
		}
		captured := cloneStudyEligibleSource(source)
		store.eligibleSources[source.IdeaID] = captured
		sources = append(sources, cloneStudyEligibleSource(captured))
	}
	if err := rows.Err(); err != nil {
		return nil, mapStudyStorageError("study_eligible_sources_read", err)
	}
	if len(sources) != len(ideaIDs) {
		return nil, newStudyStorageError(model.StudyErrorCodeStateStale, "study eligibility projection changed while locked", "study_eligible_sources_read", false, nil)
	}
	store.eligibleCorpusSHA256, err = computeStudyEligibleCorpusDigest(store.scope, sources)
	if err != nil {
		return nil, mapStudyDigestStorageError("study_eligible_corpus_digest", err)
	}
	return sources, nil
}

func lockAndValidateStudyScopeTopics(
	ctx context.Context,
	tx pgx.Tx,
	scope model.StudyNormalizedScope,
) error {
	rows, err := tx.Query(ctx, `
		SELECT topic.id
		FROM topics topic
		WHERE topic.id = ANY($1::uuid[])
		ORDER BY topic.id
		FOR KEY SHARE`, scope.Scope.TopicIDs)
	if err != nil {
		return mapStudyStorageError("study_scope_topics_lock", err)
	}
	lockedTopicIDs, collectErr := collectUUIDRows(rows)
	rows.Close()
	if collectErr != nil {
		return mapStudyStorageError("study_scope_topics_lock", collectErr)
	}
	if len(lockedTopicIDs) != len(scope.Scope.TopicIDs) {
		return newStudyStorageError(model.StudyErrorCodeStateStale, "study scope topic authority is unavailable", "study_scope_topics_lock", true, nil)
	}
	for index := range lockedTopicIDs {
		if lockedTopicIDs[index] != scope.Scope.TopicIDs[index] {
			return newStudyStorageError(model.StudyErrorCodeInternal, "study scope topic lock order is inconsistent", "study_scope_topics_lock", false, nil)
		}
	}
	return nil
}

func cloneStudyEligibleSource(source StudyEligibleSource) StudyEligibleSource {
	clone := source
	clone.IdeaSummary = cloneStudyStringPointer(source.IdeaSummary)
	clone.DefinitionID = cloneStudyUUIDPointer(source.DefinitionID)
	clone.DefinitionIsPrimary = cloneStudyBoolPointer(source.DefinitionIsPrimary)
	clone.DefinitionSortKey = cloneStudyFloat64Pointer(source.DefinitionSortKey)
	clone.DefinitionCreatedAt = cloneStudyTimePointer(source.DefinitionCreatedAt)
	clone.DefinitionUpdatedAt = cloneStudyTimePointer(source.DefinitionUpdatedAt)
	clone.ExampleID = cloneStudyUUIDPointer(source.ExampleID)
	clone.Example = cloneStudyStringPointer(source.Example)
	clone.ExampleSortKey = cloneStudyFloat64Pointer(source.ExampleSortKey)
	clone.ExampleCreatedAt = cloneStudyTimePointer(source.ExampleCreatedAt)
	clone.ExampleUpdatedAt = cloneStudyTimePointer(source.ExampleUpdatedAt)
	return clone
}

func cloneStudyUUIDPointer(value *uuid.UUID) *uuid.UUID {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	return &copyOfValue
}

func cloneStudyBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	return &copyOfValue
}

func cloneStudyFloat64Pointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	return &copyOfValue
}

func cloneStudyTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	return &copyOfValue
}

func computeStudyEligibleCorpusDigest(scope model.StudyNormalizedScope, sources []StudyEligibleSource) (string, error) {
	parts := make([][]byte, 0, 3+len(scope.Scope.TopicIDs)+len(sources)*4)
	parts = append(parts, studyDigestCount(len(scope.Scope.TopicIDs)))
	for _, topicID := range scope.Scope.TopicIDs {
		parts = append(parts, []byte(topicID.String()))
	}
	parts = append(parts, []byte(scope.Scope.CardKind), studyDigestCount(len(sources)))
	for index := range sources {
		source := sources[index]
		parts = append(parts,
			[]byte(source.IdeaID.String()),
			[]byte(source.IdeaID.String()),
			[]byte(source.SourceContentRevision),
			[]byte(source.SourceContentSHA256),
		)
	}
	return studydigest.Corpus(parts...)
}

func studyDigestCount(count int) []byte {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], uint32(count))
	return encoded[:]
}

func materializeStudyEligibleSource(source *StudyEligibleSource, cardKind model.StudyCardKind) error {
	definitionMarker := []byte("summary")
	definitionID := source.IdeaID.String()
	definitionRevision := studySourceRevisionTime(source.IdeaUpdatedAt)
	if source.DefinitionID != nil && source.DefinitionUpdatedAt != nil {
		definitionMarker = []byte("definition")
		definitionID = source.DefinitionID.String()
		definitionRevision = studySourceRevisionTime(*source.DefinitionUpdatedAt)
	}
	exampleMarker := []byte{0}
	exampleID := ""
	exampleRevision := ""
	if source.ExampleID != nil && source.ExampleUpdatedAt != nil {
		exampleMarker[0] = 1
		exampleID = source.ExampleID.String()
		exampleRevision = studySourceRevisionTime(*source.ExampleUpdatedAt)
	}
	sourceDigest, err := studydigest.CardSource(
		[]byte(source.IdeaID.String()),
		[]byte(studySourceRevisionTime(source.IdeaUpdatedAt)),
		[]byte(source.CanonicalNameID.String()),
		[]byte(studySourceRevisionTime(source.CanonicalNameUpdatedAt)),
		definitionMarker,
		[]byte(definitionID),
		[]byte(definitionRevision),
		exampleMarker,
		[]byte(exampleID),
		[]byte(exampleRevision),
	)
	if err != nil {
		return mapStudyDigestStorageError("study_eligible_source_digest", err)
	}
	source.SourceContentRevision = "study-card-source-v1:" + sourceDigest
	exampleValue := []byte(nil)
	if source.Example != nil {
		exampleValue = []byte(*source.Example)
	}
	source.SourceContentSHA256, err = studydigest.CardContent(
		[]byte(cardKind),
		[]byte(source.IdeaID.String()),
		[]byte(source.IdeaID.String()),
		[]byte(source.CanonicalName),
		[]byte(source.Definition),
		exampleMarker,
		exampleValue,
	)
	if err != nil {
		return mapStudyDigestStorageError("study_eligible_content_digest", err)
	}
	return nil
}

func studySourceRevisionTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func mapStudyDigestStorageError(operation string, err error) error {
	return newStudyStorageError(
		model.StudyErrorCodeInternal,
		"study persistence digest could not be computed",
		operation,
		false,
		err,
	)
}

func (store *studyStoreTx) ReadAcceptedPrefix(ctx context.Context, highWater int64) ([]model.StudyEvent, error) {
	if highWater < 0 {
		return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study prefix high-water is invalid", "study_accepted_prefix_read", false, nil)
	}
	if highWater > study.StudyProjectionAcceptedEventLimit {
		return nil, newStudyStorageError(model.StudyErrorCodeRequestTooLarge, "study accepted prefix exceeds the materialization work bound", "study_accepted_prefix_read", false, nil)
	}
	if highWater > store.profile.ProfileIngestionHighWater {
		return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study prefix high-water is invalid", "study_accepted_prefix_read", false, nil)
	}
	rows, err := store.tx.Query(ctx, `
		SELECT
			study_event_id,
			client_sequence,
			learner_profile_id,
			study_session_id,
			study_session_item_id,
			study_card_identity,
			source_content_sha256,
			study_event_kind,
			study_rating,
			rating_event_id,
			client_observed_at,
			client_clock_quality,
			client_stream_id,
			normalized_event_payload_sha256,
			profile_ingestion_ordinal,
			study_event_received_at
		FROM study_events
		WHERE learner_profile_id = $1
		  AND profile_ingestion_ordinal <= $2
		ORDER BY profile_ingestion_ordinal`, store.profile.ID, highWater)
	if err != nil {
		return nil, mapStudyStorageError("study_accepted_prefix_read", err)
	}
	defer rows.Close()

	events := make([]model.StudyEvent, 0)
	for rows.Next() {
		var event model.StudyEvent
		var kind, clockQuality string
		var rating *string
		var ingestionOrdinal int64
		var receivedAt time.Time
		if err := rows.Scan(
			&event.ID,
			&event.ClientSequence,
			&event.LearnerProfileID,
			&event.StudySessionID,
			&event.StudySessionItemID,
			&event.CardIdentity,
			&event.ContentSHA256,
			&kind,
			&rating,
			&event.RatingEventID,
			&event.ClientObservedAt,
			&clockQuality,
			&event.ClientStreamID,
			&event.NormalizedPayloadSHA256,
			&ingestionOrdinal,
			&receivedAt,
		); err != nil {
			return nil, mapStudyStorageError("study_accepted_prefix_read", err)
		}
		event.ProfileRef = store.profile.Ref
		event.Kind = model.StudyEventKind(kind)
		event.ClientClockQuality = model.StudyClientClockQuality(clockQuality)
		if rating != nil {
			converted := model.StudyRating(*rating)
			event.Rating = &converted
		}
		event.ProfileIngestionOrdinal = &ingestionOrdinal
		event.ReceivedAt = &receivedAt
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, mapStudyStorageError("study_accepted_prefix_read", err)
	}
	return events, nil
}

func (store *studyStoreTx) ResolveCardIdentity(ctx context.Context, identity uuid.UUID) (uuid.UUID, error) {
	if identity == uuid.Nil {
		return uuid.Nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study card identity is invalid", "study_card_identity_resolve", false, nil)
	}
	var survivor uuid.UUID
	err := store.tx.QueryRow(ctx, `
		WITH RECURSIVE identity_path(identity, visited, depth) AS (
			SELECT $1::uuid, ARRAY[$1::uuid], 0
			UNION ALL
			SELECT redirect.survivor_study_card_identity,
				path.visited || redirect.survivor_study_card_identity,
				path.depth + 1
			FROM identity_path path
			JOIN study_card_identity_redirects redirect
			  ON redirect.historical_study_card_identity = path.identity
			WHERE NOT redirect.survivor_study_card_identity = ANY(path.visited)
			  AND path.depth < 1024
		)
		SELECT identity
		FROM identity_path
		ORDER BY depth DESC
		LIMIT 1`, identity).Scan(&survivor)
	if err != nil {
		return uuid.Nil, mapStudyStorageError("study_card_identity_resolve", err)
	}
	return survivor, nil
}

func (store *studyStoreTx) GetProjection(ctx context.Context, key StudyProjectionKey) (*model.StudySchedulerCardProjection, error) {
	if err := validateStudyProjectionKey(key, store.profile.ID); err != nil {
		return nil, err
	}
	projection, err := scanStudyProjection(store.tx.QueryRow(ctx, studyProjectionSelect+`
		WHERE learner_profile_id = $1
		  AND projected_study_card_identity = $2
		  AND study_scheduler_name = $3
		  AND study_scheduler_version = $4
		  AND projection_schema_version = $5`,
		key.LearnerProfileID,
		key.CardIdentity,
		key.SchedulerName,
		key.SchedulerVersion,
		key.ProjectionSchemaVersion,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapStudyStorageError("study_projection_read", err)
	}
	return projection, nil
}

const studyProjectionSelect = `
	SELECT
		learner_profile_id,
		projected_study_card_identity,
		live_source_idea_id,
		study_scheduler_name,
		study_scheduler_version,
		projection_schema_version,
		latest_effective_rating_event_id,
		latest_effective_rating_evidence_card_identity,
		latest_effective_rating,
		latest_effective_rating_ingestion_ordinal,
		latest_presented_event_id,
		latest_presented_evidence_card_identity,
		latest_presented_ingestion_ordinal,
		rebuilt_through_profile_ingestion_ordinal,
		canonical_projection_payload,
		canonical_projection_sha256,
		projection_revision,
		updated_at
	FROM study_scheduler_card_projections
`

func (store *studyStoreTx) ReplaceProjection(
	ctx context.Context,
	projection model.StudySchedulerCardProjection,
	expectedRevision int64,
) (*model.StudySchedulerCardProjection, error) {
	key := StudyProjectionKey{
		LearnerProfileID:        projection.LearnerProfileID,
		CardIdentity:            projection.ProjectedCardIdentity,
		SchedulerName:           projection.SchedulerName,
		SchedulerVersion:        projection.SchedulerVersion,
		ProjectionSchemaVersion: projection.ProjectionSchemaVersion,
	}
	if err := validateStudyProjectionKey(key, store.profile.ID); err != nil {
		return nil, err
	}
	if expectedRevision < 0 || projection.RebuiltThroughProfileIngestionOrdinal < 0 ||
		projection.RebuiltThroughProfileIngestionOrdinal > store.profile.ProfileIngestionHighWater ||
		len(projection.CanonicalPayload) == 0 || !json.Valid(projection.CanonicalPayload) ||
		model.SHA256Hex(projection.CanonicalPayload) != projection.CanonicalSHA256 {
		return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study projection payload is invalid", "study_projection_replace", false, nil)
	}
	if projection.LiveSourceIdeaID != nil && *projection.LiveSourceIdeaID != projection.ProjectedCardIdentity {
		return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study projection live source identity is inconsistent", "study_projection_replace", false, nil)
	}
	if err := lockAndValidateCurrentProjectionIdentity(ctx, store, projection.ProjectedCardIdentity, expectedRevision); err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(projection.CanonicalPayload, &decoded); err != nil {
		return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study projection payload is invalid", "study_projection_replace", false, nil)
	}
	if _, ok := decoded.(map[string]any); !ok {
		return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study projection payload must be an object", "study_projection_replace", false, nil)
	}
	nextRevision := expectedRevision + 1
	projection.ProjectionRevision = nextRevision
	arguments := studyProjectionArguments(projection)
	var row pgx.Row
	if expectedRevision == 0 {
		row = store.tx.QueryRow(ctx, `
			INSERT INTO study_scheduler_card_projections (
				learner_profile_id,
				projected_study_card_identity,
				live_source_idea_id,
				study_scheduler_name,
				study_scheduler_version,
				projection_schema_version,
				latest_effective_rating_event_id,
				latest_effective_rating_evidence_card_identity,
				latest_effective_rating,
				latest_effective_rating_ingestion_ordinal,
				latest_presented_event_id,
				latest_presented_evidence_card_identity,
				latest_presented_ingestion_ordinal,
				rebuilt_through_profile_ingestion_ordinal,
				canonical_projection_payload,
				canonical_projection_sha256,
				projection_revision
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
			)
			RETURNING
				learner_profile_id,
				projected_study_card_identity,
				live_source_idea_id,
				study_scheduler_name,
				study_scheduler_version,
				projection_schema_version,
				latest_effective_rating_event_id,
				latest_effective_rating_evidence_card_identity,
				latest_effective_rating,
				latest_effective_rating_ingestion_ordinal,
				latest_presented_event_id,
				latest_presented_evidence_card_identity,
				latest_presented_ingestion_ordinal,
				rebuilt_through_profile_ingestion_ordinal,
				canonical_projection_payload,
				canonical_projection_sha256,
				projection_revision,
				updated_at`, arguments...)
	} else {
		arguments = append(arguments, expectedRevision)
		row = store.tx.QueryRow(ctx, `
			UPDATE study_scheduler_card_projections
			SET live_source_idea_id = $3,
				latest_effective_rating_event_id = $7,
				latest_effective_rating_evidence_card_identity = $8,
				latest_effective_rating = $9,
				latest_effective_rating_ingestion_ordinal = $10,
				latest_presented_event_id = $11,
				latest_presented_evidence_card_identity = $12,
				latest_presented_ingestion_ordinal = $13,
				rebuilt_through_profile_ingestion_ordinal = $14,
				canonical_projection_payload = $15,
				canonical_projection_sha256 = $16,
				projection_revision = $17
			WHERE learner_profile_id = $1
			  AND projected_study_card_identity = $2
			  AND study_scheduler_name = $4
			  AND study_scheduler_version = $5
			  AND projection_schema_version = $6
			  AND projection_revision = $18
			RETURNING
				learner_profile_id,
				projected_study_card_identity,
				live_source_idea_id,
				study_scheduler_name,
				study_scheduler_version,
				projection_schema_version,
				latest_effective_rating_event_id,
				latest_effective_rating_evidence_card_identity,
				latest_effective_rating,
				latest_effective_rating_ingestion_ordinal,
				latest_presented_event_id,
				latest_presented_evidence_card_identity,
				latest_presented_ingestion_ordinal,
				rebuilt_through_profile_ingestion_ordinal,
				canonical_projection_payload,
				canonical_projection_sha256,
				projection_revision,
				updated_at`, arguments...)
	}
	replaced, err := scanStudyProjection(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, newStudyStorageError(model.StudyErrorCodeStateStale, "study projection revision is stale", "study_projection_replace", false, nil)
	}
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "23505" {
			return nil, newStudyStorageError(model.StudyErrorCodeStateStale, "study projection revision is stale", "study_projection_replace", false, err)
		}
		return nil, mapStudyStorageError("study_projection_replace", err)
	}
	return replaced, nil
}

func lockAndValidateCurrentProjectionIdentity(ctx context.Context, store *studyStoreTx, identity uuid.UUID, expectedRevision int64) error {
	var status string
	err := store.tx.QueryRow(ctx, `
		SELECT status
		FROM ideas
		WHERE id = $1
		FOR KEY SHARE`, identity).Scan(&status)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return mapStudyStorageError("study_projection_identity_lock", err)
	}
	if errors.Is(err, pgx.ErrNoRows) && expectedRevision == 0 {
		return newStudyStorageError(model.StudyErrorCodeStateStale, "study projection card identity is unavailable", "study_projection_identity_lock", false, nil)
	}
	if err == nil && model.IdeaStatus(status) == model.IdeaStatusMerged {
		return newStudyStorageError(model.StudyErrorCodeStateStale, "study projection card identity is no longer current", "study_projection_identity_lock", false, nil)
	}
	survivor, resolveErr := store.ResolveCardIdentity(ctx, identity)
	if resolveErr != nil {
		return resolveErr
	}
	if survivor != identity {
		return newStudyStorageError(model.StudyErrorCodeStateStale, "study projection card identity is no longer current", "study_projection_identity_lock", false, nil)
	}
	return nil
}

func validateStudyProjectionKey(key StudyProjectionKey, lockedProfileID uuid.UUID) error {
	if key.LearnerProfileID == uuid.Nil || key.LearnerProfileID != lockedProfileID || key.CardIdentity == uuid.Nil ||
		!boundedStudyStorageText(key.SchedulerName, 128) || !boundedStudyStorageText(key.SchedulerVersion, 64) ||
		key.ProjectionSchemaVersion < 1 {
		return newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "study projection key is invalid", "study_projection", false, nil)
	}
	return nil
}

func studyProjectionArguments(projection model.StudySchedulerCardProjection) []any {
	var rating any
	if projection.LatestEffectiveRating != nil {
		rating = string(*projection.LatestEffectiveRating)
	}
	return []any{
		projection.LearnerProfileID,
		projection.ProjectedCardIdentity,
		projection.LiveSourceIdeaID,
		projection.SchedulerName,
		projection.SchedulerVersion,
		projection.ProjectionSchemaVersion,
		projection.LatestEffectiveRatingEventID,
		projection.LatestEffectiveRatingEvidenceCardIdentity,
		rating,
		projection.LatestEffectiveRatingIngestionOrdinal,
		projection.LatestPresentedEventID,
		projection.LatestPresentedEvidenceCardIdentity,
		projection.LatestPresentedIngestionOrdinal,
		projection.RebuiltThroughProfileIngestionOrdinal,
		string(projection.CanonicalPayload),
		projection.CanonicalSHA256,
		projection.ProjectionRevision,
	}
}

func scanStudyProjection(row pgx.Row) (*model.StudySchedulerCardProjection, error) {
	var projection model.StudySchedulerCardProjection
	var payload string
	var rating *string
	if err := row.Scan(
		&projection.LearnerProfileID,
		&projection.ProjectedCardIdentity,
		&projection.LiveSourceIdeaID,
		&projection.SchedulerName,
		&projection.SchedulerVersion,
		&projection.ProjectionSchemaVersion,
		&projection.LatestEffectiveRatingEventID,
		&projection.LatestEffectiveRatingEvidenceCardIdentity,
		&rating,
		&projection.LatestEffectiveRatingIngestionOrdinal,
		&projection.LatestPresentedEventID,
		&projection.LatestPresentedEvidenceCardIdentity,
		&projection.LatestPresentedIngestionOrdinal,
		&projection.RebuiltThroughProfileIngestionOrdinal,
		&payload,
		&projection.CanonicalSHA256,
		&projection.ProjectionRevision,
		&projection.UpdatedAt,
	); err != nil {
		return nil, err
	}
	projection.CanonicalPayload = json.RawMessage(payload)
	if rating != nil {
		converted := model.StudyRating(*rating)
		projection.LatestEffectiveRating = &converted
	}
	return &projection, nil
}

func newStudyStorageError(code model.StudyErrorCode, message, operation string, retryable bool, cause error) *model.StudyError {
	return &model.StudyError{
		Code:      code,
		Message:   message,
		Context:   map[string]string{"operation": operation},
		Retryable: retryable,
		Cause:     cause,
	}
}

func mapStudyStorageError(operation string, err error) error {
	if err == nil {
		return nil
	}
	var studyError *model.StudyError
	if errors.As(err, &studyError) {
		return studyError
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "40001", "40P01", "08000", "08003", "08006", "08001":
			return newStudyStorageError(model.StudyErrorCodeServiceUnavailable, "study persistence is temporarily unavailable", operation, true, err)
		}
	}
	return newStudyStorageError(model.StudyErrorCodeInternal, "study persistence operation failed", operation, false, err)
}

// Keep compile-time assertions close to the abstraction boundary.
var _ StudyStoreTransaction = (*studyStoreTx)(nil)
