package model

import "testing"

func TestNormalizeSearchInputDefaultsToAllWithoutFuzzy(t *testing.T) {
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
	if input.Fuzzy {
		t.Fatalf("expected fuzzy to remain opt-in")
	}
	if input.Threshold != 0.3 || input.Limit != 20 || input.Offset != 0 {
		t.Fatalf("unexpected defaults: %#v", input)
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
