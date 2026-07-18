package model

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	searchcfg "lexinet/internal/search"
)

type SearchScope string

const (
	SearchScopeNames SearchScope = "names"
	SearchScopeAll   SearchScope = "all"
)

type SearchMatchType string

type SearchStrategyName = searchcfg.StrategyName

const (
	SearchMatchExact  SearchMatchType = "exact"
	SearchMatchPrefix SearchMatchType = "prefix"
	SearchMatchFTS    SearchMatchType = "fts"
	SearchMatchFuzzy  SearchMatchType = "fuzzy"
)

type SearchInput struct {
	Query             string                 `json:"query"`
	Scope             SearchScope            `json:"scope,omitempty"`
	Fuzzy             bool                   `json:"fuzzy,omitempty"`
	Threshold         float64                `json:"threshold,omitempty"`
	Kinds             []NameKind             `json:"kinds,omitempty"`
	Limit             int                    `json:"limit,omitempty"`
	Offset            int                    `json:"offset,omitempty"`
	Strategy          searchcfg.StrategyName `json:"strategy,omitempty"`
	FuzzyOverride     *bool                  `json:"-"`
	ThresholdOverride *float64               `json:"-"`
	StrategyConfig    searchcfg.Strategy     `json:"-"`
}

type SearchResponse struct {
	Query      string                        `json:"query"`
	Scope      SearchScope                   `json:"scope"`
	Fuzzy      bool                          `json:"fuzzy"`
	Total      int                           `json:"total"`
	Limit      int                           `json:"limit"`
	Offset     int                           `json:"offset"`
	Results    []SearchResult                `json:"results"`
	Strategy   searchcfg.StrategyName        `json:"strategy,omitempty"`
	Provenance *searchcfg.ResponseProvenance `json:"provenance,omitempty"`
}

type SearchIdea struct {
	ID            uuid.UUID            `json:"id"`
	Slug          *string              `json:"slug,omitempty"`
	CanonicalName *string              `json:"canonical_name,omitempty"`
	Summary       *string              `json:"summary,omitempty"`
	Topics        []TopicSummary       `json:"topics,omitempty"`
	AcronymLinks  []AcronymLinkSummary `json:"acronym_links,omitempty"`
}

type SearchResult struct {
	Idea            SearchIdea                  `json:"idea"`
	Score           float64                     `json:"score"`
	MatchedVia      string                      `json:"matched_via"`
	MatchedField    string                      `json:"matched_field"`
	MatchedName     *string                     `json:"matched_name,omitempty"`
	MatchedNameType *NameKind                   `json:"matched_name_type,omitempty"`
	MatchType       SearchMatchType             `json:"match_type"`
	Matcher         searchcfg.MatcherName       `json:"matcher,omitempty"`
	Similarity      *float64                    `json:"similarity,omitempty"`
	Headline        *string                     `json:"headline,omitempty"`
	Provenance      *searchcfg.ResultProvenance `json:"provenance,omitempty"`
}

func (input *SearchInput) UnmarshalJSON(data []byte) error {
	type wireInput struct {
		Query     string                 `json:"query"`
		Scope     SearchScope            `json:"scope,omitempty"`
		Fuzzy     *bool                  `json:"fuzzy,omitempty"`
		Threshold *float64               `json:"threshold,omitempty"`
		Kinds     []NameKind             `json:"kinds,omitempty"`
		Limit     int                    `json:"limit,omitempty"`
		Offset    int                    `json:"offset,omitempty"`
		Strategy  searchcfg.StrategyName `json:"strategy,omitempty"`
	}
	var wire wireInput
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*input = SearchInput{
		Query:             wire.Query,
		Scope:             wire.Scope,
		Kinds:             wire.Kinds,
		Limit:             wire.Limit,
		Offset:            wire.Offset,
		Strategy:          wire.Strategy,
		FuzzyOverride:     wire.Fuzzy,
		ThresholdOverride: wire.Threshold,
	}
	if wire.Fuzzy != nil {
		input.Fuzzy = *wire.Fuzzy
	}
	if wire.Threshold != nil {
		input.Threshold = *wire.Threshold
	}
	return nil
}

func NormalizeSearchInput(input SearchInput) (SearchInput, error) {
	return NormalizeSearchInputWithProvider(input, nil)
}

func NormalizeSearchInputWithProvider(input SearchInput, provider searchcfg.StrategyProvider) (SearchInput, error) {
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" {
		return input, fmt.Errorf("search query is required")
	}
	if provider == nil {
		builtin := searchcfg.BuiltinProvider()
		provider = builtin
	}

	overrides := searchcfg.Overrides{
		Fuzzy:     input.FuzzyOverride,
		Threshold: input.ThresholdOverride,
	}
	if input.Fuzzy && overrides.Fuzzy == nil {
		overrides.Fuzzy = &input.Fuzzy
	}
	if input.Threshold > 0 && overrides.Threshold == nil {
		overrides.Threshold = &input.Threshold
	}
	strategy, err := provider.ResolveStrategy(input.Strategy, overrides)
	if err != nil {
		return input, err
	}
	input.Strategy = strategy.Name
	input.Fuzzy = strategy.Fuzzy
	input.Threshold = strategy.Threshold
	input.StrategyConfig = strategy

	if input.Scope == "" {
		input.Scope = SearchScopeAll
	}
	switch input.Scope {
	case SearchScopeNames, SearchScopeAll:
	default:
		return input, fmt.Errorf("invalid search scope %q", input.Scope)
	}

	thresholdOverridden := input.ThresholdOverride != nil || overrides.Threshold != nil
	if input.Threshold <= 0 {
		input.Threshold = 0.3
	}
	if input.Threshold > 1 {
		input.Threshold = 1
	}
	input.StrategyConfig.Threshold = input.Threshold
	for i := range input.StrategyConfig.Matchers {
		if thresholdOverridden && input.StrategyConfig.Matchers[i].Threshold > 0 {
			input.StrategyConfig.Matchers[i].Threshold = input.Threshold
		} else if input.StrategyConfig.Matchers[i].Threshold <= 0 && strings.HasPrefix(string(input.StrategyConfig.Matchers[i].Name), "fuzzy-") {
			input.StrategyConfig.Matchers[i].Threshold = input.Threshold
		}
		if strings.HasPrefix(string(input.StrategyConfig.Matchers[i].Name), "fuzzy-") {
			if !input.Fuzzy {
				input.StrategyConfig.Matchers[i].Enabled = false
			} else if input.FuzzyOverride != nil && *input.FuzzyOverride {
				input.StrategyConfig.Matchers[i].Enabled = true
			}
		}
	}
	if input.Limit <= 0 {
		input.Limit = provider.DefaultLimit()
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

func SearchResponseProvenance(input SearchInput) *searchcfg.ResponseProvenance {
	var overrides *searchcfg.RequestOverrides
	if input.FuzzyOverride != nil || input.ThresholdOverride != nil {
		overrides = &searchcfg.RequestOverrides{Fuzzy: input.FuzzyOverride, Threshold: input.ThresholdOverride}
	}
	return &searchcfg.ResponseProvenance{
		Strategy:  input.Strategy,
		Fuzzy:     input.Fuzzy,
		Threshold: input.Threshold,
		Matchers:  input.StrategyConfig.Matchers,
		Overrides: overrides,
	}
}

func SearchResultProvenance(input SearchInput, result SearchResult) *searchcfg.ResultProvenance {
	matcherName := SearchMatcherName(result)
	matcher, _ := searchcfg.MatcherByName(input.StrategyConfig, matcherName)
	return &searchcfg.ResultProvenance{Strategy: input.Strategy, Matcher: matcherName, Weight: matcher.Weight, Threshold: matcher.Threshold, Priority: matcher.Priority}
}

func SearchMatcherName(result SearchResult) searchcfg.MatcherName {
	if result.Matcher != "" {
		return result.Matcher
	}
	switch result.MatchType {
	case SearchMatchExact:
		return searchcfg.MatcherExact
	case SearchMatchPrefix:
		return searchcfg.MatcherPrefix
	case SearchMatchFTS:
		return searchcfg.MatcherFullText
	case SearchMatchFuzzy:
		if result.MatchedVia == "name" || result.MatchedName != nil {
			return searchcfg.MatcherFuzzyName
		}
		return searchcfg.MatcherFuzzyBody
	default:
		return searchcfg.MatcherName(result.MatchType)
	}
}
