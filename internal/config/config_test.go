package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	searchcfg "lexinet/internal/search"
)

func TestDefaultServerConfigurationAllowsLoopbackWithoutBearer(t *testing.T) {
	cfg := Default()
	if cfg.Service.TailnetEnabled {
		t.Fatal("default configuration enables Tailnet, want loopback-only default")
	}
	if cfg.Service.AuthToken != "" {
		t.Fatal("default service bearer is non-empty, want optional loopback bearer")
	}
	if !reflect.DeepEqual(cfg.Service.TailnetStudyAuthentication, TailnetStudyAuthenticationConfig{}) {
		t.Fatalf("default Tailnet study authentication = %#v, want unconfigured", cfg.Service.TailnetStudyAuthentication)
	}
}

func TestLoadFileParsesTailnetStudyAuthenticationPolicyWithoutCredentialBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	contents := `[service]
tailnet_enabled = true
tailnet_hostname = "lexinet-study"
tailnet_state_dir = "/var/lib/lexinet/tsnet-state"

[service.tailnet_study_authentication]
mode = "tailnet_node"
required_node_tag = "tag:embedded-house-panel"
application_capability = "example.invalid/cap/lexinet-study-v1"
allowed_profile_refs = ["learner.primary", "learner.secondary"]
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	authentication := cfg.Service.TailnetStudyAuthentication
	if authentication.Mode != "tailnet_node" ||
		authentication.RequiredNodeTag != "tag:embedded-house-panel" ||
		authentication.ApplicationCapability != "example.invalid/cap/lexinet-study-v1" ||
		len(authentication.AllowedProfileRefs) != 2 ||
		authentication.AllowedProfileRefs[0] != "learner.primary" ||
		authentication.AllowedProfileRefs[1] != "learner.secondary" ||
		authentication.ScopedTokenSubjectID != "" || authentication.ScopedTokenFile != "" {
		t.Fatalf("Tailnet study authentication = %#v", authentication)
	}
}

func TestConfigSetUpdatesTailnetStudyAuthenticationPolicy(t *testing.T) {
	cfg := Default()
	sets := map[string]string{
		"service.tailnet_study_authentication.mode":                    "scoped_token",
		"service.tailnet_study_authentication.allowed_profile_refs":    "learner.primary, learner.secondary",
		"service.tailnet_study_authentication.scoped_token_subject_id": "11111111-1111-1111-1111-111111111111",
		"service.tailnet_study_authentication.scoped_token_file":       "/run/secrets/lexinet-study-token",
	}
	for key, value := range sets {
		if err := cfg.Set(key, value); err != nil {
			t.Fatalf("Set(%s): %v", key, err)
		}
	}
	authentication := cfg.Service.TailnetStudyAuthentication
	if authentication.Mode != "scoped_token" ||
		authentication.ScopedTokenSubjectID != "11111111-1111-1111-1111-111111111111" ||
		authentication.ScopedTokenFile != "/run/secrets/lexinet-study-token" ||
		len(authentication.AllowedProfileRefs) != 2 ||
		authentication.AllowedProfileRefs[0] != "learner.primary" ||
		authentication.AllowedProfileRefs[1] != "learner.secondary" {
		t.Fatalf("Tailnet study authentication = %#v", authentication)
	}
	if err := cfg.Set("service.tailnet_study_authentication.allowed_profile_refs", "learner.primary,"); err == nil {
		t.Fatal("empty Tailnet profile-list value was accepted")
	}
}

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
