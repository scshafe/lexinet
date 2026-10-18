package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lexinet/internal/model"
)

const adversarialWorkerCleanupTimeout = 5 * time.Second

func TestStudyRepositoryAdversarialContracts(t *testing.T) {
	harness := startStudyTestPostgres(t)
	harness.migrateUp(t)
	pool := harness.openPool(t)
	repository := NewRepository(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	t.Run("insert_plan_once_rejects_every_predecessor", func(t *testing.T) {
		testInsertPlanOnceRejectsEveryPredecessor(t, ctx, pool, repository)
	})
	t.Run("generic_transition_reserves_successor_terminal_states", func(t *testing.T) {
		testGenericTransitionReservesSuccessorTerminalStates(t, ctx, repository)
	})
	t.Run("plan_insert_revalidates_every_live_source", func(t *testing.T) {
		testPlanInsertRevalidatesEveryLiveSource(t, ctx, pool, repository)
	})
	t.Run("plan_insert_requires_captured_eligibility", func(t *testing.T) {
		testPlanInsertRequiresCapturedEligibility(t, ctx, pool, repository)
	})
	t.Run("waiting_plan_requires_captured_empty_corpus", func(t *testing.T) {
		testWaitingPlanRequiresCapturedEmptyCorpus(t, ctx, pool, repository)
	})
	t.Run("eligibility_snapshot_linearizes_source_deletions", func(t *testing.T) {
		testEligibilitySnapshotLinearizesSourceDeletions(t, ctx, pool, repository)
	})
	t.Run("eligibility_snapshot_blocks_merge_until_plan_commit", func(t *testing.T) {
		testEligibilitySnapshotBlocksMergeUntilPlanCommit(t, ctx, pool, repository)
	})
	t.Run("replace_projection_cannot_recreate_historical_identity", func(t *testing.T) {
		testReplaceProjectionCannotRecreateHistoricalIdentity(t, ctx, pool, repository)
	})
	t.Run("unequal_projection_prefix_merge_invalidates_projections", func(t *testing.T) {
		testUnequalProjectionPrefixMergeInvalidatesProjections(t, ctx, pool, repository)
	})
	t.Run("repeated_and_reverse_merge_return_survivor", func(t *testing.T) {
		testRepeatedAndReverseMergeReturnSurvivor(t, ctx, pool, repository)
	})
}

func testInsertPlanOnceRejectsEveryPredecessor(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	topic := mustCreateAdversarialTopic(t, ctx, repository, "predecessor")
	source := mustCreateAdversarialIdea(t, ctx, repository, topic.Slug, "predecessor-source")
	scope := mustAdversarialStudyScope(t, topic)
	principal := model.OperatorStudyPrincipalID()

	t.Run("existing_predecessor", func(t *testing.T) {
		profileRef := adversarialProfileRef("existing-predecessor")
		rootPlan := adversarialStudyPlan(principal, source.ID, "existing-root")
		root := mustInsertAdversarialPlan(t, ctx, repository, profileRef, scope, rootPlan)

		attempt := adversarialStudyPlan(principal, source.ID, "existing-successor")
		attempt.PredecessorSessionID = &root.Session.ID
		err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
			_, insertErr := store.InsertPlanOnce(ctx, attempt)
			return insertErr
		})
		assertAdversarialStudyError(t, err, model.StudyErrorCodeFlushReceiptRequired, false)
		assertAdversarialPlanAbsent(t, ctx, pool, attempt)
	})

	t.Run("arbitrary_predecessor", func(t *testing.T) {
		profileRef := adversarialProfileRef("arbitrary-predecessor")
		attempt := adversarialStudyPlan(principal, source.ID, "arbitrary-successor")
		arbitraryPredecessorID := uuid.New()
		attempt.PredecessorSessionID = &arbitraryPredecessorID
		err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
			_, insertErr := store.InsertPlanOnce(ctx, attempt)
			return insertErr
		})
		assertAdversarialStudyError(t, err, model.StudyErrorCodeFlushReceiptRequired, false)
		assertAdversarialPlanAbsent(t, ctx, pool, attempt)
	})
}

func testGenericTransitionReservesSuccessorTerminalStates(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
) {
	t.Helper()
	principal := model.OperatorStudyPrincipalID()
	testCases := []struct {
		name              string
		to                model.StudySessionLifecycle
		expectedErrorCode model.StudyErrorCode
	}{
		{name: "completed_requires_atomic_successor", to: model.StudySessionLifecycleCompleted, expectedErrorCode: model.StudyErrorCodeFlushReceiptRequired},
		{name: "superseded_empty_requires_atomic_successor", to: model.StudySessionLifecycleSupersededEmpty, expectedErrorCode: model.StudyErrorCodeFlushReceiptRequired},
		{name: "abandoned_remains_generic", to: model.StudySessionLifecycleAbandoned},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			topic := mustCreateAdversarialTopic(t, ctx, repository, testCase.name)
			source := mustCreateAdversarialIdea(t, ctx, repository, topic.Slug, testCase.name+"-source")
			scope := mustAdversarialStudyScope(t, topic)
			profileRef := adversarialProfileRef(testCase.name)
			root := mustInsertAdversarialPlan(
				t,
				ctx,
				repository,
				profileRef,
				scope,
				adversarialStudyPlan(principal, source.ID, testCase.name),
			)

			var transitioned *model.StudySession
			err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
				var transitionErr error
				transitioned, transitionErr = store.TransitionSession(ctx, StudySessionTransition{
					SessionID:            root.Session.ID,
					ExpectedStateVersion: root.Session.StateVersion,
					To:                   testCase.to,
					ReasonCode:           "adversarial_transition",
					Actor:                principal,
				})
				return transitionErr
			})

			if testCase.expectedErrorCode != "" {
				assertAdversarialStudyError(t, err, testCase.expectedErrorCode, false)
				persisted, readErr := repository.GetStudySession(ctx, profileRef, root.Session.ID)
				if readErr != nil {
					t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.TRANSITION_REJECTION_READ_FAILED", readErr))
				}
				if persisted.Lifecycle != model.StudySessionLifecycleActive || persisted.StateVersion != root.Session.StateVersion {
					t.Fatal("STUDY_ADVERSARIAL.RESERVED_TRANSITION_MUTATED_SESSION")
				}
				return
			}

			if err != nil {
				t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.ABANDONED_TRANSITION_FAILED", err))
			}
			if transitioned == nil || transitioned.Lifecycle != model.StudySessionLifecycleAbandoned || transitioned.StateVersion != root.Session.StateVersion+1 {
				t.Fatal("STUDY_ADVERSARIAL.ABANDONED_TRANSITION_NOT_PERSISTED")
			}
		})
	}
}

func testPlanInsertRevalidatesEveryLiveSource(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	principal := model.OperatorStudyPrincipalID()
	testCases := []struct {
		name         string
		expectedCode model.StudyErrorCode
		prepare      func(*testing.T, context.Context, *Repository, *model.Topic) StudyPlanInsert
	}{
		{
			name:         "arbitrary_unknown_source",
			expectedCode: model.StudyErrorCodeStateStale,
			prepare: func(_ *testing.T, _ context.Context, _ *Repository, _ *model.Topic) StudyPlanInsert {
				return adversarialStudyPlan(principal, uuid.New(), "unknown-source")
			},
		},
		{
			name:         "mismatched_source_identities",
			expectedCode: model.StudyErrorCodeSchemaInvalid,
			prepare: func(t *testing.T, ctx context.Context, repository *Repository, topic *model.Topic) StudyPlanInsert {
				source := mustCreateAdversarialIdea(t, ctx, repository, topic.Slug, "mismatch-source")
				plan := adversarialStudyPlan(principal, source.ID, "mismatch")
				plan.Items[0].CardIdentity = uuid.New()
				return plan
			},
		},
		{
			name:         "out_of_scope_source",
			expectedCode: model.StudyErrorCodeStateStale,
			prepare: func(t *testing.T, ctx context.Context, repository *Repository, _ *model.Topic) StudyPlanInsert {
				source := mustCreateAdversarialIdea(t, ctx, repository, "", "out-of-scope-source")
				return adversarialStudyPlan(principal, source.ID, "out-of-scope")
			},
		},
		{
			name:         "merged_live_source",
			expectedCode: model.StudyErrorCodeStateStale,
			prepare: func(t *testing.T, ctx context.Context, repository *Repository, topic *model.Topic) StudyPlanInsert {
				source := mustCreateAdversarialIdea(t, ctx, repository, topic.Slug, "merged-source")
				target := mustCreateAdversarialIdea(t, ctx, repository, "", "merged-target")
				if _, err := repository.MergeIdea(ctx, source.ID.String(), target.ID.String()); err != nil {
					t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.SOURCE_PREPARE_MERGE_FAILED", err))
				}
				return adversarialStudyPlan(principal, source.ID, "merged-source")
			},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			topic := mustCreateAdversarialTopic(t, ctx, repository, testCase.name)
			scope := mustAdversarialStudyScope(t, topic)
			plan := testCase.prepare(t, ctx, repository, topic)
			profileRef := adversarialProfileRef(testCase.name)
			err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
				if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
					return listErr
				}
				_, insertErr := store.InsertPlanOnce(ctx, plan)
				return insertErr
			})
			assertAdversarialStudyError(t, err, testCase.expectedCode, false)
			assertAdversarialPlanAbsent(t, ctx, pool, plan)
		})
	}
}

func testPlanInsertRequiresCapturedEligibility(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	principal := model.OperatorStudyPrincipalID()

	t.Run("active_without_capture", func(t *testing.T) {
		topic := mustCreateAdversarialTopic(t, ctx, repository, "active-without-capture")
		source := mustCreateAdversarialIdea(t, ctx, repository, topic.Slug, "active-without-capture-source")
		scope := mustAdversarialStudyScope(t, topic)
		profileRef := adversarialProfileRef("active-without-capture")
		plan := adversarialStudyPlan(principal, source.ID, "active-without-capture")
		err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
			_, insertErr := store.InsertPlanOnce(ctx, plan)
			return insertErr
		})
		assertAdversarialStudyError(t, err, model.StudyErrorCodeStateStale, false)
		assertAdversarialPlanTransactionRolledBack(t, ctx, pool, profileRef, plan)
	})

	t.Run("captured_identity_mismatch", func(t *testing.T) {
		topic := mustCreateAdversarialTopic(t, ctx, repository, "captured-identity-mismatch")
		mustCreateAdversarialIdea(t, ctx, repository, topic.Slug, "captured-identity-source")
		scope := mustAdversarialStudyScope(t, topic)
		profileRef := adversarialProfileRef("captured-identity-mismatch")
		plan := adversarialStudyPlan(principal, uuid.New(), "captured-identity-mismatch")
		err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
			if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
				return listErr
			}
			_, insertErr := store.InsertPlanOnce(ctx, plan)
			return insertErr
		})
		assertAdversarialStudyError(t, err, model.StudyErrorCodeStateStale, false)
		assertAdversarialPlanTransactionRolledBack(t, ctx, pool, profileRef, plan)
	})

	t.Run("malformed_structure_after_capture", func(t *testing.T) {
		topic := mustCreateAdversarialTopic(t, ctx, repository, "malformed-captured-item")
		source := mustCreateAdversarialIdea(t, ctx, repository, topic.Slug, "malformed-captured-source")
		scope := mustAdversarialStudyScope(t, topic)
		profileRef := adversarialProfileRef("malformed-captured-item")
		plan := adversarialStudyPlan(principal, source.ID, "malformed-captured-item")
		plan.Items[0].Position = 1
		err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
			if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
				return listErr
			}
			_, insertErr := store.InsertPlanOnce(ctx, plan)
			return insertErr
		})
		assertAdversarialStudyError(t, err, model.StudyErrorCodeSchemaInvalid, false)
		assertAdversarialPlanTransactionRolledBack(t, ctx, pool, profileRef, plan)
	})

	t.Run("hostile_content_is_replaced_by_captured_source", func(t *testing.T) {
		topic := mustCreateAdversarialTopic(t, ctx, repository, "hostile-captured-content")
		source := mustCreateAdversarialIdea(t, ctx, repository, topic.Slug, "hostile-captured-content-source")
		scope := mustAdversarialStudyScope(t, topic)
		profileRef := adversarialProfileRef("hostile-captured-content")
		plan := adversarialStudyPlan(principal, source.ID, "hostile-captured-content")
		hostileExample := "HOSTILE_CALLER_EXAMPLE_CANARY"
		plan.Items[0].SourceContentRevision = "hostile-caller-revision"
		plan.Items[0].SourceContentSHA256 = model.SHA256Hex([]byte("hostile-caller-content"))
		plan.Items[0].FrontCanonicalName = "HOSTILE_CALLER_FRONT_CANARY"
		plan.Items[0].BackDefinition = "HOSTILE_CALLER_BACK_CANARY"
		plan.Items[0].BackExample = &hostileExample

		var captured StudyEligibleSource
		var result *StudyPlanInsertResult
		err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
			sources, listErr := store.ListEligibleSources(ctx)
			if listErr != nil {
				return listErr
			}
			if len(sources) != 1 || sources[0].IdeaID != source.ID {
				return errors.New("STUDY_ADVERSARIAL.HOSTILE_CAPTURE_SOURCE_UNEXPECTED")
			}
			captured = sources[0]
			var insertErr error
			result, insertErr = store.InsertPlanOnce(ctx, plan)
			return insertErr
		})
		if err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.HOSTILE_CAPTURE_INSERT_FAILED", err))
		}
		if result == nil || len(result.Items) != 1 {
			t.Fatal("STUDY_ADVERSARIAL.HOSTILE_CAPTURE_RESULT_INVALID")
		}
		assertAdversarialItemMatchesEligibleSource(t, result.Items[0], captured, source.ID)
		persisted := mustReadAdversarialPlanItem(t, ctx, repository, profileRef, plan.SessionID)
		assertAdversarialItemMatchesEligibleSource(t, persisted, captured, source.ID)
		if persisted.FrontCanonicalName == plan.Items[0].FrontCanonicalName ||
			persisted.BackDefinition == plan.Items[0].BackDefinition ||
			persisted.SourceContentRevision == plan.Items[0].SourceContentRevision ||
			persisted.SourceContentSHA256 == plan.Items[0].SourceContentSHA256 {
			t.Fatal("STUDY_ADVERSARIAL.HOSTILE_CALLER_SNAPSHOT_PERSISTED")
		}
	})
}

func testWaitingPlanRequiresCapturedEmptyCorpus(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	t.Run("without_capture", func(t *testing.T) {
		topic := mustCreateAdversarialTopic(t, ctx, repository, "waiting-without-capture")
		scope := mustAdversarialStudyScope(t, topic)
		profileRef := adversarialProfileRef("waiting-without-capture")
		plan := adversarialWaitingPlan("waiting-without-capture")
		err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
			_, insertErr := store.InsertPlanOnce(ctx, plan)
			return insertErr
		})
		assertAdversarialStudyError(t, err, model.StudyErrorCodeStateStale, false)
		assertAdversarialPlanTransactionRolledBack(t, ctx, pool, profileRef, plan)
	})

	t.Run("captured_nonempty_corpus", func(t *testing.T) {
		topic := mustCreateAdversarialTopic(t, ctx, repository, "waiting-nonempty-capture")
		mustCreateAdversarialIdea(t, ctx, repository, topic.Slug, "waiting-nonempty-source")
		scope := mustAdversarialStudyScope(t, topic)
		profileRef := adversarialProfileRef("waiting-nonempty-capture")
		plan := adversarialWaitingPlan("waiting-nonempty-capture")
		err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
			sources, listErr := store.ListEligibleSources(ctx)
			if listErr != nil {
				return listErr
			}
			if len(sources) != 1 {
				return errors.New("STUDY_ADVERSARIAL.WAITING_NONEMPTY_CAPTURE_UNEXPECTED")
			}
			_, insertErr := store.InsertPlanOnce(ctx, plan)
			return insertErr
		})
		assertAdversarialStudyError(t, err, model.StudyErrorCodeStateStale, false)
		assertAdversarialPlanTransactionRolledBack(t, ctx, pool, profileRef, plan)
	})

	t.Run("captured_empty_corpus", func(t *testing.T) {
		topic := mustCreateAdversarialTopic(t, ctx, repository, "waiting-empty-capture")
		scope := mustAdversarialStudyScope(t, topic)
		profileRef := adversarialProfileRef("waiting-empty-capture")
		plan := adversarialWaitingPlan("waiting-empty-capture")
		var result *StudyPlanInsertResult
		err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
			sources, listErr := store.ListEligibleSources(ctx)
			if listErr != nil {
				return listErr
			}
			if len(sources) != 0 {
				return errors.New("STUDY_ADVERSARIAL.WAITING_EMPTY_CAPTURE_NOT_EMPTY")
			}
			var insertErr error
			result, insertErr = store.InsertPlanOnce(ctx, plan)
			return insertErr
		})
		if err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.WAITING_EMPTY_INSERT_FAILED", err))
		}
		if result == nil || !result.Created || result.Session.Lifecycle != model.StudySessionLifecycleWaitingForCorpus ||
			result.Session.Scheduler != nil || len(result.Items) != 0 {
			t.Fatal("STUDY_ADVERSARIAL.WAITING_EMPTY_RESULT_INVALID")
		}
	})
}

func testEligibilitySnapshotLinearizesSourceDeletions(
	t *testing.T,
	parentContext context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	testCases := []struct {
		name                    string
		laterScopeMustFailStale bool
		prepare                 func(*testing.T) (*model.Topic, *model.Idea, func(context.Context, *Repository) error)
	}{
		{
			name: "definition_delete",
			prepare: func(t *testing.T) (*model.Topic, *model.Idea, func(context.Context, *Repository) error) {
				topic := mustCreateAdversarialTopic(t, parentContext, repository, "definition-delete-linearization")
				source, definition := mustCreateAdversarialDefinitionOnlyIdea(t, parentContext, repository, topic.Slug, "definition-delete-source")
				return topic, source, func(ctx context.Context, workerRepository *Repository) error {
					return workerRepository.DeleteDefinition(ctx, definition.ID)
				}
			},
		},
		{
			name:                    "topic_delete",
			laterScopeMustFailStale: true,
			prepare: func(t *testing.T) (*model.Topic, *model.Idea, func(context.Context, *Repository) error) {
				topic := mustCreateAdversarialTopic(t, parentContext, repository, "topic-delete-linearization")
				source := mustCreateAdversarialIdea(t, parentContext, repository, topic.Slug, "topic-delete-source")
				return topic, source, func(ctx context.Context, workerRepository *Repository) error {
					return workerRepository.DeleteTopic(ctx, topic.ID.String())
				}
			},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			topic, source, mutate := testCase.prepare(t)
			scope := mustAdversarialStudyScope(t, topic)
			profileRef := adversarialProfileRef(testCase.name)
			plan := adversarialStudyPlan(model.OperatorStudyPrincipalID(), source.ID, testCase.name)
			workerRepository, applicationName, workerPool := newAdversarialWorkerRepository(t, parentContext, pool, testCase.name)
			defer workerPool.Close()

			ctx, cancel := context.WithTimeout(parentContext, 20*time.Second)
			startMutation := make(chan struct{})
			mutationResult := make(chan error, 1)
			workerDone := make(chan struct{})
			go func() {
				defer close(workerDone)
				select {
				case <-startMutation:
				case <-ctx.Done():
					return
				}
				mutationResult <- mutate(ctx, workerRepository)
			}()
			defer cleanupAdversarialWorker(t, cancel, workerDone)

			var captured StudyEligibleSource
			var result *StudyPlanInsertResult
			err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
				sources, listErr := store.ListEligibleSources(ctx)
				if listErr != nil {
					return listErr
				}
				if len(sources) != 1 || sources[0].IdeaID != source.ID {
					return errors.New("STUDY_ADVERSARIAL.DELETE_CAPTURE_SOURCE_UNEXPECTED")
				}
				captured = sources[0]
				close(startMutation)
				if waitErr := waitForAdversarialPostgresLock(ctx, pool, applicationName, workerDone); waitErr != nil {
					return waitErr
				}
				var insertErr error
				result, insertErr = store.InsertPlanOnce(ctx, plan)
				return insertErr
			})
			if err != nil {
				t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.DELETE_LINEARIZATION_PLAN_FAILED", err))
			}
			select {
			case mutationErr := <-mutationResult:
				if mutationErr != nil {
					t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.DELETE_AFTER_PLAN_FAILED", mutationErr))
				}
			case <-ctx.Done():
				t.Fatal("STUDY_ADVERSARIAL.DELETE_AFTER_PLAN_TIMEOUT")
			}
			if result == nil || len(result.Items) != 1 {
				t.Fatal("STUDY_ADVERSARIAL.DELETE_LINEARIZATION_RESULT_INVALID")
			}
			assertAdversarialItemMatchesEligibleSource(t, result.Items[0], captured, source.ID)
			persisted := mustReadAdversarialPlanItem(t, parentContext, repository, profileRef, plan.SessionID)
			assertAdversarialItemMatchesEligibleSource(t, persisted, captured, source.ID)
			if testCase.laterScopeMustFailStale {
				assertAdversarialLaterEligibilityStateStale(t, parentContext, repository, scope, testCase.name)
			} else {
				assertAdversarialLaterEligibilityEmpty(t, parentContext, repository, scope, testCase.name)
			}
		})
	}
}

func testEligibilitySnapshotBlocksMergeUntilPlanCommit(
	t *testing.T,
	parentContext context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	topic := mustCreateAdversarialTopic(t, parentContext, repository, "merge-plan-barrier")
	source := mustCreateAdversarialIdea(t, parentContext, repository, topic.Slug, "merge-plan-source")
	target := mustCreateAdversarialIdea(t, parentContext, repository, "", "merge-plan-target")
	scope := mustAdversarialStudyScope(t, topic)
	plan := adversarialStudyPlan(model.OperatorStudyPrincipalID(), source.ID, "merge-plan")
	profileRef := adversarialProfileRef("merge-plan")
	workerRepository, applicationName, workerPool := newAdversarialWorkerRepository(t, parentContext, pool, "merge-plan")
	defer workerPool.Close()

	ctx, cancel := context.WithTimeout(parentContext, 20*time.Second)
	startMerge := make(chan struct{})
	type mergeOutcome struct {
		idea *model.Idea
		err  error
	}
	mergeCompleted := make(chan mergeOutcome, 1)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		select {
		case <-startMerge:
		case <-ctx.Done():
			return
		}
		idea, err := workerRepository.MergeIdea(ctx, source.ID.String(), target.ID.String())
		mergeCompleted <- mergeOutcome{idea: idea, err: err}
	}()
	defer cleanupAdversarialWorker(t, cancel, workerDone)

	var captured StudyEligibleSource
	var result *StudyPlanInsertResult
	err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		sources, listErr := store.ListEligibleSources(ctx)
		if listErr != nil {
			return listErr
		}
		if len(sources) != 1 || sources[0].IdeaID != source.ID {
			return errors.New("STUDY_ADVERSARIAL.MERGE_CAPTURE_SOURCE_UNEXPECTED")
		}
		captured = sources[0]
		close(startMerge)
		if waitErr := waitForAdversarialPostgresLock(ctx, pool, applicationName, workerDone); waitErr != nil {
			return waitErr
		}
		var insertErr error
		result, insertErr = store.InsertPlanOnce(ctx, plan)
		return insertErr
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.MERGE_LINEARIZATION_PLAN_FAILED", err))
	}
	select {
	case outcome := <-mergeCompleted:
		if outcome.err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.MERGE_AFTER_PLAN_FAILED", outcome.err))
		}
		if outcome.idea == nil || outcome.idea.ID != target.ID {
			t.Fatal("STUDY_ADVERSARIAL.CONCURRENT_MERGE_SURVIVOR_INVALID")
		}
	case <-ctx.Done():
		t.Fatal("STUDY_ADVERSARIAL.MERGE_AFTER_PLAN_TIMEOUT")
	}
	if result == nil || len(result.Items) != 1 {
		t.Fatal("STUDY_ADVERSARIAL.MERGE_LINEARIZATION_RESULT_INVALID")
	}
	assertAdversarialItemMatchesEligibleSource(t, result.Items[0], captured, source.ID)
	persisted := mustReadAdversarialPlanItem(t, parentContext, repository, profileRef, plan.SessionID)
	assertAdversarialFrozenItemMatchesEligibleSource(t, persisted, captured)
	if persisted.LiveSourceIdeaID == nil || *persisted.LiveSourceIdeaID != target.ID {
		t.Fatal("STUDY_ADVERSARIAL.MERGE_DID_NOT_REPOINT_ISSUED_ITEM")
	}
	resolved, resolveErr := repository.ResolveIdeaID(parentContext, source.ID.String())
	if resolveErr != nil || resolved != target.ID {
		t.Fatal("STUDY_ADVERSARIAL.MERGE_PLAN_BARRIER_DID_NOT_COMMIT_MERGE")
	}
}

func testReplaceProjectionCannotRecreateHistoricalIdentity(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	topic := mustCreateAdversarialTopic(t, ctx, repository, "projection-history")
	source := mustCreateAdversarialIdea(t, ctx, repository, topic.Slug, "projection-history-source")
	target := mustCreateAdversarialIdea(t, ctx, repository, "", "projection-history-target")
	scope := mustAdversarialStudyScope(t, topic)
	profileRef := adversarialProfileRef("projection-history")
	var sourceProjection, targetProjection model.StudySchedulerCardProjection
	err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		sourceProjection = adversarialProjection(store.Profile().ID, source.ID, 0, "source-before-merge")
		targetProjection = adversarialProjection(store.Profile().ID, target.ID, 0, "target-before-merge")
		if _, replaceErr := store.ReplaceProjection(ctx, sourceProjection, 0); replaceErr != nil {
			return replaceErr
		}
		_, replaceErr := store.ReplaceProjection(ctx, targetProjection, 0)
		return replaceErr
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.PROJECTION_FIXTURE_INSERT_FAILED", err))
	}
	assertAdversarialOpaqueProjectionCount(t, ctx, pool, sourceProjection.LearnerProfileID, source.ID, target.ID, 2)
	if _, err := repository.MergeIdea(ctx, source.ID.String(), target.ID.String()); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.PROJECTION_FIXTURE_MERGE_FAILED", err))
	}

	err = repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		_, replaceErr := store.ReplaceProjection(ctx, sourceProjection, 0)
		return replaceErr
	})
	assertAdversarialStudyError(t, err, model.StudyErrorCodeStateStale, false)

	var historicalCount, survivorCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE projected_study_card_identity = $2),
			count(*) FILTER (WHERE projected_study_card_identity = $3)
		FROM study_scheduler_card_projections
		WHERE learner_profile_id = $1`, sourceProjection.LearnerProfileID, source.ID, target.ID).Scan(
		&historicalCount,
		&survivorCount,
	); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.PROJECTION_IDENTITY_COUNT_FAILED", err))
	}
	if historicalCount != 0 || survivorCount != 0 {
		t.Fatal("STUDY_ADVERSARIAL.MERGE_FABRICATED_SURVIVOR_PROJECTION")
	}
	assertAdversarialOpaqueProjectionCount(t, ctx, pool, sourceProjection.LearnerProfileID, source.ID, target.ID, 0)
}

func testUnequalProjectionPrefixMergeInvalidatesProjections(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	topic := mustCreateAdversarialTopic(t, ctx, repository, "unequal-prefix")
	source := mustCreateAdversarialIdea(t, ctx, repository, topic.Slug, "unequal-prefix-source")
	target := mustCreateAdversarialIdea(t, ctx, repository, "", "unequal-prefix-target")
	scope := mustAdversarialStudyScope(t, topic)
	profileRef := adversarialProfileRef("unequal-prefix")
	root := mustInsertAdversarialPlan(
		t,
		ctx,
		repository,
		profileRef,
		scope,
		adversarialStudyPlan(model.OperatorStudyPrincipalID(), source.ID, "unequal-prefix-root"),
	)
	eventIDs := insertAdversarialSourceEvents(t, ctx, pool, root.Session.LearnerProfileID, root.Session.ID, root.Items[0])

	sourceProjection := adversarialProjection(root.Session.LearnerProfileID, source.ID, 1, "source-prefix-one")
	sourceProjection.LatestPresentedEventID = &eventIDs[0]
	sourceProjection.LatestPresentedEvidenceCardIdentity = &source.ID
	sourceProjection.LatestPresentedIngestionOrdinal = adversarialInt64Pointer(1)
	targetProjection := adversarialProjection(root.Session.LearnerProfileID, target.ID, 2, "target-prefix-two")
	err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		if _, replaceErr := store.ReplaceProjection(ctx, sourceProjection, 0); replaceErr != nil {
			return replaceErr
		}
		_, replaceErr := store.ReplaceProjection(ctx, targetProjection, 0)
		return replaceErr
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.UNEQUAL_PREFIX_FIXTURE_FAILED", err))
	}
	assertAdversarialOpaqueProjectionCount(t, ctx, pool, root.Session.LearnerProfileID, source.ID, target.ID, 2)

	merged, mergeErr := repository.MergeIdea(ctx, source.ID.String(), target.ID.String())
	if mergeErr != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.UNEQUAL_PREFIX_MERGE_FAILED", mergeErr))
	}
	if merged == nil || merged.ID != target.ID {
		t.Fatal("STUDY_ADVERSARIAL.UNEQUAL_PREFIX_MERGE_SURVIVOR_INVALID")
	}
	assertAdversarialMergeInvalidatedProjectionsAndPreservedHistory(
		t,
		ctx,
		pool,
		source.ID,
		target.ID,
		root.Items[0],
		eventIDs,
		root.Session.LearnerProfileID,
	)
}

func testRepeatedAndReverseMergeReturnSurvivor(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
) {
	t.Helper()
	source := mustCreateAdversarialIdea(t, ctx, repository, "", "idempotent-merge-source")
	target := mustCreateAdversarialIdea(t, ctx, repository, "", "idempotent-merge-target")

	first, firstErr := repository.MergeIdea(ctx, source.ID.String(), target.ID.String())
	if firstErr != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.FIRST_MERGE_FAILED", firstErr))
	}
	repeated, repeatedErr := repository.MergeIdea(ctx, source.ID.String(), target.ID.String())
	if repeatedErr != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.REPEATED_MERGE_FAILED", repeatedErr))
	}
	reversed, reversedErr := repository.MergeIdea(ctx, target.ID.String(), source.ID.String())
	if reversedErr != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.REVERSE_MERGE_RETRY_FAILED", reversedErr))
	}
	if first == nil || repeated == nil || reversed == nil || first.ID != target.ID || repeated.ID != target.ID || reversed.ID != target.ID {
		t.Fatal("STUDY_ADVERSARIAL.IDEMPOTENT_MERGE_SURVIVOR_CHANGED")
	}

	var redirectCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM study_card_identity_redirects
		WHERE historical_study_card_identity = $1
		  AND survivor_study_card_identity = $2`, source.ID, target.ID).Scan(&redirectCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.IDEMPOTENT_MERGE_REDIRECT_READ_FAILED", err))
	}
	if redirectCount != 1 {
		t.Fatal("STUDY_ADVERSARIAL.IDEMPOTENT_MERGE_REDIRECT_DUPLICATED")
	}
}

func mustCreateAdversarialTopic(t *testing.T, ctx context.Context, repository *Repository, label string) *model.Topic {
	t.Helper()
	suffix := uuid.NewString()[:8]
	slugLabel := strings.ReplaceAll(label, "_", "-")
	topic, err := repository.CreateTopic(ctx, model.CreateTopicInput{
		Slug: "adversarial-" + slugLabel + "-" + suffix,
		Name: "Adversarial " + label + " " + suffix,
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.TOPIC_CREATE_FAILED", err))
	}
	return topic
}

func mustCreateAdversarialIdea(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	topicRef string,
	label string,
) *model.Idea {
	t.Helper()
	suffix := uuid.NewString()[:8]
	summary := "Adversarial summary " + label
	idea, err := repository.CreateIdea(ctx, model.CreateIdeaInput{
		Summary: &summary,
		Names: []model.CreateNameInput{{
			Text: "Adversarial " + label + " " + suffix,
			Kind: model.NameKindCanonical,
		}},
		Definitions: []model.CreateDefinitionInput{{Body: "Adversarial definition " + label, IsPrimary: true}},
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.IDEA_CREATE_FAILED", err))
	}
	if topicRef != "" {
		if _, err := repository.AssignTopic(ctx, idea.ID.String(), topicRef); err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.TOPIC_ASSIGN_FAILED", err))
		}
	}
	return idea
}

func mustCreateAdversarialDefinitionOnlyIdea(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	topicRef string,
	label string,
) (*model.Idea, *model.Definition) {
	t.Helper()
	suffix := uuid.NewString()[:8]
	idea, err := repository.CreateIdea(ctx, model.CreateIdeaInput{
		Names: []model.CreateNameInput{{
			Text: "Adversarial " + label + " " + suffix,
			Kind: model.NameKindCanonical,
		}},
		Definitions: []model.CreateDefinitionInput{{Body: "Adversarial definition " + label, IsPrimary: true}},
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.DEFINITION_ONLY_IDEA_CREATE_FAILED", err))
	}
	if _, err := repository.AssignTopic(ctx, idea.ID.String(), topicRef); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.DEFINITION_ONLY_TOPIC_ASSIGN_FAILED", err))
	}
	definitions, err := repository.ListDefinitions(ctx, idea.ID.String())
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.DEFINITION_ONLY_READ_FAILED", err))
	}
	if len(definitions) != 1 {
		t.Fatal("STUDY_ADVERSARIAL.DEFINITION_ONLY_FIXTURE_INVALID")
	}
	return idea, &definitions[0]
}

func mustAdversarialStudyScope(t *testing.T, topic *model.Topic) model.StudyNormalizedScope {
	t.Helper()
	scope, err := model.NormalizeStudyScope(model.StudyScope{
		TopicIDs:  []uuid.UUID{topic.ID},
		TopicRefs: []string{topic.Slug},
		CardKind:  model.StudyCardKindIdea,
	})
	if err != nil {
		t.Fatal("STUDY_ADVERSARIAL.SCOPE_NORMALIZATION_FAILED")
	}
	return scope
}

func adversarialProfileRef(label string) string {
	return "adversarial-" + label + "-" + uuid.NewString()[:8]
}

func adversarialStudyPlan(
	principal model.StudyPrincipalID,
	sourceID uuid.UUID,
	label string,
) StudyPlanInsert {
	sessionID := uuid.New()
	liveSourceID := sourceID
	front := "Adversarial front " + label
	back := "Adversarial back " + label
	return StudyPlanInsert{
		SessionID:               sessionID,
		Principal:               principal,
		CreationRequestID:       uuid.New(),
		NormalizedRequestSHA256: model.SHA256Hex([]byte("adversarial-request-" + label)),
		Lifecycle:               model.StudySessionLifecycleActive,
		CorpusSHA256:            model.SHA256Hex([]byte("adversarial-corpus-" + label)),
		Scheduler: &model.StudySchedulerProvenance{
			Name:                           "variety_first",
			Version:                        "v1",
			ConfigurationSHA256:            model.SHA256Hex([]byte("adversarial-configuration")),
			InputProfileIngestionHighWater: 0,
			PlannedAt:                      time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
			Seed:                           "adversarial-seed",
		},
		LifecycleReasonCode: "adversarial_created",
		Items: []model.StudySessionItem{{
			ID:                       uuid.New(),
			StudySessionID:           sessionID,
			Position:                 0,
			CardKind:                 model.StudyCardKindIdea,
			CardIdentity:             sourceID,
			HistoricalSourceKind:     string(model.StudyCardKindIdea),
			HistoricalSourceIdentity: sourceID,
			LiveSourceIdeaID:         &liveSourceID,
			SourceContentRevision:    "adversarial-v1",
			SourceContentSHA256:      model.SHA256Hex([]byte(front + "\x00" + back)),
			FrontCanonicalName:       front,
			BackDefinition:           back,
			SelectionReason: model.StudySelectionReason{
				RatingRank:         model.StudyRatingRankUnseenUnrated,
				TieBreakerSHA256:   model.SHA256Hex([]byte("adversarial-tie-" + label)),
				BoundaryAdjustment: model.StudyBoundaryAdjustmentNone,
			},
		}},
	}
}

func adversarialWaitingPlan(label string) StudyPlanInsert {
	return StudyPlanInsert{
		SessionID:               uuid.New(),
		Principal:               model.OperatorStudyPrincipalID(),
		CreationRequestID:       uuid.New(),
		NormalizedRequestSHA256: model.SHA256Hex([]byte("adversarial-waiting-request-" + label)),
		Lifecycle:               model.StudySessionLifecycleWaitingForCorpus,
		CorpusSHA256:            model.SHA256Hex([]byte("adversarial-waiting-corpus-" + label)),
		LifecycleReasonCode:     "adversarial_waiting_for_corpus",
	}
}

func mustInsertAdversarialPlan(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	profileRef string,
	scope model.StudyNormalizedScope,
	plan StudyPlanInsert,
) *StudyPlanInsertResult {
	t.Helper()
	var result *StudyPlanInsertResult
	err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{ProfileRef: profileRef, Scope: scope}, func(store StudyStoreTransaction) error {
		if _, listErr := store.ListEligibleSources(ctx); listErr != nil {
			return listErr
		}
		var insertErr error
		result, insertErr = store.InsertPlanOnce(ctx, plan)
		return insertErr
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.PLAN_INSERT_FAILED", err))
	}
	if result == nil || !result.Created || result.Session.ID != plan.SessionID || len(result.Items) != len(plan.Items) {
		t.Fatal("STUDY_ADVERSARIAL.PLAN_INSERT_RESULT_INVALID")
	}
	return result
}

func newAdversarialWorkerRepository(
	t *testing.T,
	ctx context.Context,
	observerPool *pgxpool.Pool,
	label string,
) (*Repository, string, *pgxpool.Pool) {
	t.Helper()
	config := observerPool.Config()
	runtimeParameters := make(map[string]string, len(config.ConnConfig.RuntimeParams)+1)
	for key, value := range config.ConnConfig.RuntimeParams {
		runtimeParameters[key] = value
	}
	applicationName := "lexinet-adversarial-" + strings.ReplaceAll(label, "_", "-") + "-" + uuid.NewString()[:8]
	runtimeParameters["application_name"] = applicationName
	config.ConnConfig.RuntimeParams = runtimeParameters
	config.MinConns = 0
	config.MaxConns = 1
	workerPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.WORKER_POOL_CREATE_FAILED", err))
	}
	if err := workerPool.Ping(ctx); err != nil {
		workerPool.Close()
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.WORKER_POOL_PING_FAILED", err))
	}
	return NewRepository(workerPool), applicationName, workerPool
}

func waitForAdversarialPostgresLock(
	ctx context.Context,
	observerPool *pgxpool.Pool,
	applicationName string,
	workerDone <-chan struct{},
) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-workerDone:
			return errors.New("STUDY_ADVERSARIAL.MUTATION_COMPLETED_BEFORE_PLAN_COMMIT")
		default:
		}
		var waitingOnLock bool
		if err := observerPool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE datname = current_database()
				  AND application_name = $1
				  AND state = 'active'
				  AND wait_event_type = 'Lock'
			)`, applicationName).Scan(&waitingOnLock); err != nil {
			return err
		}
		if waitingOnLock {
			return nil
		}
		select {
		case <-workerDone:
			return errors.New("STUDY_ADVERSARIAL.MUTATION_COMPLETED_BEFORE_PLAN_COMMIT")
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func mustReadAdversarialPlanItem(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	profileRef string,
	sessionID uuid.UUID,
) model.StudySessionItem {
	t.Helper()
	items, err := repository.ListStudySessionItems(ctx, profileRef, sessionID)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.PLAN_ITEM_READ_FAILED", err))
	}
	if len(items) != 1 {
		t.Fatal("STUDY_ADVERSARIAL.PLAN_ITEM_COUNT_INVALID")
	}
	return items[0]
}

func assertAdversarialLaterEligibilityEmpty(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	scope model.StudyNormalizedScope,
	label string,
) {
	t.Helper()
	err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{
		ProfileRef: adversarialProfileRef(label + "-later-cycle"),
		Scope:      scope,
	}, func(store StudyStoreTransaction) error {
		sources, listErr := store.ListEligibleSources(ctx)
		if listErr != nil {
			return listErr
		}
		if len(sources) != 0 {
			return errors.New("STUDY_ADVERSARIAL.LATER_ELIGIBILITY_STILL_CONTAINS_MUTATED_SOURCE")
		}
		return nil
	})
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.LATER_ELIGIBILITY_READ_FAILED", err))
	}
}

func assertAdversarialLaterEligibilityStateStale(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	scope model.StudyNormalizedScope,
	label string,
) {
	t.Helper()
	err := repository.WithStudyScopeTransaction(ctx, StudyScopeTransactionInput{
		ProfileRef: adversarialProfileRef(label + "-later-cycle"),
		Scope:      scope,
	}, func(store StudyStoreTransaction) error {
		_, listErr := store.ListEligibleSources(ctx)
		return listErr
	})
	var studyError *model.StudyError
	if !errors.As(err, &studyError) || studyError.Code != model.StudyErrorCodeStateStale {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.LATER_ELIGIBILITY_DID_NOT_FAIL_STALE", err))
	}
}

func assertAdversarialItemMatchesEligibleSource(
	t *testing.T,
	item model.StudySessionItem,
	source StudyEligibleSource,
	expectedLiveSourceID uuid.UUID,
) {
	t.Helper()
	assertAdversarialFrozenItemMatchesEligibleSource(t, item, source)
	if item.CardIdentity != source.IdeaID || item.HistoricalSourceIdentity != source.IdeaID ||
		item.LiveSourceIdeaID == nil || *item.LiveSourceIdeaID != expectedLiveSourceID {
		t.Fatal("STUDY_ADVERSARIAL.ISSUED_ITEM_SOURCE_IDENTITY_MISMATCH")
	}
}

func assertAdversarialFrozenItemMatchesEligibleSource(
	t *testing.T,
	item model.StudySessionItem,
	source StudyEligibleSource,
) {
	t.Helper()
	if item.CardIdentity != source.IdeaID ||
		item.HistoricalSourceIdentity != source.IdeaID ||
		item.SourceContentRevision != source.SourceContentRevision ||
		item.SourceContentSHA256 != source.SourceContentSHA256 ||
		item.FrontCanonicalName != source.CanonicalName ||
		item.BackDefinition != source.Definition ||
		!sameAdversarialOptionalString(item.BackExample, source.Example) {
		t.Fatal("STUDY_ADVERSARIAL.ISSUED_ITEM_DID_NOT_MATCH_CAPTURED_SOURCE")
	}
}

func sameAdversarialOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func adversarialProjection(
	profileID uuid.UUID,
	cardIdentity uuid.UUID,
	highWater int64,
	label string,
) model.StudySchedulerCardProjection {
	liveSourceID := cardIdentity
	payload := []byte(fmt.Sprintf(
		`{"fixture":"%s","opaque_scheduler_state":{"canary":"must-not-be-rewritten-%s"}}`,
		label,
		label,
	))
	return model.StudySchedulerCardProjection{
		LearnerProfileID:                      profileID,
		ProjectedCardIdentity:                 cardIdentity,
		LiveSourceIdeaID:                      &liveSourceID,
		SchedulerName:                         "variety_first",
		SchedulerVersion:                      "v1",
		ProjectionSchemaVersion:               1,
		RebuiltThroughProfileIngestionOrdinal: highWater,
		CanonicalPayload:                      payload,
		CanonicalSHA256:                       model.SHA256Hex(payload),
	}
}

func insertAdversarialSourceEvents(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	profileID uuid.UUID,
	sessionID uuid.UUID,
	item model.StudySessionItem,
) []uuid.UUID {
	t.Helper()
	streamID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO study_client_streams (
			client_stream_id,
			learner_profile_id,
			adjudicated_through_client_sequence
		) VALUES ($1, $2, 2)`, streamID, profileID); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.EVENT_STREAM_INSERT_FAILED", err))
	}
	eventIDs := []uuid.UUID{uuid.New(), uuid.New()}
	eventKinds := []model.StudyEventKind{model.StudyEventKindPresented, model.StudyEventKindRevealed}
	for index := range eventIDs {
		sequence := int64(index + 1)
		payloadSHA256 := model.SHA256Hex([]byte(fmt.Sprintf("adversarial-event-%d", sequence)))
		if _, err := pool.Exec(ctx, `
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
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			eventIDs[index],
			payloadSHA256,
			profileID,
			sessionID,
			item.ID,
			item.CardIdentity,
			item.SourceContentSHA256,
			streamID,
			sequence,
		); err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.EVENT_BINDING_INSERT_FAILED", err))
		}
		if _, err := pool.Exec(ctx, `
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
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'unknown', $11)`,
			eventIDs[index],
			profileID,
			sessionID,
			item.ID,
			item.CardIdentity,
			eventKinds[index],
			item.SourceContentSHA256,
			streamID,
			sequence,
			payloadSHA256,
			sequence,
		); err != nil {
			t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.EVENT_INSERT_FAILED", err))
		}
	}
	if _, err := pool.Exec(ctx, `
		UPDATE study_learner_profiles
		SET profile_ingestion_high_water = 2
		WHERE learner_profile_id = $1`, profileID); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.EVENT_HIGH_WATER_UPDATE_FAILED", err))
	}
	return eventIDs
}

func assertAdversarialStudyError(
	t *testing.T,
	err error,
	expectedCode model.StudyErrorCode,
	expectedRetryable bool,
) {
	t.Helper()
	var studyError *model.StudyError
	if !errors.As(err, &studyError) || studyError == nil {
		t.Fatalf("STUDY_ADVERSARIAL.ERROR_NOT_TYPED: expected_code=%s actual_type=%T", expectedCode, err)
	}
	if studyError.Code != expectedCode || studyError.Retryable != expectedRetryable {
		t.Fatalf(
			"STUDY_ADVERSARIAL.ERROR_CONTRACT_MISMATCH: expected_code=%s actual_code=%s expected_retryable=%t actual_retryable=%t",
			expectedCode,
			studyError.Code,
			expectedRetryable,
			studyError.Retryable,
		)
	}
}

func assertAdversarialPlanAbsent(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	plan StudyPlanInsert,
) {
	t.Helper()
	var sessionCount, receiptCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM study_sessions WHERE study_session_id = $1),
			(SELECT count(*) FROM study_session_creation_receipts WHERE creation_request_id = $2)`,
		plan.SessionID,
		plan.CreationRequestID,
	).Scan(&sessionCount, &receiptCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.PLAN_ABSENCE_READ_FAILED", err))
	}
	if sessionCount != 0 || receiptCount != 0 {
		t.Fatal("STUDY_ADVERSARIAL.REJECTED_PLAN_PERSISTED_STATE")
	}
}

func assertAdversarialPlanTransactionRolledBack(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	profileRef string,
	plan StudyPlanInsert,
) {
	t.Helper()
	var profileCount, sessionCount, itemCount, receiptCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM study_learner_profiles WHERE learner_profile_ref = $1),
			(SELECT count(*) FROM study_sessions WHERE study_session_id = $2),
			(SELECT count(*) FROM study_session_items WHERE study_session_id = $2),
			(SELECT count(*) FROM study_session_creation_receipts WHERE creation_request_id = $3)`,
		profileRef,
		plan.SessionID,
		plan.CreationRequestID,
	).Scan(&profileCount, &sessionCount, &itemCount, &receiptCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.PLAN_ROLLBACK_READ_FAILED", err))
	}
	if profileCount != 0 || sessionCount != 0 || itemCount != 0 || receiptCount != 0 {
		t.Fatal("STUDY_ADVERSARIAL.REJECTED_PLAN_DID_NOT_FULLY_ROLL_BACK")
	}
}

func assertAdversarialOpaqueProjectionCount(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	profileID uuid.UUID,
	sourceID uuid.UUID,
	targetID uuid.UUID,
	expectedCount int,
) {
	t.Helper()
	var projectionCount, opaqueCanaryCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			count(*),
			count(*) FILTER (
				WHERE canonical_projection_payload::jsonb ? 'opaque_scheduler_state'
			)
		FROM study_scheduler_card_projections
		WHERE learner_profile_id = $1
		  AND projected_study_card_identity = ANY($2::uuid[])`,
		profileID,
		[]uuid.UUID{sourceID, targetID},
	).Scan(&projectionCount, &opaqueCanaryCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.OPAQUE_PROJECTION_COUNT_FAILED", err))
	}
	if projectionCount != expectedCount || opaqueCanaryCount != expectedCount {
		t.Fatalf(
			"STUDY_ADVERSARIAL.OPAQUE_PROJECTION_COUNT_MISMATCH: expected=%d projections=%d canaries=%d",
			expectedCount,
			projectionCount,
			opaqueCanaryCount,
		)
	}
}

func assertAdversarialMergeInvalidatedProjectionsAndPreservedHistory(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	sourceID uuid.UUID,
	targetID uuid.UUID,
	item model.StudySessionItem,
	eventIDs []uuid.UUID,
	profileID uuid.UUID,
) {
	t.Helper()
	var sourceStatus, targetStatus string
	var sourceMergedInto *uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT source.status, source.merged_into_id, target.status
		FROM ideas source
		JOIN ideas target ON target.id = $2
		WHERE source.id = $1`, sourceID, targetID).Scan(&sourceStatus, &sourceMergedInto, &targetStatus); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.MERGE_ROLLBACK_IDEA_READ_FAILED", err))
	}
	var redirectCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM study_card_identity_redirects
		WHERE historical_study_card_identity = $1`, sourceID).Scan(&redirectCount); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.MERGE_ROLLBACK_REDIRECT_READ_FAILED", err))
	}
	var itemCardIdentity, itemHistoricalSourceIdentity, itemLiveSource uuid.UUID
	var itemContentSHA256, itemFront, itemBack string
	if err := pool.QueryRow(ctx, `
		SELECT
			study_card_identity,
			historical_source_identity,
			live_source_idea_id,
			source_content_sha256,
			card_front_canonical_name,
			card_back_definition
		FROM study_session_items
		WHERE study_session_item_id = $1`, item.ID).Scan(
		&itemCardIdentity,
		&itemHistoricalSourceIdentity,
		&itemLiveSource,
		&itemContentSHA256,
		&itemFront,
		&itemBack,
	); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.MERGE_HISTORY_READ_FAILED", err))
	}
	var eventCount, sourceIdentityEventCount, expectedEventCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			count(*),
			count(*) FILTER (WHERE study_card_identity = $3),
			count(*) FILTER (WHERE study_event_id = ANY($4::uuid[]))
		FROM study_events
		WHERE learner_profile_id = $1
		  AND study_session_item_id = $2`, profileID, item.ID, sourceID, eventIDs).Scan(
		&eventCount,
		&sourceIdentityEventCount,
		&expectedEventCount,
	); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_ADVERSARIAL.MERGE_EVENT_HISTORY_READ_FAILED", err))
	}
	if sourceStatus != string(model.IdeaStatusMerged) || sourceMergedInto == nil || *sourceMergedInto != targetID ||
		targetStatus != string(model.IdeaStatusActive) || redirectCount != 1 ||
		itemCardIdentity != item.CardIdentity || itemHistoricalSourceIdentity != item.HistoricalSourceIdentity ||
		itemLiveSource != targetID || itemContentSHA256 != item.SourceContentSHA256 ||
		itemFront != item.FrontCanonicalName || itemBack != item.BackDefinition ||
		eventCount != len(eventIDs) || sourceIdentityEventCount != len(eventIDs) || expectedEventCount != len(eventIDs) {
		t.Fatal("STUDY_ADVERSARIAL.MERGE_DAMAGED_REDIRECT_EVENTS_OR_HISTORY")
	}
	assertAdversarialOpaqueProjectionCount(t, ctx, pool, profileID, sourceID, targetID, 0)
}

func cleanupAdversarialWorker(
	t *testing.T,
	cancel context.CancelFunc,
	workerDone <-chan struct{},
) {
	t.Helper()
	cancel()
	timer := time.NewTimer(adversarialWorkerCleanupTimeout)
	defer timer.Stop()
	select {
	case <-workerDone:
	case <-timer.C:
		t.Error("STUDY_ADVERSARIAL.WORKER_CLEANUP_TIMEOUT")
	}
}

func adversarialInt64Pointer(value int64) *int64 {
	return &value
}
