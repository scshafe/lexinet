package study

import (
	"sort"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

// VarietyFirstV1 is the active, transparent rating-aware finite-cycle policy.
// Its zero value is intentionally unusable; construct it through
// NewVarietyFirstV1 so provenance is always bound.
type VarietyFirstV1 struct {
	identity schedulerIdentity
}

var _ StudyScheduler = (*VarietyFirstV1)(nil)

// NewVarietyFirstV1 binds the exact frozen v1 identity and comparison rules.
func NewVarietyFirstV1() (*VarietyFirstV1, error) {
	identity, err := newSchedulerIdentity(
		VarietyFirstV1Name,
		VarietyFirstV1Version,
		[]byte(model.StudyRatingRankAgain),
		[]byte(model.StudyRatingRankHard),
		[]byte(model.StudyRatingRankUnseenUnrated),
		[]byte(model.StudyRatingRankGood),
		[]byte(model.StudyRatingRankEasy),
		[]byte("presented_ordinal_asc_nulls_first"),
		[]byte("seeded_tie_v1"),
		[]byte("left_rotate_leading_rank_v1"),
	)
	if err != nil {
		return nil, err
	}
	return &VarietyFirstV1{identity: identity}, nil
}

func (scheduler *VarietyFirstV1) Plan(snapshot StudySchedulingSnapshot) (StudyPlan, error) {
	const operation = "variety_first_v1_plan"
	if scheduler == nil || scheduler.identity.name != VarietyFirstV1Name || scheduler.identity.version != VarietyFirstV1Version {
		return StudyPlan{}, newSchedulerError(model.StudyErrorCodeInternal, "variety-first scheduler is not constructed", operation, "scheduler", "invalid_configuration", nil)
	}
	if err := validateStudySchedulingSnapshot(snapshot, operation); err != nil {
		return StudyPlan{}, err
	}

	scheduled := make([]varietyScheduledCard, len(snapshot.Cards))
	for index, card := range snapshot.Cards {
		tieSHA256, err := tieBreakerSHA256(scheduler.identity, snapshot.Seed, snapshot.CardKind, card.CardIdentity)
		if err != nil {
			return StudyPlan{}, err
		}
		rank := ratingRank(card.Projection.LatestEffectiveRating)
		scheduled[index] = varietyScheduledCard{
			planned: StudyPlannedCard{
				CardIdentity: card.CardIdentity,
				SelectionReason: model.StudySelectionReason{
					RatingRank:                      rank,
					LatestPresentedIngestionOrdinal: cloneInt64(card.Projection.LatestPresentedIngestionOrdinal),
					TieBreakerSHA256:                tieSHA256,
					BoundaryAdjustment:              model.StudyBoundaryAdjustmentNone,
				},
			},
			rankOrder: varietyRankOrder(rank),
		}
	}

	sort.Slice(scheduled, func(left, right int) bool {
		return varietyScheduledCardLess(scheduled[left], scheduled[right])
	})
	applyVarietyBoundaryAdjustment(scheduled, snapshot.PredecessorTailCardIdentity)

	plan := StudyPlan{
		Scheduler:    scheduler.identity.provenance(snapshot),
		CorpusSHA256: snapshot.CorpusSHA256,
		Cards:        make([]StudyPlannedCard, len(scheduled)),
	}
	for index := range scheduled {
		plan.Cards[index] = scheduled[index].planned
	}
	return plan, nil
}

type varietyScheduledCard struct {
	planned   StudyPlannedCard
	rankOrder int
}

func varietyRankOrder(rank model.StudyRatingRank) int {
	switch rank {
	case model.StudyRatingRankAgain:
		return 0
	case model.StudyRatingRankHard:
		return 1
	case model.StudyRatingRankUnseenUnrated:
		return 2
	case model.StudyRatingRankGood:
		return 3
	case model.StudyRatingRankEasy:
		return 4
	default:
		return 5
	}
}

func varietyScheduledCardLess(left, right varietyScheduledCard) bool {
	if left.rankOrder != right.rankOrder {
		return left.rankOrder < right.rankOrder
	}
	leftOrdinal := left.planned.SelectionReason.LatestPresentedIngestionOrdinal
	rightOrdinal := right.planned.SelectionReason.LatestPresentedIngestionOrdinal
	switch {
	case leftOrdinal == nil && rightOrdinal != nil:
		return true
	case leftOrdinal != nil && rightOrdinal == nil:
		return false
	case leftOrdinal != nil && rightOrdinal != nil:
		return *leftOrdinal < *rightOrdinal
	}
	if left.planned.SelectionReason.TieBreakerSHA256 != right.planned.SelectionReason.TieBreakerSHA256 {
		return left.planned.SelectionReason.TieBreakerSHA256 < right.planned.SelectionReason.TieBreakerSHA256
	}
	return left.planned.CardIdentity.String() < right.planned.CardIdentity.String()
}

func applyVarietyBoundaryAdjustment(scheduled []varietyScheduledCard, predecessorTail *uuid.UUID) {
	if len(scheduled) == 0 || predecessorTail == nil || scheduled[0].planned.CardIdentity != *predecessorTail {
		return
	}
	leadingRankEnd := 1
	for leadingRankEnd < len(scheduled) && scheduled[leadingRankEnd].rankOrder == scheduled[0].rankOrder {
		leadingRankEnd++
	}
	if leadingRankEnd == 1 {
		scheduled[0].planned.SelectionReason.BoundaryAdjustment = model.StudyBoundaryAdjustmentPriorityBound
		return
	}
	first := scheduled[0]
	copy(scheduled[0:leadingRankEnd-1], scheduled[1:leadingRankEnd])
	scheduled[leadingRankEnd-1] = first
	scheduled[0].planned.SelectionReason.BoundaryAdjustment = model.StudyBoundaryAdjustmentRotated
}
