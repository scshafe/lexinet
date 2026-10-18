package study

import (
	"sort"

	"lexinet/internal/model"
)

// ShuffleBagV1 is a deterministic, rating-independent comparison fixture. It
// is not the active production scheduler.
type ShuffleBagV1 struct {
	identity schedulerIdentity
}

var _ StudyScheduler = (*ShuffleBagV1)(nil)

// NewShuffleBagV1 binds the exact comparison policy and provenance identity.
func NewShuffleBagV1() (*ShuffleBagV1, error) {
	identity, err := newSchedulerIdentity(
		ShuffleBagV1Name,
		ShuffleBagV1Version,
		[]byte("rating_independent_seeded_shuffle_v1"),
		[]byte("lowercase_uuid_fallback_v1"),
	)
	if err != nil {
		return nil, err
	}
	return &ShuffleBagV1{identity: identity}, nil
}

func (scheduler *ShuffleBagV1) Plan(snapshot StudySchedulingSnapshot) (StudyPlan, error) {
	const operation = "shuffle_bag_v1_plan"
	if scheduler == nil || scheduler.identity.name != ShuffleBagV1Name || scheduler.identity.version != ShuffleBagV1Version {
		return StudyPlan{}, newSchedulerError(model.StudyErrorCodeInternal, "shuffle-bag scheduler is not constructed", operation, "scheduler", "invalid_configuration", nil)
	}
	if err := validateStudySchedulingSnapshot(snapshot, operation); err != nil {
		return StudyPlan{}, err
	}

	planned := make([]StudyPlannedCard, len(snapshot.Cards))
	for index, card := range snapshot.Cards {
		tieSHA256, err := tieBreakerSHA256(scheduler.identity, snapshot.Seed, snapshot.CardKind, card.CardIdentity)
		if err != nil {
			return StudyPlan{}, err
		}
		planned[index] = StudyPlannedCard{
			CardIdentity: card.CardIdentity,
			SelectionReason: model.StudySelectionReason{
				RatingRank:                      ratingRank(card.Projection.LatestEffectiveRating),
				LatestPresentedIngestionOrdinal: cloneInt64(card.Projection.LatestPresentedIngestionOrdinal),
				TieBreakerSHA256:                tieSHA256,
				BoundaryAdjustment:              model.StudyBoundaryAdjustmentNone,
			},
		}
	}
	sort.Slice(planned, func(left, right int) bool {
		if planned[left].SelectionReason.TieBreakerSHA256 != planned[right].SelectionReason.TieBreakerSHA256 {
			return planned[left].SelectionReason.TieBreakerSHA256 < planned[right].SelectionReason.TieBreakerSHA256
		}
		return planned[left].CardIdentity.String() < planned[right].CardIdentity.String()
	})

	return StudyPlan{
		Scheduler:    scheduler.identity.provenance(snapshot),
		CorpusSHA256: snapshot.CorpusSHA256,
		Cards:        planned,
	}, nil
}
