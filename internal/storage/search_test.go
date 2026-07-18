package storage

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"lexinet/internal/model"
	searchcfg "lexinet/internal/search"
)

func TestSearchQueryArgsUseEffectiveMatcherConfig(t *testing.T) {
	provider := searchcfg.NewStaticProvider(searchcfg.StrategyDailyDriver, map[searchcfg.StrategyName]searchcfg.Strategy{
		searchcfg.StrategyDailyDriver: {
			Name:      searchcfg.StrategyDailyDriver,
			Fuzzy:     true,
			Threshold: 0.41,
			Matchers: []searchcfg.MatcherConfig{
				{Name: searchcfg.MatcherExact, Enabled: false, Weight: 3.0, Priority: 9},
				{Name: searchcfg.MatcherPrefix, Enabled: true, Weight: 2.2, Priority: 8},
				{Name: searchcfg.MatcherFullText, Enabled: false, Weight: 1.1, Priority: 7},
				{Name: searchcfg.MatcherFuzzyName, Enabled: true, Weight: 0.44, Threshold: 0.37, Priority: 6},
				{Name: searchcfg.MatcherFuzzyBody, Enabled: false, Weight: 0.33, Threshold: 0.52, Priority: 5},
			},
		},
	}, 20)
	input, err := model.NormalizeSearchInputWithProvider(model.SearchInput{
		Query:  " rest ",
		Scope:  model.SearchScopeNames,
		Kinds:  []model.NameKind{model.NameKindCanonical, model.NameKindAcronym},
		Limit:  5,
		Offset: 2,
	}, provider)
	if err != nil {
		t.Fatalf("normalize search input: %v", err)
	}

	args := searchQueryArgs(input)
	if len(args) != 22 {
		t.Fatalf("arg count = %d, want 22", len(args))
	}
	assertComparableArg(t, args, 0, "rest")
	assertComparableArg(t, args, 1, false)
	assertComparableArg(t, args, 2, 5)
	assertComparableArg(t, args, 3, 2)
	assertStringSliceArg(t, args, 4, []string{"canonical", "acronym"})
	assertComparableArg(t, args, 5, false)
	assertComparableArg(t, args, 6, 3.0)
	assertComparableArg(t, args, 7, 9)
	assertComparableArg(t, args, 8, true)
	assertComparableArg(t, args, 9, 2.2)
	assertComparableArg(t, args, 10, 8)
	assertComparableArg(t, args, 11, false)
	assertComparableArg(t, args, 12, 1.1)
	assertComparableArg(t, args, 13, 7)
	assertComparableArg(t, args, 14, true)
	assertComparableArg(t, args, 15, 0.44)
	assertComparableArg(t, args, 16, 6)
	assertComparableArg(t, args, 17, false)
	assertComparableArg(t, args, 18, 0.33)
	assertComparableArg(t, args, 19, 5)
	assertComparableArg(t, args, 20, 0.37)
	assertComparableArg(t, args, 21, 0.52)
}

func TestSearchSQLUsesMatcherThresholdsWithoutGlobalFuzzyGate(t *testing.T) {
	if strings.Contains(searchSQL, " AS fuzzy,") || strings.Contains(searchSQL, "p.fuzzy AND") || strings.Contains(searchSQL, "% p.q") {
		t.Fatalf("search SQL should use effective matcher flags and thresholds without global fuzzy gates")
	}
	for _, want := range []string{"p.fuzzy_name_enabled", "p.fuzzy_name_threshold", "p.fuzzy_body_enabled", "p.fuzzy_body_threshold"} {
		if !strings.Contains(searchSQL, want) {
			t.Fatalf("search SQL missing %s", want)
		}
	}
}

func TestScanSearchResultPreservesFuzzyMatcherProvenance(t *testing.T) {
	ideaID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	row := fakeSearchRow{scan: func(dest ...any) error {
		*(dest[0].(*int64)) = 1
		*(dest[1].(*uuid.UUID)) = ideaID
		*(dest[2].(*pgtype.Text)) = pgtype.Text{String: "redis", Valid: true}
		*(dest[3].(*pgtype.Text)) = pgtype.Text{String: "Redis", Valid: true}
		*(dest[4].(*pgtype.Text)) = pgtype.Text{String: "In-memory data store", Valid: true}
		*(dest[5].(*float64)) = 0.24
		*(dest[6].(*string)) = "text"
		*(dest[7].(*string)) = "definition"
		*(dest[8].(*pgtype.Text)) = pgtype.Text{}
		*(dest[9].(*pgtype.Text)) = pgtype.Text{}
		*(dest[10].(*string)) = "fuzzy"
		*(dest[11].(*string)) = string(searchcfg.MatcherFuzzyBody)
		*(dest[12].(*pgtype.Float8)) = pgtype.Float8{Float64: 0.8, Valid: true}
		*(dest[13].(*pgtype.Text)) = pgtype.Text{String: "fast <b>memory</b> store", Valid: true}
		return nil
	}}

	result, total, err := scanSearchResult(row)
	if err != nil {
		t.Fatalf("scan search result: %v", err)
	}
	if total != 1 || result.MatchType != model.SearchMatchFuzzy || result.Matcher != searchcfg.MatcherFuzzyBody {
		t.Fatalf("unexpected scan result total=%d match_type=%q matcher=%q", total, result.MatchType, result.Matcher)
	}
	input, err := model.NormalizeSearchInput(model.SearchInput{Query: "memory"})
	if err != nil {
		t.Fatalf("normalize search input: %v", err)
	}
	provenance := model.SearchResultProvenance(input, *result)
	if provenance.Matcher != searchcfg.MatcherFuzzyBody || provenance.Weight != 0.3 || provenance.Threshold != 0.3 || provenance.Priority != 4 {
		t.Fatalf("unexpected provenance: %#v", provenance)
	}
}

func assertComparableArg[T comparable](t *testing.T, args []any, index int, want T) {
	t.Helper()
	got, ok := args[index].(T)
	if !ok {
		t.Fatalf("arg %d type = %T, want %T", index, args[index], want)
	}
	if got != want {
		t.Fatalf("arg %d = %#v, want %#v", index, got, want)
	}
}

func assertStringSliceArg(t *testing.T, args []any, index int, want []string) {
	t.Helper()
	got, ok := args[index].([]string)
	if !ok {
		t.Fatalf("arg %d type = %T, want []string", index, args[index])
	}
	if len(got) != len(want) {
		t.Fatalf("arg %d = %#v, want %#v", index, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d = %#v, want %#v", index, got, want)
		}
	}
}

type fakeSearchRow struct {
	scan func(dest ...any) error
}

func (r fakeSearchRow) Scan(dest ...any) error {
	return r.scan(dest...)
}
