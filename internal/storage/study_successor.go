package storage

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lexinet/internal/model"
)

// StudySuccessorPlanInsert contains the purpose-specific evidence required to
// replace one authoritative nonterminal cycle. Plan.PredecessorSessionID names
// that cycle. PredecessorFlushReceiptID is required for an active predecessor
// and must be zero for a waiting-for-corpus predecessor.
type StudySuccessorPlanInsert struct {
	Plan                          StudyPlanInsert
	PredecessorFlushReceiptID     uuid.UUID
	PredecessorTerminalReasonCode string
}

type studySuccessorFlushBarrier struct {
	StudyPrincipalID                 string
	LearnerProfileID                 uuid.UUID
	StudySessionID                   uuid.UUID
	ClientStreamID                   uuid.UUID
	AcknowledgedThroughSequence      int64
	ProfileIngestionHighWater        int64
	StudySessionStateVersion         int64
	StreamAdjudicatedThroughSequence int64
}

// InsertSuccessorPlanOnce is the canonical storage operation for ending one
// cycle and linking its unique successor. It must run inside
// WithStudyScopeTransaction after ListEligibleSources captured the next corpus
// whenever no receipt or linked successor already makes the result immutable.
//
// @canonical study_successor_plan -- all predecessor completion and
// waiting-sentinel replacement must use this operation.
func (store *studyStoreTx) InsertSuccessorPlanOnce(
	ctx context.Context,
	input StudySuccessorPlanInsert,
) (*StudyPlanInsertResult, error) {
	if err := validateStudySuccessorReceiptIdentity(input.Plan); err != nil {
		return nil, err
	}

	// Idempotency is intentionally adjudicated before reading mutable
	// predecessor, flush, stream, or corpus state. Once issued, a receipt is the
	// authority even after later accepted evidence advances the live envelope.
	receipt, err := readStudyCreationReceipt(
		ctx,
		store.tx,
		store.profile.ID,
		store.scope,
		input.Plan.Principal,
		input.Plan.CreationRequestID,
	)
	if err != nil {
		return nil, err
	}
	if receipt != nil {
		if receipt.NormalizedRequestSHA256 != input.Plan.NormalizedRequestSHA256 {
			return nil, newStudyStorageError(
				model.StudyErrorCodeIdempotencyConflict,
				"study creation request identity is already bound to different content",
				"study_successor_insert",
				false,
				nil,
			)
		}
		return store.planInsertResult(ctx, *receipt, false)
	}

	predecessorID, err := validateStudySuccessorPredecessorIdentity(input.Plan)
	if err != nil {
		return nil, err
	}
	predecessor, err := queryStudySession(ctx, store.tx, predecessorID, store.profile.ID, "", true)
	if err != nil {
		return nil, err
	}
	if !sameStudyScope(predecessor.Scope, store.scope) {
		return nil, newStudyStorageError(
			model.StudyErrorCodeSessionNotFound,
			"study predecessor session was not found in the active scope",
			"study_successor_insert",
			false,
			nil,
		)
	}

	// A distinct creation UUID arriving after another creator committed does
	// not re-run a now-stale flush barrier or rematerialize a later corpus. The
	// predecessor link is already authoritative and receives its own immutable
	// creation receipt for this UUID.
	linkedSuccessor, err := store.FindSuccessor(ctx, predecessorID)
	if err != nil {
		return nil, err
	}
	if linkedSuccessor != nil {
		return store.bindStudySuccessorCreationReceipt(
			ctx,
			input.Plan,
			linkedSuccessor.ID,
			model.StudyCreationDispositionSuccessor,
			false,
		)
	}

	if store.nonterminal == nil || store.nonterminal.ID != predecessor.ID || predecessor.Lifecycle.Terminal() {
		return nil, newStudyStorageError(
			model.StudyErrorCodeStateStale,
			"study predecessor is no longer the authoritative nonterminal session",
			"study_successor_insert",
			false,
			nil,
		)
	}
	if !boundedStudyStorageText(input.PredecessorTerminalReasonCode, 128) {
		return nil, newStudyStorageError(
			model.StudyErrorCodeSchemaInvalid,
			"study predecessor terminal reason is invalid",
			"study_successor_insert",
			false,
			nil,
		)
	}

	terminalLifecycle, err := store.validateStudySuccessorBarrier(ctx, input, *predecessor)
	if err != nil {
		return nil, err
	}

	boundPlan, err := bindStudyPlanToEligibleSnapshot(store, input.Plan)
	if err != nil {
		return nil, err
	}
	if err := validateStudySuccessorPlan(store, boundPlan); err != nil {
		return nil, err
	}
	if err := lockAndValidateStudyPlanSources(ctx, store.tx, store.scope, boundPlan); err != nil {
		return nil, err
	}

	if predecessor.Lifecycle == model.StudySessionLifecycleWaitingForCorpus && len(store.eligibleSources) == 0 {
		if predecessor.CorpusSHA256 != store.eligibleCorpusSHA256 {
			return nil, newStudyStorageError(
				model.StudyErrorCodeInternal,
				"waiting study sentinel does not match its captured empty corpus",
				"study_successor_insert",
				false,
				nil,
			)
		}
		return store.bindStudySuccessorCreationReceipt(
			ctx,
			boundPlan,
			predecessor.ID,
			model.StudyCreationDispositionConvergedToExisting,
			false,
		)
	}

	if err := store.terminalizeStudyPredecessor(
		ctx,
		*predecessor,
		terminalLifecycle,
		input.PredecessorTerminalReasonCode,
		input.Plan.Principal,
	); err != nil {
		return nil, err
	}
	if err := store.insertStudySuccessorPlan(ctx, boundPlan, predecessor.ID); err != nil {
		return nil, err
	}

	return store.bindStudySuccessorCreationReceipt(
		ctx,
		boundPlan,
		boundPlan.SessionID,
		model.StudyCreationDispositionSuccessor,
		true,
	)
}

func validateStudySuccessorReceiptIdentity(plan StudyPlanInsert) error {
	if plan.CreationRequestID == uuid.Nil || !plan.Principal.Valid() || !model.IsStudySHA256(plan.NormalizedRequestSHA256) {
		return newStudyStorageError(
			model.StudyErrorCodeSchemaInvalid,
			"study successor creation identity is invalid",
			"study_successor_insert",
			false,
			nil,
		)
	}
	return nil
}

func validateStudySuccessorPredecessorIdentity(plan StudyPlanInsert) (uuid.UUID, error) {
	if plan.PredecessorSessionID == nil || *plan.PredecessorSessionID == uuid.Nil {
		return uuid.Nil, newStudyStorageError(
			model.StudyErrorCodeSchemaInvalid,
			"study successor predecessor identity is required",
			"study_successor_insert",
			false,
			nil,
		)
	}
	predecessorID := *plan.PredecessorSessionID
	if plan.SessionID != uuid.Nil && plan.SessionID == predecessorID {
		return uuid.Nil, newStudyStorageError(
			model.StudyErrorCodeSchemaInvalid,
			"study successor cannot identify itself as predecessor",
			"study_successor_insert",
			false,
			nil,
		)
	}
	return predecessorID, nil
}

func validateStudySuccessorPlan(store *studyStoreTx, plan StudyPlanInsert) error {
	if plan.Scheduler != nil && plan.Scheduler.InputProfileIngestionHighWater != store.profile.ProfileIngestionHighWater {
		return newStudyStorageError(
			model.StudyErrorCodeStateStale,
			"study successor planning high-water is stale",
			"study_successor_insert",
			false,
			nil,
		)
	}
	planWithoutPredecessor := plan
	planWithoutPredecessor.PredecessorSessionID = nil
	return validateStudyPlanInsert(store, planWithoutPredecessor)
}

func (store *studyStoreTx) validateStudySuccessorBarrier(
	ctx context.Context,
	input StudySuccessorPlanInsert,
	predecessor model.StudySession,
) (model.StudySessionLifecycle, error) {
	switch predecessor.Lifecycle {
	case model.StudySessionLifecycleActive:
		if input.PredecessorFlushReceiptID == uuid.Nil {
			return "", newStudyStorageError(
				model.StudyErrorCodeFlushReceiptRequired,
				"active study predecessor requires an owned flush receipt",
				"study_successor_flush_barrier",
				false,
				nil,
			)
		}
		if err := store.validateStudySuccessorFlushReceipt(ctx, input, predecessor); err != nil {
			return "", err
		}
		if err := store.requireEveryStudyItemPresented(ctx, predecessor.ID); err != nil {
			return "", err
		}
		return model.StudySessionLifecycleCompleted, nil
	case model.StudySessionLifecycleWaitingForCorpus:
		if input.PredecessorFlushReceiptID != uuid.Nil {
			return "", newStudyStorageError(
				model.StudyErrorCodeSchemaInvalid,
				"waiting study predecessor replacement does not accept a flush receipt",
				"study_successor_flush_barrier",
				false,
				nil,
			)
		}
		return model.StudySessionLifecycleSupersededEmpty, nil
	default:
		return "", newStudyStorageError(
			model.StudyErrorCodeStateStale,
			"study predecessor lifecycle is no longer replaceable",
			"study_successor_insert",
			false,
			nil,
		)
	}
}

func (store *studyStoreTx) validateStudySuccessorFlushReceipt(
	ctx context.Context,
	input StudySuccessorPlanInsert,
	predecessor model.StudySession,
) error {
	var barrier studySuccessorFlushBarrier
	err := store.tx.QueryRow(ctx, `
		SELECT
			flush.study_principal_id,
			flush.learner_profile_id,
			flush.study_session_id,
			flush.client_stream_id,
			flush.acknowledged_through_client_sequence,
			flush.profile_ingestion_high_water,
			flush.study_session_state_version,
			stream.adjudicated_through_client_sequence
		FROM study_flush_receipts flush
		JOIN study_client_streams stream
		  ON stream.client_stream_id = flush.client_stream_id
		 AND stream.learner_profile_id = flush.learner_profile_id
		WHERE flush.study_flush_receipt_id = $1
		FOR UPDATE OF stream`, input.PredecessorFlushReceiptID).Scan(
		&barrier.StudyPrincipalID,
		&barrier.LearnerProfileID,
		&barrier.StudySessionID,
		&barrier.ClientStreamID,
		&barrier.AcknowledgedThroughSequence,
		&barrier.ProfileIngestionHighWater,
		&barrier.StudySessionStateVersion,
		&barrier.StreamAdjudicatedThroughSequence,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return newStudyStorageError(
			model.StudyErrorCodeFlushReceiptStale,
			"study predecessor flush receipt is unavailable",
			"study_successor_flush_barrier",
			false,
			nil,
		)
	}
	if err != nil {
		return mapStudyStorageError("study_successor_flush_barrier", err)
	}
	if barrier.StudyPrincipalID != input.Plan.Principal.String() ||
		barrier.LearnerProfileID != store.profile.ID ||
		barrier.StudySessionID != predecessor.ID ||
		barrier.ProfileIngestionHighWater != store.profile.ProfileIngestionHighWater ||
		barrier.StudySessionStateVersion != predecessor.StateVersion ||
		barrier.AcknowledgedThroughSequence != barrier.StreamAdjudicatedThroughSequence {
		return newStudyStorageError(
			model.StudyErrorCodeFlushReceiptStale,
			"study predecessor flush receipt is stale or not owned by this request",
			"study_successor_flush_barrier",
			false,
			nil,
		)
	}
	return nil
}

func (store *studyStoreTx) requireEveryStudyItemPresented(ctx context.Context, sessionID uuid.UUID) error {
	var unpresentedItemCount int64
	if err := store.tx.QueryRow(ctx, `
		SELECT count(*)
		FROM study_session_items item
		WHERE item.study_session_id = $1
		  AND NOT EXISTS (
			SELECT 1
			FROM study_events event
			WHERE event.study_session_id = item.study_session_id
			  AND event.study_session_item_id = item.study_session_item_id
			  AND event.study_card_identity = item.study_card_identity
			  AND event.source_content_sha256 = item.source_content_sha256
			  AND event.study_event_kind = 'presented'
		  )`, sessionID).Scan(&unpresentedItemCount); err != nil {
		return mapStudyStorageError("study_successor_exhaustion_read", err)
	}
	if unpresentedItemCount != 0 {
		return newStudyStorageError(
			model.StudyErrorCodeCycleNotExhausted,
			"study predecessor still has an issued item without a canonical presented fact",
			"study_successor_exhaustion_read",
			false,
			nil,
		)
	}
	return nil
}

func (store *studyStoreTx) terminalizeStudyPredecessor(
	ctx context.Context,
	predecessor model.StudySession,
	terminalLifecycle model.StudySessionLifecycle,
	reasonCode string,
	actor model.StudyPrincipalID,
) error {
	nextStateVersion := predecessor.StateVersion + 1
	commandTag, err := store.tx.Exec(ctx, `
		UPDATE study_sessions
		SET study_session_lifecycle = $4,
			study_session_state_version = $5,
			study_session_terminalized_at = now(),
			study_session_terminal_profile_ingestion_high_water = $6
		WHERE study_session_id = $1
		  AND learner_profile_id = $2
		  AND study_session_state_version = $3
		  AND study_session_lifecycle = $7`,
		predecessor.ID,
		store.profile.ID,
		predecessor.StateVersion,
		terminalLifecycle,
		nextStateVersion,
		store.profile.ProfileIngestionHighWater,
		predecessor.Lifecycle,
	)
	if err != nil {
		return mapStudyStorageError("study_successor_predecessor_terminalize", err)
	}
	if commandTag.RowsAffected() != 1 {
		return newStudyStorageError(
			model.StudyErrorCodeStateStale,
			"study predecessor state changed before terminalization",
			"study_successor_predecessor_terminalize",
			false,
			nil,
		)
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
		predecessor.ID,
		predecessor.Lifecycle,
		terminalLifecycle,
		reasonCode,
		actor.String(),
		nextStateVersion,
	); err != nil {
		return mapStudyStorageError("study_successor_predecessor_audit_insert", err)
	}
	if store.nonterminal != nil && store.nonterminal.ID == predecessor.ID {
		store.nonterminal = nil
	}
	return nil
}

func (store *studyStoreTx) insertStudySuccessorPlan(
	ctx context.Context,
	plan StudyPlanInsert,
	predecessorID uuid.UUID,
) error {
	if _, err := store.tx.Exec(ctx, `
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
			study_session_state_version
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9,
			$10, $11, $12, $13, $14, $15, $16, 1
		)`,
		plan.SessionID,
		store.profile.ID,
		string(store.scope.CanonicalSelector),
		store.scope.CanonicalSelectorHash,
		store.scope.Scope.TopicIDs,
		store.scope.Scope.TopicRefs,
		store.scope.Scope.CardKind,
		plan.Lifecycle,
		plan.CorpusSHA256,
		studySchedulerName(plan.Scheduler),
		studySchedulerVersion(plan.Scheduler),
		studySchedulerConfigurationSHA(plan.Scheduler),
		studySchedulerHighWater(plan.Scheduler),
		studySchedulerPlannedAt(plan.Scheduler),
		studySchedulerSeed(plan.Scheduler),
		predecessorID,
	); err != nil {
		return mapStudyStorageError("study_successor_session_insert", err)
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
		plan.SessionID,
		plan.Lifecycle,
		plan.LifecycleReasonCode,
		plan.Principal.String(),
	); err != nil {
		return mapStudyStorageError("study_successor_lifecycle_audit_insert", err)
	}
	for index := range plan.Items {
		if err := insertStudySessionItem(ctx, store.tx, plan.Items[index]); err != nil {
			return err
		}
	}
	created, err := store.GetSession(ctx, plan.SessionID)
	if err != nil {
		return err
	}
	store.nonterminal = created
	return nil
}

func (store *studyStoreTx) bindStudySuccessorCreationReceipt(
	ctx context.Context,
	plan StudyPlanInsert,
	authoritativeSessionID uuid.UUID,
	disposition model.StudyCreationDisposition,
	created bool,
) (*StudyPlanInsertResult, error) {
	receipt, err := insertStudyCreationReceipt(
		ctx,
		store.tx,
		store.profile.ID,
		store.scope,
		plan.Principal,
		plan.CreationRequestID,
		plan.NormalizedRequestSHA256,
		authoritativeSessionID,
		disposition,
	)
	if err != nil {
		return nil, err
	}
	return store.planInsertResult(ctx, *receipt, created)
}
