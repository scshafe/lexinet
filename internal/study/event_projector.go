package study

import (
	"github.com/google/uuid"

	"lexinet/internal/model"
)

type studyProjectionRatingEvidence struct {
	Event               model.StudyEvent
	CurrentCardIdentity uuid.UUID
	IngestionOrdinal    int64
	Voided              bool
}

type studyProjectionFoldState struct {
	LatestPresented *model.StudyEvent
	LatestRating    *studyProjectionRatingEvidence
	ratings         []*studyProjectionRatingEvidence
}

func foldResolvedAcceptedStudyEvents(
	profileID uuid.UUID,
	highWater int64,
	resolvedEvents []StudyResolvedAcceptedEvent,
) (map[uuid.UUID]*studyProjectionFoldState, error) {
	if int64(len(resolvedEvents)) != highWater {
		return nil, newStudyProjectionError(
			model.StudyErrorCodeInternal,
			"accepted study event prefix does not reach its declared high-water",
			"accepted_prefix_length_corrupt",
		)
	}

	states := make(map[uuid.UUID]*studyProjectionFoldState)
	ratingsByEventID := make(map[uuid.UUID]*studyProjectionRatingEvidence)
	seenEventIDs := make(map[uuid.UUID]struct{}, len(resolvedEvents))
	currentIdentityByHistoricalIdentity := make(map[uuid.UUID]uuid.UUID)

	for index, resolved := range resolvedEvents {
		event := resolved.Event
		if err := validateResolvedAcceptedStudyEvent(profileID, int64(index)+1, resolved); err != nil {
			return nil, err
		}
		if _, duplicate := seenEventIDs[event.ID]; duplicate {
			return nil, newStudyProjectionError(
				model.StudyErrorCodeInternal,
				"accepted study event prefix contains a duplicate event identity",
				"accepted_event_identity_duplicate",
			)
		}
		seenEventIDs[event.ID] = struct{}{}

		if priorIdentity, exists := currentIdentityByHistoricalIdentity[event.CardIdentity]; exists && priorIdentity != resolved.CurrentCardIdentity {
			return nil, newStudyProjectionError(
				model.StudyErrorCodeInternal,
				"accepted study event prefix has inconsistent canonical identity resolution",
				"canonical_identity_resolution_corrupt",
			)
		}
		currentIdentityByHistoricalIdentity[event.CardIdentity] = resolved.CurrentCardIdentity

		state := states[resolved.CurrentCardIdentity]
		if state == nil {
			state = &studyProjectionFoldState{}
			states[resolved.CurrentCardIdentity] = state
		}

		switch event.Kind {
		case model.StudyEventKindPresented:
			copyOfEvent := cloneProjectionStudyEvent(event)
			state.LatestPresented = &copyOfEvent
		case model.StudyEventKindRated:
			evidence := &studyProjectionRatingEvidence{
				Event:               cloneProjectionStudyEvent(event),
				CurrentCardIdentity: resolved.CurrentCardIdentity,
				IngestionOrdinal:    *event.ProfileIngestionOrdinal,
			}
			ratingsByEventID[event.ID] = evidence
			state.ratings = append(state.ratings, evidence)
		case model.StudyEventKindRatingVoided:
			target := ratingsByEventID[*event.RatingEventID]
			if target == nil {
				return nil, newStudyProjectionError(
					model.StudyErrorCodeInternal,
					"accepted rating correction does not name an earlier rating",
					"rating_void_target_missing",
				)
			}
			if target.Voided {
				return nil, newStudyProjectionError(
					model.StudyErrorCodeInternal,
					"accepted rating correction repeats an earlier correction",
					"rating_void_target_repeated",
				)
			}
			if target.CurrentCardIdentity != resolved.CurrentCardIdentity ||
				target.Event.LearnerProfileID != event.LearnerProfileID ||
				target.Event.StudySessionID != event.StudySessionID ||
				target.Event.StudySessionItemID != event.StudySessionItemID ||
				target.Event.CardIdentity != event.CardIdentity {
				return nil, newStudyProjectionError(
					model.StudyErrorCodeInternal,
					"accepted rating correction target is inconsistent",
					"rating_void_target_corrupt",
				)
			}
			target.Voided = true
		}
	}

	for _, state := range states {
		for _, rating := range state.ratings {
			if rating.Voided {
				continue
			}
			if state.LatestRating == nil || rating.IngestionOrdinal > state.LatestRating.IngestionOrdinal {
				state.LatestRating = rating
			}
		}
	}
	return states, nil
}

func validateResolvedAcceptedStudyEvent(
	profileID uuid.UUID,
	expectedOrdinal int64,
	resolved StudyResolvedAcceptedEvent,
) error {
	if err := model.ValidateStudyEvent(resolved.Event); err != nil {
		return newStudyProjectionError(
			model.StudyErrorCodeSchemaInvalid,
			"accepted study event shape is invalid",
			"accepted_event_shape_invalid",
		)
	}
	if resolved.Event.LearnerProfileID != profileID {
		return newStudyProjectionError(
			model.StudyErrorCodeInternal,
			"accepted study event prefix crosses learner profiles",
			"accepted_event_profile_corrupt",
		)
	}
	if resolved.Event.ProfileIngestionOrdinal == nil || *resolved.Event.ProfileIngestionOrdinal < 1 {
		return newStudyProjectionError(
			model.StudyErrorCodeInternal,
			"accepted study event is missing its authoritative ingestion ordinal",
			"accepted_event_ordinal_missing",
		)
	}
	if *resolved.Event.ProfileIngestionOrdinal != expectedOrdinal {
		return newStudyProjectionError(
			model.StudyErrorCodeInternal,
			"accepted study event prefix is not gap-free and ordered",
			"accepted_event_ordinal_corrupt",
		)
	}
	if resolved.CurrentCardIdentity == uuid.Nil {
		return newStudyProjectionError(
			model.StudyErrorCodeInternal,
			"accepted study event has no canonical current identity",
			"canonical_identity_missing",
		)
	}
	return nil
}

func cloneProjectionStudyEvent(event model.StudyEvent) model.StudyEvent {
	clone := event
	if event.Rating != nil {
		rating := *event.Rating
		clone.Rating = &rating
	}
	if event.RatingEventID != nil {
		ratingEventID := *event.RatingEventID
		clone.RatingEventID = &ratingEventID
	}
	if event.ClientObservedAt != nil {
		clientObservedAt := *event.ClientObservedAt
		clone.ClientObservedAt = &clientObservedAt
	}
	if event.ProfileIngestionOrdinal != nil {
		ordinal := *event.ProfileIngestionOrdinal
		clone.ProfileIngestionOrdinal = &ordinal
	}
	if event.ReceivedAt != nil {
		receivedAt := *event.ReceivedAt
		clone.ReceivedAt = &receivedAt
	}
	return clone
}
