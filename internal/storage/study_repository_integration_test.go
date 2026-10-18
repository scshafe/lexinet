package storage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lexinet/internal/model"
)

func TestStudyRepositoryPersistenceCheckpoint(t *testing.T) {
	harness := startStudyTestPostgres(t)
	harness.migrateUp(t)
	pool := harness.openPool(t)
	repository := NewRepository(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "study-integration", Name: "Study integration"})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.TOPIC_CREATE_FAILED", err))
	}
	sourceIdea := mustCreateStudyIdea(t, ctx, repository, topic.Slug, "source")
	targetIdea := mustCreateStudyIdea(t, ctx, repository, "", "target")
	scope := mustStudyScope(t, topic.ID, topic.Slug)
	principal := model.OperatorStudyPrincipalID()
	profileRef := "integration-profile"

	created := testConcurrentStudyPlanConvergence(t, ctx, repository, profileRef, scope, principal, sourceIdea.ID)
	testStudySnapshotAndPrefix(t, ctx, pool, repository, profileRef, scope, sourceIdea.ID, created)
	testStudyReceiptClaimPersistence(t, ctx, pool, profileRef, sourceIdea.ID, created)
	testStudyLifecycleCASAndRollback(t, ctx, pool, repository, profileRef, scope, principal, sourceIdea.ID, created)
	testStudyProjectionMerge(t, ctx, pool, repository, profileRef, scope, sourceIdea.ID, targetIdea.ID, created)
	testStudyPlanSourceRevalidation(t, ctx, pool, repository, topic, principal, sourceIdea.ID)
	testStudyHardDeletePreservesSnapshot(t, ctx, pool, repository, principal)
	testConcurrentReverseMergeHasOneWinner(t, ctx, pool, repository)
	testIdeaMutationAndMergeConvergence(t, ctx, pool, repository, principal)
	testMergedSurvivorHardDelete(t, ctx, pool, repository, principal)
	testStudyScopeInputIsolation(t, ctx, repository, principal)
}

func testStudyReceiptClaimPersistence(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	profileRef string,
	sourceID uuid.UUID,
	root *StudyPlanInsertResult,
) {
	t.Helper()
	var (
		profileID        uuid.UUID
		profileHighWater int64
	)
	if err := pool.QueryRow(ctx, `
		SELECT learner_profile_id, profile_ingestion_high_water
		FROM study_learner_profiles
		WHERE learner_profile_ref = $1`, profileRef).Scan(&profileID, &profileHighWater); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.RECEIPT_PROFILE_READ_FAILED", err))
	}
	invalidItemID := uuid.New()
	invalidCardID := uuid.New()
	invalidContentSHA := model.SHA256Hex([]byte("invalid submitted content claim"))

	newReceiptStream := func(label string) uuid.UUID {
		t.Helper()
		streamID := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO study_client_streams (client_stream_id, learner_profile_id)
			VALUES ($1, $2)`, streamID, profileID); err != nil {
			t.Fatal(studyTestDatabaseError(label+".STREAM_CREATE_FAILED", err))
		}
		return streamID
	}
	insertBinding := func(tx pgx.Tx, streamID uuid.UUID, eventID uuid.UUID, boundSHA string, itemID, cardID uuid.UUID, contentSHA string, sequence int64) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO study_event_id_bindings (
				client_event_id, bound_event_payload_sha256, learner_profile_id,
				study_session_id, study_session_item_id, study_card_identity,
				source_content_sha256, first_client_stream_id, first_client_sequence
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			eventID, boundSHA, profileID, root.Session.ID, itemID, cardID, contentSHA, streamID, sequence)
		return err
	}
	insertTerminalReceipt := func(
		tx pgx.Tx,
		streamID, originID uuid.UUID,
		sequence int64,
		itemID, cardID uuid.UUID,
		contentSHA string,
		eventID uuid.UUID,
		boundSHA, payloadSHA string,
		matches bool,
		code model.StudyEventReceiptCode,
		failureSequence int64,
	) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO study_event_receipts (
				client_stream_id, client_sequence, originating_study_flush_receipt_id,
				learner_profile_id, study_session_id,
				study_session_item_id, study_card_identity, source_content_sha256,
				client_event_id, bound_event_payload_sha256, normalized_event_payload_sha256,
				event_id_binding_matches_input, event_receipt_disposition,
				event_receipt_code, terminal_failure_client_sequence
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'terminal_rejected',$13,$14)`,
			streamID, sequence, originID, profileID, root.Session.ID, itemID, cardID, contentSHA,
			eventID, boundSHA, payloadSHA, matches, code, failureSequence)
		return err
	}
	insertFlush := func(tx pgx.Tx, streamID, originID uuid.UUID, firstSequence, lastSequence int64) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO study_flush_receipts (
				study_flush_receipt_id, study_principal_id, learner_profile_id,
				sync_request_id, normalized_sync_request_sha256, study_session_id,
				client_stream_id, request_first_client_sequence, request_last_client_sequence,
				first_client_sequence, last_client_sequence,
				acknowledged_through_client_sequence, profile_ingestion_high_water,
				study_session_state_version
			) VALUES ($1,'operator',$2,$3,$4,$5,$6,$7,$8,$7,$8,$8,$9,$10)`,
			originID, profileID, uuid.New(), model.SHA256Hex([]byte(originID.String())), root.Session.ID,
			streamID, firstSequence, lastSequence, profileHighWater, root.Session.StateVersion); err != nil {
			return err
		}
		if err := insertStudySessionPageArtifact(ctx, tx, originID, studyTestSessionPageArtifact()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE study_client_streams
			SET adjudicated_through_client_sequence = $2
			WHERE client_stream_id = $1`, streamID, lastSequence)
		return err
	}
	insertMatchedTerminal := func(tx pgx.Tx, streamID, originID uuid.UUID, sequence, failureSequence int64, code model.StudyEventReceiptCode) error {
		eventID := uuid.New()
		payloadSHA := model.SHA256Hex([]byte(eventID.String()))
		if err := insertBinding(tx, streamID, eventID, payloadSHA, invalidItemID, invalidCardID, invalidContentSHA, sequence); err != nil {
			return err
		}
		return insertTerminalReceipt(tx, streamID, originID, sequence, invalidItemID, invalidCardID,
			invalidContentSHA, eventID, payloadSHA, payloadSHA, true, code, failureSequence)
	}
	expectCommitFailure := func(label string, prepare func(pgx.Tx, uuid.UUID, uuid.UUID) error) {
		t.Helper()
		streamID := newReceiptStream(label)
		originID := uuid.New()
		candidateTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(studyTestDatabaseError(label+".TX_BEGIN_FAILED", err))
		}
		defer candidateTx.Rollback(ctx)
		if err := prepare(candidateTx, streamID, originID); err != nil {
			t.Fatal(studyTestDatabaseError(label+".FIXTURE_INSERT_FAILED", err))
		}
		if err := candidateTx.Commit(ctx); err == nil {
			t.Fatal(label + ".INVALID_BATCH_ACCEPTED")
		}
	}
	commitTerminalBatch := func(label string, failureSequence int64) {
		t.Helper()
		streamID := newReceiptStream(label)
		originID := uuid.New()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(studyTestDatabaseError(label+".TX_BEGIN_FAILED", err))
		}
		defer tx.Rollback(ctx)
		for sequence := int64(1); sequence <= 3; sequence++ {
			if err := insertMatchedTerminal(tx, streamID, originID, sequence, failureSequence, model.StudyEventReceiptCodeTransitionInvalid); err != nil {
				t.Fatal(studyTestDatabaseError(label+".RECEIPT_INSERT_FAILED", err))
			}
		}
		if err := insertFlush(tx, streamID, originID, 1, 3); err != nil {
			t.Fatal(studyTestDatabaseError(label+".FLUSH_INSERT_FAILED", err))
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(studyTestDatabaseError(label+".COMMIT_FAILED", err))
		}
	}

	// Preserve invalid submitted item/card/content claims. The first suffix is a
	// single causative failure; the second proves a causative ID conflict plus a
	// collateral mismatch whose own UUID binding comparison is also false.
	claimStreamID := newReceiptStream("STUDY_REPOSITORY.INVALID_CLAIM")
	firstOriginID := uuid.New()
	firstTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.INVALID_CLAIM_TX_BEGIN_FAILED", err))
	}
	defer firstTx.Rollback(ctx)
	firstEventID := uuid.New()
	firstPayloadSHA := model.SHA256Hex([]byte("invalid claim payload"))
	if err := insertBinding(firstTx, claimStreamID, firstEventID, firstPayloadSHA, invalidItemID, invalidCardID, invalidContentSHA, 1); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.INVALID_CLAIM_BINDING_REJECTED", err))
	}
	if err := insertTerminalReceipt(firstTx, claimStreamID, firstOriginID, 1, invalidItemID, invalidCardID,
		invalidContentSHA, firstEventID, firstPayloadSHA, firstPayloadSHA, true,
		model.StudyEventReceiptCodeContentRevisionMismatch, 1); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.INVALID_CLAIM_RECEIPT_REJECTED", err))
	}
	if err := insertFlush(firstTx, claimStreamID, firstOriginID, 1, 1); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.INVALID_CLAIM_FLUSH_REJECTED", err))
	}
	if err := firstTx.Commit(ctx); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.INVALID_CLAIM_COMMIT_FAILED", err))
	}

	secondOriginID := uuid.New()
	secondTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.ID_CONFLICT_TX_BEGIN_FAILED", err))
	}
	defer secondTx.Rollback(ctx)
	secondEventID := uuid.New()
	secondBoundSHA := model.SHA256Hex([]byte("second bound payload"))
	secondPayloadSHA := model.SHA256Hex([]byte("second conflicting payload"))
	if err := insertBinding(secondTx, claimStreamID, secondEventID, secondBoundSHA, root.Items[0].ID, sourceID, root.Items[0].SourceContentSHA256, 2); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.ID_CONFLICT_BINDING_FAILED", err))
	}
	if err := insertTerminalReceipt(secondTx, claimStreamID, secondOriginID, 2, uuid.New(), uuid.New(),
		model.SHA256Hex([]byte("second changed content")), secondEventID, secondBoundSHA, secondPayloadSHA,
		false, model.StudyEventReceiptCodeIDConflict, 2); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.ID_CONFLICT_CAUSE_FAILED", err))
	}
	thirdEventID := uuid.New()
	thirdBoundSHA := model.SHA256Hex([]byte("third bound payload"))
	thirdPayloadSHA := model.SHA256Hex([]byte("third conflicting payload"))
	if err := insertBinding(secondTx, claimStreamID, thirdEventID, thirdBoundSHA, root.Items[0].ID, sourceID, root.Items[0].SourceContentSHA256, 3); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.COLLATERAL_BINDING_FAILED", err))
	}
	if err := insertTerminalReceipt(secondTx, claimStreamID, secondOriginID, 3, uuid.New(), uuid.New(),
		model.SHA256Hex([]byte("third changed content")), thirdEventID, thirdBoundSHA, thirdPayloadSHA,
		false, model.StudyEventReceiptCodeIDConflict, 2); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.COLLATERAL_RECEIPT_FAILED", err))
	}
	if err := insertFlush(secondTx, claimStreamID, secondOriginID, 2, 3); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.ID_CONFLICT_FLUSH_FAILED", err))
	}
	if err := secondTx.Commit(ctx); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.GROUPED_TERMINAL_RECEIPTS_COMMIT_FAILED", err))
	}

	// Both a middle failure and a last-event failure must terminalize the whole
	// new suffix, including earlier rows that forward-reference the cause.
	commitTerminalBatch("STUDY_REPOSITORY.FAILURE_IN_MIDDLE", 2)
	commitTerminalBatch("STUDY_REPOSITORY.FAILURE_LAST", 3)

	expectCommitFailure("STUDY_REPOSITORY.INVENTED_ORIGIN", func(tx pgx.Tx, streamID, originID uuid.UUID) error {
		return insertMatchedTerminal(tx, streamID, originID, 1, 1, model.StudyEventReceiptCodeTransitionInvalid)
	})
	expectCommitFailure("STUDY_REPOSITORY.MISSING_TERMINAL_CAUSE", func(tx pgx.Tx, streamID, originID uuid.UUID) error {
		if err := insertMatchedTerminal(tx, streamID, originID, 1, 2, model.StudyEventReceiptCodeTransitionInvalid); err != nil {
			return err
		}
		return insertFlush(tx, streamID, originID, 1, 1)
	})
	expectCommitFailure("STUDY_REPOSITORY.UNDERFILLED_ORIGIN_RANGE", func(tx pgx.Tx, streamID, originID uuid.UUID) error {
		if err := insertMatchedTerminal(tx, streamID, originID, 1, 1, model.StudyEventReceiptCodeTransitionInvalid); err != nil {
			return err
		}
		if err := insertMatchedTerminal(tx, streamID, originID, 3, 1, model.StudyEventReceiptCodeTransitionInvalid); err != nil {
			return err
		}
		return insertFlush(tx, streamID, originID, 1, 3)
	})
	expectCommitFailure("STUDY_REPOSITORY.EXTENDED_ORIGIN_RANGE", func(tx pgx.Tx, streamID, originID uuid.UUID) error {
		if err := insertMatchedTerminal(tx, streamID, originID, 1, 1, model.StudyEventReceiptCodeTransitionInvalid); err != nil {
			return err
		}
		if err := insertMatchedTerminal(tx, streamID, originID, 2, 1, model.StudyEventReceiptCodeTransitionInvalid); err != nil {
			return err
		}
		return insertFlush(tx, streamID, originID, 1, 1)
	})
	expectCommitFailure("STUDY_REPOSITORY.NONEMPTY_FLUSH_WITHOUT_RECEIPTS", func(tx pgx.Tx, streamID, originID uuid.UUID) error {
		return insertFlush(tx, streamID, originID, 1, 1)
	})

	var acceptedEventID, acceptedItemID, acceptedCardID uuid.UUID
	var acceptedContentSHA, acceptedPayloadSHA string
	var acceptedOrdinal int64
	if err := pool.QueryRow(ctx, `
		SELECT event.study_event_id, event.study_session_item_id, event.study_card_identity,
			event.source_content_sha256, binding.bound_event_payload_sha256,
			event.profile_ingestion_ordinal
		FROM study_events event
		JOIN study_event_id_bindings binding ON binding.client_event_id = event.study_event_id
		WHERE event.study_session_id = $1
		ORDER BY event.profile_ingestion_ordinal
		LIMIT 1`, root.Session.ID).Scan(
		&acceptedEventID, &acceptedItemID, &acceptedCardID, &acceptedContentSHA,
		&acceptedPayloadSHA, &acceptedOrdinal,
	); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.ACCEPTED_CAUSE_EVENT_READ_FAILED", err))
	}
	successStreamID := newReceiptStream("STUDY_REPOSITORY.ACCEPTED_SEMANTIC_BATCH")
	successOriginID := uuid.New()
	successTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.ACCEPTED_SEMANTIC_TX_BEGIN_FAILED", err))
	}
	defer successTx.Rollback(ctx)
	if _, err := successTx.Exec(ctx, `
		INSERT INTO study_event_receipts (
			client_stream_id, client_sequence, originating_study_flush_receipt_id,
			learner_profile_id, study_session_id, study_session_item_id,
			study_card_identity, source_content_sha256, client_event_id,
			bound_event_payload_sha256, normalized_event_payload_sha256,
			event_id_binding_matches_input, event_receipt_disposition,
			event_receipt_code, canonical_study_event_id,
			canonical_profile_ingestion_ordinal
		) VALUES ($1,1,$2,$3,$4,$5,$6,$7,$8,$9,$9,true,'accepted','STUDY_EVENT_ACCEPTED',$8,$10)`,
		successStreamID, successOriginID, profileID, root.Session.ID, acceptedItemID,
		acceptedCardID, acceptedContentSHA, acceptedEventID, acceptedPayloadSHA, acceptedOrdinal); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.ACCEPTED_RECEIPT_INSERT_FAILED", err))
	}
	duplicateEventID := uuid.New()
	duplicatePayloadSHA := model.SHA256Hex([]byte("semantic duplicate payload"))
	if err := insertBinding(successTx, successStreamID, duplicateEventID, duplicatePayloadSHA,
		acceptedItemID, acceptedCardID, acceptedContentSHA, 2); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.SEMANTIC_DUPLICATE_BINDING_FAILED", err))
	}
	if _, err := successTx.Exec(ctx, `
		INSERT INTO study_event_receipts (
			client_stream_id, client_sequence, originating_study_flush_receipt_id,
			learner_profile_id, study_session_id, study_session_item_id,
			study_card_identity, source_content_sha256, client_event_id,
			bound_event_payload_sha256, normalized_event_payload_sha256,
			event_id_binding_matches_input, event_receipt_disposition,
			event_receipt_code, canonical_study_event_id,
			canonical_profile_ingestion_ordinal
		) VALUES ($1,2,$2,$3,$4,$5,$6,$7,$8,$9,$9,true,'semantic_duplicate','STUDY_EVENT_SEMANTIC_DUPLICATE',$10,$11)`,
		successStreamID, successOriginID, profileID, root.Session.ID, acceptedItemID,
		acceptedCardID, acceptedContentSHA, duplicateEventID, duplicatePayloadSHA,
		acceptedEventID, acceptedOrdinal); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.SEMANTIC_DUPLICATE_RECEIPT_INSERT_FAILED", err))
	}
	if err := insertFlush(successTx, successStreamID, successOriginID, 1, 2); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.ACCEPTED_SEMANTIC_FLUSH_FAILED", err))
	}
	if err := successTx.Commit(ctx); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.ACCEPTED_SEMANTIC_COMMIT_FAILED", err))
	}

	expectCommitFailure("STUDY_REPOSITORY.ACCEPTED_TERMINAL_CAUSE", func(tx pgx.Tx, streamID, originID uuid.UUID) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO study_event_receipts (
				client_stream_id, client_sequence, originating_study_flush_receipt_id,
				learner_profile_id, study_session_id,
				study_session_item_id, study_card_identity, source_content_sha256,
				client_event_id, bound_event_payload_sha256, normalized_event_payload_sha256,
				event_id_binding_matches_input, event_receipt_disposition,
				event_receipt_code, canonical_study_event_id,
				canonical_profile_ingestion_ordinal
			) VALUES ($1,1,$2,$3,$4,$5,$6,$7,$8,$9,$9,true,'accepted','STUDY_EVENT_ACCEPTED',$8,$10)`,
			streamID, originID, profileID, root.Session.ID, acceptedItemID, acceptedCardID,
			acceptedContentSHA, acceptedEventID, acceptedPayloadSHA, acceptedOrdinal); err != nil {
			return err
		}
		if err := insertMatchedTerminal(tx, streamID, originID, 2, 1, model.StudyEventReceiptCodeTransitionInvalid); err != nil {
			return err
		}
		return insertFlush(tx, streamID, originID, 1, 2)
	})
	expectCommitFailure("STUDY_REPOSITORY.COLLATERAL_TERMINAL_CAUSE", func(tx pgx.Tx, streamID, originID uuid.UUID) error {
		if err := insertMatchedTerminal(tx, streamID, originID, 1, 1, model.StudyEventReceiptCodeTransitionInvalid); err != nil {
			return err
		}
		if err := insertMatchedTerminal(tx, streamID, originID, 2, 1, model.StudyEventReceiptCodeTransitionInvalid); err != nil {
			return err
		}
		if err := insertMatchedTerminal(tx, streamID, originID, 3, 2, model.StudyEventReceiptCodeTransitionInvalid); err != nil {
			return err
		}
		return insertFlush(tx, streamID, originID, 1, 3)
	})
	expectCommitFailure("STUDY_REPOSITORY.MISMATCHED_TERMINAL_CAUSE_CODE", func(tx pgx.Tx, streamID, originID uuid.UUID) error {
		if err := insertMatchedTerminal(tx, streamID, originID, 1, 1, model.StudyEventReceiptCodeTransitionInvalid); err != nil {
			return err
		}
		if err := insertMatchedTerminal(tx, streamID, originID, 2, 1, model.StudyEventReceiptCodeContentRevisionMismatch); err != nil {
			return err
		}
		return insertFlush(tx, streamID, originID, 1, 2)
	})

	// A same-code rejection from an older batch cannot become the cause for a
	// newly originated suffix on the same stream.
	crossBatchStreamID := newReceiptStream("STUDY_REPOSITORY.CROSS_BATCH_CAUSE")
	oldOriginID := uuid.New()
	oldTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.CROSS_BATCH_OLD_TX_BEGIN_FAILED", err))
	}
	defer oldTx.Rollback(ctx)
	if err := insertMatchedTerminal(oldTx, crossBatchStreamID, oldOriginID, 1, 1, model.StudyEventReceiptCodeTransitionInvalid); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.CROSS_BATCH_OLD_RECEIPT_FAILED", err))
	}
	if err := insertFlush(oldTx, crossBatchStreamID, oldOriginID, 1, 1); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.CROSS_BATCH_OLD_FLUSH_FAILED", err))
	}
	if err := oldTx.Commit(ctx); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.CROSS_BATCH_OLD_COMMIT_FAILED", err))
	}
	newOriginID := uuid.New()
	newTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.CROSS_BATCH_NEW_TX_BEGIN_FAILED", err))
	}
	defer newTx.Rollback(ctx)
	if err := insertMatchedTerminal(newTx, crossBatchStreamID, newOriginID, 2, 1, model.StudyEventReceiptCodeTransitionInvalid); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.CROSS_BATCH_NEW_RECEIPT_FAILED", err))
	}
	if err := insertFlush(newTx, crossBatchStreamID, newOriginID, 2, 2); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.CROSS_BATCH_NEW_FLUSH_FAILED", err))
	}
	if err := newTx.Commit(ctx); err == nil {
		t.Fatal("STUDY_REPOSITORY.OLDER_SAME_CODE_CAUSE_ACCEPTED")
	}
}

func testStudyPlanSourceRevalidation(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	topic *model.Topic,
	principal model.StudyPrincipalID,
	mergedSourceID uuid.UUID,
) {
	t.Helper()
	scope := mustStudyScope(t, topic.ID, topic.Slug)
	for _, fixture := range []struct {
		name       string
		profileRef string
		ideaID     uuid.UUID
	}{
		{name: "missing", profileRef: "missing-source-profile", ideaID: uuid.New()},
		{name: "merged", profileRef: "merged-source-profile", ideaID: mergedSourceID},
	} {
		plan := testStudyPlan(uuid.New(), uuid.New(), principal, fixture.ideaID, 0, fixture.name, nil)
		err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: fixture.profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
			_, insertErr := store.InsertPlanOnce(ctx, plan)
			return insertErr
		})
		if !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeStateStale}) {
			t.Fatalf("STUDY_REPOSITORY.%s_SOURCE_PLAN_NOT_REJECTED", fixture.name)
		}
	}
	inconsistentIdea := mustCreateStudyIdea(t, ctx, repository, topic.Slug, "inconsistent-source")
	inconsistentPlan := testStudyPlan(uuid.New(), uuid.New(), principal, inconsistentIdea.ID, 0, "inconsistent-source", nil)
	inconsistentPlan.Items[0].HistoricalSourceIdentity = uuid.New()
	err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: "inconsistent-source-profile", Scope: scope}, func(store StudyStoreTransaction) error {
		_, insertErr := store.InsertPlanOnce(ctx, inconsistentPlan)
		return insertErr
	})
	if !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeSchemaInvalid}) {
		t.Fatal("STUDY_REPOSITORY.INCONSISTENT_ITEM_SOURCE_IDENTITIES_ACCEPTED")
	}

	racingTopicSuffix := stringsForStudyFixture(uuid.NewString())
	racingTopic, err := repository.CreateTopic(ctx, model.CreateTopicInput{
		Slug: "plan-race-" + racingTopicSuffix,
		Name: "Plan race " + racingTopicSuffix,
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PLAN_RACE_TOPIC_CREATE_FAILED", err))
	}
	racingScope := mustStudyScope(t, racingTopic.ID, racingTopic.Slug)
	racingSource := mustCreateStudyIdea(t, ctx, repository, racingTopic.Slug, "plan-race-source")
	racingTarget := mustCreateStudyIdea(t, ctx, repository, "", "plan-race-target")
	plan := testStudyPlan(uuid.New(), uuid.New(), principal, racingSource.ID, 0, "plan-race", nil)
	mergeReachedLock := make(chan struct{})
	var mergeHookOnce sync.Once
	repository.studyMergeBeforeLock = func(sourceID, targetID uuid.UUID, _ [2]uuid.UUID) {
		if sourceID == racingSource.ID && targetID == racingTarget.ID {
			mergeHookOnce.Do(func() { close(mergeReachedLock) })
		}
	}
	defer func() { repository.studyMergeBeforeLock = nil }()
	mergeCompleted := make(chan error, 1)
	var inserted *StudyPlanInsertResult
	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: "plan-race-profile", Scope: racingScope}, func(store StudyStoreTransaction) error {
		sources, readErr := store.ListEligibleSources(ctx)
		if readErr != nil {
			return readErr
		}
		found := false
		for index := range sources {
			if sources[index].IdeaID == racingSource.ID {
				found = true
			}
		}
		if !found {
			return errors.New("plan race source was not initially eligible")
		}
		go func() {
			_, mergeErr := repository.MergeIdea(ctx, racingSource.ID.String(), racingTarget.ID.String())
			mergeCompleted <- mergeErr
		}()
		select {
		case <-mergeReachedLock:
		case <-ctx.Done():
			return ctx.Err()
		}
		var insertErr error
		inserted, insertErr = store.InsertPlanOnce(ctx, plan)
		return insertErr
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.MERGE_VS_PLAN_INSERT_FAILED", err))
	}
	select {
	case mergeErr := <-mergeCompleted:
		if mergeErr != nil {
			t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.MERGE_VS_PLAN_MERGE_FAILED", mergeErr))
		}
	case <-ctx.Done():
		t.Fatal("STUDY_REPOSITORY.MERGE_VS_PLAN_COMPLETION_TIMEOUT")
	}
	repository.studyMergeBeforeLock = nil
	items, err := repository.ListStudySessionItems(ctx, "plan-race-profile", inserted.Session.ID)
	if err != nil || len(items) != 1 || items[0].HistoricalSourceIdentity != racingSource.ID || items[0].LiveSourceIdeaID == nil || *items[0].LiveSourceIdeaID != racingTarget.ID {
		t.Fatal("STUDY_REPOSITORY.MERGE_VS_PLAN_LINEARIZATION_DAMAGED_ITEM")
	}
}

func testConcurrentStudyPlanConvergence(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	profileRef string,
	scope model.StudyNormalizedScope,
	principal model.StudyPrincipalID,
	ideaID uuid.UUID,
) *StudyPlanInsertResult {
	t.Helper()
	type outcome struct {
		result               *StudyPlanInsertResult
		err                  error
		index                int
		expectedCorpusSHA256 string
		expectedSource       *StudyEligibleSource
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	plans := []StudyPlanInsert{
		testStudyPlan(uuid.New(), uuid.New(), principal, ideaID, 0, "race-0", nil),
		testStudyPlan(uuid.New(), uuid.New(), principal, ideaID, 0, "race-1", nil),
	}
	for index := range 2 {
		index := index
		go func() {
			<-start
			var result *StudyPlanInsertResult
			var expectedCorpusSHA256 string
			var expectedSource *StudyEligibleSource
			err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
				if store.NonterminalSession() == nil {
					sources, listErr := store.ListEligibleSources(ctx)
					if listErr != nil {
						return listErr
					}
					expectedCorpusSHA256 = mustStudyEligibleCorpusDigest(t, scope, sources)
					if len(sources) == 1 {
						copyOfSource := cloneStudyEligibleSource(sources[0])
						expectedSource = &copyOfSource
						if sources[0].Example != nil {
							*sources[0].Example = "HOSTILE_RETURNED_SOURCE_POINTER_CANARY"
						}
						if sources[0].IdeaSummary != nil {
							*sources[0].IdeaSummary = "HOSTILE_RETURNED_SUMMARY_POINTER_CANARY"
						}
					}
				}
				var insertErr error
				result, insertErr = store.InsertPlanOnce(ctx, plans[index])
				return insertErr
			})
			outcomes <- outcome{result: result, err: err, index: index, expectedCorpusSHA256: expectedCorpusSHA256, expectedSource: expectedSource}
		}()
	}
	close(start)
	first := <-outcomes
	second := <-outcomes
	for _, candidate := range []outcome{first, second} {
		if candidate.err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.CONCURRENT_CREATE_FAILED", candidate.err))
		}
		if candidate.result == nil {
			t.Fatal("STUDY_REPOSITORY.CONCURRENT_CREATE_RESULT_MISSING")
		}
	}
	if first.result.Session.ID != second.result.Session.ID {
		t.Fatal("STUDY_REPOSITORY.CONCURRENT_CREATE_DID_NOT_CONVERGE")
	}
	if first.result.Created == second.result.Created {
		t.Fatal("STUDY_REPOSITORY.CONCURRENT_CREATE_AUTHORITY_AMBIGUOUS")
	}
	if first.result.Receipt.CreationRequestID == second.result.Receipt.CreationRequestID {
		t.Fatal("STUDY_REPOSITORY.CONCURRENT_CREATE_RECEIPTS_COLLAPSED")
	}
	created := first.result
	createdPlan := plans[first.index]
	createdExpectedCorpusSHA256 := first.expectedCorpusSHA256
	createdExpectedSource := first.expectedSource
	if second.result.Created {
		created = second.result
		createdPlan = plans[second.index]
		createdExpectedCorpusSHA256 = second.expectedCorpusSHA256
		createdExpectedSource = second.expectedSource
	}
	if created.Receipt.Disposition != model.StudyCreationDispositionCreated {
		t.Fatal("STUDY_REPOSITORY.CREATED_RECEIPT_DISPOSITION_INVALID")
	}
	if createdExpectedCorpusSHA256 == "" || created.Session.CorpusSHA256 != createdExpectedCorpusSHA256 || created.Session.CorpusSHA256 == createdPlan.CorpusSHA256 {
		t.Fatal("STUDY_REPOSITORY.CAPTURED_CORPUS_DIGEST_NOT_AUTHORITATIVE")
	}
	if createdExpectedSource == nil || len(created.Items) != 1 ||
		created.Items[0].FrontCanonicalName != createdExpectedSource.CanonicalName ||
		created.Items[0].BackDefinition != createdExpectedSource.Definition ||
		!sameStudyStringPointer(created.Items[0].BackExample, createdExpectedSource.Example) ||
		created.Items[0].SourceContentRevision != createdExpectedSource.SourceContentRevision ||
		created.Items[0].SourceContentSHA256 != createdExpectedSource.SourceContentSHA256 {
		t.Fatal("STUDY_REPOSITORY.RETURNED_SOURCE_MUTATION_CHANGED_CAPTURED_SNAPSHOT")
	}
	converged := first.result
	if first.result.Created {
		converged = second.result
	}
	if converged.Receipt.Disposition != model.StudyCreationDispositionConvergedToExisting {
		t.Fatal("STUDY_REPOSITORY.CONVERGED_RECEIPT_DISPOSITION_INVALID")
	}
	var replay *StudyPlanInsertResult
	err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		var replayErr error
		replay, replayErr = store.InsertPlanOnce(ctx, createdPlan)
		return replayErr
	})
	if err != nil || replay == nil || replay.Created || replay.Receipt.CreationReceiptAdjudicatedAt != created.Receipt.CreationReceiptAdjudicatedAt || replay.Session.ID != created.Session.ID {
		t.Fatal("STUDY_REPOSITORY.CREATION_RECEIPT_REPLAY_UNSTABLE")
	}
	conflictingPlan := createdPlan
	conflictingPlan.NormalizedRequestSHA256 = model.SHA256Hex([]byte("changed-request"))
	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		_, conflictErr := store.InsertPlanOnce(ctx, conflictingPlan)
		return conflictErr
	})
	if !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeIdempotencyConflict}) {
		t.Fatal("STUDY_REPOSITORY.CREATION_RECEIPT_CONFLICT_NOT_REJECTED")
	}
	return created
}

func testStudyLifecycleCASAndRollback(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	profileRef string,
	scope model.StudyNormalizedScope,
	principal model.StudyPrincipalID,
	ideaID uuid.UUID,
	root *StudyPlanInsertResult,
) {
	t.Helper()
	err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		_, transitionErr := store.TransitionSession(ctx, StudySessionTransition{
			SessionID: root.Session.ID, ExpectedStateVersion: 1,
			To: model.StudySessionLifecycleCompleted, ReasonCode: "fixture_missing_flush", Actor: principal,
		})
		return transitionErr
	})
	if !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeFlushReceiptRequired}) {
		t.Fatal("STUDY_REPOSITORY.COMPLETION_WITHOUT_ATOMIC_FLUSH_BARRIER_ACCEPTED")
	}
	predecessorPlan := testStudyPlan(uuid.New(), uuid.New(), principal, ideaID, 0, "forbidden-successor", &root.Session.ID)
	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		_, insertErr := store.InsertPlanOnce(ctx, predecessorPlan)
		return insertErr
	})
	if !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeFlushReceiptRequired}) {
		t.Fatal("STUDY_REPOSITORY.ROOT_PLAN_PRIMITIVE_ACCEPTED_PREDECESSOR")
	}

	rollbackMarker := errors.New("fixture rollback")
	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		if _, err := store.TransitionSession(ctx, StudySessionTransition{
			SessionID: root.Session.ID, ExpectedStateVersion: 1,
			To: model.StudySessionLifecycleAbandoned, ReasonCode: "fixture_rollback", Actor: principal,
		}); err != nil {
			return err
		}
		return rollbackMarker
	})
	if !errors.Is(err, rollbackMarker) {
		t.Fatal("STUDY_REPOSITORY.LIFECYCLE_ROLLBACK_NOT_PROPAGATED")
	}
	afterRollback, err := repository.GetStudySession(ctx, profileRef, root.Session.ID)
	if err != nil || afterRollback.Lifecycle != model.StudySessionLifecycleActive || afterRollback.StateVersion != 1 || afterRollback.TerminalProfileIngestionHighWater != nil {
		t.Fatal("STUDY_REPOSITORY.LIFECYCLE_ROLLBACK_MUTATED_SESSION")
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM study_session_lifecycle_events WHERE study_session_id = $1`, root.Session.ID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatal("STUDY_REPOSITORY.LIFECYCLE_ROLLBACK_MUTATED_AUDIT")
	}

	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		_, transitionErr := store.TransitionSession(ctx, StudySessionTransition{
			SessionID: root.Session.ID, ExpectedStateVersion: 1,
			To: model.StudySessionLifecycleAbandoned, ReasonCode: "fixture_abandon", Actor: principal,
		})
		return transitionErr
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.ABANDON_FAILED", err))
	}
	rootAfter, err := repository.GetStudySession(ctx, profileRef, root.Session.ID)
	if err != nil || rootAfter.Lifecycle != model.StudySessionLifecycleAbandoned || rootAfter.StateVersion != 2 || rootAfter.SuccessorStudySessionID != nil ||
		rootAfter.TerminalProfileIngestionHighWater == nil || *rootAfter.TerminalProfileIngestionHighWater != rootAfter.ProfileIngestionHighWater {
		t.Fatal("STUDY_REPOSITORY.ABANDON_AUDIT_STATE_INVALID")
	}
	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		_, transitionErr := store.TransitionSession(ctx, StudySessionTransition{
			SessionID: root.Session.ID, ExpectedStateVersion: 1,
			To: model.StudySessionLifecycleAbandoned, ReasonCode: "fixture_stale", Actor: principal,
		})
		return transitionErr
	})
	if !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeStateStale}) {
		t.Fatal("STUDY_REPOSITORY.STALE_CAS_NOT_REJECTED")
	}
}

func testStudySnapshotAndPrefix(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	profileRef string,
	scope model.StudyNormalizedScope,
	ideaID uuid.UUID,
	root *StudyPlanInsertResult,
) {
	t.Helper()
	rootItem := root.Items[0]
	if _, err := pool.Exec(ctx, `UPDATE definitions SET body = 'mutated source definition' WHERE idea_id = $1`, ideaID); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.SOURCE_MUTATION_FAILED", err))
	}
	items, err := repository.ListStudySessionItems(ctx, profileRef, root.Session.ID)
	if err != nil || len(items) != 1 || items[0].BackDefinition != rootItem.BackDefinition || items[0].SourceContentSHA256 != rootItem.SourceContentSHA256 {
		t.Fatal("STUDY_REPOSITORY.IMMUTABLE_SNAPSHOT_CHANGED_WITH_SOURCE")
	}
	if _, err := pool.Exec(ctx, `UPDATE study_session_items SET card_back_definition = 'illegal rewrite' WHERE study_session_item_id = $1`, rootItem.ID); err == nil {
		t.Fatal("STUDY_REPOSITORY.IMMUTABLE_SNAPSHOT_RAW_UPDATE_ACCEPTED")
	}

	profile, err := repository.GetStudyLearnerProfile(ctx, profileRef)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PROFILE_READ_FAILED", err))
	}
	events := insertAcceptedStudyPrefix(t, ctx, pool, *profile, root.Session, rootItem)
	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		sources, err := store.ListEligibleSources(ctx)
		if err != nil {
			return err
		}
		if len(sources) != 1 || sources[0].IdeaID != ideaID || sources[0].Definition != "mutated source definition" || sources[0].DefinitionID == nil {
			return errors.New("eligible source provenance mismatch")
		}
		prefix, err := store.ReadAcceptedPrefix(ctx, 4)
		if err != nil {
			return err
		}
		if len(prefix) != 4 || prefix[3].Kind != model.StudyEventKindRatingVoided || prefix[3].RatingEventID == nil || *prefix[3].RatingEventID != events[2] {
			return errors.New("accepted prefix ordering or void target mismatch")
		}
		return nil
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.SNAPSHOT_PREFIX_READ_FAILED", err))
	}
}

func testStudyProjectionMerge(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	profileRef string,
	scope model.StudyNormalizedScope,
	sourceID, targetID uuid.UUID,
	root *StudyPlanInsertResult,
) {
	t.Helper()
	profile, err := repository.GetStudyLearnerProfile(ctx, profileRef)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PROJECTION_PROFILE_FAILED", err))
	}
	var presentedEventID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT study_event_id FROM study_events
		WHERE study_session_item_id = $1 AND study_event_kind = 'presented'`, root.Items[0].ID).Scan(&presentedEventID); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PROJECTION_EVIDENCE_FAILED", err))
	}
	payload := []byte(`{"scheduler_state":{"box":7,"opaque":"SOURCE_PROJECTION_CANARY"}}`)
	projection := model.StudySchedulerCardProjection{
		LearnerProfileID:                      profile.ID,
		ProjectedCardIdentity:                 sourceID,
		LiveSourceIdeaID:                      &sourceID,
		SchedulerName:                         "variety_first",
		SchedulerVersion:                      "v1",
		ProjectionSchemaVersion:               1,
		LatestPresentedEventID:                &presentedEventID,
		LatestPresentedEvidenceCardIdentity:   &sourceID,
		LatestPresentedIngestionOrdinal:       testInt64Pointer(1),
		RebuiltThroughProfileIngestionOrdinal: 4,
		CanonicalPayload:                      payload,
		CanonicalSHA256:                       model.SHA256Hex(payload),
	}
	targetPayload := []byte(`{"scheduler_state":{"box":2,"opaque":"TARGET_PROJECTION_CANARY"}}`)
	targetProjection := projection
	targetProjection.ProjectedCardIdentity = targetID
	targetProjection.LiveSourceIdeaID = &targetID
	targetProjection.LatestPresentedEventID = nil
	targetProjection.LatestPresentedEvidenceCardIdentity = nil
	targetProjection.LatestPresentedIngestionOrdinal = nil
	targetProjection.RebuiltThroughProfileIngestionOrdinal = 3
	targetProjection.CanonicalPayload = targetPayload
	targetProjection.CanonicalSHA256 = model.SHA256Hex(targetPayload)
	secondNamespacePayload := []byte(`{"algorithm":"other","opaque_extension":{"weights":[1,2,3],"canary":"V2_PROJECTION_CANARY"}}`)
	secondNamespace := projection
	secondNamespace.SchedulerVersion = "v2"
	secondNamespace.RebuiltThroughProfileIngestionOrdinal = 3
	secondNamespace.CanonicalPayload = secondNamespacePayload
	secondNamespace.CanonicalSHA256 = model.SHA256Hex(secondNamespacePayload)
	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		stored, replaceErr := store.ReplaceProjection(ctx, projection, 0)
		if replaceErr != nil {
			return replaceErr
		}
		readBack, readErr := store.GetProjection(ctx, StudyProjectionKey{
			LearnerProfileID: profile.ID, CardIdentity: sourceID, SchedulerName: "variety_first", SchedulerVersion: "v1", ProjectionSchemaVersion: 1,
		})
		if readErr != nil {
			return readErr
		}
		if readBack == nil || readBack.ProjectionRevision != stored.ProjectionRevision || string(readBack.CanonicalPayload) != string(payload) {
			return errors.New("projection read-after-replace mismatch")
		}
		if _, replaceErr = store.ReplaceProjection(ctx, targetProjection, 0); replaceErr != nil {
			return replaceErr
		}
		_, replaceErr = store.ReplaceProjection(ctx, secondNamespace, 0)
		return replaceErr
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PROJECTION_INSERT_FAILED", err))
	}
	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		_, replaceErr := store.ReplaceProjection(ctx, projection, 0)
		return replaceErr
	})
	if !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeStateStale}) {
		t.Fatal("STUDY_REPOSITORY.PROJECTION_CAS_NOT_ENFORCED")
	}

	projectionSnapshot := func() string {
		t.Helper()
		var snapshot string
		if err := pool.QueryRow(ctx, `
			SELECT coalesce(jsonb_agg(to_jsonb(projection)
				ORDER BY study_scheduler_name, study_scheduler_version,
					projection_schema_version, projected_study_card_identity)::text, '[]')
			FROM study_scheduler_card_projections projection
			WHERE projected_study_card_identity = ANY($1::uuid[])`, []uuid.UUID{sourceID, targetID}).Scan(&snapshot); err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PROJECTION_SNAPSHOT_FAILED", err))
		}
		return snapshot
	}
	projectionBeforeFailedMerge := projectionSnapshot()
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION study_test_fail_item_repoint() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'fixture study item repoint failure' USING ERRCODE = 'P0001';
		END
		$$;
		CREATE TRIGGER study_test_fail_item_repoint
		BEFORE UPDATE ON study_session_items
		FOR EACH ROW
		WHEN (OLD.live_source_idea_id IS DISTINCT FROM NEW.live_source_idea_id)
		EXECUTE FUNCTION study_test_fail_item_repoint()`); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PROJECTION_ROLLBACK_TRIGGER_CREATE_FAILED", err))
	}
	dropProjectionRollbackTrigger := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, `
			DROP TRIGGER IF EXISTS study_test_fail_item_repoint ON study_session_items;
			DROP FUNCTION IF EXISTS study_test_fail_item_repoint()`)
	}
	defer dropProjectionRollbackTrigger()
	if _, err := repository.MergeIdea(ctx, sourceID.String(), targetID.String()); err == nil {
		t.Fatal("STUDY_REPOSITORY.PROJECTION_INVALIDATION_FAILURE_DID_NOT_ROLL_BACK")
	}
	if projectionAfterFailedMerge := projectionSnapshot(); projectionAfterFailedMerge != projectionBeforeFailedMerge {
		t.Fatal("STUDY_REPOSITORY.PROJECTION_INVALIDATION_FAILURE_CHANGED_OPAQUE_ROWS")
	}
	var redirectExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM study_card_identity_redirects WHERE historical_study_card_identity = $1)`, sourceID).Scan(&redirectExists); err != nil || redirectExists {
		t.Fatal("STUDY_REPOSITORY.PROJECTION_INVALIDATION_FAILURE_LEFT_REDIRECT")
	}
	var rollbackLiveID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT live_source_idea_id FROM study_session_items WHERE study_session_item_id = $1`, root.Items[0].ID).Scan(&rollbackLiveID); err != nil || rollbackLiveID != sourceID {
		t.Fatal("STUDY_REPOSITORY.PROJECTION_INVALIDATION_FAILURE_REPOINTED_LIVE_ITEM")
	}
	dropProjectionRollbackTrigger()

	if _, err := repository.MergeIdea(ctx, sourceID.String(), targetID.String()); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.MERGE_FAILED", err))
	}
	items, err := repository.ListStudySessionItems(ctx, profileRef, root.Session.ID)
	if err != nil || len(items) != 1 || items[0].HistoricalSourceIdentity != sourceID || items[0].CardIdentity != sourceID || items[0].LiveSourceIdeaID == nil || *items[0].LiveSourceIdeaID != targetID {
		t.Fatal("STUDY_REPOSITORY.MERGE_REWROTE_HISTORY_OR_MISSED_LIVE_LINK")
	}
	var eventCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM study_events
		WHERE learner_profile_id = $1 AND study_card_identity = $2`, profile.ID, sourceID).Scan(&eventCount); err != nil || eventCount != 4 {
		t.Fatal("STUDY_REPOSITORY.MERGE_REWROTE_OR_REMOVED_ACCEPTED_EVENTS")
	}
	var projectionCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM study_scheduler_card_projections
		WHERE learner_profile_id = $1
		  AND projected_study_card_identity = ANY($2::uuid[])`, profile.ID, []uuid.UUID{sourceID, targetID}).Scan(&projectionCount); err != nil || projectionCount != 0 {
		t.Fatal("STUDY_REPOSITORY.MERGE_DID_NOT_INVALIDATE_OPAQUE_PROJECTIONS")
	}
	if err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		resolved, resolveErr := store.ResolveCardIdentity(ctx, sourceID)
		if resolveErr != nil {
			return resolveErr
		}
		if resolved != targetID {
			return errors.New("merge alias did not resolve to survivor")
		}
		return nil
	}); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.MERGE_ALIAS_RESOLUTION_FAILED", err))
	}
	resurrection := projection
	resurrection.CanonicalPayload = []byte(`{"fixture":"resurrection"}`)
	resurrection.CanonicalSHA256 = model.SHA256Hex(resurrection.CanonicalPayload)
	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		_, replaceErr := store.ReplaceProjection(ctx, resurrection, 0)
		return replaceErr
	})
	if !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeStateStale}) {
		t.Fatal("STUDY_REPOSITORY.HISTORICAL_PROJECTION_RESURRECTED_AFTER_MERGE")
	}
	missingIdentity := uuid.New()
	missingProjection := projection
	missingProjection.ProjectedCardIdentity = missingIdentity
	missingProjection.LiveSourceIdeaID = &missingIdentity
	missingProjection.LatestPresentedEventID = nil
	missingProjection.LatestPresentedEvidenceCardIdentity = nil
	missingProjection.LatestPresentedIngestionOrdinal = nil
	missingProjection.CanonicalPayload = []byte(`{"fixture":"missing"}`)
	missingProjection.CanonicalSHA256 = model.SHA256Hex(missingProjection.CanonicalPayload)
	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		_, replaceErr := store.ReplaceProjection(ctx, missingProjection, 0)
		return replaceErr
	})
	if !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeStateStale}) {
		t.Fatal("STUDY_REPOSITORY.NONEXISTENT_PROJECTION_IDENTITY_ACCEPTED")
	}
	var transferred bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM idea_topics WHERE idea_id = $1 AND topic_id = $2)`, targetID, scope.Scope.TopicIDs[0]).Scan(&transferred); err != nil || !transferred {
		t.Fatal("STUDY_REPOSITORY.MERGE_DID_NOT_TRANSFER_ELIGIBILITY")
	}
}

func testStudyHardDeletePreservesSnapshot(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	principal model.StudyPrincipalID,
) {
	t.Helper()
	deleteTopicSuffix := stringsForStudyFixture(uuid.NewString())
	deleteTopic, err := repository.CreateTopic(ctx, model.CreateTopicInput{
		Slug: "delete-" + deleteTopicSuffix,
		Name: "Delete " + deleteTopicSuffix,
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.DELETE_TOPIC_CREATE_FAILED", err))
	}
	idea := mustCreateStudyIdea(t, ctx, repository, deleteTopic.Slug, "delete")
	scope := mustStudyScope(t, deleteTopic.ID, deleteTopic.Slug)
	profileRef := "delete-profile"
	plan := testStudyPlan(uuid.New(), uuid.New(), principal, idea.ID, 0, "delete", nil)
	var result *StudyPlanInsertResult
	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
			return listErr
		}
		var insertErr error
		result, insertErr = store.InsertPlanOnce(ctx, plan)
		return insertErr
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.DELETE_FIXTURE_CREATE_FAILED", err))
	}
	profile, err := repository.GetStudyLearnerProfile(ctx, profileRef)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.DELETE_PROFILE_READ_FAILED", err))
	}
	events := insertAcceptedStudyPrefix(t, ctx, pool, *profile, result.Session, result.Items[0])
	projectionPayload := []byte(`{"fixture":"delete"}`)
	projection := model.StudySchedulerCardProjection{
		LearnerProfileID:                      profile.ID,
		ProjectedCardIdentity:                 idea.ID,
		LiveSourceIdeaID:                      &idea.ID,
		SchedulerName:                         "variety_first",
		SchedulerVersion:                      "v1",
		ProjectionSchemaVersion:               1,
		LatestPresentedEventID:                &events[0],
		LatestPresentedEvidenceCardIdentity:   &idea.ID,
		LatestPresentedIngestionOrdinal:       testInt64Pointer(1),
		RebuiltThroughProfileIngestionOrdinal: 4,
		CanonicalPayload:                      projectionPayload,
		CanonicalSHA256:                       model.SHA256Hex(projectionPayload),
	}
	if err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		_, replaceErr := store.ReplaceProjection(ctx, projection, 0)
		return replaceErr
	}); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.DELETE_PROJECTION_CREATE_FAILED", err))
	}
	if err := repository.DeleteIdea(ctx, idea.ID.String()); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.HARD_DELETE_FAILED", err))
	}
	items, err := repository.ListStudySessionItems(ctx, profileRef, result.Session.ID)
	if err != nil || len(items) != 1 || items[0].LiveSourceIdeaID != nil || items[0].HistoricalSourceIdentity != idea.ID || items[0].BackDefinition == "" {
		t.Fatal("STUDY_REPOSITORY.HARD_DELETE_DAMAGED_IMMUTABLE_SNAPSHOT")
	}
	var sourceStillStored uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT historical_source_identity FROM study_session_items WHERE study_session_item_id = $1`, result.Items[0].ID).Scan(&sourceStillStored); err != nil || sourceStillStored != idea.ID {
		t.Fatal("STUDY_REPOSITORY.HARD_DELETE_REMOVED_SOURCE_PROVENANCE")
	}
	var eventCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM study_events WHERE study_session_item_id = $1`, result.Items[0].ID).Scan(&eventCount); err != nil || eventCount != 4 {
		t.Fatal("STUDY_REPOSITORY.HARD_DELETE_REMOVED_ACCEPTED_EVENTS")
	}
	var projectionLiveID *uuid.UUID
	var projectedIdentity, evidenceIdentity uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT projected_study_card_identity, latest_presented_evidence_card_identity, live_source_idea_id
		FROM study_scheduler_card_projections
		WHERE learner_profile_id = $1 AND projected_study_card_identity = $2`, profile.ID, idea.ID).Scan(
		&projectedIdentity, &evidenceIdentity, &projectionLiveID,
	); err != nil || projectedIdentity != idea.ID || evidenceIdentity != idea.ID || projectionLiveID != nil {
		t.Fatal("STUDY_REPOSITORY.HARD_DELETE_DAMAGED_PROJECTION_OR_LIVE_LINK")
	}
}

func testConcurrentReverseMergeHasOneWinner(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repository *Repository) {
	t.Helper()
	left := mustCreateStudyIdea(t, ctx, repository, "", "reverse-left")
	right := mustCreateStudyIdea(t, ctx, repository, "", "reverse-right")
	barrier := make(chan struct{})
	workerContext, cancelWorkers := context.WithTimeout(ctx, 10*time.Second)
	var barrierMu sync.Mutex
	arrivals := 0
	orderedPairs := make([][2]uuid.UUID, 0, 2)
	repository.studyMergeBeforeLock = func(_ uuid.UUID, _ uuid.UUID, ordered [2]uuid.UUID) {
		barrierMu.Lock()
		orderedPairs = append(orderedPairs, ordered)
		arrivals++
		if arrivals == 2 {
			close(barrier)
		}
		barrierMu.Unlock()
		select {
		case <-barrier:
		case <-workerContext.Done():
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	for _, pair := range [][2]uuid.UUID{{left.ID, right.ID}, {right.ID, left.ID}} {
		pair := pair
		go func() {
			defer wait.Done()
			<-start
			_, err := repository.MergeIdea(workerContext, pair[0].String(), pair[1].String())
			results <- err
		}()
	}
	close(start)
	waitCompleted := make(chan struct{})
	go func() {
		wait.Wait()
		close(waitCompleted)
	}()
	defer func() {
		cancelWorkers()
		cleanupTimer := time.NewTimer(5 * time.Second)
		defer cleanupTimer.Stop()
		select {
		case <-waitCompleted:
		case <-cleanupTimer.C:
			t.Error("STUDY_REPOSITORY.REVERSE_MERGE_WORKER_CLEANUP_TIMEOUT")
		}
		repository.studyMergeBeforeLock = nil
	}()
	select {
	case <-waitCompleted:
	case <-workerContext.Done():
		t.Fatal("STUDY_REPOSITORY.REVERSE_MERGE_BARRIER_TIMEOUT")
	}
	cancelWorkers()
	close(results)
	successes := 0
	var loser error
	for err := range results {
		if err == nil {
			successes++
		} else {
			loser = err
		}
	}
	if successes != 1 {
		t.Fatalf("STUDY_REPOSITORY.REVERSE_MERGE_WINNER_COUNT=%d", successes)
	}
	if len(orderedPairs) != 2 || orderedPairs[0] != orderedPairs[1] {
		t.Fatal("STUDY_REPOSITORY.REVERSE_MERGE_LOCK_ORDER_DIVERGED")
	}
	var loserStudyError *model.StudyError
	if !errors.As(loser, &loserStudyError) || loserStudyError.Code != model.StudyErrorCodeServiceUnavailable || !loserStudyError.Retryable {
		t.Fatal("STUDY_REPOSITORY.REVERSE_MERGE_LOSER_NOT_TYPED_RETRYABLE")
	}
	repository.studyMergeBeforeLock = nil
	canonicalID, err := repository.ResolveIdeaID(ctx, left.ID.String())
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.REVERSE_MERGE_CANONICAL_READ_FAILED", err))
	}
	retried, err := repository.MergeIdea(ctx, left.ID.String(), right.ID.String())
	if err != nil || retried == nil || retried.ID != canonicalID {
		t.Fatal("STUDY_REPOSITORY.REVERSE_MERGE_RETRY_NOT_IDEMPOTENT")
	}
	var redirects int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM study_card_identity_redirects
		WHERE historical_study_card_identity = ANY($1::uuid[])
		  AND survivor_study_card_identity = ANY($1::uuid[])`, []uuid.UUID{left.ID, right.ID}).Scan(&redirects); err != nil || redirects != 1 {
		t.Fatal("STUDY_REPOSITORY.REVERSE_MERGE_REDIRECT_COUNT_INVALID")
	}
	var cycle bool
	if err := pool.QueryRow(ctx, `
		WITH RECURSIVE path(origin, identity, visited, cyclic) AS (
			SELECT historical_study_card_identity, survivor_study_card_identity,
				ARRAY[historical_study_card_identity, survivor_study_card_identity], false
			FROM study_card_identity_redirects
			WHERE historical_study_card_identity = ANY($1::uuid[])
			UNION ALL
			SELECT path.origin, redirect.survivor_study_card_identity,
				path.visited || redirect.survivor_study_card_identity,
				redirect.survivor_study_card_identity = ANY(path.visited)
			FROM path
			JOIN study_card_identity_redirects redirect ON redirect.historical_study_card_identity = path.identity
			WHERE NOT path.cyclic
		)
		SELECT coalesce(bool_or(cyclic), false) FROM path`, []uuid.UUID{left.ID, right.ID}).Scan(&cycle); err != nil || cycle {
		t.Fatal("STUDY_REPOSITORY.REVERSE_MERGE_CREATED_CYCLE")
	}
}

func testIdeaMutationAndMergeConvergence(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	principal model.StudyPrincipalID,
) {
	t.Helper()
	suffix := stringsForStudyFixture(uuid.NewString())
	primaryTopic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "merge-writes-" + suffix, Name: "Merge writes " + suffix})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.MERGE_WRITES_TOPIC_CREATE_FAILED", err))
	}
	extraTopic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "merge-extra-" + suffix, Name: "Merge extra " + suffix})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.MERGE_EXTRA_TOPIC_CREATE_FAILED", err))
	}
	source := mustCreateStudyIdea(t, ctx, repository, primaryTopic.Slug, "merge-writes-source")
	target := mustCreateStudyIdea(t, ctx, repository, "", "merge-writes-target")
	other := mustCreateStudyIdea(t, ctx, repository, "", "merge-writes-other")
	aliasCanary := "Merge alias canary " + suffix
	if _, err := repository.AddName(ctx, source.ID, model.CreateNameInput{Text: aliasCanary, Kind: model.NameKindAlias}); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PRELOCK_NAME_CREATE_FAILED", err))
	}
	definitionCanary := "Merge definition canary " + suffix
	if _, err := repository.CreateDefinition(ctx, source.ID.String(), model.CreateDefinitionInput{Body: definitionCanary}); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PRELOCK_DEFINITION_CREATE_FAILED", err))
	}
	noteCanary := "Merge note canary " + suffix
	if _, err := repository.CreateNote(ctx, source.ID.String(), model.CreateNoteInput{Body: noteCanary}); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PRELOCK_NOTE_CREATE_FAILED", err))
	}
	if _, err := repository.AssignTopic(ctx, source.ID.String(), extraTopic.ID.String()); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PRELOCK_TOPIC_ASSIGN_FAILED", err))
	}
	if _, err := repository.UpsertRelationship(ctx, source.ID.String(), other.ID.String(), model.RelationRelatedTo, nil); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PRELOCK_RELATIONSHIP_CREATE_FAILED", err))
	}
	sourceNote := "source acronym note"
	targetNote := "target acronym note"
	if _, err := repository.UpsertAcronymLink(ctx, model.UpsertAcronymLinkInput{Acronym: source.ID.String(), Expansion: other.ID.String(), Note: &sourceNote}); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PRELOCK_ACRONYM_CREATE_FAILED", err))
	}
	if _, err := repository.UpsertAcronymLink(ctx, model.UpsertAcronymLinkInput{Acronym: target.ID.String(), Expansion: other.ID.String(), Note: &targetNote}); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PRELOCK_ACRONYM_COLLISION_CREATE_FAILED", err))
	}
	if _, err := repository.UpsertAcronymLink(ctx, model.UpsertAcronymLinkInput{Acronym: source.ID.String(), Expansion: target.ID.String()}); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PRELOCK_ACRONYM_SELF_COLLAPSE_CREATE_FAILED", err))
	}
	scope := mustStudyScope(t, primaryTopic.ID, primaryTopic.Slug)
	plan := testStudyPlan(uuid.New(), uuid.New(), principal, source.ID, 0, "merge-writes", nil)
	var issued *StudyPlanInsertResult
	if err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: "merge-writes-" + suffix, Scope: scope}, func(store StudyStoreTransaction) error {
		if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
			return listErr
		}
		var insertErr error
		issued, insertErr = store.InsertPlanOnce(ctx, plan)
		return insertErr
	}); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PRELOCK_ITEM_CREATE_FAILED", err))
	}
	if _, err := repository.MergeIdea(ctx, source.ID.String(), target.ID.String()); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.PRELOCK_WRITES_MERGE_FAILED", err))
	}
	var aliasOwner, definitionOwner, noteOwner uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT idea_id FROM names WHERE text = $1`, aliasCanary).Scan(&aliasOwner); err != nil || aliasOwner != target.ID {
		t.Fatal("STUDY_REPOSITORY.MERGE_MISSED_PRELOCK_NAME")
	}
	if err := pool.QueryRow(ctx, `SELECT idea_id FROM definitions WHERE body = $1`, definitionCanary).Scan(&definitionOwner); err != nil || definitionOwner != target.ID {
		t.Fatal("STUDY_REPOSITORY.MERGE_MISSED_PRELOCK_DEFINITION")
	}
	if err := pool.QueryRow(ctx, `SELECT idea_id FROM notes WHERE body = $1`, noteCanary).Scan(&noteOwner); err != nil || noteOwner != target.ID {
		t.Fatal("STUDY_REPOSITORY.MERGE_MISSED_PRELOCK_NOTE")
	}
	var topicCount, relationshipCount, acronymCount, collapsedCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM idea_topics WHERE idea_id = $1 AND topic_id = ANY($2::uuid[])`, target.ID, []uuid.UUID{primaryTopic.ID, extraTopic.ID}).Scan(&topicCount); err != nil || topicCount != 2 {
		t.Fatal("STUDY_REPOSITORY.MERGE_MISSED_PRELOCK_TOPICS")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM relationships WHERE (from_idea_id = $1 AND to_idea_id = $2) OR (from_idea_id = $2 AND to_idea_id = $1)`, target.ID, other.ID).Scan(&relationshipCount); err != nil || relationshipCount != 1 {
		t.Fatal("STUDY_REPOSITORY.MERGE_MISSED_PRELOCK_RELATIONSHIP")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM acronym_links WHERE acronym_idea_id = $1 AND expansion_idea_id = $2`, target.ID, other.ID).Scan(&acronymCount); err != nil || acronymCount != 1 {
		t.Fatal("STUDY_REPOSITORY.MERGE_DID_NOT_CONSOLIDATE_ACRONYM_COLLISION")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM acronym_links WHERE acronym_idea_id = expansion_idea_id OR acronym_idea_id = $1 OR expansion_idea_id = $1`, source.ID).Scan(&collapsedCount); err != nil || collapsedCount != 0 {
		t.Fatal("STUDY_REPOSITORY.MERGE_LEFT_ACRONYM_SOURCE_OR_SELF_LINK")
	}
	items, err := repository.ListStudySessionItems(ctx, "merge-writes-"+suffix, issued.Session.ID)
	if err != nil || len(items) != 1 || items[0].HistoricalSourceIdentity != source.ID || items[0].LiveSourceIdeaID == nil || *items[0].LiveSourceIdeaID != target.ID {
		t.Fatal("STUDY_REPOSITORY.MERGE_MISSED_PRELOCK_ISSUED_ITEM")
	}

	mergedStatus := model.IdeaStatusMerged
	if _, err := repository.UpdateIdea(ctx, other.ID.String(), model.UpdateIdeaInput{Status: &mergedStatus}); !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeSchemaInvalid}) {
		t.Fatal("STUDY_REPOSITORY.PUBLIC_UPDATE_FORGED_MERGED_STATUS")
	}
	if _, err := pool.Exec(ctx, `UPDATE ideas SET merged_into_id = $2 WHERE id = $1`, other.ID, target.ID); err == nil {
		t.Fatal("STUDY_REPOSITORY.DATABASE_ACCEPTED_NONMERGED_REDIRECT")
	}

	racingTopic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "assign-race-" + suffix, Name: "Assign race " + suffix})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.ASSIGN_RACE_TOPIC_CREATE_FAILED", err))
	}
	racingSource := mustCreateStudyIdea(t, ctx, repository, "", "assign-race-source")
	racingTarget := mustCreateStudyIdea(t, ctx, repository, "", "assign-race-target")
	workerContext, cancelWorker := context.WithTimeout(ctx, 10*time.Second)
	resolved := make(chan struct{})
	release := make(chan struct{})
	var hookOnce sync.Once
	repository.ideaMutationAfterResolve = func(operation string, ids []uuid.UUID) {
		if operation != "topic_assign" || len(ids) != 1 || ids[0] != racingSource.ID {
			return
		}
		hookOnce.Do(func() { close(resolved) })
		select {
		case <-release:
		case <-workerContext.Done():
		}
	}
	workerResult := make(chan error, 1)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		_, assignErr := repository.AssignTopic(workerContext, racingSource.ID.String(), racingTopic.ID.String())
		workerResult <- assignErr
	}()
	defer func() {
		cancelWorker()
		cleanupTimer := time.NewTimer(5 * time.Second)
		defer cleanupTimer.Stop()
		select {
		case <-workerDone:
		case <-cleanupTimer.C:
			t.Error("STUDY_REPOSITORY.ASSIGN_RACE_WORKER_CLEANUP_TIMEOUT")
		}
		repository.ideaMutationAfterResolve = nil
	}()
	select {
	case <-resolved:
	case <-workerContext.Done():
		t.Fatal("STUDY_REPOSITORY.ASSIGN_RACE_RESOLUTION_TIMEOUT")
	}
	if _, err := repository.MergeIdea(ctx, racingSource.ID.String(), racingTarget.ID.String()); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.ASSIGN_RACE_MERGE_FAILED", err))
	}
	close(release)
	select {
	case err := <-workerResult:
		if err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.ASSIGN_RACE_RETRY_FAILED", err))
		}
	case <-workerContext.Done():
		t.Fatal("STUDY_REPOSITORY.ASSIGN_RACE_WORKER_TIMEOUT")
	}
	var assigned bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM idea_topics WHERE idea_id = $1 AND topic_id = $2)`, racingTarget.ID, racingTopic.ID).Scan(&assigned); err != nil || !assigned {
		t.Fatal("STUDY_REPOSITORY.ASSIGN_RACE_ACKNOWLEDGED_TOMBSTONE_WRITE")
	}

	runMergeWinsMutation := func(operation string, sourceID, targetID uuid.UUID, mutation func(context.Context) error) {
		t.Helper()
		mutationContext, cancelMutation := context.WithTimeout(ctx, 10*time.Second)
		reached := make(chan struct{})
		releaseMutation := make(chan struct{})
		mutationDone := make(chan struct{})
		mutationResult := make(chan error, 1)
		var reachedOnce sync.Once
		repository.ideaMutationAfterResolve = func(candidate string, ids []uuid.UUID) {
			if candidate != operation {
				return
			}
			containsSource := false
			for _, id := range ids {
				containsSource = containsSource || id == sourceID
			}
			if !containsSource {
				return
			}
			reachedOnce.Do(func() { close(reached) })
			select {
			case <-releaseMutation:
			case <-mutationContext.Done():
			}
		}
		go func() {
			defer close(mutationDone)
			mutationResult <- mutation(mutationContext)
		}()
		defer func() {
			cancelMutation()
			cleanupTimer := time.NewTimer(5 * time.Second)
			defer cleanupTimer.Stop()
			select {
			case <-mutationDone:
			case <-cleanupTimer.C:
				t.Error("STUDY_REPOSITORY.TWO_ENDPOINT_WORKER_CLEANUP_TIMEOUT")
			}
			repository.ideaMutationAfterResolve = nil
		}()
		select {
		case <-reached:
		case <-mutationContext.Done():
			t.Fatal("STUDY_REPOSITORY.TWO_ENDPOINT_RESOLUTION_TIMEOUT")
		}
		if _, err := repository.MergeIdea(ctx, sourceID.String(), targetID.String()); err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.TWO_ENDPOINT_RACE_MERGE_FAILED", err))
		}
		close(releaseMutation)
		select {
		case err := <-mutationResult:
			if err != nil {
				t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.TWO_ENDPOINT_RACE_RETRY_FAILED", err))
			}
		case <-mutationContext.Done():
			t.Fatal("STUDY_REPOSITORY.TWO_ENDPOINT_RACE_WORKER_TIMEOUT")
		}
	}

	relationshipSource := mustCreateStudyIdea(t, ctx, repository, "", "relationship-race-source")
	relationshipTarget := mustCreateStudyIdea(t, ctx, repository, "", "relationship-race-target")
	relationshipOther := mustCreateStudyIdea(t, ctx, repository, "", "relationship-race-other")
	runMergeWinsMutation("relationship_upsert", relationshipSource.ID, relationshipTarget.ID, func(workerCtx context.Context) error {
		_, err := repository.UpsertRelationship(workerCtx, relationshipSource.ID.String(), relationshipOther.ID.String(), model.RelationRelatedTo, nil)
		return err
	})
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM relationships
			WHERE (from_idea_id = $1 AND to_idea_id = $2)
			   OR (from_idea_id = $2 AND to_idea_id = $1)
		)`, relationshipTarget.ID, relationshipOther.ID).Scan(&assigned); err != nil || !assigned {
		t.Fatal("STUDY_REPOSITORY.RELATIONSHIP_RACE_ACKNOWLEDGED_TOMBSTONE_WRITE")
	}

	acronymSource := mustCreateStudyIdea(t, ctx, repository, "", "acronym-race-source")
	acronymTarget := mustCreateStudyIdea(t, ctx, repository, "", "acronym-race-target")
	acronymOther := mustCreateStudyIdea(t, ctx, repository, "", "acronym-race-other")
	runMergeWinsMutation("acronym_link_upsert", acronymSource.ID, acronymTarget.ID, func(workerCtx context.Context) error {
		_, err := repository.UpsertAcronymLink(workerCtx, model.UpsertAcronymLinkInput{
			Acronym: acronymSource.ID.String(), Expansion: acronymOther.ID.String(),
		})
		return err
	})
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM acronym_links
			WHERE acronym_idea_id = $1 AND expansion_idea_id = $2
		)`, acronymTarget.ID, acronymOther.ID).Scan(&assigned); err != nil || !assigned {
		t.Fatal("STUDY_REPOSITORY.ACRONYM_RACE_ACKNOWLEDGED_TOMBSTONE_WRITE")
	}
}

func testMergedSurvivorHardDelete(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	principal model.StudyPrincipalID,
) {
	t.Helper()
	suffix := stringsForStudyFixture(uuid.NewString())
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "delete-survivor-" + suffix, Name: "Delete survivor " + suffix})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.SURVIVOR_DELETE_TOPIC_FAILED", err))
	}
	first := mustCreateStudyIdea(t, ctx, repository, topic.Slug, "delete-survivor-first")
	second := mustCreateStudyIdea(t, ctx, repository, "", "delete-survivor-second")
	third := mustCreateStudyIdea(t, ctx, repository, "", "delete-survivor-third")
	scope := mustStudyScope(t, topic.ID, topic.Slug)
	profileRef := "delete-survivor-" + suffix
	plan := testStudyPlan(uuid.New(), uuid.New(), principal, first.ID, 0, "delete-survivor", nil)
	var created *StudyPlanInsertResult
	if err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
			return listErr
		}
		var insertErr error
		created, insertErr = store.InsertPlanOnce(ctx, plan)
		return insertErr
	}); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.SURVIVOR_DELETE_SESSION_FAILED", err))
	}
	profile, err := repository.GetStudyLearnerProfile(ctx, profileRef)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.SURVIVOR_DELETE_PROFILE_FAILED", err))
	}
	insertAcceptedStudyPrefix(t, ctx, pool, *profile, created.Session, created.Items[0])
	if _, err := repository.MergeIdea(ctx, first.ID.String(), second.ID.String()); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.SURVIVOR_DELETE_FIRST_MERGE_FAILED", err))
	}
	if _, err := repository.MergeIdea(ctx, second.ID.String(), third.ID.String()); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.SURVIVOR_DELETE_SECOND_MERGE_FAILED", err))
	}
	if err := repository.DeleteIdea(ctx, third.ID.String()); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.SURVIVOR_DELETE_FAILED", err))
	}
	var ideaCount, redirectCount, eventCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ideas WHERE id = ANY($1::uuid[])`, []uuid.UUID{first.ID, second.ID, third.ID}).Scan(&ideaCount); err != nil || ideaCount != 0 {
		t.Fatal("STUDY_REPOSITORY.SURVIVOR_DELETE_LEFT_VOCABULARY_TOMBSTONES")
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM study_card_identity_redirects
		WHERE historical_study_card_identity = ANY($1::uuid[])`, []uuid.UUID{first.ID, second.ID}).Scan(&redirectCount); err != nil || redirectCount != 2 {
		t.Fatal("STUDY_REPOSITORY.SURVIVOR_DELETE_REMOVED_IDENTITY_REDIRECTS")
	}
	items, err := repository.ListStudySessionItems(ctx, profileRef, created.Session.ID)
	if err != nil || len(items) != 1 || items[0].HistoricalSourceIdentity != first.ID || items[0].CardIdentity != first.ID || items[0].LiveSourceIdeaID != nil {
		t.Fatal("STUDY_REPOSITORY.SURVIVOR_DELETE_DAMAGED_IMMUTABLE_ITEM")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM study_events WHERE study_session_item_id = $1`, created.Items[0].ID).Scan(&eventCount); err != nil || eventCount != 4 {
		t.Fatal("STUDY_REPOSITORY.SURVIVOR_DELETE_REMOVED_ACCEPTED_EVENTS")
	}
}

func testStudyScopeInputIsolation(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	principal model.StudyPrincipalID,
) {
	t.Helper()
	suffix := stringsForStudyFixture(uuid.NewString())
	originalTopic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "scope-original-" + suffix, Name: "Scope original " + suffix})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.SCOPE_ORIGINAL_TOPIC_FAILED", err))
	}
	substituteTopic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "scope-substitute-" + suffix, Name: "Scope substitute " + suffix})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.SCOPE_SUBSTITUTE_TOPIC_FAILED", err))
	}
	originalIdea := mustCreateStudyIdea(t, ctx, repository, originalTopic.Slug, "scope-original")
	_ = mustCreateStudyIdea(t, ctx, repository, substituteTopic.Slug, "scope-substitute")
	expectedScope := mustStudyScope(t, originalTopic.ID, originalTopic.Slug)
	transactionScope := mustStudyScope(t, originalTopic.ID, originalTopic.Slug)
	input := StudyScopeTransactionInput{ProfileRef: "scope-isolation-" + suffix, Scope: transactionScope}
	plan := testStudyPlan(uuid.New(), uuid.New(), principal, originalIdea.ID, 0, "scope-isolation", nil)
	var created *StudyPlanInsertResult
	var expectedCorpusSHA256 string
	if err := repository.WithStudyScopeTransaction(ctx, input, func(store StudyStoreTransaction) error {
		input.Scope.Scope.TopicIDs[0] = substituteTopic.ID
		input.Scope.Scope.TopicRefs[0] = substituteTopic.Slug
		for index := range input.Scope.CanonicalSelector {
			input.Scope.CanonicalSelector[index] = 'x'
		}
		sources, listErr := store.ListEligibleSources(ctx)
		if listErr != nil {
			return listErr
		}
		if len(sources) != 1 || sources[0].IdeaID != originalIdea.ID {
			return errors.New("caller scope mutation changed the locked eligibility set")
		}
		expectedCorpusSHA256 = mustStudyEligibleCorpusDigest(t, expectedScope, sources)
		var insertErr error
		created, insertErr = store.InsertPlanOnce(ctx, plan)
		return insertErr
	}); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.SCOPE_INPUT_ISOLATION_FAILED", err))
	}
	if created == nil || len(created.Items) != 1 || created.Items[0].HistoricalSourceIdentity != originalIdea.ID ||
		created.Session.CorpusSHA256 != expectedCorpusSHA256 ||
		!sameStudyScope(created.Session.Scope, expectedScope) ||
		len(created.Session.Scope.Scope.TopicIDs) != 1 || created.Session.Scope.Scope.TopicIDs[0] != originalTopic.ID ||
		len(created.Session.Scope.Scope.TopicRefs) != 1 || created.Session.Scope.Scope.TopicRefs[0] != originalTopic.Slug {
		t.Fatal("STUDY_REPOSITORY.CALLER_SCOPE_MUTATION_CHANGED_PERSISTED_SCOPE")
	}
}

func mustCreateStudyIdea(t *testing.T, ctx context.Context, repository *Repository, topicRef, label string) *model.Idea {
	t.Helper()
	suffix := stringsForStudyFixture(uuid.NewString())
	summary := "Summary " + label
	idea, err := repository.CreateIdea(ctx, model.CreateIdeaInput{
		Summary:     &summary,
		Names:       []model.CreateNameInput{{Text: "Study " + label + " " + suffix, Kind: model.NameKindCanonical}},
		Definitions: []model.CreateDefinitionInput{{Body: "Definition " + label, IsPrimary: true}},
		Examples:    []model.CreateExampleInput{{Body: "Example " + label}},
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.IDEA_CREATE_FAILED", err))
	}
	if topicRef != "" {
		if _, err := repository.AssignTopic(ctx, idea.ID.String(), topicRef); err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.TOPIC_ASSIGN_FAILED", err))
		}
	}
	return idea
}

func stringsForStudyFixture(value string) string {
	if len(value) > 8 {
		return value[:8]
	}
	return value
}

func mustStudyScope(t *testing.T, topicID uuid.UUID, topicRef string) model.StudyNormalizedScope {
	t.Helper()
	scope, err := model.NormalizeStudyScope(model.StudyScope{
		TopicIDs: []uuid.UUID{topicID}, TopicRefs: []string{topicRef}, CardKind: model.StudyCardKindIdea,
	})
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func testStudyPlan(
	sessionID, requestID uuid.UUID,
	principal model.StudyPrincipalID,
	ideaID uuid.UUID,
	highWater int64,
	label string,
	predecessor *uuid.UUID,
) StudyPlanInsert {
	front := "Issued " + label
	back := "Issued definition " + label
	contentSHA := model.SHA256Hex([]byte(front + "\x00" + back))
	return StudyPlanInsert{
		SessionID: sessionID, Principal: principal, CreationRequestID: requestID,
		NormalizedRequestSHA256: model.SHA256Hex([]byte("request-" + label)),
		Lifecycle:               model.StudySessionLifecycleActive,
		CorpusSHA256:            model.SHA256Hex([]byte("corpus-" + label)),
		Scheduler: &model.StudySchedulerProvenance{
			Name: "variety_first", Version: "v1", ConfigurationSHA256: model.SHA256Hex([]byte("config")),
			InputProfileIngestionHighWater: highWater, PlannedAt: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC), Seed: "fixture-seed",
		},
		PredecessorSessionID: predecessor,
		LifecycleReasonCode:  "fixture_created",
		Items: []model.StudySessionItem{{
			ID: uuid.New(), StudySessionID: sessionID, Position: 0,
			CardKind: model.StudyCardKindIdea, CardIdentity: ideaID,
			HistoricalSourceKind: string(model.StudyCardKindIdea), HistoricalSourceIdentity: ideaID, LiveSourceIdeaID: &ideaID,
			SourceContentRevision: "fixture-v1", SourceContentSHA256: contentSHA,
			FrontCanonicalName: front, BackDefinition: back,
			SelectionReason: model.StudySelectionReason{
				RatingRank: model.StudyRatingRankUnseenUnrated, TieBreakerSHA256: model.SHA256Hex([]byte("tie-" + label)),
				BoundaryAdjustment: model.StudyBoundaryAdjustmentNone,
			},
		}},
	}
}

func mustStudyEligibleCorpusDigest(
	t *testing.T,
	scope model.StudyNormalizedScope,
	sources []StudyEligibleSource,
) string {
	t.Helper()
	digest, err := computeStudyEligibleCorpusDigest(scope, sources)
	if err != nil {
		t.Fatalf("STUDY_TEST.CORPUS_DIGEST_FAILED: %v", err)
	}
	return digest
}

func insertAcceptedStudyPrefix(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	profile model.StudyLearnerProfile,
	session model.StudySession,
	item model.StudySessionItem,
) []uuid.UUID {
	t.Helper()
	streamID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO study_client_streams (client_stream_id, learner_profile_id, adjudicated_through_client_sequence)
		VALUES ($1, $2, 4)`, streamID, profile.ID); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.EVENT_STREAM_FIXTURE_FAILED", err))
	}
	eventIDs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	kinds := []model.StudyEventKind{
		model.StudyEventKindPresented,
		model.StudyEventKindRevealed,
		model.StudyEventKindRated,
		model.StudyEventKindRatingVoided,
	}
	for index := range eventIDs {
		sequence := int64(index + 1)
		payloadSHA := model.SHA256Hex([]byte(fmt.Sprintf("accepted-event-%d", sequence)))
		if _, err := pool.Exec(ctx, `
			INSERT INTO study_event_id_bindings (
				client_event_id, bound_event_payload_sha256, learner_profile_id, study_session_id,
				study_session_item_id, study_card_identity, source_content_sha256,
				first_client_stream_id, first_client_sequence
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			eventIDs[index], payloadSHA, profile.ID, session.ID, item.ID, item.CardIdentity,
			item.SourceContentSHA256, streamID, sequence,
		); err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.EVENT_BINDING_FIXTURE_FAILED", err))
		}
		var rating any
		var target any
		if kinds[index] == model.StudyEventKindRated {
			rating = model.StudyRatingGood
		}
		if kinds[index] == model.StudyEventKindRatingVoided {
			target = eventIDs[2]
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO study_events (
				study_event_id, learner_profile_id, study_session_id, study_session_item_id,
				study_card_identity, study_event_kind, study_rating, rating_event_id,
				source_content_sha256, client_stream_id, client_sequence,
				normalized_event_payload_sha256, client_clock_quality, profile_ingestion_ordinal
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'unknown', $13)`,
			eventIDs[index], profile.ID, session.ID, item.ID, item.CardIdentity, kinds[index], rating, target,
			item.SourceContentSHA256, streamID, sequence, payloadSHA, sequence,
		); err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.EVENT_FIXTURE_FAILED", err))
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE study_learner_profiles SET profile_ingestion_high_water = 4 WHERE learner_profile_id = $1`, profile.ID); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_REPOSITORY.EVENT_HIGH_WATER_FAILED", err))
	}
	return eventIDs
}

func testInt64Pointer(value int64) *int64 { return &value }

func sameStudyStringPointer(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
