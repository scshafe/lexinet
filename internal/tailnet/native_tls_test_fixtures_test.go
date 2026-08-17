package tailnet

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/client/tailscale/apitype"
)

const (
	testHostname       = "lexinet-study"
	testSecretCanary   = "tskey-auth-secret-canary"
	testRawErrorCanary = "raw-tailscale-error-secret-canary"
)

var errTestRawTailscaleFailure = errors.New(testRawErrorCanary)

type testWhoIsClient struct{}

func (*testWhoIsClient) WhoIs(context.Context, string) (*apitype.WhoIsResponse, error) {
	return &apitype.WhoIsResponse{}, nil
}

type testListener struct {
	closeCount atomic.Int32
}

func (*testListener) Accept() (net.Conn, error) {
	return nil, net.ErrClosed
}

func (listener *testListener) Close() error {
	listener.closeCount.Add(1)
	return nil
}

func (*testListener) Addr() net.Addr {
	return testNetworkAddress("tailnet-test-address")
}

type testNetworkAddress string

func (testNetworkAddress) Network() string {
	return NativeTLSNetwork
}

func (address testNetworkAddress) String() string {
	return string(address)
}

type testNativeTLSServer struct {
	mutex sync.Mutex

	calls          []string
	upDeadline     time.Time
	upFunction     func(context.Context) error
	upError        error
	whoIsClient    WhoIsClient
	localError     error
	listener       net.Listener
	listenError    error
	listenFunction func(string, string) (net.Listener, error)
	listenNetwork  string
	listenAddress  string
	closeError     error
	closeFunction  func() error
	closeCount     atomic.Int32
}

func (server *testNativeTLSServer) Up(ctx context.Context) error {
	server.mutex.Lock()
	server.calls = append(server.calls, "up")
	server.upDeadline, _ = ctx.Deadline()
	upFunction := server.upFunction
	upError := server.upError
	server.mutex.Unlock()
	if upFunction != nil {
		return upFunction(ctx)
	}
	return upError
}

func (server *testNativeTLSServer) LocalClient() (WhoIsClient, error) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	server.calls = append(server.calls, "local_client")
	return server.whoIsClient, server.localError
}

func (server *testNativeTLSServer) ListenTLS(network string, address string) (net.Listener, error) {
	server.mutex.Lock()
	server.calls = append(server.calls, "listen_tls")
	server.listenNetwork = network
	server.listenAddress = address
	listenFunction := server.listenFunction
	listener := server.listener
	listenError := server.listenError
	server.mutex.Unlock()
	if listenFunction != nil {
		return listenFunction(network, address)
	}
	return listener, listenError
}

func (server *testNativeTLSServer) Close() error {
	server.closeCount.Add(1)
	server.mutex.Lock()
	server.calls = append(server.calls, "close")
	closeFunction := server.closeFunction
	closeError := server.closeError
	server.mutex.Unlock()
	if closeFunction != nil {
		return closeFunction()
	}
	return closeError
}

func (server *testNativeTLSServer) snapshot() (
	calls []string,
	deadline time.Time,
	network string,
	address string,
) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	return append([]string(nil), server.calls...), server.upDeadline, server.listenNetwork, server.listenAddress
}

func validTestConfig(stateDirectory string) Config {
	return Config{
		Hostname:  testHostname,
		StateDir:  stateDirectory,
		UpTimeout: 5 * time.Second,
	}
}

func testStateDirectory(t interface {
	Helper()
	TempDir() string
	Fatalf(string, ...any)
}) string {
	t.Helper()
	stateDirectory := t.TempDir()
	if err := os.Chmod(stateDirectory, 0o700); err != nil {
		t.Fatalf("chmod state directory: %v", err)
	}
	return stateDirectory
}

func emptyEnvironmentLookup(string) (string, bool) {
	return "", false
}

func successfulTestServer() (*testNativeTLSServer, *testWhoIsClient, *testListener) {
	whoIsClient := &testWhoIsClient{}
	listener := &testListener{}
	server := &testNativeTLSServer{
		whoIsClient: whoIsClient,
		listener:    listener,
	}
	return server, whoIsClient, listener
}
