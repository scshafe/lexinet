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

// studyProjectionItemCausalState reconstructs the adjudicator's accepted
// transition state for one immutable session item. Card-level rating history
// remains separate because a void on a later item can make an earlier item's
// still-effective rating authoritative again.
type studyProjectionItemCausalState struct {
	StudySessionID         uuid.UUID
	HistoricalCardIdentity uuid.UUID
	CurrentCardIdentity    uuid.UUID
	ContentSHA256          string
	Presented              bool
	Revealed               bool
	CurrentEffectiveRating *studyProjectionRatingEvidence
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
	itemStates := make(map[uuid.UUID]*studyProjectionItemCausalState)
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

		itemState, err := projectionItemCausalState(itemStates, resolved)
		if err != nil {
			return nil, err
		}

		state := states[resolved.CurrentCardIdentity]
		if state == nil {
			state = &studyProjectionFoldState{}
			states[resolved.CurrentCardIdentity] = state
		}

		switch event.Kind {
		case model.StudyEventKindPresented:
			if itemState.Presented {
				return nil, newStudyProjectionError(
					model.StudyErrorCodeInternal,
					"accepted study item contains more than one presented fact",
					"accepted_item_presented_duplicate",
				)
			}
			copyOfEvent := cloneProjectionStudyEvent(event)
			itemState.Presented = true
			state.LatestPresented = &copyOfEvent
		case model.StudyEventKindRevealed:
			if itemState.Revealed {
				return nil, newStudyProjectionError(
					model.StudyErrorCodeInternal,
					"accepted study item contains more than one revealed fact",
					"accepted_item_revealed_duplicate",
				)
			}
			if !itemState.Presented {
				return nil, newStudyProjectionError(
					model.StudyErrorCodeInternal,
					"accepted study item was revealed before presentation",
					"accepted_item_reveal_without_presented",
				)
			}
			itemState.Revealed = true
		case model.StudyEventKindRated:
			if !itemState.Revealed {
				return nil, newStudyProjectionError(
					model.StudyErrorCodeInternal,
					"accepted study item was rated before reveal",
					"accepted_item_rating_without_revealed",
				)
			}
			if itemState.CurrentEffectiveRating != nil {
				return nil, newStudyProjectionError(
					model.StudyErrorCodeInternal,
					"accepted study item contains a second effective rating",
					"accepted_item_rating_already_effective",
				)
			}
			evidence := &studyProjectionRatingEvidence{
				Event:               cloneProjectionStudyEvent(event),
				CurrentCardIdentity: resolved.CurrentCardIdentity,
				IngestionOrdinal:    *event.ProfileIngestionOrdinal,
			}
			ratingsByEventID[event.ID] = evidence
			state.ratings = append(state.ratings, evidence)
			itemState.CurrentEffectiveRating = evidence
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
			if itemState.CurrentEffectiveRating == nil || itemState.CurrentEffectiveRating.Event.ID != target.Event.ID {
				return nil, newStudyProjectionError(
					model.StudyErrorCodeInternal,
					"accepted rating correction does not name the item's current effective rating",
					"rating_void_target_not_effective",
				)
			}
			target.Voided = true
			itemState.CurrentEffectiveRating = nil
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

func projectionItemCausalState(
	states map[uuid.UUID]*studyProjectionItemCausalState,
	resolved StudyResolvedAcceptedEvent,
) (*studyProjectionItemCausalState, error) {
	event := resolved.Event
	state := states[event.StudySessionItemID]
	if state == nil {
		state = &studyProjectionItemCausalState{
			StudySessionID:         event.StudySessionID,
			HistoricalCardIdentity: event.CardIdentity,
			CurrentCardIdentity:    resolved.CurrentCardIdentity,
			ContentSHA256:          event.ContentSHA256,
		}
		states[event.StudySessionItemID] = state
		return state, nil
	}
	if state.StudySessionID != event.StudySessionID ||
		state.HistoricalCardIdentity != event.CardIdentity ||
		state.CurrentCardIdentity != resolved.CurrentCardIdentity ||
		state.ContentSHA256 != event.ContentSHA256 {
		return nil, newStudyProjectionError(
			model.StudyErrorCodeInternal,
			"accepted study item evidence has inconsistent immutable identity",
			"accepted_item_identity_corrupt",
		)
	}
	return state, nil
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
	if resolved.Event.StudySessionID == uuid.Nil {
		return newStudyProjectionError(
			model.StudyErrorCodeInternal,
			"accepted study event is missing its authoritative session identity",
			"accepted_event_session_missing",
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
