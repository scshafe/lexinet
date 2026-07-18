package search

import (
	"fmt"
	"strings"
)

type StrategyName string

const (
	StrategyDailyDriver StrategyName = "daily-driver"
	StrategyLegacy      StrategyName = "legacy"
)

type MatcherName string

const (
	MatcherExact     MatcherName = "exact"
	MatcherPrefix    MatcherName = "prefix"
	MatcherFullText  MatcherName = "full-text"
	MatcherFuzzyName MatcherName = "fuzzy-name"
	MatcherFuzzyBody MatcherName = "fuzzy-body"
)

type MatcherConfig struct {
	Name      MatcherName `toml:"name" json:"name"`
	Enabled   bool        `toml:"enabled" json:"enabled"`
	Weight    float64     `toml:"weight" json:"weight"`
	Threshold float64     `toml:"threshold,omitempty" json:"threshold,omitempty"`
	Priority  int         `toml:"priority" json:"priority"`
}

type Strategy struct {
	Name      StrategyName    `toml:"name" json:"name"`
	Fuzzy     bool            `toml:"fuzzy" json:"fuzzy"`
	Threshold float64         `toml:"threshold" json:"threshold"`
	Matchers  []MatcherConfig `toml:"matchers" json:"matchers"`
}

type Overrides struct {
	Fuzzy     *bool
	Threshold *float64
}

type RequestOverrides struct {
	Fuzzy     *bool    `json:"fuzzy,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
}

type ResponseProvenance struct {
	Strategy  StrategyName      `json:"strategy"`
	Fuzzy     bool              `json:"fuzzy"`
	Threshold float64           `json:"threshold"`
	Matchers  []MatcherConfig   `json:"matchers"`
	Overrides *RequestOverrides `json:"overrides,omitempty"`
}

type ResultProvenance struct {
	Strategy StrategyName `json:"strategy"`
	Matcher  MatcherName  `json:"matcher"`
	Weight   float64      `json:"weight"`
	Priority int          `json:"priority"`
}

type StrategyProvider interface {
	ResolveStrategy(StrategyName, Overrides) (Strategy, error)
	DefaultLimit() int
}

type StaticProvider struct {
	DefaultStrategy StrategyName
	Strategies      map[StrategyName]Strategy
	Limit           int
	FuzzyOverride   *bool
}

func NewStaticProvider(defaultStrategy StrategyName, strategies map[StrategyName]Strategy, defaultLimit int) StaticProvider {
	if defaultStrategy == "" {
		defaultStrategy = StrategyDailyDriver
	}
	merged := DefaultStrategies()
	for name, strategy := range strategies {
		name = StrategyName(strings.TrimSpace(string(name)))
		if name == "" {
			name = strategy.Name
		}
		if name == "" {
			continue
		}
		strategy.Name = name
		merged[name] = completeStrategy(name, strategy, merged[name])
	}
	if defaultLimit <= 0 {
		defaultLimit = 20
	}
	return StaticProvider{DefaultStrategy: defaultStrategy, Strategies: merged, Limit: defaultLimit}
}

func BuiltinProvider() StaticProvider {
	return NewStaticProvider(StrategyDailyDriver, nil, 20)
}

func (p StaticProvider) ResolveStrategy(name StrategyName, overrides Overrides) (Strategy, error) {
	if name == "" {
		name = p.DefaultStrategy
	}
	if name == "" {
		name = StrategyDailyDriver
	}
	strategies := p.Strategies
	if len(strategies) == 0 {
		strategies = DefaultStrategies()
	}
	strategy, ok := strategies[name]
	if !ok {
		return Strategy{}, fmt.Errorf("invalid search strategy %q", name)
	}
	if p.FuzzyOverride != nil && overrides.Fuzzy == nil {
		strategy.Fuzzy = *p.FuzzyOverride
	}
	if overrides.Fuzzy != nil {
		strategy.Fuzzy = *overrides.Fuzzy
	}
	if overrides.Threshold != nil {
		strategy.Threshold = *overrides.Threshold
	}
	return strategy, nil
}

func (p StaticProvider) DefaultLimit() int {
	if p.Limit <= 0 {
		return 20
	}
	return p.Limit
}

func DefaultStrategy() Strategy {
	return DefaultStrategies()[StrategyDailyDriver]
}

func ResolveStrategy(name StrategyName, overrides Overrides) (Strategy, error) {
	return BuiltinProvider().ResolveStrategy(name, overrides)
}

func MatcherByName(strategy Strategy, name MatcherName) (MatcherConfig, bool) {
	for _, matcher := range strategy.Matchers {
		if matcher.Name == name {
			return matcher, true
		}
	}
	return MatcherConfig{}, false
}

func DefaultStrategies() map[StrategyName]Strategy {
	return map[StrategyName]Strategy{
		StrategyDailyDriver: {
			Name:      StrategyDailyDriver,
			Fuzzy:     true,
			Threshold: 0.3,
			Matchers: []MatcherConfig{
				{Name: MatcherExact, Enabled: true, Weight: 1.0, Priority: 0},
				{Name: MatcherPrefix, Enabled: true, Weight: 0.8, Priority: 1},
				{Name: MatcherFullText, Enabled: true, Weight: 0.6, Priority: 2},
				{Name: MatcherFuzzyName, Enabled: true, Weight: 0.5, Threshold: 0.3, Priority: 3},
				{Name: MatcherFuzzyBody, Enabled: true, Weight: 0.3, Threshold: 0.3, Priority: 4},
			},
		},
		StrategyLegacy: {
			Name:      StrategyLegacy,
			Fuzzy:     false,
			Threshold: 0.3,
			Matchers: []MatcherConfig{
				{Name: MatcherExact, Enabled: true, Weight: 1.0, Priority: 0},
				{Name: MatcherPrefix, Enabled: true, Weight: 0.8, Priority: 1},
				{Name: MatcherFullText, Enabled: true, Weight: 0.6, Priority: 2},
				{Name: MatcherFuzzyName, Enabled: false, Weight: 0.5, Threshold: 0.3, Priority: 3},
				{Name: MatcherFuzzyBody, Enabled: false, Weight: 0.3, Threshold: 0.3, Priority: 4},
			},
		},
	}
}

func completeStrategy(name StrategyName, strategy Strategy, base Strategy) Strategy {
	if base.Name == "" {
		base = Strategy{Name: name, Threshold: 0.3}
	}
	strategy.Name = name
	if strategy.Threshold <= 0 {
		strategy.Threshold = base.Threshold
	}
	baseMatchers := make(map[MatcherName]MatcherConfig, len(base.Matchers))
	for _, matcher := range base.Matchers {
		baseMatchers[matcher.Name] = matcher
	}
	merged := make([]MatcherConfig, 0, len(base.Matchers)+len(strategy.Matchers))
	seen := make(map[MatcherName]bool, len(strategy.Matchers))
	for _, matcher := range strategy.Matchers {
		if matcher.Name == "" {
			continue
		}
		baseMatcher := baseMatchers[matcher.Name]
		if matcher.Weight == 0 {
			matcher.Weight = baseMatcher.Weight
		}
		if matcher.Threshold <= 0 {
			matcher.Threshold = baseMatcher.Threshold
		}
		if matcher.Priority == 0 && baseMatcher.Priority != 0 {
			matcher.Priority = baseMatcher.Priority
		}
		merged = append(merged, matcher)
		seen[matcher.Name] = true
	}
	for _, matcher := range base.Matchers {
		if !seen[matcher.Name] {
			merged = append(merged, matcher)
		}
	}
	strategy.Matchers = merged
	return strategy
}
