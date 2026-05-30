package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	AppName          = "lexinet"
	DefaultEndpoint  = "http://127.0.0.1:17892"
	DefaultListen    = "127.0.0.1:17892"
	DefaultConfigEnv = "LEXINET_CONFIG"
)

type Config struct {
	Server  ServerConfig  `toml:"server" json:"server"`
	Search  SearchConfig  `toml:"search" json:"search"`
	Display DisplayConfig `toml:"display" json:"display"`
	Service ServiceConfig `toml:"service" json:"service"`
}

type ServerConfig struct {
	URL       string `toml:"url" json:"url"`
	Token     string `toml:"token" json:"token"`
	TokenFile string `toml:"token_file" json:"token_file"`
}

type SearchConfig struct {
	Fuzzy        bool `toml:"fuzzy" json:"fuzzy"`
	DefaultLimit int  `toml:"default_limit" json:"default_limit"`
}

type DisplayConfig struct {
	Color    string `toml:"color" json:"color"`
	IDLength int    `toml:"id_length" json:"id_length"`
}

type ServiceConfig struct {
	ListenLoopback  string         `toml:"listen_loopback" json:"listen_loopback"`
	AuthToken       string         `toml:"auth_token" json:"auth_token"`
	TailnetEnabled  bool           `toml:"tailnet_enabled" json:"tailnet_enabled"`
	TailnetHostname string         `toml:"tailnet_hostname" json:"tailnet_hostname"`
	TailnetStateDir string         `toml:"tailnet_state_dir" json:"tailnet_state_dir"`
	Database        DatabaseConfig `toml:"database" json:"database"`
}

type DatabaseConfig struct {
	DSN string `toml:"dsn" json:"dsn"`
}

func Default() Config {
	return Config{
		Server: ServerConfig{
			URL:       DefaultEndpoint,
			Token:     "",
			TokenFile: "~/.config/lexinet/token",
		},
		Search: SearchConfig{
			Fuzzy:        false,
			DefaultLimit: 20,
		},
		Display: DisplayConfig{
			Color:    "auto",
			IDLength: 12,
		},
		Service: ServiceConfig{
			ListenLoopback:  DefaultListen,
			AuthToken:       "",
			TailnetEnabled:  true,
			TailnetHostname: "lexinet",
			TailnetStateDir: "~/.config/lexinet/tsnet-state",
			Database: DatabaseConfig{
				DSN: "postgres://lexinet@127.0.0.1:5432/lexinet?sslmode=disable",
			},
		},
	}
}

func Path() (string, error) {
	if p := os.Getenv(DefaultConfigEnv); p != "" {
		return expandHome(p)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, AppName, "config.toml"), nil
}

func Load() (Config, string, error) {
	path, err := Path()
	if err != nil {
		return Config{}, "", err
	}
	cfg, err := LoadFile(path)
	return cfg, path, err
}

func LoadFile(path string) (Config, error) {
	path, err := expandHome(path)
	if err != nil {
		return Config{}, err
	}
	cfg := Default()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := SaveFile(path, cfg); err != nil {
			return Config{}, err
		}
		return cfg, nil
	} else if err != nil {
		return Config{}, err
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func SaveFile(path string, cfg Config) error {
	path, err := expandHome(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(cfg)
}

func (c Config) Save(path string) error {
	return SaveFile(path, c)
}

func (c Config) Endpoint(flagValue string) string {
	if flagValue != "" {
		return strings.TrimRight(flagValue, "/")
	}
	if v := os.Getenv("LEXINET_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if c.Server.URL != "" {
		return strings.TrimRight(c.Server.URL, "/")
	}
	return DefaultEndpoint
}

func (c Config) TokenFile(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("LEXINET_TOKEN_FILE"); v != "" {
		return v
	}
	return c.Server.TokenFile
}

func (c Config) Token(flagTokenFile string) (string, error) {
	if tokenFile := c.TokenFile(flagTokenFile); tokenFile != "" {
		path, err := expandHome(tokenFile)
		if err != nil {
			return "", err
		}
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b)), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	if v := os.Getenv("LEXINET_TOKEN"); v != "" {
		return v, nil
	}
	return c.Server.Token, nil
}

func (c Config) Redacted() Config {
	redacted := c
	if redacted.Server.Token != "" {
		redacted.Server.Token = "***redacted***"
	}
	if redacted.Service.AuthToken != "" {
		redacted.Service.AuthToken = "***redacted***"
	}
	return redacted
}

func (c *Config) Set(key, value string) error {
	switch strings.ToLower(key) {
	case "server.url":
		c.Server.URL = value
	case "server.token":
		c.Server.Token = value
	case "server.token_file":
		c.Server.TokenFile = value
	case "search.fuzzy":
		v, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		c.Search.Fuzzy = v
	case "search.default_limit":
		v, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		c.Search.DefaultLimit = v
	case "display.color":
		c.Display.Color = value
	case "display.id_length":
		v, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		c.Display.IDLength = v
	case "service.listen_loopback":
		c.Service.ListenLoopback = value
	case "service.auth_token":
		c.Service.AuthToken = value
	case "service.tailnet_enabled":
		v, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		c.Service.TailnetEnabled = v
	case "service.tailnet_hostname":
		c.Service.TailnetHostname = value
	case "service.tailnet_state_dir":
		c.Service.TailnetStateDir = value
	case "service.database.dsn":
		c.Service.Database.DSN = value
	default:
		return fmt.Errorf("unknown config key %q", key)
	}
	return nil
}

func ExpandHome(path string) (string, error) {
	return expandHome(path)
}

func expandHome(path string) (string, error) {
	if path == "" || path[0] != '~' {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:]), nil
	}
	return "", fmt.Errorf("cannot expand home in %q", path)
}
