package config

import (
	"os"
	"path/filepath"
	"testing"

	searchcfg "lexinet/internal/search"
)

func TestDefaultSearchConfigSelectsDailyDriver(t *testing.T) {
	cfg := Default()
	provider := cfg.Search.StrategyProvider()
	strategy, err := provider.ResolveStrategy("", searchcfg.Overrides{})
	if err != nil {
		t.Fatalf("resolve default strategy: %v", err)
	}
	if strategy.Name != searchcfg.StrategyDailyDriver || !strategy.Fuzzy {
		t.Fatalf("default strategy = %#v, want fuzzy daily-driver", strategy)
	}
	if provider.DefaultLimit() != 20 {
		t.Fatalf("default limit = %d, want 20", provider.DefaultLimit())
	}
}

func TestLoadFileParsesCompatibilitySearchKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[search]\nfuzzy = false\ndefault_limit = 7\ndefault_strategy = \"daily-driver\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	provider := cfg.Search.StrategyProvider()
	strategy, err := provider.ResolveStrategy("", searchcfg.Overrides{})
	if err != nil {
		t.Fatalf("resolve default strategy: %v", err)
	}
	if strategy.Fuzzy {
		t.Fatalf("compat search.fuzzy=false should disable default fuzzy")
	}
	if provider.DefaultLimit() != 7 {
		t.Fatalf("default limit = %d, want 7", provider.DefaultLimit())
	}
}

func TestLoadFilePartialStrategyKeepsBuiltinDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[search.strategies.daily-driver]\nthreshold = 0.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	strategy, err := cfg.Search.StrategyProvider().ResolveStrategy("", searchcfg.Overrides{})
	if err != nil {
		t.Fatalf("resolve default strategy: %v", err)
	}
	if !strategy.Fuzzy || strategy.Threshold != 0.4 {
		t.Fatalf("partial daily-driver strategy = %#v, want fuzzy=true threshold=0.4", strategy)
	}
	matcher, ok := searchcfg.MatcherByName(strategy, searchcfg.MatcherFuzzyName)
	if !ok || !matcher.Enabled || matcher.Weight != 0.5 || matcher.Priority != 3 {
		t.Fatalf("partial strategy should retain fuzzy-name defaults, got %#v", matcher)
	}
}

func TestConfigSetUpdatesStrategyMatcherKnobs(t *testing.T) {
	cfg := Default()
	sets := map[string]string{
		"search.default_strategy":                                      "legacy",
		"search.strategies.daily-driver.matchers.fuzzy-name.enabled":   "false",
		"search.strategies.daily-driver.matchers.fuzzy-name.weight":    "0.42",
		"search.strategies.daily-driver.matchers.fuzzy-name.threshold": "0.55",
		"search.strategies.daily-driver.matchers.fuzzy-name.priority":  "9",
	}
	for key, value := range sets {
		if err := cfg.Set(key, value); err != nil {
			t.Fatalf("Set(%s): %v", key, err)
		}
	}
	if cfg.Search.DefaultStrategy != searchcfg.StrategyLegacy {
		t.Fatalf("default strategy = %q, want legacy", cfg.Search.DefaultStrategy)
	}
	strategy, err := cfg.Search.StrategyProvider().ResolveStrategy(searchcfg.StrategyDailyDriver, searchcfg.Overrides{})
	if err != nil {
		t.Fatalf("resolve daily-driver: %v", err)
	}
	matcher, ok := searchcfg.MatcherByName(strategy, searchcfg.MatcherFuzzyName)
	if !ok {
		t.Fatalf("missing fuzzy-name matcher")
	}
	if matcher.Enabled || matcher.Weight != 0.42 || matcher.Threshold != 0.55 || matcher.Priority != 9 {
		t.Fatalf("unexpected matcher config: %#v", matcher)
	}
}
