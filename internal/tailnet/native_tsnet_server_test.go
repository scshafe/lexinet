package tailnet

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/envknob"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/logtail"
	"tailscale.com/tsnet"
)

func TestNewNativeTSNetServerPinsIdentityStateAndSuppressesRawLogs(t *testing.T) {
	config := normalizedConfig{
		hostname:  testHostname,
		stateDir:  testStateDirectory(t),
		upTimeout: time.Second,
	}
	lifecycle, err := newNativeTSNetServer(config)
	if err != nil {
		t.Fatalf("new native tsnet server: %v", err)
	}
	nativeServer, ok := lifecycle.(*nativeTSNetServer)
	if !ok {
		t.Fatalf("lifecycle type = %T", lifecycle)
	}
	server, ok := nativeServer.server.(*tsnet.Server)
	if !ok {
		t.Fatalf("native backend type = %T", nativeServer.server)
	}
	if server.Hostname != config.hostname || server.Dir != config.stateDir {
		t.Fatalf("hostname/state dir = %q/%q", server.Hostname, server.Dir)
	}
	if server.AuthKey != "" || server.ClientSecret != "" || server.ClientID != "" ||
		server.IDToken != "" || server.Audience != "" || server.ControlURL != "" {
		t.Fatal("native server acquired an explicit enrollment or control-plane input")
	}
	if server.Ephemeral || server.RunWebClient || len(server.AdvertiseTags) != 0 {
		t.Fatal("native server enabled an unapproved node mode")
	}
	if server.UserLogf == nil || server.Logf == nil {
		t.Fatal("native server left a Tailscale logger on its raw default")
	}
	server.UserLogf("login URL: %s", testSecretCanary)
	server.Logf("backend secret: %s", testSecretCanary)
	if !envknob.NoLogsNoSupport() {
		t.Fatal("Tailscale support-log upload was not disabled")
	}
	buffer := &countingLogtailBuffer{}
	logger := logtail.NewLogger(logtail.Config{Buffer: buffer, Stderr: io.Discard}, discardTailscaleLogf)
	logger.Logf("raw backend canary: %s", testSecretCanary)
	if err := logger.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown verification logger: %v", err)
	}
	if buffer.writeCount.Load() != 0 {
		t.Fatalf("disabled logtail buffer writes = %d", buffer.writeCount.Load())
	}
}

func TestNativeTSNetServerDelegatesSuccessfulLifecycle(t *testing.T) {
	localClient := &local.Client{}
	listener := &testListener{}
	backend := &testTSNetBackend{
		status:      &ipnstate.Status{},
		localClient: localClient,
		listener:    listener,
	}
	server := &nativeTSNetServer{server: backend}
	if err := server.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	client, err := server.LocalClient()
	if err != nil || client != localClient {
		t.Fatalf("LocalClient = %#v, %v", client, err)
	}
	gotListener, err := server.ListenTLS(NativeTLSNetwork, NativeTLSAddress)
	if err != nil || gotListener != listener {
		t.Fatalf("ListenTLS = %#v, %v", gotListener, err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if backend.upCalls != 1 || backend.localCalls != 1 || backend.listenCalls != 1 || backend.closeCalls != 1 {
		t.Fatalf("backend calls = %#v", backend)
	}
	if backend.listenNetwork != NativeTLSNetwork || backend.listenAddress != NativeTLSAddress {
		t.Fatalf("backend ListenTLS = %q %q", backend.listenNetwork, backend.listenAddress)
	}
}

func TestNativeTSNetServerRejectsNilReadinessStatus(t *testing.T) {
	server := &nativeTSNetServer{server: &testTSNetBackend{}}
	err := server.Up(context.Background())
	if !errors.Is(err, errNativeTSNetStatusUnavailable) {
		t.Fatalf("Up error = %v", err)
	}
}

func TestNativeTSNetServerPreservesUpFailure(t *testing.T) {
	server := &nativeTSNetServer{server: &testTSNetBackend{upError: errTestRawTailscaleFailure}}
	err := server.Up(context.Background())
	if !errors.Is(err, errTestRawTailscaleFailure) {
		t.Fatalf("Up error = %v", err)
	}
}

func TestNativeTSNetServerRejectsNilLocalClient(t *testing.T) {
	server := &nativeTSNetServer{server: &testTSNetBackend{}}
	client, err := server.LocalClient()
	if client != nil || !errors.Is(err, errNativeTSNetLocalClientUnavailable) {
		t.Fatalf("LocalClient = %#v, %v", client, err)
	}
}

func TestNativeTSNetServerPreservesLocalClientFailure(t *testing.T) {
	server := &nativeTSNetServer{server: &testTSNetBackend{localError: errTestRawTailscaleFailure}}
	client, err := server.LocalClient()
	if client != nil || !errors.Is(err, errTestRawTailscaleFailure) {
		t.Fatalf("LocalClient = %#v, %v", client, err)
	}
}

func TestNativeTSNetInvariantErrorHasFixedRedactedRendering(t *testing.T) {
	if errNativeTSNetStatusUnavailable.Error() != "tsnet readiness status is unavailable" {
		t.Fatalf("invariant error = %q", errNativeTSNetStatusUnavailable.Error())
	}
}

func TestDiscardTailscaleLogfAcceptsUntrustedFormatWithoutFormatting(t *testing.T) {
	formatted := false
	discardTailscaleLogf("%s", formattingCanary{formatted: &formatted})
	if formatted {
		t.Fatal("discard logger formatted untrusted Tailscale arguments")
	}
}

type formattingCanary struct {
	formatted *bool
}

func (canary formattingCanary) String() string {
	*canary.formatted = true
	return testSecretCanary
}

type testTSNetBackend struct {
	status        *ipnstate.Status
	upError       error
	localClient   *local.Client
	localError    error
	listener      net.Listener
	listenError   error
	closeError    error
	upCalls       int
	localCalls    int
	listenCalls   int
	closeCalls    int
	listenNetwork string
	listenAddress string
}

type countingLogtailBuffer struct {
	writeCount atomic.Int32
}

func (*countingLogtailBuffer) TryReadLine() ([]byte, error) {
	return nil, nil
}

func (buffer *countingLogtailBuffer) Write(value []byte) (int, error) {
	buffer.writeCount.Add(1)
	return len(value), nil
}

func (backend *testTSNetBackend) Up(context.Context) (*ipnstate.Status, error) {
	backend.upCalls++
	return backend.status, backend.upError
}

func (backend *testTSNetBackend) LocalClient() (*local.Client, error) {
	backend.localCalls++
	return backend.localClient, backend.localError
}

func (backend *testTSNetBackend) ListenTLS(network string, address string) (net.Listener, error) {
	backend.listenCalls++
	backend.listenNetwork = network
	backend.listenAddress = address
	return backend.listener, backend.listenError
}

func (backend *testTSNetBackend) Close() error {
	backend.closeCalls++
	return backend.closeError
}
