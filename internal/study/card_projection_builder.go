package study

import (
	"encoding/json"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

// studyCanonicalCardProjectionPayload is deliberately a struct rather than a
// map. Field declaration order fixes its JSON bytes, and operational timestamps
// and mutable projection revisions are absent by construction.
type studyCanonicalCardProjectionPayload struct {
	ProjectionSchemaVersion                   int                `json:"projection_schema_version"`
	LearnerProfileID                          uuid.UUID          `json:"learner_profile_id"`
	ProjectedCardIdentity                     uuid.UUID          `json:"projected_card_identity"`
	LiveSourceIdeaID                          *uuid.UUID         `json:"live_source_idea_id"`
	SchedulerName                             string             `json:"scheduler_name"`
	SchedulerVersion                          string             `json:"scheduler_version"`
	RebuiltThroughProfileIngestionOrdinal     int64              `json:"rebuilt_through_profile_ingestion_ordinal"`
	LatestEffectiveRatingEventID              *uuid.UUID         `json:"latest_effective_rating_event_id"`
	LatestEffectiveRatingEvidenceCardIdentity *uuid.UUID         `json:"latest_effective_rating_evidence_card_identity"`
	LatestEffectiveRating                     *model.StudyRating `json:"latest_effective_rating"`
	LatestEffectiveRatingIngestionOrdinal     *int64             `json:"latest_effective_rating_ingestion_ordinal"`
	LatestPresentedEventID                    *uuid.UUID         `json:"latest_presented_event_id"`
	LatestPresentedEvidenceCardIdentity       *uuid.UUID         `json:"latest_presented_evidence_card_identity"`
	LatestPresentedIngestionOrdinal           *int64             `json:"latest_presented_ingestion_ordinal"`
}

func buildStudyCardProjection(
	input StudyProjectionRebuildInput,
	target StudyProjectionTarget,
	state *studyProjectionFoldState,
) (model.StudySchedulerCardProjection, error) {
	projection := model.StudySchedulerCardProjection{
		LearnerProfileID:                      input.LearnerProfileID,
		ProjectedCardIdentity:                 target.CurrentCardIdentity,
		LiveSourceIdeaID:                      cloneProjectionUUIDPointer(target.LiveSourceIdeaID),
		SchedulerName:                         input.SchedulerName,
		SchedulerVersion:                      input.SchedulerVersion,
		ProjectionSchemaVersion:               input.ProjectionSchemaVersion,
		RebuiltThroughProfileIngestionOrdinal: input.ProfileIngestionHighWater,
	}
	if state != nil && state.LatestRating != nil {
		eventID := state.LatestRating.Event.ID
		evidenceCardIdentity := state.LatestRating.Event.CardIdentity
		rating := *state.LatestRating.Event.Rating
		ingestionOrdinal := state.LatestRating.IngestionOrdinal
		projection.LatestEffectiveRatingEventID = &eventID
		projection.LatestEffectiveRatingEvidenceCardIdentity = &evidenceCardIdentity
		projection.LatestEffectiveRating = &rating
		projection.LatestEffectiveRatingIngestionOrdinal = &ingestionOrdinal
	}
	if state != nil && state.LatestPresented != nil {
		eventID := state.LatestPresented.ID
		evidenceCardIdentity := state.LatestPresented.CardIdentity
		ingestionOrdinal := *state.LatestPresented.ProfileIngestionOrdinal
		projection.LatestPresentedEventID = &eventID
		projection.LatestPresentedEvidenceCardIdentity = &evidenceCardIdentity
		projection.LatestPresentedIngestionOrdinal = &ingestionOrdinal
	}

	payload := studyCanonicalCardProjectionPayload{
		ProjectionSchemaVersion:                   projection.ProjectionSchemaVersion,
		LearnerProfileID:                          projection.LearnerProfileID,
		ProjectedCardIdentity:                     projection.ProjectedCardIdentity,
		LiveSourceIdeaID:                          cloneProjectionUUIDPointer(projection.LiveSourceIdeaID),
		SchedulerName:                             projection.SchedulerName,
		SchedulerVersion:                          projection.SchedulerVersion,
		RebuiltThroughProfileIngestionOrdinal:     projection.RebuiltThroughProfileIngestionOrdinal,
		LatestEffectiveRatingEventID:              cloneProjectionUUIDPointer(projection.LatestEffectiveRatingEventID),
		LatestEffectiveRatingEvidenceCardIdentity: cloneProjectionUUIDPointer(projection.LatestEffectiveRatingEvidenceCardIdentity),
		LatestEffectiveRating:                     cloneProjectionRatingPointer(projection.LatestEffectiveRating),
		LatestEffectiveRatingIngestionOrdinal:     cloneProjectionInt64Pointer(projection.LatestEffectiveRatingIngestionOrdinal),
		LatestPresentedEventID:                    cloneProjectionUUIDPointer(projection.LatestPresentedEventID),
		LatestPresentedEvidenceCardIdentity:       cloneProjectionUUIDPointer(projection.LatestPresentedEvidenceCardIdentity),
		LatestPresentedIngestionOrdinal:           cloneProjectionInt64Pointer(projection.LatestPresentedIngestionOrdinal),
	}
	canonicalPayload, err := json.Marshal(payload)
	if err != nil {
		return model.StudySchedulerCardProjection{}, newStudyProjectionError(
			model.StudyErrorCodeInternal,
			"failed to encode canonical study projection state",
			"canonical_projection_encode_failed",
		)
	}
	projection.CanonicalPayload = json.RawMessage(append([]byte(nil), canonicalPayload...))
	projection.CanonicalSHA256 = model.SHA256Hex(canonicalPayload)
	return projection, nil
}

func cloneProjectionUUIDPointer(value *uuid.UUID) *uuid.UUID {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneProjectionInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneProjectionRatingPointer(value *model.StudyRating) *model.StudyRating {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
