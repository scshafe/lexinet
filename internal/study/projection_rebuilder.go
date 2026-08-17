package study

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

const studyProjectionRebuildOperation = "study_projection_rebuild"

// StudyProjectionTarget identifies one current scheduler identity whose state
// must be present in a complete rebuild. A nil LiveSourceIdeaID explicitly
// represents historical state whose editable source no longer exists.
type StudyProjectionTarget struct {
	CurrentCardIdentity uuid.UUID
	LiveSourceIdeaID    *uuid.UUID
}

// StudyResolvedAcceptedEvent pairs one canonical accepted event with the
// current identity obtained by resolving its historical card identity through
// every durable merge redirect at the rebuild boundary.
type StudyResolvedAcceptedEvent struct {
	Event               model.StudyEvent
	CurrentCardIdentity uuid.UUID
}

// StudyProjectionRebuildInput is the complete, immutable input to one pure
// named/versioned projection rebuild. AcceptedEvents must be the exact ordered
// profile prefix from ordinal one through ProfileIngestionHighWater.
type StudyProjectionRebuildInput struct {
	LearnerProfileID          uuid.UUID
	SchedulerName             string
	SchedulerVersion          string
	ProjectionSchemaVersion   int
	ProfileIngestionHighWater int64
	Targets                   []StudyProjectionTarget
	AcceptedEvents            []StudyResolvedAcceptedEvent
}

// RebuildStudyCardProjections deterministically folds one exact accepted event
// prefix into complete timestamp-free per-card scheduler projections.
//
// @canonical study_scheduler_projection_rebuild -- all named/versioned study
// projection rebuilds must pass through this pure accepted-event fold.
func RebuildStudyCardProjections(input StudyProjectionRebuildInput) ([]model.StudySchedulerCardProjection, error) {
	if err := validateStudyProjectionRebuildIdentity(input); err != nil {
		return nil, err
	}
	targets, err := normalizeStudyProjectionTargets(input.Targets)
	if err != nil {
		return nil, err
	}
	folded, err := foldResolvedAcceptedStudyEvents(
		input.LearnerProfileID,
		input.ProfileIngestionHighWater,
		input.AcceptedEvents,
	)
	if err != nil {
		return nil, err
	}

	projections := make([]model.StudySchedulerCardProjection, 0, len(targets))
	for _, target := range targets {
		projection, buildErr := buildStudyCardProjection(input, target, folded[target.CurrentCardIdentity])
		if buildErr != nil {
			return nil, buildErr
		}
		projections = append(projections, projection)
	}
	return projections, nil
}

func validateStudyProjectionRebuildIdentity(input StudyProjectionRebuildInput) error {
	if input.LearnerProfileID == uuid.Nil {
		return newStudyProjectionError(
			model.StudyErrorCodeSchemaInvalid,
			"study projection learner identity is invalid",
			"learner_profile_invalid",
		)
	}
	if !boundedStudyProjectionText(input.SchedulerName, 128) {
		return newStudyProjectionError(
			model.StudyErrorCodeSchemaInvalid,
			"study projection scheduler name is invalid",
			"scheduler_name_invalid",
		)
	}
	if !boundedStudyProjectionText(input.SchedulerVersion, 64) {
		return newStudyProjectionError(
			model.StudyErrorCodeSchemaInvalid,
			"study projection scheduler version is invalid",
			"scheduler_version_invalid",
		)
	}
	if input.ProjectionSchemaVersion < 1 {
		return newStudyProjectionError(
			model.StudyErrorCodeSchemaInvalid,
			"study projection schema version is invalid",
			"projection_schema_version_invalid",
		)
	}
	if input.ProfileIngestionHighWater < 0 {
		return newStudyProjectionError(
			model.StudyErrorCodeSchemaInvalid,
			"study projection high-water is invalid",
			"profile_ingestion_high_water_invalid",
		)
	}
	return nil
}

func normalizeStudyProjectionTargets(targets []StudyProjectionTarget) ([]StudyProjectionTarget, error) {
	normalized := make([]StudyProjectionTarget, len(targets))
	for index, target := range targets {
		if target.CurrentCardIdentity == uuid.Nil {
			return nil, newStudyProjectionError(
				model.StudyErrorCodeSchemaInvalid,
				"study projection target identity is invalid",
				"target_identity_invalid",
			)
		}
		normalized[index] = StudyProjectionTarget{CurrentCardIdentity: target.CurrentCardIdentity}
		if target.LiveSourceIdeaID != nil {
			if *target.LiveSourceIdeaID == uuid.Nil || *target.LiveSourceIdeaID != target.CurrentCardIdentity {
				return nil, newStudyProjectionError(
					model.StudyErrorCodeSchemaInvalid,
					"study projection live source identity is invalid",
					"target_live_source_invalid",
				)
			}
			liveSourceIdeaID := *target.LiveSourceIdeaID
			normalized[index].LiveSourceIdeaID = &liveSourceIdeaID
		}
	}
	sort.Slice(normalized, func(left, right int) bool {
		return normalized[left].CurrentCardIdentity.String() < normalized[right].CurrentCardIdentity.String()
	})
	for index := 1; index < len(normalized); index++ {
		if normalized[index-1].CurrentCardIdentity == normalized[index].CurrentCardIdentity {
			return nil, newStudyProjectionError(
				model.StudyErrorCodeSchemaInvalid,
				"study projection targets contain a duplicate current identity",
				"target_identity_duplicate",
			)
		}
	}
	return normalized, nil
}

func boundedStudyProjectionText(value string, maximumRunes int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && utf8.RuneCountInString(value) <= maximumRunes
}

func newStudyProjectionError(code model.StudyErrorCode, message, reason string) *model.StudyError {
	return &model.StudyError{
		Code:    code,
		Message: message,
		Context: map[string]string{
			"operation": studyProjectionRebuildOperation,
			"reason":    reason,
		},
		Retryable: false,
	}
}
