package study

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

func TestShuffleBagV1IsDeterministicAndRatingIndependent(t *testing.T) {
	t.Parallel()

	scheduler, err := NewShuffleBagV1()
	if err != nil {
		t.Fatalf("construct shuffle-bag scheduler: %v", err)
	}
	again := model.StudyRatingAgain
	easy := model.StudyRatingEasy
	one, two := int64(1), int64(2)
	firstSnapshot := testSchedulingSnapshot(
		StudySchedulingCard{CardIdentity: schedulerTestUUID(1), Projection: StudyProjectionTail{LatestEffectiveRating: &again, LatestPresentedIngestionOrdinal: &one}},
		StudySchedulingCard{CardIdentity: schedulerTestUUID(2), Projection: StudyProjectionTail{LatestEffectiveRating: &easy, LatestPresentedIngestionOrdinal: &two}},
		StudySchedulingCard{CardIdentity: schedulerTestUUID(3)},
		StudySchedulingCard{CardIdentity: schedulerTestUUID(4)},
	)
	first, err := scheduler.Plan(firstSnapshot)
	if err != nil {
		t.Fatalf("plan first comparison bag: %v", err)
	}

	secondSnapshot := testSchedulingSnapshot(
		StudySchedulingCard{CardIdentity: schedulerTestUUID(4), Projection: StudyProjectionTail{LatestEffectiveRating: &again, LatestPresentedIngestionOrdinal: &two}},
		StudySchedulingCard{CardIdentity: schedulerTestUUID(2)},
		StudySchedulingCard{CardIdentity: schedulerTestUUID(1), Projection: StudyProjectionTail{LatestEffectiveRating: &easy}},
		StudySchedulingCard{CardIdentity: schedulerTestUUID(3), Projection: StudyProjectionTail{LatestEffectiveRating: &again, LatestPresentedIngestionOrdinal: &one}},
	)
	second, err := scheduler.Plan(secondSnapshot)
	if err != nil {
		t.Fatalf("plan second comparison bag: %v", err)
	}
	if got, want := plannedCardIdentities(second), plannedCardIdentities(first); !reflect.DeepEqual(got, want) {
		t.Fatalf("ratings or input order changed shuffle bag: got %v want %v", got, want)
	}
	if first.Scheduler.Name != ShuffleBagV1Name || first.Scheduler.Version != ShuffleBagV1Version {
		t.Fatalf("shuffle provenance identity = %#v", first.Scheduler)
	}
	if first.Scheduler.ConfigurationSHA256 != "739c550c3ed59fb69e38189d7effe8db4f11e8e1d6ae0566cd0a46946ca235c6" {
		t.Fatalf("shuffle configuration digest = %q", first.Scheduler.ConfigurationSHA256)
	}
	assertExactlyOnce(t, first, []uuid.UUID{
		schedulerTestUUID(1),
		schedulerTestUUID(2),
		schedulerTestUUID(3),
		schedulerTestUUID(4),
	})
}

func TestUnconstructedSchedulersFailClosed(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		scheduler StudyScheduler
	}{
		{name: "nil variety", scheduler: (*VarietyFirstV1)(nil)},
		{name: "zero variety", scheduler: &VarietyFirstV1{}},
		{name: "nil shuffle", scheduler: (*ShuffleBagV1)(nil)},
		{name: "zero shuffle", scheduler: &ShuffleBagV1{}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := testCase.scheduler.Plan(testSchedulingSnapshot())
			studyError, ok := err.(*model.StudyError)
			if !ok || studyError.Code != model.StudyErrorCodeInternal || studyError.Context["reason"] != "invalid_configuration" {
				t.Fatalf("unconstructed scheduler error = %#v", err)
			}
		})
	}
}

func assertExactlyOnce(t *testing.T, plan StudyPlan, expected []uuid.UUID) {
	t.Helper()
	if len(plan.Cards) != len(expected) {
		t.Fatalf("planned %d cards, want %d", len(plan.Cards), len(expected))
	}
	want := make(map[uuid.UUID]struct{}, len(expected))
	for _, identity := range expected {
		want[identity] = struct{}{}
	}
	for _, card := range plan.Cards {
		if _, exists := want[card.CardIdentity]; !exists {
			t.Fatal("plan contained an unexpected or repeated identity")
		}
		delete(want, card.CardIdentity)
	}
	if len(want) != 0 {
		t.Fatal("plan omitted expected identities")
	}
}
