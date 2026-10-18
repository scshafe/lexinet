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
	"github.com/jackc/pgx/v5/pgxpool"

	"lexinet/internal/model"
	"lexinet/internal/study"
)

const step4VerificationTimeout = 2 * time.Minute

type step4VerificationSubject struct {
	context      context.Context
	pool         *pgxpool.Pool
	repository   *Repository
	materializer *study.CycleMaterializer
}

type step4MaterializedFixture struct {
	profileRef string
	scope      model.StudyNormalizedScope
	idea       *model.Idea
	request    StudyCycleMaterializationRequest
	root       *study.CycleMaterializationResult
}

type step4SyncOutcome struct {
	result *StudyEventSyncResult
	err    error
}

func newStep4VerificationSubject(t *testing.T) step4VerificationSubject {
	t.Helper()
	harness := startStudyTestPostgres(t)
	harness.migrateUp(t)
	pool := harness.openPool(t)
	operationContext, cancel := context.WithTimeout(context.Background(), step4VerificationTimeout)
	t.Cleanup(cancel)
	return step4VerificationSubject{
		context:      operationContext,
		pool:         pool,
		repository:   NewRepository(pool),
		materializer: newStudyMaterializerIntegrationSubject(t),
	}
}

func newStep4MaterializedFixture(
	t *testing.T,
	subject step4VerificationSubject,
	label string,
) step4MaterializedFixture {
	t.Helper()
	suffix := stringsForStudyFixture(uuid.NewString())
	topic, err := subject.repository.CreateTopic(subject.context, model.CreateTopicInput{
		Slug: "step4-verify-" + label + "-" + suffix,
		Name: "Step 4 verify " + label + " " + suffix,
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.MATERIALIZED.TOPIC_CREATE_FAILED", err))
	}
	idea := mustCreateStudyIdea(t, subject.context, subject.repository, topic.Slug, "step4-verify-"+label)
	fixture := step4MaterializedFixture{
		profileRef: "step4-verify-" + label + "-" + suffix,
		scope:      mustStudyScope(t, topic.ID, topic.Slug),
		idea:       idea,
	}
	fixture.request = newStudyMaterializationRequest(
		fixture.profileRef,
		fixture.scope,
		1,
		"step4-verify-"+label+"-"+suffix,
		nil,
		uuid.Nil,
	)
	fixture.root, err = subject.repository.MaterializeStudyCycle(
		subject.context,
		subject.materializer,
		fixture.request,
	)
	if err != nil || fixture.root == nil || !fixture.root.Persistence.Created || len(fixture.root.Persistence.Items) != 1 {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.MATERIALIZED.ROOT_CREATE_FAILED", err))
	}
	return fixture
}

func step4VerificationEventSyncInput(
	fixture studyEventTestFixture,
	streamID uuid.UUID,
	expectedStateVersion int64,
	label string,
	events []model.StudyEvent,
) StudyEventSyncInput {
	return StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Plan.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyEventTestHash("step4-verify-" + label),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    expectedStateVersion,
		Events:                  events,
	}
}

func step4VerificationMaterializedSyncInput(
	fixture step4MaterializedFixture,
	streamID uuid.UUID,
	expectedStateVersion int64,
	label string,
	events []model.StudyEvent,
) StudyEventSyncInput {
	return StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               model.OperatorStudyPrincipalID(),
		ProfileRef:              fixture.profileRef,
		SessionID:               fixture.root.Persistence.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyMaterializationHash("step4-verify-" + label),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    expectedStateVersion,
		Events:                  events,
	}
}

// TestPlanLexinetStep4Verify1Item4AcceptAndIdempotentlyReplayAnOfflinePresentedRevealed
// proves one offline causal chain commits atomically and its original sync
// identity returns the immutable receipt authority after a repository restart.
func TestPlanLexinetStep4Verify1Item4AcceptAndIdempotentlyReplayAnOfflinePresentedRevealed(t *testing.T) {
	subject := newStep4VerificationSubject(t)
	fixture := newStudyEventTestFixture(t, subject.context, subject.repository, "step4-item4", "")
	item := fixture.Plan.Items[0]
	streamID := uuid.New()
	presentedID := uuid.New()
	revealedID := uuid.New()
	ratedID := uuid.New()
	voidedID := uuid.New()
	rating := model.StudyRatingGood
	events := []model.StudyEvent{
		newStudyEventTestEvent(fixture, item, streamID, 1, presentedID, model.StudyEventKindPresented, nil, nil),
		newStudyEventTestEvent(fixture, item, streamID, 2, revealedID, model.StudyEventKindRevealed, nil, nil),
		newStudyEventTestEvent(fixture, item, streamID, 3, ratedID, model.StudyEventKindRated, &rating, nil),
		newStudyEventTestEvent(fixture, item, streamID, 4, voidedID, model.StudyEventKindRatingVoided, nil, &ratedID),
	}
	input := step4VerificationEventSyncInput(fixture, streamID, fixture.Plan.Session.StateVersion, "item4", events)

	created, err := subject.repository.AdjudicateStudyEvents(subject.context, input)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM4.CREATE_FAILED", err))
	}
	assertStudyEventAcceptedRange(t, created, 1, 4, 1)
	if created.Replayed || created.FlushReceipt.AcknowledgedThroughSequence != 4 ||
		created.FlushReceipt.ProfileIngestionHighWater != 4 ||
		created.Session.StateVersion != fixture.Plan.Session.StateVersion+1 {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM4.ATOMIC_AUTHORITY_INVALID")
	}

	replayed, err := NewRepository(subject.pool).AdjudicateStudyEvents(subject.context, input)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM4.REPLAY_FAILED", err))
	}
	if !replayed.Replayed || !reflect.DeepEqual(replayed.Receipts, created.Receipts) ||
		!reflect.DeepEqual(replayed.FlushReceipt, created.FlushReceipt) ||
		!reflect.DeepEqual(replayed.PageArtifact, created.PageArtifact) ||
		replayed.Session.StateVersion != created.Session.StateVersion {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM4.REPLAY_AUTHORITY_CHANGED")
	}
	var canonicalCount, distinctOrdinalCount int
	if err := subject.pool.QueryRow(subject.context, `
		SELECT count(*), count(DISTINCT profile_ingestion_ordinal)
		FROM study_events
		WHERE study_session_id = $1`, fixture.Plan.Session.ID).Scan(&canonicalCount, &distinctOrdinalCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM4.CANONICAL_READ_FAILED", err))
	}
	if canonicalCount != 4 || distinctOrdinalCount != 4 {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM4.REPLAY_DUPLICATED_CANONICAL_FACTS")
	}

	// A terminal failure after a schema-valid presented prefix must adjudicate
	// the whole new suffix as terminal. No tempting accepted prefix, ordinal, or
	// profile high-water survives the atomic suffix decision.
	terminalFixture := newStudyEventTestFixture(t, subject.context, subject.repository, "step4-item4-terminal", "")
	terminalItem := terminalFixture.Plan.Items[0]
	terminalStreamID := uuid.New()
	terminalEvents := []model.StudyEvent{
		newStudyEventTestEvent(terminalFixture, terminalItem, terminalStreamID, 1, uuid.New(), model.StudyEventKindPresented, nil, nil),
		newStudyEventTestEvent(terminalFixture, terminalItem, terminalStreamID, 2, uuid.New(), model.StudyEventKindRated, &rating, nil),
		newStudyEventTestEvent(terminalFixture, terminalItem, terminalStreamID, 3, uuid.New(), model.StudyEventKindRevealed, nil, nil),
	}
	terminal, err := subject.repository.AdjudicateStudyEvents(subject.context, step4VerificationEventSyncInput(
		terminalFixture,
		terminalStreamID,
		terminalFixture.Plan.Session.StateVersion,
		"item4-terminal-middle",
		terminalEvents,
	))
	if err != nil || terminal == nil || len(terminal.Receipts) != len(terminalEvents) ||
		terminal.FlushReceipt.AcknowledgedThroughSequence != 3 ||
		terminal.FlushReceipt.ProfileIngestionHighWater != 0 {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM4.TERMINAL_BATCH_FAILED", err))
	}
	for _, receipt := range terminal.Receipts {
		if receipt.Disposition != model.StudyEventReceiptDispositionTerminalRejected ||
			receipt.Code != model.StudyEventReceiptCodeTransitionInvalid ||
			receipt.TerminalFailureClientSequence == nil || *receipt.TerminalFailureClientSequence != 2 ||
			receipt.CanonicalStudyEventID != nil || receipt.CanonicalProfileIngestionOrdinal != nil {
			t.Fatal("STUDY_STEP4_VERIFY.ITEM4.TERMINAL_BATCH_PARTIALLY_ACCEPTED")
		}
	}
	if err := subject.pool.QueryRow(subject.context, `
		SELECT count(*) FROM study_events WHERE study_session_id = $1`,
		terminalFixture.Plan.Session.ID,
	).Scan(&canonicalCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM4.TERMINAL_CANONICAL_READ_FAILED", err))
	}
	if canonicalCount != 0 {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM4.TERMINAL_BATCH_PERSISTED_PREFIX")
	}
}

// TestPlanLexinetStep4Verify1Item5RejectInvalidTransitions proves causal
// transition failures become stable terminal receipts without canonical facts.
func TestPlanLexinetStep4Verify1Item5RejectInvalidTransitions(t *testing.T) {
	subject := newStep4VerificationSubject(t)
	testCases := []struct {
		name         string
		kind         model.StudyEventKind
		rating       *model.StudyRating
		ratingTarget *uuid.UUID
		wantCode     model.StudyEventReceiptCode
	}{
		{name: "reveal-before-present", kind: model.StudyEventKindRevealed, wantCode: model.StudyEventReceiptCodeTransitionInvalid},
		{name: "rate-before-reveal", kind: model.StudyEventKindRated, rating: step4VerificationRating(model.StudyRatingHard), wantCode: model.StudyEventReceiptCodeTransitionInvalid},
		{name: "void-unknown-rating", kind: model.StudyEventKindRatingVoided, ratingTarget: step4VerificationUUID(uuid.New()), wantCode: model.StudyEventReceiptCodeRatingVoidTargetInvalid},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newStudyEventTestFixture(t, subject.context, subject.repository, "step4-item5-"+testCase.name, "")
			streamID := uuid.New()
			event := newStudyEventTestEvent(
				fixture,
				fixture.Plan.Items[0],
				streamID,
				1,
				uuid.New(),
				testCase.kind,
				testCase.rating,
				testCase.ratingTarget,
			)
			result, err := subject.repository.AdjudicateStudyEvents(
				subject.context,
				step4VerificationEventSyncInput(fixture, streamID, fixture.Plan.Session.StateVersion, "item5-"+testCase.name, []model.StudyEvent{event}),
			)
			if err != nil || result == nil || len(result.Receipts) != 1 {
				t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM5.ADJUDICATION_FAILED", err))
			}
			receipt := result.Receipts[0]
			if receipt.Disposition != model.StudyEventReceiptDispositionTerminalRejected ||
				receipt.Code != testCase.wantCode || receipt.CanonicalStudyEventID != nil ||
				receipt.CanonicalProfileIngestionOrdinal != nil ||
				receipt.TerminalFailureClientSequence == nil || *receipt.TerminalFailureClientSequence != 1 ||
				result.FlushReceipt.ProfileIngestionHighWater != 0 ||
				result.FlushReceipt.AcknowledgedThroughSequence != 1 {
				t.Fatal("STUDY_STEP4_VERIFY.ITEM5.TERMINAL_RECEIPT_INVALID")
			}
			var canonicalCount int
			if err := subject.pool.QueryRow(subject.context, `
				SELECT count(*) FROM study_events WHERE study_session_id = $1`,
				fixture.Plan.Session.ID,
			).Scan(&canonicalCount); err != nil {
				t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM5.CANONICAL_READ_FAILED", err))
			}
			if canonicalCount != 0 {
				t.Fatal("STUDY_STEP4_VERIFY.ITEM5.INVALID_TRANSITION_WROTE_FACT")
			}
		})
	}
}

// TestPlanLexinetStep4Verify1Item6ReconcileADistinctUuidDuplicateObservationWithoutA
// proves semantic identity points a new event UUID at the original canonical
// observation and ordinal without allocating another fact.
func TestPlanLexinetStep4Verify1Item6ReconcileADistinctUuidDuplicateObservationWithoutA(t *testing.T) {
	subject := newStep4VerificationSubject(t)
	fixture := newStudyEventTestFixture(t, subject.context, subject.repository, "step4-item6", "")
	item := fixture.Plan.Items[0]
	streamID := uuid.New()
	canonicalID := uuid.New()
	first, err := subject.repository.AdjudicateStudyEvents(subject.context, step4VerificationEventSyncInput(
		fixture,
		streamID,
		fixture.Plan.Session.StateVersion,
		"item6-first",
		[]model.StudyEvent{newStudyEventTestEvent(fixture, item, streamID, 1, canonicalID, model.StudyEventKindPresented, nil, nil)},
	))
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM6.FIRST_FAILED", err))
	}
	assertStudyEventAcceptedRange(t, first, 1, 1, 1)

	duplicateID := uuid.New()
	duplicate, err := subject.repository.AdjudicateStudyEvents(subject.context, step4VerificationEventSyncInput(
		fixture,
		streamID,
		first.Session.StateVersion,
		"item6-duplicate",
		[]model.StudyEvent{newStudyEventTestEvent(fixture, item, streamID, 2, duplicateID, model.StudyEventKindPresented, nil, nil)},
	))
	if err != nil || duplicate == nil || len(duplicate.Receipts) != 1 {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM6.DUPLICATE_FAILED", err))
	}
	assertStudyEventSemanticDuplicate(t, duplicate.Receipts[0], canonicalID, 1, "STUDY_STEP4_VERIFY.ITEM6.DUPLICATE_AUTHORITY_INVALID")
	if duplicate.Receipts[0].EventID != duplicateID || duplicateID == canonicalID ||
		duplicate.FlushReceipt.ProfileIngestionHighWater != 1 {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM6.DISTINCT_UUID_NOT_RECONCILED")
	}
	var canonicalCount, bindingCount int
	if err := subject.pool.QueryRow(subject.context, `
		SELECT
			(SELECT count(*) FROM study_events WHERE study_session_id = $1),
			(SELECT count(*) FROM study_event_id_bindings WHERE client_event_id = ANY($2::uuid[]))`,
		fixture.Plan.Session.ID,
		[]uuid.UUID{canonicalID, duplicateID},
	).Scan(&canonicalCount, &bindingCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM6.AUTHORITY_READ_FAILED", err))
	}
	if canonicalCount != 1 || bindingCount != 2 {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM6.NEW_ORDINAL_OR_MISSING_BINDING")
	}
}

// TestPlanLexinetStep4Verify1Item7ConcurrentlySyncTwoClientStreamsAndProveUnique
// observes real PostgreSQL lock contention between two streams before proving
// their committed per-profile prefix is exactly ordinals one and two.
func TestPlanLexinetStep4Verify1Item7ConcurrentlySyncTwoClientStreamsAndProveUnique(t *testing.T) {
	subject := newStep4VerificationSubject(t)
	profileRef := "step4-item7-" + stringsForStudyFixture(uuid.NewString())
	fixtures := []studyEventTestFixture{
		newStudyEventTestFixture(t, subject.context, subject.repository, "step4-item7-a", profileRef),
		newStudyEventTestFixture(t, subject.context, subject.repository, "step4-item7-b", profileRef),
	}

	firstAtCommit := make(chan struct{})
	releaseFirst := make(chan struct{})
	var signalOnce sync.Once
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	subject.repository.studyEventBeforeCommit = func(hookContext context.Context, _ pgx.Tx) error {
		signalOnce.Do(func() { close(firstAtCommit) })
		select {
		case <-releaseFirst:
			return nil
		case <-hookContext.Done():
			return hookContext.Err()
		}
	}
	secondRepository, secondApplicationName, secondPool := newAdversarialWorkerRepository(
		t,
		subject.context,
		subject.pool,
		"study_step4_item7",
	)
	workerContext, cancelWorkers := context.WithCancel(subject.context)
	firstDone := make(chan struct{})
	secondDone := make(chan struct{})
	secondStarted := false
	defer func() {
		cancelWorkers()
		release()
		waitForStudyEventWorkerCleanup(t, firstDone, "STUDY_STEP4_VERIFY.ITEM7.FIRST_CLEANUP_TIMEOUT")
		if secondStarted {
			waitForStudyEventWorkerCleanup(t, secondDone, "STUDY_STEP4_VERIFY.ITEM7.SECOND_CLEANUP_TIMEOUT")
		}
		subject.repository.studyEventBeforeCommit = nil
		secondPool.Close()
	}()

	runSync := func(repository *Repository, fixture studyEventTestFixture, label string) step4SyncOutcome {
		streamID := uuid.New()
		result, err := repository.AdjudicateStudyEvents(workerContext, step4VerificationEventSyncInput(
			fixture,
			streamID,
			fixture.Plan.Session.StateVersion,
			label,
			[]model.StudyEvent{newStudyEventTestEvent(
				fixture,
				fixture.Plan.Items[0],
				streamID,
				1,
				uuid.New(),
				model.StudyEventKindPresented,
				nil,
				nil,
			)},
		))
		return step4SyncOutcome{result: result, err: err}
	}
	firstOutcome := make(chan step4SyncOutcome, 1)
	go func() {
		defer close(firstDone)
		firstOutcome <- runSync(subject.repository, fixtures[0], "item7-first")
	}()
	select {
	case <-firstAtCommit:
	case <-subject.context.Done():
		t.Fatal("STUDY_STEP4_VERIFY.ITEM7.FIRST_COMMIT_BARRIER_TIMEOUT")
	}

	secondOutcome := make(chan step4SyncOutcome, 1)
	secondStarted = true
	go func() {
		defer close(secondDone)
		secondOutcome <- runSync(secondRepository, fixtures[1], "item7-second")
	}()
	waitContext, cancelWait := context.WithTimeout(subject.context, 10*time.Second)
	lockErr := waitForAdversarialPostgresLock(waitContext, subject.pool, secondApplicationName, secondDone)
	cancelWait()
	if lockErr != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM7.SECOND_NOT_LOCK_BLOCKED", lockErr))
	}
	release()
	select {
	case <-firstDone:
	case <-subject.context.Done():
		t.Fatal("STUDY_STEP4_VERIFY.ITEM7.FIRST_JOIN_TIMEOUT")
	}
	select {
	case <-secondDone:
	case <-subject.context.Done():
		t.Fatal("STUDY_STEP4_VERIFY.ITEM7.SECOND_JOIN_TIMEOUT")
	}
	subject.repository.studyEventBeforeCommit = nil

	ordinals := make([]int64, 0, 2)
	highWaters := make([]int64, 0, 2)
	for _, outcome := range []step4SyncOutcome{<-firstOutcome, <-secondOutcome} {
		if outcome.err != nil || outcome.result == nil || len(outcome.result.Receipts) != 1 ||
			outcome.result.Receipts[0].CanonicalProfileIngestionOrdinal == nil {
			t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM7.CONCURRENT_SYNC_FAILED", outcome.err))
		}
		ordinals = append(ordinals, *outcome.result.Receipts[0].CanonicalProfileIngestionOrdinal)
		highWaters = append(highWaters, outcome.result.FlushReceipt.ProfileIngestionHighWater)
	}
	sort.Slice(ordinals, func(left, right int) bool { return ordinals[left] < ordinals[right] })
	sort.Slice(highWaters, func(left, right int) bool { return highWaters[left] < highWaters[right] })
	if !reflect.DeepEqual(ordinals, []int64{1, 2}) || !reflect.DeepEqual(highWaters, []int64{1, 2}) {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM7.COMMITTED_PREFIX_NOT_GAP_FREE")
	}
	profile, err := subject.repository.GetStudyLearnerProfile(subject.context, profileRef)
	if err != nil || profile.ProfileIngestionHighWater != 2 {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM7.PROFILE_HIGH_WATER_INVALID", err))
	}
	var eventCount, distinctOrdinalCount, minimumOrdinal, maximumOrdinal int64
	if err := subject.pool.QueryRow(subject.context, `
		SELECT count(*), count(DISTINCT profile_ingestion_ordinal),
		       min(profile_ingestion_ordinal), max(profile_ingestion_ordinal)
		FROM study_events
		WHERE learner_profile_id = $1`, profile.ID).Scan(
		&eventCount,
		&distinctOrdinalCount,
		&minimumOrdinal,
		&maximumOrdinal,
	); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM7.PREFIX_READ_FAILED", err))
	}
	if eventCount != 2 || distinctOrdinalCount != 2 || minimumOrdinal != 1 || maximumOrdinal != 2 {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM7.DATABASE_PREFIX_INVALID")
	}
}

// TestPlanLexinetStep4Verify1Item8ProveATerminalRejectionOrSemanticDuplicateCannot
// proves both terminal receipt kinds advance the stream cursor while only
// accepted facts advance the learner ingestion prefix.
func TestPlanLexinetStep4Verify1Item8ProveATerminalRejectionOrSemanticDuplicateCannot(t *testing.T) {
	subject := newStep4VerificationSubject(t)
	fixture := newStudyEventTestFixture(t, subject.context, subject.repository, "step4-item8", "")
	item := fixture.Plan.Items[0]
	streamID := uuid.New()
	rating := model.StudyRatingEasy
	terminal, err := subject.repository.AdjudicateStudyEvents(subject.context, step4VerificationEventSyncInput(
		fixture,
		streamID,
		fixture.Plan.Session.StateVersion,
		"item8-terminal",
		[]model.StudyEvent{newStudyEventTestEvent(fixture, item, streamID, 1, uuid.New(), model.StudyEventKindRated, &rating, nil)},
	))
	if err != nil || terminal == nil || len(terminal.Receipts) != 1 ||
		terminal.Receipts[0].Disposition != model.StudyEventReceiptDispositionTerminalRejected ||
		terminal.Receipts[0].Code != model.StudyEventReceiptCodeTransitionInvalid ||
		terminal.FlushReceipt.AcknowledgedThroughSequence != 1 ||
		terminal.FlushReceipt.ProfileIngestionHighWater != 0 {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM8.TERMINAL_FAILED", err))
	}

	presentedID := uuid.New()
	presented, err := subject.repository.AdjudicateStudyEvents(subject.context, step4VerificationEventSyncInput(
		fixture,
		streamID,
		terminal.Session.StateVersion,
		"item8-presented",
		[]model.StudyEvent{newStudyEventTestEvent(fixture, item, streamID, 2, presentedID, model.StudyEventKindPresented, nil, nil)},
	))
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM8.POST_TERMINAL_FAILED", err))
	}
	assertStudyEventAcceptedRange(t, presented, 2, 2, 1)

	semantic, err := subject.repository.AdjudicateStudyEvents(subject.context, step4VerificationEventSyncInput(
		fixture,
		streamID,
		presented.Session.StateVersion,
		"item8-semantic",
		[]model.StudyEvent{newStudyEventTestEvent(fixture, item, streamID, 3, uuid.New(), model.StudyEventKindPresented, nil, nil)},
	))
	if err != nil || semantic == nil || len(semantic.Receipts) != 1 {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM8.SEMANTIC_FAILED", err))
	}
	assertStudyEventSemanticDuplicate(t, semantic.Receipts[0], presentedID, 1, "STUDY_STEP4_VERIFY.ITEM8.SEMANTIC_RECEIPT_INVALID")
	if semantic.FlushReceipt.AcknowledgedThroughSequence != 3 || semantic.FlushReceipt.ProfileIngestionHighWater != 1 {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM8.SEMANTIC_CURSOR_INVALID")
	}

	revealed, err := subject.repository.AdjudicateStudyEvents(subject.context, step4VerificationEventSyncInput(
		fixture,
		streamID,
		semantic.Session.StateVersion,
		"item8-revealed",
		[]model.StudyEvent{newStudyEventTestEvent(fixture, item, streamID, 4, uuid.New(), model.StudyEventKindRevealed, nil, nil)},
	))
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM8.POST_SEMANTIC_FAILED", err))
	}
	assertStudyEventAcceptedRange(t, revealed, 4, 4, 2)
	if revealed.FlushReceipt.AcknowledgedThroughSequence != 4 || revealed.FlushReceipt.ProfileIngestionHighWater != 2 {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM8.CONTIGUOUS_PROGRESS_INVALID")
	}
}

// TestPlanLexinetStep4Verify1Item9ChangedReceiptReplayConflicts proves a
// durable sync identity rejects both a changed request hash and changed event
// authority without mutating the original result.
func TestPlanLexinetStep4Verify1Item9ChangedReceiptReplayConflicts(t *testing.T) {
	subject := newStep4VerificationSubject(t)
	fixture := newStudyEventTestFixture(t, subject.context, subject.repository, "step4-item9", "")
	streamID := uuid.New()
	input := step4VerificationEventSyncInput(
		fixture,
		streamID,
		fixture.Plan.Session.StateVersion,
		"item9-original",
		[]model.StudyEvent{newStudyEventTestEvent(
			fixture,
			fixture.Plan.Items[0],
			streamID,
			1,
			uuid.New(),
			model.StudyEventKindPresented,
			nil,
			nil,
		)},
	)
	created, err := subject.repository.AdjudicateStudyEvents(subject.context, input)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM9.CREATE_FAILED", err))
	}
	assertStudyEventAcceptedRange(t, created, 1, 1, 1)

	assertStudyEventSyncRejectedWithoutWrites(
		t,
		subject.context,
		subject.pool,
		subject.repository,
		input,
		func(changed *StudyEventSyncInput) {
			changed.NormalizedRequestSHA256 = studyEventTestHash("step4-verify-item9-changed-hash")
		},
		model.StudyErrorCodeIdempotencyConflict,
		"STEP4_ITEM9_CHANGED_HASH",
	)
	assertStep4VerificationOriginalSyncAuthority(t, subject, input, created)
	assertStudyEventSyncRejectedWithoutWrites(
		t,
		subject.context,
		subject.pool,
		subject.repository,
		input,
		func(changed *StudyEventSyncInput) {
			changed.Events = append([]model.StudyEvent(nil), changed.Events...)
			changed.Events[0].ID = uuid.New()
		},
		model.StudyErrorCodeIdempotencyConflict,
		"STEP4_ITEM9_CHANGED_EVENT",
	)
	assertStep4VerificationOriginalSyncAuthority(t, subject, input, created)
}

// TestPlanLexinetStep4Verify1Item11RejectLocalOnlyExhaustion proves a flush
// that acknowledges only the locally presented prefix cannot complete a
// predecessor while another issued item lacks canonical presented evidence.
func TestPlanLexinetStep4Verify1Item11RejectLocalOnlyExhaustion(t *testing.T) {
	subject := newStep4VerificationSubject(t)
	fixture := newStudySuccessorProductionFixture(t, subject.context, subject.repository, "step4-item11", 2)
	streamID := uuid.New()
	partial, err := subject.repository.AdjudicateStudyEvents(subject.context, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Root.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studySuccessorProductionHash("step4-item11-partial"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    fixture.Root.Session.StateVersion,
		Events: []model.StudyEvent{studySuccessorProductionEvent(
			fixture,
			fixture.Root.Items[0],
			streamID,
			1,
			uuid.New(),
			model.StudyEventKindPresented,
			nil,
		)},
	})
	if err != nil || partial == nil || partial.FlushReceipt.ProfileIngestionHighWater != 1 {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM11.PARTIAL_SYNC_FAILED", err))
	}
	input := studySuccessorProductionInput(
		fixture,
		partial.FlushReceipt.ProfileIngestionHighWater,
		partial.FlushReceipt.ID,
		"step4-item11-successor",
	)
	result, err := insertStudySuccessorProductionPlan(subject.context, subject.repository, fixture, input)
	if result != nil || !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeCycleNotExhausted}) {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM11.LOCAL_EXHAUSTION_ACCEPTED")
	}
	predecessor, readErr := subject.repository.GetStudySession(subject.context, fixture.ProfileRef, fixture.Root.Session.ID)
	if readErr != nil || predecessor.Lifecycle != model.StudySessionLifecycleActive ||
		predecessor.StateVersion != partial.Session.StateVersion || predecessor.SuccessorStudySessionID != nil ||
		predecessor.TerminalProfileIngestionHighWater != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM11.REJECTION_MUTATED_PREDECESSOR", readErr))
	}
	var presentedItemCount, successorCount, receiptCount int
	if err := subject.pool.QueryRow(subject.context, `
		SELECT
			count(DISTINCT event.study_session_item_id),
			(SELECT count(*) FROM study_sessions WHERE predecessor_study_session_id = $1),
			(SELECT count(*) FROM study_session_creation_receipts WHERE creation_request_id = $2)
		FROM study_events event
		WHERE event.study_session_id = $1 AND event.study_event_kind = 'presented'`,
		fixture.Root.Session.ID,
		input.Plan.CreationRequestID,
	).Scan(&presentedItemCount, &successorCount, &receiptCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM11.AUTHORITY_READ_FAILED", err))
	}
	if presentedItemCount != 1 || successorCount != 0 || receiptCount != 0 {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM11.REJECTION_PERSISTED_SUCCESSOR_AUTHORITY")
	}
}

// TestStudyStep4StorageRaceConcurrentSuccessorRequestsConverge observes real
// scope-lock contention and proves two distinct storage creation UUIDs bind to
// exactly one predecessor-linked successor. The public Step 4 map proof lives
// at the canonical service/HTTP boundary.
func TestStudyStep4StorageRaceConcurrentSuccessorRequestsConverge(t *testing.T) {
	subject := newStep4VerificationSubject(t)
	fixture := newStudySuccessorProductionFixture(t, subject.context, subject.repository, "step4-item12", 1)
	streamID := uuid.New()
	barrier, err := subject.repository.AdjudicateStudyEvents(subject.context, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Root.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studySuccessorProductionHash("step4-item12-barrier"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    fixture.Root.Session.StateVersion,
		Events: []model.StudyEvent{studySuccessorProductionEvent(
			fixture,
			fixture.Root.Items[0],
			streamID,
			1,
			uuid.New(),
			model.StudyEventKindPresented,
			nil,
		)},
	})
	if err != nil || barrier == nil || barrier.FlushReceipt.ProfileIngestionHighWater != 1 {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM12.BARRIER_FAILED", err))
	}
	inputs := []StudySuccessorPlanInsert{
		studySuccessorProductionInput(fixture, 1, barrier.FlushReceipt.ID, "step4-item12-a"),
		studySuccessorProductionInput(fixture, 1, barrier.FlushReceipt.ID, "step4-item12-b"),
	}
	predecessorID := fixture.Root.Session.ID
	outcomes := runStudySuccessorContendedScopeTransactions(
		t,
		subject.context,
		subject.pool,
		"study_step4_item12",
		fixture.ProfileRef,
		fixture.Scope,
		func(operationContext context.Context, store StudyStoreTransaction) (*StudyPlanInsertResult, error) {
			if _, listErr := store.ListEligibleSources(operationContext); listErr != nil {
				return nil, listErr
			}
			return store.InsertSuccessorPlanOnce(operationContext, inputs[0])
		},
		func(operationContext context.Context, store StudyStoreTransaction) (*StudyPlanInsertResult, error) {
			if current := store.NonterminalSession(); current != nil && current.ID == predecessorID {
				if _, listErr := store.ListEligibleSources(operationContext); listErr != nil {
					return nil, listErr
				}
			}
			return store.InsertSuccessorPlanOnce(operationContext, inputs[1])
		},
	)
	for _, outcome := range outcomes {
		if outcome.err != nil || outcome.result == nil ||
			outcome.result.Receipt.Disposition != model.StudyCreationDispositionSuccessor {
			t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM12.CONCURRENT_SUCCESSOR_FAILED", outcome.err))
		}
	}
	if outcomes[0].result.Session.ID != outcomes[1].result.Session.ID ||
		outcomes[0].result.Created == outcomes[1].result.Created ||
		outcomes[0].result.Receipt.CreationRequestID == outcomes[1].result.Receipt.CreationRequestID {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM12.SUCCESSOR_AUTHORITY_NOT_UNIQUE")
	}
	predecessor, err := subject.repository.GetStudySession(subject.context, fixture.ProfileRef, predecessorID)
	if err != nil || predecessor.Lifecycle != model.StudySessionLifecycleCompleted ||
		predecessor.SuccessorStudySessionID == nil ||
		*predecessor.SuccessorStudySessionID != outcomes[0].result.Session.ID ||
		predecessor.TerminalProfileIngestionHighWater == nil ||
		*predecessor.TerminalProfileIngestionHighWater != 1 {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM12.PREDECESSOR_LINK_INVALID", err))
	}
	var successorCount, receiptCount int
	if err := subject.pool.QueryRow(subject.context, `
		SELECT
			(SELECT count(*) FROM study_sessions WHERE predecessor_study_session_id = $1),
			(SELECT count(*) FROM study_session_creation_receipts
			 WHERE creation_request_id = ANY($2::uuid[]))`,
		predecessorID,
		[]uuid.UUID{inputs[0].Plan.CreationRequestID, inputs[1].Plan.CreationRequestID},
	).Scan(&successorCount, &receiptCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM12.CARDINALITY_READ_FAILED", err))
	}
	if successorCount != 1 || receiptCount != 2 {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM12.CARDINALITY_INVALID")
	}
}

// TestPlanLexinetStep4Verify1Item16AcknowledgeAFinalPredecessorRatingBeforeSuccessorCreation
// proves the production materializer rebuilds through the acknowledged rating
// and freezes that exact prefix as both terminal and successor authority.
func TestPlanLexinetStep4Verify1Item16AcknowledgeAFinalPredecessorRatingBeforeSuccessorCreation(t *testing.T) {
	subject := newStep4VerificationSubject(t)
	fixture := newStep4MaterializedFixture(t, subject, "item16")
	item := fixture.root.Persistence.Items[0]
	streamID := uuid.New()
	ratedID := uuid.New()
	rating := model.StudyRatingGood
	events := []model.StudyEvent{
		newStudyMaterializationEvent(fixture.profileRef, item, streamID, 1, uuid.New(), model.StudyEventKindPresented, nil, nil),
		newStudyMaterializationEvent(fixture.profileRef, item, streamID, 2, uuid.New(), model.StudyEventKindRevealed, nil, nil),
		newStudyMaterializationEvent(fixture.profileRef, item, streamID, 3, ratedID, model.StudyEventKindRated, &rating, nil),
	}
	barrier, err := subject.repository.AdjudicateStudyEvents(subject.context, step4VerificationMaterializedSyncInput(
		fixture,
		streamID,
		fixture.root.Persistence.Session.StateVersion,
		"item16-barrier",
		events,
	))
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM16.RATING_SYNC_FAILED", err))
	}
	assertStudyEventAcceptedRange(t, barrier, 1, 3, 1)
	if barrier.FlushReceipt.ProfileIngestionHighWater != 3 ||
		barrier.Receipts[2].CanonicalStudyEventID == nil || *barrier.Receipts[2].CanonicalStudyEventID != ratedID {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM16.RATING_NOT_ACKNOWLEDGED")
	}

	predecessorID := fixture.root.Persistence.Session.ID
	successorRequest := newStudyMaterializationRequest(
		fixture.profileRef,
		fixture.scope,
		1,
		"step4-verify-item16-successor",
		&predecessorID,
		barrier.FlushReceipt.ID,
	)
	successor, err := subject.repository.MaterializeStudyCycle(subject.context, subject.materializer, successorRequest)
	if err != nil || successor == nil || !successor.Persistence.Created || len(successor.Persistence.Items) != 1 {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM16.SUCCESSOR_FAILED", err))
	}
	predecessor, err := subject.repository.GetStudySession(subject.context, fixture.profileRef, predecessorID)
	if err != nil || predecessor.TerminalProfileIngestionHighWater == nil ||
		*predecessor.TerminalProfileIngestionHighWater != 3 ||
		predecessor.SuccessorStudySessionID == nil ||
		*predecessor.SuccessorStudySessionID != successor.Persistence.Session.ID {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM16.TERMINAL_HIGH_WATER_INVALID", err))
	}
	if successor.Persistence.Session.Scheduler == nil ||
		successor.Persistence.Session.Scheduler.InputProfileIngestionHighWater != 3 ||
		successor.Persistence.Items[0].SelectionReason.RatingRank != model.StudyRatingRankGood {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM16.SUCCESSOR_PREFIX_INVALID")
	}
	var projectedRating string
	var ratingOrdinal, rebuiltThrough int64
	if err := subject.pool.QueryRow(subject.context, `
		SELECT latest_effective_rating, latest_effective_rating_ingestion_ordinal,
		       rebuilt_through_profile_ingestion_ordinal
		FROM study_scheduler_card_projections
		WHERE learner_profile_id = $1
		  AND projected_study_card_identity = $2
		  AND study_scheduler_name = $3
		  AND study_scheduler_version = $4`,
		successor.Persistence.Session.LearnerProfileID,
		item.CardIdentity,
		successor.Persistence.Session.Scheduler.Name,
		successor.Persistence.Session.Scheduler.Version,
	).Scan(&projectedRating, &ratingOrdinal, &rebuiltThrough); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM16.PROJECTION_READ_FAILED", err))
	}
	if projectedRating != string(model.StudyRatingGood) || ratingOrdinal != 3 || rebuiltThrough != 3 {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM16.PROJECTION_PREFIX_INVALID")
	}
}

// TestPlanLexinetStep4Verify1Item17AcceptARetainedLateCorrectionAgainstACompleted
// proves a retained void remains append-only after completion while the
// already issued successor and predecessor terminal high-water stay frozen.
func TestPlanLexinetStep4Verify1Item17AcceptARetainedLateCorrectionAgainstACompleted(t *testing.T) {
	subject := newStep4VerificationSubject(t)
	fixture := newStep4MaterializedFixture(t, subject, "item17")
	item := fixture.root.Persistence.Items[0]
	streamID := uuid.New()
	ratedID := uuid.New()
	rating := model.StudyRatingGood
	initialEvents := []model.StudyEvent{
		newStudyMaterializationEvent(fixture.profileRef, item, streamID, 1, uuid.New(), model.StudyEventKindPresented, nil, nil),
		newStudyMaterializationEvent(fixture.profileRef, item, streamID, 2, uuid.New(), model.StudyEventKindRevealed, nil, nil),
		newStudyMaterializationEvent(fixture.profileRef, item, streamID, 3, ratedID, model.StudyEventKindRated, &rating, nil),
	}
	barrier, err := subject.repository.AdjudicateStudyEvents(subject.context, step4VerificationMaterializedSyncInput(
		fixture,
		streamID,
		fixture.root.Persistence.Session.StateVersion,
		"item17-barrier",
		initialEvents,
	))
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM17.INITIAL_SYNC_FAILED", err))
	}
	predecessorID := fixture.root.Persistence.Session.ID
	successorRequest := newStudyMaterializationRequest(
		fixture.profileRef,
		fixture.scope,
		1,
		"step4-verify-item17-successor",
		&predecessorID,
		barrier.FlushReceipt.ID,
	)
	successor, err := subject.repository.MaterializeStudyCycle(subject.context, subject.materializer, successorRequest)
	if err != nil || successor == nil || !successor.Persistence.Created {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM17.SUCCESSOR_FAILED", err))
	}
	issuedBefore, err := subject.repository.ReadStudySessionCycle(subject.context, successor.Persistence.Session.ID)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM17.SUCCESSOR_BEFORE_READ_FAILED", err))
	}
	completedBefore, err := subject.repository.GetStudySession(subject.context, fixture.profileRef, predecessorID)
	if err != nil || completedBefore.Lifecycle != model.StudySessionLifecycleCompleted ||
		completedBefore.TerminalProfileIngestionHighWater == nil ||
		*completedBefore.TerminalProfileIngestionHighWater != 3 {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM17.PREDECESSOR_NOT_COMPLETED", err))
	}

	voidedID := uuid.New()
	correction := newStudyMaterializationEvent(
		fixture.profileRef,
		item,
		streamID,
		4,
		voidedID,
		model.StudyEventKindRatingVoided,
		nil,
		&ratedID,
	)
	corrected, err := subject.repository.AdjudicateStudyEvents(subject.context, step4VerificationMaterializedSyncInput(
		fixture,
		streamID,
		completedBefore.StateVersion,
		"item17-late-void",
		[]model.StudyEvent{correction},
	))
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM17.LATE_CORRECTION_FAILED", err))
	}
	assertStudyEventAcceptedRange(t, corrected, 4, 4, 4)
	if corrected.Receipts[0].CanonicalStudyEventID == nil ||
		*corrected.Receipts[0].CanonicalStudyEventID != voidedID ||
		corrected.Session.Lifecycle != model.StudySessionLifecycleCompleted ||
		corrected.Session.TerminalProfileIngestionHighWater == nil ||
		*corrected.Session.TerminalProfileIngestionHighWater != 3 ||
		corrected.Session.SuccessorStudySessionID == nil ||
		*corrected.Session.SuccessorStudySessionID != successor.Persistence.Session.ID {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM17.LATE_CORRECTION_CHANGED_TERMINAL_AUTHORITY")
	}
	issuedAfter, err := subject.repository.ReadStudySessionCycle(subject.context, successor.Persistence.Session.ID)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM17.SUCCESSOR_AFTER_READ_FAILED", err))
	}
	if issuedBefore.Session.ProfileIngestionHighWater != 3 || issuedAfter.Session.ProfileIngestionHighWater != 4 {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM17.LIVE_PROFILE_HIGH_WATER_INVALID")
	}
	issuedBeforeSession := issuedBefore.Session
	issuedBeforeSession.ProfileIngestionHighWater = issuedAfter.Session.ProfileIngestionHighWater
	if !reflect.DeepEqual(issuedAfter.Session, issuedBeforeSession) ||
		!reflect.DeepEqual(issuedAfter.Items, issuedBefore.Items) {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM17.LATE_CORRECTION_REWROTE_SUCCESSOR")
	}
	var correctionCount int
	var correctionOrdinal int64
	if err := subject.pool.QueryRow(subject.context, `
		SELECT count(*), max(profile_ingestion_ordinal)
		FROM study_events
		WHERE study_session_id = $1
		  AND study_event_kind = 'rating_voided'
		  AND rating_event_id = $2`, predecessorID, ratedID).Scan(&correctionCount, &correctionOrdinal); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM17.CORRECTION_READ_FAILED", err))
	}
	if correctionCount != 1 || correctionOrdinal != 4 {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM17.LATE_CORRECTION_NOT_RETAINED")
	}
}

// TestPlanLexinetStep4Verify1Item18ProveNextOrPresentedNeverWritesAGood proves
// root and successor materialization plus presented-only progress cannot invent
// a rated event, a good rating, or an effective-rating projection.
func TestPlanLexinetStep4Verify1Item18ProveNextOrPresentedNeverWritesAGood(t *testing.T) {
	subject := newStep4VerificationSubject(t)
	fixture := newStep4MaterializedFixture(t, subject, "item18")
	item := fixture.root.Persistence.Items[0]
	var eventsBefore int
	if err := subject.pool.QueryRow(subject.context, `
		SELECT count(*) FROM study_events WHERE learner_profile_id = $1`,
		fixture.root.Persistence.Session.LearnerProfileID,
	).Scan(&eventsBefore); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM18.INITIAL_EVENT_READ_FAILED", err))
	}
	if eventsBefore != 0 {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM18.ROOT_MATERIALIZATION_WROTE_EVENT")
	}

	streamID := uuid.New()
	presented, err := subject.repository.AdjudicateStudyEvents(subject.context, step4VerificationMaterializedSyncInput(
		fixture,
		streamID,
		fixture.root.Persistence.Session.StateVersion,
		"item18-presented",
		[]model.StudyEvent{newStudyMaterializationEvent(
			fixture.profileRef,
			item,
			streamID,
			1,
			uuid.New(),
			model.StudyEventKindPresented,
			nil,
			nil,
		)},
	))
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM18.PRESENTED_FAILED", err))
	}
	assertStudyEventAcceptedRange(t, presented, 1, 1, 1)

	predecessorID := fixture.root.Persistence.Session.ID
	successorRequest := newStudyMaterializationRequest(
		fixture.profileRef,
		fixture.scope,
		1,
		"step4-verify-item18-next",
		&predecessorID,
		presented.FlushReceipt.ID,
	)
	successor, err := subject.repository.MaterializeStudyCycle(subject.context, subject.materializer, successorRequest)
	if err != nil || successor == nil || !successor.Persistence.Created || len(successor.Persistence.Items) != 1 {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM18.NEXT_FAILED", err))
	}
	if successor.Persistence.Items[0].SelectionReason.RatingRank != model.StudyRatingRankUnseenUnrated {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM18.NEXT_INVENTED_RATING_RANK")
	}
	var ratedEventCount, goodEventCount int
	if err := subject.pool.QueryRow(subject.context, `
		SELECT
			count(*) FILTER (WHERE study_event_kind = 'rated'),
			count(*) FILTER (WHERE study_rating = 'good')
		FROM study_events
		WHERE learner_profile_id = $1`, successor.Persistence.Session.LearnerProfileID).Scan(
		&ratedEventCount,
		&goodEventCount,
	); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM18.RATING_EVENT_READ_FAILED", err))
	}
	if ratedEventCount != 0 || goodEventCount != 0 {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM18.PRESENTED_OR_NEXT_WROTE_GOOD")
	}
	var effectiveRating *string
	var effectiveRatingOrdinal *int64
	var rebuiltThrough int64
	if err := subject.pool.QueryRow(subject.context, `
		SELECT latest_effective_rating, latest_effective_rating_ingestion_ordinal,
		       rebuilt_through_profile_ingestion_ordinal
		FROM study_scheduler_card_projections
		WHERE learner_profile_id = $1
		  AND projected_study_card_identity = $2
		  AND study_scheduler_name = $3
		  AND study_scheduler_version = $4`,
		successor.Persistence.Session.LearnerProfileID,
		item.CardIdentity,
		successor.Persistence.Session.Scheduler.Name,
		successor.Persistence.Session.Scheduler.Version,
	).Scan(&effectiveRating, &effectiveRatingOrdinal, &rebuiltThrough); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM18.PROJECTION_READ_FAILED", err))
	}
	if effectiveRating != nil || effectiveRatingOrdinal != nil || rebuiltThrough != 1 {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM18.PROJECTION_INVENTED_EFFECTIVE_RATING")
	}
}

// TestPlanLexinetStep4Verify1Item19MutateSourceContentAfterSessionCreationAndProve
// proves acknowledged vocabulary mutation changes the live source while exact
// creation replay and committed reads retain the originally issued projection.
func TestPlanLexinetStep4Verify1Item19MutateSourceContentAfterSessionCreationAndProve(t *testing.T) {
	subject := newStep4VerificationSubject(t)
	fixture := newStep4MaterializedFixture(t, subject, "item19")
	issuedItem := fixture.root.Persistence.Items[0]
	if fixture.idea.CanonicalNameID == nil {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM19.CANONICAL_NAME_ID_MISSING")
	}
	definitions, err := subject.repository.ListDefinitions(subject.context, fixture.idea.ID.String())
	if err != nil || len(definitions) == 0 {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM19.DEFINITION_READ_FAILED", err))
	}
	primaryDefinition := definitions[0]
	for index := range definitions {
		if definitions[index].IsPrimary {
			primaryDefinition = definitions[index]
			break
		}
	}
	mutatedName := "Mutated issued name " + stringsForStudyFixture(uuid.NewString())
	mutatedDefinition := "Mutated issued definition " + stringsForStudyFixture(uuid.NewString())
	updatedName, err := subject.repository.UpdateName(
		subject.context,
		*fixture.idea.CanonicalNameID,
		model.UpdateNameInput{Text: &mutatedName},
	)
	if err != nil || updatedName.Text != mutatedName {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM19.NAME_MUTATION_FAILED", err))
	}
	updatedDefinition, err := subject.repository.UpdateDefinition(
		subject.context,
		primaryDefinition.ID,
		model.UpdateDefinitionInput{Body: &mutatedDefinition},
	)
	if err != nil || updatedDefinition.Body != mutatedDefinition {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM19.DEFINITION_MUTATION_FAILED", err))
	}
	if issuedItem.FrontCanonicalName == mutatedName || issuedItem.BackDefinition == mutatedDefinition {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM19.MUTATION_FIXTURE_NOT_DISTINCT")
	}

	committed, err := subject.repository.ReadCommittedStudyCycle(subject.context, StudyCommittedCycleReadInput{
		Principal:         fixture.request.Materialization.Principal,
		ProfileRef:        fixture.profileRef,
		SessionID:         fixture.root.Persistence.Session.ID,
		CreationRequestID: fixture.request.Materialization.CreationRequestID,
	})
	if err != nil || committed == nil || len(committed.Items) != 1 {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM19.COMMITTED_READ_FAILED", err))
	}
	if !reflect.DeepEqual(committed.Items[0], issuedItem) ||
		committed.Items[0].FrontCanonicalName != issuedItem.FrontCanonicalName ||
		committed.Items[0].BackDefinition != issuedItem.BackDefinition ||
		committed.Items[0].SourceContentSHA256 != issuedItem.SourceContentSHA256 ||
		committed.Items[0].SourceContentRevision != issuedItem.SourceContentRevision {
		t.Fatal("STUDY_STEP4_VERIFY.ITEM19.COMMITTED_PROJECTION_CHANGED")
	}

	replayed, err := subject.repository.MaterializeStudyCycle(subject.context, subject.materializer, fixture.request)
	if err != nil || replayed == nil || replayed.Persistence.Created || replayed.SchedulingSnapshot != nil ||
		!reflect.DeepEqual(replayed.Persistence.Items, fixture.root.Persistence.Items) ||
		!reflect.DeepEqual(replayed.AuthoritativePlan, fixture.root.AuthoritativePlan) ||
		replayed.Persistence.Receipt.CreationReceiptAdjudicatedAt != fixture.root.Persistence.Receipt.CreationReceiptAdjudicatedAt {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ITEM19.REPLAY_CHANGED_ISSUED_PROJECTION", err))
	}
}

func step4VerificationRating(value model.StudyRating) *model.StudyRating {
	return &value
}

func step4VerificationUUID(value uuid.UUID) *uuid.UUID {
	return &value
}

func assertStep4VerificationOriginalSyncAuthority(
	t *testing.T,
	subject step4VerificationSubject,
	input StudyEventSyncInput,
	original *StudyEventSyncResult,
) {
	t.Helper()
	replayed, err := NewRepository(subject.pool).AdjudicateStudyEvents(subject.context, input)
	if err != nil || replayed == nil || !replayed.Replayed ||
		!reflect.DeepEqual(replayed.Receipts, original.Receipts) ||
		!reflect.DeepEqual(replayed.FlushReceipt, original.FlushReceipt) ||
		!reflect.DeepEqual(replayed.PageArtifact, original.PageArtifact) ||
		!reflect.DeepEqual(replayed.Session, original.Session) {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ORIGINAL_SYNC_AUTHORITY_CHANGED", err))
	}
	profile, err := subject.repository.GetStudyLearnerProfile(subject.context, input.ProfileRef)
	if err != nil || profile.ProfileIngestionHighWater != original.FlushReceipt.ProfileIngestionHighWater {
		t.Fatal(studyTestDatabaseError("STUDY_STEP4_VERIFY.ORIGINAL_PROFILE_AUTHORITY_CHANGED", err))
	}
}
