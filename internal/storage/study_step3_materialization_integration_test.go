package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lexinet/internal/model"
	"lexinet/internal/study"
)

func TestPlanLexinetStep3Verify1Item2CorpusSizes012AndRepresentativeLarger(t *testing.T) {
	harness := startStudyTestPostgres(t)
	harness.migrateUp(t)
	pool := harness.openPool(t)
	repository := NewRepository(pool)
	materializer := newStudyMaterializerIntegrationSubject(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	for _, cardinality := range []int{0, 1, 2, 7} {
		cardinality := cardinality
		t.Run(fmt.Sprintf("corpus_%d", cardinality), func(t *testing.T) {
			testStudyMaterializerCorpusCardinality(t, ctx, repository, materializer, cardinality)
		})
	}
	t.Run("topic filtering and frozen fallback bytes", func(t *testing.T) {
		testStudyMaterializerTopicAndFallback(t, ctx, repository, materializer)
	})
	t.Run("projection correction and successor", func(t *testing.T) {
		testStudyMaterializerProjectionCorrection(t, ctx, pool, repository, materializer)
	})
	t.Run("oldest sentinel precedes latest presented", func(t *testing.T) {
		testStudyMaterializerOldestSentinelOrdering(t, ctx, repository, materializer)
	})
	t.Run("boundary successor and merge-stable replay", func(t *testing.T) {
		testStudyMaterializerBoundaryAndMergeReplay(t, ctx, repository, materializer)
	})
	t.Run("waiting convergence and atomic replacement", func(t *testing.T) {
		testStudyMaterializerWaitingResolution(t, ctx, repository, materializer)
	})
	t.Run("real storage digest vector", func(t *testing.T) {
		testStudyMaterializerStorageDigestVector(t, ctx, pool, repository)
	})
	t.Run("storage work bounds precede materialization", func(t *testing.T) {
		testStudyMaterializerStorageBounds(t, ctx, pool, repository)
	})
}

func testStudyMaterializerOldestSentinelOrdering(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	materializer *study.CycleMaterializer,
) {
	t.Helper()
	suffix := stringsForStudyFixture(uuid.NewString())
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "materializer-sentinel-" + suffix, Name: "Materializer sentinel " + suffix})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.SENTINEL.TOPIC_CREATE_FAILED", err))
	}
	presentedIdea := mustCreateStudyIdea(t, ctx, repository, topic.Slug, "sentinel-presented-"+suffix)
	profileRef := "materializer-sentinel-" + suffix
	scope := mustStudyScope(t, topic.ID, topic.Slug)
	rootRequest := newStudyMaterializationRequest(profileRef, scope, 1, "sentinel-root-"+suffix, nil, uuid.Nil)
	root, err := repository.MaterializeStudyCycle(ctx, materializer, rootRequest)
	if err != nil || root == nil || len(root.Persistence.Items) != 1 || root.Persistence.Items[0].CardIdentity != presentedIdea.ID {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.SENTINEL.ROOT_FAILED", err))
	}
	streamID := uuid.New()
	flushed, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               model.OperatorStudyPrincipalID(),
		ProfileRef:              profileRef,
		SessionID:               root.Persistence.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyMaterializationHash("sentinel-presented-" + suffix),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    root.Persistence.Session.StateVersion,
		Events: []model.StudyEvent{
			newStudyMaterializationEvent(profileRef, root.Persistence.Items[0], streamID, 1, uuid.New(), model.StudyEventKindPresented, nil, nil),
		},
	})
	if err != nil || flushed == nil || flushed.FlushReceipt.ProfileIngestionHighWater != 1 {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.SENTINEL.FLUSH_FAILED", err))
	}
	oldest := mustCreateStudyIdea(t, ctx, repository, topic.Slug, "sentinel-oldest-"+suffix)
	predecessorID := root.Persistence.Session.ID
	successorRequest := newStudyMaterializationRequest(profileRef, scope, 2, "sentinel-successor-"+suffix, &predecessorID, flushed.FlushReceipt.ID)
	successor, err := repository.MaterializeStudyCycle(ctx, materializer, successorRequest)
	if err != nil || successor == nil || len(successor.Persistence.Items) != 2 {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.SENTINEL.SUCCESSOR_FAILED", err))
	}
	first := successor.Persistence.Items[0]
	second := successor.Persistence.Items[1]
	if first.CardIdentity != oldest.ID || first.SelectionReason.RatingRank != model.StudyRatingRankUnseenUnrated ||
		first.SelectionReason.LatestPresentedIngestionOrdinal != nil || first.SelectionReason.BoundaryAdjustment != model.StudyBoundaryAdjustmentNone ||
		second.CardIdentity != presentedIdea.ID || second.SelectionReason.RatingRank != model.StudyRatingRankUnseenUnrated ||
		second.SelectionReason.LatestPresentedIngestionOrdinal == nil || *second.SelectionReason.LatestPresentedIngestionOrdinal != 1 ||
		second.SelectionReason.BoundaryAdjustment != model.StudyBoundaryAdjustmentNone {
		t.Fatalf("STUDY_MATERIALIZER.SENTINEL.OLDEST_ORDER_INVALID: %#v", successor.Persistence.Items)
	}
}

func TestPlanLexinetStep3Verify1Item3ShadowOutputCannotChangeAnyProductionPlan(t *testing.T) {
	harness := startStudyTestPostgres(t)
	harness.migrateUp(t)
	pool := harness.openPool(t)
	repository := NewRepository(pool)
	materializer := newStudyMaterializerIntegrationSubject(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	suffix := stringsForStudyFixture(uuid.NewString())
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "materializer-shadow-" + suffix, Name: "Materializer shadow " + suffix})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.SHADOW.TOPIC_CREATE_FAILED", err))
	}
	for index := 0; index < 3; index++ {
		mustCreateStudyIdea(t, ctx, repository, topic.Slug, fmt.Sprintf("shadow-%d-%s", index, suffix))
	}
	profileRef := "materializer-shadow-" + suffix
	scope := mustStudyScope(t, topic.ID, topic.Slug)
	request := newStudyMaterializationRequest(profileRef, scope, 3, "shadow-authority-"+suffix, nil, uuid.Nil)
	created, err := repository.MaterializeStudyCycle(ctx, materializer, request)
	if err != nil || created == nil || created.SchedulingSnapshot == nil || created.AuthoritativePlan == nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.SHADOW.COMMIT_FAILED", err))
	}
	// This fresh repository query is the explicit committed-visibility barrier;
	// no shadow adapter is invoked before it succeeds.
	committedRead := StudyCommittedCycleReadInput{
		Principal:         request.Materialization.Principal,
		ProfileRef:        profileRef,
		SessionID:         created.Persistence.Session.ID,
		CreationRequestID: request.Materialization.CreationRequestID,
	}
	committed, err := repository.ReadCommittedStudyCycle(ctx, committedRead)
	if err != nil || committed == nil || !reflect.DeepEqual(committed.Session, created.Persistence.Session) ||
		!reflect.DeepEqual(committed.Items, created.Persistence.Items) {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.SHADOW.COMMITTED_VISIBILITY_FAILED", err))
	}
	baseline := *committed
	baseline.Items = cloneStudySessionItems(committed.Items)
	authoritativePlan := *created.AuthoritativePlan
	authoritativePlan.Cards = append([]study.StudyPlannedCard(nil), created.AuthoritativePlan.Cards...)
	snapshot := *created.SchedulingSnapshot
	snapshot.Cards = append([]study.StudySchedulingCard(nil), created.SchedulingSnapshot.Cards...)

	testCases := []struct {
		name       string
		adapter    study.StudyShadowAdapter
		timeout    time.Duration
		wantStatus study.StudyShadowStatus
	}{
		{
			name: "reverse",
			adapter: studyMaterializationShadowAdapterFunc(func(_ context.Context, _ study.StudySchedulingSnapshot, plan study.StudyPlan) (study.StudyShadowEvaluation, error) {
				identities := studyMaterializationPlanIdentities(plan)
				for left, right := 0, len(identities)-1; left < right; left, right = left+1, right-1 {
					identities[left], identities[right] = identities[right], identities[left]
				}
				return study.StudyShadowEvaluation{OrderedCardIdentities: identities, Payload: json.RawMessage(`{"mode":"reverse"}`)}, nil
			}),
			timeout:    time.Second,
			wantStatus: study.StudyShadowStatusCompleted,
		},
		{
			name: "mutate private input",
			adapter: studyMaterializationShadowAdapterFunc(func(_ context.Context, candidate study.StudySchedulingSnapshot, plan study.StudyPlan) (study.StudyShadowEvaluation, error) {
				identities := studyMaterializationPlanIdentities(plan)
				candidate.Cards[0].CardIdentity = uuid.New()
				plan.Cards[0].CardIdentity = uuid.New()
				return study.StudyShadowEvaluation{OrderedCardIdentities: identities, Payload: json.RawMessage(`{"mode":"mutated"}`)}, nil
			}),
			timeout:    time.Second,
			wantStatus: study.StudyShadowStatusCompleted,
		},
		{
			name: "error",
			adapter: studyMaterializationShadowAdapterFunc(func(context.Context, study.StudySchedulingSnapshot, study.StudyPlan) (study.StudyShadowEvaluation, error) {
				return study.StudyShadowEvaluation{}, errors.New("private shadow failure")
			}),
			timeout:    time.Second,
			wantStatus: study.StudyShadowStatusFailed,
		},
		{
			name: "timeout",
			adapter: studyMaterializationShadowAdapterFunc(func(adapterContext context.Context, _ study.StudySchedulingSnapshot, _ study.StudyPlan) (study.StudyShadowEvaluation, error) {
				<-adapterContext.Done()
				return study.StudyShadowEvaluation{}, adapterContext.Err()
			}),
			timeout:    10 * time.Millisecond,
			wantStatus: study.StudyShadowStatusTimedOut,
		},
		{
			name: "panic",
			adapter: studyMaterializationShadowAdapterFunc(func(context.Context, study.StudySchedulingSnapshot, study.StudyPlan) (study.StudyShadowEvaluation, error) {
				panic("private shadow panic")
			}),
			timeout:    time.Second,
			wantStatus: study.StudyShadowStatusPanic,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			runner, runnerErr := study.NewStudyShadowRunner(testCase.adapter, "step3-shadow", "1", testCase.timeout)
			if runnerErr != nil {
				t.Fatalf("construct shadow runner: %v", runnerErr)
			}
			report := runner.Run(ctx, snapshot, authoritativePlan)
			if report.Status != testCase.wantStatus || report.Authority != study.StudyShadowAuthorityNonAuthoritative {
				t.Fatalf("shadow report = %#v", report)
			}
			after, readErr := repository.ReadCommittedStudyCycle(ctx, committedRead)
			if readErr != nil || !reflect.DeepEqual(after.Session, baseline.Session) || !reflect.DeepEqual(after.Items, baseline.Items) ||
				!reflect.DeepEqual(authoritativePlan, *created.AuthoritativePlan) || !reflect.DeepEqual(snapshot, *created.SchedulingSnapshot) {
				t.Fatal("STUDY_MATERIALIZER.SHADOW.CHANGED_PRODUCTION_AUTHORITY")
			}
		})
	}

	notConfigured, err := study.NewStudyShadowRunner(nil, "", "", 0)
	if err != nil || notConfigured.Run(ctx, snapshot, authoritativePlan).Status != study.StudyShadowStatusNotConfigured {
		t.Fatal("STUDY_MATERIALIZER.SHADOW.NOT_CONFIGURED_INVALID")
	}
	testStudyMaterializerRollbackSuppressesShadow(t, ctx, pool, repository, materializer)
}

type studyMaterializationShadowAdapterFunc func(
	context.Context,
	study.StudySchedulingSnapshot,
	study.StudyPlan,
) (study.StudyShadowEvaluation, error)

func (function studyMaterializationShadowAdapterFunc) Evaluate(
	ctx context.Context,
	snapshot study.StudySchedulingSnapshot,
	plan study.StudyPlan,
) (study.StudyShadowEvaluation, error) {
	return function(ctx, snapshot, plan)
}

func studyMaterializationPlanIdentities(plan study.StudyPlan) []uuid.UUID {
	identities := make([]uuid.UUID, len(plan.Cards))
	for index, card := range plan.Cards {
		identities[index] = card.CardIdentity
	}
	return identities
}

func testStudyMaterializerRollbackSuppressesShadow(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	materializer *study.CycleMaterializer,
) {
	t.Helper()
	var adapterCalls atomic.Int32
	adapter := studyMaterializationShadowAdapterFunc(func(_ context.Context, _ study.StudySchedulingSnapshot, plan study.StudyPlan) (study.StudyShadowEvaluation, error) {
		adapterCalls.Add(1)
		return study.StudyShadowEvaluation{OrderedCardIdentities: studyMaterializationPlanIdentities(plan)}, nil
	})
	runner, err := study.NewStudyShadowRunner(adapter, "rollback-shadow", "1", time.Second)
	if err != nil {
		t.Fatalf("construct rollback shadow runner: %v", err)
	}

	suffix := stringsForStudyFixture(uuid.NewString())
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "materializer-rollback-" + suffix, Name: "Materializer rollback " + suffix})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.ROLLBACK.TOPIC_CREATE_FAILED", err))
	}
	mustCreateStudyIdea(t, ctx, repository, topic.Slug, "rollback-"+suffix)
	profileRef := "materializer-rollback-" + suffix
	scope := mustStudyScope(t, topic.ID, topic.Slug)
	request := newStudyMaterializationRequest(profileRef, scope, 1, "rollback-create-"+suffix, nil, uuid.Nil)
	missingPredecessor := uuid.New()
	repository.studyMaterializationBeforeCommit = func(hookContext context.Context, tx pgx.Tx) error {
		if _, hookErr := tx.Exec(hookContext, `SET CONSTRAINTS study_sessions_predecessor_same_scope_fk DEFERRED`); hookErr != nil {
			return hookErr
		}
		_, hookErr := tx.Exec(hookContext, `
			UPDATE study_sessions
			SET predecessor_study_session_id = $2
			WHERE study_session_id = $1`, request.Materialization.SessionID, missingPredecessor)
		return hookErr
	}
	failed, failure := repository.MaterializeStudyCycle(ctx, materializer, request)
	repository.studyMaterializationBeforeCommit = nil
	var commitStudyError *model.StudyError
	if failed != nil || !errors.As(failure, &commitStudyError) || commitStudyError.Context["operation"] != "study_transaction_commit" {
		t.Fatal("STUDY_MATERIALIZER.ROLLBACK.COMMIT_HOOK_DID_NOT_RETURN_NIL_RESULT")
	}
	if failed != nil && failed.SchedulingSnapshot != nil && failed.AuthoritativePlan != nil {
		_ = runner.Run(ctx, *failed.SchedulingSnapshot, *failed.AuthoritativePlan)
	}
	var sessionCount, itemCount, projectionCount, receiptCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM study_sessions WHERE study_session_id = $1),
			(SELECT count(*) FROM study_session_items WHERE study_session_id = $1),
			(SELECT count(*) FROM study_scheduler_card_projections projection
			 JOIN study_learner_profiles profile ON profile.learner_profile_id = projection.learner_profile_id
			 WHERE profile.learner_profile_ref = $3),
			(SELECT count(*) FROM study_session_creation_receipts WHERE creation_request_id = $2)`,
		request.Materialization.SessionID,
		request.Materialization.CreationRequestID,
		profileRef,
	).Scan(&sessionCount, &itemCount, &projectionCount, &receiptCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.ROLLBACK.COUNTS_FAILED", err))
	}
	if sessionCount != 0 || itemCount != 0 || projectionCount != 0 || receiptCount != 0 || adapterCalls.Load() != 0 {
		t.Fatal("STUDY_MATERIALIZER.ROLLBACK.PERSISTED_OR_INVOKED_SHADOW")
	}

	root, err := repository.MaterializeStudyCycle(ctx, materializer, request)
	if err != nil || root == nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.ROLLBACK.SAME_REQUEST_RETRY_FAILED", err))
	}
	directRequest := newStudyMaterializationRequest(profileRef, scope, 1, "rollback-direct-resolve-"+suffix, nil, uuid.Nil)
	rollbackMarker := errors.New("injected resolver callback rollback")
	err = repository.WithStudyScopeTransaction(ctx, directRequest.Transaction, func(transaction StudyStoreTransaction) error {
		adapter, adapterErr := NewStudyMaterializationAdapter(transaction)
		if adapterErr != nil {
			return adapterErr
		}
		resolved, resolveErr := adapter.ResolveExistingCycle(ctx, study.ExistingCycleResolutionInput{
			Principal:               directRequest.Materialization.Principal,
			CreationRequestID:       directRequest.Materialization.CreationRequestID,
			NormalizedRequestSHA256: directRequest.Materialization.NormalizedRequestSHA256,
		})
		if resolveErr != nil || resolved == nil || resolved.Persistence.Session.ID != root.Persistence.Session.ID {
			return fmt.Errorf("direct resolver failed: %w", resolveErr)
		}
		return rollbackMarker
	})
	if !errors.Is(err, rollbackMarker) {
		t.Fatal("STUDY_MATERIALIZER.ROLLBACK.DIRECT_RESOLVER_CALLBACK_ERROR_NOT_PROPAGATED")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM study_session_creation_receipts WHERE creation_request_id = $1`, directRequest.Materialization.CreationRequestID).Scan(&receiptCount); err != nil || receiptCount != 0 {
		t.Fatal("STUDY_MATERIALIZER.ROLLBACK.DIRECT_RESOLVER_RECEIPT_PERSISTED")
	}

	preflightRequest := newStudyMaterializationRequest(profileRef, scope, 1, "rollback-preflight-"+suffix, nil, uuid.Nil)
	repository.studyMaterializationBeforeCommit = func(context.Context, pgx.Tx) error {
		return &pgconn.PgError{Code: "40001"}
	}
	preflightResult, preflightErr := repository.MaterializeStudyCycle(ctx, materializer, preflightRequest)
	repository.studyMaterializationBeforeCommit = nil
	if preflightResult != nil || !isStudyMaterializationCode(preflightErr, model.StudyErrorCodeServiceUnavailable) {
		t.Fatal("STUDY_MATERIALIZER.ROLLBACK.PREFLIGHT_COMMIT_FAILURE_EXPOSED_RESULT")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM study_session_creation_receipts WHERE creation_request_id = $1`, preflightRequest.Materialization.CreationRequestID).Scan(&receiptCount); err != nil || receiptCount != 0 || adapterCalls.Load() != 0 {
		t.Fatal("STUDY_MATERIALIZER.ROLLBACK.PREFLIGHT_RECEIPT_OR_SHADOW_PERSISTED")
	}
}

func testStudyMaterializerCorpusCardinality(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	materializer *study.CycleMaterializer,
	cardinality int,
) {
	t.Helper()
	suffix := stringsForStudyFixture(uuid.NewString())
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{
		Slug: fmt.Sprintf("materializer-cardinality-%d-%s", cardinality, suffix),
		Name: fmt.Sprintf("Materializer cardinality %d %s", cardinality, suffix),
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.CARDINALITY.TOPIC_CREATE_FAILED", err))
	}
	ideaIDs := make([]uuid.UUID, cardinality)
	for index := range ideaIDs {
		ideaIDs[index] = mustCreateStudyIdea(t, ctx, repository, topic.Slug, fmt.Sprintf("materializer-%d-%d", cardinality, index)).ID
	}
	profileRef := fmt.Sprintf("materializer-cardinality-%d-%s", cardinality, suffix)
	scope := mustStudyScope(t, topic.ID, topic.Slug)
	request := newStudyMaterializationRequest(profileRef, scope, cardinality, fmt.Sprintf("cardinality-%d-%s", cardinality, suffix), nil, uuid.Nil)
	created, err := repository.MaterializeStudyCycle(ctx, materializer, request)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.CARDINALITY.CREATE_FAILED", err))
	}
	if created == nil || !created.Persistence.Created || created.Persistence.Session.ID != request.Materialization.SessionID ||
		len(created.Persistence.Items) != cardinality {
		t.Fatalf("STUDY_MATERIALIZER.CARDINALITY.INVALID_RESULT: %#v", created)
	}
	if cardinality == 0 {
		if created.Persistence.Session.Lifecycle != model.StudySessionLifecycleWaitingForCorpus ||
			created.SchedulingSnapshot != nil || created.AuthoritativePlan != nil {
			t.Fatal("STUDY_MATERIALIZER.CARDINALITY.EMPTY_NOT_WAITING")
		}
	} else {
		if created.Persistence.Session.Lifecycle != model.StudySessionLifecycleActive ||
			created.SchedulingSnapshot == nil || created.AuthoritativePlan == nil ||
			len(created.AuthoritativePlan.Cards) != cardinality {
			t.Fatal("STUDY_MATERIALIZER.CARDINALITY.ACTIVE_AUTHORITY_INVALID")
		}
		assertStudyMaterializationCoverage(t, created.Persistence.Items, ideaIDs)
		for _, item := range created.Persistence.Items {
			if item.SelectionReason.RatingRank != model.StudyRatingRankUnseenUnrated ||
				item.SelectionReason.LatestPresentedIngestionOrdinal != nil {
				t.Fatal("STUDY_MATERIALIZER.CARDINALITY.FRESH_SENTINEL_PROVENANCE_INVALID")
			}
		}
	}

	replayed, err := repository.MaterializeStudyCycle(ctx, materializer, request)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.CARDINALITY.REPLAY_FAILED", err))
	}
	if replayed == nil || replayed.Persistence.Created || replayed.SchedulingSnapshot != nil ||
		replayed.Persistence.Session.ID != created.Persistence.Session.ID ||
		replayed.Persistence.Receipt.CreationReceiptAdjudicatedAt != created.Persistence.Receipt.CreationReceiptAdjudicatedAt ||
		!reflect.DeepEqual(replayed.AuthoritativePlan, created.AuthoritativePlan) {
		t.Fatal("STUDY_MATERIALIZER.CARDINALITY.BYTE_REPLAY_CHANGED_AUTHORITY")
	}

	if cardinality > 0 {
		mustCreateStudyIdea(t, ctx, repository, topic.Slug, fmt.Sprintf("materializer-%d-late", cardinality))
		afterCorpusChange, replayErr := repository.MaterializeStudyCycle(ctx, materializer, request)
		if replayErr != nil || afterCorpusChange == nil || afterCorpusChange.Persistence.Session.ID != created.Persistence.Session.ID ||
			afterCorpusChange.SchedulingSnapshot != nil || !reflect.DeepEqual(afterCorpusChange.AuthoritativePlan, created.AuthoritativePlan) {
			t.Fatal("STUDY_MATERIALIZER.CARDINALITY.IMMUTABLE_ACTIVE_REPLAY_FAILED")
		}

		distinct := newStudyMaterializationRequest(profileRef, scope, cardinality+1, fmt.Sprintf("cardinality-%d-distinct-%s", cardinality, suffix), nil, uuid.Nil)
		converged, convergeErr := repository.MaterializeStudyCycle(ctx, materializer, distinct)
		if convergeErr != nil || converged == nil || converged.Persistence.Created ||
			converged.Persistence.Session.ID != created.Persistence.Session.ID ||
			converged.Persistence.Receipt.Disposition != model.StudyCreationDispositionConvergedToExisting ||
			converged.SchedulingSnapshot != nil {
			t.Fatal("STUDY_MATERIALIZER.CARDINALITY.DISTINCT_ACTIVE_DID_NOT_PREFLIGHT_CONVERGE")
		}
	}

	conflict := request
	conflict.Materialization.NormalizedRequestSHA256 = model.SHA256Hex([]byte("changed-" + suffix))
	conflicted, conflictErr := repository.MaterializeStudyCycle(ctx, materializer, conflict)
	var studyError *model.StudyError
	if conflicted != nil || !errors.As(conflictErr, &studyError) || studyError.Code != model.StudyErrorCodeIdempotencyConflict {
		t.Fatal("STUDY_MATERIALIZER.CARDINALITY.CHANGED_HASH_DID_NOT_CONFLICT")
	}
}

func testStudyMaterializerTopicAndFallback(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	materializer *study.CycleMaterializer,
) {
	t.Helper()
	suffix := stringsForStudyFixture(uuid.NewString())
	inTopic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "materializer-in-" + suffix, Name: "Materializer in " + suffix})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.FALLBACK.TOPIC_CREATE_FAILED", err))
	}
	outTopic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "materializer-out-" + suffix, Name: "Materializer out " + suffix})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.FALLBACK.OUT_TOPIC_CREATE_FAILED", err))
	}
	summary := "Summary fallback " + suffix
	fallback, err := repository.CreateIdea(ctx, model.CreateIdeaInput{
		Summary: &summary,
		Names:   []model.CreateNameInput{{Text: "Fallback " + suffix, Kind: model.NameKindCanonical}},
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.FALLBACK.IDEA_CREATE_FAILED", err))
	}
	if _, err := repository.AssignTopic(ctx, fallback.ID.String(), inTopic.Slug); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.FALLBACK.TOPIC_ASSIGN_FAILED", err))
	}
	primary, err := repository.CreateIdea(ctx, model.CreateIdeaInput{
		Names: []model.CreateNameInput{{Text: "Primary " + suffix, Kind: model.NameKindCanonical}},
		Definitions: []model.CreateDefinitionInput{
			{Body: "non-primary-low", SortKey: -10},
			{Body: "chosen-primary", IsPrimary: true, SortKey: 100},
		},
		Examples: []model.CreateExampleInput{
			{Body: "later-example", SortKey: 5},
			{Body: "chosen-example", SortKey: -5},
		},
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.FALLBACK.PRIMARY_CREATE_FAILED", err))
	}
	if _, err := repository.AssignTopic(ctx, primary.ID.String(), inTopic.Slug); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.FALLBACK.PRIMARY_ASSIGN_FAILED", err))
	}
	sortedFallback, err := repository.CreateIdea(ctx, model.CreateIdeaInput{
		Names: []model.CreateNameInput{{Text: "Sorted fallback " + suffix, Kind: model.NameKindCanonical}},
		Definitions: []model.CreateDefinitionInput{
			{Body: "later-sorted-definition", SortKey: 4},
			{Body: "chosen-sorted-definition", SortKey: -4},
		},
		Examples: []model.CreateExampleInput{
			{Body: "only-chosen-sorted-example", SortKey: 0},
		},
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.FALLBACK.SORTED_CREATE_FAILED", err))
	}
	if _, err := repository.AssignTopic(ctx, sortedFallback.ID.String(), inTopic.Slug); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.FALLBACK.SORTED_ASSIGN_FAILED", err))
	}
	mustCreateStudyIdea(t, ctx, repository, outTopic.Slug, "out-of-scope-"+suffix)

	scope := mustStudyScope(t, inTopic.ID, inTopic.Slug)
	request := newStudyMaterializationRequest("materializer-fallback-"+suffix, scope, 3, "fallback-"+suffix, nil, uuid.Nil)
	result, err := repository.MaterializeStudyCycle(ctx, materializer, request)
	if err != nil || result == nil || len(result.Persistence.Items) != 3 {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.FALLBACK.MATERIALIZE_FAILED", err))
	}
	items := make(map[uuid.UUID]model.StudySessionItem, 3)
	for _, item := range result.Persistence.Items {
		items[item.CardIdentity] = item
	}
	if len(items) != 3 || items[fallback.ID].BackDefinition != summary || items[fallback.ID].BackExample != nil ||
		items[primary.ID].BackDefinition != "chosen-primary" || items[primary.ID].BackExample == nil || *items[primary.ID].BackExample != "chosen-example" ||
		items[sortedFallback.ID].BackDefinition != "chosen-sorted-definition" || items[sortedFallback.ID].BackExample == nil ||
		*items[sortedFallback.ID].BackExample != "only-chosen-sorted-example" {
		t.Fatalf("STUDY_MATERIALIZER.FALLBACK.FROZEN_BYTES_INVALID: %#v", items)
	}
}

func testStudyMaterializerProjectionCorrection(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	materializer *study.CycleMaterializer,
) {
	t.Helper()
	suffix := stringsForStudyFixture(uuid.NewString())
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "materializer-projection-" + suffix, Name: "Materializer projection " + suffix})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.PROJECTION.TOPIC_CREATE_FAILED", err))
	}
	ideaIDs := make([]uuid.UUID, 5)
	for index := range ideaIDs {
		ideaIDs[index] = mustCreateStudyIdea(t, ctx, repository, topic.Slug, fmt.Sprintf("projection-%d-%s", index, suffix)).ID
	}
	profileRef := "materializer-projection-" + suffix
	scope := mustStudyScope(t, topic.ID, topic.Slug)
	rootRequest := newStudyMaterializationRequest(profileRef, scope, 5, "projection-root-"+suffix, nil, uuid.Nil)
	root, err := repository.MaterializeStudyCycle(ctx, materializer, rootRequest)
	if err != nil || root == nil || len(root.Persistence.Items) != 5 {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.PROJECTION.ROOT_FAILED", err))
	}

	streamID := uuid.New()
	ratings := map[int]model.StudyRating{
		0: model.StudyRatingHard,
		1: model.StudyRatingGood, // voided below to unseen/unrated
		2: model.StudyRatingGood,
		3: model.StudyRatingEasy,
		4: model.StudyRatingAgain, // predecessor tail: unavoidable priority repeat
	}
	ratingIDs := make(map[int]uuid.UUID, len(ratings))
	events := make([]model.StudyEvent, 0, 15)
	sequence := int64(1)
	for index, item := range root.Persistence.Items {
		events = append(events,
			newStudyMaterializationEvent(profileRef, item, streamID, sequence, uuid.New(), model.StudyEventKindPresented, nil, nil),
			newStudyMaterializationEvent(profileRef, item, streamID, sequence+1, uuid.New(), model.StudyEventKindRevealed, nil, nil),
		)
		ratingID := uuid.New()
		ratingIDs[index] = ratingID
		rating := ratings[index]
		events = append(events, newStudyMaterializationEvent(profileRef, item, streamID, sequence+2, ratingID, model.StudyEventKindRated, &rating, nil))
		sequence += 3
	}
	initial, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               model.OperatorStudyPrincipalID(),
		ProfileRef:              profileRef,
		SessionID:               root.Persistence.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyMaterializationHash("projection-initial-" + suffix),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    root.Persistence.Session.StateVersion,
		Events:                  events,
	})
	if err != nil || initial == nil || initial.FlushReceipt.ProfileIngestionHighWater != 15 {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.PROJECTION.INITIAL_EVENTS_FAILED", err))
	}
	voidedRatingID := ratingIDs[1]
	correction, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               model.OperatorStudyPrincipalID(),
		ProfileRef:              profileRef,
		SessionID:               root.Persistence.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyMaterializationHash("projection-correction-" + suffix),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    initial.Session.StateVersion,
		Events: []model.StudyEvent{
			newStudyMaterializationEvent(profileRef, root.Persistence.Items[1], streamID, 16, uuid.New(), model.StudyEventKindRatingVoided, nil, &voidedRatingID),
		},
	})
	if err != nil || correction == nil || correction.FlushReceipt.ProfileIngestionHighWater != 16 {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.PROJECTION.CORRECTION_FAILED", err))
	}

	predecessorID := root.Persistence.Session.ID
	successorRequest := newStudyMaterializationRequest(
		profileRef,
		scope,
		5,
		"projection-successor-"+suffix,
		&predecessorID,
		correction.FlushReceipt.ID,
	)
	successor, err := repository.MaterializeStudyCycle(ctx, materializer, successorRequest)
	if err != nil || successor == nil || !successor.Persistence.Created || len(successor.Persistence.Items) != 5 {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.PROJECTION.SUCCESSOR_FAILED", err))
	}
	wantRanks := []model.StudyRatingRank{
		model.StudyRatingRankAgain,
		model.StudyRatingRankHard,
		model.StudyRatingRankUnseenUnrated,
		model.StudyRatingRankGood,
		model.StudyRatingRankEasy,
	}
	wantIdentities := []uuid.UUID{
		root.Persistence.Items[4].CardIdentity,
		root.Persistence.Items[0].CardIdentity,
		root.Persistence.Items[1].CardIdentity,
		root.Persistence.Items[2].CardIdentity,
		root.Persistence.Items[3].CardIdentity,
	}
	wantPresentedOrdinals := map[uuid.UUID]int64{}
	for index, item := range root.Persistence.Items {
		wantPresentedOrdinals[item.CardIdentity] = int64(index*3 + 1)
	}
	for index, item := range successor.Persistence.Items {
		ordinal := item.SelectionReason.LatestPresentedIngestionOrdinal
		if item.CardIdentity != wantIdentities[index] || item.SelectionReason.RatingRank != wantRanks[index] ||
			ordinal == nil || *ordinal != wantPresentedOrdinals[item.CardIdentity] {
			t.Fatalf("STUDY_MATERIALIZER.PROJECTION.RANK_OR_ORDINAL_INVALID: %#v", successor.Persistence.Items)
		}
	}
	if successor.Persistence.Items[0].SelectionReason.BoundaryAdjustment != model.StudyBoundaryAdjustmentPriorityBound ||
		successor.Persistence.Items[1].SelectionReason.BoundaryAdjustment != model.StudyBoundaryAdjustmentNone {
		t.Fatal("STUDY_MATERIALIZER.PROJECTION.PRIORITY_BOUNDARY_PROVENANCE_INVALID")
	}
	assertStudyMaterializationCoverage(t, successor.Persistence.Items, ideaIDs)
	assertStudyMaterializationProjectionDiscardRebuild(t, ctx, pool, repository, profileRef, scope, successor.Persistence.Session.LearnerProfileID)

	distinctLinked := newStudyMaterializationRequest(profileRef, scope, 0, "projection-linked-"+suffix, &predecessorID, uuid.New())
	linked, linkedErr := repository.MaterializeStudyCycle(ctx, materializer, distinctLinked)
	if linkedErr != nil || linked == nil || linked.Persistence.Created ||
		linked.Persistence.Session.ID != successor.Persistence.Session.ID ||
		linked.Persistence.Receipt.Disposition != model.StudyCreationDispositionSuccessor || linked.SchedulingSnapshot != nil {
		t.Fatal("STUDY_MATERIALIZER.PROJECTION.LINKED_SUCCESSOR_DID_NOT_PREFLIGHT_CONVERGE")
	}
}

func assertStudyMaterializationProjectionDiscardRebuild(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	profileRef string,
	scope model.StudyNormalizedScope,
	profileID uuid.UUID,
) {
	t.Helper()
	type projectionBytes struct {
		payload string
		sha256  string
	}
	rows, err := pool.Query(ctx, `
		SELECT projected_study_card_identity, canonical_projection_payload, canonical_projection_sha256
		FROM study_scheduler_card_projections
		WHERE learner_profile_id = $1
		  AND study_scheduler_name = $2
		  AND study_scheduler_version = $3
		  AND projection_schema_version = $4`,
		profileID,
		study.VarietyFirstV1Name,
		study.VarietyFirstV1Version,
		study.StudyProjectionSchemaVersionV1,
	)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.PROJECTION.REBUILD_READ_FAILED", err))
	}
	expected := make(map[uuid.UUID]projectionBytes)
	for rows.Next() {
		var identity uuid.UUID
		var bytes projectionBytes
		if err := rows.Scan(&identity, &bytes.payload, &bytes.sha256); err != nil {
			rows.Close()
			t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.PROJECTION.REBUILD_SCAN_FAILED", err))
		}
		expected[identity] = bytes
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.PROJECTION.REBUILD_ROWS_FAILED", err))
	}
	rows.Close()
	if len(expected) != 5 {
		t.Fatal("STUDY_MATERIALIZER.PROJECTION.REBUILD_BASELINE_INCOMPLETE")
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM study_scheduler_card_projections
		WHERE learner_profile_id = $1
		  AND study_scheduler_name = $2
		  AND study_scheduler_version = $3
		  AND projection_schema_version = $4`,
		profileID,
		study.VarietyFirstV1Name,
		study.VarietyFirstV1Version,
		study.StudyProjectionSchemaVersionV1,
	); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.PROJECTION.REBUILD_DELETE_FAILED", err))
	}
	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(transaction StudyStoreTransaction) error {
		adapter, adapterErr := NewStudyMaterializationAdapter(transaction)
		if adapterErr != nil {
			return adapterErr
		}
		captured, adapterErr := adapter.CaptureEligibleCorpus(ctx)
		if adapterErr != nil {
			return adapterErr
		}
		prefix, adapterErr := adapter.ReadAcceptedPrefix(ctx, captured.LearnerProfile.ProfileIngestionHighWater)
		if adapterErr != nil {
			return adapterErr
		}
		resolved := make([]study.StudyResolvedAcceptedEvent, len(prefix))
		for index, event := range prefix {
			identity, resolveErr := adapter.ResolveCardIdentity(ctx, event.CardIdentity)
			if resolveErr != nil {
				return resolveErr
			}
			resolved[index] = study.StudyResolvedAcceptedEvent{Event: event, CurrentCardIdentity: identity}
		}
		targets := make([]study.StudyProjectionTarget, len(captured.Sources))
		for index, source := range captured.Sources {
			liveSource := source.LiveSourceIdeaID
			targets[index] = study.StudyProjectionTarget{CurrentCardIdentity: source.CardIdentity, LiveSourceIdeaID: &liveSource}
		}
		rebuilt, rebuildErr := study.RebuildStudyCardProjections(study.StudyProjectionRebuildInput{
			LearnerProfileID:          captured.LearnerProfile.ID,
			SchedulerName:             study.VarietyFirstV1Name,
			SchedulerVersion:          study.VarietyFirstV1Version,
			ProjectionSchemaVersion:   study.StudyProjectionSchemaVersionV1,
			ProfileIngestionHighWater: captured.LearnerProfile.ProfileIngestionHighWater,
			Targets:                   targets,
			AcceptedEvents:            resolved,
		})
		if rebuildErr != nil {
			return rebuildErr
		}
		if len(rebuilt) != len(expected) {
			return errors.New("projection rebuild identity count changed")
		}
		seen := make(map[uuid.UUID]struct{}, len(rebuilt))
		for _, projection := range rebuilt {
			if _, duplicate := seen[projection.ProjectedCardIdentity]; duplicate {
				return errors.New("projection rebuild repeated an identity")
			}
			if _, exists := expected[projection.ProjectedCardIdentity]; !exists {
				return errors.New("projection rebuild introduced an identity")
			}
			seen[projection.ProjectedCardIdentity] = struct{}{}
			persisted, replaceErr := adapter.ReplaceProjection(ctx, projection, 0)
			if replaceErr != nil {
				return replaceErr
			}
			want, exists := expected[persisted.ProjectedCardIdentity]
			if !exists || string(persisted.CanonicalPayload) != want.payload || persisted.CanonicalSHA256 != want.sha256 {
				return errors.New("projection rebuild bytes changed")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.PROJECTION.REBUILD_FAILED", err))
	}
	rebuiltRows, err := pool.Query(ctx, `
		SELECT projected_study_card_identity, canonical_projection_payload, canonical_projection_sha256
		FROM study_scheduler_card_projections
		WHERE learner_profile_id = $1
		  AND study_scheduler_name = $2
		  AND study_scheduler_version = $3
		  AND projection_schema_version = $4`,
		profileID,
		study.VarietyFirstV1Name,
		study.VarietyFirstV1Version,
		study.StudyProjectionSchemaVersionV1,
	)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.PROJECTION.REBUILD_COMMITTED_READ_FAILED", err))
	}
	committedSeen := make(map[uuid.UUID]struct{}, len(expected))
	for rebuiltRows.Next() {
		var identity uuid.UUID
		var payload, sha256 string
		if err := rebuiltRows.Scan(&identity, &payload, &sha256); err != nil {
			rebuiltRows.Close()
			t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.PROJECTION.REBUILD_COMMITTED_SCAN_FAILED", err))
		}
		want, exists := expected[identity]
		if !exists || payload != want.payload || sha256 != want.sha256 {
			rebuiltRows.Close()
			t.Fatal("STUDY_MATERIALIZER.PROJECTION.REBUILD_COMMITTED_BYTES_CHANGED")
		}
		if _, duplicate := committedSeen[identity]; duplicate {
			rebuiltRows.Close()
			t.Fatal("STUDY_MATERIALIZER.PROJECTION.REBUILD_COMMITTED_IDENTITY_REPEATED")
		}
		committedSeen[identity] = struct{}{}
	}
	if err := rebuiltRows.Err(); err != nil {
		rebuiltRows.Close()
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.PROJECTION.REBUILD_COMMITTED_ROWS_FAILED", err))
	}
	rebuiltRows.Close()
	if len(committedSeen) != len(expected) {
		t.Fatal("STUDY_MATERIALIZER.PROJECTION.REBUILD_COMMITTED_COVERAGE_INCOMPLETE")
	}
}

func testStudyMaterializerBoundaryAndMergeReplay(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	materializer *study.CycleMaterializer,
) {
	t.Helper()
	suffix := stringsForStudyFixture(uuid.NewString())
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "materializer-boundary-" + suffix, Name: "Materializer boundary " + suffix})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUNDARY.TOPIC_CREATE_FAILED", err))
	}
	source := mustCreateStudyIdea(t, ctx, repository, topic.Slug, "boundary-source-"+suffix)
	target := mustCreateStudyIdea(t, ctx, repository, topic.Slug, "boundary-target-"+suffix)
	profileRef := "materializer-boundary-" + suffix
	scope := mustStudyScope(t, topic.ID, topic.Slug)
	rootRequest := newStudyMaterializationRequest(profileRef, scope, 2, "boundary-root-"+suffix, nil, uuid.Nil)
	root, err := repository.MaterializeStudyCycle(ctx, materializer, rootRequest)
	if err != nil || root == nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUNDARY.ROOT_FAILED", err))
	}
	streamID := uuid.New()
	predecessorTailItem := root.Persistence.Items[len(root.Persistence.Items)-1]
	otherItem := root.Persistence.Items[0]
	events := []model.StudyEvent{
		newStudyMaterializationEvent(profileRef, predecessorTailItem, streamID, 1, uuid.New(), model.StudyEventKindPresented, nil, nil),
		newStudyMaterializationEvent(profileRef, otherItem, streamID, 2, uuid.New(), model.StudyEventKindPresented, nil, nil),
	}
	flush, err := repository.AdjudicateStudyEvents(ctx, StudyEventSyncInput{
		PageArtifact:            studyTestSessionPageArtifact(),
		Principal:               model.OperatorStudyPrincipalID(),
		ProfileRef:              profileRef,
		SessionID:               root.Persistence.Session.ID,
		SyncRequestID:           uuid.New(),
		NormalizedRequestSHA256: studyMaterializationHash("boundary-flush-" + suffix),
		ClientStreamID:          streamID,
		ExpectedStateVersion:    root.Persistence.Session.StateVersion,
		Events:                  events,
	})
	if err != nil || flush == nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUNDARY.FLUSH_FAILED", err))
	}
	predecessorID := root.Persistence.Session.ID
	successorRequest := newStudyMaterializationRequest(profileRef, scope, 2, "boundary-successor-"+suffix, &predecessorID, flush.FlushReceipt.ID)
	successorRequest.Materialization.Seed = findStudyMaterializationBoundarySeed(
		t,
		root.Persistence.Session,
		predecessorTailItem.CardIdentity,
		otherItem.CardIdentity,
		flush.FlushReceipt.ProfileIngestionHighWater,
		successorRequest.Materialization.PlannedAt,
	)
	successor, err := repository.MaterializeStudyCycle(ctx, materializer, successorRequest)
	if err != nil || successor == nil || !successor.Persistence.Created {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUNDARY.SUCCESSOR_FAILED", err))
	}
	if successor.Persistence.Session.PredecessorStudySessionID == nil ||
		*successor.Persistence.Session.PredecessorStudySessionID != predecessorID {
		t.Fatal("STUDY_MATERIALIZER.BOUNDARY.SUCCESSOR_LINK_INVALID")
	}
	freshRoot := newStudyMaterializationRequest(profileRef, scope, 0, "boundary-fresh-root-"+suffix, nil, uuid.Nil)
	rootConverged, err := repository.MaterializeStudyCycle(ctx, materializer, freshRoot)
	if err != nil || rootConverged == nil || rootConverged.Persistence.Created ||
		rootConverged.Persistence.Session.ID != successor.Persistence.Session.ID ||
		rootConverged.Persistence.Receipt.Disposition != model.StudyCreationDispositionConvergedToExisting ||
		rootConverged.SchedulingSnapshot != nil {
		t.Fatal("STUDY_MATERIALIZER.BOUNDARY.FRESH_ROOT_DID_NOT_RESUME_ACTIVE_SUCCESSOR")
	}
	if successor.SchedulingSnapshot == nil || successor.AuthoritativePlan == nil {
		t.Fatal("STUDY_MATERIALIZER.BOUNDARY.CREATED_AUTHORITY_MISSING")
	}
	baseSnapshot := *successor.SchedulingSnapshot
	baseSnapshot.Cards = append([]study.StudySchedulingCard(nil), successor.SchedulingSnapshot.Cards...)
	baseSnapshot.PredecessorTailCardIdentity = nil
	canonicalScheduler, schedulerErr := study.NewVarietyFirstV1()
	if schedulerErr != nil {
		t.Fatalf("construct boundary scheduler: %v", schedulerErr)
	}
	basePlan, schedulerErr := canonicalScheduler.Plan(baseSnapshot)
	if schedulerErr != nil || len(basePlan.Cards) != 2 || basePlan.Cards[0].CardIdentity != predecessorTailItem.CardIdentity {
		t.Fatal("STUDY_MATERIALIZER.BOUNDARY.BASE_HEAD_NOT_PREDECESSOR_TAIL")
	}
	actual := successor.Persistence.Items
	if actual[0].CardIdentity != otherItem.CardIdentity || actual[1].CardIdentity != predecessorTailItem.CardIdentity ||
		actual[0].SelectionReason.RatingRank != model.StudyRatingRankUnseenUnrated ||
		actual[1].SelectionReason.RatingRank != model.StudyRatingRankUnseenUnrated ||
		actual[0].SelectionReason.LatestPresentedIngestionOrdinal == nil || *actual[0].SelectionReason.LatestPresentedIngestionOrdinal != 2 ||
		actual[1].SelectionReason.LatestPresentedIngestionOrdinal == nil || *actual[1].SelectionReason.LatestPresentedIngestionOrdinal != 1 ||
		actual[0].SelectionReason.BoundaryAdjustment != model.StudyBoundaryAdjustmentRotated ||
		actual[1].SelectionReason.BoundaryAdjustment != model.StudyBoundaryAdjustmentNone {
		t.Fatalf("STUDY_MATERIALIZER.BOUNDARY.WITHIN_RANK_ROTATION_INVALID: %#v", actual)
	}

	// Redirect the historical predecessor tail after issuance. Receipt replay
	// must return the immutable successor without resolving today's head.
	predecessorTail := predecessorTailItem.CardIdentity
	mergeTarget := target.ID
	if predecessorTail == target.ID {
		mergeTarget = source.ID
	}
	if _, err := repository.MergeIdea(ctx, predecessorTail.String(), mergeTarget.String()); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUNDARY.MERGE_FAILED", err))
	}
	replayed, err := repository.MaterializeStudyCycle(ctx, materializer, successorRequest)
	if err != nil || replayed == nil || replayed.Persistence.Session.ID != successor.Persistence.Session.ID ||
		replayed.SchedulingSnapshot != nil || !reflect.DeepEqual(replayed.AuthoritativePlan, successor.AuthoritativePlan) {
		t.Fatal("STUDY_MATERIALIZER.BOUNDARY.MERGE_CHANGED_ISSUED_AUTHORITY")
	}

	rootAsSuccessor := rootRequest
	otherRootPredecessor := root.Persistence.Session.ID
	rootAsSuccessor.Materialization.SessionID = uuid.New()
	rootAsSuccessor.Materialization.PredecessorSessionID = &otherRootPredecessor
	rootAsSuccessor.Materialization.PredecessorFlushReceiptID = uuid.New()
	rootAsSuccessor.Materialization.PredecessorTerminalReasonCode = "cycle_completed"
	if confused, confusedErr := repository.MaterializeStudyCycle(ctx, materializer, rootAsSuccessor); confused != nil || !isStudyMaterializationCode(confusedErr, model.StudyErrorCodeIdempotencyConflict) {
		t.Fatal("STUDY_MATERIALIZER.BOUNDARY.ROOT_RECEIPT_SATISFIED_SUCCESSOR")
	}
	successorAsRoot := successorRequest
	successorAsRoot.Materialization.PredecessorSessionID = nil
	successorAsRoot.Materialization.PredecessorFlushReceiptID = uuid.Nil
	if confused, confusedErr := repository.MaterializeStudyCycle(ctx, materializer, successorAsRoot); confused != nil || !isStudyMaterializationCode(confusedErr, model.StudyErrorCodeIdempotencyConflict) {
		t.Fatal("STUDY_MATERIALIZER.BOUNDARY.SUCCESSOR_RECEIPT_SATISFIED_ROOT")
	}
	otherPredecessor := uuid.New()
	crossPredecessor := successorRequest
	crossPredecessor.Materialization.PredecessorSessionID = &otherPredecessor
	if confused, confusedErr := repository.MaterializeStudyCycle(ctx, materializer, crossPredecessor); confused != nil ||
		(!isStudyMaterializationCode(confusedErr, model.StudyErrorCodeIdempotencyConflict) &&
			!isStudyMaterializationCode(confusedErr, model.StudyErrorCodeSessionNotFound)) {
		t.Fatal("STUDY_MATERIALIZER.BOUNDARY.SUCCESSOR_RECEIPT_CROSSED_PREDECESSOR")
	}
	selfPredecessor := successorRequest
	selfPredecessorID := successor.Persistence.Session.ID
	selfPredecessor.Materialization.SessionID = uuid.New()
	selfPredecessor.Materialization.PredecessorSessionID = &selfPredecessorID
	if confused, confusedErr := repository.MaterializeStudyCycle(ctx, materializer, selfPredecessor); confused != nil || !isStudyMaterializationCode(confusedErr, model.StudyErrorCodeIdempotencyConflict) {
		t.Fatal("STUDY_MATERIALIZER.BOUNDARY.SUCCESSOR_RECEIPT_ACCEPTED_OWN_ID")
	}
	zeroFlush := successorRequest
	zeroFlush.Materialization.PredecessorFlushReceiptID = uuid.Nil
	if confused, confusedErr := repository.MaterializeStudyCycle(ctx, materializer, zeroFlush); confused != nil || !isStudyMaterializationCode(confusedErr, model.StudyErrorCodeSchemaInvalid) {
		t.Fatal("STUDY_MATERIALIZER.BOUNDARY.EXPLICIT_PREDECESSOR_ACCEPTED_ZERO_FLUSH")
	}
}

func testStudyMaterializerWaitingResolution(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	materializer *study.CycleMaterializer,
) {
	t.Helper()
	suffix := stringsForStudyFixture(uuid.NewString())
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "materializer-waiting-" + suffix, Name: "Materializer waiting " + suffix})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.WAITING.TOPIC_CREATE_FAILED", err))
	}
	profileRef := "materializer-waiting-" + suffix
	scope := mustStudyScope(t, topic.ID, topic.Slug)
	waitingRequest := newStudyMaterializationRequest(profileRef, scope, 0, "waiting-root-"+suffix, nil, uuid.Nil)
	waiting, err := repository.MaterializeStudyCycle(ctx, materializer, waitingRequest)
	if err != nil || waiting == nil || waiting.Persistence.Session.Lifecycle != model.StudySessionLifecycleWaitingForCorpus {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.WAITING.CREATE_FAILED", err))
	}

	distinctEmpty := newStudyMaterializationRequest(profileRef, scope, 0, "waiting-distinct-empty-"+suffix, nil, uuid.Nil)
	converged, err := repository.MaterializeStudyCycle(ctx, materializer, distinctEmpty)
	if err != nil || converged == nil || converged.Persistence.Created ||
		converged.Persistence.Session.ID != waiting.Persistence.Session.ID ||
		converged.Persistence.Receipt.Disposition != model.StudyCreationDispositionConvergedToExisting {
		t.Fatal("STUDY_MATERIALIZER.WAITING.DISTINCT_EMPTY_DID_NOT_CAPTURE_AND_CONVERGE")
	}

	idea := mustCreateStudyIdea(t, ctx, repository, topic.Slug, "waiting-populated-"+suffix)
	replayedWaiting, err := repository.MaterializeStudyCycle(ctx, materializer, waitingRequest)
	if err != nil || replayedWaiting == nil || replayedWaiting.Persistence.Session.ID != waiting.Persistence.Session.ID ||
		replayedWaiting.Persistence.Session.Lifecycle != model.StudySessionLifecycleWaitingForCorpus || replayedWaiting.SchedulingSnapshot != nil {
		t.Fatal("STUDY_MATERIALIZER.WAITING.RECEIPT_DID_NOT_PRECEDE_NEW_CORPUS")
	}

	replacementRequest := newStudyMaterializationRequest(profileRef, scope, 1, "waiting-replacement-"+suffix, nil, uuid.Nil)
	replacement, err := repository.MaterializeStudyCycle(ctx, materializer, replacementRequest)
	if err != nil || replacement == nil || !replacement.Persistence.Created ||
		replacement.Persistence.Session.Lifecycle != model.StudySessionLifecycleActive ||
		replacement.Persistence.Session.PredecessorStudySessionID == nil ||
		*replacement.Persistence.Session.PredecessorStudySessionID != waiting.Persistence.Session.ID ||
		len(replacement.Persistence.Items) != 1 || replacement.Persistence.Items[0].CardIdentity != idea.ID {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.WAITING.REPLACEMENT_FAILED", err))
	}
	formerWaiting, err := repository.GetStudySession(ctx, profileRef, waiting.Persistence.Session.ID)
	if err != nil || formerWaiting.Lifecycle != model.StudySessionLifecycleSupersededEmpty ||
		formerWaiting.SuccessorStudySessionID == nil || *formerWaiting.SuccessorStudySessionID != replacement.Persistence.Session.ID {
		t.Fatal("STUDY_MATERIALIZER.WAITING.REPLACEMENT_NOT_ATOMIC")
	}
	replayedReplacement, err := repository.MaterializeStudyCycle(ctx, materializer, replacementRequest)
	if err != nil || replayedReplacement == nil || replayedReplacement.Persistence.Created ||
		replayedReplacement.Persistence.Session.ID != replacement.Persistence.Session.ID ||
		replayedReplacement.Persistence.Receipt.Disposition != model.StudyCreationDispositionSuccessor ||
		replayedReplacement.SchedulingSnapshot != nil ||
		!reflect.DeepEqual(replayedReplacement.AuthoritativePlan, replacement.AuthoritativePlan) {
		t.Fatal("STUDY_MATERIALIZER.WAITING.REPLACEMENT_REQUEST_REPLAY_FAILED")
	}
	freshRoot := newStudyMaterializationRequest(profileRef, scope, 0, "waiting-fresh-root-"+suffix, nil, uuid.Nil)
	rootConverged, err := repository.MaterializeStudyCycle(ctx, materializer, freshRoot)
	if err != nil || rootConverged == nil || rootConverged.Persistence.Created ||
		rootConverged.Persistence.Session.ID != replacement.Persistence.Session.ID ||
		rootConverged.Persistence.Receipt.Disposition != model.StudyCreationDispositionConvergedToExisting ||
		rootConverged.SchedulingSnapshot != nil {
		t.Fatal("STUDY_MATERIALIZER.WAITING.FRESH_ROOT_DID_NOT_RESUME_REPLACEMENT")
	}
	explicitWaiting := newStudyMaterializationRequest(
		profileRef,
		scope,
		1,
		"waiting-explicit-predecessor-"+suffix,
		&waiting.Persistence.Session.ID,
		uuid.New(),
	)
	if result, explicitErr := repository.MaterializeStudyCycle(ctx, materializer, explicitWaiting); result != nil || !isStudyMaterializationCode(explicitErr, model.StudyErrorCodeSchemaInvalid) {
		t.Fatal("STUDY_MATERIALIZER.WAITING.EXPLICIT_WAITING_PREDECESSOR_ACCEPTED")
	}
}

func testStudyMaterializerStorageDigestVector(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	topicID := uuid.MustParse("10000000-0000-4000-8000-000000000001")
	ideaID := uuid.MustParse("20000000-0000-4000-8000-000000000001")
	nameID := uuid.MustParse("30000000-0000-4000-8000-000000000001")
	definitionID := uuid.MustParse("40000000-0000-4000-8000-000000000001")
	exampleID := uuid.MustParse("50000000-0000-4000-8000-000000000001")
	if _, err := pool.Exec(ctx, `ALTER TABLE ideas DISABLE TRIGGER ideas_set_updated_at`); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.DIGEST.DISABLE_TRIGGER_FAILED", err))
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `ALTER TABLE ideas ENABLE TRIGGER ideas_set_updated_at`)
	})
	vectorTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.DIGEST.FIXTURE_BEGIN_FAILED", err))
	}
	defer vectorTx.Rollback(ctx)
	vectorStatements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO topics (id, slug, name, created_at, updated_at) VALUES ($1, 'materializer-vector', 'Materializer vector', '2026-01-01T02:03:04.012345Z', '2026-01-01T02:03:04.012345Z')`, []any{topicID}},
		{`INSERT INTO ideas (id, summary, status, created_at, updated_at) VALUES ($1, 'Vector summary', 'active', '2026-01-02T03:04:05.123456Z', '2026-01-03T04:05:06.234567Z')`, []any{ideaID}},
		{`INSERT INTO names (id, idea_id, text, kind, sort_key, created_at, updated_at) VALUES ($1, $2, 'Vector canonical', 'canonical', 0, '2026-01-04T05:06:07.345678Z', '2026-01-05T06:07:08.456789Z')`, []any{nameID, ideaID}},
		{`UPDATE ideas SET canonical_name_id = $1, updated_at = '2026-01-03T04:05:06.234567Z' WHERE id = $2`, []any{nameID, ideaID}},
		{`INSERT INTO definitions (id, idea_id, body, is_primary, sort_key, created_at, updated_at) VALUES ($1, $2, 'Vector definition', true, 0, '2026-01-06T07:08:09.567890Z', '2026-01-07T08:09:10.678901Z')`, []any{definitionID, ideaID}},
		{`INSERT INTO examples (id, idea_id, body, sort_key, created_at, updated_at) VALUES ($1, $2, 'Vector example', 0, '2026-01-08T09:10:11.789012Z', '2026-01-09T10:11:12.890123Z')`, []any{exampleID, ideaID}},
		{`INSERT INTO idea_topics (idea_id, topic_id) VALUES ($1, $2)`, []any{ideaID, topicID}},
	}
	for _, statement := range vectorStatements {
		if _, err := vectorTx.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.DIGEST.FIXTURE_INSERT_FAILED", err))
		}
	}
	if err := vectorTx.Commit(ctx); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.DIGEST.FIXTURE_COMMIT_FAILED", err))
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE ideas ENABLE TRIGGER ideas_set_updated_at`); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.DIGEST.ENABLE_TRIGGER_FAILED", err))
	}

	scope := mustStudyScope(t, topicID, "materializer-vector")
	var captured study.CapturedEligibleCorpus
	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: "materializer-vector", Scope: scope}, func(transaction StudyStoreTransaction) error {
		adapter, adapterErr := NewStudyMaterializationAdapter(transaction)
		if adapterErr != nil {
			return adapterErr
		}
		captured, adapterErr = adapter.CaptureEligibleCorpus(ctx)
		return adapterErr
	})
	if err != nil || len(captured.Sources) != 1 {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.DIGEST.CAPTURE_FAILED", err))
	}
	source := captured.Sources[0]
	const (
		wantRevision = "study-card-source-v1:b0015e064cf76a08fa73cdcc5aa0f983378a6423d2db186191e9344e5886a432"
		wantContent  = "e389d7f84169b88f3c26fc902e1fc3b2ad2f793bffb43d015cc041064f04056d"
		wantCorpus   = "8a603e442b925002416ac94cc4e866d635764e0c401acbea7678cdf96c318c20"
	)
	if source.CardIdentity != ideaID || source.LiveSourceIdeaID != ideaID || source.HistoricalSourceIdentity != ideaID ||
		source.SourceContentRevision != wantRevision || source.SourceContentSHA256 != wantContent || captured.CorpusSHA256 != wantCorpus {
		t.Fatalf("STUDY_MATERIALIZER.DIGEST.FIXED_VECTOR_CHANGED: source=%#v corpus=%s", source, captured.CorpusSHA256)
	}
}

func testStudyMaterializerStorageBounds(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	suffix := stringsForStudyFixture(uuid.NewString())
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "materializer-bound-" + suffix, Name: "Materializer bound " + suffix})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUND.TOPIC_CREATE_FAILED", err))
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUND.FIXTURE_BEGIN_FAILED", err))
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE study_materialization_bound_fixture (
			idea_id uuid PRIMARY KEY,
			name_id uuid NOT NULL,
			ordinal bigint NOT NULL
		) ON COMMIT DROP`); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUND.FIXTURE_TABLE_FAILED", err))
	}
	if _, err := tx.Exec(ctx, `INSERT INTO study_materialization_bound_fixture (idea_id, name_id, ordinal)
		SELECT gen_random_uuid(), gen_random_uuid(), ordinal
		FROM generate_series(1, $1) AS ordinal`, study.StudyMaximumSchedulingCards+1); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUND.FIXTURE_IDS_FAILED", err))
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ideas (id, summary, status)
		SELECT idea_id, 'Bound summary ' || ordinal, 'active'
		FROM study_materialization_bound_fixture`); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUND.FIXTURE_IDEAS_FAILED", err))
	}
	if _, err := tx.Exec(ctx, `INSERT INTO names (id, idea_id, text, kind)
		SELECT name_id, idea_id, 'Bound name ' || ordinal || ' ' || $1, 'canonical'
		FROM study_materialization_bound_fixture`, suffix); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUND.FIXTURE_NAMES_FAILED", err))
	}
	if _, err := tx.Exec(ctx, `UPDATE ideas AS idea
		SET canonical_name_id = fixture.name_id
		FROM study_materialization_bound_fixture AS fixture
		WHERE idea.id = fixture.idea_id`); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUND.FIXTURE_CANONICAL_FAILED", err))
	}
	if _, err := tx.Exec(ctx, `INSERT INTO idea_topics (idea_id, topic_id)
		SELECT idea_id, $1 FROM study_materialization_bound_fixture`, topic.ID); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUND.FIXTURE_TOPICS_FAILED", err))
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUND.FIXTURE_COMMIT_FAILED", err))
	}

	scope := mustStudyScope(t, topic.ID, topic.Slug)
	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: "materializer-bound-" + suffix, Scope: scope}, func(store StudyStoreTransaction) error {
		_, listErr := store.ListEligibleSources(ctx)
		if !isStudyMaterializationCode(listErr, model.StudyErrorCodeRequestTooLarge) {
			return fmt.Errorf("eligible bound error: %w", listErr)
		}
		if _, digestErr := store.CapturedEligibleCorpusSHA256(); !isStudyMaterializationCode(digestErr, model.StudyErrorCodeStateStale) {
			return fmt.Errorf("over-bound capture established digest authority: %w", digestErr)
		}
		return listErr
	})
	if !isStudyMaterializationCode(err, model.StudyErrorCodeRequestTooLarge) {
		t.Fatal("STUDY_MATERIALIZER.BOUND.ELIGIBLE_NOT_REJECTED_BEFORE_MATERIALIZATION")
	}

	emptyTopic, err := repository.CreateTopic(ctx, model.CreateTopicInput{Slug: "materializer-prefix-bound-" + suffix, Name: "Materializer prefix bound " + suffix})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUND.PREFIX_TOPIC_CREATE_FAILED", err))
	}
	emptyScope := mustStudyScope(t, emptyTopic.ID, emptyTopic.Slug)
	profileRef := "materializer-prefix-bound-" + suffix
	if err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: emptyScope}, func(StudyStoreTransaction) error { return nil }); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUND.PREFIX_PROFILE_CREATE_FAILED", err))
	}
	if _, err := pool.Exec(ctx, `UPDATE study_learner_profiles SET profile_ingestion_high_water = $2 WHERE learner_profile_ref = $1`, profileRef, study.StudyProjectionAcceptedEventLimit+1); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_MATERIALIZER.BOUND.PREFIX_PROFILE_UPDATE_FAILED", err))
	}
	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: emptyScope}, func(store StudyStoreTransaction) error {
		_, prefixErr := store.ReadAcceptedPrefix(ctx, study.StudyProjectionAcceptedEventLimit+1)
		return prefixErr
	})
	if !isStudyMaterializationCode(err, model.StudyErrorCodeRequestTooLarge) {
		t.Fatal("STUDY_MATERIALIZER.BOUND.PREFIX_NOT_REJECTED_BEFORE_QUERY")
	}
}

func newStudyMaterializerIntegrationSubject(t *testing.T) *study.CycleMaterializer {
	t.Helper()
	scheduler, err := study.NewVarietyFirstV1()
	if err != nil {
		t.Fatalf("construct production scheduler: %v", err)
	}
	allocator := study.StudyItemIDAllocatorFunc(func(_ context.Context, requestedCount int) ([]uuid.UUID, error) {
		identities := make([]uuid.UUID, requestedCount)
		for index := range identities {
			identities[index] = uuid.New()
		}
		return identities, nil
	})
	materializer, err := study.NewCanonicalCycleMaterializer(scheduler, allocator)
	if err != nil {
		t.Fatalf("construct production materializer: %v", err)
	}
	return materializer
}

func newStudyMaterializationRequest(
	profileRef string,
	scope model.StudyNormalizedScope,
	_ int,
	label string,
	predecessorID *uuid.UUID,
	flushReceiptID uuid.UUID,
) StudyCycleMaterializationRequest {
	return StudyCycleMaterializationRequest{
		Transaction: StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope},
		Materialization: study.CycleMaterializationInput{
			SessionID:                     uuid.New(),
			Principal:                     model.OperatorStudyPrincipalID(),
			CreationRequestID:             uuid.New(),
			NormalizedRequestSHA256:       studyMaterializationHash("request-" + label),
			PlannedAt:                     time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
			Seed:                          "seed-" + label,
			PredecessorSessionID:          cloneStudyUUIDPointer(predecessorID),
			PredecessorFlushReceiptID:     flushReceiptID,
			LifecycleReasonCode:           "materialized",
			PredecessorTerminalReasonCode: "cycle_completed",
		},
	}
}

func newStudyMaterializationEvent(
	profileRef string,
	item model.StudySessionItem,
	streamID uuid.UUID,
	sequence int64,
	eventID uuid.UUID,
	kind model.StudyEventKind,
	rating *model.StudyRating,
	ratingEventID *uuid.UUID,
) model.StudyEvent {
	event := model.StudyEvent{
		ID:                 eventID,
		ClientSequence:     sequence,
		ProfileRef:         profileRef,
		StudySessionItemID: item.ID,
		CardIdentity:       item.CardIdentity,
		ContentSHA256:      item.SourceContentSHA256,
		Kind:               kind,
		ClientClockQuality: model.StudyClientClockQualityUnknown,
		ClientStreamID:     streamID,
		RatingEventID:      cloneStudyUUIDPointer(ratingEventID),
	}
	if rating != nil {
		copyOfRating := *rating
		event.Rating = &copyOfRating
	}
	return event
}

func findStudyMaterializationBoundarySeed(
	t *testing.T,
	session model.StudySession,
	predecessorTail uuid.UUID,
	other uuid.UUID,
	highWater int64,
	plannedAt time.Time,
) string {
	t.Helper()
	scheduler, err := study.NewVarietyFirstV1()
	if err != nil {
		t.Fatalf("construct boundary seed scheduler: %v", err)
	}
	tailOrdinal := int64(1)
	otherOrdinal := int64(2)
	for candidate := 0; candidate < 128; candidate++ {
		seed := fmt.Sprintf("boundary-seed-%03d", candidate)
		plan, planErr := scheduler.Plan(study.StudySchedulingSnapshot{
			LearnerProfileID:               session.LearnerProfileID,
			CardKind:                       session.Scope.Scope.CardKind,
			CorpusSHA256:                   session.CorpusSHA256,
			InputProfileIngestionHighWater: highWater,
			PlannedAt:                      plannedAt,
			Seed:                           seed,
			Cards: []study.StudySchedulingCard{
				{CardIdentity: predecessorTail, Projection: study.StudyProjectionTail{LatestPresentedIngestionOrdinal: &tailOrdinal}},
				{CardIdentity: other, Projection: study.StudyProjectionTail{LatestPresentedIngestionOrdinal: &otherOrdinal}},
			},
		})
		if planErr == nil && len(plan.Cards) == 2 && plan.Cards[0].CardIdentity == predecessorTail {
			return seed
		}
	}
	t.Fatal("STUDY_MATERIALIZER.BOUNDARY.SEED_NOT_FOUND")
	return ""
}

func assertStudyMaterializationCoverage(t *testing.T, items []model.StudySessionItem, identities []uuid.UUID) {
	t.Helper()
	want := make(map[uuid.UUID]struct{}, len(identities))
	for _, identity := range identities {
		want[identity] = struct{}{}
	}
	seenItems := make(map[uuid.UUID]struct{}, len(items))
	seenCards := make(map[uuid.UUID]struct{}, len(items))
	for position, item := range items {
		if item.Position != position || item.ID == uuid.Nil || item.StudySessionID == uuid.Nil {
			t.Fatal("STUDY_MATERIALIZER.COVERAGE.ORDER_OR_ID_INVALID")
		}
		if _, duplicate := seenItems[item.ID]; duplicate {
			t.Fatal("STUDY_MATERIALIZER.COVERAGE.DUPLICATE_ITEM")
		}
		if _, duplicate := seenCards[item.CardIdentity]; duplicate {
			t.Fatal("STUDY_MATERIALIZER.COVERAGE.DUPLICATE_CARD")
		}
		if _, expected := want[item.CardIdentity]; !expected {
			t.Fatal("STUDY_MATERIALIZER.COVERAGE.UNKNOWN_CARD")
		}
		seenItems[item.ID] = struct{}{}
		seenCards[item.CardIdentity] = struct{}{}
	}
	if len(seenCards) != len(want) {
		t.Fatal("STUDY_MATERIALIZER.COVERAGE.INCOMPLETE")
	}
}

func studyMaterializationHash(label string) string {
	return model.SHA256Hex([]byte("study-materialization:" + label))
}

func isStudyMaterializationCode(err error, code model.StudyErrorCode) bool {
	var studyError *model.StudyError
	return errors.As(err, &studyError) && studyError.Code == code
}
