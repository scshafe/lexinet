package study

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

type fakeStudyShadowAdapter func(context.Context, StudySchedulingSnapshot, StudyPlan) (StudyShadowEvaluation, error)

func (adapter fakeStudyShadowAdapter) Evaluate(
	ctx context.Context,
	snapshot StudySchedulingSnapshot,
	authoritativePlan StudyPlan,
) (StudyShadowEvaluation, error) {
	return adapter(ctx, snapshot, authoritativePlan)
}

type shadowRunnerCallerContextKey struct{}

func TestStudyShadowRunnerNotConfiguredIsExplicit(t *testing.T) {
	t.Parallel()

	runner, err := NewStudyShadowRunner(nil, "", "", 0)
	if err != nil {
		t.Fatalf("construct unconfigured runner: %v", err)
	}
	report := runner.Run(context.Background(), StudySchedulingSnapshot{}, StudyPlan{})
	if report.Authority != StudyShadowAuthorityNonAuthoritative ||
		report.Status != StudyShadowStatusNotConfigured ||
		report.Evaluation != nil || report.Failure != nil {
		t.Fatalf("not-configured report = %#v", report)
	}
}

func TestStudyShadowRunnerReverseOrderAndNestedMutationCannotChangeAuthority(t *testing.T) {
	t.Parallel()

	snapshot, authoritativePlan := studyShadowRunnerFixture()
	snapshotBefore := cloneStudyShadowTestValue(t, snapshot)
	planBefore := cloneStudyShadowTestValue(t, authoritativePlan)
	adapter := fakeStudyShadowAdapter(func(
		adapterContext context.Context,
		privateSnapshot StudySchedulingSnapshot,
		privatePlan StudyPlan,
	) (StudyShadowEvaluation, error) {
		if adapterContext.Value(shadowRunnerCallerContextKey{}) != nil {
			return StudyShadowEvaluation{}, errors.New("caller context value crossed shadow boundary")
		}
		*privateSnapshot.PredecessorTailCardIdentity = studyShadowTestUUIDThree
		*privateSnapshot.Cards[0].Projection.LatestEffectiveRating = model.StudyRatingEasy
		*privateSnapshot.Cards[0].Projection.LatestPresentedIngestionOrdinal = 999
		privateSnapshot.Cards[0], privateSnapshot.Cards[1] = privateSnapshot.Cards[1], privateSnapshot.Cards[0]
		*privatePlan.Cards[0].SelectionReason.LatestPresentedIngestionOrdinal = 998
		privatePlan.Cards[0].CardIdentity = studyShadowTestUUIDThree
		privatePlan.Scheduler.Seed = "mutated-private-seed"
		return StudyShadowEvaluation{
			OrderedCardIdentities: []uuid.UUID{studyShadowTestUUIDTwo, studyShadowTestUUIDOne},
			Payload:               json.RawMessage(`{"comparison":"reverse"}`),
		}, nil
	})
	runner := mustStudyShadowRunner(t, adapter, time.Second)
	callerContext := context.WithValue(context.Background(), shadowRunnerCallerContextKey{}, "private-caller-value")
	report := runner.Run(callerContext, snapshot, authoritativePlan)
	if report.Status != StudyShadowStatusCompleted || report.Failure != nil || report.Evaluation == nil {
		t.Fatalf("completed report = %#v", report)
	}
	if !reflect.DeepEqual(report.Evaluation.OrderedCardIdentities, []uuid.UUID{studyShadowTestUUIDTwo, studyShadowTestUUIDOne}) {
		t.Fatalf("shadow reverse order = %v", report.Evaluation.OrderedCardIdentities)
	}
	assertStudyShadowCallerValuesUnchanged(t, snapshot, snapshotBefore, authoritativePlan, planBefore)
}

func TestStudyShadowRunnerClonesReturnedEvaluationAliases(t *testing.T) {
	t.Parallel()

	snapshot, authoritativePlan := studyShadowRunnerFixture()
	snapshotBefore := cloneStudyShadowTestValue(t, snapshot)
	planBefore := cloneStudyShadowTestValue(t, authoritativePlan)
	adapterOrder := []uuid.UUID{studyShadowTestUUIDOne, studyShadowTestUUIDTwo}
	adapterPayload := json.RawMessage(`{"score":1}`)
	adapter := fakeStudyShadowAdapter(func(
		context.Context,
		StudySchedulingSnapshot,
		StudyPlan,
	) (StudyShadowEvaluation, error) {
		return StudyShadowEvaluation{
			OrderedCardIdentities: adapterOrder,
			Payload:               adapterPayload,
		}, nil
	})
	report := mustStudyShadowRunner(t, adapter, time.Second).Run(context.Background(), snapshot, authoritativePlan)
	if report.Status != StudyShadowStatusCompleted || report.Evaluation == nil {
		t.Fatalf("completed report = %#v", report)
	}

	adapterOrder[0] = studyShadowTestUUIDThree
	adapterPayload[2] = 'X'
	if !reflect.DeepEqual(report.Evaluation.OrderedCardIdentities, []uuid.UUID{studyShadowTestUUIDOne, studyShadowTestUUIDTwo}) ||
		string(report.Evaluation.Payload) != `{"score":1}` {
		t.Fatalf("adapter mutation changed returned report = %#v", report.Evaluation)
	}
	report.Evaluation.OrderedCardIdentities[0] = studyShadowTestUUIDThree
	report.Evaluation.Payload[2] = 'Y'
	assertStudyShadowCallerValuesUnchanged(t, snapshot, snapshotBefore, authoritativePlan, planBefore)
}

func TestStudyShadowRunnerFinalValidationUsesFrozenAuthoritativePlan(t *testing.T) {
	snapshot, authoritativePlan := studyShadowRunnerFixture()
	started := make(chan struct{})
	release := make(chan struct{})
	adapter := fakeStudyShadowAdapter(func(
		context.Context,
		StudySchedulingSnapshot,
		StudyPlan,
	) (StudyShadowEvaluation, error) {
		close(started)
		<-release
		return StudyShadowEvaluation{
			OrderedCardIdentities: []uuid.UUID{studyShadowTestUUIDOne, studyShadowTestUUIDTwo},
			Payload:               json.RawMessage(`{"frozen":true}`),
		}, nil
	})
	runner := mustStudyShadowRunner(t, adapter, time.Second)
	reportChannel := make(chan StudyShadowReport, 1)
	go func() {
		reportChannel <- runner.Run(context.Background(), snapshot, authoritativePlan)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("shadow adapter did not start")
	}
	// The adapter start proves Run completed both private copies. Mutating the
	// caller's retained aggregate must not influence final output validation.
	authoritativePlan.Cards[0].CardIdentity = studyShadowTestUUIDThree
	*authoritativePlan.Cards[0].SelectionReason.LatestPresentedIngestionOrdinal = 999
	authoritativePlan.Scheduler.Seed = "caller-mutated-after-copy"
	close(release)
	select {
	case report := <-reportChannel:
		if report.Status != StudyShadowStatusCompleted || report.Evaluation == nil {
			t.Fatalf("frozen-authority report = %#v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("shadow runner did not return")
	}
}

func TestStudyShadowRunnerIsImmediatelyReusableAfterCompletionAndPanic(t *testing.T) {
	t.Parallel()

	snapshot, authoritativePlan := studyShadowRunnerFixture()
	testCases := []struct {
		name       string
		adapter    fakeStudyShadowAdapter
		wantStatus StudyShadowStatus
	}{
		{
			name: "completion",
			adapter: func(context.Context, StudySchedulingSnapshot, StudyPlan) (StudyShadowEvaluation, error) {
				return StudyShadowEvaluation{
					OrderedCardIdentities: []uuid.UUID{studyShadowTestUUIDOne, studyShadowTestUUIDTwo},
				}, nil
			},
			wantStatus: StudyShadowStatusCompleted,
		},
		{
			name: "panic",
			adapter: func(context.Context, StudySchedulingSnapshot, StudyPlan) (StudyShadowEvaluation, error) {
				panic("private-reuse-panic")
			},
			wantStatus: StudyShadowStatusPanic,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			runner := mustStudyShadowRunner(t, testCase.adapter, time.Second)
			first := runner.Run(context.Background(), snapshot, authoritativePlan)
			second := runner.Run(context.Background(), snapshot, authoritativePlan)
			if first.Status != testCase.wantStatus || second.Status != testCase.wantStatus {
				t.Fatalf("sequential statuses = %q, %q; want %q", first.Status, second.Status, testCase.wantStatus)
			}
		})
	}
}

func TestStudyShadowRunnerAdapterErrorIsSanitizedAndNonAuthoritative(t *testing.T) {
	t.Parallel()

	snapshot, authoritativePlan := studyShadowRunnerFixture()
	snapshotBefore := cloneStudyShadowTestValue(t, snapshot)
	planBefore := cloneStudyShadowTestValue(t, authoritativePlan)
	canary := "private-error-" + snapshot.Seed + "-" + studyShadowTestUUIDOne.String()
	adapter := fakeStudyShadowAdapter(func(
		context.Context,
		StudySchedulingSnapshot,
		StudyPlan,
	) (StudyShadowEvaluation, error) {
		return StudyShadowEvaluation{}, errors.New(canary)
	})
	report := mustStudyShadowRunner(t, adapter, time.Second).Run(context.Background(), snapshot, authoritativePlan)
	if report.Status != StudyShadowStatusFailed || report.Failure == nil ||
		report.Failure.Code != StudyShadowFailureCodeAdapterFailed || report.Evaluation != nil {
		t.Fatalf("adapter-error report = %#v", report)
	}
	serialized, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	if strings.Contains(string(serialized), canary) || strings.Contains(string(serialized), snapshot.Seed) || strings.Contains(string(serialized), studyShadowTestUUIDOne.String()) {
		t.Fatalf("failure report exposed private adapter input: %s", serialized)
	}
	assertStudyShadowCallerValuesUnchanged(t, snapshot, snapshotBefore, authoritativePlan, planBefore)
}

func TestStudyShadowRunnerAdapterPanicIsSanitizedAndNonAuthoritative(t *testing.T) {
	t.Parallel()

	snapshot, authoritativePlan := studyShadowRunnerFixture()
	snapshotBefore := cloneStudyShadowTestValue(t, snapshot)
	planBefore := cloneStudyShadowTestValue(t, authoritativePlan)
	canary := "private-panic-" + snapshot.Seed + "-" + studyShadowTestUUIDOne.String()
	adapter := fakeStudyShadowAdapter(func(
		context.Context,
		StudySchedulingSnapshot,
		StudyPlan,
	) (StudyShadowEvaluation, error) {
		panic(canary)
	})
	report := mustStudyShadowRunner(t, adapter, time.Second).Run(context.Background(), snapshot, authoritativePlan)
	if report.Status != StudyShadowStatusPanic || report.Failure == nil ||
		report.Failure.Code != StudyShadowFailureCodeAdapterPanic || report.Evaluation != nil {
		t.Fatalf("panic report = %#v", report)
	}
	serialized, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	if strings.Contains(string(serialized), canary) || strings.Contains(string(serialized), snapshot.Seed) || strings.Contains(string(serialized), studyShadowTestUUIDOne.String()) {
		t.Fatalf("panic report exposed private adapter input: %s", serialized)
	}
	assertStudyShadowCallerValuesUnchanged(t, snapshot, snapshotBefore, authoritativePlan, planBefore)
}

func TestStudyShadowRunnerTimeoutLeavesLateMutationOnPrivateCopies(t *testing.T) {
	snapshot, authoritativePlan := studyShadowRunnerFixture()
	snapshotBefore := cloneStudyShadowTestValue(t, snapshot)
	planBefore := cloneStudyShadowTestValue(t, authoritativePlan)
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var invocationCount atomic.Int32
	adapter := fakeStudyShadowAdapter(func(
		_ context.Context,
		privateSnapshot StudySchedulingSnapshot,
		privatePlan StudyPlan,
	) (StudyShadowEvaluation, error) {
		invocation := invocationCount.Add(1)
		if invocation > 1 {
			return StudyShadowEvaluation{
				OrderedCardIdentities: []uuid.UUID{studyShadowTestUUIDOne, studyShadowTestUUIDTwo},
				Payload:               json.RawMessage(`{"reused":true}`),
			}, nil
		}
		close(started)
		<-release
		*privateSnapshot.PredecessorTailCardIdentity = studyShadowTestUUIDThree
		*privateSnapshot.Cards[0].Projection.LatestEffectiveRating = model.StudyRatingEasy
		*privatePlan.Cards[0].SelectionReason.LatestPresentedIngestionOrdinal = 777
		privatePlan.Cards[0].CardIdentity = studyShadowTestUUIDThree
		close(finished)
		return StudyShadowEvaluation{
			OrderedCardIdentities: []uuid.UUID{studyShadowTestUUIDOne, studyShadowTestUUIDTwo},
			Payload:               json.RawMessage(`{"late":true}`),
		}, nil
	})
	runner := mustStudyShadowRunner(t, adapter, 25*time.Millisecond)
	reportChannel := make(chan StudyShadowReport, 1)
	go func() {
		reportChannel <- runner.Run(context.Background(), snapshot, authoritativePlan)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("shadow adapter did not start")
	}
	var report StudyShadowReport
	select {
	case report = <-reportChannel:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("shadow runner did not time out")
	}
	if report.Status != StudyShadowStatusTimedOut || report.Failure == nil ||
		report.Failure.Code != StudyShadowFailureCodeAdapterTimedOut || report.Evaluation != nil {
		close(release)
		t.Fatalf("timeout report = %#v", report)
	}
	assertStudyShadowCallerValuesUnchanged(t, snapshot, snapshotBefore, authoritativePlan, planBefore)
	busyReport := runner.Run(context.Background(), snapshot, authoritativePlan)
	if busyReport.Status != StudyShadowStatusFailed || busyReport.Failure == nil ||
		busyReport.Failure.Code != StudyShadowFailureCodeAdapterBusy || invocationCount.Load() != 1 {
		close(release)
		t.Fatalf("in-flight adapter was not bounded to one execution: report %#v count %d", busyReport, invocationCount.Load())
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("late adapter did not finish")
	}
	select {
	case token := <-runner.executionSlot:
		runner.executionSlot <- token
	case <-time.After(time.Second):
		t.Fatal("late adapter exit did not release its execution slot")
	}
	reuseReport := runner.Run(context.Background(), snapshot, authoritativePlan)
	if reuseReport.Status != StudyShadowStatusCompleted || invocationCount.Load() != 2 {
		t.Fatalf("runner was not reusable after late adapter exit: %#v", reuseReport)
	}
	assertStudyShadowCallerValuesUnchanged(t, snapshot, snapshotBefore, authoritativePlan, planBefore)
}

func TestStudyShadowRunnerConfigurationBoundsFailClosed(t *testing.T) {
	t.Parallel()

	adapter := fakeStudyShadowAdapter(func(
		context.Context,
		StudySchedulingSnapshot,
		StudyPlan,
	) (StudyShadowEvaluation, error) {
		return StudyShadowEvaluation{}, nil
	})
	testCases := []struct {
		name           string
		adapter        StudyShadowAdapter
		adapterName    string
		adapterVersion string
		timeout        time.Duration
	}{
		{name: "nil with identity", adapterName: "configured", timeout: time.Second},
		{name: "name too long", adapter: adapter, adapterName: strings.Repeat("n", StudyMaximumShadowAdapterNameRunes+1), adapterVersion: "1", timeout: time.Second},
		{name: "version too long", adapter: adapter, adapterName: "fixture", adapterVersion: strings.Repeat("v", StudyMaximumShadowAdapterVersionRunes+1), timeout: time.Second},
		{name: "zero timeout", adapter: adapter, adapterName: "fixture", adapterVersion: "1"},
		{name: "timeout too long", adapter: adapter, adapterName: "fixture", adapterVersion: "1", timeout: StudyMaximumShadowTimeout + time.Nanosecond},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewStudyShadowRunner(testCase.adapter, testCase.adapterName, testCase.adapterVersion, testCase.timeout)
			var studyError *model.StudyError
			if !errors.As(err, &studyError) || studyError.Code != model.StudyErrorCodeSchemaInvalid ||
				studyError.Context["reason"] != "invalid_configuration" {
				t.Fatalf("configuration error = %#v", err)
			}
		})
	}
}

func TestStudyShadowRunnerRejectsInconsistentAuthorityBeforeAdapterInvocation(t *testing.T) {
	t.Parallel()

	var invocationCount atomic.Int32
	adapter := fakeStudyShadowAdapter(func(
		context.Context,
		StudySchedulingSnapshot,
		StudyPlan,
	) (StudyShadowEvaluation, error) {
		invocationCount.Add(1)
		return StudyShadowEvaluation{}, nil
	})
	runner := mustStudyShadowRunner(t, adapter, time.Second)
	testCases := []struct {
		name   string
		mutate func(*StudySchedulingSnapshot, *StudyPlan)
	}{
		{name: "identity count", mutate: func(_ *StudySchedulingSnapshot, plan *StudyPlan) { plan.Cards = plan.Cards[:1] }},
		{name: "identity set", mutate: func(_ *StudySchedulingSnapshot, plan *StudyPlan) {
			plan.Cards[1].CardIdentity = studyShadowTestUUIDThree
		}},
		{name: "snapshot duplicate", mutate: func(snapshot *StudySchedulingSnapshot, _ *StudyPlan) {
			snapshot.Cards[1].CardIdentity = snapshot.Cards[0].CardIdentity
		}},
		{name: "corpus provenance", mutate: func(_ *StudySchedulingSnapshot, plan *StudyPlan) { plan.CorpusSHA256 = strings.Repeat("e", 64) }},
		{name: "high-water provenance", mutate: func(_ *StudySchedulingSnapshot, plan *StudyPlan) { plan.Scheduler.InputProfileIngestionHighWater++ }},
		{name: "time provenance", mutate: func(_ *StudySchedulingSnapshot, plan *StudyPlan) {
			plan.Scheduler.PlannedAt = plan.Scheduler.PlannedAt.Add(time.Nanosecond)
		}},
		{name: "seed provenance", mutate: func(_ *StudySchedulingSnapshot, plan *StudyPlan) { plan.Scheduler.Seed = "different-private-seed" }},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			snapshot, authoritativePlan := studyShadowRunnerFixture()
			testCase.mutate(&snapshot, &authoritativePlan)
			report := runner.Run(context.Background(), snapshot, authoritativePlan)
			if report.Status != StudyShadowStatusFailed || report.Failure == nil ||
				report.Failure.Code != StudyShadowFailureCodeInputInvalid {
				t.Fatalf("inconsistent authority report = %#v", report)
			}
		})
	}
	if invocationCount.Load() != 0 {
		t.Fatalf("adapter invoked %d times for invalid authority", invocationCount.Load())
	}
}

func TestStudyShadowRunnerRejectsOverBoundInputBeforeAdapterInvocation(t *testing.T) {
	var invocationCount atomic.Int32
	adapter := fakeStudyShadowAdapter(func(
		context.Context,
		StudySchedulingSnapshot,
		StudyPlan,
	) (StudyShadowEvaluation, error) {
		invocationCount.Add(1)
		return StudyShadowEvaluation{}, nil
	})
	runner := mustStudyShadowRunner(t, adapter, time.Second)
	snapshot, authoritativePlan := studyShadowRunnerFixture()
	snapshot.Cards = make([]StudySchedulingCard, StudyMaximumSchedulingCards+1)
	report := runner.Run(context.Background(), snapshot, authoritativePlan)
	if report.Status != StudyShadowStatusFailed || report.Failure == nil ||
		report.Failure.Code != StudyShadowFailureCodeInputInvalid ||
		report.Failure.Context["reason"] != "input_work_bound_exceeded" {
		t.Fatalf("over-bound input report = %#v", report)
	}
	if invocationCount.Load() != 0 {
		t.Fatalf("adapter invoked %d times for over-bound input", invocationCount.Load())
	}
}

func TestStudyShadowRunnerRejectsUnknownAndDuplicateSameCountOrders(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		order []uuid.UUID
	}{
		{name: "unknown", order: []uuid.UUID{studyShadowTestUUIDOne, studyShadowTestUUIDThree}},
		{name: "duplicate", order: []uuid.UUID{studyShadowTestUUIDOne, studyShadowTestUUIDOne}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			snapshot, authoritativePlan := studyShadowRunnerFixture()
			adapter := fakeStudyShadowAdapter(func(
				context.Context,
				StudySchedulingSnapshot,
				StudyPlan,
			) (StudyShadowEvaluation, error) {
				return StudyShadowEvaluation{
					OrderedCardIdentities: testCase.order,
					Payload:               json.RawMessage(`{"comparison":"invalid"}`),
				}, nil
			})
			report := mustStudyShadowRunner(t, adapter, time.Second).Run(context.Background(), snapshot, authoritativePlan)
			if report.Status != StudyShadowStatusFailed || report.Failure == nil ||
				report.Failure.Code != StudyShadowFailureCodeEvaluationInvalid || report.Evaluation != nil {
				t.Fatalf("invalid evaluation report = %#v", report)
			}
		})
	}
}

func TestStudyShadowEvaluationPayloadAndOrderBounds(t *testing.T) {
	t.Parallel()

	atBoundPayload := json.RawMessage(`"` + strings.Repeat("x", StudyMaximumShadowEvaluationPayloadBytes-2) + `"`)
	cloned, failure := validateAndCloneStudyShadowEvaluation(
		StudyShadowEvaluation{Payload: atBoundPayload},
		StudyPlan{},
	)
	if failure != nil || len(cloned.Payload) != StudyMaximumShadowEvaluationPayloadBytes {
		t.Fatalf("exact-bound payload result = %d, failure %#v", len(cloned.Payload), failure)
	}

	overBoundPayload := json.RawMessage(`"` + strings.Repeat("x", StudyMaximumShadowEvaluationPayloadBytes-1) + `"`)
	_, failure = validateAndCloneStudyShadowEvaluation(
		StudyShadowEvaluation{Payload: overBoundPayload},
		StudyPlan{},
	)
	if failure == nil || failure.Code != StudyShadowFailureCodeEvaluationInvalid ||
		failure.Context["reason"] != "evaluation_payload_work_bound_exceeded" {
		t.Fatalf("over-bound payload failure = %#v", failure)
	}

	overBoundOrder := make([]uuid.UUID, StudyMaximumSchedulingCards+1)
	_, failure = validateAndCloneStudyShadowEvaluation(
		StudyShadowEvaluation{OrderedCardIdentities: overBoundOrder},
		StudyPlan{},
	)
	if failure == nil || failure.Code != StudyShadowFailureCodeEvaluationInvalid ||
		failure.Context["reason"] != "evaluation_order_work_bound_exceeded" {
		t.Fatalf("over-bound order failure = %#v", failure)
	}
}

func TestStudyShadowStatusVocabularyIsExact(t *testing.T) {
	t.Parallel()

	for _, status := range []StudyShadowStatus{
		StudyShadowStatusNotConfigured,
		StudyShadowStatusCompleted,
		StudyShadowStatusFailed,
		StudyShadowStatusTimedOut,
		StudyShadowStatusPanic,
	} {
		if !status.Valid() {
			t.Fatalf("status %q is not valid", status)
		}
	}
	if StudyShadowStatus("unknown").Valid() {
		t.Fatal("unknown shadow status is valid")
	}
}

var (
	studyShadowTestProfileID = uuid.MustParse("a0000000-0000-4000-8000-000000000001")
	studyShadowTestUUIDOne   = uuid.MustParse("a0000000-0000-4000-8000-000000000011")
	studyShadowTestUUIDTwo   = uuid.MustParse("a0000000-0000-4000-8000-000000000012")
	studyShadowTestUUIDThree = uuid.MustParse("a0000000-0000-4000-8000-000000000013")
)

func studyShadowRunnerFixture() (StudySchedulingSnapshot, StudyPlan) {
	rating := model.StudyRatingHard
	firstOrdinal := int64(4)
	secondOrdinal := int64(9)
	predecessorTail := studyShadowTestUUIDTwo
	snapshot := StudySchedulingSnapshot{
		LearnerProfileID:               studyShadowTestProfileID,
		CardKind:                       model.StudyCardKindIdea,
		CorpusSHA256:                   strings.Repeat("a", 64),
		InputProfileIngestionHighWater: 12,
		PlannedAt:                      time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC),
		Seed:                           "private-shadow-seed",
		Cards: []StudySchedulingCard{
			{
				CardIdentity: studyShadowTestUUIDOne,
				Projection: StudyProjectionTail{
					LatestEffectiveRating:           &rating,
					LatestPresentedIngestionOrdinal: &firstOrdinal,
				},
			},
			{
				CardIdentity: studyShadowTestUUIDTwo,
				Projection: StudyProjectionTail{
					LatestPresentedIngestionOrdinal: &secondOrdinal,
				},
			},
		},
		PredecessorTailCardIdentity: &predecessorTail,
	}
	planFirstOrdinal := int64(4)
	planSecondOrdinal := int64(9)
	plan := StudyPlan{
		Scheduler: model.StudySchedulerProvenance{
			Name:                           VarietyFirstV1Name,
			Version:                        VarietyFirstV1Version,
			ConfigurationSHA256:            strings.Repeat("b", 64),
			InputProfileIngestionHighWater: 12,
			PlannedAt:                      snapshot.PlannedAt,
			Seed:                           snapshot.Seed,
		},
		CorpusSHA256: snapshot.CorpusSHA256,
		Cards: []StudyPlannedCard{
			{
				CardIdentity: studyShadowTestUUIDOne,
				SelectionReason: model.StudySelectionReason{
					RatingRank:                      model.StudyRatingRankHard,
					LatestPresentedIngestionOrdinal: &planFirstOrdinal,
					TieBreakerSHA256:                strings.Repeat("c", 64),
					BoundaryAdjustment:              model.StudyBoundaryAdjustmentNone,
				},
			},
			{
				CardIdentity: studyShadowTestUUIDTwo,
				SelectionReason: model.StudySelectionReason{
					RatingRank:                      model.StudyRatingRankUnseenUnrated,
					LatestPresentedIngestionOrdinal: &planSecondOrdinal,
					TieBreakerSHA256:                strings.Repeat("d", 64),
					BoundaryAdjustment:              model.StudyBoundaryAdjustmentNone,
				},
			},
		},
	}
	return snapshot, plan
}

func mustStudyShadowRunner(t *testing.T, adapter StudyShadowAdapter, timeout time.Duration) *StudyShadowRunner {
	t.Helper()
	runner, err := NewStudyShadowRunner(adapter, "fake_shadow_v1", "1", timeout)
	if err != nil {
		t.Fatalf("construct shadow runner: %v", err)
	}
	return runner
}

func cloneStudyShadowTestValue[T any](t *testing.T, value T) T {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode test value: %v", err)
	}
	var clone T
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatalf("decode test value: %v", err)
	}
	return clone
}

func assertStudyShadowCallerValuesUnchanged(
	t *testing.T,
	snapshot StudySchedulingSnapshot,
	wantSnapshot StudySchedulingSnapshot,
	authoritativePlan StudyPlan,
	wantPlan StudyPlan,
) {
	t.Helper()
	if !reflect.DeepEqual(snapshot, wantSnapshot) {
		t.Fatal("shadow adapter changed caller snapshot")
	}
	if !reflect.DeepEqual(authoritativePlan, wantPlan) {
		t.Fatal("shadow adapter changed authoritative plan")
	}
}
