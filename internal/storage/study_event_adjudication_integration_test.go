package storage

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lexinet/internal/model"
)

type studyEventTestFixture struct {
	ProfileRef string
	Principal  model.StudyPrincipalID
	Scope      model.StudyNormalizedScope
	IdeaID     uuid.UUID
	Plan       *StudyPlanInsertResult
}

// TestPlanLexinetStep2Verify1Item2MigrateUpExerciseStreamRestartReprovisionSemanticDuplicate
// is the exact Step 2 verification-map gate for immutable replay, one atomic
// suffix, terminal consumption, and reprovisioned-stream UUID authority.
func TestPlanLexinetStep2Verify1Item2MigrateUpExerciseStreamRestartReprovisionSemanticDuplicate(t *testing.T) {
	harness := startStudyTestPostgres(t)
	harness.migrateUp(t)
	pool := harness.openPool(t)
	repository := NewRepository(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	fixture := newStudyEventTestFixture(t, ctx, repository, "item2", "")
	item := fixture.Plan.Items[0]
	streamID := uuid.New()
	presentedID := uuid.New()
	revealedID := uuid.New()
	ratedID := uuid.New()
	voidedID := uuid.New()
	ratingGood := model.StudyRatingGood
	initialEvents := []model.StudyEvent{
		newStudyEventTestEvent(fixture, item, streamID, 1, presentedID, model.StudyEventKindPresented, nil, nil),
		newStudyEventTestEvent(fixture, item, streamID, 2, revealedID, model.StudyEventKindRevealed, nil, nil),
		newStudyEventTestEvent(fixture, item, streamID, 3, ratedID, model.StudyEventKindRated, &ratingGood, nil),
		newStudyEventTestEvent(fixture, item, streamID, 4, voidedID, model.StudyEventKindRatingVoided, nil, &ratedID),
	}
	initialInput := StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("item2-initial"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    fixture.Plan.Session.StateVersion,
		Events:                  initialEvents,
	}
	initial, err := repository.AdjudicateStudyEvents(ctx, initialInput)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.ITEM2.INITIAL_SYNC_FAILED", err))
	}
	assertStudyEventAcceptedRange(t, initial, 1, 4, 1)
	if initial.FlushReceipt.RequestFirstClientSequence == nil || *initial.FlushReceipt.RequestFirstClientSequence != 1 ||
		initial.FlushReceipt.RequestLastClientSequence == nil || *initial.FlushReceipt.RequestLastClientSequence != 4 ||
		initial.FlushReceipt.FirstClientSequence == nil || *initial.FlushReceipt.FirstClientSequence != 1 ||
		initial.FlushReceipt.LastClientSequence == nil || *initial.FlushReceipt.LastClientSequence != 4 ||
		initial.FlushReceipt.AcknowledgedThroughSequence != 4 ||
		initial.FlushReceipt.ProfileIngestionHighWater != 4 ||
		initial.Session.StateVersion != fixture.Plan.Session.StateVersion+1 {
		t.Fatal("STUDY_EVENT.ITEM2.INITIAL_FLUSH_INVALID")
	}

	// A process restart must discover the immutable flush before applying the
	// now-stale expected state version and return byte-stable receipt authority.
	restartedRepository := NewRepository(pool)
	replayed, err := restartedRepository.AdjudicateStudyEvents(ctx, initialInput)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.ITEM2.RESTART_REPLAY_FAILED", err))
	}
	if !replayed.Replayed || replayed.FlushReceipt.ID != initial.FlushReceipt.ID ||
		len(replayed.Receipts) != len(initial.Receipts) {
		t.Fatal("STUDY_EVENT.ITEM2.RESTART_REPLAY_UNSTABLE")
	}
	for index := range initial.Receipts {
		if !reflect.DeepEqual(replayed.Receipts[index], initial.Receipts[index]) {
			t.Fatal("STUDY_EVENT.ITEM2.RECEIPT_REPLAY_UNSTABLE")
		}
	}

	assertStudyEventSyncRejectedWithoutWrites(t, ctx, pool, restartedRepository, initialInput, func(input *StudyEventSyncInput) {
		input.Events = nil
	}, model.StudyErrorCodeIdempotencyConflict, "EMPTY_REUSED_SYNC")
	assertStudyEventSyncRejectedWithoutWrites(t, ctx, pool, restartedRepository, initialInput, func(input *StudyEventSyncInput) {
		input.Events = append([]model.StudyEvent(nil), input.Events[:3]...)
	}, model.StudyErrorCodeIdempotencyConflict, "TRUNCATED_REUSED_SYNC")
	assertStudyEventSyncRejectedWithoutWrites(t, ctx, pool, restartedRepository, initialInput, func(input *StudyEventSyncInput) {
		extra := newStudyEventTestEvent(fixture, item, streamID, 5, uuid.New(), model.StudyEventKindPresented, nil, nil)
		input.Events = append(append([]model.StudyEvent(nil), input.Events...), extra)
	}, model.StudyErrorCodeIdempotencyConflict, "EXTENDED_REUSED_SYNC")
	assertStudyEventSyncRejectedWithoutWrites(t, ctx, pool, restartedRepository, initialInput, func(input *StudyEventSyncInput) {
		input.Events = append([]model.StudyEvent(nil), input.Events...)
		input.Events[2].ID = uuid.New()
	}, model.StudyErrorCodeIdempotencyConflict, "CHANGED_REUSED_SYNC")
	assertStudyEventSyncRejectedWithoutWrites(t, ctx, pool, restartedRepository, initialInput, func(input *StudyEventSyncInput) {
		input.NormalizedRequestSHA256 = studyEventTestHash("changed-reused-sync-hash")
	}, model.StudyErrorCodeIdempotencyConflict, "CHANGED_REUSED_SYNC_HASH")

	duplicateInput := StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("item2-semantic-duplicate"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    initial.Session.StateVersion,
		Events: []model.StudyEvent{
			newStudyEventTestEvent(fixture, item, streamID, 5, uuid.New(), model.StudyEventKindPresented, nil, nil),
			newStudyEventTestEvent(fixture, item, streamID, 6, uuid.New(), model.StudyEventKindRated, &ratingGood, nil),
		},
	}
	duplicate, err := restartedRepository.AdjudicateStudyEvents(ctx, duplicateInput)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.ITEM2.SEMANTIC_DUPLICATE_FAILED", err))
	}
	assertStudyEventSemanticDuplicate(t, duplicate.Receipts[0], presentedID, 1, "STUDY_EVENT.ITEM2.SEMANTIC_DUPLICATE_INVALID")
	if len(duplicate.Receipts) != 2 ||
		duplicate.Receipts[1].Disposition != model.StudyEventReceiptDispositionAccepted ||
		duplicate.Receipts[1].CanonicalProfileIngestionOrdinal == nil ||
		*duplicate.Receipts[1].CanonicalProfileIngestionOrdinal != 5 ||
		duplicate.FlushReceipt.ProfileIngestionHighWater != 5 || duplicate.FlushReceipt.AcknowledgedThroughSequence != 6 {
		t.Fatal("STUDY_EVENT.ITEM2.MIXED_SUCCESS_SUFFIX_INVALID")
	}
	mixedRatedID := duplicate.Receipts[1].EventID

	ratingHard := model.StudyRatingHard
	terminalEvents := []model.StudyEvent{
		newStudyEventTestEvent(fixture, item, streamID, 7, uuid.New(), model.StudyEventKindRatingVoided, nil, &mixedRatedID),
		newStudyEventTestEvent(fixture, item, streamID, 8, uuid.New(), model.StudyEventKindRated, &ratingGood, nil),
		newStudyEventTestEvent(fixture, item, streamID, 9, uuid.New(), model.StudyEventKindRated, &ratingHard, nil),
		newStudyEventTestEvent(fixture, item, streamID, 10, uuid.New(), model.StudyEventKindRatingVoided, nil, nil),
	}
	terminalInput := StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("item2-terminal-middle"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    duplicate.Session.StateVersion,
		Events:                  terminalEvents,
	}
	// The collateral event still needs a schema-valid target even though no
	// accepted fact is allocated for the failed batch.
	terminalEvents[3].RatingEventID = &terminalEvents[1].ID
	terminalInput.Events = terminalEvents
	terminal, err := restartedRepository.AdjudicateStudyEvents(ctx, terminalInput)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.ITEM2.TERMINAL_GROUP_FAILED", err))
	}
	if len(terminal.Receipts) != 4 || terminal.FlushReceipt.ProfileIngestionHighWater != 5 ||
		terminal.FlushReceipt.AcknowledgedThroughSequence != 10 {
		t.Fatal("STUDY_EVENT.ITEM2.TERMINAL_GROUP_RANGE_INVALID")
	}
	for index := range terminal.Receipts {
		receipt := terminal.Receipts[index]
		if receipt.Disposition != model.StudyEventReceiptDispositionTerminalRejected ||
			receipt.Code != model.StudyEventReceiptCodeRatingAlreadyEffective ||
			receipt.TerminalFailureClientSequence == nil || *receipt.TerminalFailureClientSequence != 9 ||
			receipt.CanonicalStudyEventID != nil || receipt.CanonicalProfileIngestionOrdinal != nil ||
			receipt.OriginatingStudyFlushReceiptID != terminal.FlushReceipt.ID {
			t.Fatal("STUDY_EVENT.ITEM2.TERMINAL_GROUP_RECEIPT_INVALID")
		}
	}

	voidedAfterFailureID := uuid.New()
	replayAndSuffix := []model.StudyEvent{
		terminalEvents[3],
		newStudyEventTestEvent(fixture, item, streamID, 11, voidedAfterFailureID, model.StudyEventKindRatingVoided, nil, &mixedRatedID),
	}
	continued, err := restartedRepository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("item2-prefix-and-suffix"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    terminal.Session.StateVersion,
		Events:                  replayAndSuffix,
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.ITEM2.PREFIX_SUFFIX_FAILED", err))
	}
	if len(continued.Receipts) != 2 || !reflect.DeepEqual(continued.Receipts[0], terminal.Receipts[3]) ||
		continued.Receipts[1].Disposition != model.StudyEventReceiptDispositionAccepted ||
		continued.Receipts[1].CanonicalProfileIngestionOrdinal == nil ||
		*continued.Receipts[1].CanonicalProfileIngestionOrdinal != 6 ||
		continued.FlushReceipt.FirstClientSequence == nil || *continued.FlushReceipt.FirstClientSequence != 11 ||
		continued.FlushReceipt.RequestFirstClientSequence == nil || *continued.FlushReceipt.RequestFirstClientSequence != 10 {
		t.Fatal("STUDY_EVENT.ITEM2.PREFIX_SUFFIX_INVALID")
	}

	movedUUID := initialEvents[0]
	movedUUID.ClientSequence = 12
	idConflict, err := restartedRepository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("item2-moved-uuid"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    continued.Session.StateVersion,
		Events:                  []model.StudyEvent{movedUUID},
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.ITEM2.MOVED_UUID_FAILED", err))
	}
	if len(idConflict.Receipts) != 1 || idConflict.Receipts[0].Code != model.StudyEventReceiptCodeIDConflict ||
		idConflict.Receipts[0].EventIDBindingMatchesInput ||
		idConflict.Receipts[0].CanonicalProfileIngestionOrdinal != nil ||
		idConflict.FlushReceipt.ProfileIngestionHighWater != 6 {
		t.Fatal("STUDY_EVENT.ITEM2.MOVED_UUID_NOT_CONFLICT")
	}

	reprovisionedStreamID := uuid.New()
	reprovisionedEvent := initialEvents[0]
	reprovisionedEvent.ClientStreamID = reprovisionedStreamID
	reprovisioned, err := restartedRepository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("item2-reprovisioned"),
		ClientStreamID:          reprovisionedStreamID,
		ExpectedStateVersion:    idConflict.Session.StateVersion,
		Events:                  []model.StudyEvent{reprovisionedEvent},
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.ITEM2.REPROVISION_FAILED", err))
	}
	assertStudyEventSemanticDuplicate(t, reprovisioned.Receipts[0], presentedID, 1, "STUDY_EVENT.ITEM2.REPROVISION_INVALID")

	staleInput := StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("item2-stale"),
		ClientStreamID:          uuid.New(),
		ExpectedStateVersion:    reprovisioned.Session.StateVersion - 1,
		Events: []model.StudyEvent{
			newStudyEventTestEvent(fixture, item, uuid.Nil, 1, uuid.New(), model.StudyEventKindPresented, nil, nil),
		},
	}
	staleInput.Events[0].ClientStreamID = staleInput.ClientStreamID
	assertStudyEventSyncRejectedWithoutWrites(t, ctx, pool, restartedRepository, staleInput, func(*StudyEventSyncInput) {}, model.StudyErrorCodeStateStale, "STATE_STALE")

	gapInput := staleInput
	gapInput.SyncRequestID = uuid.New()
	gapInput.NormalizedRequestSHA256 = studyEventTestHash("item2-gap")
	gapInput.ClientStreamID = uuid.New()
	gapInput.ExpectedStateVersion = reprovisioned.Session.StateVersion
	gapInput.Events[0].ID = uuid.New()
	gapInput.Events[0].ClientSequence = 2
	gapInput.Events[0].ClientStreamID = gapInput.ClientStreamID
	assertStudyEventSyncRejectedWithoutWrites(t, ctx, pool, restartedRepository, gapInput, func(*StudyEventSyncInput) {}, model.StudyErrorCodeStreamSequenceGap, "STREAM_GAP")

	contentMismatchEvent := newStudyEventTestEvent(
		fixture,
		item,
		streamID,
		13,
		uuid.New(),
		model.StudyEventKindRated,
		&ratingGood,
		nil,
	)
	contentMismatchEvent.ContentSHA256 = studyEventTestHash("unissued-content")
	contentMismatch, err := restartedRepository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("item2-content-mismatch"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    reprovisioned.Session.StateVersion,
		Events:                  []model.StudyEvent{contentMismatchEvent},
	})
	if err != nil || len(contentMismatch.Receipts) != 1 ||
		contentMismatch.Receipts[0].Disposition != model.StudyEventReceiptDispositionTerminalRejected ||
		contentMismatch.Receipts[0].Code != model.StudyEventReceiptCodeContentRevisionMismatch ||
		contentMismatch.Receipts[0].CanonicalProfileIngestionOrdinal != nil ||
		contentMismatch.FlushReceipt.ProfileIngestionHighWater != 6 {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.ITEM2.CONTENT_MISMATCH_INVALID", err))
	}

	lateReplay, err := restartedRepository.AdjudicateStudyEvents(ctx, initialInput)
	if err != nil || !lateReplay.Replayed || !reflect.DeepEqual(lateReplay.FlushReceipt, initial.FlushReceipt) ||
		lateReplay.Session.StateVersion != contentMismatch.Session.StateVersion ||
		lateReplay.Session.StateVersion <= initial.Session.StateVersion {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.ITEM2.LATE_REPLAY_INVALID", err))
	}
	for index := range initial.Receipts {
		if !reflect.DeepEqual(lateReplay.Receipts[index], initial.Receipts[index]) {
			t.Fatal("STUDY_EVENT.ITEM2.LATE_REPLAY_RECEIPT_CHANGED")
		}
	}

	testStudyEventTerminalUUIDsFreshlyReadjudicate(t, ctx, restartedRepository)
	testStudyEventIDConflictCollateralFreshlyReadjudicates(t, ctx, restartedRepository)
	testStudyEventCrossSessionReplayRejected(t, ctx, pool, restartedRepository, fixture, initialEvents[0])
	testStudyEventEmptyFlushReplay(t, ctx, restartedRepository, fixture, contentMismatch.Session.StateVersion)
	testStudyEventCompletedCorrections(t, ctx, restartedRepository, fixture, contentMismatch, streamID)
	testStudyEventDeferredCommitFailureRollsBack(t, ctx, pool, restartedRepository)
}

// TestPlanLexinetStep2Verify1Item3ConcurrentlyIngestFromTwoClientInstancesAndProve
// proves profile ordinals remain unique and gap-free across independent
// streams and sessions for the same learner.
func TestPlanLexinetStep2Verify1Item3ConcurrentlyIngestFromTwoClientInstancesAndProve(t *testing.T) {
	harness := startStudyTestPostgres(t)
	harness.migrateUp(t)
	pool := harness.openPool(t)
	repository := NewRepository(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	profileRef := "item3-" + stringsForStudyFixture(uuid.NewString())
	fixtures := []studyEventTestFixture{
		newStudyEventTestFixture(t, ctx, repository, "item3-a", profileRef),
		newStudyEventTestFixture(t, ctx, repository, "item3-b", profileRef),
	}

	type outcome struct {
		result *StudyEventSyncResult
		err    error
	}
	firstAtCommit := make(chan struct{})
	releaseFirst := make(chan struct{})
	var signalOnce sync.Once
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer release()
	repository.studyEventBeforeCommit = func(hookContext context.Context, _ pgx.Tx) error {
		signalOnce.Do(func() { close(firstAtCommit) })
		select {
		case <-releaseFirst:
			return nil
		case <-hookContext.Done():
			return hookContext.Err()
		}
	}
	secondRepository, secondApplicationName, secondPool := newAdversarialWorkerRepository(t, ctx, pool, "study_event_item3")
	workerContext, cancelWorkers := context.WithCancel(ctx)
	firstDone := make(chan struct{})
	secondDone := make(chan struct{})
	secondStarted := false
	defer func() {
		cancelWorkers()
		release()
		waitForStudyEventWorkerCleanup(t, firstDone, "STUDY_EVENT.ITEM3.FIRST_CLEANUP_TIMEOUT")
		if secondStarted {
			waitForStudyEventWorkerCleanup(t, secondDone, "STUDY_EVENT.ITEM3.SECOND_CLEANUP_TIMEOUT")
		}
		repository.studyEventBeforeCommit = nil
		secondPool.Close()
	}()

	runSync := func(targetRepository *Repository, fixture studyEventTestFixture) outcome {
		streamID := uuid.New()
		result, err := targetRepository.AdjudicateStudyEvents(workerContext, StudyEventSyncInput{
			PageArtifact:            studyTestSessionPageArtifact(),
			Principal:               fixture.Principal,
			ProfileRef:              fixture.ProfileRef,
			SessionID:               fixture.Plan.Session.ID,
			SyncRequestID:           uuid.New(),
			NormalizedRequestSHA256: studyEventTestHash("item3-" + fixture.IdeaID.String()),
			ClientStreamID:          streamID,
			ExpectedStateVersion:    fixture.Plan.Session.StateVersion,
			Events: []model.StudyEvent{
				newStudyEventTestEvent(fixture, fixture.Plan.Items[0], streamID, 1, uuid.New(), model.StudyEventKindPresented, nil, nil),
			},
		})
		return outcome{result: result, err: err}
	}
	firstOutcome := make(chan outcome, 1)
	go func() {
		defer close(firstDone)
		firstOutcome <- runSync(repository, fixtures[0])
	}()
	select {
	case <-firstAtCommit:
	case <-ctx.Done():
		t.Fatal("STUDY_EVENT.ITEM3.FIRST_DID_NOT_REACH_COMMIT_BARRIER")
	}

	secondOutcome := make(chan outcome, 1)
	secondStarted = true
	go func() {
		defer close(secondDone)
		secondOutcome <- runSync(secondRepository, fixtures[1])
	}()
	waitContext, cancelWait := context.WithTimeout(ctx, 10*time.Second)
	if err := waitForAdversarialPostgresLock(waitContext, pool, secondApplicationName, secondDone); err != nil {
		cancelWait()
		release()
		select {
		case <-firstDone:
		case <-ctx.Done():
		}
		select {
		case <-secondDone:
		case <-ctx.Done():
		}
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.ITEM3.SECOND_NOT_LOCK_BLOCKED", err))
	}
	cancelWait()
	release()
	select {
	case <-firstDone:
	case <-ctx.Done():
		t.Fatal("STUDY_EVENT.ITEM3.FIRST_DID_NOT_FINISH")
	}
	select {
	case <-secondDone:
	case <-ctx.Done():
		t.Fatal("STUDY_EVENT.ITEM3.SECOND_DID_NOT_FINISH")
	}
	repository.studyEventBeforeCommit = nil

	ordinals := make([]int64, 0, len(fixtures))
	for _, result := range []outcome{<-firstOutcome, <-secondOutcome} {
		if result.err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_EVENT.ITEM3.CONCURRENT_SYNC_FAILED", result.err))
		}
		if result.result == nil || len(result.result.Receipts) != 1 ||
			result.result.Receipts[0].Disposition != model.StudyEventReceiptDispositionAccepted ||
			result.result.Receipts[0].CanonicalProfileIngestionOrdinal == nil {
			t.Fatal("STUDY_EVENT.ITEM3.CONCURRENT_RECEIPT_INVALID")
		}
		ordinals = append(ordinals, *result.result.Receipts[0].CanonicalProfileIngestionOrdinal)
	}
	sort.Slice(ordinals, func(left, right int) bool { return ordinals[left] < ordinals[right] })
	if len(ordinals) != 2 || ordinals[0] != 1 || ordinals[1] != 2 {
		t.Fatal("STUDY_EVENT.ITEM3.PROFILE_ORDINALS_NOT_GAP_FREE")
	}
	profile, err := repository.GetStudyLearnerProfile(ctx, profileRef)
	if err != nil || profile.ProfileIngestionHighWater != 2 {
		t.Fatal("STUDY_EVENT.ITEM3.PROFILE_HIGH_WATER_INVALID")
	}
	var acceptedCount, distinctOrdinalCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*), count(DISTINCT profile_ingestion_ordinal)
		FROM study_events
		WHERE learner_profile_id = $1`, profile.ID).Scan(&acceptedCount, &distinctOrdinalCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.ITEM3.EVENT_COUNT_FAILED", err))
	}
	if acceptedCount != 2 || distinctOrdinalCount != 2 {
		t.Fatal("STUDY_EVENT.ITEM3.DATABASE_ORDINAL_AUTHORITY_INVALID")
	}
	testStudyEventGlobalUUIDRace(t, ctx, pool, repository)
}

func newStudyEventTestFixture(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	label string,
	profileRef string,
) studyEventTestFixture {
	t.Helper()
	suffix := stringsForStudyFixture(uuid.NewString())
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{
		Slug: "study-event-" + label + "-" + suffix,
		Name: "Study event " + label + " " + suffix,
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.FIXTURE_TOPIC_CREATE_FAILED", err))
	}
	idea := mustCreateStudyIdea(t, ctx, repository, topic.Slug, "event-"+label)
	if profileRef == "" {
		profileRef = "study-event-" + label + "-" + suffix
	}
	principal := model.OperatorStudyPrincipalID()
	profile, err := repository.GetStudyLearnerProfile(ctx, profileRef)
	if err != nil && !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeProfileNotFound}) {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.FIXTURE_PROFILE_READ_FAILED", err))
	}
	highWater := int64(0)
	if profile != nil {
		highWater = profile.ProfileIngestionHighWater
	}
	planInput := testStudyPlan(uuid.New(), uuid.New(), principal, idea.ID, highWater, "event-"+label+"-"+suffix, nil)
	var plan *StudyPlanInsertResult
	scope := mustStudyScope(t, topic.ID, topic.Slug)
	err = repository.WithStudyScopeTransaction(
		ctx,
		StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope},
		func(store StudyStoreTransaction) error {
			if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
				return listErr
			}
			var insertErr error
			plan, insertErr = store.InsertPlanOnce(ctx, planInput)
			return insertErr
		},
	)
	if err != nil || plan == nil || len(plan.Items) != 1 {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.FIXTURE_PLAN_CREATE_FAILED", err))
	}
	return studyEventTestFixture{
		ProfileRef: profileRef,
		Principal:  principal,
		Scope:      scope,
		IdeaID:     idea.ID,
		Plan:       plan,
	}
}

func newStudyEventTestEvent(
	fixture studyEventTestFixture,
	item model.StudySessionItem,
	streamID uuid.UUID,
	sequence int64,
	eventID uuid.UUID,
	kind model.StudyEventKind,
	rating *model.StudyRating,
	ratingEventID *uuid.UUID,
) model.StudyEvent {
	return model.StudyEvent{
		ID:                 eventID,
		ClientSequence:     sequence,
		ProfileRef:         fixture.ProfileRef,
		StudySessionItemID: item.ID,
		CardIdentity:       item.CardIdentity,
		ContentSHA256:      item.SourceContentSHA256,
		Kind:               kind,
		Rating:             cloneStudyRatingPointer(rating),
		RatingEventID:      cloneStudyUUIDPointer(ratingEventID),
		ClientClockQuality: model.StudyClientClockQualityUnknown,
		ClientStreamID:     streamID,
	}
}

func cloneStudyRatingPointer(value *model.StudyRating) *model.StudyRating {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	return &copyOfValue
}

func studyEventTestHash(label string) string {
	return model.SHA256Hex([]byte("study-event-test:" + label))
}

func assertStudyEventAcceptedRange(t *testing.T, result *StudyEventSyncResult, first, last, firstOrdinal int64) {
	t.Helper()
	if result == nil || len(result.Receipts) != int(last-first+1) {
		t.Fatal("STUDY_EVENT.ACCEPTED_RANGE_LENGTH_INVALID")
	}
	for index := range result.Receipts {
		receipt := result.Receipts[index]
		expectedSequence := first + int64(index)
		expectedOrdinal := firstOrdinal + int64(index)
		if receipt.ClientSequence != expectedSequence ||
			receipt.Disposition != model.StudyEventReceiptDispositionAccepted ||
			receipt.Code != model.StudyEventReceiptCodeAccepted ||
			receipt.CanonicalStudyEventID == nil || *receipt.CanonicalStudyEventID != receipt.EventID ||
			receipt.CanonicalProfileIngestionOrdinal == nil || *receipt.CanonicalProfileIngestionOrdinal != expectedOrdinal ||
			receipt.OriginatingStudyFlushReceiptID != result.FlushReceipt.ID {
			t.Fatal("STUDY_EVENT.ACCEPTED_RANGE_RECEIPT_INVALID")
		}
	}
}

func assertStudyEventSemanticDuplicate(
	t *testing.T,
	receipt model.StudyEventReceipt,
	canonicalID uuid.UUID,
	canonicalOrdinal int64,
	failure string,
) {
	t.Helper()
	if receipt.Disposition != model.StudyEventReceiptDispositionSemanticDuplicate ||
		receipt.Code != model.StudyEventReceiptCodeSemanticDuplicate ||
		receipt.CanonicalStudyEventID == nil || *receipt.CanonicalStudyEventID != canonicalID ||
		receipt.CanonicalProfileIngestionOrdinal == nil || *receipt.CanonicalProfileIngestionOrdinal != canonicalOrdinal {
		t.Fatal(failure)
	}
}

func assertStudyEventSyncRejectedWithoutWrites(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	base StudyEventSyncInput,
	mutate func(*StudyEventSyncInput),
	expectedCode model.StudyErrorCode,
	label string,
) {
	t.Helper()
	before := readStudyEventPersistenceCounts(t, ctx, pool)
	mutate(&base)
	_, err := repository.AdjudicateStudyEvents(ctx, base)
	if !errors.Is(err, &model.StudyError{Code: expectedCode}) {
		t.Fatal("STUDY_EVENT." + label + ".WRONG_ERROR")
	}
	after := readStudyEventPersistenceCounts(t, ctx, pool)
	if before != after {
		t.Fatal("STUDY_EVENT." + label + ".ERROR_PERSISTED_STATE")
	}
}

func testStudyEventTerminalUUIDsFreshlyReadjudicate(t *testing.T, ctx context.Context, repository *Repository) {
	t.Helper()
	fixture := newStudyEventTestFixture(t, ctx, repository, "terminal-readjudicate", "")
	item := fixture.Plan.Items[0]
	failedStreamID := uuid.New()
	presentedID := uuid.New()
	ratedID := uuid.New()
	rating := model.StudyRatingEasy
	failedEvents := []model.StudyEvent{
		newStudyEventTestEvent(fixture, item, failedStreamID, 1, presentedID, model.StudyEventKindPresented, nil, nil),
		newStudyEventTestEvent(fixture, item, failedStreamID, 2, ratedID, model.StudyEventKindRated, &rating, nil),
	}
	failed, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("terminal-readjudicate-failed"),
		ClientStreamID:          failedStreamID,
		ExpectedStateVersion:    fixture.Plan.Session.StateVersion,
		Events:                  failedEvents,
	})
	if err != nil || len(failed.Receipts) != 2 {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.TERMINAL_READJUDICATE.SETUP_FAILED", err))
	}
	for index := range failed.Receipts {
		if failed.Receipts[index].Disposition != model.StudyEventReceiptDispositionTerminalRejected ||
			failed.Receipts[index].Code != model.StudyEventReceiptCodeTransitionInvalid ||
			failed.Receipts[index].TerminalFailureClientSequence == nil ||
			*failed.Receipts[index].TerminalFailureClientSequence != 2 {
			t.Fatal("STUDY_EVENT.TERMINAL_READJUDICATE.SETUP_RECEIPT_INVALID")
		}
	}

	presentedStreamID := uuid.New()
	presented := failedEvents[0]
	presented.ClientStreamID = presentedStreamID
	presentedResult, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("terminal-readjudicate-collateral"),
		ClientStreamID:          presentedStreamID,
		ExpectedStateVersion:    failed.Session.StateVersion,
		Events:                  []model.StudyEvent{presented},
	})
	if err != nil || presentedResult.Receipts[0].Disposition != model.StudyEventReceiptDispositionAccepted {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.TERMINAL_READJUDICATE.COLLATERAL_NOT_FRESH", err))
	}

	ratingStreamID := uuid.New()
	revealedID := uuid.New()
	revealed := newStudyEventTestEvent(fixture, item, ratingStreamID, 1, revealedID, model.StudyEventKindRevealed, nil, nil)
	rated := failedEvents[1]
	rated.ClientStreamID = ratingStreamID
	ratedResult, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("terminal-readjudicate-cause"),
		ClientStreamID:          ratingStreamID,
		ExpectedStateVersion:    presentedResult.Session.StateVersion,
		Events:                  []model.StudyEvent{revealed, rated},
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.TERMINAL_READJUDICATE.CAUSE_FAILED", err))
	}
	assertStudyEventAcceptedRange(t, ratedResult, 1, 2, 2)
	if ratedResult.Receipts[1].EventID != ratedID || ratedResult.Receipts[1].Code == model.StudyEventReceiptCodeTransitionInvalid {
		t.Fatal("STUDY_EVENT.TERMINAL_READJUDICATE.COPIED_PRIOR_CODE")
	}
}

func testStudyEventIDConflictCollateralFreshlyReadjudicates(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
) {
	t.Helper()
	fixture := newStudyEventTestFixture(t, ctx, repository, "id-conflict-collateral", "")
	item := fixture.Plan.Items[0]
	failedStreamID := uuid.New()
	sharedID := uuid.New()
	failedEvents := []model.StudyEvent{
		newStudyEventTestEvent(fixture, item, failedStreamID, 1, sharedID, model.StudyEventKindPresented, nil, nil),
		newStudyEventTestEvent(fixture, item, failedStreamID, 2, sharedID, model.StudyEventKindPresented, nil, nil),
		newStudyEventTestEvent(fixture, item, failedStreamID, 3, uuid.New(), model.StudyEventKindRevealed, nil, nil),
	}
	failed, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("id-conflict-collateral-failed"),
		ClientStreamID:          failedStreamID,
		ExpectedStateVersion:    fixture.Plan.Session.StateVersion,
		Events:                  failedEvents,
	})
	if err != nil || len(failed.Receipts) != 3 {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.ID_CONFLICT_COLLATERAL.SETUP_FAILED", err))
	}
	for index := range failed.Receipts {
		receipt := failed.Receipts[index]
		if receipt.Disposition != model.StudyEventReceiptDispositionTerminalRejected ||
			receipt.Code != model.StudyEventReceiptCodeIDConflict ||
			receipt.TerminalFailureClientSequence == nil || *receipt.TerminalFailureClientSequence != 2 {
			t.Fatal("STUDY_EVENT.ID_CONFLICT_COLLATERAL.GROUP_INVALID")
		}
	}
	if !failed.Receipts[0].EventIDBindingMatchesInput || failed.Receipts[1].EventIDBindingMatchesInput ||
		!failed.Receipts[2].EventIDBindingMatchesInput {
		t.Fatal("STUDY_EVENT.ID_CONFLICT_COLLATERAL.BINDING_TRUTH_INVALID")
	}

	reprovisionedStreamID := uuid.New()
	readjudicated := failedEvents[0]
	readjudicated.ClientStreamID = reprovisionedStreamID
	result, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("id-conflict-collateral-readjudicated"),
		ClientStreamID:          reprovisionedStreamID,
		ExpectedStateVersion:    failed.Session.StateVersion,
		Events:                  []model.StudyEvent{readjudicated},
	})
	if err != nil || len(result.Receipts) != 1 ||
		result.Receipts[0].Disposition != model.StudyEventReceiptDispositionAccepted ||
		result.Receipts[0].Code != model.StudyEventReceiptCodeAccepted ||
		result.Receipts[0].CanonicalProfileIngestionOrdinal == nil ||
		*result.Receipts[0].CanonicalProfileIngestionOrdinal != 1 {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.ID_CONFLICT_COLLATERAL.NOT_FRESH", err))
	}
}

func testStudyEventCompletedCorrections(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	fixture studyEventTestFixture,
	barrier *StudyEventSyncResult,
	streamID uuid.UUID,
) {
	t.Helper()
	predecessorID := fixture.Plan.Session.ID
	successorInput := StudySuccessorPlanInsert{
		Plan: testStudyPlan(
			uuid.New(),
			uuid.New(),
			fixture.Principal,
			fixture.IdeaID,
			barrier.FlushReceipt.ProfileIngestionHighWater,
			"event-completed-correction",
			&predecessorID,
		),
		PredecessorFlushReceiptID:     barrier.FlushReceipt.ID,
		PredecessorTerminalReasonCode: "cycle_exhausted",
	}
	var successor *StudyPlanInsertResult
	err := repository.WithStudyScopeTransaction(
		ctx,
		StudyScopeTransactionInput{ProfileRef: fixture.ProfileRef, Scope: fixture.Scope},
		func(store StudyStoreTransaction) error {
			if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
				return listErr
			}
			var insertErr error
			successor, insertErr = store.InsertSuccessorPlanOnce(ctx, successorInput)
			return insertErr
		},
	)
	if err != nil || successor == nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.COMPLETED.SUCCESSOR_FAILED", err))
	}
	predecessor, err := repository.GetStudySession(ctx, fixture.ProfileRef, predecessorID)
	if err != nil || predecessor.Lifecycle != model.StudySessionLifecycleCompleted ||
		predecessor.TerminalProfileIngestionHighWater == nil ||
		*predecessor.TerminalProfileIngestionHighWater != barrier.FlushReceipt.ProfileIngestionHighWater ||
		predecessor.SuccessorStudySessionID == nil || *predecessor.SuccessorStudySessionID != successor.Session.ID {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.COMPLETED.PREDECESSOR_INVALID", err))
	}
	terminalHighWater := *predecessor.TerminalProfileIngestionHighWater
	rating := model.StudyRatingAgain
	ratedID := uuid.New()
	correctionEvents := []model.StudyEvent{
		newStudyEventTestEvent(fixture, fixture.Plan.Items[0], streamID, 14, ratedID, model.StudyEventKindRated, &rating, nil),
		newStudyEventTestEvent(fixture, fixture.Plan.Items[0], streamID, 15, uuid.New(), model.StudyEventKindRatingVoided, nil, &ratedID),
	}
	correction, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               predecessorID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("completed-correction"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    predecessor.StateVersion,
		Events:                  correctionEvents,
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.COMPLETED.CORRECTION_FAILED", err))
	}
	assertStudyEventAcceptedRange(t, correction, 14, 15, terminalHighWater+1)
	if correction.Session.Lifecycle != model.StudySessionLifecycleCompleted ||
		correction.Session.TerminalProfileIngestionHighWater == nil ||
		*correction.Session.TerminalProfileIngestionHighWater != terminalHighWater ||
		correction.Session.SuccessorStudySessionID == nil ||
		*correction.Session.SuccessorStudySessionID != successor.Session.ID {
		t.Fatal("STUDY_EVENT.COMPLETED.CORRECTION_CHANGED_TERMINAL_LINKAGE")
	}

	presentation := newStudyEventTestEvent(
		fixture,
		fixture.Plan.Items[0],
		streamID,
		16,
		uuid.New(),
		model.StudyEventKindPresented,
		nil,
		nil,
	)
	rejected, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               predecessorID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("completed-presentation"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    correction.Session.StateVersion,
		Events:                  []model.StudyEvent{presentation},
	})
	if err != nil || len(rejected.Receipts) != 1 ||
		rejected.Receipts[0].Disposition != model.StudyEventReceiptDispositionTerminalRejected ||
		rejected.Receipts[0].Code != model.StudyEventReceiptCodeTransitionInvalid ||
		rejected.FlushReceipt.ProfileIngestionHighWater != correction.FlushReceipt.ProfileIngestionHighWater ||
		rejected.Session.TerminalProfileIngestionHighWater == nil ||
		*rejected.Session.TerminalProfileIngestionHighWater != terminalHighWater ||
		rejected.Session.SuccessorStudySessionID == nil ||
		*rejected.Session.SuccessorStudySessionID != successor.Session.ID {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.COMPLETED.PRESENTATION_NOT_REJECTED", err))
	}
	persistedSuccessor, err := repository.GetStudySession(ctx, fixture.ProfileRef, successor.Session.ID)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.COMPLETED.SUCCESSOR_READ_FAILED", err))
	}
	persistedSuccessorItems, err := repository.ListStudySessionItems(ctx, fixture.ProfileRef, successor.Session.ID)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.COMPLETED.SUCCESSOR_ITEMS_READ_FAILED", err))
	}
	if persistedSuccessor.ID != successor.Session.ID ||
		persistedSuccessor.LearnerProfileID != successor.Session.LearnerProfileID ||
		!reflect.DeepEqual(persistedSuccessor.Scope, successor.Session.Scope) ||
		persistedSuccessor.Lifecycle != successor.Session.Lifecycle ||
		persistedSuccessor.CorpusSHA256 != successor.Session.CorpusSHA256 ||
		!reflect.DeepEqual(persistedSuccessor.Scheduler, successor.Session.Scheduler) ||
		persistedSuccessor.Scheduler == nil ||
		persistedSuccessor.Scheduler.InputProfileIngestionHighWater != barrier.FlushReceipt.ProfileIngestionHighWater ||
		!reflect.DeepEqual(persistedSuccessor.PredecessorStudySessionID, successor.Session.PredecessorStudySessionID) ||
		persistedSuccessor.StateVersion != successor.Session.StateVersion ||
		!persistedSuccessor.CreatedAt.Equal(successor.Session.CreatedAt) ||
		!persistedSuccessor.UpdatedAt.Equal(successor.Session.UpdatedAt) ||
		!reflect.DeepEqual(persistedSuccessorItems, successor.Items) {
		t.Fatal("STUDY_EVENT.COMPLETED.CORRECTION_CHANGED_SUCCESSOR_PLAN")
	}
}

func testStudyEventDeferredCommitFailureRollsBack(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	fixture := newStudyEventTestFixture(t, ctx, repository, "deferred-failure", "")
	streamID := uuid.New()
	syncID := uuid.New()
	input := StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           syncID,
		NormalizedRequestSHA256: studyEventTestHash("deferred-failure"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    fixture.Plan.Session.StateVersion,
		Events: []model.StudyEvent{
			newStudyEventTestEvent(fixture, fixture.Plan.Items[0], streamID, 1, uuid.New(), model.StudyEventKindPresented, nil, nil),
		},
	}
	countsBefore := readStudyEventPersistenceCounts(t, ctx, pool)
	defer func() { repository.studyEventBeforeCommit = nil }()
	repository.studyEventBeforeCommit = func(context.Context, pgx.Tx) error {
		return &pgconn.PgError{Code: "40001"}
	}
	retryableResult, retryableErr := repository.AdjudicateStudyEvents(ctx, input)
	var retryableStudyError *model.StudyError
	if retryableResult != nil || !errors.As(retryableErr, &retryableStudyError) ||
		retryableStudyError.Code != model.StudyErrorCodeServiceUnavailable ||
		!retryableStudyError.Retryable ||
		retryableStudyError.Context["operation"] != "study_event_before_commit" ||
		readStudyEventPersistenceCounts(t, ctx, pool) != countsBefore {
		t.Fatal("STUDY_EVENT.RETRYABLE_FAILURE.PERSISTED_STATE")
	}

	repository.studyEventBeforeCommit = func(hookContext context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(hookContext, `
			UPDATE study_client_streams
			SET adjudicated_through_client_sequence = adjudicated_through_client_sequence + 1
			WHERE client_stream_id = $1`, streamID)
		return err
	}
	result, err := repository.AdjudicateStudyEvents(ctx, input)
	repository.studyEventBeforeCommit = nil
	var studyError *model.StudyError
	if result != nil || !errors.As(err, &studyError) ||
		studyError.Code != model.StudyErrorCodeInternal ||
		studyError.Context["operation"] != "study_event_sync_commit" {
		t.Fatal("STUDY_EVENT.DEFERRED_FAILURE.NOT_STRUCTURED")
	}
	countsAfter := readStudyEventPersistenceCounts(t, ctx, pool)
	if countsBefore != countsAfter {
		t.Fatal("STUDY_EVENT.DEFERRED_FAILURE.PERSISTED_PARTIAL_STATE")
	}
	session, readErr := repository.GetStudySession(ctx, fixture.ProfileRef, fixture.Plan.Session.ID)
	if readErr != nil || session.StateVersion != fixture.Plan.Session.StateVersion {
		t.Fatal("STUDY_EVENT.DEFERRED_FAILURE.SESSION_ADVANCED")
	}
	retried, retryErr := repository.AdjudicateStudyEvents(ctx, input)
	if retryErr != nil || retried == nil || len(retried.Receipts) != 1 ||
		retried.Receipts[0].Disposition != model.StudyEventReceiptDispositionAccepted ||
		retried.Receipts[0].CanonicalProfileIngestionOrdinal == nil ||
		*retried.Receipts[0].CanonicalProfileIngestionOrdinal != 1 {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.DEFERRED_FAILURE.RETRY_FAILED", retryErr))
	}
}

type studyEventPersistenceCounts struct {
	Streams  int
	Bindings int
	Events   int
	Receipts int
	Flushes  int
}

func readStudyEventPersistenceCounts(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
) studyEventPersistenceCounts {
	t.Helper()
	var counts studyEventPersistenceCounts
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM study_client_streams),
			(SELECT count(*) FROM study_event_id_bindings),
			(SELECT count(*) FROM study_events),
			(SELECT count(*) FROM study_event_receipts),
			(SELECT count(*) FROM study_flush_receipts)`).Scan(
		&counts.Streams,
		&counts.Bindings,
		&counts.Events,
		&counts.Receipts,
		&counts.Flushes,
	); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.COUNTS_READ_FAILED", err))
	}
	return counts
}

func testStudyEventGlobalUUIDRace(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	fixtures := []studyEventTestFixture{
		newStudyEventTestFixture(t, ctx, repository, "uuid-race-a", ""),
		newStudyEventTestFixture(t, ctx, repository, "uuid-race-b", ""),
	}
	sharedIDs := [2]uuid.UUID{uuid.New(), uuid.New()}
	type outcome struct {
		result *StudyEventSyncResult
		err    error
	}
	firstAtCommit := make(chan struct{})
	releaseFirst := make(chan struct{})
	var signalOnce sync.Once
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer release()
	repository.studyEventBeforeCommit = func(hookContext context.Context, _ pgx.Tx) error {
		signalOnce.Do(func() { close(firstAtCommit) })
		select {
		case <-releaseFirst:
			return nil
		case <-hookContext.Done():
			return hookContext.Err()
		}
	}
	secondRepository, secondApplicationName, secondPool := newAdversarialWorkerRepository(t, ctx, pool, "study_event_uuid_race")
	workerContext, cancelWorkers := context.WithCancel(ctx)
	firstDone := make(chan struct{})
	secondDone := make(chan struct{})
	secondStarted := false
	defer func() {
		cancelWorkers()
		release()
		waitForStudyEventWorkerCleanup(t, firstDone, "STUDY_EVENT.UUID_RACE.FIRST_CLEANUP_TIMEOUT")
		if secondStarted {
			waitForStudyEventWorkerCleanup(t, secondDone, "STUDY_EVENT.UUID_RACE.SECOND_CLEANUP_TIMEOUT")
		}
		repository.studyEventBeforeCommit = nil
		secondPool.Close()
	}()

	runSync := func(target *Repository, fixture studyEventTestFixture, eventIDs [2]uuid.UUID) outcome {
		streamID := uuid.New()
		result, err := target.AdjudicateStudyEvents(workerContext, StudyEventSyncInput{
			PageArtifact:            studyTestSessionPageArtifact(),
			Principal:               fixture.Principal,
			ProfileRef:              fixture.ProfileRef,
			SessionID:               fixture.Plan.Session.ID,
			SyncRequestID:           uuid.New(),
			NormalizedRequestSHA256: studyEventTestHash("uuid-race-" + fixture.ProfileRef),
			ClientStreamID:          streamID,
			ExpectedStateVersion:    fixture.Plan.Session.StateVersion,
			Events: []model.StudyEvent{
				newStudyEventTestEvent(fixture, fixture.Plan.Items[0], streamID, 1, eventIDs[0], model.StudyEventKindPresented, nil, nil),
				newStudyEventTestEvent(fixture, fixture.Plan.Items[0], streamID, 2, eventIDs[1], model.StudyEventKindRevealed, nil, nil),
			},
		})
		return outcome{result: result, err: err}
	}
	firstOutcome := make(chan outcome, 1)
	go func() {
		defer close(firstDone)
		firstOutcome <- runSync(repository, fixtures[0], sharedIDs)
	}()
	select {
	case <-firstAtCommit:
	case <-ctx.Done():
		t.Fatal("STUDY_EVENT.UUID_RACE.FIRST_DID_NOT_REACH_COMMIT_BARRIER")
	}
	secondOutcome := make(chan outcome, 1)
	secondStarted = true
	go func() {
		defer close(secondDone)
		secondOutcome <- runSync(secondRepository, fixtures[1], [2]uuid.UUID{sharedIDs[1], sharedIDs[0]})
	}()
	waitContext, cancelWait := context.WithTimeout(ctx, 10*time.Second)
	if err := waitForAdversarialPostgresLock(waitContext, pool, secondApplicationName, secondDone); err != nil {
		cancelWait()
		release()
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.UUID_RACE.SECOND_NOT_LOCK_BLOCKED", err))
	}
	cancelWait()
	release()
	select {
	case <-firstDone:
	case <-ctx.Done():
		t.Fatal("STUDY_EVENT.UUID_RACE.FIRST_DID_NOT_FINISH")
	}
	select {
	case <-secondDone:
	case <-ctx.Done():
		t.Fatal("STUDY_EVENT.UUID_RACE.SECOND_DID_NOT_FINISH")
	}
	repository.studyEventBeforeCommit = nil
	winner := <-firstOutcome
	loser := <-secondOutcome
	if winner.err != nil || winner.result == nil || len(winner.result.Receipts) != 2 {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.UUID_RACE.WINNER_FAILED", winner.err))
	}
	if loser.err != nil || loser.result == nil || len(loser.result.Receipts) != 2 {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.UUID_RACE.LOSER_FAILED", loser.err))
	}
	for index := range winner.result.Receipts {
		if winner.result.Receipts[index].Disposition != model.StudyEventReceiptDispositionAccepted ||
			winner.result.Receipts[index].Code != model.StudyEventReceiptCodeAccepted ||
			!winner.result.Receipts[index].EventIDBindingMatchesInput {
			t.Fatal("STUDY_EVENT.UUID_RACE.WINNER_RECEIPT_INVALID")
		}
		if loser.result.Receipts[index].Disposition != model.StudyEventReceiptDispositionTerminalRejected ||
			loser.result.Receipts[index].Code != model.StudyEventReceiptCodeIDConflict ||
			loser.result.Receipts[index].EventIDBindingMatchesInput {
			t.Fatal("STUDY_EVENT.UUID_RACE.LOSER_RECEIPT_INVALID")
		}
	}
	var bindingCount, bindingProfileCount int
	var allOwnedByWinner bool
	if err := pool.QueryRow(ctx, `
		SELECT count(*), count(DISTINCT learner_profile_id), bool_and(learner_profile_id = $2)
		FROM study_event_id_bindings
		WHERE client_event_id = ANY($1::uuid[])`, sharedIDs[:], winner.result.Session.LearnerProfileID).Scan(
		&bindingCount,
		&bindingProfileCount,
		&allOwnedByWinner,
	); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.UUID_RACE.BINDING_READ_FAILED", err))
	}
	if bindingCount != 2 || bindingProfileCount != 1 || !allOwnedByWinner {
		t.Fatal("STUDY_EVENT.UUID_RACE.AUTHORITY_NOT_UNIQUE")
	}
	winnerProfile, winnerProfileErr := repository.GetStudyLearnerProfile(ctx, fixtures[0].ProfileRef)
	loserProfile, loserProfileErr := repository.GetStudyLearnerProfile(ctx, fixtures[1].ProfileRef)
	if winnerProfileErr != nil || loserProfileErr != nil ||
		winnerProfile.ProfileIngestionHighWater != 2 || loserProfile.ProfileIngestionHighWater != 0 {
		t.Fatal("STUDY_EVENT.UUID_RACE.ORDINAL_AUTHORITY_INVALID")
	}
}

func testStudyEventCrossSessionReplayRejected(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	first studyEventTestFixture,
	priorEvent model.StudyEvent,
) {
	t.Helper()
	second := newStudyEventTestFixture(t, ctx, repository, "cross-session", first.ProfileRef)
	profile, err := repository.GetStudyLearnerProfile(ctx, first.ProfileRef)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.CROSS_SESSION.PROFILE_READ_FAILED", err))
	}
	streamID := priorEvent.ClientStreamID
	input := StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               second.Principal,
		ProfileRef:              second.ProfileRef,
		SessionID:               second.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("cross-session-replay"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    second.Plan.Session.StateVersion,
		Events:                  []model.StudyEvent{priorEvent},
	}
	assertStudyEventSyncRejectedWithoutWrites(t, ctx, pool, repository, input, func(*StudyEventSyncInput) {}, model.StudyErrorCodeIdempotencyConflict, "CROSS_SESSION_REPLAY")
	refreshed, err := repository.GetStudyLearnerProfile(ctx, first.ProfileRef)
	if err != nil || refreshed.ProfileIngestionHighWater != profile.ProfileIngestionHighWater {
		t.Fatal("STUDY_EVENT.CROSS_SESSION.PROFILE_CHANGED")
	}
}

func testStudyEventEmptyFlushReplay(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	fixture studyEventTestFixture,
	expectedVersion int64,
) {
	t.Helper()
	streamID := uuid.New()
	input := StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("empty-flush"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    expectedVersion,
	}
	first, err := repository.AdjudicateStudyEvents(ctx, input)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.EMPTY_FLUSH.CREATE_FAILED", err))
	}
	if len(first.Receipts) != 0 || first.FlushReceipt.RequestFirstClientSequence != nil ||
		first.FlushReceipt.RequestLastClientSequence != nil || first.FlushReceipt.FirstClientSequence != nil ||
		first.FlushReceipt.LastClientSequence != nil || first.Session.StateVersion != expectedVersion {
		t.Fatal("STUDY_EVENT.EMPTY_FLUSH.INVALID")
	}
	replayed, err := repository.AdjudicateStudyEvents(ctx, input)
	if err != nil || !replayed.Replayed || replayed.FlushReceipt.ID != first.FlushReceipt.ID ||
		replayed.Session.StateVersion != expectedVersion {
		t.Fatal(studyTestDatabaseError("STUDY_EVENT.EMPTY_FLUSH.REPLAY_FAILED", err))
	}
}

func waitForStudyEventWorkerCleanup(t *testing.T, done <-chan struct{}, failure string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Error(failure)
	}
}
