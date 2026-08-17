package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
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
	start := make(chan struct{})
	ready := make(chan struct{}, len(plans))
	outcomes := make(chan outcome, len(plans))
	for index := range plans {
		index := index
		go func() {
			ready <- struct{}{}
			<-start
			var result *StudyPlanInsertResult
			err := repository.WithStudyScopeTransaction(
				ctx,
				StudyScopeTransactionInput{ProfileRef: fixture.ProfileRef, Scope: fixture.Scope},
				func(store StudyStoreTransaction) error {
					if current := store.NonterminalSession(); current != nil && current.ID == predecessorID {
						if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
							return listErr
						}
					}
					var insertErr error
					result, insertErr = store.InsertSuccessorPlanOnce(ctx, plans[index])
					return insertErr
				},
			)
			outcomes <- outcome{index: index, result: result, err: err}
		}()
	}
	for range plans {
		<-ready
	}
	close(start)
	first := <-outcomes
	second := <-outcomes
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
	type waitingReplacementOutcome struct {
		result *StudyPlanInsertResult
		err    error
	}
	start := make(chan struct{})
	ready := make(chan struct{}, len(activeInputs))
	outcomes := make(chan waitingReplacementOutcome, len(activeInputs))
	for index := range activeInputs {
		index := index
		go func() {
			ready <- struct{}{}
			<-start
			var result *StudyPlanInsertResult
			txErr := repository.WithStudyScopeTransaction(
				ctx,
				StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope},
				func(store StudyStoreTransaction) error {
					if current := store.NonterminalSession(); current != nil && current.ID == waitingID {
						sources, listErr := store.ListEligibleSources(ctx)
						if listErr != nil {
							return listErr
						}
						if len(sources) != 1 {
							return errors.New("STUDY_SUCCESSOR.WAITING_REPLACEMENT_CORPUS_INVALID")
						}
					}
					var insertErr error
					result, insertErr = store.InsertSuccessorPlanOnce(ctx, activeInputs[index])
					return insertErr
				},
			)
			outcomes <- waitingReplacementOutcome{result: result, err: txErr}
		}()
	}
	for range activeInputs {
		<-ready
	}
	close(start)
	firstReplacement := <-outcomes
	secondReplacement := <-outcomes
	for _, outcome := range []waitingReplacementOutcome{firstReplacement, secondReplacement} {
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
