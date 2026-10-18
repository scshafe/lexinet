package tailnet

import (
	"context"
	"net"

	"tailscale.com/client/local"
	"tailscale.com/envknob"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/logtail"
	"tailscale.com/tsnet"
)

type nativeTLSServer interface {
	Up(context.Context) error
	LocalClient() (WhoIsClient, error)
	ListenTLS(string, string) (net.Listener, error)
	Close() error
}

type nativeTSNetServer struct {
	server tsnetServerBackend
}

type tsnetServerBackend interface {
	Up(context.Context) (*ipnstate.Status, error)
	LocalClient() (*local.Client, error)
	ListenTLS(string, string) (net.Listener, error)
	Close() error
}

func newNativeTSNetServer(config normalizedConfig) (nativeTLSServer, error) {
	// tsnet otherwise maintains a local raw-log buffer and may send it to
	// Tailscale support infrastructure. This application exposes no raw-log
	// mode, so disable both buffering/upload and callbacks before startup.
	envknob.SetNoLogsNoSupport()
	logtail.Disable()
	return &nativeTSNetServer{server: &tsnet.Server{
		Hostname: config.hostname,
		Dir:      config.stateDir,
		UserLogf: discardTailscaleLogf,
		Logf:     discardTailscaleLogf,
	}}, nil
}

func (server *nativeTSNetServer) Up(ctx context.Context) error {
	status, err := server.server.Up(ctx)
	if err != nil {
		return err
	}
	if status == nil {
		return errNativeTSNetStatusUnavailable
	}
	return nil
}

func (server *nativeTSNetServer) LocalClient() (WhoIsClient, error) {
	client, err := server.server.LocalClient()
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errNativeTSNetLocalClientUnavailable
	}
	return client, nil
}

func (server *nativeTSNetServer) ListenTLS(network string, address string) (net.Listener, error) {
	return server.server.ListenTLS(network, address)
}

func (server *nativeTSNetServer) Close() error {
	return server.server.Close()
}

type redactedNativeTSNetInvariantError string

func (err redactedNativeTSNetInvariantError) Error() string {
	return string(err)
}

const (
	errNativeTSNetStatusUnavailable      = redactedNativeTSNetInvariantError("tsnet readiness status is unavailable")
	errNativeTSNetLocalClientUnavailable = redactedNativeTSNetInvariantError("tsnet identity client is unavailable")
)

func discardTailscaleLogf(string, ...any) {}
