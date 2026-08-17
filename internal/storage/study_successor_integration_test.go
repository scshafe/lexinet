package storage

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lexinet/internal/model"
)

type studySuccessorTestFixture struct {
	ProfileRef string
	Scope      model.StudyNormalizedScope
	IdeaID     uuid.UUID
	Root       *StudyPlanInsertResult
	FlushID    uuid.UUID
	StreamID   uuid.UUID
}

type studySuccessorProductionFixture struct {
	ProfileRef string
	Principal  model.StudyPrincipalID
	Scope      model.StudyNormalizedScope
	IdeaIDs    []uuid.UUID
	Root       *StudyPlanInsertResult
}

type studySuccessorEventOutcome struct {
	result *StudyEventSyncResult
	err    error
}

type studySuccessorInsertOutcome struct {
	result *StudyPlanInsertResult
	err    error
}

type studySuccessorScopedInsertOperation func(
	context.Context,
	StudyStoreTransaction,
) (*StudyPlanInsertResult, error)

func TestStudySuccessorPlanPersistence(t *testing.T) {
	harness := startStudyTestPostgres(t)
	harness.migrateUp(t)
	pool := harness.openPool(t)
	repository := NewRepository(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	t.Run("active predecessor completes once and concurrent creators converge", func(t *testing.T) {
		testStudySuccessorActiveConvergence(t, ctx, pool, repository)
	})
	t.Run("production event adjudication supplies the successor barrier", func(t *testing.T) {
		testStudySuccessorProductionEventBarrier(t, ctx, repository)
	})
	t.Run("production presented evidence exhausts every item exactly", func(t *testing.T) {
		testStudySuccessorProductionMultiItemExhaustion(t, ctx, repository)
	})
	t.Run("production terminal collateral cannot satisfy exhaustion", func(t *testing.T) {
		testStudySuccessorProductionTerminalCollateral(t, ctx, pool, repository)
	})
	t.Run("production streams cannot cross learner profiles", func(t *testing.T) {
		testStudySuccessorProductionCrossProfileStream(t, ctx, pool, repository)
	})
	t.Run("production correction and successor serialize in both orders", func(t *testing.T) {
		testStudySuccessorCorrectionSerialization(t, ctx, pool, repository)
	})
	t.Run("active predecessor rejects missing stale and unowned barriers", func(t *testing.T) {
		testStudySuccessorActiveBarrierFailures(t, ctx, pool, repository)
	})
	t.Run("waiting sentinel remains empty then is atomically superseded", func(t *testing.T) {
		testStudySuccessorWaitingReplacement(t, ctx, pool, repository)
	})
	t.Run("callback failure rolls back predecessor and successor", func(t *testing.T) {
		testStudySuccessorRollback(t, ctx, pool, repository)
	})
}

func testStudySuccessorProductionEventBarrier(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
) {
	t.Helper()
	suffix := stringsForStudyFixture(uuid.NewString())
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{
		Slug: "successor-production-event-" + suffix,
		Name: "Successor production event " + suffix,
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.PRODUCTION_EVENT_TOPIC_CREATE_FAILED", err))
	}
	idea := mustCreateStudyIdea(t, ctx, repository, topic.Slug, "successor-production-event")
	scope := mustStudyScope(t, topic.ID, topic.Slug)
	profileRef := "successor-production-event-" + suffix
	principal := model.OperatorStudyPrincipalID()

	rootPlan := testStudyPlan(uuid.New(), uuid.New(), principal, idea.ID, 0, "successor-production-event-root", nil)
	var root *StudyPlanInsertResult
	err = repository.WithStudyScopeTransaction(
		ctx,
		StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope},
		func(store StudyStoreTransaction) error {
			if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
				return listErr
			}
			var insertErr error
			root, insertErr = store.InsertPlanOnce(ctx, rootPlan)
			return insertErr
		},
	)
	if err != nil || root == nil || len(root.Items) != 1 {
		t.Fatal("STUDY_SUCCESSOR.PRODUCTION_EVENT_ROOT_CREATE_FAILED")
	}

	streamID := uuid.New()
	eventID := uuid.New()
	syncResult, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		Principal:               principal,
		ProfileRef:              profileRef,
		SessionID:               root.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: model.SHA256Hex([]byte("successor-production-event-sync-" + suffix)),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    root.Session.StateVersion,
		Events: []model.StudyEvent{{
			ID:                 eventID,
			ClientSequence:     1,
			ProfileRef:         profileRef,
			StudySessionItemID: root.Items[0].ID,
			CardIdentity:       root.Items[0].CardIdentity,
			ContentSHA256:      root.Items[0].SourceContentSHA256,
			Kind:               model.StudyEventKindPresented,
			ClientClockQuality: model.StudyClientClockQualityUnknown,
		}},
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.PRODUCTION_EVENT_ADJUDICATION_FAILED", err))
	}
	if syncResult == nil || len(syncResult.Receipts) != 1 ||
		syncResult.Receipts[0].Disposition != model.StudyEventReceiptDispositionAccepted ||
		syncResult.Receipts[0].CanonicalStudyEventID == nil ||
		*syncResult.Receipts[0].CanonicalStudyEventID != eventID ||
		syncResult.Receipts[0].CanonicalProfileIngestionOrdinal == nil ||
		*syncResult.Receipts[0].CanonicalProfileIngestionOrdinal != 1 ||
		syncResult.Receipts[0].OriginatingStudyFlushReceiptID != syncResult.FlushReceipt.ID ||
		syncResult.FlushReceipt.FirstClientSequence == nil ||
		*syncResult.FlushReceipt.FirstClientSequence != 1 ||
		syncResult.FlushReceipt.LastClientSequence == nil ||
		*syncResult.FlushReceipt.LastClientSequence != 1 ||
		syncResult.FlushReceipt.AcknowledgedThroughSequence != 1 ||
		syncResult.FlushReceipt.ProfileIngestionHighWater != 1 ||
		syncResult.FlushReceipt.StudySessionStateVersion != 2 ||
		syncResult.Session.StateVersion != 2 {
		t.Fatal("STUDY_SUCCESSOR.PRODUCTION_EVENT_BARRIER_INVALID")
	}

	predecessorID := root.Session.ID
	successorInput := StudySuccessorPlanInsert{
		Plan: testStudyPlan(
			uuid.New(),
			uuid.New(),
			principal,
			idea.ID,
			syncResult.FlushReceipt.ProfileIngestionHighWater,
			"successor-production-event-child",
			&predecessorID,
		),
		PredecessorFlushReceiptID:     syncResult.FlushReceipt.ID,
		PredecessorTerminalReasonCode: "cycle_exhausted",
	}
	var successor *StudyPlanInsertResult
	err = repository.WithStudyScopeTransaction(
		ctx,
		StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope},
		func(store StudyStoreTransaction) error {
			if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
				return listErr
			}
			var insertErr error
			successor, insertErr = store.InsertSuccessorPlanOnce(ctx, successorInput)
			return insertErr
		},
	)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.PRODUCTION_EVENT_SUCCESSOR_FAILED", err))
	}
	if successor == nil || !successor.Created || successor.Session.Lifecycle != model.StudySessionLifecycleActive ||
		successor.Session.PredecessorStudySessionID == nil ||
		*successor.Session.PredecessorStudySessionID != predecessorID {
		t.Fatal("STUDY_SUCCESSOR.PRODUCTION_EVENT_SUCCESSOR_INVALID")
	}
	predecessor, err := repository.GetStudySession(ctx, profileRef, predecessorID)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.PRODUCTION_EVENT_PREDECESSOR_READ_FAILED", err))
	}
	if predecessor.Lifecycle != model.StudySessionLifecycleCompleted ||
		predecessor.StateVersion != 3 ||
		predecessor.SuccessorStudySessionID == nil ||
		*predecessor.SuccessorStudySessionID != successor.Session.ID ||
		predecessor.TerminalProfileIngestionHighWater == nil ||
		*predecessor.TerminalProfileIngestionHighWater != 1 {
		t.Fatal("STUDY_SUCCESSOR.PRODUCTION_EVENT_PREDECESSOR_INVALID")
	}
}

func testStudySuccessorProductionMultiItemExhaustion(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
) {
	t.Helper()
	fixture := newStudySuccessorProductionFixture(t, ctx, repository, "multi-item", 2)
	streamID := uuid.New()
	first, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Root.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studySuccessorProductionHash("multi-item-first"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    fixture.Root.Session.StateVersion,
		Events: []model.StudyEvent{
			studySuccessorProductionEvent(fixture, fixture.Root.Items[0], streamID, 1, uuid.New(), model.StudyEventKindPresented, nil),
		},
	})
	if err != nil || first == nil || len(first.Receipts) != 1 ||
		first.Receipts[0].Disposition != model.StudyEventReceiptDispositionAccepted ||
		first.FlushReceipt.ProfileIngestionHighWater != 1 || first.Session.StateVersion != 2 {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.MULTI_ITEM.FIRST_PRESENTED_FAILED", err))
	}

	incompleteInput := studySuccessorProductionInput(
		fixture,
		first.FlushReceipt.ProfileIngestionHighWater,
		first.FlushReceipt.ID,
		"multi-item-incomplete",
	)
	result, err := insertStudySuccessorProductionPlan(ctx, repository, fixture, incompleteInput)
	if result != nil {
		t.Fatal("STUDY_SUCCESSOR.MULTI_ITEM.INCOMPLETE_CREATED_SUCCESSOR")
	}
	assertStudySuccessorError(t, err, model.StudyErrorCodeCycleNotExhausted)
	predecessor, err := repository.GetStudySession(ctx, fixture.ProfileRef, fixture.Root.Session.ID)
	if err != nil || predecessor.Lifecycle != model.StudySessionLifecycleActive ||
		predecessor.StateVersion != first.Session.StateVersion || predecessor.SuccessorStudySessionID != nil {
		t.Fatal("STUDY_SUCCESSOR.MULTI_ITEM.INCOMPLETE_MUTATED_PREDECESSOR")
	}

	second, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Root.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studySuccessorProductionHash("multi-item-second"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    first.Session.StateVersion,
		Events: []model.StudyEvent{
			studySuccessorProductionEvent(fixture, fixture.Root.Items[1], streamID, 2, uuid.New(), model.StudyEventKindPresented, nil),
		},
	})
	if err != nil || second == nil || len(second.Receipts) != 1 ||
		second.Receipts[0].Disposition != model.StudyEventReceiptDispositionAccepted ||
		second.FlushReceipt.ProfileIngestionHighWater != 2 || second.Session.StateVersion != 3 {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.MULTI_ITEM.SECOND_PRESENTED_FAILED", err))
	}

	completeInput := studySuccessorProductionInput(
		fixture,
		second.FlushReceipt.ProfileIngestionHighWater,
		second.FlushReceipt.ID,
		"multi-item-complete",
	)
	successor, err := insertStudySuccessorProductionPlan(ctx, repository, fixture, completeInput)
	if err != nil || successor == nil || !successor.Created || len(successor.Items) != 2 {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.MULTI_ITEM.SUCCESSOR_CREATE_FAILED", err))
	}
	predecessor, err = repository.GetStudySession(ctx, fixture.ProfileRef, fixture.Root.Session.ID)
	if err != nil || predecessor.Lifecycle != model.StudySessionLifecycleCompleted ||
		predecessor.StateVersion != 4 || predecessor.TerminalProfileIngestionHighWater == nil ||
		*predecessor.TerminalProfileIngestionHighWater != 2 || predecessor.SuccessorStudySessionID == nil ||
		*predecessor.SuccessorStudySessionID != successor.Session.ID {
		t.Fatal("STUDY_SUCCESSOR.MULTI_ITEM.COMPLETED_PREDECESSOR_INVALID")
	}
}

func testStudySuccessorProductionTerminalCollateral(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	fixture := newStudySuccessorProductionFixture(t, ctx, repository, "terminal-collateral", 1)
	streamID := uuid.New()
	rating := model.StudyRatingGood
	terminal, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Root.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studySuccessorProductionHash("terminal-collateral"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    fixture.Root.Session.StateVersion,
		Events: []model.StudyEvent{
			studySuccessorProductionEvent(fixture, fixture.Root.Items[0], streamID, 1, uuid.New(), model.StudyEventKindPresented, nil),
			studySuccessorProductionEvent(fixture, fixture.Root.Items[0], streamID, 2, uuid.New(), model.StudyEventKindRated, &rating),
		},
	})
	if err != nil || terminal == nil || len(terminal.Receipts) != 2 ||
		terminal.FlushReceipt.ProfileIngestionHighWater != 0 || terminal.Session.StateVersion != 2 {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.TERMINAL_COLLATERAL.ADJUDICATION_FAILED", err))
	}
	for index := range terminal.Receipts {
		receipt := terminal.Receipts[index]
		if receipt.Disposition != model.StudyEventReceiptDispositionTerminalRejected ||
			receipt.Code != model.StudyEventReceiptCodeTransitionInvalid ||
			receipt.CanonicalStudyEventID != nil || receipt.CanonicalProfileIngestionOrdinal != nil ||
			receipt.TerminalFailureClientSequence == nil || *receipt.TerminalFailureClientSequence != 2 ||
			receipt.OriginatingStudyFlushReceiptID != terminal.FlushReceipt.ID {
			t.Fatal("STUDY_SUCCESSOR.TERMINAL_COLLATERAL.RECEIPT_INVALID")
		}
	}

	input := studySuccessorProductionInput(
		fixture,
		terminal.FlushReceipt.ProfileIngestionHighWater,
		terminal.FlushReceipt.ID,
		"terminal-collateral-successor",
	)
	result, err := insertStudySuccessorProductionPlan(ctx, repository, fixture, input)
	if result != nil {
		t.Fatal("STUDY_SUCCESSOR.TERMINAL_COLLATERAL.CREATED_SUCCESSOR")
	}
	assertStudySuccessorError(t, err, model.StudyErrorCodeCycleNotExhausted)

	var canonicalPresentedCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM study_events
		WHERE study_session_id = $1
		  AND study_event_kind = 'presented'`, fixture.Root.Session.ID).Scan(&canonicalPresentedCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.TERMINAL_COLLATERAL.CANONICAL_READ_FAILED", err))
	}
	predecessor, readErr := repository.GetStudySession(ctx, fixture.ProfileRef, fixture.Root.Session.ID)
	if canonicalPresentedCount != 0 || readErr != nil ||
		predecessor.Lifecycle != model.StudySessionLifecycleActive || predecessor.StateVersion != 2 ||
		predecessor.SuccessorStudySessionID != nil {
		t.Fatal("STUDY_SUCCESSOR.TERMINAL_COLLATERAL.BARRIER_MUTATED_STATE")
	}
}

func testStudySuccessorProductionCrossProfileStream(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	owner := newStudySuccessorProductionFixture(t, ctx, repository, "stream-owner", 1)
	other := newStudySuccessorProductionFixture(t, ctx, repository, "stream-other", 1)
	streamID := uuid.New()
	owned, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		Principal:               owner.Principal,
		ProfileRef:              owner.ProfileRef,
		SessionID:               owner.Root.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studySuccessorProductionHash("stream-owner"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    owner.Root.Session.StateVersion,
		Events: []model.StudyEvent{
			studySuccessorProductionEvent(owner, owner.Root.Items[0], streamID, 1, uuid.New(), model.StudyEventKindPresented, nil),
		},
	})
	if err != nil || owned == nil || owned.FlushReceipt.AcknowledgedThroughSequence != 1 {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.CROSS_PROFILE_STREAM.OWNER_SYNC_FAILED", err))
	}

	denied, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		Principal:               other.Principal,
		ProfileRef:              other.ProfileRef,
		SessionID:               other.Root.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studySuccessorProductionHash("stream-other"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    other.Root.Session.StateVersion,
		Events: []model.StudyEvent{
			studySuccessorProductionEvent(other, other.Root.Items[0], streamID, 2, uuid.New(), model.StudyEventKindPresented, nil),
		},
	})
	if denied != nil {
		t.Fatal("STUDY_SUCCESSOR.CROSS_PROFILE_STREAM.DENIED_SYNC_RETURNED_RESULT")
	}
	assertStudySuccessorError(t, err, model.StudyErrorCodeAuthorizationDenied)

	otherSession, readErr := repository.GetStudySession(ctx, other.ProfileRef, other.Root.Session.ID)
	var streamCursor int64
	var deniedReceiptCount int
	if queryErr := pool.QueryRow(ctx, `
		SELECT
			(SELECT adjudicated_through_client_sequence FROM study_client_streams WHERE client_stream_id = $1),
			(SELECT count(*) FROM study_event_receipts WHERE client_stream_id = $1 AND client_sequence = 2)`,
		streamID,
	).Scan(&streamCursor, &deniedReceiptCount); queryErr != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.CROSS_PROFILE_STREAM.STATE_READ_FAILED", queryErr))
	}
	if readErr != nil || otherSession.StateVersion != other.Root.Session.StateVersion ||
		otherSession.ProfileIngestionHighWater != 0 || streamCursor != 1 || deniedReceiptCount != 0 {
		t.Fatal("STUDY_SUCCESSOR.CROSS_PROFILE_STREAM.DENIAL_PERSISTED_STATE")
	}
}

func testStudySuccessorCorrectionSerialization(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	t.Run("correction commits before successor", func(t *testing.T) {
		testStudySuccessorCorrectionFirst(t, ctx, pool, repository)
	})
	t.Run("successor commits before correction", func(t *testing.T) {
		testStudySuccessorSuccessorFirst(t, ctx, pool, repository)
	})
}

func testStudySuccessorCorrectionFirst(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	fixture, initial, correctionInput := newStudySuccessorCorrectionFixture(
		t,
		ctx,
		repository,
		"correction-first",
	)
	staleSuccessorInput := studySuccessorProductionInput(
		fixture,
		initial.FlushReceipt.ProfileIngestionHighWater,
		initial.FlushReceipt.ID,
		"correction-first-stale-successor",
	)

	racingContext, cancelRacing := context.WithTimeout(ctx, 15*time.Second)
	releaseCorrection := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCorrection) }) }
	correctionAtCommit := make(chan int32, 1)
	correctionDone := make(chan struct{})
	correctionOutcomes := make(chan studySuccessorEventOutcome, 1)
	successorDone := make(chan struct{})
	successorOutcomes := make(chan studySuccessorInsertOutcome, 1)
	successorStarted := false

	correctionRepository := NewRepository(pool)
	correctionRepository.studyEventBeforeCommit = func(hookContext context.Context, tx pgx.Tx) error {
		var backendPID int32
		if err := tx.QueryRow(hookContext, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
			return err
		}
		select {
		case correctionAtCommit <- backendPID:
		case <-hookContext.Done():
			return hookContext.Err()
		}
		select {
		case <-releaseCorrection:
			return nil
		case <-hookContext.Done():
			return hookContext.Err()
		}
	}
	successorRepository, successorApplicationName, successorPool := newAdversarialWorkerRepository(
		t,
		racingContext,
		pool,
		"study_successor_correction_first",
	)
	defer func() {
		cancelRacing()
		release()
		waitForStudySuccessorWorkerCleanup(t, correctionDone, "STUDY_SUCCESSOR.CORRECTION_FIRST.CORRECTION_CLEANUP_TIMEOUT")
		if successorStarted {
			waitForStudySuccessorWorkerCleanup(t, successorDone, "STUDY_SUCCESSOR.CORRECTION_FIRST.SUCCESSOR_CLEANUP_TIMEOUT")
		}
		correctionRepository.studyEventBeforeCommit = nil
		successorPool.Close()
	}()

	go func() {
		defer close(correctionDone)
		result, err := correctionRepository.AdjudicateStudyEvents(racingContext, correctionInput)
		correctionOutcomes <- studySuccessorEventOutcome{result: result, err: err}
	}()
	var correctionPID int32
	select {
	case correctionPID = <-correctionAtCommit:
	case <-correctionDone:
		outcome := <-correctionOutcomes
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.CORRECTION_FIRST.CORRECTION_DID_NOT_REACH_BARRIER", outcome.err))
	case <-racingContext.Done():
		t.Fatal("STUDY_SUCCESSOR.CORRECTION_FIRST.CORRECTION_BARRIER_TIMEOUT")
	}

	successorStarted = true
	go func() {
		defer close(successorDone)
		result, err := insertStudySuccessorProductionPlan(
			racingContext,
			successorRepository,
			fixture,
			staleSuccessorInput,
		)
		successorOutcomes <- studySuccessorInsertOutcome{result: result, err: err}
	}()
	waitContext, cancelWait := context.WithTimeout(racingContext, 10*time.Second)
	waiterPID, waitErr := waitForStudySuccessorPostgresBlock(
		waitContext,
		pool,
		successorApplicationName,
		correctionPID,
		successorDone,
	)
	cancelWait()
	if waitErr != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.CORRECTION_FIRST.SUCCESSOR_NOT_BLOCKED", waitErr))
	}
	if waiterPID == correctionPID {
		t.Fatal("STUDY_SUCCESSOR.CORRECTION_FIRST.BACKEND_IDENTITIES_COLLAPSED")
	}
	release()
	if err := waitForStudySuccessorWorker(racingContext, correctionDone); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.CORRECTION_FIRST.CORRECTION_JOIN_FAILED", err))
	}
	if err := waitForStudySuccessorWorker(racingContext, successorDone); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.CORRECTION_FIRST.SUCCESSOR_JOIN_FAILED", err))
	}
	correctionRepository.studyEventBeforeCommit = nil
	correctionOutcome := <-correctionOutcomes
	if correctionOutcome.err != nil || correctionOutcome.result == nil ||
		len(correctionOutcome.result.Receipts) != 1 ||
		correctionOutcome.result.Receipts[0].Disposition != model.StudyEventReceiptDispositionAccepted ||
		correctionOutcome.result.FlushReceipt.ProfileIngestionHighWater != 4 ||
		correctionOutcome.result.Session.StateVersion != 3 {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.CORRECTION_FIRST.CORRECTION_FAILED", correctionOutcome.err))
	}
	successorOutcome := <-successorOutcomes
	if successorOutcome.result != nil {
		t.Fatal("STUDY_SUCCESSOR.CORRECTION_FIRST.STALE_SUCCESSOR_RETURNED_RESULT")
	}
	assertStudySuccessorError(t, successorOutcome.err, model.StudyErrorCodeFlushReceiptStale)

	retryInput := studySuccessorProductionInput(
		fixture,
		correctionOutcome.result.FlushReceipt.ProfileIngestionHighWater,
		correctionOutcome.result.FlushReceipt.ID,
		"correction-first-current-successor",
	)
	successor, err := insertStudySuccessorProductionPlan(ctx, repository, fixture, retryInput)
	if err != nil || successor == nil || !successor.Created {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.CORRECTION_FIRST.RETRY_FAILED", err))
	}
	assertStudySuccessorPersistentSnapshot(t, ctx, repository, fixture, successor, 4, 4, 4)
}

func testStudySuccessorSuccessorFirst(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	fixture, initial, correctionInput := newStudySuccessorCorrectionFixture(
		t,
		ctx,
		repository,
		"successor-first",
	)
	successorInput := studySuccessorProductionInput(
		fixture,
		initial.FlushReceipt.ProfileIngestionHighWater,
		initial.FlushReceipt.ID,
		"successor-first-successor",
	)

	racingContext, cancelRacing := context.WithTimeout(ctx, 15*time.Second)
	releaseSuccessor := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSuccessor) }) }
	successorHasLock := make(chan int32, 1)
	successorDone := make(chan struct{})
	successorOutcomes := make(chan studySuccessorInsertOutcome, 1)
	correctionDone := make(chan struct{})
	correctionOutcomes := make(chan studySuccessorEventOutcome, 1)
	correctionStarted := false

	correctionRepository, correctionApplicationName, correctionPool := newAdversarialWorkerRepository(
		t,
		racingContext,
		pool,
		"study_successor_successor_first",
	)
	defer func() {
		cancelRacing()
		release()
		waitForStudySuccessorWorkerCleanup(t, successorDone, "STUDY_SUCCESSOR.SUCCESSOR_FIRST.SUCCESSOR_CLEANUP_TIMEOUT")
		if correctionStarted {
			waitForStudySuccessorWorkerCleanup(t, correctionDone, "STUDY_SUCCESSOR.SUCCESSOR_FIRST.CORRECTION_CLEANUP_TIMEOUT")
		}
		correctionPool.Close()
	}()

	go func() {
		defer close(successorDone)
		var result *StudyPlanInsertResult
		err := repository.WithStudyScopeTransaction(
			racingContext,
			StudyScopeTransactionInput{ProfileRef: fixture.ProfileRef, Scope: fixture.Scope},
			func(store StudyStoreTransaction) error {
				concrete, ok := store.(*studyStoreTx)
				if !ok {
					return errors.New("STUDY_SUCCESSOR.SUCCESSOR_FIRST.STORE_TYPE_INVALID")
				}
				var backendPID int32
				if queryErr := concrete.tx.QueryRow(racingContext, `SELECT pg_backend_pid()`).Scan(&backendPID); queryErr != nil {
					return queryErr
				}
				select {
				case successorHasLock <- backendPID:
				case <-racingContext.Done():
					return racingContext.Err()
				}
				select {
				case <-releaseSuccessor:
				case <-racingContext.Done():
					return racingContext.Err()
				}
				if _, listErr := store.ListEligibleSources(racingContext); listErr != nil {
					return listErr
				}
				var insertErr error
				result, insertErr = store.InsertSuccessorPlanOnce(racingContext, successorInput)
				return insertErr
			},
		)
		successorOutcomes <- studySuccessorInsertOutcome{result: result, err: err}
	}()
	var successorPID int32
	select {
	case successorPID = <-successorHasLock:
	case <-successorDone:
		outcome := <-successorOutcomes
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.SUCCESSOR_FIRST.SUCCESSOR_DID_NOT_REACH_BARRIER", outcome.err))
	case <-racingContext.Done():
		t.Fatal("STUDY_SUCCESSOR.SUCCESSOR_FIRST.SUCCESSOR_BARRIER_TIMEOUT")
	}

	correctionStarted = true
	go func() {
		defer close(correctionDone)
		result, err := correctionRepository.AdjudicateStudyEvents(racingContext, correctionInput)
		correctionOutcomes <- studySuccessorEventOutcome{result: result, err: err}
	}()
	waitContext, cancelWait := context.WithTimeout(racingContext, 10*time.Second)
	waiterPID, waitErr := waitForStudySuccessorPostgresBlock(
		waitContext,
		pool,
		correctionApplicationName,
		successorPID,
		correctionDone,
	)
	cancelWait()
	if waitErr != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.SUCCESSOR_FIRST.CORRECTION_NOT_BLOCKED", waitErr))
	}
	if waiterPID == successorPID {
		t.Fatal("STUDY_SUCCESSOR.SUCCESSOR_FIRST.BACKEND_IDENTITIES_COLLAPSED")
	}
	release()
	if err := waitForStudySuccessorWorker(racingContext, successorDone); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.SUCCESSOR_FIRST.SUCCESSOR_JOIN_FAILED", err))
	}
	if err := waitForStudySuccessorWorker(racingContext, correctionDone); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.SUCCESSOR_FIRST.CORRECTION_JOIN_FAILED", err))
	}
	successorOutcome := <-successorOutcomes
	if successorOutcome.err != nil || successorOutcome.result == nil || !successorOutcome.result.Created {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.SUCCESSOR_FIRST.SUCCESSOR_FAILED", successorOutcome.err))
	}
	correctionOutcome := <-correctionOutcomes
	if correctionOutcome.result != nil {
		t.Fatal("STUDY_SUCCESSOR.SUCCESSOR_FIRST.STALE_CORRECTION_RETURNED_RESULT")
	}
	assertStudySuccessorError(t, correctionOutcome.err, model.StudyErrorCodeStateStale)
	completedBeforeCorrection, err := repository.GetStudySession(ctx, fixture.ProfileRef, fixture.Root.Session.ID)
	if err != nil || completedBeforeCorrection.Lifecycle != model.StudySessionLifecycleCompleted ||
		completedBeforeCorrection.StateVersion != 3 ||
		completedBeforeCorrection.TerminalProfileIngestionHighWater == nil ||
		*completedBeforeCorrection.TerminalProfileIngestionHighWater != 3 ||
		completedBeforeCorrection.SuccessorStudySessionID == nil ||
		*completedBeforeCorrection.SuccessorStudySessionID != successorOutcome.result.Session.ID {
		t.Fatal("STUDY_SUCCESSOR.SUCCESSOR_FIRST.PRE_CORRECTION_TERMINAL_SNAPSHOT_INVALID")
	}

	retryCorrection := correctionInput
	retryCorrection.SyncRequestID = uuid.New()
	retryCorrection.NormalizedRequestSHA256 = studySuccessorProductionHash("successor-first-correction-retry")
	retryCorrection.ExpectedStateVersion = completedBeforeCorrection.StateVersion
	corrected, err := repository.AdjudicateStudyEvents(ctx, retryCorrection)
	if err != nil || corrected == nil || len(corrected.Receipts) != 1 ||
		corrected.Receipts[0].Disposition != model.StudyEventReceiptDispositionAccepted ||
		corrected.FlushReceipt.ProfileIngestionHighWater != 4 || corrected.Session.StateVersion != 4 {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.SUCCESSOR_FIRST.CORRECTION_RETRY_FAILED", err))
	}
	assertStudySuccessorPersistentSnapshot(t, ctx, repository, fixture, successorOutcome.result, 3, 4, 4)
}

func testStudySuccessorActiveConvergence(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	fixture := newStudySuccessorActiveFixture(t, ctx, pool, repository, "converge", true)
	principal := model.OperatorStudyPrincipalID()
	predecessorID := fixture.Root.Session.ID
	plans := []StudySuccessorPlanInsert{
		{
			Plan:                          testStudyPlan(uuid.New(), uuid.New(), principal, fixture.IdeaID, 1, "successor-race-a", &predecessorID),
			PredecessorFlushReceiptID:     fixture.FlushID,
			PredecessorTerminalReasonCode: "cycle_exhausted",
		},
		{
			Plan:                          testStudyPlan(uuid.New(), uuid.New(), principal, fixture.IdeaID, 1, "successor-race-b", &predecessorID),
			PredecessorFlushReceiptID:     fixture.FlushID,
			PredecessorTerminalReasonCode: "cycle_exhausted",
		},
	}

	type outcome struct {
		index  int
		result *StudyPlanInsertResult
		err    error
	}
	contendedOutcomes := runStudySuccessorContendedScopeTransactions(
		t,
		ctx,
		pool,
		"active_converge",
		fixture.ProfileRef,
		fixture.Scope,
		func(operationContext context.Context, store StudyStoreTransaction) (*StudyPlanInsertResult, error) {
			sources, err := store.ListEligibleSources(operationContext)
			if err != nil {
				return nil, err
			}
			if len(sources) != 1 {
				return nil, errors.New("STUDY_SUCCESSOR.CONVERGENCE.CORPUS_INVALID")
			}
			return store.InsertSuccessorPlanOnce(operationContext, plans[0])
		},
		func(operationContext context.Context, store StudyStoreTransaction) (*StudyPlanInsertResult, error) {
			if current := store.NonterminalSession(); current != nil && current.ID == predecessorID {
				if _, err := store.ListEligibleSources(operationContext); err != nil {
					return nil, err
				}
			}
			return store.InsertSuccessorPlanOnce(operationContext, plans[1])
		},
	)
	first := outcome{index: 0, result: contendedOutcomes[0].result, err: contendedOutcomes[0].err}
	second := outcome{index: 1, result: contendedOutcomes[1].result, err: contendedOutcomes[1].err}
	for _, candidate := range []outcome{first, second} {
		if candidate.err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.CONCURRENT_INSERT_FAILED", candidate.err))
		}
		if candidate.result == nil || candidate.result.Receipt.Disposition != model.StudyCreationDispositionSuccessor {
			t.Fatal("STUDY_SUCCESSOR.CONCURRENT_INSERT_RESULT_INVALID")
		}
	}
	if first.result.Session.ID != second.result.Session.ID {
		t.Fatal("STUDY_SUCCESSOR.CONCURRENT_CREATORS_DID_NOT_CONVERGE")
	}
	if first.result.Created == second.result.Created {
		t.Fatal("STUDY_SUCCESSOR.CONCURRENT_CREATION_AUTHORITY_AMBIGUOUS")
	}
	if first.result.Receipt.CreationRequestID == second.result.Receipt.CreationRequestID {
		t.Fatal("STUDY_SUCCESSOR.CONCURRENT_RECEIPTS_COLLAPSED")
	}

	created := first
	if second.result.Created {
		created = second
	}
	createdInput := plans[created.index]
	predecessor, err := repository.GetStudySession(ctx, fixture.ProfileRef, predecessorID)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.PREDECESSOR_READ_FAILED", err))
	}
	if predecessor.Lifecycle != model.StudySessionLifecycleCompleted ||
		predecessor.StateVersion != 3 ||
		predecessor.SuccessorStudySessionID == nil ||
		*predecessor.SuccessorStudySessionID != created.result.Session.ID ||
		predecessor.TerminalProfileIngestionHighWater == nil ||
		*predecessor.TerminalProfileIngestionHighWater != 1 {
		t.Fatal("STUDY_SUCCESSOR.PREDECESSOR_TERMINAL_STATE_INVALID")
	}
	if created.result.Session.PredecessorStudySessionID == nil ||
		*created.result.Session.PredecessorStudySessionID != predecessorID ||
		created.result.Session.Lifecycle != model.StudySessionLifecycleActive ||
		created.result.Session.StateVersion != 1 {
		t.Fatal("STUDY_SUCCESSOR.LINKED_SESSION_STATE_INVALID")
	}

	var predecessorAuditCount, successorAuditCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM study_session_lifecycle_events WHERE study_session_id = $1),
			(SELECT count(*) FROM study_session_lifecycle_events WHERE study_session_id = $2)`,
		predecessorID,
		created.result.Session.ID,
	).Scan(&predecessorAuditCount, &successorAuditCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.AUDIT_COUNT_FAILED", err))
	}
	if predecessorAuditCount != 2 || successorAuditCount != 1 {
		t.Fatal("STUDY_SUCCESSOR.LIFECYCLE_AUDIT_CARDINALITY_INVALID")
	}

	var replay *StudyPlanInsertResult
	err = repository.WithStudyScopeTransaction(
		ctx,
		StudyScopeTransactionInput{ProfileRef: fixture.ProfileRef, Scope: fixture.Scope},
		func(store StudyStoreTransaction) error {
			var replayErr error
			replay, replayErr = store.InsertSuccessorPlanOnce(ctx, createdInput)
			return replayErr
		},
	)
	if err != nil || replay == nil || replay.Created ||
		replay.Session.ID != created.result.Session.ID ||
		replay.Receipt.CreationReceiptAdjudicatedAt != created.result.Receipt.CreationReceiptAdjudicatedAt {
		t.Fatal("STUDY_SUCCESSOR.RECEIPT_REPLAY_WAS_NOT_STABLE")
	}

	conflict := createdInput
	conflict.Plan.NormalizedRequestSHA256 = model.SHA256Hex([]byte("changed-successor-request"))
	err = repository.WithStudyScopeTransaction(
		ctx,
		StudyScopeTransactionInput{ProfileRef: fixture.ProfileRef, Scope: fixture.Scope},
		func(store StudyStoreTransaction) error {
			_, conflictErr := store.InsertSuccessorPlanOnce(ctx, conflict)
			return conflictErr
		},
	)
	assertStudySuccessorError(t, err, model.StudyErrorCodeIdempotencyConflict)
}

func testStudySuccessorActiveBarrierFailures(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	fixture := newStudySuccessorActiveFixture(t, ctx, pool, repository, "barrier", false)
	predecessorID := fixture.Root.Session.ID
	base := StudySuccessorPlanInsert{
		Plan: testStudyPlan(
			uuid.New(),
			uuid.New(),
			model.OperatorStudyPrincipalID(),
			fixture.IdeaID,
			0,
			"successor-barrier",
			&predecessorID,
		),
		PredecessorFlushReceiptID:     fixture.FlushID,
		PredecessorTerminalReasonCode: "cycle_exhausted",
	}

	missing := base
	missing.Plan.CreationRequestID = uuid.New()
	missing.Plan.NormalizedRequestSHA256 = model.SHA256Hex([]byte("successor-missing-flush"))
	missing.PredecessorFlushReceiptID = uuid.Nil
	assertStudySuccessorAttemptError(t, ctx, repository, fixture, missing, false, model.StudyErrorCodeFlushReceiptRequired)

	unknown := base
	unknown.Plan.CreationRequestID = uuid.New()
	unknown.Plan.NormalizedRequestSHA256 = model.SHA256Hex([]byte("successor-unknown-flush"))
	unknown.PredecessorFlushReceiptID = uuid.New()
	assertStudySuccessorAttemptError(t, ctx, repository, fixture, unknown, false, model.StudyErrorCodeFlushReceiptStale)

	unowned := base
	unowned.Plan.CreationRequestID = uuid.New()
	unowned.Plan.NormalizedRequestSHA256 = model.SHA256Hex([]byte("successor-unowned-flush"))
	unownedPrincipal, err := model.NewStudyPrincipalID(model.StudyPrincipalNamespaceFixture, uuid.New())
	if err != nil {
		t.Fatal("STUDY_SUCCESSOR.UNOWNED_PRINCIPAL_FIXTURE_INVALID")
	}
	unowned.Plan.Principal = unownedPrincipal
	assertStudySuccessorAttemptError(t, ctx, repository, fixture, unowned, false, model.StudyErrorCodeFlushReceiptStale)

	crossSessionFixture := newStudySuccessorActiveFixtureForProfile(
		t,
		ctx,
		pool,
		repository,
		"cross-session",
		fixture.ProfileRef,
		0,
		false,
	)
	crossSession := base
	crossSession.Plan.CreationRequestID = uuid.New()
	crossSession.Plan.NormalizedRequestSHA256 = model.SHA256Hex([]byte("successor-cross-session-flush"))
	crossSession.PredecessorFlushReceiptID = crossSessionFixture.FlushID
	assertStudySuccessorAttemptError(t, ctx, repository, fixture, crossSession, false, model.StudyErrorCodeFlushReceiptStale)

	crossProfileFixture := newStudySuccessorActiveFixture(t, ctx, pool, repository, "cross-profile", false)
	crossProfile := base
	crossProfile.Plan.CreationRequestID = uuid.New()
	crossProfile.Plan.NormalizedRequestSHA256 = model.SHA256Hex([]byte("successor-cross-profile-flush"))
	crossProfile.PredecessorFlushReceiptID = crossProfileFixture.FlushID
	assertStudySuccessorAttemptError(t, ctx, repository, fixture, crossProfile, false, model.StudyErrorCodeFlushReceiptStale)

	cycleIncomplete := base
	cycleIncomplete.Plan.CreationRequestID = uuid.New()
	cycleIncomplete.Plan.NormalizedRequestSHA256 = model.SHA256Hex([]byte("successor-cycle-incomplete"))
	assertStudySuccessorAttemptError(t, ctx, repository, fixture, cycleIncomplete, false, model.StudyErrorCodeCycleNotExhausted)

	if _, err := pool.Exec(ctx, `
		UPDATE study_client_streams
		SET adjudicated_through_client_sequence = adjudicated_through_client_sequence + 1
		WHERE client_stream_id = $1`, fixture.StreamID); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.STALE_STREAM_FIXTURE_FAILED", err))
	}
	staleCursor := base
	staleCursor.Plan.CreationRequestID = uuid.New()
	staleCursor.Plan.NormalizedRequestSHA256 = model.SHA256Hex([]byte("successor-stale-cursor"))
	assertStudySuccessorAttemptError(t, ctx, repository, fixture, staleCursor, false, model.StudyErrorCodeFlushReceiptStale)

	predecessor, err := repository.GetStudySession(ctx, fixture.ProfileRef, predecessorID)
	if err != nil || predecessor.Lifecycle != model.StudySessionLifecycleActive || predecessor.SuccessorStudySessionID != nil {
		t.Fatal("STUDY_SUCCESSOR.REJECTED_BARRIER_MUTATED_PREDECESSOR")
	}

	stalePlanFixture := newStudySuccessorActiveFixture(t, ctx, pool, repository, "stale-plan", true)
	stalePlanPredecessorID := stalePlanFixture.Root.Session.ID
	stalePlan := StudySuccessorPlanInsert{
		Plan: testStudyPlan(
			uuid.New(),
			uuid.New(),
			model.OperatorStudyPrincipalID(),
			stalePlanFixture.IdeaID,
			0,
			"successor-stale-plan",
			&stalePlanPredecessorID,
		),
		PredecessorFlushReceiptID:     stalePlanFixture.FlushID,
		PredecessorTerminalReasonCode: "cycle_exhausted",
	}
	assertStudySuccessorAttemptError(t, ctx, repository, stalePlanFixture, stalePlan, true, model.StudyErrorCodeStateStale)

	staleHighWaterFixture := newStudySuccessorActiveFixture(t, ctx, pool, repository, "stale-high-water", true)
	_ = newStudySuccessorActiveFixtureForProfile(
		t,
		ctx,
		pool,
		repository,
		"profile-prefix-advance",
		staleHighWaterFixture.ProfileRef,
		1,
		true,
	)
	staleHighWaterPredecessorID := staleHighWaterFixture.Root.Session.ID
	staleHighWater := StudySuccessorPlanInsert{
		Plan: testStudyPlan(
			uuid.New(),
			uuid.New(),
			model.OperatorStudyPrincipalID(),
			staleHighWaterFixture.IdeaID,
			2,
			"successor-stale-profile-high-water",
			&staleHighWaterPredecessorID,
		),
		PredecessorFlushReceiptID:     staleHighWaterFixture.FlushID,
		PredecessorTerminalReasonCode: "cycle_exhausted",
	}
	assertStudySuccessorAttemptError(
		t,
		ctx,
		repository,
		staleHighWaterFixture,
		staleHighWater,
		false,
		model.StudyErrorCodeFlushReceiptStale,
	)
}

func testStudySuccessorWaitingReplacement(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	suffix := stringsForStudyFixture(uuid.NewString())
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{
		Slug: "successor-waiting-" + suffix,
		Name: "Successor waiting " + suffix,
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.WAITING_TOPIC_CREATE_FAILED", err))
	}
	scope := mustStudyScope(t, topic.ID, topic.Slug)
	profileRef := "successor-waiting-" + suffix
	principal := model.OperatorStudyPrincipalID()
	waitingPlan := studySuccessorWaitingPlan(uuid.New(), uuid.New(), principal, "waiting-root", nil)
	var waiting *StudyPlanInsertResult
	err = repository.WithStudyScopeTransaction(
		ctx,
		StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope},
		func(store StudyStoreTransaction) error {
			if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
				return listErr
			}
			var insertErr error
			waiting, insertErr = store.InsertPlanOnce(ctx, waitingPlan)
			return insertErr
		},
	)
	if err != nil || waiting == nil || waiting.Session.Lifecycle != model.StudySessionLifecycleWaitingForCorpus {
		t.Fatal("STUDY_SUCCESSOR.WAITING_SENTINEL_CREATE_FAILED")
	}

	waitingID := waiting.Session.ID
	unchanged := StudySuccessorPlanInsert{
		Plan:                          studySuccessorWaitingPlan(uuid.New(), uuid.New(), principal, "waiting-unchanged", &waitingID),
		PredecessorTerminalReasonCode: "eligible_corpus_available",
	}
	var unchangedResult *StudyPlanInsertResult
	err = repository.WithStudyScopeTransaction(
		ctx,
		StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope},
		func(store StudyStoreTransaction) error {
			if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
				return listErr
			}
			var insertErr error
			unchangedResult, insertErr = store.InsertSuccessorPlanOnce(ctx, unchanged)
			return insertErr
		},
	)
	if err != nil || unchangedResult == nil || unchangedResult.Created ||
		unchangedResult.Session.ID != waitingID ||
		unchangedResult.Receipt.Disposition != model.StudyCreationDispositionConvergedToExisting {
		t.Fatal("STUDY_SUCCESSOR.UNCHANGED_EMPTY_SENTINEL_DID_NOT_CONVERGE")
	}
	if successor, readErr := readStudySuccessorForTest(ctx, pool, waitingID); readErr != nil || successor != uuid.Nil {
		t.Fatal("STUDY_SUCCESSOR.UNCHANGED_EMPTY_SENTINEL_CREATED_SUCCESSOR")
	}

	flushRejected := unchanged
	flushRejected.Plan.CreationRequestID = uuid.New()
	flushRejected.Plan.NormalizedRequestSHA256 = model.SHA256Hex([]byte("waiting-flush-rejected"))
	flushRejected.PredecessorFlushReceiptID = uuid.New()
	assertStudySuccessorAttemptError(
		t,
		ctx,
		repository,
		studySuccessorTestFixture{ProfileRef: profileRef, Scope: scope},
		flushRejected,
		true,
		model.StudyErrorCodeSchemaInvalid,
	)

	idea := mustCreateStudyIdea(t, ctx, repository, topic.Slug, "waiting-now-nonempty")
	activeInputs := []StudySuccessorPlanInsert{
		{
			Plan:                          testStudyPlan(uuid.New(), uuid.New(), principal, idea.ID, 0, "waiting-successor-a", &waitingID),
			PredecessorTerminalReasonCode: "eligible_corpus_available",
		},
		{
			Plan:                          testStudyPlan(uuid.New(), uuid.New(), principal, idea.ID, 0, "waiting-successor-b", &waitingID),
			PredecessorTerminalReasonCode: "eligible_corpus_available",
		},
	}
	contendedOutcomes := runStudySuccessorContendedScopeTransactions(
		t,
		ctx,
		pool,
		"waiting_replace",
		profileRef,
		scope,
		func(operationContext context.Context, store StudyStoreTransaction) (*StudyPlanInsertResult, error) {
			sources, err := store.ListEligibleSources(operationContext)
			if err != nil {
				return nil, err
			}
			if len(sources) != 1 {
				return nil, errors.New("STUDY_SUCCESSOR.WAITING_REPLACEMENT_CORPUS_INVALID")
			}
			return store.InsertSuccessorPlanOnce(operationContext, activeInputs[0])
		},
		func(operationContext context.Context, store StudyStoreTransaction) (*StudyPlanInsertResult, error) {
			if current := store.NonterminalSession(); current != nil && current.ID == waitingID {
				sources, err := store.ListEligibleSources(operationContext)
				if err != nil {
					return nil, err
				}
				if len(sources) != 1 {
					return nil, errors.New("STUDY_SUCCESSOR.WAITING_REPLACEMENT_CORPUS_INVALID")
				}
			}
			return store.InsertSuccessorPlanOnce(operationContext, activeInputs[1])
		},
	)
	firstReplacement := contendedOutcomes[0]
	secondReplacement := contendedOutcomes[1]
	for _, outcome := range []studySuccessorInsertOutcome{firstReplacement, secondReplacement} {
		if outcome.err != nil || outcome.result == nil ||
			outcome.result.Receipt.Disposition != model.StudyCreationDispositionSuccessor {
			t.Fatal("STUDY_SUCCESSOR.CONCURRENT_WAITING_REPLACEMENT_FAILED")
		}
	}
	if firstReplacement.result.Session.ID != secondReplacement.result.Session.ID ||
		firstReplacement.result.Created == secondReplacement.result.Created ||
		firstReplacement.result.Receipt.CreationRequestID == secondReplacement.result.Receipt.CreationRequestID {
		t.Fatal("STUDY_SUCCESSOR.CONCURRENT_WAITING_REPLACEMENT_DID_NOT_CONVERGE")
	}
	activeResult := firstReplacement.result
	if secondReplacement.result.Created {
		activeResult = secondReplacement.result
	}
	if activeResult.Session.Lifecycle != model.StudySessionLifecycleActive ||
		activeResult.Session.PredecessorStudySessionID == nil ||
		*activeResult.Session.PredecessorStudySessionID != waitingID {
		t.Fatal("STUDY_SUCCESSOR.WAITING_REPLACEMENT_CREATE_FAILED")
	}
	var replacementReceiptCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM study_session_creation_receipts
		WHERE creation_request_id = ANY($1::uuid[])
		  AND study_session_id = $2`,
		[]uuid.UUID{activeInputs[0].Plan.CreationRequestID, activeInputs[1].Plan.CreationRequestID},
		activeResult.Session.ID,
	).Scan(&replacementReceiptCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.WAITING_RECEIPT_COUNT_FAILED", err))
	}
	if replacementReceiptCount != 2 {
		t.Fatal("STUDY_SUCCESSOR.CONCURRENT_WAITING_REPLACEMENT_RECEIPTS_MISSING")
	}
	waitingAfter, err := repository.GetStudySession(ctx, profileRef, waitingID)
	if err != nil || waitingAfter.Lifecycle != model.StudySessionLifecycleSupersededEmpty ||
		waitingAfter.TerminalProfileIngestionHighWater == nil ||
		*waitingAfter.TerminalProfileIngestionHighWater != 0 ||
		waitingAfter.SuccessorStudySessionID == nil ||
		*waitingAfter.SuccessorStudySessionID != activeResult.Session.ID {
		t.Fatal("STUDY_SUCCESSOR.WAITING_SENTINEL_TERMINAL_STATE_INVALID")
	}
}

func testStudySuccessorRollback(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	fixture := newStudySuccessorActiveFixture(t, ctx, pool, repository, "rollback", true)
	predecessorID := fixture.Root.Session.ID
	input := StudySuccessorPlanInsert{
		Plan: testStudyPlan(
			uuid.New(),
			uuid.New(),
			model.OperatorStudyPrincipalID(),
			fixture.IdeaID,
			1,
			"successor-rollback",
			&predecessorID,
		),
		PredecessorFlushReceiptID:     fixture.FlushID,
		PredecessorTerminalReasonCode: "cycle_exhausted",
	}
	rollbackMarker := errors.New("STUDY_SUCCESSOR.INJECTED_ROLLBACK")
	err := repository.WithStudyScopeTransaction(
		ctx,
		StudyScopeTransactionInput{ProfileRef: fixture.ProfileRef, Scope: fixture.Scope},
		func(store StudyStoreTransaction) error {
			if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
				return listErr
			}
			if _, insertErr := store.InsertSuccessorPlanOnce(ctx, input); insertErr != nil {
				return insertErr
			}
			return rollbackMarker
		},
	)
	if !errors.Is(err, rollbackMarker) {
		t.Fatal("STUDY_SUCCESSOR.INJECTED_ROLLBACK_NOT_PROPAGATED")
	}
	predecessor, err := repository.GetStudySession(ctx, fixture.ProfileRef, predecessorID)
	if err != nil || predecessor.Lifecycle != model.StudySessionLifecycleActive ||
		predecessor.StateVersion != 2 || predecessor.SuccessorStudySessionID != nil ||
		predecessor.TerminalProfileIngestionHighWater != nil {
		t.Fatal("STUDY_SUCCESSOR.ROLLBACK_MUTATED_PREDECESSOR")
	}
	var successorCount, receiptCount, auditCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM study_sessions WHERE study_session_id = $1),
			(SELECT count(*) FROM study_session_creation_receipts WHERE creation_request_id = $2),
			(SELECT count(*) FROM study_session_lifecycle_events WHERE study_session_id = $3)`,
		input.Plan.SessionID,
		input.Plan.CreationRequestID,
		predecessorID,
	).Scan(&successorCount, &receiptCount, &auditCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.ROLLBACK_STATE_READ_FAILED", err))
	}
	if successorCount != 0 || receiptCount != 0 || auditCount != 1 {
		t.Fatal("STUDY_SUCCESSOR.ROLLBACK_PERSISTED_PARTIAL_STATE")
	}
}

func newStudySuccessorProductionFixture(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	label string,
	itemCount int,
) studySuccessorProductionFixture {
	t.Helper()
	if itemCount < 1 {
		t.Fatal("STUDY_SUCCESSOR.PRODUCTION_FIXTURE.ITEM_COUNT_INVALID")
	}
	suffix := stringsForStudyFixture(uuid.NewString())
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{
		Slug: "successor-production-" + label + "-" + suffix,
		Name: "Successor production " + label + " " + suffix,
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.PRODUCTION_FIXTURE.TOPIC_CREATE_FAILED", err))
	}
	ideaIDs := make([]uuid.UUID, itemCount)
	for index := range ideaIDs {
		idea := mustCreateStudyIdea(t, ctx, repository, topic.Slug, label+"-item-"+stringsForStudyFixture(uuid.NewString()))
		ideaIDs[index] = idea.ID
	}
	fixture := studySuccessorProductionFixture{
		ProfileRef: "successor-production-" + label + "-" + suffix,
		Principal:  model.OperatorStudyPrincipalID(),
		Scope:      mustStudyScope(t, topic.ID, topic.Slug),
		IdeaIDs:    ideaIDs,
	}
	plan := studySuccessorPlanForIdeas(
		uuid.New(),
		uuid.New(),
		fixture.Principal,
		ideaIDs,
		0,
		label+"-root-"+suffix,
		nil,
	)
	err = repository.WithStudyScopeTransaction(
		ctx,
		StudyScopeTransactionInput{ProfileRef: fixture.ProfileRef, Scope: fixture.Scope},
		func(store StudyStoreTransaction) error {
			if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
				return listErr
			}
			var insertErr error
			fixture.Root, insertErr = store.InsertPlanOnce(ctx, plan)
			return insertErr
		},
	)
	if err != nil || fixture.Root == nil || !fixture.Root.Created || len(fixture.Root.Items) != itemCount {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.PRODUCTION_FIXTURE.ROOT_CREATE_FAILED", err))
	}
	return fixture
}

func studySuccessorPlanForIdeas(
	sessionID, requestID uuid.UUID,
	principal model.StudyPrincipalID,
	ideaIDs []uuid.UUID,
	highWater int64,
	label string,
	predecessorID *uuid.UUID,
) StudyPlanInsert {
	plan := testStudyPlan(sessionID, requestID, principal, ideaIDs[0], highWater, label, predecessorID)
	for index := 1; index < len(ideaIDs); index++ {
		ideaID := ideaIDs[index]
		item := plan.Items[0]
		item.ID = uuid.New()
		item.Position = index
		item.CardIdentity = ideaID
		item.HistoricalSourceIdentity = ideaID
		item.LiveSourceIdeaID = &ideaID
		item.SelectionReason.TieBreakerSHA256 = model.SHA256Hex([]byte(label + "-tie-" + ideaID.String()))
		plan.Items = append(plan.Items, item)
	}
	return plan
}

func studySuccessorProductionInput(
	fixture studySuccessorProductionFixture,
	highWater int64,
	flushReceiptID uuid.UUID,
	label string,
) StudySuccessorPlanInsert {
	predecessorID := fixture.Root.Session.ID
	return StudySuccessorPlanInsert{
		Plan: studySuccessorPlanForIdeas(
			uuid.New(),
			uuid.New(),
			fixture.Principal,
			fixture.IdeaIDs,
			highWater,
			label,
			&predecessorID,
		),
		PredecessorFlushReceiptID:     flushReceiptID,
		PredecessorTerminalReasonCode: "cycle_exhausted",
	}
}

func insertStudySuccessorProductionPlan(
	ctx context.Context,
	repository *Repository,
	fixture studySuccessorProductionFixture,
	input StudySuccessorPlanInsert,
) (*StudyPlanInsertResult, error) {
	var result *StudyPlanInsertResult
	err := repository.WithStudyScopeTransaction(
		ctx,
		StudyScopeTransactionInput{ProfileRef: fixture.ProfileRef, Scope: fixture.Scope},
		func(store StudyStoreTransaction) error {
			if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
				return listErr
			}
			var insertErr error
			result, insertErr = store.InsertSuccessorPlanOnce(ctx, input)
			return insertErr
		},
	)
	return result, err
}

func studySuccessorProductionEvent(
	fixture studySuccessorProductionFixture,
	item model.StudySessionItem,
	streamID uuid.UUID,
	sequence int64,
	eventID uuid.UUID,
	kind model.StudyEventKind,
	rating *model.StudyRating,
) model.StudyEvent {
	event := model.StudyEvent{
		ID:                 eventID,
		ClientSequence:     sequence,
		ProfileRef:         fixture.ProfileRef,
		StudySessionItemID: item.ID,
		CardIdentity:       item.CardIdentity,
		ContentSHA256:      item.SourceContentSHA256,
		Kind:               kind,
		ClientClockQuality: model.StudyClientClockQualityUnknown,
		ClientStreamID:     streamID,
	}
	if rating != nil {
		copyOfRating := *rating
		event.Rating = &copyOfRating
	}
	return event
}

func studySuccessorProductionHash(label string) string {
	return model.SHA256Hex([]byte("study-successor-production:" + label))
}

func newStudySuccessorCorrectionFixture(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	label string,
) (studySuccessorProductionFixture, *StudyEventSyncResult, StudyEventSyncInput) {
	t.Helper()
	fixture := newStudySuccessorProductionFixture(t, ctx, repository, label, 1)
	streamID := uuid.New()
	rating := model.StudyRatingGood
	ratedID := uuid.New()
	initial, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Root.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studySuccessorProductionHash(label + "-initial"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    fixture.Root.Session.StateVersion,
		Events: []model.StudyEvent{
			studySuccessorProductionEvent(fixture, fixture.Root.Items[0], streamID, 1, uuid.New(), model.StudyEventKindPresented, nil),
			studySuccessorProductionEvent(fixture, fixture.Root.Items[0], streamID, 2, uuid.New(), model.StudyEventKindRevealed, nil),
			studySuccessorProductionEvent(fixture, fixture.Root.Items[0], streamID, 3, ratedID, model.StudyEventKindRated, &rating),
		},
	})
	if err != nil || initial == nil || len(initial.Receipts) != 3 ||
		initial.Receipts[0].Disposition != model.StudyEventReceiptDispositionAccepted ||
		initial.Receipts[1].Disposition != model.StudyEventReceiptDispositionAccepted ||
		initial.Receipts[2].Disposition != model.StudyEventReceiptDispositionAccepted ||
		initial.FlushReceipt.AcknowledgedThroughSequence != 3 ||
		initial.FlushReceipt.ProfileIngestionHighWater != 3 || initial.Session.StateVersion != 2 {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.CORRECTION_RACE.INITIAL_SYNC_FAILED", err))
	}
	correctionEvent := studySuccessorProductionEvent(
		fixture,
		fixture.Root.Items[0],
		streamID,
		4,
		uuid.New(),
		model.StudyEventKindRatingVoided,
		nil,
	)
	correctionEvent.RatingEventID = &ratedID
	correctionInput := StudyEventSyncInput{
		Principal:               fixture.Principal,
		ProfileRef:              fixture.ProfileRef,
		SessionID:               fixture.Root.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studySuccessorProductionHash(label + "-correction"),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    initial.Session.StateVersion,
		Events:                  []model.StudyEvent{correctionEvent},
	}
	return fixture, initial, correctionInput
}

func assertStudySuccessorPersistentSnapshot(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	fixture studySuccessorProductionFixture,
	created *StudyPlanInsertResult,
	expectedTerminalHighWater int64,
	expectedPredecessorStateVersion int64,
	expectedProfileHighWater int64,
) {
	t.Helper()
	if created == nil || created.Session.Scheduler == nil ||
		created.Session.Scheduler.InputProfileIngestionHighWater != expectedTerminalHighWater {
		t.Fatal("STUDY_SUCCESSOR.CORRECTION_RACE.CREATED_SNAPSHOT_INVALID")
	}
	predecessor, predecessorErr := repository.GetStudySession(ctx, fixture.ProfileRef, fixture.Root.Session.ID)
	successor, successorErr := repository.GetStudySession(ctx, fixture.ProfileRef, created.Session.ID)
	items, itemsErr := repository.ListStudySessionItems(ctx, fixture.ProfileRef, created.Session.ID)
	profile, profileErr := repository.GetStudyLearnerProfile(ctx, fixture.ProfileRef)
	if predecessorErr != nil || successorErr != nil || itemsErr != nil || profileErr != nil {
		t.Fatal("STUDY_SUCCESSOR.CORRECTION_RACE.SNAPSHOT_READ_FAILED")
	}
	if profile.ProfileIngestionHighWater != expectedProfileHighWater ||
		predecessor.Lifecycle != model.StudySessionLifecycleCompleted ||
		predecessor.StateVersion != expectedPredecessorStateVersion ||
		predecessor.TerminalProfileIngestionHighWater == nil ||
		*predecessor.TerminalProfileIngestionHighWater != expectedTerminalHighWater ||
		predecessor.SuccessorStudySessionID == nil || *predecessor.SuccessorStudySessionID != created.Session.ID {
		t.Fatal("STUDY_SUCCESSOR.CORRECTION_RACE.TERMINAL_SNAPSHOT_CHANGED")
	}
	if successor.ID != created.Session.ID || successor.Lifecycle != model.StudySessionLifecycleActive ||
		successor.StateVersion != created.Session.StateVersion || successor.CorpusSHA256 != created.Session.CorpusSHA256 ||
		successor.PredecessorStudySessionID == nil || *successor.PredecessorStudySessionID != fixture.Root.Session.ID ||
		!reflect.DeepEqual(successor.Scope, created.Session.Scope) ||
		!reflect.DeepEqual(successor.Scheduler, created.Session.Scheduler) ||
		!reflect.DeepEqual(items, created.Items) {
		t.Fatal("STUDY_SUCCESSOR.CORRECTION_RACE.SUCCESSOR_SNAPSHOT_CHANGED")
	}
}

func runStudySuccessorContendedScopeTransactions(
	t *testing.T,
	ctx context.Context,
	observerPool *pgxpool.Pool,
	label string,
	profileRef string,
	scope model.StudyNormalizedScope,
	holderOperation studySuccessorScopedInsertOperation,
	waiterOperation studySuccessorScopedInsertOperation,
) [2]studySuccessorInsertOutcome {
	t.Helper()
	tracingContext, cancelRacing := context.WithTimeout(ctx, 15*time.Second)
	holderRepository, holderApplicationName, holderPool := newAdversarialWorkerRepository(
		t,
		tracingContext,
		observerPool,
		label+"_holder",
	)
	defer holderPool.Close()
	waiterRepository, waiterApplicationName, waiterPool := newAdversarialWorkerRepository(
		t,
		tracingContext,
		observerPool,
		label+"_waiter",
	)
	defer waiterPool.Close()
	if holderApplicationName == waiterApplicationName {
		cancelRacing()
		t.Fatal("STUDY_SUCCESSOR.CONTENTION.APPLICATION_IDENTITIES_COLLAPSED")
	}

	releaseHolder := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseHolder) }) }
	holderHasLock := make(chan int32, 1)
	holderDone := make(chan struct{})
	waiterDone := make(chan struct{})
	holderOutcomes := make(chan studySuccessorInsertOutcome, 1)
	waiterOutcomes := make(chan studySuccessorInsertOutcome, 1)
	holderStarted := false
	waiterStarted := false
	defer func() {
		cancelRacing()
		release()
		if holderStarted {
			waitForStudySuccessorWorkerCleanup(t, holderDone, "STUDY_SUCCESSOR.CONTENTION.HOLDER_CLEANUP_TIMEOUT")
		}
		if waiterStarted {
			waitForStudySuccessorWorkerCleanup(t, waiterDone, "STUDY_SUCCESSOR.CONTENTION.WAITER_CLEANUP_TIMEOUT")
		}
	}()

	holderStarted = true
	go func() {
		defer close(holderDone)
		var result *StudyPlanInsertResult
		err := holderRepository.WithStudyScopeTransaction(
			tracingContext,
			StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope},
			func(store StudyStoreTransaction) error {
				concrete, ok := store.(*studyStoreTx)
				if !ok {
					return errors.New("STUDY_SUCCESSOR.CONTENTION.HOLDER_STORE_TYPE_INVALID")
				}
				var backendPID int32
				if queryErr := concrete.tx.QueryRow(tracingContext, `SELECT pg_backend_pid()`).Scan(&backendPID); queryErr != nil {
					return queryErr
				}
				select {
				case holderHasLock <- backendPID:
				case <-tracingContext.Done():
					return tracingContext.Err()
				}
				select {
				case <-releaseHolder:
				case <-tracingContext.Done():
					return tracingContext.Err()
				}
				var operationErr error
				result, operationErr = holderOperation(tracingContext, store)
				return operationErr
			},
		)
		holderOutcomes <- studySuccessorInsertOutcome{result: result, err: err}
	}()

	var holderPID int32
	select {
	case holderPID = <-holderHasLock:
	case <-holderDone:
		outcome := <-holderOutcomes
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.CONTENTION.HOLDER_DID_NOT_ACQUIRE_LOCK", outcome.err))
	case <-tracingContext.Done():
		t.Fatal("STUDY_SUCCESSOR.CONTENTION.HOLDER_LOCK_TIMEOUT")
	}
	var holderIdentityMatches bool
	if err := observerPool.QueryRow(tracingContext, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid = $1
			  AND application_name = $2
		)`, holderPID, holderApplicationName).Scan(&holderIdentityMatches); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.CONTENTION.HOLDER_IDENTITY_READ_FAILED", err))
	}
	if !holderIdentityMatches {
		t.Fatal("STUDY_SUCCESSOR.CONTENTION.HOLDER_IDENTITY_MISMATCH")
	}

	waiterStarted = true
	go func() {
		defer close(waiterDone)
		var result *StudyPlanInsertResult
		err := waiterRepository.WithStudyScopeTransaction(
			tracingContext,
			StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope},
			func(store StudyStoreTransaction) error {
				var operationErr error
				result, operationErr = waiterOperation(tracingContext, store)
				return operationErr
			},
		)
		waiterOutcomes <- studySuccessorInsertOutcome{result: result, err: err}
	}()
	waitContext, cancelWait := context.WithTimeout(tracingContext, 10*time.Second)
	waiterPID, waitErr := waitForStudySuccessorPostgresBlock(
		waitContext,
		observerPool,
		waiterApplicationName,
		holderPID,
		waiterDone,
	)
	cancelWait()
	if waitErr != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.CONTENTION.WAITER_NOT_BLOCKED", waitErr))
	}
	if waiterPID == 0 || waiterPID == holderPID {
		t.Fatal("STUDY_SUCCESSOR.CONTENTION.BACKEND_IDENTITIES_COLLAPSED")
	}

	release()
	if err := waitForStudySuccessorWorker(tracingContext, holderDone); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.CONTENTION.HOLDER_JOIN_FAILED", err))
	}
	if err := waitForStudySuccessorWorker(tracingContext, waiterDone); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.CONTENTION.WAITER_JOIN_FAILED", err))
	}
	return [2]studySuccessorInsertOutcome{<-holderOutcomes, <-waiterOutcomes}
}

func waitForStudySuccessorPostgresBlock(
	ctx context.Context,
	observerPool *pgxpool.Pool,
	applicationName string,
	blockerPID int32,
	workerDone <-chan struct{},
) (int32, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-workerDone:
			return 0, errors.New("STUDY_SUCCESSOR.WORKER_COMPLETED_BEFORE_LOCK_OBSERVATION")
		default:
		}
		var waiterPID int32
		if err := observerPool.QueryRow(ctx, `
			SELECT COALESCE((
				SELECT pid
				FROM pg_stat_activity
				WHERE datname = current_database()
				  AND application_name = $1
				  AND state = 'active'
				  AND wait_event_type = 'Lock'
				  AND $2 = ANY(pg_blocking_pids(pid))
				ORDER BY pid
				LIMIT 1
			), 0)`, applicationName, blockerPID).Scan(&waiterPID); err != nil {
			return 0, err
		}
		if waiterPID != 0 {
			return waiterPID, nil
		}
		select {
		case <-workerDone:
			return 0, errors.New("STUDY_SUCCESSOR.WORKER_COMPLETED_BEFORE_LOCK_OBSERVATION")
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
		}
	}
}

func waitForStudySuccessorWorker(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitForStudySuccessorWorkerCleanup(t *testing.T, done <-chan struct{}, failure string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Error(failure)
	}
}

func newStudySuccessorActiveFixture(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	label string,
	presented bool,
) studySuccessorTestFixture {
	t.Helper()
	return newStudySuccessorActiveFixtureForProfile(t, ctx, pool, repository, label, "", 0, presented)
}

func newStudySuccessorActiveFixtureForProfile(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	label string,
	profileRef string,
	planningHighWater int64,
	presented bool,
) studySuccessorTestFixture {
	t.Helper()
	suffix := stringsForStudyFixture(uuid.NewString())
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{
		Slug: "successor-" + label + "-" + suffix,
		Name: "Successor " + label + " " + suffix,
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.TOPIC_CREATE_FAILED", err))
	}
	idea := mustCreateStudyIdea(t, ctx, repository, topic.Slug, "successor-"+label)
	scope := mustStudyScope(t, topic.ID, topic.Slug)
	if profileRef == "" {
		profileRef = "successor-" + label + "-" + suffix
	}
	rootPlan := testStudyPlan(
		uuid.New(),
		uuid.New(),
		model.OperatorStudyPrincipalID(),
		idea.ID,
		planningHighWater,
		"successor-root-"+label,
		nil,
	)
	var root *StudyPlanInsertResult
	err = repository.WithStudyScopeTransaction(
		ctx,
		StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope},
		func(store StudyStoreTransaction) error {
			if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
				return listErr
			}
			var insertErr error
			root, insertErr = store.InsertPlanOnce(ctx, rootPlan)
			return insertErr
		},
	)
	if err != nil || root == nil || len(root.Items) != 1 {
		t.Fatal("STUDY_SUCCESSOR.ROOT_CREATE_FAILED")
	}
	flushID, streamID := insertStudySuccessorFlushFixture(t, ctx, pool, *root, presented)
	return studySuccessorTestFixture{
		ProfileRef: profileRef,
		Scope:      scope,
		IdeaID:     idea.ID,
		Root:       root,
		FlushID:    flushID,
		StreamID:   streamID,
	}
}

func insertStudySuccessorFlushFixture(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	root StudyPlanInsertResult,
	presented bool,
) (uuid.UUID, uuid.UUID) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.FLUSH_FIXTURE_BEGIN_FAILED", err))
	}
	defer tx.Rollback(ctx)

	streamID := uuid.New()
	sequence := int64(0)
	profileHighWater := root.Session.ProfileIngestionHighWater
	stateVersion := root.Session.StateVersion
	if presented {
		sequence = 1
		profileHighWater++
		stateVersion++
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO study_client_streams (
			client_stream_id,
			learner_profile_id,
			adjudicated_through_client_sequence
		) VALUES ($1, $2, $3)`, streamID, root.Session.LearnerProfileID, sequence); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.STREAM_FIXTURE_FAILED", err))
	}
	if presented {
		eventID := uuid.New()
		payloadSHA256 := model.SHA256Hex([]byte("study-successor-presented-fixture" + eventID.String()))
		item := root.Items[0]
		if _, err := tx.Exec(ctx, `
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
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1)`,
			eventID,
			payloadSHA256,
			root.Session.LearnerProfileID,
			root.Session.ID,
			item.ID,
			item.CardIdentity,
			item.SourceContentSHA256,
			streamID,
		); err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.EVENT_BINDING_FIXTURE_FAILED", err))
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO study_events (
				study_event_id,
				learner_profile_id,
				study_session_id,
				study_session_item_id,
				study_card_identity,
				study_event_kind,
				source_content_sha256,
				client_stream_id,
				client_sequence,
				normalized_event_payload_sha256,
				client_clock_quality,
				profile_ingestion_ordinal
			) VALUES ($1, $2, $3, $4, $5, 'presented', $6, $7, 1, $8, 'unknown', $9)`,
			eventID,
			root.Session.LearnerProfileID,
			root.Session.ID,
			item.ID,
			item.CardIdentity,
			item.SourceContentSHA256,
			streamID,
			payloadSHA256,
			profileHighWater,
		); err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.PRESENTED_FIXTURE_FAILED", err))
		}
		if _, err := tx.Exec(ctx, `
			UPDATE study_learner_profiles
			SET profile_ingestion_high_water = $2
			WHERE learner_profile_id = $1`, root.Session.LearnerProfileID, profileHighWater); err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.HIGH_WATER_FIXTURE_FAILED", err))
		}
		if _, err := tx.Exec(ctx, `
			UPDATE study_sessions
			SET study_session_state_version = $2
			WHERE study_session_id = $1`, root.Session.ID, stateVersion); err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.STATE_VERSION_FIXTURE_FAILED", err))
		}
	}

	flushID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO study_flush_receipts (
			study_flush_receipt_id,
			study_principal_id,
			learner_profile_id,
			sync_request_id,
			normalized_sync_request_sha256,
			study_session_id,
			client_stream_id,
			acknowledged_through_client_sequence,
			profile_ingestion_high_water,
			study_session_state_version
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		flushID,
		model.OperatorStudyPrincipalID().String(),
		root.Session.LearnerProfileID,
		uuid.New(),
		model.SHA256Hex([]byte("study-successor-flush-fixture"+flushID.String())),
		root.Session.ID,
		streamID,
		sequence,
		profileHighWater,
		stateVersion,
	); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.FLUSH_FIXTURE_FAILED", err))
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_SUCCESSOR.FLUSH_FIXTURE_COMMIT_FAILED", err))
	}
	return flushID, streamID
}

func studySuccessorWaitingPlan(
	sessionID uuid.UUID,
	requestID uuid.UUID,
	principal model.StudyPrincipalID,
	label string,
	predecessorID *uuid.UUID,
) StudyPlanInsert {
	return StudyPlanInsert{
		SessionID:               sessionID,
		Principal:               principal,
		CreationRequestID:       requestID,
		NormalizedRequestSHA256: model.SHA256Hex([]byte("request-" + label)),
		Lifecycle:               model.StudySessionLifecycleWaitingForCorpus,
		CorpusSHA256:            model.SHA256Hex([]byte("corpus-" + label)),
		PredecessorSessionID:    predecessorID,
		LifecycleReasonCode:     "fixture_waiting_for_corpus",
	}
}

func assertStudySuccessorAttemptError(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	fixture studySuccessorTestFixture,
	input StudySuccessorPlanInsert,
	captureEligibility bool,
	expectedCode model.StudyErrorCode,
) {
	t.Helper()
	err := repository.WithStudyScopeTransaction(
		ctx,
		StudyScopeTransactionInput{ProfileRef: fixture.ProfileRef, Scope: fixture.Scope},
		func(store StudyStoreTransaction) error {
			if captureEligibility {
				if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
					return listErr
				}
			}
			_, insertErr := store.InsertSuccessorPlanOnce(ctx, input)
			return insertErr
		},
	)
	assertStudySuccessorError(t, err, expectedCode)
}

func assertStudySuccessorError(t *testing.T, err error, expectedCode model.StudyErrorCode) {
	t.Helper()
	var studyError *model.StudyError
	if !errors.As(err, &studyError) || studyError == nil {
		t.Fatalf("STUDY_SUCCESSOR.ERROR_NOT_TYPED: expected_code=%s actual_type=%T", expectedCode, err)
	}
	if studyError.Code != expectedCode || studyError.Retryable {
		t.Fatalf(
			"STUDY_SUCCESSOR.ERROR_INVALID: expected_code=%s actual_code=%s retryable=%t",
			expectedCode,
			studyError.Code,
			studyError.Retryable,
		)
	}
}

func readStudySuccessorForTest(ctx context.Context, pool *pgxpool.Pool, predecessorID uuid.UUID) (uuid.UUID, error) {
	var successorID *uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT (
			SELECT successor.study_session_id
			FROM study_sessions successor
			WHERE successor.predecessor_study_session_id = $1
		)`, predecessorID).Scan(&successorID); err != nil {
		return uuid.Nil, err
	}
	if successorID == nil {
		return uuid.Nil, nil
	}
	return *successorID, nil
}
