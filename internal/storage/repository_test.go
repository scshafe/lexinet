package storage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"lexinet/internal/model"
	searchcfg "lexinet/internal/search"
)

func TestRepositorySearchStrategyProviderOption(t *testing.T) {
	provider := searchcfg.NewStaticProvider(searchcfg.StrategyLegacy, nil, 7)
	repo := NewRepository(nil, WithSearchStrategyProvider(provider))
	strategy, err := repo.searchStrategies.ResolveStrategy("", searchcfg.Overrides{})
	if err != nil {
		t.Fatalf("resolve strategy: %v", err)
	}
	if strategy.Name != searchcfg.StrategyLegacy || strategy.Fuzzy {
		t.Fatalf("repository provider strategy = %#v, want legacy fuzzy off", strategy)
	}
	if repo.searchStrategies.DefaultLimit() != 7 {
		t.Fatalf("default limit = %d, want 7", repo.searchStrategies.DefaultLimit())
	}
}

func TestUniqueIdeaCandidateDedupeAndAmbiguity(t *testing.T) {
	id1 := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	id2 := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	got, err := uniqueIdeaCandidate("same-ref", []uuid.UUID{id1, id1})
	if err != nil {
		t.Fatalf("expected duplicate candidate to resolve, got %v", err)
	}
	if got != id1 {
		t.Fatalf("expected %s, got %s", id1, got)
	}

	_, err = uniqueIdeaCandidate("ambiguous-ref", []uuid.UUID{id2, id1})
	var ambiguous *AmbiguousIdeaRefError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("expected AmbiguousIdeaRefError, got %T %v", err, err)
	}
	if !errors.Is(err, ErrIdeaRefAmbiguous) {
		t.Fatalf("expected ErrIdeaRefAmbiguous unwrap, got %v", err)
	}
	if len(ambiguous.Candidates) != 2 || ambiguous.Candidates[0] != id1 || ambiguous.Candidates[1] != id2 {
		t.Fatalf("expected sorted candidates [%s %s], got %v", id1, id2, ambiguous.Candidates)
	}
}

func TestUniqueIdeaCandidateNotFound(t *testing.T) {
	_, err := uniqueIdeaCandidate("missing", nil)
	if !errors.Is(err, ErrIdeaNotFound) {
		t.Fatalf("expected ErrIdeaNotFound, got %v", err)
	}
}

func TestLooksLikeUUIDPrefix(t *testing.T) {
	cases := []struct {
		ref  string
		want bool
	}{
		{"11111111-1111", true},
		{"ABCDEF", true},
		{"grpc", false},
		{"bad--dash", false},
		{"11111111-1111-1111-1111-111111111111-extra", false},
	}
	for _, tc := range cases {
		if got := looksLikeUUIDPrefix(tc.ref); got != tc.want {
			t.Fatalf("looksLikeUUIDPrefix(%q) = %v, want %v", tc.ref, got, tc.want)
		}
	}
}

func TestChooseCanonicalName(t *testing.T) {
	names := []model.CreateNameInput{
		{Text: "REST", Kind: model.NameKindAcronym},
		{Text: "Representational State Transfer", Kind: model.NameKindCanonical},
	}
	if got := chooseCanonicalName(names); got != 1 {
		t.Fatalf("expected explicit canonical at index 1, got %d", got)
	}
	if got := chooseCanonicalName([]model.CreateNameInput{{Text: "first"}, {Text: "second"}}); got != 0 {
		t.Fatalf("expected fallback canonical at index 0, got %d", got)
	}
}

func TestNormalizeNameInputClearsKindSpecificFields(t *testing.T) {
	expansion := "Representational State Transfer"
	langTag := "en"
	deprecationNote := "use REST instead"
	input := model.CreateNameInput{
		Text:            "REST",
		Kind:            model.NameKindAlias,
		Expansion:       &expansion,
		LangTag:         &langTag,
		DeprecationNote: &deprecationNote,
	}
	normalizeNameInput(&input)
	if input.Expansion != nil || input.LangTag != nil || input.DeprecationNote != nil {
		t.Fatalf("expected alias to clear kind-specific fields, got %#v", input)
	}
}

func TestResolveIdeaIDDedupesNameAndSlugCandidates(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	runner := &fakeRunner{
		nameIDs: []uuid.UUID{id},
		slugIDs: []uuid.UUID{id},
		ideas: map[uuid.UUID]fakeIdeaState{
			id: {status: string(model.IdeaStatusActive)},
		},
	}

	got, err := resolveIdeaID(context.Background(), runner, "rest")
	if err != nil {
		t.Fatalf("expected ref to resolve, got %v", err)
	}
	if got != id {
		t.Fatalf("expected %s, got %s", id, got)
	}
}

func TestResolveIdeaIDDetectsNameSlugAmbiguity(t *testing.T) {
	nameID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	slugID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	runner := &fakeRunner{nameIDs: []uuid.UUID{nameID}, slugIDs: []uuid.UUID{slugID}}

	_, err := resolveIdeaID(context.Background(), runner, "shared-ref")
	if !errors.Is(err, ErrIdeaRefAmbiguous) {
		t.Fatalf("expected ambiguous ref, got %v", err)
	}
}

func TestResolveIdeaIDFollowsMergedIdeas(t *testing.T) {
	mergedID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	survivorID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	runner := &fakeRunner{
		nameIDs: []uuid.UUID{mergedID},
		ideas: map[uuid.UUID]fakeIdeaState{
			mergedID:   {status: string(model.IdeaStatusMerged), mergedInto: survivorID},
			survivorID: {status: string(model.IdeaStatusActive)},
		},
	}

	got, err := resolveIdeaID(context.Background(), runner, "old-name")
	if err != nil {
		t.Fatalf("expected merged ref to resolve, got %v", err)
	}
	if got != survivorID {
		t.Fatalf("expected survivor %s, got %s", survivorID, got)
	}
}

func TestRelationshipHelpersTreatSymmetricEdgeAsAdjacentBothWays(t *testing.T) {
	from := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	to := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	relationship := model.Relationship{FromIdeaID: from, ToIdeaID: to, Kind: model.RelationSynonymOf}

	neighbor, ok := relationshipNeighborID(relationship, to)
	if !ok || neighbor != from {
		t.Fatalf("expected symmetric stored edge to be adjacent from to-side, got %s ok=%v", neighbor, ok)
	}
	if label := relationshipLabelForIdea(relationship, to); label != model.RelationSynonymOf {
		t.Fatalf("expected symmetric label to stay synonym_of, got %q", label)
	}
}

func TestRelationshipLabelForIncomingDirectedEdgeUsesReverseLabel(t *testing.T) {
	specialization := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	general := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	relationship := model.Relationship{FromIdeaID: specialization, ToIdeaID: general, Kind: model.RelationIsA}

	if label := relationshipLabelForIdea(relationship, general); label != model.RelationGeneralizationOf {
		t.Fatalf("expected incoming is_a label generalization_of, got %q", label)
	}
	if label := relationshipLabelForIdea(relationship, specialization); label != model.RelationIsA {
		t.Fatalf("expected outgoing label is_a, got %q", label)
	}
}

type fakeRunner struct {
	prefixIDs []uuid.UUID
	nameIDs   []uuid.UUID
	slugIDs   []uuid.UUID
	ideas     map[uuid.UUID]fakeIdeaState
}

type fakeIdeaState struct {
	status     string
	mergedInto uuid.UUID
}

func (r *fakeRunner) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (r *fakeRunner) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "left(lower(id::text)"):
		return &fakeUUIDRows{ids: r.prefixIDs}, nil
	case strings.Contains(sql, "FROM names"):
		return &fakeUUIDRows{ids: r.nameIDs}, nil
	case strings.Contains(sql, "WHERE slug"):
		return &fakeUUIDRows{ids: r.slugIDs}, nil
	default:
		return &fakeUUIDRows{}, nil
	}
}

func (r *fakeRunner) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	id, _ := args[0].(uuid.UUID)
	state, ok := r.ideas[id]
	if !ok {
		return fakeIdeaRow{err: pgx.ErrNoRows}
	}
	return fakeIdeaRow{state: state}
}

type fakeUUIDRows struct {
	ids []uuid.UUID
	i   int
	err error
}

func (r *fakeUUIDRows) Close()                                       {}
func (r *fakeUUIDRows) Err() error                                   { return r.err }
func (r *fakeUUIDRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeUUIDRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeUUIDRows) Values() ([]any, error)                       { return []any{r.ids[r.i-1]}, nil }
func (r *fakeUUIDRows) RawValues() [][]byte                          { return nil }
func (r *fakeUUIDRows) Conn() *pgx.Conn                              { return nil }
func (r *fakeUUIDRows) Next() bool {
	if r.i >= len(r.ids) {
		return false
	}
	r.i++
	return true
}
func (r *fakeUUIDRows) Scan(dest ...any) error {
	*(dest[0].(*uuid.UUID)) = r.ids[r.i-1]
	return nil
}

type fakeIdeaRow struct {
	state fakeIdeaState
	err   error
}

func (r fakeIdeaRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*string)) = r.state.status
	mergedInto := dest[1].(*pgtype.UUID)
	if r.state.mergedInto != uuid.Nil {
		mergedInto.Bytes = r.state.mergedInto
		mergedInto.Valid = true
	}
	return nil
}
