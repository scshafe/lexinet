package tailnet

import (
	"fmt"
	"net"

	"tailscale.com/tsnet"

	"lexinet/internal/config"
)

type Config struct {
	Hostname string
	StateDir string
	Port     string
}

func Listen(cfg Config) (net.Listener, func() error, error) {
	if cfg.Hostname == "" {
		cfg.Hostname = "lexinet"
	}
	if cfg.Port == "" {
		cfg.Port = "17892"
	}
	stateDir, err := config.ExpandHome(cfg.StateDir)
	if err != nil {
		return nil, nil, err
	}
	srv := &tsnet.Server{Hostname: cfg.Hostname, Dir: stateDir}
	ln, err := srv.Listen("tcp", fmt.Sprintf(":%s", cfg.Port))
	if err != nil {
		_ = srv.Close()
		return nil, nil, err
	}
	return ln, srv.Close, nil
}
