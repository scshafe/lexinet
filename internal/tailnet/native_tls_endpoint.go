package tailnet

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
)

// NativeTLSEndpoint is the inseparable listener, caller-identity client, and
// embedded-node lifecycle returned by the canonical native Tailnet transport.
type NativeTLSEndpoint struct {
	listener    net.Listener
	whoIsClient WhoIsClient
	server      nativeTLSServer
	closeOnce   sync.Once
	closeError  error
}

// Listener returns the fixed-port native TLS listener. The caller may pass it
// to an HTTP server; closing the endpoint also stops its embedded tsnet server.
func (endpoint *NativeTLSEndpoint) Listener() net.Listener {
	if endpoint == nil {
		return nil
	}
	return endpoint.listener
}

// WhoIsClient returns the LocalAPI client bound to this embedded tsnet node.
// The HTTP authentication boundary must call WhoIs with each request's exact
// RemoteAddr; this transport does not cache or interpret identity.
func (endpoint *NativeTLSEndpoint) WhoIsClient() WhoIsClient {
	if endpoint == nil {
		return nil
	}
	return endpoint.whoIsClient
}

// Close stops the embedded node exactly once and returns a stable redacted
// TAILNET_CLOSE_FAILED error if Tailscale shutdown fails.
func (endpoint *NativeTLSEndpoint) Close() error {
	if endpoint == nil || endpoint.server == nil {
		return nil
	}
	endpoint.closeOnce.Do(func() {
		if err := endpoint.server.Close(); err != nil {
			endpoint.closeError = newError(
				ErrorCodeCloseFailed,
				"native Tailnet transport could not close",
				nil,
				err,
			)
		}
	})
	return endpoint.closeError
}

// OpenNativeTLS is the @canonical tailnet_native_tls_transport. It rejects
// ambient enrollment controls, performs a bounded Up, obtains LocalClient, and
// opens only tsnet.ListenTLS("tcp", ":443"). Every partial failure closes the
// embedded server exactly once and returns a structured redacted error.
func OpenNativeTLS(ctx context.Context, config Config) (*NativeTLSEndpoint, error) {
	return openNativeTLS(ctx, config, os.LookupEnv, newNativeTSNetServer)
}

// ValidateNativeTLSConfiguration performs the side-effect-free startup
// preflight for native Tailnet TLS. It normalizes bounded fields, rejects
// ambient enrollment controls, and verifies the persisted state directory
// without constructing or starting tsnet. OpenNativeTLS defensively repeats
// the same checks immediately before construction.
func ValidateNativeTLSConfiguration(config Config) error {
	_, err := validateNativeTLSConfiguration(config, os.LookupEnv)
	return err
}

type environmentLookup func(string) (string, bool)

type nativeTLSServerFactory func(normalizedConfig) (nativeTLSServer, error)

func openNativeTLS(
	ctx context.Context,
	config Config,
	lookupEnvironment environmentLookup,
	factory nativeTLSServerFactory,
) (*NativeTLSEndpoint, error) {
	if ctx == nil {
		return nil, configurationError("context")
	}
	normalized, err := validateNativeTLSConfiguration(config, lookupEnvironment)
	if err != nil {
		return nil, err
	}
	if factory == nil {
		return nil, newError(
			ErrorCodeServerCreateFailed,
			"native Tailnet server could not be created",
			nil,
			nil,
		)
	}

	server, err := factory(normalized)
	if err != nil {
		failure := newError(
			ErrorCodeServerCreateFailed,
			"native Tailnet server could not be created",
			nil,
			err,
		)
		if server != nil {
			return nil, closeServerAfterFailure(server, failure)
		}
		return nil, failure
	}
	if server == nil {
		return nil, newError(
			ErrorCodeServerCreateFailed,
			"native Tailnet server could not be created",
			nil,
			nil,
		)
	}

	startupContext, cancelStartup := context.WithTimeout(ctx, normalized.upTimeout)
	defer cancelStartup()
	err = server.Up(startupContext)
	if err != nil {
		return nil, closeServerAfterFailure(server, newError(
			ErrorCodeUpFailed,
			"native Tailnet server did not become ready",
			nil,
			err,
		))
	}

	whoIsClient, err := server.LocalClient()
	if err != nil || whoIsClient == nil {
		return nil, closeServerAfterFailure(server, newError(
			ErrorCodeLocalClientFailed,
			"native Tailnet identity client is unavailable",
			nil,
			err,
		))
	}

	listener, err := listenNativeTLSWithinBudget(startupContext, server)
	if err != nil {
		return nil, err
	}

	return &NativeTLSEndpoint{
		listener:    listener,
		whoIsClient: whoIsClient,
		server:      server,
	}, nil
}

func validateNativeTLSConfiguration(
	config Config,
	lookupEnvironment environmentLookup,
) (normalizedConfig, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return normalizedConfig{}, err
	}
	if err := rejectAmbientTailscaleControls(lookupEnvironment); err != nil {
		return normalizedConfig{}, err
	}
	if err := validateStateDirectory(normalized.stateDir); err != nil {
		return normalizedConfig{}, err
	}
	return normalized, nil
}

type nativeTLSListenResult struct {
	listener net.Listener
	err      error
}

func listenNativeTLSWithinBudget(ctx context.Context, server nativeTLSServer) (net.Listener, error) {
	resultChannel := make(chan nativeTLSListenResult, 1)
	go func() {
		listener, err := server.ListenTLS(NativeTLSNetwork, NativeTLSAddress)
		resultChannel <- nativeTLSListenResult{listener: listener, err: err}
	}()

	select {
	case result := <-resultChannel:
		if ctx.Err() != nil {
			return nil, closeServerAndListenResultAfterBudget(server, result, ctx.Err())
		}
		if result.err != nil || result.listener == nil {
			failure := newError(
				ErrorCodeNativeTLSListenFailed,
				"native Tailnet TLS listener is unavailable",
				nil,
				result.err,
			)
			if result.listener != nil {
				failure.cause = errors.Join(failure.cause, result.listener.Close())
			}
			return nil, closeServerAfterFailure(server, failure)
		}
		return result.listener, nil
	case <-ctx.Done():
		// Upstream tsnet.ListenTLS performs a second Up with a background
		// context. Closing the already-started server cancels that hidden wait;
		// reading the result channel then joins the goroutine before returning.
		closeError := server.Close()
		result := <-resultChannel
		return nil, listenBudgetError(result, ctx.Err(), closeError)
	}
}

func closeServerAndListenResultAfterBudget(
	server nativeTLSServer,
	result nativeTLSListenResult,
	contextError error,
) error {
	closeError := server.Close()
	return listenBudgetError(result, contextError, closeError)
}

func listenBudgetError(result nativeTLSListenResult, contextError error, closeError error) *Error {
	failure := newError(
		ErrorCodeNativeTLSListenFailed,
		"native Tailnet TLS listener is unavailable",
		map[string]string{"reason": "startup_budget_exhausted"},
		errors.Join(contextError, result.err),
	)
	if result.listener != nil {
		failure.cause = errors.Join(failure.cause, result.listener.Close())
	}
	if closeError != nil {
		failure.Context["cleanup"] = "failed"
		failure.cause = errors.Join(failure.cause, closeError)
	}
	return failure
}

func rejectAmbientTailscaleControls(lookupEnvironment environmentLookup) error {
	if lookupEnvironment == nil {
		return newError(
			ErrorCodeAmbientConfigurationRejected,
			"ambient Tailscale enrollment configuration is not allowed",
			map[string]string{"environment_variables": "environment lookup unavailable"},
			nil,
		)
	}
	rejectedNames := make([]string, 0, len(ambientTailscaleEnrollmentControls))
	for _, name := range ambientTailscaleEnrollmentControls {
		value, exists := lookupEnvironment(name)
		if exists && value != "" {
			rejectedNames = append(rejectedNames, name)
		}
	}
	if len(rejectedNames) == 0 {
		return nil
	}
	return newError(
		ErrorCodeAmbientConfigurationRejected,
		"ambient Tailscale enrollment configuration is not allowed",
		map[string]string{"environment_variables": strings.Join(rejectedNames, ",")},
		nil,
	)
}

func closeServerAfterFailure(server nativeTLSServer, failure *Error) error {
	closeErr := server.Close()
	if closeErr == nil {
		return failure
	}
	failure.Context = cloneStringMap(failure.Context)
	if failure.Context == nil {
		failure.Context = make(map[string]string, 1)
	}
	failure.Context["cleanup"] = "failed"
	failure.cause = errors.Join(failure.cause, closeErr)
	return failure
}
