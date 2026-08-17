package studysession

import (
	"bytes"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

func TestCanonicalizeStudyRequestJSONMatchesRFC8785Vector(t *testing.T) {
	t.Parallel()
	input := []byte(`{"numbers":[333333333.33333329,1E30,4.50,2e-3,0.000000000000000000000000001],"string":"€$\u000f\nA'B\"\\\"/","literals":[null,true,false]}`)
	want := []byte("{\"literals\":[null,true,false],\"numbers\":[333333333.3333333,1e+30,4.5,0.002,1e-27],\"string\":\"€$\\u000f\\nA'B\\\"\\\\\\\"/\"}")
	got, err := CanonicalizeStudyRequestJSON(input)
	if err != nil {
		t.Fatalf("canonicalize RFC vector: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("RFC vector mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestCanonicalizeStudyRequestJSONNormalizesEquivalentBytes(t *testing.T) {
	t.Parallel()
	left, err := CanonicalizeStudyRequestJSON([]byte(` { "z": 1.0, "a": [true, null] } `))
	if err != nil {
		t.Fatalf("canonicalize left: %v", err)
	}
	right, err := CanonicalizeStudyRequestJSON([]byte(`{"a":[true,null],"z":1e0}`))
	if err != nil {
		t.Fatalf("canonicalize right: %v", err)
	}
	if !bytes.Equal(left, right) {
		t.Fatalf("equivalent JSON diverged: %s != %s", left, right)
	}
}

func TestCanonicalizeStudyRequestJSONRejectsDuplicateMembers(t *testing.T) {
	t.Parallel()
	if _, err := CanonicalizeStudyRequestJSON([]byte(`{"scope":{"card_kind":"idea","card_kind":"idea"}}`)); err == nil {
		t.Fatal("duplicate nested member was accepted")
	}
}

func TestNormalizeCreateStudyRequestExcludesIdempotencyUUIDAndDetectsMutation(t *testing.T) {
	t.Parallel()
	scope := mustNormalizedScope(t, []uuid.UUID{
		uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"),
		uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
	}, []string{"topic-b", "topic-a"})
	request := validCreateRequest()
	first, err := normalizeCreateStudyRequest(request, scope)
	if err != nil {
		t.Fatalf("normalize first request: %v", err)
	}
	request.CreationRequestID = uuid.MustParse("99999999-9999-4999-8999-999999999999")
	second, err := normalizeCreateStudyRequest(request, scope)
	if err != nil {
		t.Fatalf("normalize second request: %v", err)
	}
	if first != second {
		t.Fatalf("idempotency UUID entered content hash: %s != %s", first, second)
	}
	request.Page.Limit++
	mutated, err := normalizeCreateStudyRequest(request, scope)
	if err != nil {
		t.Fatalf("normalize mutated request: %v", err)
	}
	if mutated == first {
		t.Fatal("semantic page mutation retained request hash")
	}
}

func TestNormalizeSyncStudyRequestNormalizesTimeAndPreservesLargeSequence(t *testing.T) {
	t.Parallel()
	sessionID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	request := validSyncRequest()
	request.Events[0].ClientSequence = 9_007_199_254_740_993
	observed := time.Date(2026, 8, 17, 12, 0, 0, 123456789, time.FixedZone("offset", -7*60*60))
	request.Events[0].ClientObservedAt = &observed
	first, _, err := normalizeSyncStudyRequest(sessionID, request)
	if err != nil {
		t.Fatalf("normalize sync: %v", err)
	}
	request.SyncID = uuid.MustParse("99999999-9999-4999-8999-999999999999")
	second, _, err := normalizeSyncStudyRequest(sessionID, request)
	if err != nil {
		t.Fatalf("normalize sync replay: %v", err)
	}
	if first != second {
		t.Fatal("sync idempotency UUID entered content hash")
	}
	request.Events[0].ClientSequence++
	mutated, _, err := normalizeSyncStudyRequest(sessionID, request)
	if err != nil {
		t.Fatalf("normalize sync mutation: %v", err)
	}
	if mutated == first {
		t.Fatal("large integer sequence mutation collided")
	}
}

func TestNormalizeSyncStudyRequestPreservesMaxInt64AsIdentityString(t *testing.T) {
	t.Parallel()
	request := validSyncRequest()
	request.ExpectedStateVersion = math.MaxInt64
	request.Events[0].ClientSequence = math.MaxInt64
	digest, events, err := normalizeSyncStudyRequest(uuid.MustParse("11111111-1111-4111-8111-111111111111"), request)
	if err != nil {
		t.Fatalf("normalize MaxInt64 sync: %v", err)
	}
	if !model.IsStudySHA256(digest) || len(events) != 1 || events[0].ClientSequence != math.MaxInt64 {
		t.Fatalf("MaxInt64 normalization changed authority: digest=%q events=%#v", digest, events)
	}
}

func validCreateRequest() StudyCreateSessionRequest {
	return StudyCreateSessionRequest{
		ProtocolVersion:   model.StudyProtocolVersion,
		SchemaVersion:     model.StudySchemaVersion,
		CreationRequestID: uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
		ProfileRef:        "profile-a",
		Scope: StudyScopeRequest{
			TopicRefs: []string{"topic-a", "topic-b"},
			CardKind:  model.StudyCardKindIdea,
		},
		Page: StudyPageRequest{AfterPosition: -1, Limit: 16},
	}
}

func validSyncRequest() StudySyncRequest {
	return StudySyncRequest{
		ProtocolVersion:      model.StudyProtocolVersion,
		SchemaVersion:        model.StudySchemaVersion,
		SyncID:               uuid.MustParse("55555555-5555-4555-8555-555555555555"),
		ClientStreamID:       uuid.MustParse("66666666-6666-4666-8666-666666666666"),
		ExpectedStateVersion: 1,
		Events: []StudyEventRequest{{
			EventID:            uuid.MustParse("77777777-7777-4777-8777-777777777777"),
			ClientSequence:     1,
			ProfileRef:         "profile-a",
			SessionItemID:      uuid.MustParse("22222222-2222-4222-8222-222222222222"),
			CardIdentity:       uuid.MustParse("33333333-3333-4333-8333-333333333333"),
			ContentSHA256:      repeatedSHA('a'),
			Kind:               model.StudyEventKindPresented,
			ClientClockQuality: model.StudyClientClockQualityUnknown,
		}},
		Page: StudyPageRequest{AfterPosition: -1, Limit: 16},
	}
}

func mustNormalizedScope(t *testing.T, topicIDs []uuid.UUID, topicRefs []string) model.StudyNormalizedScope {
	t.Helper()
	scope, err := model.NormalizeStudyScope(model.StudyScope{
		TopicIDs:  topicIDs,
		TopicRefs: topicRefs,
		CardKind:  model.StudyCardKindIdea,
	})
	if err != nil {
		t.Fatalf("normalize fixture scope: %v", err)
	}
	return scope
}

func repeatedSHA(character byte) string {
	return string(bytes.Repeat([]byte{character}, 64))
}
