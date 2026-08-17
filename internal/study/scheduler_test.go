package study

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

var (
	testProfileID = uuid.MustParse("10000000-0000-4000-8000-000000000001")
	testCardOne   = uuid.MustParse("00000000-0000-4000-8000-000000000001")
	testCardTwo   = uuid.MustParse("00000000-0000-4000-8000-000000000002")
	testCardThree = uuid.MustParse("00000000-0000-4000-8000-000000000003")
)

func TestPlanLexinetStep3Verify1Item1FixedClockSeedTestsProveStablePlansAnd(t *testing.T) {
	t.Parallel()

	scheduler := mustVarietyFirstV1(t)
	plannedAt := time.Date(2026, time.August, 17, 8, 30, 0, 123456789, time.FixedZone("fixture", -7*60*60))
	good := model.StudyRatingGood
	presented := int64(11)
	tail := testCardTwo
	snapshot := testSchedulingSnapshot(
		StudySchedulingCard{CardIdentity: testCardOne},
		StudySchedulingCard{CardIdentity: testCardTwo},
		StudySchedulingCard{
			CardIdentity: testCardThree,
			Projection: StudyProjectionTail{
				LatestEffectiveRating:           &good,
				LatestPresentedIngestionOrdinal: &presented,
			},
		},
	)
	snapshot.PlannedAt = plannedAt
	snapshot.PredecessorTailCardIdentity = &tail
	before := cloneSchedulingSnapshot(snapshot)

	first, err := scheduler.Plan(snapshot)
	if err != nil {
		t.Fatalf("plan fixed snapshot: %v", err)
	}
	if !reflect.DeepEqual(snapshot, before) {
		t.Fatal("scheduler mutated its input snapshot")
	}

	reordered := cloneSchedulingSnapshot(snapshot)
	reordered.Cards[0], reordered.Cards[2] = reordered.Cards[2], reordered.Cards[0]
	second, err := scheduler.Plan(reordered)
	if err != nil {
		t.Fatalf("plan reordered snapshot: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("input order changed plan:\nfirst  %#v\nsecond %#v", first, second)
	}

	if first.Scheduler != (model.StudySchedulerProvenance{
		Name:                           VarietyFirstV1Name,
		Version:                        VarietyFirstV1Version,
		ConfigurationSHA256:            "6772f65e0a6e872e8c2945cff325922331db9029c189a0be2e63c990d5bb82fc",
		InputProfileIngestionHighWater: 41,
		PlannedAt:                      plannedAt.UTC(),
		Seed:                           "fixed-seed",
	}) {
		t.Fatalf("scheduler provenance = %#v", first.Scheduler)
	}
	if first.CorpusSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("corpus digest = %q", first.CorpusSHA256)
	}
	wantOrder := []uuid.UUID{testCardOne, testCardTwo, testCardThree}
	if got := plannedCardIdentities(first); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("fixed order = %v, want %v", got, wantOrder)
	}
	if first.Cards[0].SelectionReason.BoundaryAdjustment != model.StudyBoundaryAdjustmentRotated {
		t.Fatalf("new head boundary reason = %q", first.Cards[0].SelectionReason.BoundaryAdjustment)
	}
	if first.Cards[1].SelectionReason.BoundaryAdjustment != model.StudyBoundaryAdjustmentNone ||
		first.Cards[2].SelectionReason.BoundaryAdjustment != model.StudyBoundaryAdjustmentNone {
		t.Fatal("boundary adjustment leaked beyond the new head")
	}
	wantTies := map[uuid.UUID]string{
		testCardOne:   "b8a82d752c839ddb6056cc594f6720845f6a57fd7cb26c1ccb5b6fb91bae5892",
		testCardTwo:   "9f2e5a341d009df1f7c821e579c176c8a939c697d665481974c038f5d9a91598",
		testCardThree: "18210fd901baf802a042cc8c2c53568ecfb0c2bd32328d8b7958414aa291b176",
	}
	for _, card := range first.Cards {
		if card.SelectionReason.TieBreakerSHA256 != wantTies[card.CardIdentity] {
			t.Fatalf("tie digest for planned card is not stable")
		}
	}
}

func TestSchedulerSnapshotValidationIsStructuredAndRedacted(t *testing.T) {
	t.Parallel()

	scheduler := mustVarietyFirstV1(t)
	invalidRating := model.StudyRating("private-rating-canary")
	testCases := []struct {
		name     string
		mutate   func(*StudySchedulingSnapshot)
		wantCode model.StudyErrorCode
	}{
		{name: "profile", mutate: func(snapshot *StudySchedulingSnapshot) { snapshot.LearnerProfileID = uuid.Nil }, wantCode: model.StudyErrorCodeSchemaInvalid},
		{name: "card kind", mutate: func(snapshot *StudySchedulingSnapshot) { snapshot.CardKind = "invalid" }, wantCode: model.StudyErrorCodeSchemaInvalid},
		{name: "corpus", mutate: func(snapshot *StudySchedulingSnapshot) { snapshot.CorpusSHA256 = "private-digest-canary" }, wantCode: model.StudyErrorCodeSchemaInvalid},
		{name: "high water", mutate: func(snapshot *StudySchedulingSnapshot) { snapshot.InputProfileIngestionHighWater = -1 }, wantCode: model.StudyErrorCodeSchemaInvalid},
		{name: "planned at", mutate: func(snapshot *StudySchedulingSnapshot) { snapshot.PlannedAt = time.Time{} }, wantCode: model.StudyErrorCodeSchemaInvalid},
		{name: "seed", mutate: func(snapshot *StudySchedulingSnapshot) { snapshot.Seed = string([]byte{0xff, 0xfe}) }, wantCode: model.StudyErrorCodeSchemaInvalid},
		{name: "tail", mutate: func(snapshot *StudySchedulingSnapshot) {
			zero := uuid.Nil
			snapshot.PredecessorTailCardIdentity = &zero
		}, wantCode: model.StudyErrorCodeSchemaInvalid},
		{name: "card", mutate: func(snapshot *StudySchedulingSnapshot) { snapshot.Cards[0].CardIdentity = uuid.Nil }, wantCode: model.StudyErrorCodeSchemaInvalid},
		{name: "duplicate card", mutate: func(snapshot *StudySchedulingSnapshot) { snapshot.Cards = append(snapshot.Cards, snapshot.Cards[0]) }, wantCode: model.StudyErrorCodeSchemaInvalid},
		{name: "rating", mutate: func(snapshot *StudySchedulingSnapshot) {
			snapshot.Cards[0].Projection.LatestEffectiveRating = &invalidRating
		}, wantCode: model.StudyErrorCodeInternal},
		{name: "ordinal", mutate: func(snapshot *StudySchedulingSnapshot) {
			invalid := int64(0)
			snapshot.Cards[0].Projection.LatestPresentedIngestionOrdinal = &invalid
		}, wantCode: model.StudyErrorCodeInternal},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			snapshot := testSchedulingSnapshot(StudySchedulingCard{CardIdentity: testCardOne})
			testCase.mutate(&snapshot)
			_, err := scheduler.Plan(snapshot)
			var studyError *model.StudyError
			if !errors.As(err, &studyError) || studyError.Code != testCase.wantCode {
				t.Fatalf("error = %#v, want code %s", err, testCase.wantCode)
			}
			serialized := err.Error()
			for _, canary := range []string{"private-rating-canary", "private-digest-canary", testCardOne.String()} {
				if strings.Contains(serialized, canary) {
					t.Fatalf("error exposed supplied value: %q", serialized)
				}
			}
		})
	}
}

func testSchedulingSnapshot(cards ...StudySchedulingCard) StudySchedulingSnapshot {
	return StudySchedulingSnapshot{
		LearnerProfileID:               testProfileID,
		CardKind:                       model.StudyCardKindIdea,
		CorpusSHA256:                   strings.Repeat("a", 64),
		InputProfileIngestionHighWater: 41,
		PlannedAt:                      time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC),
		Seed:                           "fixed-seed",
		Cards:                          cards,
	}
}

func mustVarietyFirstV1(t *testing.T) *VarietyFirstV1 {
	t.Helper()
	scheduler, err := NewVarietyFirstV1()
	if err != nil {
		t.Fatalf("construct variety-first scheduler: %v", err)
	}
	return scheduler
}

func cloneSchedulingSnapshot(snapshot StudySchedulingSnapshot) StudySchedulingSnapshot {
	clone := snapshot
	clone.Cards = append([]StudySchedulingCard(nil), snapshot.Cards...)
	if snapshot.PredecessorTailCardIdentity != nil {
		copyOfTail := *snapshot.PredecessorTailCardIdentity
		clone.PredecessorTailCardIdentity = &copyOfTail
	}
	for index := range clone.Cards {
		if clone.Cards[index].Projection.LatestEffectiveRating != nil {
			copyOfRating := *clone.Cards[index].Projection.LatestEffectiveRating
			clone.Cards[index].Projection.LatestEffectiveRating = &copyOfRating
		}
		if clone.Cards[index].Projection.LatestPresentedIngestionOrdinal != nil {
			copyOfOrdinal := *clone.Cards[index].Projection.LatestPresentedIngestionOrdinal
			clone.Cards[index].Projection.LatestPresentedIngestionOrdinal = &copyOfOrdinal
		}
	}
	return clone
}

func plannedCardIdentities(plan StudyPlan) []uuid.UUID {
	identities := make([]uuid.UUID, len(plan.Cards))
	for index := range plan.Cards {
		identities[index] = plan.Cards[index].CardIdentity
	}
	return identities
}
