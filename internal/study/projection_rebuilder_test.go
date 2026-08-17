package study

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

func TestRebuildStudyCardProjectionsIncludesUnseenTargetsInCanonicalOrder(t *testing.T) {
	profileID := projectionTestUUID("unseen-profile")
	firstCardID := projectionTestUUID("unseen-card-first")
	secondCardID := projectionTestUUID("unseen-card-second")
	inputOrder := []uuid.UUID{firstCardID, secondCardID}
	if inputOrder[0].String() < inputOrder[1].String() {
		inputOrder[0], inputOrder[1] = inputOrder[1], inputOrder[0]
	}
	targets := make([]StudyProjectionTarget, len(inputOrder))
	for index, cardID := range inputOrder {
		liveSourceID := cardID
		targets[index] = StudyProjectionTarget{CurrentCardIdentity: cardID, LiveSourceIdeaID: &liveSourceID}
	}
	input := projectionTestRebuildInput(profileID, 0, targets, nil)

	first, err := RebuildStudyCardProjections(input)
	if err != nil {
		t.Fatalf("STUDY_PROJECTION.UNSEEN_REBUILD_FAILED: %v", err)
	}
	second, err := RebuildStudyCardProjections(input)
	if err != nil {
		t.Fatalf("STUDY_PROJECTION.UNSEEN_REPLAY_FAILED: %v", err)
	}
	if len(first) != 2 || first[0].ProjectedCardIdentity.String() >= first[1].ProjectedCardIdentity.String() {
		t.Fatal("STUDY_PROJECTION.TARGET_ORDER_NOT_CANONICAL")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("STUDY_PROJECTION.BYTE_IDENTICAL_REPLAY_CHANGED")
	}
	for _, projection := range first {
		if projection.LatestEffectiveRatingEventID != nil ||
			projection.LatestPresentedEventID != nil ||
			projection.RebuiltThroughProfileIngestionOrdinal != 0 ||
			!json.Valid(projection.CanonicalPayload) ||
			model.SHA256Hex(projection.CanonicalPayload) != projection.CanonicalSHA256 {
			t.Fatal("STUDY_PROJECTION.UNSEEN_STATE_INVALID")
		}
		payload := string(projection.CanonicalPayload)
		expectedPayload := fmt.Sprintf(
			`{"projection_schema_version":1,"learner_profile_id":"%s","projected_card_identity":"%s","live_source_idea_id":"%s","scheduler_name":"%s","scheduler_version":"%s","rebuilt_through_profile_ingestion_ordinal":0,"latest_effective_rating_event_id":null,"latest_effective_rating_evidence_card_identity":null,"latest_effective_rating":null,"latest_effective_rating_ingestion_ordinal":null,"latest_presented_event_id":null,"latest_presented_evidence_card_identity":null,"latest_presented_ingestion_ordinal":null}`,
			profileID,
			projection.ProjectedCardIdentity,
			projection.ProjectedCardIdentity,
			VarietyFirstV1Name,
			VarietyFirstV1Version,
		)
		if payload != expectedPayload {
			t.Fatal("STUDY_PROJECTION.CANONICAL_PAYLOAD_GOLDEN_CHANGED")
		}
		for _, forbidden := range []string{"updated_at", "received_at", "created_at", "client_observed_at"} {
			if strings.Contains(payload, forbidden) {
				t.Fatal("STUDY_PROJECTION.CANONICAL_PAYLOAD_CONTAINS_TIMESTAMP")
			}
		}
	}

	first[0].CanonicalPayload[0] = 'X'
	if first[0].LiveSourceIdeaID != nil {
		*first[0].LiveSourceIdeaID = uuid.Nil
	}
	if !json.Valid(second[0].CanonicalPayload) {
		t.Fatal("STUDY_PROJECTION.REPLAY_PAYLOAD_ALIASED_PRIOR_RESULT")
	}
	for _, target := range input.Targets {
		if target.LiveSourceIdeaID == nil || *target.LiveSourceIdeaID == uuid.Nil {
			t.Fatal("STUDY_PROJECTION.RESULT_MUTATION_REACHED_INPUT")
		}
	}
}

func TestRebuildStudyCardProjectionsVoidingLatestRatingResurfacesPriorEvidence(t *testing.T) {
	profileID := projectionTestUUID("rating-profile")
	cardID := projectionTestUUID("rating-card")
	firstSessionID := projectionTestUUID("rating-session-first")
	firstItemID := projectionTestUUID("rating-item-first")
	secondSessionID := projectionTestUUID("rating-session-second")
	secondItemID := projectionTestUUID("rating-item-second")

	events := []StudyResolvedAcceptedEvent{
		projectionTestResolvedEvent(projectionTestEvent(profileID, cardID, firstSessionID, firstItemID, 1, model.StudyEventKindPresented), cardID),
		projectionTestResolvedEvent(projectionTestEvent(profileID, cardID, firstSessionID, firstItemID, 2, model.StudyEventKindRevealed), cardID),
		projectionTestResolvedEvent(projectionTestRatedEvent(profileID, cardID, firstSessionID, firstItemID, 3, model.StudyRatingGood), cardID),
		projectionTestResolvedEvent(projectionTestEvent(profileID, cardID, secondSessionID, secondItemID, 4, model.StudyEventKindPresented), cardID),
		projectionTestResolvedEvent(projectionTestEvent(profileID, cardID, secondSessionID, secondItemID, 5, model.StudyEventKindRevealed), cardID),
		projectionTestResolvedEvent(projectionTestRatedEvent(profileID, cardID, secondSessionID, secondItemID, 6, model.StudyRatingAgain), cardID),
	}
	latestRatingID := events[5].Event.ID
	events = append(events, projectionTestResolvedEvent(
		projectionTestVoidedEvent(profileID, cardID, secondSessionID, secondItemID, 7, latestRatingID),
		cardID,
	))
	liveSourceID := cardID
	targets := []StudyProjectionTarget{{CurrentCardIdentity: cardID, LiveSourceIdeaID: &liveSourceID}}

	projections, err := RebuildStudyCardProjections(projectionTestRebuildInput(profileID, 7, targets, events))
	if err != nil {
		t.Fatalf("STUDY_PROJECTION.RATING_VOID_REBUILD_FAILED: %v", err)
	}
	projection := projections[0]
	assertProjectionRating(t, projection, events[2].Event.ID, cardID, model.StudyRatingGood, 3)
	assertProjectionPresented(t, projection, events[3].Event.ID, cardID, 4)

	replacement := projectionTestResolvedEvent(
		projectionTestRatedEvent(profileID, cardID, secondSessionID, secondItemID, 8, model.StudyRatingHard),
		cardID,
	)
	events = append(events, replacement)
	projections, err = RebuildStudyCardProjections(projectionTestRebuildInput(profileID, 8, targets, events))
	if err != nil {
		t.Fatalf("STUDY_PROJECTION.REPLACEMENT_RATING_REBUILD_FAILED: %v", err)
	}
	assertProjectionRating(t, projections[0], replacement.Event.ID, cardID, model.StudyRatingHard, 8)
	assertProjectionPresented(t, projections[0], events[3].Event.ID, cardID, 4)
}

func TestRebuildStudyCardProjectionsConsolidatesMergedHistoricalEvidence(t *testing.T) {
	profileID := projectionTestUUID("merge-profile")
	historicalSourceID := projectionTestUUID("merge-source-card")
	currentSurvivorID := projectionTestUUID("merge-survivor-card")
	sourceSessionID := projectionTestUUID("merge-source-session")
	sourceItemID := projectionTestUUID("merge-source-item")
	survivorSessionID := projectionTestUUID("merge-survivor-session")
	survivorItemID := projectionTestUUID("merge-survivor-item")

	events := []StudyResolvedAcceptedEvent{
		projectionTestResolvedEvent(projectionTestEvent(profileID, historicalSourceID, sourceSessionID, sourceItemID, 1, model.StudyEventKindPresented), currentSurvivorID),
		projectionTestResolvedEvent(projectionTestEvent(profileID, historicalSourceID, sourceSessionID, sourceItemID, 2, model.StudyEventKindRevealed), currentSurvivorID),
		projectionTestResolvedEvent(projectionTestRatedEvent(profileID, historicalSourceID, sourceSessionID, sourceItemID, 3, model.StudyRatingGood), currentSurvivorID),
		projectionTestResolvedEvent(projectionTestEvent(profileID, currentSurvivorID, survivorSessionID, survivorItemID, 4, model.StudyEventKindPresented), currentSurvivorID),
		projectionTestResolvedEvent(projectionTestEvent(profileID, currentSurvivorID, survivorSessionID, survivorItemID, 5, model.StudyEventKindRevealed), currentSurvivorID),
		projectionTestResolvedEvent(projectionTestRatedEvent(profileID, currentSurvivorID, survivorSessionID, survivorItemID, 6, model.StudyRatingEasy), currentSurvivorID),
	}
	latestSurvivorRatingID := events[5].Event.ID
	events = append(events, projectionTestResolvedEvent(
		projectionTestVoidedEvent(profileID, currentSurvivorID, survivorSessionID, survivorItemID, 7, latestSurvivorRatingID),
		currentSurvivorID,
	))
	liveSourceID := currentSurvivorID

	projections, err := RebuildStudyCardProjections(projectionTestRebuildInput(
		profileID,
		7,
		[]StudyProjectionTarget{{CurrentCardIdentity: currentSurvivorID, LiveSourceIdeaID: &liveSourceID}},
		events,
	))
	if err != nil {
		t.Fatalf("STUDY_PROJECTION.MERGE_REBUILD_FAILED: %v", err)
	}
	if len(projections) != 1 || projections[0].ProjectedCardIdentity != currentSurvivorID {
		t.Fatal("STUDY_PROJECTION.MERGE_DID_NOT_CONSOLIDATE_TO_SURVIVOR")
	}
	assertProjectionRating(t, projections[0], events[2].Event.ID, historicalSourceID, model.StudyRatingGood, 3)
	assertProjectionPresented(t, projections[0], events[3].Event.ID, currentSurvivorID, 4)
}

func TestRebuildStudyCardProjectionsIgnoresAcceptedEvidenceOutsideRequestedTargets(t *testing.T) {
	profileID := projectionTestUUID("non-target-profile")
	targetCardID := projectionTestUUID("requested-target-card")
	nonTargetCardID := projectionTestUUID("accepted-non-target-card")
	sessionID := projectionTestUUID("non-target-session")
	itemID := projectionTestUUID("non-target-item")
	liveSourceID := targetCardID
	event := projectionTestResolvedEvent(
		projectionTestEvent(profileID, nonTargetCardID, sessionID, itemID, 1, model.StudyEventKindPresented),
		nonTargetCardID,
	)

	projections, err := RebuildStudyCardProjections(projectionTestRebuildInput(
		profileID,
		1,
		[]StudyProjectionTarget{{CurrentCardIdentity: targetCardID, LiveSourceIdeaID: &liveSourceID}},
		[]StudyResolvedAcceptedEvent{event},
	))
	if err != nil {
		t.Fatalf("STUDY_PROJECTION.NON_TARGET_REBUILD_FAILED: %v", err)
	}
	if len(projections) != 1 || projections[0].ProjectedCardIdentity != targetCardID ||
		projections[0].LatestEffectiveRatingEventID != nil || projections[0].LatestPresentedEventID != nil {
		t.Fatal("STUDY_PROJECTION.NON_TARGET_EVIDENCE_FABRICATED_TARGET_STATE")
	}
}

func TestRebuildStudyCardProjectionsRejectsCorruptAcceptedPrefixes(t *testing.T) {
	profileID := projectionTestUUID("corruption-profile")
	otherProfileID := projectionTestUUID("corruption-other-profile")
	historicalCardID := projectionTestUUID("corruption-historical-card")
	currentCardID := projectionTestUUID("corruption-current-card")
	otherCurrentCardID := projectionTestUUID("corruption-other-current-card")
	sessionID := projectionTestUUID("corruption-session")
	itemID := projectionTestUUID("corruption-item")
	otherItemID := projectionTestUUID("corruption-other-item")
	liveSourceID := currentCardID
	baseTarget := []StudyProjectionTarget{{CurrentCardIdentity: currentCardID, LiveSourceIdeaID: &liveSourceID}}

	validFirst := projectionTestResolvedEvent(
		projectionTestEvent(profileID, historicalCardID, sessionID, itemID, 1, model.StudyEventKindPresented),
		currentCardID,
	)
	validSecond := projectionTestResolvedEvent(
		projectionTestEvent(profileID, historicalCardID, sessionID, itemID, 2, model.StudyEventKindRevealed),
		currentCardID,
	)

	tests := []struct {
		name      string
		highWater int64
		events    []StudyResolvedAcceptedEvent
		code      model.StudyErrorCode
		reason    string
	}{
		{
			name:      "prefix length does not reach high-water",
			highWater: 2,
			events:    []StudyResolvedAcceptedEvent{validFirst},
			code:      model.StudyErrorCodeInternal,
			reason:    "accepted_prefix_length_corrupt",
		},
		{
			name:      "accepted marker is missing",
			highWater: 1,
			events: []StudyResolvedAcceptedEvent{projectionTestMutateResolvedEvent(validFirst, func(event *model.StudyEvent) {
				event.ProfileIngestionOrdinal = nil
			})},
			code:   model.StudyErrorCodeInternal,
			reason: "accepted_event_ordinal_missing",
		},
		{
			name:      "accepted ordinal is zero",
			highWater: 1,
			events: []StudyResolvedAcceptedEvent{projectionTestMutateResolvedEvent(validFirst, func(event *model.StudyEvent) {
				zero := int64(0)
				event.ProfileIngestionOrdinal = &zero
			})},
			code:   model.StudyErrorCodeInternal,
			reason: "accepted_event_ordinal_missing",
		},
		{
			name:      "permuted accepted prefix",
			highWater: 2,
			events:    []StudyResolvedAcceptedEvent{validSecond, validFirst},
			code:      model.StudyErrorCodeInternal,
			reason:    "accepted_event_ordinal_corrupt",
		},
		{
			name:      "duplicate accepted ordinal",
			highWater: 2,
			events: []StudyResolvedAcceptedEvent{
				validFirst,
				projectionTestMutateResolvedEvent(validSecond, func(event *model.StudyEvent) {
					one := int64(1)
					event.ProfileIngestionOrdinal = &one
				}),
			},
			code:   model.StudyErrorCodeInternal,
			reason: "accepted_event_ordinal_corrupt",
		},
		{
			name:      "profile changes inside accepted prefix",
			highWater: 1,
			events: []StudyResolvedAcceptedEvent{projectionTestMutateResolvedEvent(validFirst, func(event *model.StudyEvent) {
				event.LearnerProfileID = otherProfileID
			})},
			code:   model.StudyErrorCodeInternal,
			reason: "accepted_event_profile_corrupt",
		},
		{
			name:      "canonical current identity is missing",
			highWater: 1,
			events: []StudyResolvedAcceptedEvent{{
				Event:               validFirst.Event,
				CurrentCardIdentity: uuid.Nil,
			}},
			code:   model.StudyErrorCodeInternal,
			reason: "canonical_identity_missing",
		},
		{
			name:      "same historical identity resolves inconsistently",
			highWater: 2,
			events: []StudyResolvedAcceptedEvent{
				validFirst,
				{Event: validSecond.Event, CurrentCardIdentity: otherCurrentCardID},
			},
			code:   model.StudyErrorCodeInternal,
			reason: "canonical_identity_resolution_corrupt",
		},
		{
			name:      "duplicate accepted event identity",
			highWater: 2,
			events: []StudyResolvedAcceptedEvent{
				validFirst,
				projectionTestMutateResolvedEvent(validSecond, func(event *model.StudyEvent) {
					event.ID = validFirst.Event.ID
				}),
			},
			code:   model.StudyErrorCodeInternal,
			reason: "accepted_event_identity_duplicate",
		},
		{
			name:      "invalid accepted event payload shape",
			highWater: 1,
			events: []StudyResolvedAcceptedEvent{projectionTestMutateResolvedEvent(validFirst, func(event *model.StudyEvent) {
				event.Kind = model.StudyEventKindRated
				event.Rating = nil
			})},
			code:   model.StudyErrorCodeSchemaInvalid,
			reason: "accepted_event_shape_invalid",
		},
		{
			name:      "void target is unknown",
			highWater: 1,
			events: []StudyResolvedAcceptedEvent{projectionTestResolvedEvent(
				projectionTestVoidedEvent(profileID, historicalCardID, sessionID, itemID, 1, projectionTestUUID("unknown-rating")),
				currentCardID,
			)},
			code:   model.StudyErrorCodeInternal,
			reason: "rating_void_target_missing",
		},
		{
			name:      "void target belongs to another item",
			highWater: 2,
			events: func() []StudyResolvedAcceptedEvent {
				rated := projectionTestResolvedEvent(
					projectionTestRatedEvent(profileID, historicalCardID, sessionID, itemID, 1, model.StudyRatingGood),
					currentCardID,
				)
				voided := projectionTestResolvedEvent(
					projectionTestVoidedEvent(profileID, historicalCardID, sessionID, otherItemID, 2, rated.Event.ID),
					currentCardID,
				)
				return []StudyResolvedAcceptedEvent{rated, voided}
			}(),
			code:   model.StudyErrorCodeInternal,
			reason: "rating_void_target_corrupt",
		},
		{
			name:      "same rating is voided twice",
			highWater: 3,
			events: func() []StudyResolvedAcceptedEvent {
				rated := projectionTestResolvedEvent(
					projectionTestRatedEvent(profileID, historicalCardID, sessionID, itemID, 1, model.StudyRatingGood),
					currentCardID,
				)
				firstVoid := projectionTestResolvedEvent(
					projectionTestVoidedEvent(profileID, historicalCardID, sessionID, itemID, 2, rated.Event.ID),
					currentCardID,
				)
				secondVoid := projectionTestResolvedEvent(
					projectionTestVoidedEvent(profileID, historicalCardID, sessionID, itemID, 3, rated.Event.ID),
					currentCardID,
				)
				return []StudyResolvedAcceptedEvent{rated, firstVoid, secondVoid}
			}(),
			code:   model.StudyErrorCodeInternal,
			reason: "rating_void_target_repeated",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := RebuildStudyCardProjections(projectionTestRebuildInput(
				profileID,
				test.highWater,
				baseTarget,
				test.events,
			))
			assertProjectionStudyError(t, err, test.code, test.reason)
		})
	}
}

func TestRebuildStudyCardProjectionsRejectsInvalidTargets(t *testing.T) {
	profileID := projectionTestUUID("target-profile")
	cardID := projectionTestUUID("target-card")
	otherCardID := projectionTestUUID("target-other-card")

	tests := []struct {
		name    string
		targets []StudyProjectionTarget
		reason  string
	}{
		{
			name:    "zero current identity",
			targets: []StudyProjectionTarget{{CurrentCardIdentity: uuid.Nil}},
			reason:  "target_identity_invalid",
		},
		{
			name: "live source differs from current identity",
			targets: []StudyProjectionTarget{{
				CurrentCardIdentity: cardID,
				LiveSourceIdeaID:    &otherCardID,
			}},
			reason: "target_live_source_invalid",
		},
		{
			name: "duplicate current identity",
			targets: []StudyProjectionTarget{
				{CurrentCardIdentity: cardID},
				{CurrentCardIdentity: cardID},
			},
			reason: "target_identity_duplicate",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := RebuildStudyCardProjections(projectionTestRebuildInput(profileID, 0, test.targets, nil))
			assertProjectionStudyError(t, err, model.StudyErrorCodeSchemaInvalid, test.reason)
		})
	}
}

func TestRebuildStudyCardProjectionsExcludesOperationalTimestampsFromCanonicalBytes(t *testing.T) {
	profileID := projectionTestUUID("timestamp-profile")
	cardID := projectionTestUUID("timestamp-card")
	sessionID := projectionTestUUID("timestamp-session")
	itemID := projectionTestUUID("timestamp-item")
	liveSourceID := cardID
	targets := []StudyProjectionTarget{{CurrentCardIdentity: cardID, LiveSourceIdeaID: &liveSourceID}}

	firstEvent := projectionTestEvent(profileID, cardID, sessionID, itemID, 1, model.StudyEventKindPresented)
	secondEvent := cloneProjectionStudyEvent(firstEvent)
	firstObservedAt := time.Date(2026, 8, 17, 8, 0, 0, 0, time.UTC)
	firstReceivedAt := time.Date(2026, 8, 17, 8, 0, 1, 0, time.UTC)
	secondObservedAt := time.Date(2036, 1, 2, 3, 4, 5, 6, time.FixedZone("fixture", 9*60*60))
	secondReceivedAt := time.Date(2046, 6, 7, 8, 9, 10, 11, time.UTC)
	firstEvent.ClientObservedAt = &firstObservedAt
	firstEvent.ReceivedAt = &firstReceivedAt
	secondEvent.ClientObservedAt = &secondObservedAt
	secondEvent.ReceivedAt = &secondReceivedAt

	first, err := RebuildStudyCardProjections(projectionTestRebuildInput(
		profileID,
		1,
		targets,
		[]StudyResolvedAcceptedEvent{projectionTestResolvedEvent(firstEvent, cardID)},
	))
	if err != nil {
		t.Fatalf("STUDY_PROJECTION.FIRST_TIMESTAMP_REBUILD_FAILED: %v", err)
	}
	second, err := RebuildStudyCardProjections(projectionTestRebuildInput(
		profileID,
		1,
		targets,
		[]StudyResolvedAcceptedEvent{projectionTestResolvedEvent(secondEvent, cardID)},
	))
	if err != nil {
		t.Fatalf("STUDY_PROJECTION.SECOND_TIMESTAMP_REBUILD_FAILED: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("STUDY_PROJECTION.OPERATIONAL_TIMESTAMP_CHANGED_CANONICAL_STATE")
	}
}

func projectionTestRebuildInput(
	profileID uuid.UUID,
	highWater int64,
	targets []StudyProjectionTarget,
	events []StudyResolvedAcceptedEvent,
) StudyProjectionRebuildInput {
	return StudyProjectionRebuildInput{
		LearnerProfileID:          profileID,
		SchedulerName:             VarietyFirstV1Name,
		SchedulerVersion:          VarietyFirstV1Version,
		ProjectionSchemaVersion:   1,
		ProfileIngestionHighWater: highWater,
		Targets:                   targets,
		AcceptedEvents:            events,
	}
}

func projectionTestResolvedEvent(event model.StudyEvent, currentCardIdentity uuid.UUID) StudyResolvedAcceptedEvent {
	return StudyResolvedAcceptedEvent{Event: event, CurrentCardIdentity: currentCardIdentity}
}

func projectionTestEvent(
	profileID, historicalCardID, sessionID, itemID uuid.UUID,
	ordinal int64,
	kind model.StudyEventKind,
) model.StudyEvent {
	receivedAt := time.Date(2026, 8, 17, 12, 0, int(ordinal), 0, time.UTC)
	eventID := projectionTestUUID("event-" + historicalCardID.String() + "-" + sessionID.String() + "-" + itemID.String() + "-" + string(kind) + "-" + projectionTestOrdinal(ordinal))
	streamID := projectionTestUUID("stream-" + profileID.String())
	return model.StudyEvent{
		ID:                      eventID,
		ClientSequence:          ordinal,
		ProfileRef:              "projection-test-profile",
		LearnerProfileID:        profileID,
		StudySessionID:          sessionID,
		StudySessionItemID:      itemID,
		CardIdentity:            historicalCardID,
		ContentSHA256:           model.SHA256Hex([]byte("projection-content-" + historicalCardID.String())),
		Kind:                    kind,
		ClientClockQuality:      model.StudyClientClockQualityUnknown,
		ClientStreamID:          streamID,
		NormalizedPayloadSHA256: model.SHA256Hex([]byte("projection-event-" + eventID.String())),
		ProfileIngestionOrdinal: projectionTestInt64Pointer(ordinal),
		ReceivedAt:              &receivedAt,
	}
}

func projectionTestRatedEvent(
	profileID, historicalCardID, sessionID, itemID uuid.UUID,
	ordinal int64,
	rating model.StudyRating,
) model.StudyEvent {
	event := projectionTestEvent(profileID, historicalCardID, sessionID, itemID, ordinal, model.StudyEventKindRated)
	event.Rating = &rating
	return event
}

func projectionTestVoidedEvent(
	profileID, historicalCardID, sessionID, itemID uuid.UUID,
	ordinal int64,
	ratingEventID uuid.UUID,
) model.StudyEvent {
	event := projectionTestEvent(profileID, historicalCardID, sessionID, itemID, ordinal, model.StudyEventKindRatingVoided)
	event.RatingEventID = &ratingEventID
	return event
}

func projectionTestMutateResolvedEvent(
	resolved StudyResolvedAcceptedEvent,
	mutate func(*model.StudyEvent),
) StudyResolvedAcceptedEvent {
	clone := cloneProjectionStudyEvent(resolved.Event)
	mutate(&clone)
	return StudyResolvedAcceptedEvent{Event: clone, CurrentCardIdentity: resolved.CurrentCardIdentity}
}

func projectionTestUUID(label string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(label))
}

func projectionTestOrdinal(ordinal int64) string {
	const digits = "0123456789"
	if ordinal == 0 {
		return "0"
	}
	value := ordinal
	encoded := make([]byte, 0, 20)
	for value > 0 {
		encoded = append(encoded, digits[value%10])
		value /= 10
	}
	for left, right := 0, len(encoded)-1; left < right; left, right = left+1, right-1 {
		encoded[left], encoded[right] = encoded[right], encoded[left]
	}
	return string(encoded)
}

func projectionTestInt64Pointer(value int64) *int64 {
	return &value
}

func assertProjectionRating(
	t *testing.T,
	projection model.StudySchedulerCardProjection,
	eventID, evidenceCardIdentity uuid.UUID,
	rating model.StudyRating,
	ordinal int64,
) {
	t.Helper()
	if projection.LatestEffectiveRatingEventID == nil || *projection.LatestEffectiveRatingEventID != eventID ||
		projection.LatestEffectiveRatingEvidenceCardIdentity == nil || *projection.LatestEffectiveRatingEvidenceCardIdentity != evidenceCardIdentity ||
		projection.LatestEffectiveRating == nil || *projection.LatestEffectiveRating != rating ||
		projection.LatestEffectiveRatingIngestionOrdinal == nil || *projection.LatestEffectiveRatingIngestionOrdinal != ordinal {
		t.Fatal("STUDY_PROJECTION.LATEST_EFFECTIVE_RATING_MISMATCH")
	}
}

func assertProjectionPresented(
	t *testing.T,
	projection model.StudySchedulerCardProjection,
	eventID, evidenceCardIdentity uuid.UUID,
	ordinal int64,
) {
	t.Helper()
	if projection.LatestPresentedEventID == nil || *projection.LatestPresentedEventID != eventID ||
		projection.LatestPresentedEvidenceCardIdentity == nil || *projection.LatestPresentedEvidenceCardIdentity != evidenceCardIdentity ||
		projection.LatestPresentedIngestionOrdinal == nil || *projection.LatestPresentedIngestionOrdinal != ordinal {
		t.Fatal("STUDY_PROJECTION.LATEST_PRESENTED_MISMATCH")
	}
}

func assertProjectionStudyError(t *testing.T, err error, code model.StudyErrorCode, reason string) {
	t.Helper()
	var studyError *model.StudyError
	if !errors.As(err, &studyError) || studyError.Code != code {
		t.Fatalf("STUDY_PROJECTION.UNEXPECTED_ERROR: got=%v want_code=%s", err, code)
	}
	if studyError.Context["operation"] != studyProjectionRebuildOperation || studyError.Context["reason"] != reason {
		t.Fatalf("STUDY_PROJECTION.UNEXPECTED_ERROR_CONTEXT: %#v", studyError.Context)
	}
	if studyError.Cause != nil || studyError.Retryable {
		t.Fatal("STUDY_PROJECTION.ERROR_EXPOSED_CAUSE_OR_RETRYABLE_STATE")
	}
}
