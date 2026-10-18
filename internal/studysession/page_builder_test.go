package studysession

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"lexinet/internal/model"
	studydigest "lexinet/internal/study/digest"
)

func TestBuildStudySessionPageAppliesCountAndPositionPrefix(t *testing.T) {
	t.Parallel()
	items := fixtureSessionItems(4, 32)
	page, artifact, err := BuildStudySessionPage(items, StudyPageRequest{AfterPosition: 0, Limit: 2})
	if err != nil {
		t.Fatalf("build page: %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].Position != 1 || page.Items[1].Position != 2 ||
		page.NextAfterPosition != 2 || page.Complete {
		t.Fatalf("unexpected page: %#v", page)
	}
	if len(artifact.CanonicalJSON) > StudyMaximumPageJSONBytes ||
		artifact.CanonicalSHA256 != model.SHA256Hex(artifact.CanonicalJSON) {
		t.Fatalf("invalid artifact: %#v", artifact)
	}
	if err := model.ValidateStudySessionPageArtifact(artifact); err != nil {
		t.Fatalf("model rejected built artifact: %v", err)
	}
}

func TestBuildStudySessionPageAppliesTypedByteBudget(t *testing.T) {
	t.Parallel()
	items := fixtureSessionItems(64, 16_000)
	page, artifact, err := BuildStudySessionPage(items, StudyPageRequest{AfterPosition: -1, Limit: 64})
	if err != nil {
		t.Fatalf("build byte-bounded page: %v", err)
	}
	if len(page.Items) == 0 || len(page.Items) >= 64 || page.Complete {
		t.Fatalf("byte budget did not produce a strict nonempty prefix: items=%d complete=%v", len(page.Items), page.Complete)
	}
	if len(artifact.CanonicalJSON) > StudyMaximumPageJSONBytes {
		t.Fatalf("page bytes = %d", len(artifact.CanonicalJSON))
	}
	if err := model.ValidateStudySessionPageArtifact(artifact); err != nil {
		t.Fatalf("model rejected byte-bounded artifact: %v", err)
	}
}

func TestDecodeStudySessionPageArtifactAllowsOnlyLiveSourceLinkDrift(t *testing.T) {
	t.Parallel()
	items := fixtureSessionItems(1, 32)
	_, artifact, err := BuildStudySessionPage(items, StudyPageRequest{AfterPosition: -1, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	repointed := fixtureSessionItems(1, 32)
	repointedID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	repointed[0].LiveSourceIdeaID = &repointedID
	if _, err := decodeStudySessionPageArtifact(artifact, StudyPageRequest{AfterPosition: -1, Limit: 1}, repointed); err != nil {
		t.Fatalf("merge-repointed live link rejected: %v", err)
	}
	deleted := fixtureSessionItems(1, 32)
	deleted[0].LiveSourceIdeaID = nil
	if _, err := decodeStudySessionPageArtifact(artifact, StudyPageRequest{AfterPosition: -1, Limit: 1}, deleted); err != nil {
		t.Fatalf("hard-deleted live link rejected: %v", err)
	}
	mutated := fixtureSessionItems(1, 32)
	mutated[0].FrontCanonicalName = "Substituted immutable content"
	if _, err := decodeStudySessionPageArtifact(artifact, StudyPageRequest{AfterPosition: -1, Limit: 1}, mutated); err == nil {
		t.Fatal("immutable page mutation was accepted")
	}
}

func TestDecodeStudySessionPageArtifactRejectsMutation(t *testing.T) {
	t.Parallel()
	items := fixtureSessionItems(2, 32)
	_, artifact, err := BuildStudySessionPage(items, StudyPageRequest{AfterPosition: -1, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	artifact.CanonicalJSON[10] ^= 1
	if _, err := decodeStudySessionPageArtifact(artifact, StudyPageRequest{AfterPosition: -1, Limit: 2}, items); err == nil {
		t.Fatal("mutated artifact was accepted")
	}
}

func TestMarshalBoundedStudyResponseReturnsInternalForGeneratedOverflow(t *testing.T) {
	t.Parallel()
	_, err := marshalBoundedStudyResponse(map[string]string{"generated": strings.Repeat("x", StudyMaximumResponseBodyBytes)})
	if !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeInternal}) {
		t.Fatalf("overflow error = %v", err)
	}
}

func TestMarshalBoundedStudyResponseContainsExactPageBytes(t *testing.T) {
	t.Parallel()
	items := fixtureSessionItems(1, 32)
	page, artifact, err := BuildStudySessionPage(items, StudyPageRequest{AfterPosition: -1, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	response, err := marshalBoundedStudyResponse(struct {
		Page StudySessionPageResource `json:"page"`
	}{Page: page})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(response, artifact.CanonicalJSON) {
		t.Fatalf("response does not embed exact page bytes: %s", response)
	}
}

func TestTypedPageAndNestedResponsePreserveMaxInt64AndHTMLEscaping(t *testing.T) {
	t.Parallel()
	items := fixtureSessionItems(1, 32)
	maximum := int64(math.MaxInt64)
	items[0].SelectionReason.LatestPresentedIngestionOrdinal = &maximum
	items[0].FrontCanonicalName = "<Fixture & concept>"
	contentSHA256, err := studydigest.CardContent(
		[]byte(items[0].CardKind),
		[]byte(items[0].CardIdentity.String()),
		[]byte(items[0].HistoricalSourceIdentity.String()),
		[]byte(items[0].FrontCanonicalName),
		[]byte(items[0].BackDefinition),
		[]byte{0},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	items[0].SourceContentSHA256 = contentSHA256
	page, artifact, err := BuildStudySessionPage(items, StudyPageRequest{AfterPosition: -1, Limit: 1})
	if err != nil {
		t.Fatalf("build max-int page: %v", err)
	}
	if !bytes.Contains(artifact.CanonicalJSON, []byte(`"latest_presented_ingestion_ordinal":9223372036854775807`)) ||
		!bytes.Contains(artifact.CanonicalJSON, []byte(`"canonical_name":"\u003cFixture \u0026 concept\u003e"`)) {
		t.Fatalf("typed page lost integer or HTML escaping: %s", artifact.CanonicalJSON)
	}
	decoded, err := decodeStudySessionPageArtifact(artifact, StudyPageRequest{AfterPosition: -1, Limit: 1}, items)
	if err != nil {
		t.Fatalf("replay max-int page: %v", err)
	}
	if decoded.Items[0].SelectionReason.LatestPresentedIngestionOrdinal == nil ||
		*decoded.Items[0].SelectionReason.LatestPresentedIngestionOrdinal != math.MaxInt64 {
		t.Fatalf("decoded ordinal = %#v", decoded.Items[0].SelectionReason.LatestPresentedIngestionOrdinal)
	}
	response, err := marshalBoundedStudyResponse(struct {
		Page         StudySessionPageResource `json:"page"`
		StateVersion int64                    `json:"state_version"`
		HighWater    int64                    `json:"profile_ingestion_high_water"`
	}{Page: page, StateVersion: math.MaxInt64, HighWater: math.MaxInt64})
	if err != nil {
		t.Fatalf("marshal max-int response: %v", err)
	}
	if !bytes.Contains(response, artifact.CanonicalJSON) ||
		bytes.Count(response, []byte("9223372036854775807")) != 3 {
		t.Fatalf("nested response rounded or rewrote page: %s", response)
	}
}

func fixtureSessionItems(count, definitionRunes int) []model.StudySessionItem {
	sessionID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	items := make([]model.StudySessionItem, count)
	for index := range items {
		itemID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("item-"+string(rune(index))))
		cardID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("card-"+string(rune(index))))
		items[index] = model.StudySessionItem{
			ID:                       itemID,
			StudySessionID:           sessionID,
			Position:                 index,
			CardKind:                 model.StudyCardKindIdea,
			CardIdentity:             cardID,
			HistoricalSourceKind:     string(model.StudyCardKindIdea),
			HistoricalSourceIdentity: cardID,
			LiveSourceIdeaID:         cloneUUIDPointer(&cardID),
			SourceContentRevision:    "revision-1",
			SourceContentSHA256:      repeatedSHA('a'),
			FrontCanonicalName:       "Fixture",
			BackDefinition:           strings.Repeat("d", definitionRunes),
			SelectionReason: model.StudySelectionReason{
				RatingRank:         model.StudyRatingRankUnseenUnrated,
				TieBreakerSHA256:   repeatedSHA('b'),
				BoundaryAdjustment: model.StudyBoundaryAdjustmentNone,
			},
			CreatedAt: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
		}
		contentSHA256, err := studydigest.CardContent(
			[]byte(items[index].CardKind),
			[]byte(items[index].CardIdentity.String()),
			[]byte(items[index].HistoricalSourceIdentity.String()),
			[]byte(items[index].FrontCanonicalName),
			[]byte(items[index].BackDefinition),
			[]byte{0},
			nil,
		)
		if err != nil {
			panic(err)
		}
		items[index].SourceContentSHA256 = contentSHA256
	}
	return items
}
