package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenNativeTLSReturnsListenerAndWhoIsFromOneLifecycle(t *testing.T) {
	stateDirectory := testStateDirectory(t)
	server, whoIsClient, listener := successfulTestServer()
	var factoryConfig normalizedConfig
	factoryCalls := 0
	startedAt := time.Now()
	endpoint, err := openNativeTLS(
		context.Background(),
		validTestConfig(stateDirectory),
		emptyEnvironmentLookup,
		func(config normalizedConfig) (nativeTLSServer, error) {
			factoryCalls++
			factoryConfig = config
			return server, nil
		},
	)
	if err != nil {
		t.Fatalf("open native TLS: %v", err)
	}
	if factoryCalls != 1 || factoryConfig.hostname != testHostname || factoryConfig.stateDir != stateDirectory {
		t.Fatalf("factory calls/config = %d/%#v", factoryCalls, factoryConfig)
	}
	if endpoint.Listener() != listener || endpoint.WhoIsClient() != whoIsClient {
		t.Fatal("endpoint did not preserve listener and WhoIs client identity")
	}
	calls, deadline, network, address := server.snapshot()
	if !reflect.DeepEqual(calls, []string{"up", "local_client", "listen_tls"}) {
		t.Fatalf("startup calls = %#v", calls)
	}
	if deadline.Before(startedAt) || deadline.After(startedAt.Add(6*time.Second)) {
		t.Fatalf("Up deadline = %s", deadline)
	}
	if network != NativeTLSNetwork || address != NativeTLSAddress {
		t.Fatalf("ListenTLS = %q %q", network, address)
	}
	if server.closeCount.Load() != 0 {
		t.Fatalf("server close count before endpoint close = %d", server.closeCount.Load())
	}
}

func TestOpenNativeTLSRejectsNilContextBeforeFactory(t *testing.T) {
	factoryCalls := 0
	endpoint, err := openNativeTLS(
		nil,
		validTestConfig(testStateDirectory(t)),
		emptyEnvironmentLookup,
		func(normalizedConfig) (nativeTLSServer, error) {
			factoryCalls++
			return nil, nil
		},
	)
	if endpoint != nil || factoryCalls != 0 {
		t.Fatalf("endpoint/factory calls = %#v/%d", endpoint, factoryCalls)
	}
	assertTailnetErrorCode(t, err, ErrorCodeConfigurationInvalid)
}

func TestValidateNativeTLSConfigurationPerformsPreflightWithoutTSNetCreation(t *testing.T) {
	config := validTestConfig(testStateDirectory(t))
	if err := ValidateNativeTLSConfiguration(config); err != nil {
		t.Fatalf("validate native TLS configuration: %v", err)
	}
}

func TestValidateNativeTLSConfigurationRejectsInvalidStateWithoutTSNet(t *testing.T) {
	config := validTestConfig(testStateDirectory(t) + "/missing")
	err := ValidateNativeTLSConfiguration(config)
	assertTailnetErrorCode(t, err, ErrorCodeStateDirectoryInvalid)
}

func TestOpenNativeTLSRejectsEveryAmbientEnrollmentControlWithoutReadingValuesIntoErrors(t *testing.T) {
	for _, environmentVariable := range ambientTailscaleEnrollmentControls {
		t.Run(environmentVariable, func(t *testing.T) {
			factoryCalls := 0
			lookup := func(name string) (string, bool) {
				if name == environmentVariable {
					return testSecretCanary, true
				}
				return "", false
			}
			endpoint, err := openNativeTLS(
				context.Background(),
				validTestConfig(testStateDirectory(t)),
				lookup,
				func(normalizedConfig) (nativeTLSServer, error) {
					factoryCalls++
					return nil, nil
				},
			)
			if endpoint != nil || factoryCalls != 0 {
				t.Fatalf("endpoint/factory calls = %#v/%d", endpoint, factoryCalls)
			}
			tailnetError := assertTailnetErrorCode(t, err, ErrorCodeAmbientConfigurationRejected)
			if tailnetError.Context["environment_variables"] != environmentVariable {
				t.Fatalf("environment context = %#v", tailnetError.Context)
			}
			assertErrorRenderingsExclude(t, err, testSecretCanary)
		})
	}
}

func TestOpenNativeTLSReportsAllAmbientControlNamesInFixedOrder(t *testing.T) {
	lookup := func(name string) (string, bool) {
		switch name {
		case "TS_DEBUG_ENV_FILE", "TS_AUTHKEY", "TSNET_FORCE_LOGIN":
			return testSecretCanary, true
		default:
			return "", false
		}
	}
	_, err := openNativeTLS(
		context.Background(),
		validTestConfig(testStateDirectory(t)),
		lookup,
		func(normalizedConfig) (nativeTLSServer, error) { return nil, nil },
	)
	tailnetError := assertTailnetErrorCode(t, err, ErrorCodeAmbientConfigurationRejected)
	want := "TS_DEBUG_ENV_FILE,TS_AUTHKEY,TSNET_FORCE_LOGIN"
	if tailnetError.Context["environment_variables"] != want {
		t.Fatalf("environment variables = %q, want %q", tailnetError.Context["environment_variables"], want)
	}
}

func TestOpenNativeTLSIgnoresDefinedButEmptyAmbientControls(t *testing.T) {
	server, _, _ := successfulTestServer()
	lookup := func(string) (string, bool) { return "", true }
	endpoint, err := openNativeTLS(
		context.Background(),
		validTestConfig(testStateDirectory(t)),
		lookup,
		func(normalizedConfig) (nativeTLSServer, error) { return server, nil },
	)
	if err != nil || endpoint == nil {
		t.Fatalf("endpoint/error = %#v/%v", endpoint, err)
	}
}

func TestOpenNativeTLSRejectsUnavailableEnvironmentLookup(t *testing.T) {
	factoryCalls := 0
	_, err := openNativeTLS(
		context.Background(),
		validTestConfig(testStateDirectory(t)),
		nil,
		func(normalizedConfig) (nativeTLSServer, error) {
			factoryCalls++
			return nil, nil
		},
	)
	if factoryCalls != 0 {
		t.Fatalf("factory calls = %d", factoryCalls)
	}
	assertTailnetErrorCode(t, err, ErrorCodeAmbientConfigurationRejected)
}

func TestOpenNativeTLSValidatesPreEnrolledStateBeforeFactory(t *testing.T) {
	factoryCalls := 0
	config := validTestConfig(testStateDirectory(t) + "/missing")
	_, err := openNativeTLS(
		context.Background(),
		config,
		emptyEnvironmentLookup,
		func(normalizedConfig) (nativeTLSServer, error) {
			factoryCalls++
			return nil, nil
		},
	)
	if factoryCalls != 0 {
		t.Fatalf("factory calls = %d", factoryCalls)
	}
	assertTailnetErrorCode(t, err, ErrorCodeStateDirectoryInvalid)
}

func TestOpenNativeTLSRejectsMissingFactory(t *testing.T) {
	endpoint, err := openNativeTLS(
		context.Background(),
		validTestConfig(testStateDirectory(t)),
		emptyEnvironmentLookup,
		nil,
	)
	if endpoint != nil {
		t.Fatalf("endpoint = %#v", endpoint)
	}
	assertTailnetErrorCode(t, err, ErrorCodeServerCreateFailed)
}

func TestOpenNativeTLSRedactsFactoryFailure(t *testing.T) {
	endpoint, err := openNativeTLS(
		context.Background(),
		validTestConfig(testStateDirectory(t)),
		emptyEnvironmentLookup,
		func(normalizedConfig) (nativeTLSServer, error) {
			return nil, errTestRawTailscaleFailure
		},
	)
	if endpoint != nil {
		t.Fatalf("endpoint = %#v", endpoint)
	}
	assertTailnetErrorCode(t, err, ErrorCodeServerCreateFailed)
	assertErrorRenderingsExclude(t, err, testRawErrorCanary)
}

func TestOpenNativeTLSClosesServerReturnedWithFactoryFailure(t *testing.T) {
	server, _, _ := successfulTestServer()
	_, err := openNativeTLS(
		context.Background(),
		validTestConfig(testStateDirectory(t)),
		emptyEnvironmentLookup,
		func(normalizedConfig) (nativeTLSServer, error) {
			return server, errTestRawTailscaleFailure
		},
	)
	assertTailnetErrorCode(t, err, ErrorCodeServerCreateFailed)
	if server.closeCount.Load() != 1 {
		t.Fatalf("server close count = %d", server.closeCount.Load())
	}
}

func TestOpenNativeTLSRejectsNilServerFromFactory(t *testing.T) {
	_, err := openNativeTLS(
		context.Background(),
		validTestConfig(testStateDirectory(t)),
		emptyEnvironmentLookup,
		func(normalizedConfig) (nativeTLSServer, error) { return nil, nil },
	)
	assertTailnetErrorCode(t, err, ErrorCodeServerCreateFailed)
}

func TestOpenNativeTLSClosesExactlyOnceWhenUpFails(t *testing.T) {
	server, _, _ := successfulTestServer()
	server.upError = errTestRawTailscaleFailure
	endpoint, err := openNativeTLS(
		context.Background(),
		validTestConfig(testStateDirectory(t)),
		emptyEnvironmentLookup,
		func(normalizedConfig) (nativeTLSServer, error) { return server, nil },
	)
	if endpoint != nil {
		t.Fatalf("endpoint = %#v", endpoint)
	}
	assertTailnetErrorCode(t, err, ErrorCodeUpFailed)
	calls, _, _, _ := server.snapshot()
	if !reflect.DeepEqual(calls, []string{"up", "close"}) || server.closeCount.Load() != 1 {
		t.Fatalf("calls/close count = %#v/%d", calls, server.closeCount.Load())
	}
	assertErrorRenderingsExclude(t, err, testRawErrorCanary)
}

func TestOpenNativeTLSBoundsUpWithConfiguredTimeout(t *testing.T) {
	server, _, _ := successfulTestServer()
	server.upFunction = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	config := validTestConfig(testStateDirectory(t))
	config.UpTimeout = 5 * time.Millisecond
	startedAt := time.Now()
	_, err := openNativeTLS(
		context.Background(),
		config,
		emptyEnvironmentLookup,
		func(normalizedConfig) (nativeTLSServer, error) { return server, nil },
	)
	if time.Since(startedAt) > time.Second {
		t.Fatalf("bounded Up took %s", time.Since(startedAt))
	}
	assertTailnetErrorCode(t, err, ErrorCodeUpFailed)
	if server.closeCount.Load() != 1 {
		t.Fatalf("server close count = %d", server.closeCount.Load())
	}
}

func TestOpenNativeTLSClosesExactlyOnceWhenLocalClientFails(t *testing.T) {
	server, _, _ := successfulTestServer()
	server.localError = errTestRawTailscaleFailure
	_, err := openNativeTLS(
		context.Background(),
		validTestConfig(testStateDirectory(t)),
		emptyEnvironmentLookup,
		func(normalizedConfig) (nativeTLSServer, error) { return server, nil },
	)
	assertTailnetErrorCode(t, err, ErrorCodeLocalClientFailed)
	calls, _, _, _ := server.snapshot()
	if !reflect.DeepEqual(calls, []string{"up", "local_client", "close"}) || server.closeCount.Load() != 1 {
		t.Fatalf("calls/close count = %#v/%d", calls, server.closeCount.Load())
	}
}

func TestOpenNativeTLSClosesExactlyOnceWhenLocalClientIsNil(t *testing.T) {
	server, _, _ := successfulTestServer()
	server.whoIsClient = nil
	_, err := openNativeTLS(
		context.Background(),
		validTestConfig(testStateDirectory(t)),
		emptyEnvironmentLookup,
		func(normalizedConfig) (nativeTLSServer, error) { return server, nil },
	)
	assertTailnetErrorCode(t, err, ErrorCodeLocalClientFailed)
	if server.closeCount.Load() != 1 {
		t.Fatalf("server close count = %d", server.closeCount.Load())
	}
}

// TestPlanLexinetStep5Verify1Item4CertificateFailureIsFailClosed proves only
// the deterministic wrapper contract after ListenTLS reports a certificate
// prerequisite failure: close once and return no listener or raw fallback. It
// is not issuance, hostname, handshake, or real-Tailnet evidence.
func TestPlanLexinetStep5Verify1Item4CertificateFailureIsFailClosed(t *testing.T) {
	server, _, _ := successfulTestServer()
	server.listener = nil
	server.listenError = errors.New("certificate-unavailable-" + testSecretCanary)
	endpoint, err := openNativeTLS(
		context.Background(),
		validTestConfig(testStateDirectory(t)),
		emptyEnvironmentLookup,
		func(normalizedConfig) (nativeTLSServer, error) { return server, nil },
	)
	if endpoint != nil {
		t.Fatalf("certificate failure returned endpoint = %#v", endpoint)
	}
	assertTailnetErrorCode(t, err, ErrorCodeNativeTLSListenFailed)
	calls, _, network, address := server.snapshot()
	if !reflect.DeepEqual(calls, []string{"up", "local_client", "listen_tls", "close"}) {
		t.Fatalf("calls = %#v; a fallback path must not run", calls)
	}
	if network != NativeTLSNetwork || address != NativeTLSAddress || server.closeCount.Load() != 1 {
		t.Fatalf("ListenTLS/close = %q %q/%d", network, address, server.closeCount.Load())
	}
	assertErrorRenderingsExclude(t, err, testSecretCanary)
}

func TestOpenNativeTLSClosesServerWhenListenReturnsNilWithoutError(t *testing.T) {
	server, _, _ := successfulTestServer()
	server.listener = nil
	_, err := openNativeTLS(
		context.Background(),
		validTestConfig(testStateDirectory(t)),
		emptyEnvironmentLookup,
		func(normalizedConfig) (nativeTLSServer, error) { return server, nil },
	)
	assertTailnetErrorCode(t, err, ErrorCodeNativeTLSListenFailed)
	if server.closeCount.Load() != 1 {
		t.Fatalf("server close count = %d", server.closeCount.Load())
	}
}

func TestOpenNativeTLSClosesAdversarialListenerReturnedWithError(t *testing.T) {
	server, _, listener := successfulTestServer()
	server.listenError = errTestRawTailscaleFailure
	_, err := openNativeTLS(
		context.Background(),
		validTestConfig(testStateDirectory(t)),
		emptyEnvironmentLookup,
		func(normalizedConfig) (nativeTLSServer, error) { return server, nil },
	)
	assertTailnetErrorCode(t, err, ErrorCodeNativeTLSListenFailed)
	if listener.closeCount.Load() != 1 || server.closeCount.Load() != 1 {
		t.Fatalf("listener/server close count = %d/%d", listener.closeCount.Load(), server.closeCount.Load())
	}
}

func TestOpenNativeTLSCancelsAndJoinsHiddenListenTLSUpAtStartupBudget(t *testing.T) {
	server, _, _ := successfulTestServer()
	listenUnblock := make(chan struct{})
	server.listenFunction = func(string, string) (net.Listener, error) {
		<-listenUnblock
		return nil, context.Canceled
	}
	server.closeFunction = func() error {
		close(listenUnblock)
		return nil
	}
	config := validTestConfig(testStateDirectory(t))
	config.UpTimeout = 5 * time.Millisecond
	startedAt := time.Now()
	endpoint, err := openNativeTLS(
		context.Background(),
		config,
		emptyEnvironmentLookup,
		func(normalizedConfig) (nativeTLSServer, error) { return server, nil },
	)
	if endpoint != nil || time.Since(startedAt) > time.Second {
		t.Fatalf("endpoint/elapsed = %#v/%s", endpoint, time.Since(startedAt))
	}
	tailnetError := assertTailnetErrorCode(t, err, ErrorCodeNativeTLSListenFailed)
	if tailnetError.Context["reason"] != "startup_budget_exhausted" {
		t.Fatalf("error context = %#v", tailnetError.Context)
	}
	if server.closeCount.Load() != 1 {
		t.Fatalf("server close count = %d", server.closeCount.Load())
	}
	calls, _, _, _ := server.snapshot()
	if !reflect.DeepEqual(calls, []string{"up", "local_client", "listen_tls", "close"}) {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestOpenNativeTLSReportsCleanupFailureWithoutRenderingCause(t *testing.T) {
	server, _, _ := successfulTestServer()
	server.upError = errTestRawTailscaleFailure
	server.closeError = errors.New("cleanup-" + testSecretCanary)
	_, err := openNativeTLS(
		context.Background(),
		validTestConfig(testStateDirectory(t)),
		emptyEnvironmentLookup,
		func(normalizedConfig) (nativeTLSServer, error) { return server, nil },
	)
	tailnetError := assertTailnetErrorCode(t, err, ErrorCodeUpFailed)
	if tailnetError.Context["cleanup"] != "failed" {
		t.Fatalf("error context = %#v", tailnetError.Context)
	}
	assertErrorRenderingsExclude(t, err, testSecretCanary)
}

func TestNativeTLSEndpointCloseIsConcurrentAndExactlyOnce(t *testing.T) {
	server, whoIsClient, listener := successfulTestServer()
	endpoint := &NativeTLSEndpoint{listener: listener, whoIsClient: whoIsClient, server: server}

	const closerCount = 64
	var waitGroup sync.WaitGroup
	var failures atomic.Int32
	waitGroup.Add(closerCount)
	for range closerCount {
		go func() {
			defer waitGroup.Done()
			if err := endpoint.Close(); err != nil {
				failures.Add(1)
			}
		}()
	}
	waitGroup.Wait()
	if failures.Load() != 0 || server.closeCount.Load() != 1 {
		t.Fatalf("failures/server close count = %d/%d", failures.Load(), server.closeCount.Load())
	}
}

func TestNativeTLSEndpointCloseCachesRedactedFailure(t *testing.T) {
	server, whoIsClient, listener := successfulTestServer()
	server.closeError = errTestRawTailscaleFailure
	endpoint := &NativeTLSEndpoint{listener: listener, whoIsClient: whoIsClient, server: server}
	first := endpoint.Close()
	second := endpoint.Close()
	if first != second || server.closeCount.Load() != 1 {
		t.Fatalf("cached errors/close count = %p %p/%d", first, second, server.closeCount.Load())
	}
	assertTailnetErrorCode(t, first, ErrorCodeCloseFailed)
	assertErrorRenderingsExclude(t, first, testRawErrorCanary)
}

func TestNilNativeTLSEndpointAccessorsAndCloseAreSafe(t *testing.T) {
	var endpoint *NativeTLSEndpoint
	if endpoint.Listener() != nil || endpoint.WhoIsClient() != nil || endpoint.Close() != nil {
		t.Fatal("nil endpoint returned a non-nil value")
	}
}

func assertTailnetErrorCode(t *testing.T, err error, code ErrorCode) *Error {
	t.Helper()
	if !errors.Is(err, &Error{Code: code}) {
		t.Fatalf("error = %v, want %s", err, code)
	}
	var tailnetError *Error
	if !errors.As(err, &tailnetError) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	return tailnetError
}

func assertErrorRenderingsExclude(t *testing.T, err error, forbidden string) {
	t.Helper()
	encoded, marshalError := json.Marshal(err)
	if marshalError != nil {
		t.Fatalf("marshal error: %v", marshalError)
	}
	for _, rendering := range []string{err.Error(), string(encoded)} {
		if strings.Contains(rendering, forbidden) {
			t.Fatalf("error rendering leaked forbidden material: %q", rendering)
		}
	}
}
