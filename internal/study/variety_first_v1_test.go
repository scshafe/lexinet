package study

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

func TestVarietyFirstV1ExactRatingAndPresentedOrder(t *testing.T) {
	t.Parallel()

	scheduler := mustVarietyFirstV1(t)
	again := model.StudyRatingAgain
	hard := model.StudyRatingHard
	good := model.StudyRatingGood
	easy := model.StudyRatingEasy
	ordinal1, ordinal2, ordinal3, ordinal4 := int64(1), int64(2), int64(3), int64(4)
	cards := []StudySchedulingCard{
		{CardIdentity: schedulerTestUUID(9), Projection: StudyProjectionTail{LatestEffectiveRating: &easy}},
		{CardIdentity: schedulerTestUUID(8), Projection: StudyProjectionTail{LatestEffectiveRating: &good, LatestPresentedIngestionOrdinal: &ordinal4}},
		{CardIdentity: schedulerTestUUID(7)},
		{CardIdentity: schedulerTestUUID(6), Projection: StudyProjectionTail{LatestEffectiveRating: &hard, LatestPresentedIngestionOrdinal: &ordinal3}},
		{CardIdentity: schedulerTestUUID(5), Projection: StudyProjectionTail{LatestEffectiveRating: &again, LatestPresentedIngestionOrdinal: &ordinal2}},
		{CardIdentity: schedulerTestUUID(4), Projection: StudyProjectionTail{LatestEffectiveRating: &hard}},
		{CardIdentity: schedulerTestUUID(3), Projection: StudyProjectionTail{LatestEffectiveRating: &again, LatestPresentedIngestionOrdinal: &ordinal1}},
	}
	plan, err := scheduler.Plan(testSchedulingSnapshot(cards...))
	if err != nil {
		t.Fatalf("plan ranked snapshot: %v", err)
	}
	want := []uuid.UUID{
		schedulerTestUUID(3),
		schedulerTestUUID(5),
		schedulerTestUUID(4),
		schedulerTestUUID(6),
		schedulerTestUUID(7),
		schedulerTestUUID(8),
		schedulerTestUUID(9),
	}
	if got := plannedCardIdentities(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("ranked order = %v, want %v", got, want)
	}
	wantRanks := []model.StudyRatingRank{
		model.StudyRatingRankAgain,
		model.StudyRatingRankAgain,
		model.StudyRatingRankHard,
		model.StudyRatingRankHard,
		model.StudyRatingRankUnseenUnrated,
		model.StudyRatingRankGood,
		model.StudyRatingRankEasy,
	}
	for index := range plan.Cards {
		if plan.Cards[index].SelectionReason.RatingRank != wantRanks[index] {
			t.Fatalf("rank at %d = %q, want %q", index, plan.Cards[index].SelectionReason.RatingRank, wantRanks[index])
		}
	}
}

func TestVarietyFirstV1SeedAffectsOnlyNeverPresentedTies(t *testing.T) {
	t.Parallel()

	scheduler := mustVarietyFirstV1(t)
	hard := model.StudyRatingHard
	firstOrdinal, secondOrdinal := int64(10), int64(20)
	snapshot := testSchedulingSnapshot(
		StudySchedulingCard{CardIdentity: schedulerTestUUID(1), Projection: StudyProjectionTail{LatestEffectiveRating: &hard, LatestPresentedIngestionOrdinal: &secondOrdinal}},
		StudySchedulingCard{CardIdentity: schedulerTestUUID(2), Projection: StudyProjectionTail{LatestEffectiveRating: &hard, LatestPresentedIngestionOrdinal: &firstOrdinal}},
		StudySchedulingCard{CardIdentity: schedulerTestUUID(3), Projection: StudyProjectionTail{LatestEffectiveRating: &hard}},
		StudySchedulingCard{CardIdentity: schedulerTestUUID(4), Projection: StudyProjectionTail{LatestEffectiveRating: &hard}},
	)
	first, err := scheduler.Plan(snapshot)
	if err != nil {
		t.Fatalf("plan first seed: %v", err)
	}
	snapshot.Seed = "different-fixed-seed"
	second, err := scheduler.Plan(snapshot)
	if err != nil {
		t.Fatalf("plan second seed: %v", err)
	}

	for _, identity := range []uuid.UUID{schedulerTestUUID(2), schedulerTestUUID(1)} {
		if indexOfPlannedCard(first, identity) != indexOfPlannedCard(second, identity) {
			t.Fatalf("seed moved a real-ordinal card")
		}
	}
	if indexOfPlannedCard(first, schedulerTestUUID(2)) >= indexOfPlannedCard(first, schedulerTestUUID(1)) ||
		indexOfPlannedCard(second, schedulerTestUUID(2)) >= indexOfPlannedCard(second, schedulerTestUUID(1)) {
		t.Fatal("seed overrode real presented ordinal order")
	}
	firstNilOrder := []uuid.UUID{first.Cards[0].CardIdentity, first.Cards[1].CardIdentity}
	secondNilOrder := []uuid.UUID{second.Cards[0].CardIdentity, second.Cards[1].CardIdentity}
	if !reflect.DeepEqual(firstNilOrder, []uuid.UUID{schedulerTestUUID(3), schedulerTestUUID(4)}) ||
		!reflect.DeepEqual(secondNilOrder, []uuid.UUID{schedulerTestUUID(4), schedulerTestUUID(3)}) {
		t.Fatalf("known seeds did not reverse the nil/nil subgroup: first %v second %v", firstNilOrder, secondNilOrder)
	}
}

func TestVarietyFirstV1DuplicatePresentedOrdinalIsCorruption(t *testing.T) {
	t.Parallel()

	scheduler := mustVarietyFirstV1(t)
	ordinalOne, ordinalTwo := int64(17), int64(17)
	_, err := scheduler.Plan(testSchedulingSnapshot(
		StudySchedulingCard{CardIdentity: schedulerTestUUID(1), Projection: StudyProjectionTail{LatestPresentedIngestionOrdinal: &ordinalOne}},
		StudySchedulingCard{CardIdentity: schedulerTestUUID(2), Projection: StudyProjectionTail{LatestPresentedIngestionOrdinal: &ordinalTwo}},
	))
	var studyError *model.StudyError
	if !errors.As(err, &studyError) || studyError.Code != model.StudyErrorCodeInternal || studyError.Context["reason"] != "projection_corrupt" {
		t.Fatalf("duplicate ordinal error = %#v", err)
	}
}

func TestVarietyFirstV1BoundaryRotationAndPriorityProvenance(t *testing.T) {
	t.Parallel()

	scheduler := mustVarietyFirstV1(t)
	again := model.StudyRatingAgain
	hard := model.StudyRatingHard
	one, two, three, four := int64(1), int64(2), int64(3), int64(4)
	tail := schedulerTestUUID(1)
	snapshot := testSchedulingSnapshot(
		StudySchedulingCard{CardIdentity: tail, Projection: StudyProjectionTail{LatestEffectiveRating: &again, LatestPresentedIngestionOrdinal: &one}},
		StudySchedulingCard{CardIdentity: schedulerTestUUID(2), Projection: StudyProjectionTail{LatestEffectiveRating: &again, LatestPresentedIngestionOrdinal: &two}},
		StudySchedulingCard{CardIdentity: schedulerTestUUID(3), Projection: StudyProjectionTail{LatestEffectiveRating: &again, LatestPresentedIngestionOrdinal: &three}},
		StudySchedulingCard{CardIdentity: schedulerTestUUID(4), Projection: StudyProjectionTail{LatestEffectiveRating: &hard, LatestPresentedIngestionOrdinal: &four}},
	)
	snapshot.PredecessorTailCardIdentity = &tail
	plan, err := scheduler.Plan(snapshot)
	if err != nil {
		t.Fatalf("plan rotatable boundary: %v", err)
	}
	want := []uuid.UUID{schedulerTestUUID(2), schedulerTestUUID(3), tail, schedulerTestUUID(4)}
	if got := plannedCardIdentities(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("rotated order = %v, want %v", got, want)
	}
	for index, card := range plan.Cards {
		wantAdjustment := model.StudyBoundaryAdjustmentNone
		if index == 0 {
			wantAdjustment = model.StudyBoundaryAdjustmentRotated
		}
		if card.SelectionReason.BoundaryAdjustment != wantAdjustment {
			t.Fatalf("adjustment at %d = %q, want %q", index, card.SelectionReason.BoundaryAdjustment, wantAdjustment)
		}
	}

	prioritySnapshot := testSchedulingSnapshot(
		StudySchedulingCard{CardIdentity: tail, Projection: StudyProjectionTail{LatestEffectiveRating: &again, LatestPresentedIngestionOrdinal: &one}},
		StudySchedulingCard{CardIdentity: schedulerTestUUID(4), Projection: StudyProjectionTail{LatestEffectiveRating: &hard, LatestPresentedIngestionOrdinal: &four}},
	)
	prioritySnapshot.PredecessorTailCardIdentity = &tail
	priorityPlan, err := scheduler.Plan(prioritySnapshot)
	if err != nil {
		t.Fatalf("plan priority-bound boundary: %v", err)
	}
	if priorityPlan.Cards[0].CardIdentity != tail || priorityPlan.Cards[0].SelectionReason.BoundaryAdjustment != model.StudyBoundaryAdjustmentPriorityBound {
		t.Fatalf("priority-bound head = %#v", priorityPlan.Cards[0])
	}
}

func TestVarietyFirstV1SingletonBoundaryIsPriorityBound(t *testing.T) {
	t.Parallel()

	scheduler := mustVarietyFirstV1(t)
	tail := schedulerTestUUID(1)
	snapshot := testSchedulingSnapshot(StudySchedulingCard{CardIdentity: tail})
	snapshot.PredecessorTailCardIdentity = &tail
	plan, err := scheduler.Plan(snapshot)
	if err != nil {
		t.Fatalf("plan singleton boundary: %v", err)
	}
	if len(plan.Cards) != 1 || plan.Cards[0].CardIdentity != tail ||
		plan.Cards[0].SelectionReason.BoundaryAdjustment != model.StudyBoundaryAdjustmentPriorityBound {
		t.Fatalf("singleton boundary plan = %#v", plan)
	}
}

func TestVarietyFirstV1CoverageForRepresentativeCorpusSizes(t *testing.T) {
	t.Parallel()

	scheduler := mustVarietyFirstV1(t)
	for _, size := range []int{0, 1, 2, 31} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			cards := make([]StudySchedulingCard, size)
			for index := range cards {
				cards[index].CardIdentity = schedulerTestUUID(index + 1)
			}
			plan, err := scheduler.Plan(testSchedulingSnapshot(cards...))
			if err != nil {
				t.Fatalf("plan corpus size %d: %v", size, err)
			}
			if len(plan.Cards) != size {
				t.Fatalf("planned %d cards, want %d", len(plan.Cards), size)
			}
			seen := make(map[uuid.UUID]struct{}, size)
			for _, card := range plan.Cards {
				if _, exists := seen[card.CardIdentity]; exists {
					t.Fatal("plan repeated a card")
				}
				seen[card.CardIdentity] = struct{}{}
			}
		})
	}
}

func TestVarietyComparatorUUIDFallbackOnlyAfterNilTieCollision(t *testing.T) {
	t.Parallel()

	lower := varietyScheduledCard{
		planned:   StudyPlannedCard{CardIdentity: schedulerTestUUID(1), SelectionReason: model.StudySelectionReason{TieBreakerSHA256: "same"}},
		rankOrder: 2,
	}
	higher := varietyScheduledCard{
		planned:   StudyPlannedCard{CardIdentity: schedulerTestUUID(2), SelectionReason: model.StudySelectionReason{TieBreakerSHA256: "same"}},
		rankOrder: 2,
	}
	if !varietyScheduledCardLess(lower, higher) || varietyScheduledCardLess(higher, lower) {
		t.Fatal("nil tie collision did not fall back to lowercase UUID")
	}

	ordinal := int64(7)
	lower.planned.SelectionReason.LatestPresentedIngestionOrdinal = &ordinal
	higher.planned.SelectionReason.LatestPresentedIngestionOrdinal = &ordinal
	lower.planned.SelectionReason.TieBreakerSHA256 = "a"
	higher.planned.SelectionReason.TieBreakerSHA256 = "z"
	if varietyScheduledCardLess(lower, higher) || varietyScheduledCardLess(higher, lower) {
		t.Fatal("real-ordinal comparison consulted seeded tie data")
	}
}

func schedulerTestUUID(value int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", value))
}

func indexOfPlannedCard(plan StudyPlan, identity uuid.UUID) int {
	for index := range plan.Cards {
		if plan.Cards[index].CardIdentity == identity {
			return index
		}
	}
	return -1
}
