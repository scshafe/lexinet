package storage

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"lexinet/internal/model"
	studydigest "lexinet/internal/study/digest"
)

func TestStudyStep4ReplayPageMigrationForwardContractAndCleanup(t *testing.T) {
	harness := startStudyTestPostgres(t)
	migrator := harness.newMigrator(t)
	defer closeStudyTestMigrator(t, migrator)
	if err := migrator.Migrate(7); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.MIGRATION.BASELINE_0007_FAILED", err))
	}
	pool := harness.openPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	assertStudyReplayPageMigrationAbsent(t, ctx, pool)
	if err := migrator.Migrate(8); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.MIGRATION.FORWARD_0008_FAILED", err))
	}
	assertStudyReplayPageMigrationPresent(t, ctx, pool)

	// Migration 0008 is forward-only in production. Its down file exists for
	// local cleanup, not physical catalog equivalence: PostgreSQL deliberately
	// retains a dropped-column attnum tombstone. Exercise cleanup and a logical
	// reapply without weakening the full catalog digest used by the 0007 proof.
	if err := migrator.Migrate(7); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.MIGRATION.DOWN_TO_0007_FAILED", err))
	}
	assertStudyReplayPageMigrationAbsent(t, ctx, pool)
	if err := migrator.Migrate(8); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.MIGRATION.REAPPLY_0008_FAILED", err))
	}
	assertStudyReplayPageMigrationPresent(t, ctx, pool)
}

func assertStudyReplayPageMigrationAbsent(
	t *testing.T,
	ctx context.Context,
	pool dbPool,
) {
	t.Helper()
	var pageTableAbsent, markerColumnAbsent, validatorFunctionAbsent, insertGuardFunctionAbsent, studySessionTablePresent bool
	if err := pool.QueryRow(ctx, `
		SELECT
			to_regclass('public.study_flush_receipt_session_pages') IS NULL,
			NOT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = 'public'
				  AND table_name = 'study_flush_receipts'
				  AND column_name = 'session_page_artifact_required'
			),
			NOT EXISTS (
				SELECT 1 FROM pg_proc procedure
				JOIN pg_namespace namespace ON namespace.oid = procedure.pronamespace
				WHERE namespace.nspname = 'public'
				  AND procedure.proname = 'study_validate_required_flush_session_page'
			),
			NOT EXISTS (
				SELECT 1 FROM pg_proc procedure
				JOIN pg_namespace namespace ON namespace.oid = procedure.pronamespace
				WHERE namespace.nspname = 'public'
				  AND procedure.proname = 'study_reject_new_flush_without_session_page_requirement'
			),
			to_regclass('public.study_sessions') IS NOT NULL`).Scan(
		&pageTableAbsent,
		&markerColumnAbsent,
		&validatorFunctionAbsent,
		&insertGuardFunctionAbsent,
		&studySessionTablePresent,
	); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.MIGRATION.DOWN_INSPECTION_FAILED", err))
	}
	if !pageTableAbsent || !markerColumnAbsent || !validatorFunctionAbsent ||
		!insertGuardFunctionAbsent || !studySessionTablePresent {
		t.Fatal("STUDY_STEP4.MIGRATION.DOWN_SCOPE_INVALID")
	}
}

func assertStudyReplayPageMigrationPresent(
	t *testing.T,
	ctx context.Context,
	pool dbPool,
) {
	t.Helper()
	var pageTablePresent, markerDefaultTrue, requiredTriggerDeferred, insertGuardPresent, appendOnlyTriggerPresent, validatorFunctionPresent bool
	if err := pool.QueryRow(ctx, `
		SELECT
			to_regclass('public.study_flush_receipt_session_pages') IS NOT NULL,
			EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = 'public'
				  AND table_name = 'study_flush_receipts'
				  AND column_name = 'session_page_artifact_required'
				  AND is_nullable = 'NO'
				  AND column_default = 'true'
			),
			EXISTS (
				SELECT 1 FROM pg_trigger trigger_row
				JOIN pg_class relation ON relation.oid = trigger_row.tgrelid
				WHERE relation.relname = 'study_flush_receipts'
				  AND trigger_row.tgname = 'study_flush_receipts_require_session_page'
				  AND trigger_row.tgdeferrable
				  AND trigger_row.tginitdeferred
			),
			EXISTS (
				SELECT 1 FROM pg_trigger trigger_row
				JOIN pg_class relation ON relation.oid = trigger_row.tgrelid
				WHERE relation.relname = 'study_flush_receipts'
				  AND trigger_row.tgname = 'study_flush_receipts_require_page_marker_on_insert'
			),
			EXISTS (
				SELECT 1 FROM pg_trigger trigger_row
				JOIN pg_class relation ON relation.oid = trigger_row.tgrelid
				WHERE relation.relname = 'study_flush_receipt_session_pages'
				  AND trigger_row.tgname = 'study_flush_receipt_session_pages_append_only'
			),
			EXISTS (
				SELECT 1 FROM pg_proc procedure
				JOIN pg_namespace namespace ON namespace.oid = procedure.pronamespace
				WHERE namespace.nspname = 'public'
				  AND procedure.proname = 'study_validate_required_flush_session_page'
			)`).Scan(
		&pageTablePresent,
		&markerDefaultTrue,
		&requiredTriggerDeferred,
		&insertGuardPresent,
		&appendOnlyTriggerPresent,
		&validatorFunctionPresent,
	); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.MIGRATION.FORWARD_INSPECTION_FAILED", err))
	}
	if !pageTablePresent || !markerDefaultTrue || !requiredTriggerDeferred || !insertGuardPresent ||
		!appendOnlyTriggerPresent || !validatorFunctionPresent {
		t.Fatal("STUDY_STEP4.MIGRATION.FORWARD_CONTRACT_INVALID")
	}
}

func TestStudyStep4RequiredPageArtifactConstraintRollsBackRawFlush(t *testing.T) {
	harness := startStudyTestPostgres(t)
	harness.migrateUp(t)
	pool := harness.openPool(t)
	repository := NewRepository(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fixture := newStudyEventTestFixture(t, ctx, repository, "step4-required-page", "")
	profile, err := repository.GetStudyLearnerProfile(ctx, fixture.ProfileRef)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.REQUIRED_PAGE.PROFILE_READ_FAILED", err))
	}
	streamID := uuid.New()
	flushID := uuid.New()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.REQUIRED_PAGE.BEGIN_FAILED", err))
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO study_client_streams (client_stream_id, learner_profile_id)
		VALUES ($1, $2)`, streamID, profile.ID); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.REQUIRED_PAGE.STREAM_INSERT_FAILED", err))
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO study_flush_receipts (
			study_flush_receipt_id, study_principal_id, learner_profile_id,
			sync_request_id, normalized_sync_request_sha256, study_session_id,
			client_stream_id, acknowledged_through_client_sequence,
			profile_ingestion_high_water, study_session_state_version
		) VALUES ($1,$2,$3,$4,$5,$6,$7,0,$8,$9)`,
		flushID,
		fixture.Principal.String(),
		profile.ID,
		uuid.New(),
		model.SHA256Hex([]byte("required-page-without-artifact")),
		fixture.Plan.Session.ID,
		streamID,
		profile.ProfileIngestionHighWater,
		fixture.Plan.Session.StateVersion,
	); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.REQUIRED_PAGE.FLUSH_INSERT_FAILED", err))
	}
	commitErr := tx.Commit(ctx)
	var postgresError *pgconn.PgError
	if !errors.As(commitErr, &postgresError) || postgresError.Code != "23514" {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.REQUIRED_PAGE.MISSING_ARTIFACT_COMMIT_NOT_REJECTED", commitErr))
	}
	var flushCount, streamCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM study_flush_receipts WHERE study_flush_receipt_id = $1),
			(SELECT count(*) FROM study_client_streams WHERE client_stream_id = $2)`,
		flushID,
		streamID,
	).Scan(&flushCount, &streamCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.REQUIRED_PAGE.ROLLBACK_READ_FAILED", err))
	}
	if flushCount != 0 || streamCount != 0 {
		t.Fatal("STUDY_STEP4.REQUIRED_PAGE.PARTIAL_COMMIT_ESCAPED")
	}
}

func TestStudyStep4NewRawFlushCannotForgeLegacyPageMarker(t *testing.T) {
	harness := startStudyTestPostgres(t)
	harness.migrateUp(t)
	pool := harness.openPool(t)
	repository := NewRepository(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fixture := newStudyEventTestFixture(t, ctx, repository, "step4-explicit-false-page", "")
	profile, err := repository.GetStudyLearnerProfile(ctx, fixture.ProfileRef)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.PAGE_MARKER.PROFILE_READ_FAILED", err))
	}
	streamID := uuid.New()
	flushID := uuid.New()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.PAGE_MARKER.BEGIN_FAILED", err))
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO study_client_streams (client_stream_id, learner_profile_id)
		VALUES ($1, $2)`, streamID, profile.ID); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.PAGE_MARKER.STREAM_INSERT_FAILED", err))
	}
	_, insertErr := tx.Exec(ctx, `
		INSERT INTO study_flush_receipts (
			study_flush_receipt_id, study_principal_id, learner_profile_id,
			sync_request_id, normalized_sync_request_sha256, study_session_id,
			client_stream_id, acknowledged_through_client_sequence,
			profile_ingestion_high_water, study_session_state_version,
			session_page_artifact_required
		) VALUES ($1,$2,$3,$4,$5,$6,$7,0,$8,$9,false)`,
		flushID,
		fixture.Principal.String(),
		profile.ID,
		uuid.New(),
		model.SHA256Hex([]byte("explicit-false-page-marker")),
		fixture.Plan.Session.ID,
		streamID,
		profile.ProfileIngestionHighWater,
		fixture.Plan.Session.StateVersion,
	)
	var postgresError *pgconn.PgError
	if !errors.As(insertErr, &postgresError) || postgresError.Code != "23514" {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.PAGE_MARKER.EXPLICIT_FALSE_NOT_REJECTED", insertErr))
	}
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.PAGE_MARKER.ROLLBACK_FAILED", err))
	}
	var flushCount, streamCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM study_flush_receipts WHERE study_flush_receipt_id = $1),
			(SELECT count(*) FROM study_client_streams WHERE client_stream_id = $2)`,
		flushID,
		streamID,
	).Scan(&flushCount, &streamCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.PAGE_MARKER.ROLLBACK_READ_FAILED", err))
	}
	if flushCount != 0 || streamCount != 0 {
		t.Fatal("STUDY_STEP4.PAGE_MARKER.PARTIAL_COMMIT_ESCAPED")
	}
}

func TestStudyStep4LegacyFlushWithoutPageFailsExplicitly(t *testing.T) {
	harness := startStudyTestPostgres(t)
	migrator := harness.newMigrator(t)
	if err := migrator.Migrate(7); err != nil {
		closeStudyTestMigrator(t, migrator)
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.LEGACY.MIGRATE_TO_0007_FAILED", err))
	}
	closeStudyTestMigrator(t, migrator)
	pool := harness.openPool(t)
	repository := NewRepository(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fixture := newStudyEventTestFixture(t, ctx, repository, "step4-legacy-page", "")
	profile, err := repository.GetStudyLearnerProfile(ctx, fixture.ProfileRef)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.LEGACY.PROFILE_READ_FAILED", err))
	}
	streamID := uuid.New()
	syncID := uuid.New()
	requestSHA256 := model.SHA256Hex([]byte("legacy-page-request"))
	if _, err := pool.Exec(ctx, `
		INSERT INTO study_client_streams (client_stream_id, learner_profile_id)
		VALUES ($1, $2)`, streamID, profile.ID); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.LEGACY.STREAM_INSERT_FAILED", err))
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO study_flush_receipts (
			study_principal_id, learner_profile_id, sync_request_id,
			normalized_sync_request_sha256, study_session_id, client_stream_id,
			acknowledged_through_client_sequence, profile_ingestion_high_water,
			study_session_state_version
		) VALUES ($1,$2,$3,$4,$5,$6,0,$7,$8)`,
		fixture.Principal.String(),
		profile.ID,
		syncID,
		requestSHA256,
		fixture.Plan.Session.ID,
		streamID,
		profile.ProfileIngestionHighWater,
		fixture.Plan.Session.StateVersion,
	); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.LEGACY.FLUSH_INSERT_FAILED", err))
	}
	harness.migrateUp(t)

	_, err = repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           syncID,
		NormalizedRequestSHA256: requestSHA256,
		ClientStreamID:          streamID,
		ExpectedStateVersion:    fixture.Plan.Session.StateVersion,
		Events:                  []model.StudyEvent{},
	})
	assertStudyStep4ErrorCode(t, err, model.StudyErrorCodeInternal)
	var artifactCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM study_flush_receipt_session_pages`).Scan(&artifactCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.LEGACY.ARTIFACT_COUNT_FAILED", err))
	}
	if artifactCount != 0 {
		t.Fatal("STUDY_STEP4.LEGACY.ARTIFACT_WAS_INVENTED")
	}
}

func TestStudyStep4ExactPageReplaySurvivesMergeAndDelete(t *testing.T) {
	harness := startStudyTestPostgres(t)
	harness.migrateUp(t)
	pool := harness.openPool(t)
	repository := NewRepository(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture := newStudyEventTestFixture(t, ctx, repository, "step4-page-replay", "")
	target := mustCreateStudyIdea(t, ctx, repository, "", "step4-page-replay-target")
	originalArtifact := studyStep4FullPageArtifact(t, "immutable-page-canary")
	input := StudyEventSyncInput{
		PageArtifact:            originalArtifact,
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: model.SHA256Hex([]byte("step4-page-replay-request")),
		ClientStreamID:          uuid.New(),
		ExpectedStateVersion:    fixture.Plan.Session.StateVersion,
		Events:                  []model.StudyEvent{},
	}
	created, err := repository.AdjudicateStudyEvents(ctx, input)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.PAGE_REPLAY.INITIAL_FAILED", err))
	}
	if !reflect.DeepEqual(created.PageArtifact, originalArtifact) {
		t.Fatal("STUDY_STEP4.PAGE_REPLAY.INITIAL_BYTES_CHANGED")
	}

	if _, err := repository.MergeIdea(ctx, fixture.IdeaID.String(), target.ID.String()); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.PAGE_REPLAY.MERGE_FAILED", err))
	}
	input.PageArtifact = studyStep4FullPageArtifact(t, "mutable-merge-view")
	mergedReplay, err := repository.AdjudicateStudyEvents(ctx, input)
	if err != nil || !mergedReplay.Replayed || !reflect.DeepEqual(mergedReplay.PageArtifact, originalArtifact) {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.PAGE_REPLAY.MERGE_REPLAY_CHANGED", err))
	}

	if err := repository.DeleteIdea(ctx, target.ID.String()); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.PAGE_REPLAY.DELETE_FAILED", err))
	}
	input.PageArtifact = studyStep4PageArtifact(
		-1,
		1,
		`{"after_position":-1,"next_after_position":-1,"complete":true,"items":[]}`,
	)
	deletedReplay, err := repository.AdjudicateStudyEvents(ctx, input)
	if err != nil || !deletedReplay.Replayed || !reflect.DeepEqual(deletedReplay.PageArtifact, originalArtifact) {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.PAGE_REPLAY.DELETE_REPLAY_CHANGED", err))
	}
	input.PageArtifact = model.StudySessionPageArtifact{}
	invalidCandidateReplay, err := repository.AdjudicateStudyEvents(ctx, input)
	if err != nil || !invalidCandidateReplay.Replayed || !reflect.DeepEqual(invalidCandidateReplay.PageArtifact, originalArtifact) {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.PAGE_REPLAY.INVALID_CANDIDATE_WAS_NOT_IGNORED", err))
	}
	var storedJSON, storedSHA256 string
	if err := pool.QueryRow(ctx, `
		SELECT canonical_session_page, canonical_session_page_sha256
		FROM study_flush_receipt_session_pages
		WHERE study_flush_receipt_id = $1`, created.FlushReceipt.ID).Scan(&storedJSON, &storedSHA256); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.PAGE_REPLAY.STORED_READ_FAILED", err))
	}
	if storedJSON != string(originalArtifact.CanonicalJSON) || storedSHA256 != originalArtifact.CanonicalSHA256 {
		t.Fatal("STUDY_STEP4.PAGE_REPLAY.STORED_BYTES_CHANGED")
	}
}

func TestStudyStep4ScopeResolutionAndTopicDeleteRaceFailClosed(t *testing.T) {
	harness := startStudyTestPostgres(t)
	harness.migrateUp(t)
	pool := harness.openPool(t)
	repository := NewRepository(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "step4-canonical-topic", Name: "step4-topic-alias"})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.SCOPE.TOPIC_CREATE_FAILED", err))
	}
	resolved, err := repository.ResolveStudyScope(ctx, []string{topic.Name}, model.StudyCardKindIdea)
	if err != nil || len(resolved.Scope.TopicIDs) != 1 || resolved.Scope.TopicIDs[0] != topic.ID ||
		len(resolved.Scope.TopicRefs) != 1 || resolved.Scope.TopicRefs[0] != topic.Slug {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.SCOPE.CANONICAL_RESOLUTION_FAILED", err))
	}
	_, err = repository.ResolveStudyScope(ctx, []string{topic.Slug, topic.Name}, model.StudyCardKindIdea)
	assertStudyStep4ErrorCode(t, err, model.StudyErrorCodeSchemaInvalid)

	racingTopic, err := repository.CreateTopic(ctx, model.CreateTopicInput{
		Slug: "step4-delete-race-" + stringsForStudyFixture(uuid.NewString()),
		Name: "step4-delete-race-name-" + stringsForStudyFixture(uuid.NewString()),
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.SCOPE.RACE_TOPIC_CREATE_FAILED", err))
	}
	racingIdea := mustCreateStudyIdea(t, ctx, repository, racingTopic.Slug, "step4-delete-race")
	racingScope := mustStudyScope(t, racingTopic.ID, racingTopic.Slug)

	deleteTx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.SCOPE.DELETE_BEGIN_FAILED", err))
	}
	defer deleteTx.Rollback(ctx)
	var lockedIdeaID uuid.UUID
	if err := deleteTx.QueryRow(ctx, `SELECT id FROM ideas WHERE id = $1 FOR UPDATE`, racingIdea.ID).Scan(&lockedIdeaID); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.SCOPE.DELETE_IDEA_LOCK_FAILED", err))
	}
	if _, err := deleteTx.Exec(ctx, `DELETE FROM topics WHERE id = $1`, racingTopic.ID); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.SCOPE.DELETE_STAGE_FAILED", err))
	}

	workerRepository, applicationName, workerPool := newAdversarialWorkerRepository(t, ctx, pool, "step4_topic_delete")
	defer workerPool.Close()
	workerDone := make(chan struct{})
	workerError := make(chan error, 1)
	go func() {
		defer close(workerDone)
		workerError <- workerRepository.WithStudyScopeTransaction(
			ctx,
			StudyScopeTransactionInput{ProfileRef: "step4-delete-race-profile", Scope: racingScope},
			func(store StudyStoreTransaction) error {
				_, listErr := store.ListEligibleSources(ctx)
				return listErr
			},
		)
	}()
	if err := waitForAdversarialPostgresLock(ctx, pool, applicationName, workerDone); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.SCOPE.RACE_LOCK_NOT_OBSERVED", err))
	}
	if err := deleteTx.Commit(ctx); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.SCOPE.DELETE_COMMIT_FAILED", err))
	}
	select {
	case err := <-workerError:
		assertStudyStep4ErrorCode(t, err, model.StudyErrorCodeStateStale)
	case <-ctx.Done():
		t.Fatal("STUDY_STEP4.SCOPE.RACE_WORKER_TIMEOUT")
	}
}

func TestStudyStep4CommittedReadsUseExactReceiptAndOneSnapshot(t *testing.T) {
	harness := startStudyTestPostgres(t)
	harness.migrateUp(t)
	pool := harness.openPool(t)
	repository := NewRepository(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture := newStudyEventTestFixture(t, ctx, repository, "step4-readback", "")
	owner, err := repository.GetStudyLearnerProfileBySessionID(ctx, fixture.Plan.Session.ID)
	if err != nil || owner.ID != fixture.Plan.Session.LearnerProfileID || owner.Ref != fixture.ProfileRef {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.READBACK.SESSION_OWNER_FAILED", err))
	}
	_, err = repository.GetStudyLearnerProfileBySessionID(ctx, uuid.New())
	assertStudyStep4ErrorCode(t, err, model.StudyErrorCodeSessionNotFound)
	readInput := StudyCommittedCycleReadInput{
		Principal:         fixture.Principal,
		ProfileRef:        fixture.ProfileRef,
		SessionID:         fixture.Plan.Session.ID,
		CreationRequestID: fixture.Plan.Receipt.CreationRequestID,
	}
	committed, err := repository.ReadCommittedStudyCycle(ctx, readInput)
	if err != nil || !reflect.DeepEqual(committed.Session, fixture.Plan.Session) ||
		!reflect.DeepEqual(committed.Items, fixture.Plan.Items) ||
		!reflect.DeepEqual(committed.Receipt, fixture.Plan.Receipt) {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.READBACK.EXACT_RECEIPT_FAILED", err))
	}
	readInput.CreationRequestID = uuid.New()
	_, err = repository.ReadCommittedStudyCycle(ctx, readInput)
	assertStudyStep4ErrorCode(t, err, model.StudyErrorCodeInternal)

	oldStateVersion := fixture.Plan.Session.StateVersion
	oldLiveSource := *fixture.Plan.Items[0].LiveSourceIdeaID
	writer, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.READBACK.WRITER_BEGIN_FAILED", err))
	}
	defer writer.Rollback(ctx)
	if _, err := writer.Exec(ctx, `LOCK TABLE study_session_items IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.READBACK.ITEM_TABLE_LOCK_FAILED", err))
	}
	if _, err := writer.Exec(ctx, `
		UPDATE study_sessions
		SET study_session_state_version = study_session_state_version + 1
		WHERE study_session_id = $1`, fixture.Plan.Session.ID); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.READBACK.SESSION_STAGE_FAILED", err))
	}
	if _, err := writer.Exec(ctx, `
		UPDATE study_session_items
		SET live_source_idea_id = NULL
		WHERE study_session_id = $1`, fixture.Plan.Session.ID); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.READBACK.ITEM_STAGE_FAILED", err))
	}

	readerRepository, applicationName, readerPool := newAdversarialWorkerRepository(t, ctx, pool, "step4_snapshot_read")
	defer readerPool.Close()
	readerDone := make(chan struct{})
	type readOutcome struct {
		cycle *StudySessionCycleRead
		err   error
	}
	readerOutcome := make(chan readOutcome, 1)
	go func() {
		defer close(readerDone)
		cycle, readErr := readerRepository.ReadStudySessionCycle(ctx, fixture.Plan.Session.ID)
		readerOutcome <- readOutcome{cycle: cycle, err: readErr}
	}()
	if err := waitForAdversarialPostgresLock(ctx, pool, applicationName, readerDone); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.READBACK.SNAPSHOT_BLOCK_NOT_OBSERVED", err))
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.READBACK.WRITER_COMMIT_FAILED", err))
	}
	select {
	case outcome := <-readerOutcome:
		if outcome.err != nil || outcome.cycle == nil || outcome.cycle.Profile.Ref != fixture.ProfileRef ||
			outcome.cycle.Session.StateVersion != oldStateVersion || len(outcome.cycle.Items) != 1 ||
			outcome.cycle.Items[0].LiveSourceIdeaID == nil || *outcome.cycle.Items[0].LiveSourceIdeaID != oldLiveSource {
			t.Fatal(studyTestDatabaseError("STUDY_STEP4.READBACK.SNAPSHOT_MIXED_GENERATIONS", outcome.err))
		}
	case <-ctx.Done():
		t.Fatal("STUDY_STEP4.READBACK.SNAPSHOT_READER_TIMEOUT")
	}
	current, err := repository.ReadStudySessionCycle(ctx, fixture.Plan.Session.ID)
	if err != nil || current.Session.StateVersion != oldStateVersion+1 ||
		len(current.Items) != 1 || current.Items[0].LiveSourceIdeaID != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.READBACK.CURRENT_SNAPSHOT_INVALID", err))
	}
}

func TestStudyStep4StreamLockPrecedesFlushReceiptLock(t *testing.T) {
	harness := startStudyTestPostgres(t)
	harness.migrateUp(t)
	pool := harness.openPool(t)
	repository := NewRepository(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture := newStudyEventTestFixture(t, ctx, repository, "step4-lock-order", "")
	input := StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: model.SHA256Hex([]byte("step4-lock-order-request")),
		ClientStreamID:          uuid.New(),
		ExpectedStateVersion:    fixture.Plan.Session.StateVersion,
		Events:                  []model.StudyEvent{},
	}
	created, err := repository.AdjudicateStudyEvents(ctx, input)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.LOCK_ORDER.INITIAL_SYNC_FAILED", err))
	}

	flushBlocker, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.LOCK_ORDER.BLOCKER_BEGIN_FAILED", err))
	}
	defer flushBlocker.Rollback(ctx)
	var lockedFlushID uuid.UUID
	if err := flushBlocker.QueryRow(ctx, `
		SELECT study_flush_receipt_id
		FROM study_flush_receipts
		WHERE study_flush_receipt_id = $1
		FOR UPDATE`, created.FlushReceipt.ID).Scan(&lockedFlushID); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.LOCK_ORDER.FLUSH_LOCK_FAILED", err))
	}

	workerRepository, applicationName, workerPool := newAdversarialWorkerRepository(t, ctx, pool, "step4_lock_order")
	defer workerPool.Close()
	workerDone := make(chan struct{})
	type syncOutcome struct {
		result *StudyEventSyncResult
		err    error
	}
	workerOutcome := make(chan syncOutcome, 1)
	go func() {
		defer close(workerDone)
		result, replayErr := workerRepository.AdjudicateStudyEvents(ctx, input)
		workerOutcome <- syncOutcome{result: result, err: replayErr}
	}()
	if err := waitForAdversarialPostgresLock(ctx, pool, applicationName, workerDone); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.LOCK_ORDER.FLUSH_WAIT_NOT_OBSERVED", err))
	}
	var observedStreamID uuid.UUID
	err = pool.QueryRow(ctx, `
		SELECT client_stream_id
		FROM study_client_streams
		WHERE client_stream_id = $1
		FOR UPDATE NOWAIT`, input.ClientStreamID).Scan(&observedStreamID)
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != "55P03" {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.LOCK_ORDER.STREAM_WAS_NOT_LOCKED_FIRST", err))
	}
	if err := flushBlocker.Commit(ctx); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4.LOCK_ORDER.BLOCKER_COMMIT_FAILED", err))
	}
	select {
	case outcome := <-workerOutcome:
		if outcome.err != nil || outcome.result == nil || !outcome.result.Replayed ||
			!reflect.DeepEqual(outcome.result.PageArtifact, created.PageArtifact) {
			t.Fatal(studyTestDatabaseError("STUDY_STEP4.LOCK_ORDER.REPLAY_FAILED", outcome.err))
		}
	case <-ctx.Done():
		t.Fatal("STUDY_STEP4.LOCK_ORDER.REPLAY_TIMEOUT")
	}
}

func studyStep4PageArtifact(
	afterPosition int,
	limit int,
	canonicalJSON string,
) model.StudySessionPageArtifact {
	bytes := []byte(canonicalJSON)
	return model.StudySessionPageArtifact{
		Request: model.StudySessionPageRequest{
			AfterPosition: afterPosition,
			Limit:         limit,
		},
		CanonicalJSON:   bytes,
		CanonicalSHA256: model.SHA256Hex(bytes),
	}
}

func studyStep4FullPageArtifact(t *testing.T, canonicalName string) model.StudySessionPageArtifact {
	t.Helper()
	contentSHA256, err := studydigest.CardContent(
		[]byte(model.StudyCardKindIdea),
		[]byte("33333333-3333-4333-8333-333333333333"),
		[]byte("33333333-3333-4333-8333-333333333333"),
		[]byte(canonicalName),
		[]byte("A bounded storage replay fixture."),
		[]byte{0},
		nil,
	)
	if err != nil {
		t.Fatalf("STUDY_STEP4.PAGE_FIXTURE.DIGEST_FAILED: %v", err)
	}
	liveSourceID := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	canonicalJSON, err := model.EncodeStudySessionPageDocument(model.StudySessionPageDocument{
		AfterPosition:     -1,
		NextAfterPosition: 0,
		Complete:          true,
		Items: []model.StudySessionPageCardProjection{{
			SessionItemID:    uuid.MustParse("22222222-2222-4222-8222-222222222222"),
			Position:         0,
			CardKind:         model.StudyCardKindIdea,
			CardIdentity:     uuid.MustParse("33333333-3333-4333-8333-333333333333"),
			LiveSourceIdeaID: &liveSourceID,
			SourceRevision: model.StudySessionPageSourceRevision{
				SourceKind:      "idea",
				SourceIdentity:  uuid.MustParse("33333333-3333-4333-8333-333333333333"),
				ContentRevision: "fixture-revision-1",
				ContentSHA256:   contentSHA256,
			},
			Front: model.StudySessionPageCardFront{CanonicalName: canonicalName},
			Back:  model.StudySessionPageCardBack{Definition: "A bounded storage replay fixture."},
			SelectionReason: model.StudySessionPageSelectionReason{
				RatingRank:         model.StudyRatingRankUnseenUnrated,
				TieBreakerSHA256:   strings.Repeat("e", 64),
				BoundaryAdjustment: model.StudyBoundaryAdjustmentNone,
			},
		}},
	})
	if err != nil {
		t.Fatalf("STUDY_STEP4.PAGE_FIXTURE.ENCODE_FAILED: %v", err)
	}
	return studyStep4PageArtifact(-1, 1, string(canonicalJSON))
}

func assertStudyStep4ErrorCode(t *testing.T, err error, want model.StudyErrorCode) {
	t.Helper()
	var studyError *model.StudyError
	if !errors.As(err, &studyError) || studyError.Code != want {
		t.Fatalf("STUDY_STEP4.ERROR_CODE_INVALID: got=%T want=%s", err, want)
	}
}
