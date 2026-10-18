package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPServerLifecycleEitherServerExitShutsDownAndJoinsBoth(t *testing.T) {
	testCases := []struct {
		name            string
		triggerLoopback bool
	}{
		{name: "loopback exits first", triggerLoopback: true},
		{name: "Tailnet exits first", triggerLoopback: false},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			loopbackListener := &serverLifecycleListener{identifier: "loopback"}
			tailnetListener := &serverLifecycleListener{identifier: "tailnet"}
			loopbackServer := newServerLifecycleHTTPServer(loopbackListener)
			tailnetServer := newServerLifecycleHTTPServer(tailnetListener)
			tailnetEndpoint := &serverLifecycleEndpoint{}
			triggerError := errors.New("trigger server failed")

			result := make(chan error, 1)
			go func() {
				result <- serveHTTPServerLifecycle(
					context.Background(),
					loopbackServer,
					loopbackListener,
					tailnetServer,
					tailnetListener,
					tailnetEndpoint,
				)
			}()

			awaitServerLifecycleSignal(t, loopbackServer.serveStarted, "loopback server start")
			awaitServerLifecycleSignal(t, tailnetServer.serveStarted, "Tailnet server start")
			if testCase.triggerLoopback {
				loopbackServer.releaseServe(triggerError)
			} else {
				tailnetServer.releaseServe(triggerError)
			}

			err := awaitServerLifecycleResult(t, result)
			if !errors.Is(err, triggerError) {
				t.Fatalf("error = %v, want trigger error", err)
			}
			assertServerLifecycleCompleted(t, loopbackServer, loopbackListener, 1, 0)
			assertServerLifecycleCompleted(t, tailnetServer, tailnetListener, 1, 0)
			if got := tailnetEndpoint.closeCalls.Load(); got != 1 {
				t.Fatalf("Tailnet endpoint close calls = %d, want 1", got)
			}
		})
	}
}

func TestHTTPServerLifecycleContextCancellationShutsDownAndJoinsBoth(t *testing.T) {
	loopbackListener := &serverLifecycleListener{identifier: "loopback"}
	tailnetListener := &serverLifecycleListener{identifier: "tailnet"}
	loopbackServer := newServerLifecycleHTTPServer(loopbackListener)
	tailnetServer := newServerLifecycleHTTPServer(tailnetListener)
	tailnetEndpoint := &serverLifecycleEndpoint{}
	ctx, cancel := context.WithCancel(context.Background())

	result := make(chan error, 1)
	go func() {
		result <- serveHTTPServerLifecycle(
			ctx,
			loopbackServer,
			loopbackListener,
			tailnetServer,
			tailnetListener,
			tailnetEndpoint,
		)
	}()

	awaitServerLifecycleSignal(t, loopbackServer.serveStarted, "loopback server start")
	awaitServerLifecycleSignal(t, tailnetServer.serveStarted, "Tailnet server start")
	cancel()

	err := awaitServerLifecycleResult(t, result)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	assertServerLifecycleCompleted(t, loopbackServer, loopbackListener, 1, 0)
	assertServerLifecycleCompleted(t, tailnetServer, tailnetListener, 1, 0)
	if got := tailnetEndpoint.closeCalls.Load(); got != 1 {
		t.Fatalf("Tailnet endpoint close calls = %d, want 1", got)
	}
}

func TestHTTPServerLifecycleShutdownFailuresForceCloseAndRemainVisible(t *testing.T) {
	loopbackListener := &serverLifecycleListener{identifier: "loopback"}
	tailnetListener := &serverLifecycleListener{identifier: "tailnet"}
	loopbackServer := newServerLifecycleHTTPServer(loopbackListener)
	tailnetServer := newServerLifecycleHTTPServer(tailnetListener)
	tailnetEndpoint := &serverLifecycleEndpoint{}
	loopbackShutdownError := errors.New("loopback shutdown failed")
	tailnetShutdownError := errors.New("Tailnet shutdown failed")
	loopbackCloseError := errors.New("loopback force-close failed")
	tailnetCloseError := errors.New("Tailnet force-close failed")
	tailnetEndpointCloseError := errors.New("Tailnet endpoint close failed")
	loopbackServer.shutdownError = loopbackShutdownError
	loopbackServer.closeError = loopbackCloseError
	tailnetServer.shutdownError = tailnetShutdownError
	tailnetServer.closeError = tailnetCloseError
	tailnetEndpoint.closeError = tailnetEndpointCloseError
	ctx, cancel := context.WithCancel(context.Background())

	result := make(chan error, 1)
	go func() {
		result <- serveHTTPServerLifecycle(
			ctx,
			loopbackServer,
			loopbackListener,
			tailnetServer,
			tailnetListener,
			tailnetEndpoint,
		)
	}()

	awaitServerLifecycleSignal(t, loopbackServer.serveStarted, "loopback server start")
	awaitServerLifecycleSignal(t, tailnetServer.serveStarted, "Tailnet server start")
	cancel()

	err := awaitServerLifecycleResult(t, result)
	for _, expectedError := range []error{
		context.Canceled,
		loopbackShutdownError,
		tailnetShutdownError,
		loopbackCloseError,
		tailnetCloseError,
		tailnetEndpointCloseError,
	} {
		if !errors.Is(err, expectedError) {
			t.Errorf("error = %v, want joined error %q", err, expectedError)
		}
	}
	assertServerLifecycleCompleted(t, loopbackServer, loopbackListener, 1, 1)
	assertServerLifecycleCompleted(t, tailnetServer, tailnetListener, 1, 1)
	if got := tailnetEndpoint.closeCalls.Load(); got != 1 {
		t.Fatalf("Tailnet endpoint close calls = %d, want 1", got)
	}
}

type serverLifecycleHTTPServer struct {
	expectedListener net.Listener
	actualListener   net.Listener
	serveStarted     chan struct{}
	serveExited      chan struct{}
	serveResult      chan error
	releaseServeOnce sync.Once
	shutdownError    error
	closeError       error
	shutdownCalls    atomic.Int32
	closeCalls       atomic.Int32
}

func newServerLifecycleHTTPServer(expectedListener net.Listener) *serverLifecycleHTTPServer {
	return &serverLifecycleHTTPServer{
		expectedListener: expectedListener,
		serveStarted:     make(chan struct{}),
		serveExited:      make(chan struct{}),
		serveResult:      make(chan error, 1),
	}
}

func (server *serverLifecycleHTTPServer) Serve(listener net.Listener) error {
	server.actualListener = listener
	close(server.serveStarted)
	err := <-server.serveResult
	close(server.serveExited)
	return err
}

func (server *serverLifecycleHTTPServer) Shutdown(context.Context) error {
	server.shutdownCalls.Add(1)
	if server.shutdownError == nil {
		server.releaseServe(http.ErrServerClosed)
	}
	return server.shutdownError
}

func (server *serverLifecycleHTTPServer) Close() error {
	server.closeCalls.Add(1)
	server.releaseServe(http.ErrServerClosed)
	return server.closeError
}

func (server *serverLifecycleHTTPServer) releaseServe(err error) {
	server.releaseServeOnce.Do(func() {
		server.serveResult <- err
	})
}

type serverLifecycleEndpoint struct {
	closeCalls atomic.Int32
	closeError error
}

func (endpoint *serverLifecycleEndpoint) Close() error {
	endpoint.closeCalls.Add(1)
	return endpoint.closeError
}

type serverLifecycleListener struct {
	identifier string
}

func (listener *serverLifecycleListener) Accept() (net.Conn, error) {
	return nil, errors.New("test lifecycle listener does not accept connections")
}

func (listener *serverLifecycleListener) Close() error {
	return nil
}

func (listener *serverLifecycleListener) Addr() net.Addr {
	return serverLifecycleAddress(listener.identifier)
}

type serverLifecycleAddress string

func (address serverLifecycleAddress) Network() string {
	return "server-lifecycle-test"
}

func (address serverLifecycleAddress) String() string {
	return string(address)
}

func awaitServerLifecycleSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func awaitServerLifecycleResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP server lifecycle completion")
		return nil
	}
}

func assertServerLifecycleCompleted(
	t *testing.T,
	server *serverLifecycleHTTPServer,
	expectedListener net.Listener,
	expectedShutdownCalls int32,
	expectedCloseCalls int32,
) {
	t.Helper()
	awaitServerLifecycleSignal(t, server.serveExited, "server exit")
	if server.expectedListener != expectedListener || server.actualListener != expectedListener {
		t.Fatalf("server listener wiring = (%p, %p), want %p", server.expectedListener, server.actualListener, expectedListener)
	}
	if got := server.shutdownCalls.Load(); got != expectedShutdownCalls {
		t.Fatalf("server shutdown calls = %d, want %d", got, expectedShutdownCalls)
	}
	if got := server.closeCalls.Load(); got != expectedCloseCalls {
		t.Fatalf("server force-close calls = %d, want %d", got, expectedCloseCalls)
	}
}
