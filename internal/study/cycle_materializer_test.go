package study

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"lexinet/internal/model"
	studydigest "lexinet/internal/study/digest"
)

func TestCycleMaterializerCreatesRootAndEmptyWaitingCycles(t *testing.T) {
	t.Parallel()

	materializer := newCycleMaterializerForTest(t)
	t.Run("active root", func(t *testing.T) {
		store := newCycleMaterializationStoreForTest(t, 2)
		input := cycleMaterializationInputForTest(len(store.captured.Sources))
		result, err := materializer.Materialize(context.Background(), store, input)
		if err != nil {
			t.Fatalf("materialize active root: %v", err)
		}
		if store.insertCalls != 1 || store.lastInsert.PredecessorSessionID != nil {
			t.Fatalf("root insert = calls:%d predecessor:%v", store.insertCalls, store.lastInsert.PredecessorSessionID)
		}
		if result.Persistence.Session.Lifecycle != model.StudySessionLifecycleActive ||
			result.SchedulingSnapshot == nil || result.AuthoritativePlan == nil ||
			len(result.Persistence.Items) != 2 || len(result.AuthoritativePlan.Cards) != 2 {
			t.Fatalf("active materialization result = %#v", result)
		}
	})

	t.Run("empty waiting sentinel", func(t *testing.T) {
		store := newCycleMaterializationStoreForTest(t, 0)
		input := cycleMaterializationInputForTest(0)
		result, err := materializer.Materialize(context.Background(), store, input)
		if err != nil {
			t.Fatalf("materialize empty sentinel: %v", err)
		}
		if store.lastInsert.Lifecycle != model.StudySessionLifecycleWaitingForCorpus ||
			store.lastInsert.Scheduler != nil || len(store.lastInsert.Items) != 0 ||
			result.SchedulingSnapshot != nil || result.AuthoritativePlan != nil ||
			store.prefixCalls != 0 || store.projectionCalls != 0 {
			t.Fatalf("waiting materialization result = %#v insert=%#v", result, store.lastInsert)
		}
	})
}

func TestCycleMaterializerAutomaticallyReplacesPopulatedWaitingSentinel(t *testing.T) {
	t.Parallel()

	materializer := newCycleMaterializerForTest(t)
	store := newCycleMaterializationStoreForTest(t, 1)
	waitingID := uuid.New()
	emptyCorpusSHA256, err := materializationCorpusSHA256(store.captured.Scope, nil)
	if err != nil {
		t.Fatalf("identify empty waiting corpus: %v", err)
	}
	store.captured.NonterminalSession = &model.StudySession{
		ID:               waitingID,
		LearnerProfileID: store.captured.LearnerProfile.ID,
		Scope:            cloneCycleNormalizedScope(store.captured.Scope),
		Lifecycle:        model.StudySessionLifecycleWaitingForCorpus,
		CorpusSHA256:     emptyCorpusSHA256,
		StateVersion:     1,
	}
	store.sessionItems[waitingID] = []model.StudySessionItem{}

	input := cycleMaterializationInputForTest(1)
	result, err := materializer.Materialize(context.Background(), store, input)
	if err != nil {
		t.Fatalf("replace waiting sentinel: %v", err)
	}
	if store.lastInsert.PredecessorSessionID == nil || *store.lastInsert.PredecessorSessionID != waitingID ||
		store.lastInsert.PredecessorFlushReceiptID != uuid.Nil || result.Persistence.Session.Lifecycle != model.StudySessionLifecycleActive {
		t.Fatalf("waiting replacement did not use the locked predecessor: %#v", store.lastInsert)
	}
}

func TestCycleMaterializerReturnsPersistedConvergenceAuthority(t *testing.T) {
	t.Parallel()

	materializer := newCycleMaterializerForTest(t)
	store := newCycleMaterializationStoreForTest(t, 2)
	store.insertOverride = func(input CyclePersistenceInput) *CyclePersistenceResult {
		persisted := store.defaultInsertResult(input)
		persisted.Created = false
		persisted.Receipt.Disposition = model.StudyCreationDispositionConvergedToExisting
		return persisted
	}

	result, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(2))
	if err != nil {
		t.Fatalf("materialize converged authority: %v", err)
	}
	if result.Persistence.Created || result.SchedulingSnapshot != nil || result.AuthoritativePlan == nil ||
		result.AuthoritativePlan.Cards[0].CardIdentity != result.Persistence.Items[0].CardIdentity ||
		result.AuthoritativePlan.Cards[1].CardIdentity != result.Persistence.Items[1].CardIdentity {
		t.Fatalf("materializer returned hypothetical rather than persisted authority: %#v", result)
	}
}

func TestCycleMaterializerResolvesReceiptBeforeMutableAuthority(t *testing.T) {
	t.Parallel()

	canonical := newCycleMaterializerForTest(t)
	planCalls := 0
	materializer, err := newCycleMaterializer(
		cycleMaterializationSchedulerFunc(func(snapshot StudySchedulingSnapshot) (StudyPlan, error) {
			planCalls++
			return canonical.scheduler.Plan(snapshot)
		}),
		canonical.configuration,
	)
	if err != nil {
		t.Fatalf("construct counting materializer: %v", err)
	}
	store := newCycleMaterializationStoreForTest(t, 2)
	input := cycleMaterializationInputForTest(2)
	created, err := materializer.Materialize(context.Background(), store, input)
	if err != nil {
		t.Fatalf("create initial cycle: %v", err)
	}
	persisted := cloneCyclePersistenceResult(created.Persistence)
	persisted.Created = false
	store.resolveOverride = func(candidate ExistingCycleResolutionInput) (*ExistingCycleResolution, error) {
		if candidate.Principal != input.Principal || candidate.CreationRequestID != input.CreationRequestID ||
			candidate.NormalizedRequestSHA256 != input.NormalizedRequestSHA256 || candidate.PredecessorSessionID != nil {
			return nil, &model.StudyError{Code: model.StudyErrorCodeIdempotencyConflict, Message: "creation identity conflict"}
		}
		return &ExistingCycleResolution{
			LearnerProfile: store.captured.LearnerProfile,
			Scope:          cloneCycleNormalizedScope(store.captured.Scope),
			Persistence:    cloneCyclePersistenceResult(persisted),
		}, nil
	}
	store.resolveCalls = 0
	store.captureCalls = 0
	store.prefixCalls = 0
	store.projectionCalls = 0
	store.insertCalls = 0
	store.captured.LearnerProfile.ProfileIngestionHighWater = 41
	persisted.Session.ProfileIngestionHighWater = 41
	store.captured.Sources = append(store.captured.Sources, cloneMaterializationSource(newCycleMaterializationStoreForTest(t, 1).captured.Sources[0]))

	replayed, err := materializer.Materialize(context.Background(), store, input)
	if err != nil {
		t.Fatalf("replay after mutable authority changed: %v", err)
	}
	if replayed.Persistence.Session.ID != created.Persistence.Session.ID || replayed.Persistence.Created ||
		replayed.SchedulingSnapshot != nil || replayed.AuthoritativePlan == nil ||
		!equalCyclePlans(*replayed.AuthoritativePlan, *created.AuthoritativePlan) {
		t.Fatalf("replay did not return exact durable authority: %#v", replayed)
	}
	if store.resolveCalls != 1 || store.captureCalls != 0 || store.prefixCalls != 0 ||
		store.projectionCalls != 0 || store.insertCalls != 0 || planCalls != 1 {
		t.Fatalf("replay consulted mutable authority: resolve=%d capture=%d prefix=%d projection=%d insert=%d plan=%d",
			store.resolveCalls, store.captureCalls, store.prefixCalls, store.projectionCalls, store.insertCalls, planCalls)
	}

	conflicting := input
	conflicting.NormalizedRequestSHA256 = model.SHA256Hex([]byte("changed-request"))
	_, err = materializer.Materialize(context.Background(), store, conflicting)
	var studyError *model.StudyError
	if !errors.As(err, &studyError) || studyError.Code != model.StudyErrorCodeIdempotencyConflict || store.captureCalls != 0 || planCalls != 1 {
		t.Fatalf("changed replay identity = err:%#v capture:%d plan:%d", err, store.captureCalls, planCalls)
	}
}

func TestCycleMaterializerDoesNotReadMutablePredecessorTailForConvergence(t *testing.T) {
	t.Parallel()

	materializer := newCycleMaterializerForTest(t)
	store := newCycleMaterializationStoreForTest(t, 2)
	predecessor, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(2))
	if err != nil {
		t.Fatalf("materialize predecessor: %v", err)
	}
	predecessorID := predecessor.Persistence.Session.ID
	predecessorSession := cloneCycleSession(predecessor.Persistence.Session)
	store.captured.NonterminalSession = &predecessorSession
	store.sessionItems[predecessorID] = cloneCyclePersistenceResult(predecessor.Persistence).Items
	store.insertOverride = func(input CyclePersistenceInput) *CyclePersistenceResult {
		persisted := store.defaultInsertResult(input)
		persisted.Created = false
		persisted.Receipt.Disposition = model.StudyCreationDispositionSuccessor
		return persisted
	}

	input := cycleMaterializationInputForTest(2)
	input.PredecessorSessionID = &predecessorID
	input.PredecessorFlushReceiptID = uuid.New()
	result, err := materializer.Materialize(context.Background(), store, input)
	if err != nil {
		t.Fatalf("converge after predecessor redirect changed: %v", err)
	}
	if result.SchedulingSnapshot != nil || result.AuthoritativePlan == nil {
		t.Fatalf("convergence re-adjudicated a historical predecessor: result=%#v", result)
	}
}

func TestCycleMaterializerReplaysWaitingReceiptBeforeNewCorpusCapture(t *testing.T) {
	t.Parallel()

	canonical := newCycleMaterializerForTest(t)
	store := newCycleMaterializationStoreForTest(t, 0)
	input := cycleMaterializationInputForTest(0)
	waiting, err := canonical.Materialize(context.Background(), store, input)
	if err != nil {
		t.Fatalf("create waiting cycle: %v", err)
	}
	persisted := cloneCyclePersistenceResult(waiting.Persistence)
	persisted.Created = false
	populated := newCycleMaterializationStoreForTest(t, 1).captured.Sources[0]
	store.captured.Sources = []MaterializationSource{cloneMaterializationSource(populated)}
	store.captured.CorpusSHA256, err = materializationCorpusSHA256(store.captured.Scope, store.captured.Sources)
	if err != nil {
		t.Fatalf("identify changed corpus: %v", err)
	}
	store.resolveOverride = func(ExistingCycleResolutionInput) (*ExistingCycleResolution, error) {
		return &ExistingCycleResolution{
			LearnerProfile: store.captured.LearnerProfile,
			Scope:          cloneCycleNormalizedScope(store.captured.Scope),
			Persistence:    cloneCyclePersistenceResult(persisted),
		}, nil
	}
	store.resolveCalls = 0
	store.captureCalls = 0
	store.prefixCalls = 0
	store.projectionCalls = 0
	store.insertCalls = 0
	panicking, err := newCycleMaterializer(
		cycleMaterializationSchedulerFunc(func(StudySchedulingSnapshot) (StudyPlan, error) {
			panic("scheduler must not run for receipt replay")
		}),
		canonical.configuration,
	)
	if err != nil {
		t.Fatalf("construct replay materializer: %v", err)
	}

	replayed, err := panicking.Materialize(context.Background(), store, input)
	if err != nil {
		t.Fatalf("replay waiting receipt after corpus changed: %v", err)
	}
	if replayed.Persistence.Session.ID != waiting.Persistence.Session.ID || replayed.AuthoritativePlan != nil ||
		store.resolveCalls != 1 || store.captureCalls != 0 || store.prefixCalls != 0 ||
		store.projectionCalls != 0 || store.insertCalls != 0 {
		t.Fatalf("waiting receipt replay consulted mutable authority: result=%#v resolve=%d capture=%d prefix=%d projection=%d insert=%d",
			replayed, store.resolveCalls, store.captureCalls, store.prefixCalls, store.projectionCalls, store.insertCalls)
	}
}

func TestCycleMaterializerPreflightRejectsPredecessorConfusionAndInvalidBarrierShape(t *testing.T) {
	t.Parallel()

	materializer := newCycleMaterializerForTest(t)
	baseStore := newCycleMaterializationStoreForTest(t, 1)
	baseInput := cycleMaterializationInputForTest(1)
	created, err := materializer.Materialize(context.Background(), baseStore, baseInput)
	if err != nil {
		t.Fatalf("create predecessor-confusion fixture: %v", err)
	}
	rootPersistence := cloneCyclePersistenceResult(created.Persistence)
	rootPersistence.Created = false

	t.Run("root receipt cannot satisfy successor", func(t *testing.T) {
		store := newCycleMaterializationStoreForTest(t, 1)
		store.resolveOverride = func(ExistingCycleResolutionInput) (*ExistingCycleResolution, error) {
			return &ExistingCycleResolution{LearnerProfile: store.captured.LearnerProfile, Scope: cloneCycleNormalizedScope(store.captured.Scope), Persistence: cloneCyclePersistenceResult(rootPersistence)}, nil
		}
		// Keep the locked authority consistent with the returned durable rows.
		store.captured.LearnerProfile.ID = rootPersistence.Session.LearnerProfileID
		store.captured.Scope = cloneCycleNormalizedScope(rootPersistence.Session.Scope)
		predecessor := uuid.New()
		input := baseInput
		input.PredecessorSessionID = &predecessor
		input.PredecessorFlushReceiptID = uuid.New()
		input.PredecessorTerminalReasonCode = "cycle_completed"
		_, err := materializer.Materialize(context.Background(), store, input)
		assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, "predecessor_mismatch")
		if store.captureCalls != 0 {
			t.Fatal("predecessor-confused receipt reached mutable capture")
		}
	})

	for _, testCase := range []struct {
		name   string
		mutate func(*CycleMaterializationInput)
	}{
		{
			name: "flush without predecessor",
			mutate: func(input *CycleMaterializationInput) {
				input.PredecessorFlushReceiptID = uuid.New()
			},
		},
		{
			name: "predecessor without reason",
			mutate: func(input *CycleMaterializationInput) {
				predecessor := uuid.New()
				input.PredecessorSessionID = &predecessor
				input.PredecessorFlushReceiptID = uuid.New()
				input.PredecessorTerminalReasonCode = ""
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := newCycleMaterializationStoreForTest(t, 1)
			input := baseInput
			testCase.mutate(&input)
			_, err := materializer.Materialize(context.Background(), store, input)
			assertCycleMaterializationError(t, err, model.StudyErrorCodeSchemaInvalid, "invalid_input")
			if store.resolveCalls != 0 {
				t.Fatal("invalid barrier shape reached receipt resolution")
			}
		})
	}
}

func TestCycleMaterializerRejectsPanickingAndInvalidSchedulerOutput(t *testing.T) {
	t.Parallel()

	validMaterializer := newCycleMaterializerForTest(t)
	configuration := validMaterializer.configuration
	testCases := []struct {
		name      string
		scheduler StudyScheduler
		reason    string
	}{
		{
			name: "panic",
			scheduler: cycleMaterializationSchedulerFunc(func(StudySchedulingSnapshot) (StudyPlan, error) {
				panic("private scheduler panic")
			}),
			reason: "scheduler_panic",
		},
		{
			name: "mutated identity",
			scheduler: cycleMaterializationSchedulerFunc(func(snapshot StudySchedulingSnapshot) (StudyPlan, error) {
				plan, err := validMaterializer.scheduler.Plan(snapshot)
				if err == nil {
					plan.Cards[0].CardIdentity = uuid.New()
				}
				return plan, err
			}),
			reason: "identity_mismatch",
		},
		{
			name: "provenance mismatch",
			scheduler: cycleMaterializationSchedulerFunc(func(snapshot StudySchedulingSnapshot) (StudyPlan, error) {
				plan, err := validMaterializer.scheduler.Plan(snapshot)
				if err == nil {
					plan.Scheduler.ConfigurationSHA256 = model.SHA256Hex([]byte("wrong-configuration"))
				}
				return plan, err
			}),
			reason: "invalid_scheduler_output",
		},
		{
			name: "noncanonical reverse order",
			scheduler: cycleMaterializationSchedulerFunc(func(snapshot StudySchedulingSnapshot) (StudyPlan, error) {
				plan, err := validMaterializer.scheduler.Plan(snapshot)
				if err == nil {
					for left, right := 0, len(plan.Cards)-1; left < right; left, right = left+1, right-1 {
						plan.Cards[left], plan.Cards[right] = plan.Cards[right], plan.Cards[left]
					}
				}
				return plan, err
			}),
			reason: "noncanonical_scheduler_output",
		},
		{
			name: "forged boundary marker",
			scheduler: cycleMaterializationSchedulerFunc(func(snapshot StudySchedulingSnapshot) (StudyPlan, error) {
				plan, err := validMaterializer.scheduler.Plan(snapshot)
				if err == nil {
					plan.Cards[0].SelectionReason.BoundaryAdjustment = model.StudyBoundaryAdjustmentPriorityBound
				}
				return plan, err
			}),
			reason: "noncanonical_scheduler_output",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			materializer, err := newCycleMaterializer(testCase.scheduler, configuration)
			if err != nil {
				t.Fatalf("construct materializer: %v", err)
			}
			store := newCycleMaterializationStoreForTest(t, 2)
			originalIdentity := store.captured.Sources[0].CardIdentity
			_, err = materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(2))
			var studyError *model.StudyError
			if !errors.As(err, &studyError) || studyError.Code != model.StudyErrorCodeInternal ||
				studyError.Context["reason"] != testCase.reason || store.insertCalls != 0 {
				t.Fatalf("invalid scheduler result = err:%#v calls:%d", err, store.insertCalls)
			}
			if store.captured.Sources[0].CardIdentity != originalIdentity {
				t.Fatal("scheduler mutated captured authority through an alias")
			}
		})
	}
}

func TestCycleMaterializerDetachesNestedSchedulerInputAndAcceptedOutput(t *testing.T) {
	t.Parallel()

	t.Run("nested input pointers", func(t *testing.T) {
		canonical := newCycleMaterializerForTest(t)
		firstID := uuid.New()
		secondID := uuid.New()
		firstRating := model.StudyRatingGood
		secondRating := model.StudyRatingHard
		firstOrdinal := int64(1)
		secondOrdinal := int64(3)
		predecessorTail := firstID
		snapshot := StudySchedulingSnapshot{
			LearnerProfileID: uuid.New(), CardKind: model.StudyCardKindIdea,
			CorpusSHA256: model.SHA256Hex([]byte("detached-input-corpus")), InputProfileIngestionHighWater: 3,
			PlannedAt: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), Seed: "detached-input",
			PredecessorTailCardIdentity: &predecessorTail,
			Cards: []StudySchedulingCard{
				{CardIdentity: firstID, Projection: StudyProjectionTail{LatestEffectiveRating: &firstRating, LatestPresentedIngestionOrdinal: &firstOrdinal}},
				{CardIdentity: secondID, Projection: StudyProjectionTail{LatestEffectiveRating: &secondRating, LatestPresentedIngestionOrdinal: &secondOrdinal}},
			},
		}
		mutating := cycleMaterializationSchedulerFunc(func(private StudySchedulingSnapshot) (StudyPlan, error) {
			*private.PredecessorTailCardIdentity = secondID
			*private.Cards[0].Projection.LatestEffectiveRating = model.StudyRatingEasy
			*private.Cards[0].Projection.LatestPresentedIngestionOrdinal = 2
			return canonical.scheduler.Plan(private)
		})
		_, err := invokeAndValidateMaterializationScheduler(mutating, canonical.configuration, snapshot)
		assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, "invalid_scheduler_output")
		if predecessorTail != firstID || firstRating != model.StudyRatingGood || firstOrdinal != 1 {
			t.Fatal("scheduler mutated authoritative nested snapshot pointers")
		}
	})

	t.Run("accepted output pointers", func(t *testing.T) {
		canonical := newCycleMaterializerForTest(t)
		var retained StudyPlan
		materializer, err := newCycleMaterializer(
			cycleMaterializationSchedulerFunc(func(snapshot StudySchedulingSnapshot) (StudyPlan, error) {
				plan, planErr := canonical.scheduler.Plan(snapshot)
				retained = plan
				return plan, planErr
			}),
			canonical.configuration,
		)
		if err != nil {
			t.Fatalf("construct retaining scheduler: %v", err)
		}
		store := newCycleMaterializationStoreForTest(t, 1)
		store.captured.LearnerProfile.ProfileIngestionHighWater = 1
		ordinal := int64(1)
		receivedAt := time.Date(2026, 8, 17, 11, 59, 0, 0, time.UTC)
		source := store.captured.Sources[0]
		store.acceptedPrefix = []model.StudyEvent{{
			ID: uuid.New(), ClientSequence: 1, ProfileRef: store.captured.LearnerProfile.Ref,
			LearnerProfileID: store.captured.LearnerProfile.ID, StudySessionID: uuid.New(), StudySessionItemID: uuid.New(),
			CardIdentity: source.CardIdentity, ContentSHA256: source.SourceContentSHA256,
			Kind: model.StudyEventKindPresented, ClientClockQuality: model.StudyClientClockQualityUnknown,
			ClientStreamID: uuid.New(), NormalizedPayloadSHA256: model.SHA256Hex([]byte("detached-output-event")),
			ProfileIngestionOrdinal: &ordinal, ReceivedAt: &receivedAt,
		}}
		result, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(1))
		if err != nil {
			t.Fatalf("materialize retained scheduler output: %v", err)
		}
		if len(retained.Cards) != 1 || retained.Cards[0].SelectionReason.LatestPresentedIngestionOrdinal == nil {
			t.Fatal("retained scheduler output lacks nested ordinal fixture")
		}
		originalIdentity := result.Persistence.Items[0].CardIdentity
		originalOrdinal := *result.Persistence.Items[0].SelectionReason.LatestPresentedIngestionOrdinal
		retained.Cards[0].CardIdentity = uuid.New()
		*retained.Cards[0].SelectionReason.LatestPresentedIngestionOrdinal = 99
		retained.Cards[0].SelectionReason.TieBreakerSHA256 = model.SHA256Hex([]byte("mutated-retained-output"))
		if store.lastInsert.Items[0].CardIdentity != originalIdentity ||
			*store.lastInsert.Items[0].SelectionReason.LatestPresentedIngestionOrdinal != originalOrdinal ||
			result.Persistence.Items[0].CardIdentity != originalIdentity ||
			*result.Persistence.Items[0].SelectionReason.LatestPresentedIngestionOrdinal != originalOrdinal ||
			result.AuthoritativePlan.Cards[0].CardIdentity != originalIdentity ||
			*result.AuthoritativePlan.Cards[0].SelectionReason.LatestPresentedIngestionOrdinal != originalOrdinal {
			t.Fatal("retained scheduler output remained aliased to persisted authority")
		}
	})
}

func TestCycleMaterializerRejectsUntrustedCorpusSchedulerAndPersistenceAuthority(t *testing.T) {
	t.Parallel()

	t.Run("captured corpus digest", func(t *testing.T) {
		materializer := newCycleMaterializerForTest(t)
		store := newCycleMaterializationStoreForTest(t, 1)
		store.captured.CorpusSHA256 = model.SHA256Hex([]byte("forged-corpus"))
		_, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(1))
		assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, "digest_mismatch")
		if store.insertCalls != 0 {
			t.Fatal("forged corpus reached persistence")
		}
	})

	t.Run("captured frozen bytes retain their content digest", func(t *testing.T) {
		materializer := newCycleMaterializerForTest(t)
		store := newCycleMaterializationStoreForTest(t, 1)
		store.captured.Sources[0].BackDefinition = "substituted captured back"
		_, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(1))
		assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, "content_digest_mismatch")
		if store.insertCalls != 0 {
			t.Fatal("forged captured content reached persistence")
		}
	})

	t.Run("scheduler error redaction", func(t *testing.T) {
		const privateText = "private-scheduler-error-text"
		configuration := newCycleMaterializerForTest(t).configuration
		materializer, err := newCycleMaterializer(
			cycleMaterializationSchedulerFunc(func(StudySchedulingSnapshot) (StudyPlan, error) {
				return StudyPlan{}, errors.New(privateText)
			}),
			configuration,
		)
		if err != nil {
			t.Fatalf("construct materializer: %v", err)
		}
		store := newCycleMaterializationStoreForTest(t, 1)
		_, err = materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(1))
		assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, "scheduler_error")
		var studyError *model.StudyError
		if !errors.As(err, &studyError) || studyError.Cause != nil || errors.Unwrap(err) != nil {
			t.Fatalf("scheduler failure retained its extension error graph: %#v", err)
		}
		encoded, marshalErr := json.Marshal(err)
		if marshalErr != nil {
			t.Fatalf("marshal scheduler failure: %v", marshalErr)
		}
		if strings.Contains(err.Error(), privateText) || strings.Contains(string(encoded), privateText) {
			t.Fatal("scheduler error text escaped the structured materializer boundary")
		}
	})

	t.Run("receipt session mismatch", func(t *testing.T) {
		materializer := newCycleMaterializerForTest(t)
		store := newCycleMaterializationStoreForTest(t, 1)
		store.insertOverride = func(input CyclePersistenceInput) *CyclePersistenceResult {
			result := store.defaultInsertResult(input)
			result.Receipt.CanonicalStudySessionID = uuid.New()
			return result
		}
		_, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(1))
		assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, "authority_mismatch")
	})

	t.Run("persisted live high-water mismatch", func(t *testing.T) {
		materializer := newCycleMaterializerForTest(t)
		store := newCycleMaterializationStoreForTest(t, 1)
		store.insertOverride = func(input CyclePersistenceInput) *CyclePersistenceResult {
			result := store.defaultInsertResult(input)
			result.Session.ProfileIngestionHighWater++
			return result
		}
		_, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(1))
		assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, "authority_mismatch")
	})

	t.Run("created root cannot claim successor disposition", func(t *testing.T) {
		materializer := newCycleMaterializerForTest(t)
		store := newCycleMaterializationStoreForTest(t, 1)
		store.insertOverride = func(input CyclePersistenceInput) *CyclePersistenceResult {
			result := store.defaultInsertResult(input)
			result.Receipt.Disposition = model.StudyCreationDispositionSuccessor
			return result
		}
		_, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(1))
		assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, "invalid_authority")
	})

	for _, disposition := range []model.StudyCreationDisposition{
		model.StudyCreationDispositionCreated,
		model.StudyCreationDispositionConvergedToExisting,
	} {
		t.Run("created waiting successor rejects "+string(disposition), func(t *testing.T) {
			materializer := newCycleMaterializerForTest(t)
			store := newCycleMaterializationStoreForTest(t, 1)
			waitingID := uuid.New()
			emptyCorpusSHA256, digestErr := materializationCorpusSHA256(store.captured.Scope, nil)
			if digestErr != nil {
				t.Fatalf("identify waiting corpus: %v", digestErr)
			}
			store.captured.NonterminalSession = &model.StudySession{
				ID: waitingID, LearnerProfileID: store.captured.LearnerProfile.ID,
				Scope: cloneCycleNormalizedScope(store.captured.Scope), Lifecycle: model.StudySessionLifecycleWaitingForCorpus,
				CorpusSHA256: emptyCorpusSHA256, ProfileIngestionHighWater: store.captured.LearnerProfile.ProfileIngestionHighWater, StateVersion: 1,
			}
			store.sessionItems[waitingID] = []model.StudySessionItem{}
			store.insertOverride = func(input CyclePersistenceInput) *CyclePersistenceResult {
				result := store.defaultInsertResult(input)
				result.Receipt.Disposition = disposition
				return result
			}
			_, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(1))
			assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, "invalid_authority")
		})
	}

	t.Run("persisted frozen bytes retain their content digest", func(t *testing.T) {
		materializer := newCycleMaterializerForTest(t)
		store := newCycleMaterializationStoreForTest(t, 1)
		store.insertOverride = func(input CyclePersistenceInput) *CyclePersistenceResult {
			result := store.defaultInsertResult(input)
			result.Created = false
			result.Items[0].FrontCanonicalName = "substituted persisted front"
			return result
		}
		_, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(1))
		assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, "content_digest_mismatch")
	})

	for _, testCase := range []struct {
		name   string
		reason string
		mutate func(*CyclePersistenceResult)
	}{
		{
			name:   "persisted tie digest substitution",
			reason: "tie_digest_mismatch",
			mutate: func(result *CyclePersistenceResult) {
				result.Items[0].SelectionReason.TieBreakerSHA256 = model.SHA256Hex([]byte("substituted-tie"))
			},
		},
		{
			name:   "persisted swapped order",
			reason: "invalid_persisted_order",
			mutate: func(result *CyclePersistenceResult) {
				result.Items[0], result.Items[1] = result.Items[1], result.Items[0]
				result.Items[0].Position = 0
				result.Items[1].Position = 1
			},
		},
		{
			name:   "persisted forged root priority marker",
			reason: "invalid_boundary_shape",
			mutate: func(result *CyclePersistenceResult) {
				result.Items[0].SelectionReason.BoundaryAdjustment = model.StudyBoundaryAdjustmentPriorityBound
			},
		},
		{
			name:   "persisted forged root rotation marker",
			reason: "invalid_boundary_shape",
			mutate: func(result *CyclePersistenceResult) {
				result.Items[0].SelectionReason.BoundaryAdjustment = model.StudyBoundaryAdjustmentRotated
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			materializer := newCycleMaterializerForTest(t)
			store := newCycleMaterializationStoreForTest(t, 2)
			store.insertOverride = func(input CyclePersistenceInput) *CyclePersistenceResult {
				result := store.defaultInsertResult(input)
				result.Created = false
				result.Receipt.Disposition = model.StudyCreationDispositionConvergedToExisting
				testCase.mutate(result)
				return result
			}
			_, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(2))
			assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, testCase.reason)
		})
	}

	t.Run("persisted duplicate presented ordinal", func(t *testing.T) {
		materializer := newCycleMaterializerForTest(t)
		store := newCycleMaterializationStoreForTest(t, 2)
		created, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(2))
		if err != nil {
			t.Fatalf("materialize duplicate-ordinal fixture: %v", err)
		}
		persistence := cloneCyclePersistenceResult(created.Persistence)
		ordinal := int64(1)
		persistence.Session.ProfileIngestionHighWater = ordinal
		persistence.Session.Scheduler.InputProfileIngestionHighWater = ordinal
		for index := range persistence.Items {
			persistence.Items[index].SelectionReason.LatestPresentedIngestionOrdinal = &ordinal
		}
		_, _, err = reconstructPersistedCycleAuthority(persistence, materializer.configuration)
		assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, "duplicate_presented_ordinal")
	})

	t.Run("active predecessor without tail", func(t *testing.T) {
		materializer := newCycleMaterializerForTest(t)
		store := newCycleMaterializationStoreForTest(t, 1)
		predecessorID := uuid.New()
		store.captured.NonterminalSession = &model.StudySession{
			ID: predecessorID, LearnerProfileID: store.captured.LearnerProfile.ID,
			Scope: cloneCycleNormalizedScope(store.captured.Scope), Lifecycle: model.StudySessionLifecycleActive,
			CorpusSHA256: store.captured.CorpusSHA256, StateVersion: 1,
		}
		store.sessionItems[predecessorID] = []model.StudySessionItem{}
		input := cycleMaterializationInputForTest(1)
		input.PredecessorSessionID = &predecessorID
		input.PredecessorFlushReceiptID = uuid.New()
		_, err := materializer.Materialize(context.Background(), store, input)
		assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, "invalid_authority")
	})
}

func TestCycleReceiptPredecessorRequiresExactDispositionTuple(t *testing.T) {
	t.Parallel()

	rootID := uuid.New()
	predecessorID := uuid.New()
	successorID := uuid.New()
	root := CyclePersistenceResult{
		Session: model.StudySession{ID: rootID},
		Receipt: model.StudyCreationReceipt{Disposition: model.StudyCreationDispositionCreated},
	}
	successor := CyclePersistenceResult{
		Session: model.StudySession{ID: successorID, PredecessorStudySessionID: &predecessorID},
		Receipt: model.StudyCreationReceipt{Disposition: model.StudyCreationDispositionSuccessor},
	}
	convergedSuccessor := successor
	convergedSuccessor.Receipt.Disposition = model.StudyCreationDispositionConvergedToExisting
	createdSuccessor := successor
	createdSuccessor.Receipt.Disposition = model.StudyCreationDispositionCreated
	successorWithoutPredecessor := root
	successorWithoutPredecessor.Receipt.Disposition = model.StudyCreationDispositionSuccessor

	for _, testCase := range []struct {
		name        string
		persistence CyclePersistenceResult
		requested   *uuid.UUID
		implicit    bool
		want        bool
	}{
		{name: "created root", persistence: root, want: true},
		{name: "created cannot target successor", persistence: createdSuccessor},
		{name: "successor exact predecessor", persistence: successor, requested: &predecessorID, want: true},
		{name: "successor own id is not predecessor", persistence: successor, requested: &successorID},
		{name: "successor cross predecessor", persistence: successor, requested: &rootID},
		{name: "successor requires predecessor", persistence: successorWithoutPredecessor, requested: &predecessorID},
		{name: "successor nil request needs waiting fact", persistence: successor},
		{name: "verified implicit waiting replacement", persistence: successor, implicit: true, want: true},
		{name: "implicit fact cannot accompany explicit predecessor", persistence: successor, requested: &predecessorID, implicit: true},
		{name: "converged root", persistence: CyclePersistenceResult{Session: root.Session, Receipt: model.StudyCreationReceipt{Disposition: model.StudyCreationDispositionConvergedToExisting}}, want: true},
		{name: "converged may target active successor", persistence: convergedSuccessor, want: true},
		{name: "converged cannot satisfy explicit successor", persistence: convergedSuccessor, requested: &predecessorID},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := validCycleReceiptPredecessor(testCase.persistence, testCase.requested, testCase.implicit); got != testCase.want {
				t.Fatalf("receipt predecessor tuple valid = %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestCycleSessionLifecycleAuthorityMatrix(t *testing.T) {
	t.Parallel()

	scope, err := model.NormalizeStudyScope(model.StudyScope{
		TopicIDs: []uuid.UUID{uuid.New()}, TopicRefs: []string{"lifecycle-matrix"}, CardKind: model.StudyCardKindIdea,
	})
	if err != nil {
		t.Fatalf("normalize lifecycle scope: %v", err)
	}
	emptyCorpusSHA256, err := materializationCorpusSHA256(scope, nil)
	if err != nil {
		t.Fatalf("identify lifecycle empty corpus: %v", err)
	}
	terminalizedAt := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	terminalHighWater := int64(2)
	newScheduled := func(lifecycle model.StudySessionLifecycle) model.StudySession {
		return model.StudySession{
			ID: uuid.New(), LearnerProfileID: uuid.New(), Scope: cloneCycleNormalizedScope(scope),
			Lifecycle: lifecycle, CorpusSHA256: model.SHA256Hex([]byte("scheduled-corpus-" + uuid.NewString())),
			Scheduler: &model.StudySchedulerProvenance{
				Name: VarietyFirstV1Name, Version: VarietyFirstV1Version,
				ConfigurationSHA256:            model.SHA256Hex([]byte("configuration")),
				InputProfileIngestionHighWater: 1, PlannedAt: terminalizedAt, Seed: "lifecycle-seed",
			},
			ProfileIngestionHighWater: 2, StateVersion: 1,
		}
	}
	terminalize := func(session model.StudySession) model.StudySession {
		session.StateVersion = 2
		session.TerminalizedAt = &terminalizedAt
		session.TerminalProfileIngestionHighWater = &terminalHighWater
		return session
	}
	active := newScheduled(model.StudySessionLifecycleActive)
	waiting := model.StudySession{
		ID: uuid.New(), LearnerProfileID: uuid.New(), Scope: cloneCycleNormalizedScope(scope),
		Lifecycle: model.StudySessionLifecycleWaitingForCorpus, CorpusSHA256: emptyCorpusSHA256,
		ProfileIngestionHighWater: 2, StateVersion: 1,
	}
	superseded := terminalize(waiting)
	superseded.Lifecycle = model.StudySessionLifecycleSupersededEmpty
	supersededSuccessor := uuid.New()
	superseded.SuccessorStudySessionID = &supersededSuccessor
	completed := terminalize(newScheduled(model.StudySessionLifecycleCompleted))
	completedSuccessor := uuid.New()
	completed.SuccessorStudySessionID = &completedSuccessor
	abandonedEmpty := terminalize(waiting)
	abandonedEmpty.Lifecycle = model.StudySessionLifecycleAbandoned
	abandonedScheduled := terminalize(newScheduled(model.StudySessionLifecycleAbandoned))

	for _, testCase := range []struct {
		name      string
		session   model.StudySession
		itemCount int
	}{
		{name: "active", session: active, itemCount: 1},
		{name: "waiting", session: waiting},
		{name: "superseded empty", session: superseded},
		{name: "completed", session: completed, itemCount: 1},
		{name: "abandoned empty", session: abandonedEmpty},
		{name: "abandoned scheduled", session: abandonedScheduled, itemCount: 1},
	} {
		t.Run("valid "+testCase.name, func(t *testing.T) {
			if err := validateCycleSessionLifecycleAuthority(testCase.session, testCase.itemCount); err != nil {
				t.Fatalf("valid lifecycle rejected: %v", err)
			}
		})
	}

	zeroTime := time.Time{}
	zeroUUID := uuid.Nil
	for _, testCase := range []struct {
		name      string
		base      model.StudySession
		itemCount int
		mutate    func(*model.StudySession)
	}{
		{name: "active terminal fields", base: active, itemCount: 1, mutate: func(session *model.StudySession) {
			session.TerminalizedAt = &terminalizedAt
			session.TerminalProfileIngestionHighWater = &terminalHighWater
		}},
		{name: "waiting terminal fields", base: waiting, mutate: func(session *model.StudySession) {
			session.TerminalizedAt = &terminalizedAt
			session.TerminalProfileIngestionHighWater = &terminalHighWater
		}},
		{name: "zero predecessor", base: active, itemCount: 1, mutate: func(session *model.StudySession) { session.PredecessorStudySessionID = &zeroUUID }},
		{name: "self predecessor", base: active, itemCount: 1, mutate: func(session *model.StudySession) {
			identity := session.ID
			session.PredecessorStudySessionID = &identity
		}},
		{name: "equal predecessor successor", base: completed, itemCount: 1, mutate: func(session *model.StudySession) {
			identity := *session.SuccessorStudySessionID
			session.PredecessorStudySessionID = &identity
		}},
		{name: "zero successor", base: completed, itemCount: 1, mutate: func(session *model.StudySession) { session.SuccessorStudySessionID = &zeroUUID }},
		{name: "self successor", base: completed, itemCount: 1, mutate: func(session *model.StudySession) { identity := session.ID; session.SuccessorStudySessionID = &identity }},
		{name: "terminal missing time", base: completed, itemCount: 1, mutate: func(session *model.StudySession) { session.TerminalizedAt = nil }},
		{name: "terminal zero time", base: completed, itemCount: 1, mutate: func(session *model.StudySession) { session.TerminalizedAt = &zeroTime }},
		{name: "terminal missing high-water", base: completed, itemCount: 1, mutate: func(session *model.StudySession) { session.TerminalProfileIngestionHighWater = nil }},
		{name: "terminal high-water exceeds live", base: completed, itemCount: 1, mutate: func(session *model.StudySession) {
			highWater := session.ProfileIngestionHighWater + 1
			session.TerminalProfileIngestionHighWater = &highWater
		}},
		{name: "scheduler high-water exceeds live", base: active, itemCount: 1, mutate: func(session *model.StudySession) {
			session.Scheduler.InputProfileIngestionHighWater = session.ProfileIngestionHighWater + 1
		}},
		{name: "terminal high-water predates scheduler", base: completed, itemCount: 1, mutate: func(session *model.StudySession) {
			session.Scheduler.InputProfileIngestionHighWater = 2
			terminal := int64(1)
			session.TerminalProfileIngestionHighWater = &terminal
		}},
		{name: "active successor", base: active, itemCount: 1, mutate: func(session *model.StudySession) {
			successor := uuid.New()
			session.SuccessorStudySessionID = &successor
		}},
		{name: "waiting scheduler", base: waiting, mutate: func(session *model.StudySession) {
			session.Scheduler = cloneCycleSchedulerProvenancePointer(active.Scheduler)
		}},
		{name: "waiting items", base: waiting, itemCount: 1, mutate: func(*model.StudySession) {}},
		{name: "waiting successor", base: waiting, mutate: func(session *model.StudySession) {
			successor := uuid.New()
			session.SuccessorStudySessionID = &successor
		}},
		{name: "superseded missing successor", base: superseded, mutate: func(session *model.StudySession) { session.SuccessorStudySessionID = nil }},
		{name: "completed missing successor", base: completed, itemCount: 1, mutate: func(session *model.StudySession) { session.SuccessorStudySessionID = nil }},
		{name: "completed missing items", base: completed, mutate: func(*model.StudySession) {}},
		{name: "abandoned successor", base: abandonedScheduled, itemCount: 1, mutate: func(session *model.StudySession) {
			successor := uuid.New()
			session.SuccessorStudySessionID = &successor
		}},
		{name: "abandoned mixed authority", base: abandonedEmpty, itemCount: 1, mutate: func(*model.StudySession) {}},
	} {
		t.Run("invalid "+testCase.name, func(t *testing.T) {
			session := cloneCycleSession(testCase.base)
			testCase.mutate(&session)
			if err := validateCycleSessionLifecycleAuthority(session, testCase.itemCount); err == nil {
				t.Fatal("invalid lifecycle authority was accepted")
			}
		})
	}
}

func TestCycleMaterializerRejectsUnresolvedPreflightAndInvalidCapturedPredecessor(t *testing.T) {
	t.Parallel()

	t.Run("root active must have resolved in preflight", func(t *testing.T) {
		materializer, store, _ := activePredecessorFixtureForTest(t)
		_, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(2))
		assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, "unresolved_preflight")
		assertNoCyclePlanningAfterCapture(t, store)
	})

	t.Run("explicit predecessor requires locked nonterminal", func(t *testing.T) {
		materializer := newCycleMaterializerForTest(t)
		store := newCycleMaterializationStoreForTest(t, 2)
		predecessorID := uuid.New()
		input := cycleMaterializationInputForTest(2)
		input.PredecessorSessionID = &predecessorID
		input.PredecessorFlushReceiptID = uuid.New()
		_, err := materializer.Materialize(context.Background(), store, input)
		assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, "invalid_authority")
		assertNoCyclePlanningAfterCapture(t, store)
	})

	t.Run("explicit predecessor must equal locked nonterminal", func(t *testing.T) {
		materializer, store, _ := activePredecessorFixtureForTest(t)
		predecessorID := uuid.New()
		input := cycleMaterializationInputForTest(2)
		input.PredecessorSessionID = &predecessorID
		input.PredecessorFlushReceiptID = uuid.New()
		_, err := materializer.Materialize(context.Background(), store, input)
		assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, "invalid_authority")
		assertNoCyclePlanningAfterCapture(t, store)
	})

	t.Run("captured predecessor live high-water must equal profile", func(t *testing.T) {
		materializer, store, predecessorID := activePredecessorFixtureForTest(t)
		store.captured.NonterminalSession.ProfileIngestionHighWater++
		input := cycleMaterializationInputForTest(2)
		input.PredecessorSessionID = &predecessorID
		input.PredecessorFlushReceiptID = uuid.New()
		_, err := materializer.Materialize(context.Background(), store, input)
		assertCycleMaterializationError(t, err, model.StudyErrorCodeInternal, "invalid_authority")
		assertNoCyclePlanningAfterCapture(t, store)
	})

	for _, testCase := range []struct {
		name   string
		mutate func(*cycleMaterializationStoreForTest, uuid.UUID)
	}{
		{name: "over-bound predecessor items", mutate: func(store *cycleMaterializationStoreForTest, predecessorID uuid.UUID) {
			store.sessionItems[predecessorID] = make([]model.StudySessionItem, StudyMaximumSchedulingCards+1)
		}},
		{name: "gapped predecessor position", mutate: func(store *cycleMaterializationStoreForTest, predecessorID uuid.UUID) {
			store.sessionItems[predecessorID][1].Position = 3
		}},
		{name: "duplicate predecessor item identity", mutate: func(store *cycleMaterializationStoreForTest, predecessorID uuid.UUID) {
			store.sessionItems[predecessorID][1].ID = store.sessionItems[predecessorID][0].ID
		}},
		{name: "duplicate predecessor card identity", mutate: func(store *cycleMaterializationStoreForTest, predecessorID uuid.UUID) {
			store.sessionItems[predecessorID][1].CardIdentity = store.sessionItems[predecessorID][0].CardIdentity
		}},
		{name: "nil predecessor tail identity", mutate: func(store *cycleMaterializationStoreForTest, predecessorID uuid.UUID) {
			store.sessionItems[predecessorID][1].CardIdentity = uuid.Nil
		}},
		{name: "substituted predecessor content", mutate: func(store *cycleMaterializationStoreForTest, predecessorID uuid.UUID) {
			store.sessionItems[predecessorID][1].BackDefinition = "substituted predecessor content"
		}},
		{name: "substituted predecessor corpus", mutate: func(store *cycleMaterializationStoreForTest, _ uuid.UUID) {
			store.captured.NonterminalSession.CorpusSHA256 = model.SHA256Hex([]byte("substituted predecessor corpus"))
		}},
		{name: "substituted predecessor raw topic", mutate: func(store *cycleMaterializationStoreForTest, _ uuid.UUID) {
			store.captured.NonterminalSession.Scope.Scope.TopicIDs[0] = uuid.New()
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			materializer, store, predecessorID := activePredecessorFixtureForTest(t)
			testCase.mutate(store, predecessorID)
			input := cycleMaterializationInputForTest(2)
			input.PredecessorSessionID = &predecessorID
			input.PredecessorFlushReceiptID = uuid.New()
			result, err := materializer.Materialize(context.Background(), store, input)
			if result != nil || err == nil {
				t.Fatalf("invalid predecessor authority accepted: result=%#v err=%v", result, err)
			}
			assertNoCyclePlanningAfterCapture(t, store)
		})
	}

	t.Run("waiting predecessor cannot contain an item", func(t *testing.T) {
		materializer := newCycleMaterializerForTest(t)
		store := newCycleMaterializationStoreForTest(t, 1)
		waitingID := uuid.New()
		emptyCorpusSHA256, err := materializationCorpusSHA256(store.captured.Scope, nil)
		if err != nil {
			t.Fatalf("identify waiting corpus: %v", err)
		}
		store.captured.NonterminalSession = &model.StudySession{
			ID: waitingID, LearnerProfileID: store.captured.LearnerProfile.ID,
			Scope: cloneCycleNormalizedScope(store.captured.Scope), Lifecycle: model.StudySessionLifecycleWaitingForCorpus,
			CorpusSHA256: emptyCorpusSHA256, ProfileIngestionHighWater: store.captured.LearnerProfile.ProfileIngestionHighWater, StateVersion: 1,
		}
		store.sessionItems[waitingID] = []model.StudySessionItem{{ID: uuid.New(), StudySessionID: waitingID}}
		result, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(1))
		if result != nil || err == nil {
			t.Fatalf("itemful waiting predecessor accepted: result=%#v err=%v", result, err)
		}
		assertNoCyclePlanningAfterCapture(t, store)
	})
}

func activePredecessorFixtureForTest(
	t *testing.T,
) (*CycleMaterializer, *cycleMaterializationStoreForTest, uuid.UUID) {
	t.Helper()
	materializer := newCycleMaterializerForTest(t)
	store := newCycleMaterializationStoreForTest(t, 2)
	created, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(2))
	if err != nil {
		t.Fatalf("materialize active predecessor fixture: %v", err)
	}
	predecessorID := created.Persistence.Session.ID
	nonterminal := cloneCycleSession(created.Persistence.Session)
	store.captured.NonterminalSession = &nonterminal
	store.sessionItems[predecessorID] = cloneCyclePersistenceResult(created.Persistence).Items
	store.resolveCalls = 0
	store.captureCalls = 0
	store.prefixCalls = 0
	store.projectionCalls = 0
	store.insertCalls = 0
	return materializer, store, predecessorID
}

func assertNoCyclePlanningAfterCapture(t *testing.T, store *cycleMaterializationStoreForTest) {
	t.Helper()
	if store.prefixCalls != 0 || store.projectionCalls != 0 || store.insertCalls != 0 {
		t.Fatalf("invalid captured path reached planning: prefix=%d projection=%d insert=%d",
			store.prefixCalls, store.projectionCalls, store.insertCalls)
	}
}

func TestCycleMaterializerRejectsUnboundedStoreAndSchedulerReturnsBeforeDeepClone(t *testing.T) {
	t.Parallel()

	t.Run("early persisted items", func(t *testing.T) {
		materializer, store, input, resolution := validWaitingReplacementResolutionForTest(t)
		resolution.Persistence.Items = make([]model.StudySessionItem, StudyMaximumSchedulingCards+1)
		store.resolveOverride = func(ExistingCycleResolutionInput) (*ExistingCycleResolution, error) { return resolution, nil }
		_, err := materializer.Materialize(context.Background(), store, input)
		assertCycleMaterializationError(t, err, model.StudyErrorCodeRequestTooLarge, "work_bound_exceeded")
		if store.captureCalls != 0 {
			t.Fatal("over-bound early persistence reached corpus capture")
		}
	})

	t.Run("early selector bytes", func(t *testing.T) {
		materializer, store, input, resolution := validWaitingReplacementResolutionForTest(t)
		resolution.Scope.CanonicalSelector = make([]byte, StudyMaximumCanonicalSelectorBytes+1)
		store.resolveOverride = func(ExistingCycleResolutionInput) (*ExistingCycleResolution, error) { return resolution, nil }
		_, err := materializer.Materialize(context.Background(), store, input)
		assertCycleMaterializationError(t, err, model.StudyErrorCodeRequestTooLarge, "work_bound_exceeded")
		if store.captureCalls != 0 {
			t.Fatal("over-bound early scope reached corpus capture")
		}
	})

	t.Run("captured selector bytes", func(t *testing.T) {
		materializer := newCycleMaterializerForTest(t)
		store := newCycleMaterializationStoreForTest(t, 1)
		store.captured.Scope.CanonicalSelector = make([]byte, StudyMaximumCanonicalSelectorBytes+1)
		_, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(1))
		assertCycleMaterializationError(t, err, model.StudyErrorCodeRequestTooLarge, "work_bound_exceeded")
		if store.prefixCalls != 0 || store.projectionCalls != 0 || store.insertCalls != 0 {
			t.Fatal("over-bound captured scope reached planning")
		}
	})

	t.Run("insert result items", func(t *testing.T) {
		materializer := newCycleMaterializerForTest(t)
		store := newCycleMaterializationStoreForTest(t, 1)
		store.insertOverride = func(input CyclePersistenceInput) *CyclePersistenceResult {
			result := store.defaultInsertResult(input)
			result.Items = make([]model.StudySessionItem, StudyMaximumSchedulingCards+1)
			return result
		}
		_, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(1))
		assertCycleMaterializationError(t, err, model.StudyErrorCodeRequestTooLarge, "work_bound_exceeded")
	})

	t.Run("scheduler cards", func(t *testing.T) {
		canonical := newCycleMaterializerForTest(t)
		materializer, err := newCycleMaterializer(
			cycleMaterializationSchedulerFunc(func(StudySchedulingSnapshot) (StudyPlan, error) {
				return StudyPlan{Cards: make([]StudyPlannedCard, StudyMaximumSchedulingCards+1)}, nil
			}),
			canonical.configuration,
		)
		if err != nil {
			t.Fatalf("construct over-bound scheduler: %v", err)
		}
		store := newCycleMaterializationStoreForTest(t, 1)
		_, err = materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(1))
		assertCycleMaterializationError(t, err, model.StudyErrorCodeRequestTooLarge, "work_bound_exceeded")
		if store.insertCalls != 0 {
			t.Fatal("over-bound scheduler output reached persistence")
		}
	})
}

func TestCycleMaterializerValidatesImplicitWaitingReplacementEvidenceBeforeMutableReads(t *testing.T) {
	t.Parallel()

	t.Run("valid nested waiting replacement", func(t *testing.T) {
		materializer, store, input, resolution := validWaitingReplacementResolutionForTest(t)
		store.resolveOverride = func(ExistingCycleResolutionInput) (*ExistingCycleResolution, error) {
			return resolution, nil
		}
		result, err := materializer.Materialize(context.Background(), store, input)
		if err != nil || result == nil || result.Persistence.Session.ID != resolution.Persistence.Session.ID ||
			result.SchedulingSnapshot != nil || result.AuthoritativePlan == nil {
			t.Fatalf("valid implicit waiting replacement replay = result:%#v err:%v", result, err)
		}
		if store.captureCalls != 0 || store.prefixCalls != 0 || store.projectionCalls != 0 || store.insertCalls != 0 {
			t.Fatalf("valid replay consulted mutable authority: capture=%d prefix=%d projection=%d insert=%d",
				store.captureCalls, store.prefixCalls, store.projectionCalls, store.insertCalls)
		}
	})

	t.Run("renamed topic references retain historical authority", func(t *testing.T) {
		materializer, store, input, resolution := validWaitingReplacementResolutionForTest(t)
		renamedScope, err := model.NormalizeStudyScope(model.StudyScope{
			TopicIDs:  append([]uuid.UUID(nil), resolution.Scope.Scope.TopicIDs...),
			TopicRefs: []string{"renamed-materializer-a", "renamed-materializer-b"},
			CardKind:  model.StudyCardKindIdea,
		})
		if err != nil {
			t.Fatalf("normalize renamed scope: %v", err)
		}
		resolution.Scope = renamedScope
		resolution.Persistence.Receipt.Scope = cloneCycleNormalizedScope(renamedScope)
		store.resolveOverride = func(ExistingCycleResolutionInput) (*ExistingCycleResolution, error) {
			return resolution, nil
		}
		result, err := materializer.Materialize(context.Background(), store, input)
		if err != nil || result == nil || result.Persistence.Session.ID != resolution.Persistence.Session.ID {
			t.Fatalf("renamed topic reference replay = result:%#v err:%v", result, err)
		}
	})

	for _, testCase := range []struct {
		name   string
		mutate func(*ExistingCycleResolution)
	}{
		{name: "nil evidence", mutate: func(resolution *ExistingCycleResolution) { resolution.ImplicitWaitingReplacement = nil }},
		{name: "wrong predecessor id", mutate: func(resolution *ExistingCycleResolution) {
			resolution.ImplicitWaitingReplacement.Predecessor.ID = uuid.New()
		}},
		{name: "wrong lifecycle", mutate: func(resolution *ExistingCycleResolution) {
			resolution.ImplicitWaitingReplacement.Predecessor.Lifecycle = model.StudySessionLifecycleCompleted
		}},
		{name: "wrong reciprocal link", mutate: func(resolution *ExistingCycleResolution) {
			replacement := uuid.New()
			resolution.ImplicitWaitingReplacement.Predecessor.SuccessorStudySessionID = &replacement
		}},
		{name: "wrong profile", mutate: func(resolution *ExistingCycleResolution) {
			resolution.ImplicitWaitingReplacement.Predecessor.LearnerProfileID = uuid.New()
		}},
		{name: "wrong predecessor live high-water", mutate: func(resolution *ExistingCycleResolution) {
			resolution.ImplicitWaitingReplacement.Predecessor.ProfileIngestionHighWater++
		}},
		{name: "wrong locked live high-water", mutate: func(resolution *ExistingCycleResolution) {
			resolution.LearnerProfile.ProfileIngestionHighWater++
		}},
		{name: "raw topic identity substitution", mutate: func(resolution *ExistingCycleResolution) {
			resolution.ImplicitWaitingReplacement.Predecessor.Scope.Scope.TopicIDs[0] = uuid.New()
		}},
		{name: "unsorted raw topic identities", mutate: func(resolution *ExistingCycleResolution) {
			topics := resolution.ImplicitWaitingReplacement.Predecessor.Scope.Scope.TopicIDs
			topics[0], topics[1] = topics[1], topics[0]
		}},
		{name: "unsorted raw topic references", mutate: func(resolution *ExistingCycleResolution) {
			refs := resolution.ImplicitWaitingReplacement.Predecessor.Scope.Scope.TopicRefs
			refs[0], refs[1] = refs[1], refs[0]
		}},
		{name: "scheduler substitution", mutate: func(resolution *ExistingCycleResolution) {
			scheduler := *resolution.Persistence.Session.Scheduler
			resolution.ImplicitWaitingReplacement.Predecessor.Scheduler = &scheduler
		}},
		{name: "nonempty predecessor items", mutate: func(resolution *ExistingCycleResolution) {
			resolution.ImplicitWaitingReplacement.Items = []model.StudySessionItem{{ID: uuid.New()}}
		}},
		{name: "nonempty corpus", mutate: func(resolution *ExistingCycleResolution) {
			resolution.ImplicitWaitingReplacement.Predecessor.CorpusSHA256 = model.SHA256Hex([]byte("not-empty"))
		}},
		{name: "unterminalized state version", mutate: func(resolution *ExistingCycleResolution) {
			resolution.ImplicitWaitingReplacement.Predecessor.StateVersion = 1
		}},
		{name: "missing terminal time", mutate: func(resolution *ExistingCycleResolution) {
			resolution.ImplicitWaitingReplacement.Predecessor.TerminalizedAt = nil
		}},
		{name: "missing terminal high-water", mutate: func(resolution *ExistingCycleResolution) {
			resolution.ImplicitWaitingReplacement.Predecessor.TerminalProfileIngestionHighWater = nil
		}},
		{name: "self predecessor", mutate: func(resolution *ExistingCycleResolution) {
			predecessorID := resolution.ImplicitWaitingReplacement.Predecessor.ID
			resolution.ImplicitWaitingReplacement.Predecessor.PredecessorStudySessionID = &predecessorID
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			materializer, store, input, resolution := validWaitingReplacementResolutionForTest(t)
			testCase.mutate(resolution)
			store.resolveOverride = func(ExistingCycleResolutionInput) (*ExistingCycleResolution, error) {
				return resolution, nil
			}
			result, err := materializer.Materialize(context.Background(), store, input)
			if result != nil || err == nil {
				t.Fatalf("substituted waiting evidence was accepted: result=%#v err=%v", result, err)
			}
			if store.captureCalls != 0 || store.prefixCalls != 0 || store.projectionCalls != 0 || store.insertCalls != 0 {
				t.Fatalf("invalid evidence reached mutable authority: capture=%d prefix=%d projection=%d insert=%d",
					store.captureCalls, store.prefixCalls, store.projectionCalls, store.insertCalls)
			}
		})
	}
}

func validWaitingReplacementResolutionForTest(
	t *testing.T,
) (*CycleMaterializer, *cycleMaterializationStoreForTest, CycleMaterializationInput, *ExistingCycleResolution) {
	t.Helper()
	materializer := newCycleMaterializerForTest(t)
	store := newCycleMaterializationStoreForTest(t, 2)
	secondTopicID := uuid.New()
	scope, err := model.NormalizeStudyScope(model.StudyScope{
		TopicIDs:  []uuid.UUID{store.captured.Scope.Scope.TopicIDs[0], secondTopicID},
		TopicRefs: []string{store.captured.Scope.Scope.TopicRefs[0], "materializer-test-second"},
		CardKind:  model.StudyCardKindIdea,
	})
	if err != nil {
		t.Fatalf("normalize waiting replacement scope: %v", err)
	}
	store.captured.Scope = scope
	store.captured.CorpusSHA256, err = materializationCorpusSHA256(scope, store.captured.Sources)
	if err != nil {
		t.Fatalf("identify waiting replacement source corpus: %v", err)
	}
	input := cycleMaterializationInputForTest(2)
	created, err := materializer.Materialize(context.Background(), store, input)
	if err != nil {
		t.Fatalf("materialize waiting replacement replay authority: %v", err)
	}
	persistence := cloneCyclePersistenceResult(created.Persistence)
	persistence.Created = false
	persistence.Receipt.Disposition = model.StudyCreationDispositionSuccessor
	waitingID := uuid.New()
	persistence.Session.PredecessorStudySessionID = &waitingID
	replacementID := persistence.Session.ID
	terminalizedAt := time.Date(2026, 8, 17, 11, 59, 0, 0, time.UTC)
	terminalHighWater := store.captured.LearnerProfile.ProfileIngestionHighWater
	priorPredecessorID := uuid.New()
	emptyCorpusSHA256, err := materializationCorpusSHA256(store.captured.Scope, nil)
	if err != nil {
		t.Fatalf("identify waiting replacement evidence corpus: %v", err)
	}
	evidence := &WaitingReplacementEvidence{
		Predecessor: model.StudySession{
			ID:                                waitingID,
			LearnerProfileID:                  store.captured.LearnerProfile.ID,
			Scope:                             cloneCycleNormalizedScope(store.captured.Scope),
			Lifecycle:                         model.StudySessionLifecycleSupersededEmpty,
			CorpusSHA256:                      emptyCorpusSHA256,
			PredecessorStudySessionID:         &priorPredecessorID,
			SuccessorStudySessionID:           &replacementID,
			ProfileIngestionHighWater:         terminalHighWater,
			StateVersion:                      2,
			TerminalizedAt:                    &terminalizedAt,
			TerminalProfileIngestionHighWater: &terminalHighWater,
		},
		Items: []model.StudySessionItem{},
	}
	store.resolveCalls = 0
	store.captureCalls = 0
	store.prefixCalls = 0
	store.projectionCalls = 0
	store.insertCalls = 0
	return materializer, store, input, &ExistingCycleResolution{
		LearnerProfile:             store.captured.LearnerProfile,
		Scope:                      cloneCycleNormalizedScope(store.captured.Scope),
		Persistence:                persistence,
		ImplicitWaitingReplacement: evidence,
	}
}

func TestNewCanonicalCycleMaterializerDerivesIdentityAndRejectsPrivateDrift(t *testing.T) {
	t.Parallel()

	scheduler, err := NewVarietyFirstV1()
	if err != nil {
		t.Fatalf("construct scheduler: %v", err)
	}
	materializer, err := NewCanonicalCycleMaterializer(scheduler)
	if err != nil {
		t.Fatalf("construct canonical materializer: %v", err)
	}
	if materializer.configuration.SchedulerName != scheduler.identity.name ||
		materializer.configuration.SchedulerVersion != scheduler.identity.version ||
		materializer.configuration.SchedulerConfigurationSHA256 != scheduler.identity.configurationSHA256 ||
		materializer.configuration.ProjectionSchemaVersion != StudyProjectionSchemaVersionV1 {
		t.Fatalf("canonical materializer identity drifted: %#v", materializer.configuration)
	}

	canonical := materializer.configuration
	for _, testCase := range []struct {
		name   string
		mutate func(*cycleMaterializerConfiguration)
	}{
		{name: "name", mutate: func(configuration *cycleMaterializerConfiguration) { configuration.SchedulerName = "forged" }},
		{name: "version", mutate: func(configuration *cycleMaterializerConfiguration) { configuration.SchedulerVersion = "forged" }},
		{name: "configuration digest", mutate: func(configuration *cycleMaterializerConfiguration) {
			configuration.SchedulerConfigurationSHA256 = model.SHA256Hex([]byte("forged"))
		}},
		{name: "projection schema", mutate: func(configuration *cycleMaterializerConfiguration) { configuration.ProjectionSchemaVersion++ }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			configuration := canonical
			testCase.mutate(&configuration)
			_, err := newCycleMaterializer(scheduler, configuration)
			assertCycleMaterializationError(t, err, model.StudyErrorCodeSchemaInvalid, "invalid_configuration")
		})
	}
}

func assertCycleMaterializationError(
	t *testing.T,
	err error,
	wantCode model.StudyErrorCode,
	wantReason string,
) {
	t.Helper()
	var studyError *model.StudyError
	if !errors.As(err, &studyError) || studyError.Code != wantCode || studyError.Context["reason"] != wantReason {
		t.Fatalf("study materialization error = %#v, want code=%s reason=%s", err, wantCode, wantReason)
	}
}

type cycleMaterializationSchedulerFunc func(StudySchedulingSnapshot) (StudyPlan, error)

func (function cycleMaterializationSchedulerFunc) Plan(snapshot StudySchedulingSnapshot) (StudyPlan, error) {
	return function(snapshot)
}

type cycleMaterializationStoreForTest struct {
	captured        CapturedEligibleCorpus
	projections     map[uuid.UUID]model.StudySchedulerCardProjection
	sessionItems    map[uuid.UUID][]model.StudySessionItem
	acceptedPrefix  []model.StudyEvent
	resolveCalls    int
	captureCalls    int
	prefixCalls     int
	projectionCalls int
	insertCalls     int
	lastInsert      CyclePersistenceInput
	resolveOverride func(ExistingCycleResolutionInput) (*ExistingCycleResolution, error)
	insertOverride  func(CyclePersistenceInput) *CyclePersistenceResult
}

func newCycleMaterializationStoreForTest(t *testing.T, sourceCount int) *cycleMaterializationStoreForTest {
	t.Helper()
	topicID := uuid.New()
	scope, err := model.NormalizeStudyScope(model.StudyScope{
		TopicIDs: []uuid.UUID{topicID}, TopicRefs: []string{"materializer-test"}, CardKind: model.StudyCardKindIdea,
	})
	if err != nil {
		t.Fatalf("normalize scope: %v", err)
	}
	store := &cycleMaterializationStoreForTest{
		captured: CapturedEligibleCorpus{
			LearnerProfile: model.StudyLearnerProfile{ID: uuid.New(), Ref: "materializer-test", ProfileIngestionHighWater: 0},
			Scope:          scope,
			CorpusSHA256:   model.SHA256Hex([]byte("materializer-corpus")),
			Sources:        make([]MaterializationSource, sourceCount),
		},
		projections:  make(map[uuid.UUID]model.StudySchedulerCardProjection),
		sessionItems: make(map[uuid.UUID][]model.StudySessionItem),
	}
	for index := range store.captured.Sources {
		identity := uuid.New()
		store.captured.Sources[index] = MaterializationSource{
			CardIdentity:             identity,
			LiveSourceIdeaID:         identity,
			HistoricalSourceKind:     string(model.StudyCardKindIdea),
			HistoricalSourceIdentity: identity,
			SourceContentRevision:    "study-card-source-v1:" + model.SHA256Hex([]byte("revision-"+identity.String())),
			FrontCanonicalName:       "front " + identity.String(),
			BackDefinition:           "back " + identity.String(),
		}
		contentSHA256, digestErr := studydigest.CardContent(
			[]byte(model.StudyCardKindIdea),
			[]byte(identity.String()),
			[]byte(identity.String()),
			[]byte(store.captured.Sources[index].FrontCanonicalName),
			[]byte(store.captured.Sources[index].BackDefinition),
			[]byte{0},
			nil,
		)
		if digestErr != nil {
			t.Fatalf("identify materializer content: %v", digestErr)
		}
		store.captured.Sources[index].SourceContentSHA256 = contentSHA256
	}
	store.captured.CorpusSHA256, err = materializationCorpusSHA256(store.captured.Scope, store.captured.Sources)
	if err != nil {
		t.Fatalf("identify materializer corpus: %v", err)
	}
	return store
}

func (store *cycleMaterializationStoreForTest) CaptureEligibleCorpus(context.Context) (CapturedEligibleCorpus, error) {
	store.captureCalls++
	return store.captured, nil
}

func (store *cycleMaterializationStoreForTest) ResolveExistingCycle(
	_ context.Context,
	input ExistingCycleResolutionInput,
) (*ExistingCycleResolution, error) {
	store.resolveCalls++
	if store.resolveOverride == nil {
		return nil, nil
	}
	return store.resolveOverride(input)
}

func (store *cycleMaterializationStoreForTest) ReadAcceptedPrefix(context.Context, int64) ([]model.StudyEvent, error) {
	store.prefixCalls++
	events := make([]model.StudyEvent, len(store.acceptedPrefix))
	for index := range store.acceptedPrefix {
		events[index] = cloneCycleStudyEvent(store.acceptedPrefix[index])
	}
	return events, nil
}

func (store *cycleMaterializationStoreForTest) ResolveCardIdentity(_ context.Context, identity uuid.UUID) (uuid.UUID, error) {
	return identity, nil
}

func (store *cycleMaterializationStoreForTest) GetProjection(_ context.Context, key MaterializationProjectionKey) (*model.StudySchedulerCardProjection, error) {
	store.projectionCalls++
	projection, exists := store.projections[key.CardIdentity]
	if !exists {
		return nil, nil
	}
	clone := cloneCycleProjection(projection)
	return &clone, nil
}

func (store *cycleMaterializationStoreForTest) ReplaceProjection(
	_ context.Context,
	projection model.StudySchedulerCardProjection,
	expectedRevision int64,
) (*model.StudySchedulerCardProjection, error) {
	store.projectionCalls++
	projection = cloneCycleProjection(projection)
	projection.ProjectionRevision = expectedRevision + 1
	store.projections[projection.ProjectedCardIdentity] = projection
	clone := cloneCycleProjection(projection)
	return &clone, nil
}

func (store *cycleMaterializationStoreForTest) ListSessionItems(_ context.Context, sessionID uuid.UUID) ([]model.StudySessionItem, error) {
	items := store.sessionItems[sessionID]
	clone := make([]model.StudySessionItem, len(items))
	for index := range items {
		clone[index] = cloneCycleSessionItem(items[index])
	}
	return clone, nil
}

func (store *cycleMaterializationStoreForTest) InsertCycleOnce(_ context.Context, input CyclePersistenceInput) (*CyclePersistenceResult, error) {
	store.insertCalls++
	store.lastInsert = cloneCyclePersistenceInput(input)
	if store.insertOverride != nil {
		return store.insertOverride(input), nil
	}
	return store.defaultInsertResult(input), nil
}

func (store *cycleMaterializationStoreForTest) defaultInsertResult(input CyclePersistenceInput) *CyclePersistenceResult {
	items := make([]model.StudySessionItem, len(input.Items))
	for index := range input.Items {
		items[index] = cloneCycleSessionItem(input.Items[index])
	}
	disposition := model.StudyCreationDispositionCreated
	if input.PredecessorSessionID != nil {
		disposition = model.StudyCreationDispositionSuccessor
	}
	return &CyclePersistenceResult{
		Session: model.StudySession{
			ID:                        input.SessionID,
			LearnerProfileID:          store.captured.LearnerProfile.ID,
			Scope:                     cloneCycleNormalizedScope(store.captured.Scope),
			Lifecycle:                 input.Lifecycle,
			CorpusSHA256:              input.CorpusSHA256,
			Scheduler:                 cloneCycleSchedulerProvenancePointer(input.Scheduler),
			PredecessorStudySessionID: cloneCycleUUIDPointer(input.PredecessorSessionID),
			ProfileIngestionHighWater: store.captured.LearnerProfile.ProfileIngestionHighWater,
			StateVersion:              1,
		},
		Items: items,
		Receipt: model.StudyCreationReceipt{
			StudyPrincipalID:        input.Principal,
			CreationRequestID:       input.CreationRequestID,
			NormalizedRequestSHA256: input.NormalizedRequestSHA256,
			Disposition:             disposition,
			CanonicalStudySessionID: input.SessionID,
			LearnerProfileID:        store.captured.LearnerProfile.ID,
			Scope:                   cloneCycleNormalizedScope(store.captured.Scope),
		},
		Created: true,
	}
}

func newCycleMaterializerForTest(t *testing.T) *CycleMaterializer {
	t.Helper()
	scheduler, err := NewVarietyFirstV1()
	if err != nil {
		t.Fatalf("construct scheduler: %v", err)
	}
	materializer, err := NewCanonicalCycleMaterializer(scheduler)
	if err != nil {
		t.Fatalf("construct materializer: %v", err)
	}
	return materializer
}

func cycleMaterializationInputForTest(itemCount int) CycleMaterializationInput {
	itemIDs := make([]uuid.UUID, itemCount)
	for index := range itemIDs {
		itemIDs[index] = uuid.New()
	}
	return CycleMaterializationInput{
		SessionID:                     uuid.New(),
		ItemIDs:                       itemIDs,
		Principal:                     model.OperatorStudyPrincipalID(),
		CreationRequestID:             uuid.New(),
		NormalizedRequestSHA256:       model.SHA256Hex([]byte("materializer-request-" + uuid.NewString())),
		PlannedAt:                     time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
		Seed:                          "materializer-seed",
		LifecycleReasonCode:           "materialized",
		PredecessorTerminalReasonCode: "cycle_completed",
	}
}
