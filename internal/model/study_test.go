package model

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNormalizeStudyScopeSortsStableIdentitiesAndReferences(t *testing.T) {
	firstTopicID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	secondTopicID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	normalized, err := NormalizeStudyScope(StudyScope{
		TopicIDs:  []uuid.UUID{secondTopicID, firstTopicID},
		TopicRefs: []string{"software", "literature"},
		CardKind:  StudyCardKindIdea,
	})
	if err != nil {
		t.Fatalf("normalize study scope: %v", err)
	}
	if normalized.Scope.TopicIDs[0] != firstTopicID || normalized.Scope.TopicRefs[0] != "literature" {
		t.Fatalf("normalized scope was not sorted: %#v", normalized.Scope)
	}
}

func TestNormalizeStudyScopePermutationHasStableContentIdentity(t *testing.T) {
	firstTopicID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	secondTopicID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	first, err := NormalizeStudyScope(StudyScope{
		TopicIDs:  []uuid.UUID{firstTopicID, secondTopicID},
		TopicRefs: []string{"literature", "software"},
		CardKind:  StudyCardKindIdea,
	})
	if err != nil {
		t.Fatalf("normalize first study scope: %v", err)
	}
	second, err := NormalizeStudyScope(StudyScope{
		TopicIDs:  []uuid.UUID{secondTopicID, firstTopicID},
		TopicRefs: []string{"software", "literature"},
		CardKind:  StudyCardKindIdea,
	})
	if err != nil {
		t.Fatalf("normalize second study scope: %v", err)
	}
	if first.CanonicalSelectorHash != second.CanonicalSelectorHash || string(first.CanonicalSelector) != string(second.CanonicalSelector) {
		t.Fatalf("permuted scope changed canonical identity: first=%s second=%s", first.CanonicalSelectorHash, second.CanonicalSelectorHash)
	}
}

func TestNormalizeStudyScopeRejectsDuplicateStableTopicIdentity(t *testing.T) {
	topicID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	_, err := NormalizeStudyScope(StudyScope{
		TopicIDs:  []uuid.UUID{topicID, topicID},
		TopicRefs: []string{"literature", "software"},
		CardKind:  StudyCardKindIdea,
	})
	assertStudyErrorCode(t, err, StudyErrorCodeSchemaInvalid)
}

func TestNormalizeStudyScopeRejectsTopicReferenceBeyondFrozenBound(t *testing.T) {
	_, err := NormalizeStudyScope(StudyScope{
		TopicIDs:  []uuid.UUID{uuid.MustParse("11111111-1111-1111-1111-111111111111")},
		TopicRefs: []string{strings.Repeat("a", 129)},
		CardKind:  StudyCardKindIdea,
	})
	assertStudyErrorCode(t, err, StudyErrorCodeSchemaInvalid)
}

func TestNormalizeStudyEventPayloadNormalizesObservedTimeToUTC(t *testing.T) {
	observedAt := time.Date(2026, 8, 16, 12, 30, 0, 123, time.FixedZone("fixture", -7*60*60))
	event := validStudyEventFixture()
	event.ClientObservedAt = &observedAt
	firstPayload, firstHash, err := NormalizeStudyEventPayload(event)
	if err != nil {
		t.Fatalf("normalize first study event: %v", err)
	}
	utcObservedAt := observedAt.UTC()
	event.ClientObservedAt = &utcObservedAt
	secondPayload, secondHash, err := NormalizeStudyEventPayload(event)
	if err != nil {
		t.Fatalf("normalize second study event: %v", err)
	}
	if firstHash != secondHash || string(firstPayload) != string(secondPayload) {
		t.Fatalf("equivalent instants produced different normalized payloads: first=%s second=%s", firstHash, secondHash)
	}
}

func TestValidateStudyEventRejectsRatedEventWithoutRating(t *testing.T) {
	event := validStudyEventFixture()
	event.Kind = StudyEventKindRated
	err := ValidateStudyEvent(event)
	assertStudyErrorCode(t, err, StudyErrorCodeSchemaInvalid)
}

func TestValidateStudyEventRejectsRatingVoidWithoutExactTarget(t *testing.T) {
	event := validStudyEventFixture()
	event.Kind = StudyEventKindRatingVoided
	zeroTarget := uuid.Nil
	event.RatingEventID = &zeroTarget
	err := ValidateStudyEvent(event)
	assertStudyErrorCode(t, err, StudyErrorCodeSchemaInvalid)
}

func TestValidateStudySessionItemAcceptsFrozenTextBoundaries(t *testing.T) {
	item := validStudySessionItemFixture()
	item.FrontCanonicalName = strings.Repeat("f", 512)
	item.BackDefinition = strings.Repeat("d", 16384)
	example := strings.Repeat("e", 16384)
	item.BackExample = &example
	if err := ValidateStudySessionItem(item); err != nil {
		t.Fatalf("validate boundary study session item: %v", err)
	}
}

func TestValidateStudySessionItemRejectsFrontBeyondFrozenBound(t *testing.T) {
	item := validStudySessionItemFixture()
	item.FrontCanonicalName = strings.Repeat("f", 513)
	err := ValidateStudySessionItem(item)
	assertStudyErrorCode(t, err, StudyErrorCodeSchemaInvalid)
}

func TestValidateStudySessionItemRejectsInvalidUTF8WithoutCoercion(t *testing.T) {
	item := validStudySessionItemFixture()
	item.FrontCanonicalName = string([]byte{0xff, 0xfe})
	err := ValidateStudySessionItem(item)
	assertStudyErrorCode(t, err, StudyErrorCodeSchemaInvalid)
}

func TestStudyEventReceiptCodeMatchesOnlyItsDisposition(t *testing.T) {
	if StudyEventReceiptCodeAccepted.ValidFor(StudyEventReceiptDispositionTerminalRejected) {
		t.Fatal("accepted code was valid for terminal rejection")
	}
}

func TestStudyEventReceiptDispositionRejectsUnknownValue(t *testing.T) {
	if StudyEventReceiptDisposition("unknown").Valid() {
		t.Fatal("unknown event receipt disposition was valid")
	}
}

func TestStudyEventReceiptCodeRejectsUnknownValue(t *testing.T) {
	if StudyEventReceiptCode("STUDY_EVENT_UNKNOWN").Valid() {
		t.Fatal("unknown event receipt code was valid")
	}
}

func TestStudyErrorCodeEnumeratesFrozenAuthorizationAndLimitFailures(t *testing.T) {
	for _, code := range []StudyErrorCode{
		StudyErrorCodeUnauthorized,
		StudyErrorCodeAuthorizationDenied,
		StudyErrorCodeRequestTooLarge,
		StudyErrorCodeRateLimited,
	} {
		if !code.Valid() {
			t.Fatalf("frozen error code is invalid: %s", code)
		}
	}
}

func TestStudyErrorIsComparesStableCode(t *testing.T) {
	err := &StudyError{Code: StudyErrorCodeStateStale, Message: "fixture stale state"}
	if !errors.Is(err, &StudyError{Code: StudyErrorCodeStateStale}) {
		t.Fatal("errors.Is did not match equal study error codes")
	}
}

func TestStudyPrincipalIDRequiresTypedNonSecretDerivation(t *testing.T) {
	for _, credentialShaped := range []string{
		"node principal bearer material",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJmaXh0dXJlIn0.signature",
		"compact-bearer-token-value",
	} {
		if _, err := RestoreStudyPrincipalID(credentialShaped); err == nil {
			t.Fatalf("principal restoration accepted credential-shaped input")
		}
	}
	principal, err := NewStudyPrincipalID(
		StudyPrincipalNamespaceFixture,
		uuid.MustParse("11111111-1111-4111-8111-111111111111"),
	)
	if err != nil || !principal.Valid() || principal.String() != "fixture:11111111-1111-4111-8111-111111111111" {
		t.Fatalf("typed non-secret principal derivation failed")
	}
}

func TestStudyReceiptJSONOmitsInternalPrincipalAndOwnership(t *testing.T) {
	encoded, err := json.Marshal(StudyCreationReceipt{
		StudyPrincipalID:        OperatorStudyPrincipalID(),
		CreationRequestID:       uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		NormalizedRequestSHA256: strings.Repeat("1", 64),
		Disposition:             StudyCreationDispositionCreated,
		CanonicalStudySessionID: uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		LearnerProfileID:        uuid.MustParse("33333333-3333-4333-8333-333333333333"),
	})
	if err != nil {
		t.Fatalf("marshal creation receipt: %v", err)
	}
	for _, forbidden := range []string{"study_principal_id", "learner_profile_id", "scope", "operator"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("creation receipt exposed internal ownership field %q: %s", forbidden, encoded)
		}
	}

	eventEncoded, err := json.Marshal(StudyEventReceipt{
		ClientStreamID:                 uuid.MustParse("44444444-4444-4444-8444-444444444444"),
		ClientSequence:                 1,
		OriginatingStudyFlushReceiptID: uuid.MustParse("55555555-5555-4555-8555-555555555555"),
		LearnerProfileID:               uuid.MustParse("66666666-6666-4666-8666-666666666666"),
		StudySessionID:                 uuid.MustParse("77777777-7777-4777-8777-777777777777"),
		Disposition:                    StudyEventReceiptDispositionTerminalRejected,
		Code:                           StudyEventReceiptCodeTransitionInvalid,
	})
	if err != nil {
		t.Fatalf("marshal event receipt: %v", err)
	}
	for _, forbidden := range []string{
		"44444444-4444-4444-8444-444444444444",
		"55555555-5555-4555-8555-555555555555",
		"66666666-6666-4666-8666-666666666666",
		"77777777-7777-4777-8777-777777777777",
	} {
		if strings.Contains(string(eventEncoded), forbidden) {
			t.Fatalf("event receipt exposed internal ownership/batch value %q: %s", forbidden, eventEncoded)
		}
	}
}

func TestStudySchemaErrorRedactsInvalidReferenceCanary(t *testing.T) {
	const canary = "reference-canary-must-not-escape"
	_, err := NormalizeStudyScope(StudyScope{
		TopicIDs:  []uuid.UUID{uuid.MustParse("11111111-1111-1111-1111-111111111111")},
		TopicRefs: []string{canary + " whitespace"},
		CardKind:  StudyCardKindIdea,
	})
	assertStudyErrorRedactsCanary(t, err, canary)
}

func TestStudySchemaErrorRedactsProjectionContentCanary(t *testing.T) {
	const canary = "projection-content-canary-must-not-escape"
	item := validStudySessionItemFixture()
	item.FrontCanonicalName = strings.Repeat("f", 513) + canary
	err := ValidateStudySessionItem(item)
	assertStudyErrorRedactsCanary(t, err, canary)
}

func validStudyEventFixture() StudyEvent {
	return StudyEvent{
		ID:                 uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
		ClientSequence:     1,
		ProfileRef:         "household.primary",
		StudySessionItemID: uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
		CardIdentity:       uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"),
		ContentSHA256:      strings.Repeat("1", 64),
		Kind:               StudyEventKindPresented,
		ClientClockQuality: StudyClientClockQualityUnknown,
	}
}

func validStudySessionItemFixture() StudySessionItem {
	return StudySessionItem{
		ID:                       uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
		StudySessionID:           uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
		Position:                 0,
		CardKind:                 StudyCardKindIdea,
		CardIdentity:             uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"),
		HistoricalSourceKind:     "idea",
		HistoricalSourceIdentity: uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"),
		SourceContentRevision:    "fixture-revision",
		SourceContentSHA256:      strings.Repeat("1", 64),
		FrontCanonicalName:       "Fixture",
		BackDefinition:           "A deterministic study fixture.",
		SelectionReason: StudySelectionReason{
			RatingRank:         StudyRatingRankUnseenUnrated,
			TieBreakerSHA256:   strings.Repeat("2", 64),
			BoundaryAdjustment: StudyBoundaryAdjustmentNone,
		},
	}
}

func assertStudyErrorCode(t *testing.T, err error, want StudyErrorCode) {
	t.Helper()
	var studyErr *StudyError
	if !errors.As(err, &studyErr) {
		t.Fatalf("error type = %T, want *StudyError", err)
	}
	if studyErr.Code != want {
		t.Fatalf("error code = %s, want %s", studyErr.Code, want)
	}
}

func assertStudyErrorRedactsCanary(t *testing.T, err error, canary string) {
	t.Helper()
	var studyErr *StudyError
	if !errors.As(err, &studyErr) {
		t.Fatalf("error type = %T, want *StudyError", err)
	}
	encoded, marshalErr := json.Marshal(studyErr)
	if marshalErr != nil {
		t.Fatalf("marshal study error: %v", marshalErr)
	}
	if strings.Contains(studyErr.Error(), canary) || strings.Contains(string(encoded), canary) {
		t.Fatalf("study error exposed invalid-value canary: %s", encoded)
	}
}
