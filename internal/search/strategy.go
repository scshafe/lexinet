package search

import "fmt"

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
	Name      MatcherName `json:"name"`
	Enabled   bool        `json:"enabled"`
	Weight    float64     `json:"weight"`
	Threshold float64     `json:"threshold,omitempty"`
}

type Strategy struct {
	Name      StrategyName    `json:"name"`
	Fuzzy     bool            `json:"fuzzy"`
	Threshold float64         `json:"threshold"`
	Matchers  []MatcherConfig `json:"matchers"`
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
}

func DefaultStrategy() Strategy {
	return builtins()[StrategyDailyDriver]
}

func ResolveStrategy(name StrategyName, overrides Overrides) (Strategy, error) {
	if name == "" {
		name = StrategyDailyDriver
	}
	strategy, ok := builtins()[name]
	if !ok {
		return Strategy{}, fmt.Errorf("invalid search strategy %q", name)
	}
	if overrides.Fuzzy != nil {
		strategy.Fuzzy = *overrides.Fuzzy
	}
	if overrides.Threshold != nil {
		strategy.Threshold = *overrides.Threshold
	}
	return strategy, nil
}

func MatcherByName(strategy Strategy, name MatcherName) (MatcherConfig, bool) {
	for _, matcher := range strategy.Matchers {
		if matcher.Name == name {
			return matcher, true
		}
	}
	return MatcherConfig{}, false
}

func builtins() map[StrategyName]Strategy {
	return map[StrategyName]Strategy{
		StrategyDailyDriver: {
			Name:      StrategyDailyDriver,
			Fuzzy:     true,
			Threshold: 0.3,
			Matchers: []MatcherConfig{
				{Name: MatcherExact, Enabled: true, Weight: 1.0},
				{Name: MatcherPrefix, Enabled: true, Weight: 0.8},
				{Name: MatcherFullText, Enabled: true, Weight: 0.6},
				{Name: MatcherFuzzyName, Enabled: true, Weight: 0.5, Threshold: 0.3},
				{Name: MatcherFuzzyBody, Enabled: true, Weight: 0.3, Threshold: 0.3},
			},
		},
		StrategyLegacy: {
			Name:      StrategyLegacy,
			Fuzzy:     false,
			Threshold: 0.3,
			Matchers: []MatcherConfig{
				{Name: MatcherExact, Enabled: true, Weight: 1.0},
				{Name: MatcherPrefix, Enabled: true, Weight: 0.8},
				{Name: MatcherFullText, Enabled: true, Weight: 0.6},
				{Name: MatcherFuzzyName, Enabled: false, Weight: 0.5, Threshold: 0.3},
				{Name: MatcherFuzzyBody, Enabled: false, Weight: 0.3, Threshold: 0.3},
			},
		},
	}
}
