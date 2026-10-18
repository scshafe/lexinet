package study

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"lexinet/internal/model"
	studydigest "lexinet/internal/study/digest"
)

const (
	// StudyMaximumSchedulingCards is the explicit current CPU/memory work bound
	// for one pure scheduling invocation.
	StudyMaximumSchedulingCards = 65_536

	VarietyFirstV1Name    = "variety_first_v1"
	VarietyFirstV1Version = "1"
	ShuffleBagV1Name      = "shuffle_bag_v1"
	ShuffleBagV1Version   = "1"
)

// StudyScheduler is the single production-selection seam.
//
// @extension-point study_scheduler -- policies receive a complete immutable
// snapshot and return a complete immutable plan without storage, clock, or
// randomness authority.
type StudyScheduler interface {
	Plan(StudySchedulingSnapshot) (StudyPlan, error)
}

// StudySchedulingSnapshot contains every authoritative input to one pure plan.
// Callers resolve merge aliases before setting PredecessorTailCardIdentity.
type StudySchedulingSnapshot struct {
	LearnerProfileID               uuid.UUID
	CardKind                       model.StudyCardKind
	CorpusSHA256                   string
	InputProfileIngestionHighWater int64
	PlannedAt                      time.Time
	Seed                           string
	Cards                          []StudySchedulingCard
	PredecessorTailCardIdentity    *uuid.UUID
}

// StudySchedulingCard is one current canonical identity and its rebuildable
// named/versioned projection tail.
type StudySchedulingCard struct {
	CardIdentity uuid.UUID
	Projection   StudyProjectionTail
}

// StudyProjectionTail is the minimal effective scheduler state. It excludes
// timestamps because server ingestion ordinals are the ordering authority.
type StudyProjectionTail struct {
	LatestEffectiveRating           *model.StudyRating
	LatestPresentedIngestionOrdinal *int64
}

// StudyPlan is the pure, fully reproducible policy output. Materialization
// later binds these identities to immutable source snapshots transactionally.
type StudyPlan struct {
	Scheduler    model.StudySchedulerProvenance
	CorpusSHA256 string
	Cards        []StudyPlannedCard
}

// StudyPlannedCard freezes one ordered identity and its selection provenance.
type StudyPlannedCard struct {
	CardIdentity    uuid.UUID
	SelectionReason model.StudySelectionReason
}

type schedulerIdentity struct {
	name                string
	version             string
	configurationSHA256 string
}

func newSchedulerIdentity(name, version string, configurationParts ...[]byte) (schedulerIdentity, error) {
	if !boundedSchedulerText(name, 128) {
		return schedulerIdentity{}, newSchedulerError(
			model.StudyErrorCodeInternal,
			"study scheduler name is invalid",
			"study_scheduler_construct",
			"name",
			"invalid_configuration",
			nil,
		)
	}
	if !boundedSchedulerText(version, 64) {
		return schedulerIdentity{}, newSchedulerError(
			model.StudyErrorCodeInternal,
			"study scheduler version is invalid",
			"study_scheduler_construct",
			"version",
			"invalid_configuration",
			nil,
		)
	}
	digestParts := make([][]byte, 0, 2+len(configurationParts))
	digestParts = append(digestParts, []byte(name), []byte(version))
	digestParts = append(digestParts, configurationParts...)
	configurationSHA256, err := studydigest.Configuration(digestParts...)
	if err != nil {
		return schedulerIdentity{}, newSchedulerError(
			model.StudyErrorCodeInternal,
			"study scheduler configuration cannot be identified",
			"study_scheduler_construct",
			"configuration",
			"digest_failed",
			err,
		)
	}
	return schedulerIdentity{
		name:                name,
		version:             version,
		configurationSHA256: configurationSHA256,
	}, nil
}

func (identity schedulerIdentity) provenance(snapshot StudySchedulingSnapshot) model.StudySchedulerProvenance {
	return model.StudySchedulerProvenance{
		Name:                           identity.name,
		Version:                        identity.version,
		ConfigurationSHA256:            identity.configurationSHA256,
		InputProfileIngestionHighWater: snapshot.InputProfileIngestionHighWater,
		PlannedAt:                      snapshot.PlannedAt.UTC(),
		Seed:                           snapshot.Seed,
	}
}

func validateStudySchedulingSnapshot(snapshot StudySchedulingSnapshot, operation string) error {
	if len(snapshot.Cards) > StudyMaximumSchedulingCards {
		return newSchedulerError(model.StudyErrorCodeRequestTooLarge, "study scheduler card count exceeds the current work bound", operation, "cards", "work_bound_exceeded", nil)
	}
	if snapshot.LearnerProfileID == uuid.Nil {
		return newSchedulerError(model.StudyErrorCodeSchemaInvalid, "study scheduler profile is required", operation, "learner_profile_id", "invalid_snapshot", nil)
	}
	if !snapshot.CardKind.Valid() {
		return newSchedulerError(model.StudyErrorCodeSchemaInvalid, "study scheduler card kind is invalid", operation, "card_kind", "invalid_snapshot", nil)
	}
	if !model.IsStudySHA256(snapshot.CorpusSHA256) {
		return newSchedulerError(model.StudyErrorCodeSchemaInvalid, "study scheduler corpus digest is invalid", operation, "corpus_sha256", "invalid_snapshot", nil)
	}
	if snapshot.InputProfileIngestionHighWater < 0 {
		return newSchedulerError(model.StudyErrorCodeSchemaInvalid, "study scheduler input high-water is invalid", operation, "input_profile_ingestion_high_water", "invalid_snapshot", nil)
	}
	if snapshot.PlannedAt.IsZero() {
		return newSchedulerError(model.StudyErrorCodeSchemaInvalid, "study scheduler planning time is required", operation, "planned_at", "invalid_snapshot", nil)
	}
	if !boundedSchedulerText(snapshot.Seed, 128) {
		return newSchedulerError(model.StudyErrorCodeSchemaInvalid, "study scheduler seed is invalid", operation, "seed", "invalid_snapshot", nil)
	}
	if snapshot.PredecessorTailCardIdentity != nil && *snapshot.PredecessorTailCardIdentity == uuid.Nil {
		return newSchedulerError(model.StudyErrorCodeSchemaInvalid, "study predecessor tail is invalid", operation, "predecessor_tail_card_identity", "invalid_snapshot", nil)
	}

	identities := make(map[uuid.UUID]struct{}, len(snapshot.Cards))
	presentedOrdinals := make(map[int64]struct{}, len(snapshot.Cards))
	for _, card := range snapshot.Cards {
		if card.CardIdentity == uuid.Nil {
			return newSchedulerError(model.StudyErrorCodeSchemaInvalid, "study scheduler card identity is required", operation, "card_identity", "invalid_snapshot", nil)
		}
		if _, exists := identities[card.CardIdentity]; exists {
			return newSchedulerError(model.StudyErrorCodeSchemaInvalid, "study scheduler snapshot repeats a card", operation, "card_identity", "duplicate_card", nil)
		}
		identities[card.CardIdentity] = struct{}{}
		if card.Projection.LatestEffectiveRating != nil && !card.Projection.LatestEffectiveRating.Valid() {
			return newSchedulerError(model.StudyErrorCodeInternal, "study scheduler projection contains an invalid rating", operation, "latest_effective_rating", "projection_corrupt", nil)
		}
		ordinal := card.Projection.LatestPresentedIngestionOrdinal
		if ordinal == nil {
			continue
		}
		if *ordinal < 1 {
			return newSchedulerError(model.StudyErrorCodeInternal, "study scheduler projection contains an invalid presented ordinal", operation, "latest_presented_ingestion_ordinal", "projection_corrupt", nil)
		}
		if _, exists := presentedOrdinals[*ordinal]; exists {
			return newSchedulerError(model.StudyErrorCodeInternal, "study scheduler projections contain a duplicate presented ordinal", operation, "latest_presented_ingestion_ordinal", "projection_corrupt", nil)
		}
		presentedOrdinals[*ordinal] = struct{}{}
	}
	return nil
}

func tieBreakerSHA256(identity schedulerIdentity, seed string, cardKind model.StudyCardKind, cardIdentity uuid.UUID) (string, error) {
	tieSHA256, err := studydigest.Tie(
		[]byte(identity.name),
		[]byte(identity.version),
		[]byte(identity.configurationSHA256),
		[]byte(seed),
		[]byte(cardKind),
		[]byte(cardIdentity.String()),
	)
	if err != nil {
		return "", newSchedulerError(
			model.StudyErrorCodeInternal,
			"study scheduler tie cannot be identified",
			"study_scheduler_plan",
			"tie_breaker",
			"digest_failed",
			err,
		)
	}
	return tieSHA256, nil
}

func ratingRank(rating *model.StudyRating) model.StudyRatingRank {
	if rating == nil {
		return model.StudyRatingRankUnseenUnrated
	}
	switch *rating {
	case model.StudyRatingAgain:
		return model.StudyRatingRankAgain
	case model.StudyRatingHard:
		return model.StudyRatingRankHard
	case model.StudyRatingGood:
		return model.StudyRatingRankGood
	case model.StudyRatingEasy:
		return model.StudyRatingRankEasy
	default:
		return model.StudyRatingRankUnseenUnrated
	}
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	return &copyOfValue
}

func boundedSchedulerText(value string, maximumRunes int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && utf8.RuneCountInString(value) <= maximumRunes
}

func newSchedulerError(
	code model.StudyErrorCode,
	message string,
	operation string,
	field string,
	reason string,
	cause error,
) *model.StudyError {
	return &model.StudyError{
		Code:    code,
		Message: message,
		Context: map[string]string{
			"operation": operation,
			"field":     field,
			"reason":    reason,
		},
		Retryable: false,
		Cause:     cause,
	}
}
