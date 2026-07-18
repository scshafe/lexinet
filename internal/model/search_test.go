package model

import (
	"encoding/json"
	"testing"

	searchcfg "lexinet/internal/search"
)

func TestNormalizeSearchInputDefaultsToDailyDriverWithFuzzy(t *testing.T) {
	input, err := NormalizeSearchInput(SearchInput{Query: "  in-memory cache  "})
	if err != nil {
		t.Fatalf("expected valid search input, got %v", err)
	}
	if input.Query != "in-memory cache" {
		t.Fatalf("expected trimmed query, got %q", input.Query)
	}
	if input.Scope != SearchScopeAll {
		t.Fatalf("expected all scope by default, got %q", input.Scope)
	}
	if input.Strategy != searchcfg.StrategyDailyDriver {
		t.Fatalf("expected daily-driver strategy by default, got %q", input.Strategy)
	}
	if !input.Fuzzy {
		t.Fatalf("expected fuzzy enabled by default")
	}
	if input.Threshold != 0.3 || input.Limit != 20 || input.Offset != 0 {
		t.Fatalf("unexpected defaults: %#v", input)
	}
}

func TestNormalizeSearchInputLegacyDefaultsToFuzzyOff(t *testing.T) {
	input, err := NormalizeSearchInput(SearchInput{Query: "cache", Strategy: searchcfg.StrategyLegacy})
	if err != nil {
		t.Fatalf("expected valid legacy search input, got %v", err)
	}
	if input.Strategy != searchcfg.StrategyLegacy {
		t.Fatalf("strategy = %q, want legacy", input.Strategy)
	}
	if input.Fuzzy {
		t.Fatalf("expected legacy fuzzy default to be off")
	}
}

func TestNormalizeSearchInputUsesStrategyProviderDefaults(t *testing.T) {
	provider := searchcfg.NewStaticProvider(searchcfg.StrategyLegacy, map[searchcfg.StrategyName]searchcfg.Strategy{
		searchcfg.StrategyLegacy: {
			Name:      searchcfg.StrategyLegacy,
			Fuzzy:     false,
			Threshold: 0.45,
			Matchers: []searchcfg.MatcherConfig{
				{Name: searchcfg.MatcherExact, Enabled: true, Weight: 0.9, Priority: 4},
				{Name: searchcfg.MatcherFuzzyName, Enabled: false, Weight: 0.2, Threshold: 0.45, Priority: 9},
			},
		},
	}, 7)
	input, err := NormalizeSearchInputWithProvider(SearchInput{Query: "cache"}, provider)
	if err != nil {
		t.Fatalf("normalize with provider: %v", err)
	}
	if input.Strategy != searchcfg.StrategyLegacy || input.Fuzzy || input.Threshold != 0.45 || input.Limit != 7 {
		t.Fatalf("unexpected provider defaults: %#v", input)
	}
	matcher, ok := searchcfg.MatcherByName(input.StrategyConfig, searchcfg.MatcherExact)
	if !ok || matcher.Weight != 0.9 || matcher.Priority != 4 {
		t.Fatalf("exact matcher = %#v, ok=%v", matcher, ok)
	}
}

func TestNormalizeSearchInputRejectsUnknownStrategy(t *testing.T) {
	if _, err := NormalizeSearchInput(SearchInput{Query: "rest", Strategy: "experimental"}); err == nil {
		t.Fatalf("expected invalid strategy error")
	}
}

func TestNormalizeSearchInputDistinguishesFuzzyOverride(t *testing.T) {
	var omitted SearchInput
	if err := json.Unmarshal([]byte(`{"query":"redos"}`), &omitted); err != nil {
		t.Fatal(err)
	}
	omitted, err := NormalizeSearchInput(omitted)
	if err != nil {
		t.Fatalf("normalize omitted: %v", err)
	}
	if !omitted.Fuzzy {
		t.Fatalf("omitted fuzzy should use daily-driver fuzzy=true")
	}

	var explicitFalse SearchInput
	if err := json.Unmarshal([]byte(`{"query":"redos","fuzzy":false}`), &explicitFalse); err != nil {
		t.Fatal(err)
	}
	if explicitFalse.FuzzyOverride == nil {
		t.Fatalf("expected explicit false to be recorded as an override")
	}
	explicitFalse, err = NormalizeSearchInput(explicitFalse)
	if err != nil {
		t.Fatalf("normalize explicit false: %v", err)
	}
	if explicitFalse.Fuzzy {
		t.Fatalf("explicit fuzzy=false should disable fuzzy")
	}

	var explicitTrue SearchInput
	if err := json.Unmarshal([]byte(`{"query":"redos","strategy":"legacy","fuzzy":true}`), &explicitTrue); err != nil {
		t.Fatal(err)
	}
	explicitTrue, err = NormalizeSearchInput(explicitTrue)
	if err != nil {
		t.Fatalf("normalize explicit true: %v", err)
	}
	if !explicitTrue.Fuzzy {
		t.Fatalf("explicit fuzzy=true should override legacy fuzzy default")
	}
	matcher, ok := searchcfg.MatcherByName(explicitTrue.StrategyConfig, searchcfg.MatcherFuzzyName)
	if !ok || !matcher.Enabled {
		t.Fatalf("explicit fuzzy=true should enable fuzzy matcher, got %#v", matcher)
	}
}

func TestNormalizeSearchInputValidatesEnumsAndBounds(t *testing.T) {
	input, err := NormalizeSearchInput(SearchInput{
		Query:     "rest",
		Scope:     SearchScopeNames,
		Threshold: 2,
		Limit:     200,
		Offset:    -1,
		Kinds:     []NameKind{NameKindCanonical},
	})
	if err != nil {
		t.Fatalf("expected valid bounded search input, got %v", err)
	}
	if input.Threshold != 1 || input.Limit != 100 || input.Offset != 0 {
		t.Fatalf("expected bounded threshold/limit/offset, got %#v", input)
	}

	if _, err := NormalizeSearchInput(SearchInput{Query: "rest", Scope: "body"}); err == nil {
		t.Fatalf("expected invalid scope error")
	}
	if _, err := NormalizeSearchInput(SearchInput{Query: "rest", Kinds: []NameKind{"primary"}}); err == nil {
		t.Fatalf("expected invalid kind error")
	}
	if _, err := NormalizeSearchInput(SearchInput{Query: "   "}); err == nil {
		t.Fatalf("expected empty query error")
	}
}

func TestNormalizeSearchInputMatcherWeightDefaults(t *testing.T) {
	input, err := NormalizeSearchInput(SearchInput{Query: "redis"})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	weights := map[searchcfg.MatcherName]float64{}
	for _, matcher := range input.StrategyConfig.Matchers {
		weights[matcher.Name] = matcher.Weight
	}
	for matcher, want := range map[searchcfg.MatcherName]float64{
		searchcfg.MatcherExact:     1.0,
		searchcfg.MatcherPrefix:    0.8,
		searchcfg.MatcherFullText:  0.6,
		searchcfg.MatcherFuzzyName: 0.5,
		searchcfg.MatcherFuzzyBody: 0.3,
	} {
		if weights[matcher] != want {
			t.Fatalf("weight[%s] = %g, want %g", matcher, weights[matcher], want)
		}
	}
}
