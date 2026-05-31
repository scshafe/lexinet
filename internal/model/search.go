package model

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

type SearchScope string

const (
	SearchScopeNames SearchScope = "names"
	SearchScopeAll   SearchScope = "all"
)

type SearchMatchType string

const (
	SearchMatchExact  SearchMatchType = "exact"
	SearchMatchPrefix SearchMatchType = "prefix"
	SearchMatchFTS    SearchMatchType = "fts"
	SearchMatchFuzzy  SearchMatchType = "fuzzy"
)

type SearchInput struct {
	Query     string      `json:"query"`
	Scope     SearchScope `json:"scope,omitempty"`
	Fuzzy     bool        `json:"fuzzy,omitempty"`
	Threshold float64     `json:"threshold,omitempty"`
	Kinds     []NameKind  `json:"kinds,omitempty"`
	Limit     int         `json:"limit,omitempty"`
	Offset    int         `json:"offset,omitempty"`
}

type SearchResponse struct {
	Query   string         `json:"query"`
	Scope   SearchScope    `json:"scope"`
	Fuzzy   bool           `json:"fuzzy"`
	Total   int            `json:"total"`
	Limit   int            `json:"limit"`
	Offset  int            `json:"offset"`
	Results []SearchResult `json:"results"`
}

type SearchIdea struct {
	ID            uuid.UUID `json:"id"`
	Slug          *string   `json:"slug,omitempty"`
	CanonicalName *string   `json:"canonical_name,omitempty"`
	Summary       *string   `json:"summary,omitempty"`
}

type SearchResult struct {
	Idea            SearchIdea      `json:"idea"`
	Score           float64         `json:"score"`
	MatchedVia      string          `json:"matched_via"`
	MatchedField    string          `json:"matched_field"`
	MatchedName     *string         `json:"matched_name,omitempty"`
	MatchedNameType *NameKind       `json:"matched_name_type,omitempty"`
	MatchType       SearchMatchType `json:"match_type"`
	Similarity      *float64        `json:"similarity,omitempty"`
	Headline        *string         `json:"headline,omitempty"`
}

func NormalizeSearchInput(input SearchInput) (SearchInput, error) {
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" {
		return input, fmt.Errorf("search query is required")
	}

	if input.Scope == "" {
		input.Scope = SearchScopeAll
	}
	switch input.Scope {
	case SearchScopeNames, SearchScopeAll:
	default:
		return input, fmt.Errorf("invalid search scope %q", input.Scope)
	}

	if input.Threshold <= 0 {
		input.Threshold = 0.3
	}
	if input.Threshold > 1 {
		input.Threshold = 1
	}
	if input.Limit <= 0 {
		input.Limit = 20
	}
	if input.Limit > 100 {
		input.Limit = 100
	}
	if input.Offset < 0 {
		input.Offset = 0
	}

	for _, kind := range input.Kinds {
		if !kind.Valid() {
			return input, fmt.Errorf("invalid name kind %q", kind)
		}
	}
	return input, nil
}
