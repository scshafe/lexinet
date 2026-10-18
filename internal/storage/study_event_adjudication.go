package storage

import (
	"bytes"
	"context"
	"errors"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lexinet/internal/model"
)

// StudyEventSyncInput is the storage-authoritative portion of one normalized
// study.v1 sync request. The composition layer supplies exact typed page
// bytes; this operation binds them atomically to the immutable flush barrier.
type StudyEventSyncInput struct {
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

// StudyEventSyncResult contains immutable receipt authority plus the live
// session envelope after the transaction. Replayed is operational metadata and
// does not weaken receipt/flush immutability.
type StudyEventSyncResult struct {
	Receipts     []model.StudyEventReceipt
	FlushReceipt model.StudyFlushReceipt
	Session      model.StudySession
	PageArtifact model.StudySessionPageArtifact
	Replayed     bool
}

type normalizedStudyEventSyncInput struct {
	Principal               model.StudyPrincipalID
	ProfileRef              string
	SessionID               uuid.UUID
	SyncRequestID           uuid.UUID
	NormalizedRequestSHA256 string
	ClientStreamID          uuid.UUID
	ExpectedStateVersion    int64
	Events                  []normalizedStudySyncEvent
	PageArtifact            model.StudySessionPageArtifact
}

type normalizedStudySyncEvent struct {
	Event         model.StudyEvent
	PayloadSHA256 string
}

type studyEventBindingAuthority struct {
	EventID             uuid.UUID
	BoundPayloadSHA256  string
	LearnerProfileID    uuid.UUID
	StudySessionID      uuid.UUID
	StudySessionItemID  uuid.UUID
	CardIdentity        uuid.UUID
	ContentSHA256       string
	FirstClientStreamID uuid.UUID
	FirstClientSequence int64
}

type studyCanonicalEventRef struct {
	ID      uuid.UUID
	Ordinal int64
}

type studyItemEventState struct {
	Item            model.StudySessionItem
	Presented       *studyCanonicalEventRef
	Revealed        *studyCanonicalEventRef
	EffectiveRating *studyCanonicalEventRef
	Ratings         map[uuid.UUID]*studyCanonicalEventRef
}

type studySuffixDecision struct {
	Input          normalizedStudySyncEvent
	Binding        studyEventBindingAuthority
	BindingMatches bool
	Disposition    model.StudyEventReceiptDisposition
	Code           model.StudyEventReceiptCode
	Canonical      *studyCanonicalEventRef
}

// AdjudicateStudyEvents reconciles an immutable replay prefix and at most one
// new contiguous suffix. All new terminal receipts share one transaction with
// stream/session advancement and accepted profile ordinals.
func (repository *Repository) AdjudicateStudyEvents(
	ctx context.Context,
	input StudyEventSyncInput,
) (*StudyEventSyncResult, error) {
	normalized, err := normalizeStudyEventSyncInput(input)
	if err != nil {
		return nil, err
	}

	tx, err := repository.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return nil, mapStudyStorageError("study_event_sync_begin", err)
	}
	defer tx.Rollback(ctx)

	profile, err := lockExistingStudyProfile(ctx, tx, normalized.ProfileRef)
	if err != nil {
		return nil, err
	}
	session, err := queryStudySession(ctx, tx, normalized.SessionID, profile.ID, "", true)
	if err != nil {
		return nil, err
	}
	stream, err := lockStudyClientStream(ctx, tx, profile.ID, normalized.ClientStreamID, true)
	if err != nil {
		return nil, err
	}

	existingFlush, err := readStudyFlushReceipt(
		ctx,
		tx,
		normalized.Principal,
		normalized.SessionID,
		normalized.SyncRequestID,
	)
	if err != nil {
		return nil, err
	}
	if existingFlush != nil {
		result, replayErr := replayStudyEventSync(ctx, tx, normalized, *session, *existingFlush)
		if replayErr != nil {
			return nil, replayErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return nil, mapStudyStorageError("study_event_sync_replay_commit", commitErr)
		}
		return result, nil
	}
	if err := model.ValidateStudySessionPageArtifact(normalized.PageArtifact); err != nil {
		return nil, err
	}
	normalized.PageArtifact = cloneStudySessionPageArtifact(normalized.PageArtifact)

	if session.StateVersion != normalized.ExpectedStateVersion {
		return nil, newStudyStorageError(
			model.StudyErrorCodeStateStale,
			"study session state version is stale",
			"study_event_sync",
			false,
			nil,
		)
	}

	replayPrefix, newSuffix, err := reconcileStudyEventWindow(
		ctx,
		tx,
		normalized.Events,
		*stream,
		profile.ID,
		session.ID,
	)
	if err != nil {
		return nil, err
	}

	result, err := adjudicateStudyEventSuffix(ctx, tx, normalized, *profile, *session, *stream, replayPrefix, newSuffix)
	if err != nil {
		return nil, err
	}
	if repository.studyEventBeforeCommit != nil {
		if err := repository.studyEventBeforeCommit(ctx, tx); err != nil {
			return nil, mapStudyStorageError("study_event_before_commit", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapStudyStorageError("study_event_sync_commit", err)
	}
	return result, nil
}

func normalizeStudyEventSyncInput(input StudyEventSyncInput) (normalizedStudyEventSyncInput, error) {
	if !input.Principal.Valid() || !validStudyStorageReference(input.ProfileRef) ||
		input.SessionID == uuid.Nil || input.SyncRequestID == uuid.Nil || input.ClientStreamID == uuid.Nil ||
		input.ExpectedStateVersion < 1 || !model.IsStudySHA256(input.NormalizedRequestSHA256) {
		return normalizedStudyEventSyncInput{}, newStudyStorageError(
			model.StudyErrorCodeSchemaInvalid,
			"study event sync identity is invalid",
			"study_event_sync_normalize",
			false,
			nil,
		)
	}
	if len(input.Events) > model.StudyMaximumEvents {
		return normalizedStudyEventSyncInput{}, newStudyStorageError(
			model.StudyErrorCodeRequestTooLarge,
			"study event sync exceeds the event bound",
			"study_event_sync_normalize",
			false,
			nil,
		)
	}
	normalized := normalizedStudyEventSyncInput{
		Principal:               input.Principal,
		ProfileRef:              input.ProfileRef,
		SessionID:               input.SessionID,
		SyncRequestID:           input.SyncRequestID,
		NormalizedRequestSHA256: input.NormalizedRequestSHA256,
		ClientStreamID:          input.ClientStreamID,
		ExpectedStateVersion:    input.ExpectedStateVersion,
		Events:                  make([]normalizedStudySyncEvent, len(input.Events)),
		PageArtifact:            input.PageArtifact,
	}
	for index := range input.Events {
		event := cloneStudySyncEvent(input.Events[index])
		if event.ProfileRef != input.ProfileRef {
			return normalizedStudyEventSyncInput{}, newStudyStorageError(
				model.StudyErrorCodeSchemaInvalid,
				"study event profile does not match the sync profile",
				"study_event_sync_normalize",
				false,
				nil,
			)
		}
		event.LearnerProfileID = uuid.Nil
		event.StudySessionID = input.SessionID
		event.ClientStreamID = input.ClientStreamID
		event.NormalizedPayloadSHA256 = ""
		event.ProfileIngestionOrdinal = nil
		event.ReceivedAt = nil
		_, payloadSHA256, err := model.NormalizeStudyEventPayload(event)
		if err != nil {
			return normalizedStudyEventSyncInput{}, err
		}
		event.NormalizedPayloadSHA256 = payloadSHA256
		normalized.Events[index] = normalizedStudySyncEvent{Event: event, PayloadSHA256: payloadSHA256}
		if index > 0 && event.ClientSequence != normalized.Events[index-1].Event.ClientSequence+1 {
			return normalizedStudyEventSyncInput{}, newStudyStorageError(
				model.StudyErrorCodeStreamSequenceGap,
				"study event window is not contiguous",
				"study_event_sync_normalize",
				false,
				nil,
			)
		}
	}
	return normalized, nil
}

func cloneStudySessionPageArtifact(source model.StudySessionPageArtifact) model.StudySessionPageArtifact {
	clone := source
	clone.CanonicalJSON = append([]byte(nil), source.CanonicalJSON...)
	return clone
}

func cloneStudySyncEvent(source model.StudyEvent) model.StudyEvent {
	clone := source
	if source.Rating != nil {
		value := *source.Rating
		clone.Rating = &value
	}
	clone.RatingEventID = cloneStudyUUIDPointer(source.RatingEventID)
	clone.ClientObservedAt = cloneStudyTimePointer(source.ClientObservedAt)
	return clone
}

func lockExistingStudyProfile(ctx context.Context, tx pgx.Tx, profileRef string) (*model.StudyLearnerProfile, error) {
	profile, err := scanStudyProfile(tx.QueryRow(ctx, `
		SELECT learner_profile_id, learner_profile_ref, profile_ingestion_high_water, created_at, updated_at
		FROM study_learner_profiles
		WHERE learner_profile_ref = $1
		FOR UPDATE`, profileRef))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, newStudyStorageError(
			model.StudyErrorCodeProfileNotFound,
			"study learner profile was not found",
			"study_event_profile_lock",
			false,
			nil,
		)
	}
	if err != nil {
		return nil, mapStudyStorageError("study_event_profile_lock", err)
	}
	return profile, nil
}

func lockStudyClientStream(
	ctx context.Context,
	tx pgx.Tx,
	profileID, streamID uuid.UUID,
	create bool,
) (*model.StudyClientStream, error) {
	if create {
		if _, err := tx.Exec(ctx, `
			INSERT INTO study_client_streams (client_stream_id, learner_profile_id)
			VALUES ($1, $2)
			ON CONFLICT (client_stream_id) DO NOTHING`, streamID, profileID); err != nil {
			return nil, mapStudyStorageError("study_event_stream_provision", err)
		}
	}
	var stream model.StudyClientStream
	if err := tx.QueryRow(ctx, `
		SELECT client_stream_id, learner_profile_id, adjudicated_through_client_sequence, created_at, updated_at
		FROM study_client_streams
		WHERE client_stream_id = $1
		FOR UPDATE`, streamID).Scan(
		&stream.ID,
		&stream.LearnerProfileID,
		&stream.AdjudicatedThroughSequence,
		&stream.CreatedAt,
		&stream.UpdatedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return nil, newStudyStorageError(
			model.StudyErrorCodeStreamSequenceGap,
			"study client stream is unavailable",
			"study_event_stream_lock",
			false,
			nil,
		)
	} else if err != nil {
		return nil, mapStudyStorageError("study_event_stream_lock", err)
	}
	if stream.LearnerProfileID != profileID {
		return nil, newStudyStorageError(
			model.StudyErrorCodeAuthorizationDenied,
			"study client stream is not authorized for this learner",
			"study_event_stream_lock",
			false,
			nil,
		)
	}
	return &stream, nil
}

func replayStudyEventSync(
	ctx context.Context,
	tx pgx.Tx,
	input normalizedStudyEventSyncInput,
	session model.StudySession,
	flush model.StudyFlushReceipt,
) (*StudyEventSyncResult, error) {
	if flush.NormalizedSyncRequestSHA256 != input.NormalizedRequestSHA256 || flush.ClientStreamID != input.ClientStreamID {
		return nil, newStudyStorageError(
			model.StudyErrorCodeIdempotencyConflict,
			"study sync request identity is already bound to different content",
			"study_event_sync_replay",
			false,
			nil,
		)
	}
	requestFirst, requestLast := studySyncRequestSequenceRange(input.Events)
	if !sameStudyOptionalInt64(flush.RequestFirstClientSequence, requestFirst) ||
		!sameStudyOptionalInt64(flush.RequestLastClientSequence, requestLast) {
		return nil, newStudyStorageError(
			model.StudyErrorCodeIdempotencyConflict,
			"study sync request event window differs from its immutable flush receipt",
			"study_event_sync_replay",
			false,
			nil,
		)
	}
	if !flush.SessionPageArtifactRequired {
		return nil, newStudyStorageError(
			model.StudyErrorCodeInternal,
			"legacy study sync receipt has no exact page replay authority",
			"study_event_sync_replay",
			false,
			nil,
		)
	}
	receipts, err := readAndMatchStudyEventReceipts(
		ctx,
		tx,
		input.ClientStreamID,
		session.LearnerProfileID,
		session.ID,
		input.Events,
	)
	if err != nil {
		return nil, err
	}
	pageArtifact, err := readStudySessionPageArtifact(ctx, tx, flush.ID)
	if err != nil {
		return nil, err
	}
	refreshed, err := queryStudySession(ctx, tx, session.ID, session.LearnerProfileID, "", false)
	if err != nil {
		return nil, err
	}
	return &StudyEventSyncResult{
		Receipts:     receipts,
		FlushReceipt: flush,
		Session:      *refreshed,
		PageArtifact: *pageArtifact,
		Replayed:     true,
	}, nil
}

func reconcileStudyEventWindow(
	ctx context.Context,
	tx pgx.Tx,
	events []normalizedStudySyncEvent,
	stream model.StudyClientStream,
	profileID, sessionID uuid.UUID,
) ([]model.StudyEventReceipt, []normalizedStudySyncEvent, error) {
	replayCount := 0
	for replayCount < len(events) && events[replayCount].Event.ClientSequence <= stream.AdjudicatedThroughSequence {
		replayCount++
	}
	replay, err := readAndMatchStudyEventReceipts(ctx, tx, stream.ID, profileID, sessionID, events[:replayCount])
	if err != nil {
		return nil, nil, err
	}
	newSuffix := events[replayCount:]
	if len(newSuffix) > 0 && newSuffix[0].Event.ClientSequence != stream.AdjudicatedThroughSequence+1 {
		return nil, nil, newStudyStorageError(
			model.StudyErrorCodeStreamSequenceGap,
			"study event suffix does not begin at the next stream sequence",
			"study_event_sync_reconcile",
			false,
			nil,
		)
	}
	return replay, newSuffix, nil
}

func readAndMatchStudyEventReceipts(
	ctx context.Context,
	tx pgx.Tx,
	streamID uuid.UUID,
	profileID uuid.UUID,
	sessionID uuid.UUID,
	events []normalizedStudySyncEvent,
) ([]model.StudyEventReceipt, error) {
	receipts := make([]model.StudyEventReceipt, 0, len(events))
	for index := range events {
		receipt, err := readStudyEventReceiptAt(ctx, tx, streamID, events[index].Event.ClientSequence)
		if err != nil {
			return nil, err
		}
		if receipt == nil ||
			receipt.LearnerProfileID != profileID ||
			receipt.StudySessionID != sessionID ||
			receipt.EventID != events[index].Event.ID ||
			receipt.PayloadSHA256 != events[index].PayloadSHA256 {
			return nil, newStudyStorageError(
				model.StudyErrorCodeIdempotencyConflict,
				"study replay prefix differs from its immutable receipt",
				"study_event_sync_reconcile",
				false,
				nil,
			)
		}
		receipts = append(receipts, *receipt)
	}
	return receipts, nil
}

func adjudicateStudyEventSuffix(
	ctx context.Context,
	tx pgx.Tx,
	input normalizedStudyEventSyncInput,
	profile model.StudyLearnerProfile,
	session model.StudySession,
	stream model.StudyClientStream,
	replayPrefix []model.StudyEventReceipt,
	suffix []normalizedStudySyncEvent,
) (*StudyEventSyncResult, error) {
	if len(suffix) == 0 {
		flush, err := insertStudyFlushReceipt(ctx, tx, uuid.New(), input, profile, session, stream, nil, nil)
		if err != nil {
			return nil, err
		}
		refreshed, err := queryStudySession(ctx, tx, session.ID, profile.ID, "", false)
		if err != nil {
			return nil, err
		}
		return &StudyEventSyncResult{
			Receipts:     replayPrefix,
			FlushReceipt: *flush,
			Session:      *refreshed,
			PageArtifact: cloneStudySessionPageArtifact(input.PageArtifact),
		}, nil
	}

	for index := range suffix {
		suffix[index].Event.LearnerProfileID = profile.ID
	}
	bindings, err := bindStudySuffixEventIDs(ctx, tx, profile.ID, session.ID, stream.ID, suffix)
	if err != nil {
		return nil, err
	}
	states, err := loadStudyItemEventStates(ctx, tx, session.ID)
	if err != nil {
		return nil, err
	}
	decisions, failureIndex, failureCode, err := simulateStudyEventSuffix(ctx, tx, session, suffix, bindings, states)
	if err != nil {
		return nil, err
	}

	originID := uuid.New()
	newReceipts := make([]model.StudyEventReceipt, 0, len(suffix))
	if failureIndex >= 0 {
		failureSequence := suffix[failureIndex].Event.ClientSequence
		for index := range decisions {
			receipt := terminalStudyEventReceipt(originID, profile.ID, session.ID, stream.ID, decisions[index], failureCode, failureSequence)
			inserted, err := insertStudyEventReceipt(ctx, tx, receipt)
			if err != nil {
				return nil, err
			}
			newReceipts = append(newReceipts, *inserted)
		}
	} else {
		ordinal := profile.ProfileIngestionHighWater
		for index := range decisions {
			if decisions[index].Disposition == model.StudyEventReceiptDispositionAccepted {
				ordinal++
				decisions[index].Canonical.Ordinal = ordinal
				if err := insertAcceptedStudyEvent(ctx, tx, profile.ID, session.ID, stream.ID, decisions[index]); err != nil {
					return nil, err
				}
			}
		}
		if ordinal != profile.ProfileIngestionHighWater {
			if _, err := tx.Exec(ctx, `
				UPDATE study_learner_profiles
				SET profile_ingestion_high_water = $2
				WHERE learner_profile_id = $1`, profile.ID, ordinal); err != nil {
				return nil, mapStudyStorageError("study_event_profile_advance", err)
			}
			profile.ProfileIngestionHighWater = ordinal
		}
		for index := range decisions {
			receipt := successfulStudyEventReceipt(originID, profile.ID, session.ID, stream.ID, decisions[index])
			inserted, err := insertStudyEventReceipt(ctx, tx, receipt)
			if err != nil {
				return nil, err
			}
			newReceipts = append(newReceipts, *inserted)
		}
	}

	lastSequence := suffix[len(suffix)-1].Event.ClientSequence
	if _, err := tx.Exec(ctx, `
		UPDATE study_client_streams
		SET adjudicated_through_client_sequence = $2
		WHERE client_stream_id = $1`, stream.ID, lastSequence); err != nil {
		return nil, mapStudyStorageError("study_event_stream_advance", err)
	}
	stream.AdjudicatedThroughSequence = lastSequence

	var nextStateVersion int64
	if err := tx.QueryRow(ctx, `
		UPDATE study_sessions
		SET study_session_state_version = study_session_state_version + 1
		WHERE study_session_id = $1
		  AND learner_profile_id = $2
		  AND study_session_state_version = $3
		RETURNING study_session_state_version`, session.ID, profile.ID, session.StateVersion).Scan(&nextStateVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, newStudyStorageError(
				model.StudyErrorCodeStateStale,
				"study session state version changed during sync",
				"study_event_session_advance",
				false,
				err,
			)
		}
		return nil, mapStudyStorageError("study_event_session_advance", err)
	}
	session.StateVersion = nextStateVersion

	firstSequence := suffix[0].Event.ClientSequence
	flush, err := insertStudyFlushReceipt(
		ctx,
		tx,
		originID,
		input,
		profile,
		session,
		stream,
		&firstSequence,
		&lastSequence,
	)
	if err != nil {
		return nil, err
	}
	refreshed, err := queryStudySession(ctx, tx, session.ID, profile.ID, "", false)
	if err != nil {
		return nil, err
	}
	receipts := append(append(make([]model.StudyEventReceipt, 0, len(replayPrefix)+len(newReceipts)), replayPrefix...), newReceipts...)
	return &StudyEventSyncResult{
		Receipts:     receipts,
		FlushReceipt: *flush,
		Session:      *refreshed,
		PageArtifact: cloneStudySessionPageArtifact(input.PageArtifact),
	}, nil
}

func bindStudySuffixEventIDs(
	ctx context.Context,
	tx pgx.Tx,
	profileID, sessionID, streamID uuid.UUID,
	suffix []normalizedStudySyncEvent,
) (map[uuid.UUID]studyEventBindingAuthority, error) {
	firstByID := make(map[uuid.UUID]normalizedStudySyncEvent, len(suffix))
	for index := range suffix {
		if _, exists := firstByID[suffix[index].Event.ID]; !exists {
			firstByID[suffix[index].Event.ID] = suffix[index]
		}
	}
	identities := make([]uuid.UUID, 0, len(firstByID))
	for identity := range firstByID {
		identities = append(identities, identity)
	}
	sort.Slice(identities, func(left, right int) bool {
		return bytes.Compare(identities[left][:], identities[right][:]) < 0
	})

	bindings := make(map[uuid.UUID]studyEventBindingAuthority, len(identities))
	for _, identity := range identities {
		candidate := firstByID[identity]
		_, err := tx.Exec(ctx, `
			INSERT INTO study_event_id_bindings (
				client_event_id,
				bound_event_payload_sha256,
				learner_profile_id,
				study_session_id,
				study_session_item_id,
				study_card_identity,
				source_content_sha256,
				first_client_stream_id,
				first_client_sequence
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (client_event_id) DO NOTHING`,
			candidate.Event.ID,
			candidate.PayloadSHA256,
			profileID,
			sessionID,
			candidate.Event.StudySessionItemID,
			candidate.Event.CardIdentity,
			candidate.Event.ContentSHA256,
			streamID,
			candidate.Event.ClientSequence,
		)
		if err != nil {
			return nil, mapStudyStorageError("study_event_binding_insert", err)
		}
		var binding studyEventBindingAuthority
		if err := tx.QueryRow(ctx, `
			SELECT
				client_event_id,
				bound_event_payload_sha256,
				learner_profile_id,
				study_session_id,
				study_session_item_id,
				study_card_identity,
				source_content_sha256,
				first_client_stream_id,
				first_client_sequence
			FROM study_event_id_bindings
			WHERE client_event_id = $1
			FOR UPDATE`, identity).Scan(
			&binding.EventID,
			&binding.BoundPayloadSHA256,
			&binding.LearnerProfileID,
			&binding.StudySessionID,
			&binding.StudySessionItemID,
			&binding.CardIdentity,
			&binding.ContentSHA256,
			&binding.FirstClientStreamID,
			&binding.FirstClientSequence,
		); err != nil {
			return nil, mapStudyStorageError("study_event_binding_lock", err)
		}
		bindings[identity] = binding
	}
	return bindings, nil
}

func loadStudyItemEventStates(
	ctx context.Context,
	tx pgx.Tx,
	sessionID uuid.UUID,
) (map[uuid.UUID]*studyItemEventState, error) {
	items, err := listStudySessionItems(ctx, tx, sessionID)
	if err != nil {
		return nil, err
	}
	states := make(map[uuid.UUID]*studyItemEventState, len(items))
	for index := range items {
		item := items[index]
		states[item.ID] = &studyItemEventState{Item: item, Ratings: make(map[uuid.UUID]*studyCanonicalEventRef)}
	}
	rows, err := tx.Query(ctx, `
		SELECT study_event_id, study_session_item_id, study_event_kind, rating_event_id, profile_ingestion_ordinal
		FROM study_events
		WHERE study_session_id = $1
		ORDER BY profile_ingestion_ordinal`, sessionID)
	if err != nil {
		return nil, mapStudyStorageError("study_event_state_read", err)
	}
	defer rows.Close()
	for rows.Next() {
		var eventID, itemID uuid.UUID
		var kind string
		var ratingEventID *uuid.UUID
		var ordinal int64
		if err := rows.Scan(&eventID, &itemID, &kind, &ratingEventID, &ordinal); err != nil {
			return nil, mapStudyStorageError("study_event_state_read", err)
		}
		state := states[itemID]
		if state == nil {
			return nil, newStudyStorageError(
				model.StudyErrorCodeInternal,
				"accepted study evidence has no immutable session item",
				"study_event_state_read",
				false,
				nil,
			)
		}
		canonical := &studyCanonicalEventRef{ID: eventID, Ordinal: ordinal}
		switch model.StudyEventKind(kind) {
		case model.StudyEventKindPresented:
			state.Presented = canonical
		case model.StudyEventKindRevealed:
			state.Revealed = canonical
		case model.StudyEventKindRated:
			state.Ratings[eventID] = canonical
			state.EffectiveRating = canonical
		case model.StudyEventKindRatingVoided:
			if ratingEventID != nil && state.EffectiveRating != nil && state.EffectiveRating.ID == *ratingEventID {
				state.EffectiveRating = nil
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, mapStudyStorageError("study_event_state_read", err)
	}
	return states, nil
}

func simulateStudyEventSuffix(
	ctx context.Context,
	tx pgx.Tx,
	session model.StudySession,
	suffix []normalizedStudySyncEvent,
	bindings map[uuid.UUID]studyEventBindingAuthority,
	states map[uuid.UUID]*studyItemEventState,
) ([]studySuffixDecision, int, model.StudyEventReceiptCode, error) {
	decisions := make([]studySuffixDecision, len(suffix))
	for index := range suffix {
		candidate := suffix[index]
		binding := bindings[candidate.Event.ID]
		bindingMatches := studyEventBindingMatches(binding, session.LearnerProfileID, session.ID, candidate)
		decisions[index] = studySuffixDecision{Input: candidate, Binding: binding, BindingMatches: bindingMatches}
	}
	for index := range decisions {
		candidate := decisions[index].Input
		bindingMatches := decisions[index].BindingMatches
		if !bindingMatches {
			return decisions, index, model.StudyEventReceiptCodeIDConflict, nil
		}
		canonical, err := readCanonicalStudyEventAuthority(ctx, tx, session.LearnerProfileID, session.ID, candidate)
		if err != nil {
			return nil, -1, "", err
		}
		if canonical != nil {
			decisions[index].Disposition = model.StudyEventReceiptDispositionSemanticDuplicate
			decisions[index].Code = model.StudyEventReceiptCodeSemanticDuplicate
			decisions[index].Canonical = canonical
			continue
		}

		if session.Lifecycle != model.StudySessionLifecycleActive &&
			!(session.Lifecycle == model.StudySessionLifecycleCompleted &&
				(candidate.Event.Kind == model.StudyEventKindRated || candidate.Event.Kind == model.StudyEventKindRatingVoided)) {
			return decisions, index, model.StudyEventReceiptCodeTransitionInvalid, nil
		}
		state := states[candidate.Event.StudySessionItemID]
		if state == nil || state.Item.CardIdentity != candidate.Event.CardIdentity || state.Item.SourceContentSHA256 != candidate.Event.ContentSHA256 {
			return decisions, index, model.StudyEventReceiptCodeContentRevisionMismatch, nil
		}

		switch candidate.Event.Kind {
		case model.StudyEventKindPresented:
			if state.Presented != nil {
				decisions[index].Disposition = model.StudyEventReceiptDispositionSemanticDuplicate
				decisions[index].Code = model.StudyEventReceiptCodeSemanticDuplicate
				decisions[index].Canonical = state.Presented
				continue
			}
			canonical := &studyCanonicalEventRef{ID: candidate.Event.ID}
			state.Presented = canonical
			decisions[index].Disposition = model.StudyEventReceiptDispositionAccepted
			decisions[index].Code = model.StudyEventReceiptCodeAccepted
			decisions[index].Canonical = canonical
		case model.StudyEventKindRevealed:
			if state.Revealed != nil {
				decisions[index].Disposition = model.StudyEventReceiptDispositionSemanticDuplicate
				decisions[index].Code = model.StudyEventReceiptCodeSemanticDuplicate
				decisions[index].Canonical = state.Revealed
				continue
			}
			if state.Presented == nil {
				return decisions, index, model.StudyEventReceiptCodeTransitionInvalid, nil
			}
			canonical := &studyCanonicalEventRef{ID: candidate.Event.ID}
			state.Revealed = canonical
			decisions[index].Disposition = model.StudyEventReceiptDispositionAccepted
			decisions[index].Code = model.StudyEventReceiptCodeAccepted
			decisions[index].Canonical = canonical
		case model.StudyEventKindRated:
			if state.Revealed == nil {
				return decisions, index, model.StudyEventReceiptCodeTransitionInvalid, nil
			}
			if state.EffectiveRating != nil {
				return decisions, index, model.StudyEventReceiptCodeRatingAlreadyEffective, nil
			}
			canonical := &studyCanonicalEventRef{ID: candidate.Event.ID}
			state.Ratings[candidate.Event.ID] = canonical
			state.EffectiveRating = canonical
			decisions[index].Disposition = model.StudyEventReceiptDispositionAccepted
			decisions[index].Code = model.StudyEventReceiptCodeAccepted
			decisions[index].Canonical = canonical
		case model.StudyEventKindRatingVoided:
			target := state.Ratings[*candidate.Event.RatingEventID]
			if target == nil || state.EffectiveRating == nil || state.EffectiveRating.ID != target.ID {
				return decisions, index, model.StudyEventReceiptCodeRatingVoidTargetInvalid, nil
			}
			state.EffectiveRating = nil
			canonical := &studyCanonicalEventRef{ID: candidate.Event.ID}
			decisions[index].Disposition = model.StudyEventReceiptDispositionAccepted
			decisions[index].Code = model.StudyEventReceiptCodeAccepted
			decisions[index].Canonical = canonical
		}
	}
	return decisions, -1, "", nil
}

func studyEventBindingMatches(
	binding studyEventBindingAuthority,
	profileID, sessionID uuid.UUID,
	candidate normalizedStudySyncEvent,
) bool {
	// The first stream is immutable provenance, not event identity. A
	// reprovisioned client may resubmit the same normalized payload (whose
	// numeric sequence is included in the digest) from a new transport stream.
	return binding.EventID == candidate.Event.ID &&
		binding.BoundPayloadSHA256 == candidate.PayloadSHA256 &&
		binding.LearnerProfileID == profileID &&
		binding.StudySessionID == sessionID &&
		binding.StudySessionItemID == candidate.Event.StudySessionItemID &&
		binding.CardIdentity == candidate.Event.CardIdentity &&
		binding.ContentSHA256 == candidate.Event.ContentSHA256 &&
		binding.FirstClientSequence == candidate.Event.ClientSequence
}

func terminalStudyEventReceipt(
	originID, profileID, sessionID, streamID uuid.UUID,
	decision studySuffixDecision,
	code model.StudyEventReceiptCode,
	failureSequence int64,
) model.StudyEventReceipt {
	failure := failureSequence
	return model.StudyEventReceipt{
		ClientStreamID:                 streamID,
		ClientSequence:                 decision.Input.Event.ClientSequence,
		OriginatingStudyFlushReceiptID: originID,
		LearnerProfileID:               profileID,
		StudySessionID:                 sessionID,
		StudySessionItemID:             decision.Input.Event.StudySessionItemID,
		CardIdentity:                   decision.Input.Event.CardIdentity,
		ContentSHA256:                  decision.Input.Event.ContentSHA256,
		EventID:                        decision.Input.Event.ID,
		BoundPayloadSHA256:             decision.Binding.BoundPayloadSHA256,
		PayloadSHA256:                  decision.Input.PayloadSHA256,
		EventIDBindingMatchesInput:     decision.BindingMatches,
		Disposition:                    model.StudyEventReceiptDispositionTerminalRejected,
		Code:                           code,
		TerminalFailureClientSequence:  &failure,
	}
}

func successfulStudyEventReceipt(
	originID, profileID, sessionID, streamID uuid.UUID,
	decision studySuffixDecision,
) model.StudyEventReceipt {
	canonicalID := decision.Canonical.ID
	canonicalOrdinal := decision.Canonical.Ordinal
	return model.StudyEventReceipt{
		ClientStreamID:                   streamID,
		ClientSequence:                   decision.Input.Event.ClientSequence,
		OriginatingStudyFlushReceiptID:   originID,
		LearnerProfileID:                 profileID,
		StudySessionID:                   sessionID,
		StudySessionItemID:               decision.Input.Event.StudySessionItemID,
		CardIdentity:                     decision.Input.Event.CardIdentity,
		ContentSHA256:                    decision.Input.Event.ContentSHA256,
		EventID:                          decision.Input.Event.ID,
		BoundPayloadSHA256:               decision.Binding.BoundPayloadSHA256,
		PayloadSHA256:                    decision.Input.PayloadSHA256,
		EventIDBindingMatchesInput:       decision.BindingMatches,
		Disposition:                      decision.Disposition,
		Code:                             decision.Code,
		CanonicalStudyEventID:            &canonicalID,
		CanonicalProfileIngestionOrdinal: &canonicalOrdinal,
	}
}

func insertAcceptedStudyEvent(
	ctx context.Context,
	tx pgx.Tx,
	profileID, sessionID, streamID uuid.UUID,
	decision studySuffixDecision,
) error {
	event := decision.Input.Event
	var rating any
	if event.Rating != nil {
		rating = string(*event.Rating)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO study_events (
			study_event_id,
			learner_profile_id,
			study_session_id,
			study_session_item_id,
			study_card_identity,
			study_event_kind,
			study_rating,
			rating_event_id,
			source_content_sha256,
			client_stream_id,
			client_sequence,
			normalized_event_payload_sha256,
			client_observed_at,
			client_clock_quality,
			profile_ingestion_ordinal
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		event.ID,
		profileID,
		sessionID,
		event.StudySessionItemID,
		event.CardIdentity,
		event.Kind,
		rating,
		event.RatingEventID,
		event.ContentSHA256,
		streamID,
		event.ClientSequence,
		decision.Input.PayloadSHA256,
		event.ClientObservedAt,
		event.ClientClockQuality,
		decision.Canonical.Ordinal,
	); err != nil {
		return mapStudyStorageError("study_event_insert", err)
	}
	return nil
}

func insertStudyEventReceipt(
	ctx context.Context,
	tx pgx.Tx,
	receipt model.StudyEventReceipt,
) (*model.StudyEventReceipt, error) {
	if err := tx.QueryRow(ctx, `
		INSERT INTO study_event_receipts (
			client_stream_id,
			client_sequence,
			originating_study_flush_receipt_id,
			learner_profile_id,
			study_session_id,
			study_session_item_id,
			study_card_identity,
			source_content_sha256,
			client_event_id,
			bound_event_payload_sha256,
			normalized_event_payload_sha256,
			event_id_binding_matches_input,
			event_receipt_disposition,
			event_receipt_code,
			terminal_failure_client_sequence,
			canonical_study_event_id,
			canonical_profile_ingestion_ordinal
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		RETURNING study_event_receipt_created_at`,
		receipt.ClientStreamID,
		receipt.ClientSequence,
		receipt.OriginatingStudyFlushReceiptID,
		receipt.LearnerProfileID,
		receipt.StudySessionID,
		receipt.StudySessionItemID,
		receipt.CardIdentity,
		receipt.ContentSHA256,
		receipt.EventID,
		receipt.BoundPayloadSHA256,
		receipt.PayloadSHA256,
		receipt.EventIDBindingMatchesInput,
		receipt.Disposition,
		receipt.Code,
		receipt.TerminalFailureClientSequence,
		receipt.CanonicalStudyEventID,
		receipt.CanonicalProfileIngestionOrdinal,
	).Scan(&receipt.CreatedAt); err != nil {
		return nil, mapStudyStorageError("study_event_receipt_insert", err)
	}
	return &receipt, nil
}

func insertStudyFlushReceipt(
	ctx context.Context,
	tx pgx.Tx,
	receiptID uuid.UUID,
	input normalizedStudyEventSyncInput,
	profile model.StudyLearnerProfile,
	session model.StudySession,
	stream model.StudyClientStream,
	firstSequence, lastSequence *int64,
) (*model.StudyFlushReceipt, error) {
	receipt := model.StudyFlushReceipt{
		ID:                          receiptID,
		StudyPrincipalID:            input.Principal,
		LearnerProfileID:            profile.ID,
		SyncRequestID:               input.SyncRequestID,
		NormalizedSyncRequestSHA256: input.NormalizedRequestSHA256,
		StudySessionID:              session.ID,
		ClientStreamID:              stream.ID,
		RequestFirstClientSequence:  nil,
		RequestLastClientSequence:   nil,
		FirstClientSequence:         cloneStudyInt64Pointer(firstSequence),
		LastClientSequence:          cloneStudyInt64Pointer(lastSequence),
		AcknowledgedThroughSequence: stream.AdjudicatedThroughSequence,
		ProfileIngestionHighWater:   profile.ProfileIngestionHighWater,
		StudySessionStateVersion:    session.StateVersion,
		SessionPageArtifactRequired: true,
	}
	receipt.RequestFirstClientSequence, receipt.RequestLastClientSequence = studySyncRequestSequenceRange(input.Events)
	if err := tx.QueryRow(ctx, `
		INSERT INTO study_flush_receipts (
			study_flush_receipt_id,
			study_principal_id,
			learner_profile_id,
			sync_request_id,
			normalized_sync_request_sha256,
			study_session_id,
			client_stream_id,
			request_first_client_sequence,
			request_last_client_sequence,
			first_client_sequence,
			last_client_sequence,
			acknowledged_through_client_sequence,
			profile_ingestion_high_water,
			study_session_state_version,
			session_page_artifact_required
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		RETURNING study_flush_receipt_issued_at`,
		receipt.ID,
		receipt.StudyPrincipalID.String(),
		receipt.LearnerProfileID,
		receipt.SyncRequestID,
		receipt.NormalizedSyncRequestSHA256,
		receipt.StudySessionID,
		receipt.ClientStreamID,
		receipt.RequestFirstClientSequence,
		receipt.RequestLastClientSequence,
		receipt.FirstClientSequence,
		receipt.LastClientSequence,
		receipt.AcknowledgedThroughSequence,
		receipt.ProfileIngestionHighWater,
		receipt.StudySessionStateVersion,
		receipt.SessionPageArtifactRequired,
	).Scan(&receipt.IssuedAt); err != nil {
		return nil, mapStudyStorageError("study_flush_receipt_insert", err)
	}
	if err := insertStudySessionPageArtifact(ctx, tx, receipt.ID, input.PageArtifact); err != nil {
		return nil, err
	}
	return &receipt, nil
}

func insertStudySessionPageArtifact(
	ctx context.Context,
	tx pgx.Tx,
	flushReceiptID uuid.UUID,
	artifact model.StudySessionPageArtifact,
) error {
	if flushReceiptID == uuid.Nil {
		return newStudyStorageError(model.StudyErrorCodeInternal, "study page artifact has no flush receipt authority", "study_session_page_insert", false, nil)
	}
	if err := model.ValidateStudySessionPageArtifact(artifact); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO study_flush_receipt_session_pages (
			study_flush_receipt_id,
			page_after_position,
			page_limit,
			canonical_session_page,
			canonical_session_page_sha256
		) VALUES ($1, $2, $3, $4, $5)`,
		flushReceiptID,
		artifact.Request.AfterPosition,
		artifact.Request.Limit,
		string(artifact.CanonicalJSON),
		artifact.CanonicalSHA256,
	); err != nil {
		return mapStudyStorageError("study_session_page_insert", err)
	}
	return nil
}

func readStudySessionPageArtifact(
	ctx context.Context,
	tx pgx.Tx,
	flushReceiptID uuid.UUID,
) (*model.StudySessionPageArtifact, error) {
	var artifact model.StudySessionPageArtifact
	var canonicalJSON string
	err := tx.QueryRow(ctx, `
		SELECT page_after_position, page_limit, canonical_session_page,
		       canonical_session_page_sha256
		FROM study_flush_receipt_session_pages
		WHERE study_flush_receipt_id = $1
		FOR UPDATE`, flushReceiptID).Scan(
		&artifact.Request.AfterPosition,
		&artifact.Request.Limit,
		&canonicalJSON,
		&artifact.CanonicalSHA256,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, newStudyStorageError(model.StudyErrorCodeInternal, "study sync replay page evidence is unavailable", "study_session_page_read", false, nil)
	}
	if err != nil {
		return nil, mapStudyStorageError("study_session_page_read", err)
	}
	artifact.CanonicalJSON = []byte(canonicalJSON)
	if err := model.ValidateStudySessionPageArtifact(artifact); err != nil {
		return nil, newStudyStorageError(model.StudyErrorCodeInternal, "stored study sync replay page evidence is invalid", "study_session_page_read", false, err)
	}
	clone := cloneStudySessionPageArtifact(artifact)
	return &clone, nil
}

func cloneStudyInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	return &copyOfValue
}

const studyEventReceiptSelect = `
	SELECT
		client_stream_id,
		client_sequence,
		originating_study_flush_receipt_id,
		learner_profile_id,
		study_session_id,
		study_session_item_id,
		study_card_identity,
		source_content_sha256,
		client_event_id,
		bound_event_payload_sha256,
		normalized_event_payload_sha256,
		event_id_binding_matches_input,
		event_receipt_disposition,
		event_receipt_code,
		terminal_failure_client_sequence,
		canonical_study_event_id,
		canonical_profile_ingestion_ordinal,
		study_event_receipt_created_at
	FROM study_event_receipts`

func readStudyEventReceiptAt(
	ctx context.Context,
	tx pgx.Tx,
	streamID uuid.UUID,
	sequence int64,
) (*model.StudyEventReceipt, error) {
	receipt, err := scanStudyEventReceipt(tx.QueryRow(ctx, studyEventReceiptSelect+`
		WHERE client_stream_id = $1 AND client_sequence = $2`, streamID, sequence))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapStudyStorageError("study_event_receipt_read", err)
	}
	return receipt, nil
}

func scanStudyEventReceipt(row pgx.Row) (*model.StudyEventReceipt, error) {
	var receipt model.StudyEventReceipt
	var disposition, code string
	if err := row.Scan(
		&receipt.ClientStreamID,
		&receipt.ClientSequence,
		&receipt.OriginatingStudyFlushReceiptID,
		&receipt.LearnerProfileID,
		&receipt.StudySessionID,
		&receipt.StudySessionItemID,
		&receipt.CardIdentity,
		&receipt.ContentSHA256,
		&receipt.EventID,
		&receipt.BoundPayloadSHA256,
		&receipt.PayloadSHA256,
		&receipt.EventIDBindingMatchesInput,
		&disposition,
		&code,
		&receipt.TerminalFailureClientSequence,
		&receipt.CanonicalStudyEventID,
		&receipt.CanonicalProfileIngestionOrdinal,
		&receipt.CreatedAt,
	); err != nil {
		return nil, err
	}
	receipt.Disposition = model.StudyEventReceiptDisposition(disposition)
	receipt.Code = model.StudyEventReceiptCode(code)
	return &receipt, nil
}

func readCanonicalStudyEventAuthority(
	ctx context.Context,
	tx pgx.Tx,
	profileID, sessionID uuid.UUID,
	candidate normalizedStudySyncEvent,
) (*studyCanonicalEventRef, error) {
	var canonical studyCanonicalEventRef
	err := tx.QueryRow(ctx, `
		SELECT study_event_id, profile_ingestion_ordinal
		FROM study_events
		WHERE study_event_id = $1
		  AND normalized_event_payload_sha256 = $2
		  AND learner_profile_id = $3
		  AND study_session_id = $4
		  AND study_session_item_id = $5
		  AND study_card_identity = $6
		  AND source_content_sha256 = $7`,
		candidate.Event.ID,
		candidate.PayloadSHA256,
		profileID,
		sessionID,
		candidate.Event.StudySessionItemID,
		candidate.Event.CardIdentity,
		candidate.Event.ContentSHA256,
	).Scan(&canonical.ID, &canonical.Ordinal)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapStudyStorageError("study_event_canonical_authority_read", err)
	}
	return &canonical, nil
}

func readStudyFlushReceipt(
	ctx context.Context,
	tx pgx.Tx,
	principal model.StudyPrincipalID,
	sessionID, syncRequestID uuid.UUID,
) (*model.StudyFlushReceipt, error) {
	var receipt model.StudyFlushReceipt
	var storedPrincipal string
	err := tx.QueryRow(ctx, `
		SELECT
			study_flush_receipt_id,
			study_principal_id,
			learner_profile_id,
			sync_request_id,
			normalized_sync_request_sha256,
			study_session_id,
			client_stream_id,
			request_first_client_sequence,
			request_last_client_sequence,
			first_client_sequence,
			last_client_sequence,
			acknowledged_through_client_sequence,
			profile_ingestion_high_water,
			study_session_state_version,
			session_page_artifact_required,
			study_flush_receipt_issued_at
			FROM study_flush_receipts
			WHERE study_principal_id = $1
			  AND study_session_id = $2
			  AND sync_request_id = $3
			FOR UPDATE`, principal.String(), sessionID, syncRequestID).Scan(
		&receipt.ID,
		&storedPrincipal,
		&receipt.LearnerProfileID,
		&receipt.SyncRequestID,
		&receipt.NormalizedSyncRequestSHA256,
		&receipt.StudySessionID,
		&receipt.ClientStreamID,
		&receipt.RequestFirstClientSequence,
		&receipt.RequestLastClientSequence,
		&receipt.FirstClientSequence,
		&receipt.LastClientSequence,
		&receipt.AcknowledgedThroughSequence,
		&receipt.ProfileIngestionHighWater,
		&receipt.StudySessionStateVersion,
		&receipt.SessionPageArtifactRequired,
		&receipt.IssuedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapStudyStorageError("study_flush_receipt_read", err)
	}
	restored, err := model.RestoreStudyPrincipalID(storedPrincipal)
	if err != nil {
		return nil, err
	}
	receipt.StudyPrincipalID = restored
	return &receipt, nil
}

func studySyncRequestSequenceRange(events []normalizedStudySyncEvent) (*int64, *int64) {
	if len(events) == 0 {
		return nil, nil
	}
	first := events[0].Event.ClientSequence
	last := events[len(events)-1].Event.ClientSequence
	return &first, &last
}

func sameStudyOptionalInt64(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
