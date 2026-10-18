package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"tailscale.com/tailcfg"

	"lexinet/internal/model"
	searchcfg "lexinet/internal/search"
	"lexinet/internal/storage"
	"lexinet/internal/study"
	"lexinet/internal/studyauth"
	"lexinet/internal/studysession"
	"lexinet/internal/tailnet"
)

type Config struct {
	Version          string
	ListenLoopback   string
	AuthToken        string
	DatabaseDSN      string
	SearchStrategies searchcfg.StrategyProvider
	TailnetEnabled   bool
	TailnetHostname  string
	TailnetStateDir  string
	TailnetUpTimeout time.Duration

	TailnetAuthenticationMode    string
	TailnetRequiredNodeTag       string
	TailnetApplicationCapability string
	TailnetAllowedProfileRefs    []string
	TailnetScopedTokenSubjectID  string
	TailnetScopedTokenFile       string
}

const (
	ErrorCodeTailnetStudyAuthenticationInvalid = "CONFIG.TAILNET_STUDY_AUTHENTICATION_INVALID"
	ErrorCodeTailnetTransportInvalid           = "CONFIG.TAILNET_TRANSPORT_INVALID"
)

// ConfigurationError describes a stable, machine-readable server configuration failure.
type ConfigurationError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Context map[string]any `json:"context"`
}

func (e *ConfigurationError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// PreparedConfiguration is the detached, validated startup authority. Scoped
// token bytes are read exactly once before migrations and retained only as a
// digest inside tailnetAuthentication.
type PreparedConfiguration struct {
	config                Config
	tailnetAuthentication *studyauth.Boundary
	tailnetTransport      tailnet.Config
}

// PrepareConfiguration is the @canonical server_configuration_validation and
// startup preparation boundary. Production callers must retain its result
// across migrations and pass it to ServePrepared rather than re-reading
// credential files or reparsing policy afterward.
func PrepareConfiguration(cfg Config) (*PreparedConfiguration, error) {
	if cfg.ListenLoopback == "" {
		cfg.ListenLoopback = "127.0.0.1:17892"
	}
	cfg.TailnetAllowedProfileRefs = append([]string(nil), cfg.TailnetAllowedProfileRefs...)
	prepared := &PreparedConfiguration{config: cfg}
	if !cfg.TailnetEnabled {
		return prepared, nil
	}

	policy, err := tailnetStudyAuthenticationPolicy(cfg)
	if err != nil {
		return nil, err
	}
	boundary, err := studyauth.NewBoundary(policy)
	if err != nil {
		return nil, tailnetAuthenticationConfigurationError()
	}
	transportConfig := tailnet.Config{
		Hostname:  cfg.TailnetHostname,
		StateDir:  cfg.TailnetStateDir,
		UpTimeout: cfg.TailnetUpTimeout,
	}
	if err := tailnet.ValidateNativeTLSConfiguration(transportConfig); err != nil {
		return nil, &ConfigurationError{
			Code:    ErrorCodeTailnetTransportInvalid,
			Message: "native Tailnet TLS configuration is invalid",
			Context: map[string]any{"configuration_field": "service.tailnet_transport"},
		}
	}
	prepared.tailnetAuthentication = boundary
	prepared.tailnetTransport = transportConfig
	return prepared, nil
}

// ValidateConfiguration preserves the prior validation entry point for tests
// and integrations that do not need to retain prepared credential authority.
// Production serve composition uses PrepareConfiguration exactly once.
func ValidateConfiguration(cfg Config) error {
	_, err := PrepareConfiguration(cfg)
	return err
}

func tailnetStudyAuthenticationPolicy(cfg Config) (studyauth.Policy, error) {
	var subjectID uuid.UUID
	if cfg.TailnetScopedTokenSubjectID != "" {
		parsed, err := uuid.Parse(cfg.TailnetScopedTokenSubjectID)
		if err != nil || parsed == uuid.Nil || parsed.String() != cfg.TailnetScopedTokenSubjectID {
			return studyauth.Policy{}, tailnetAuthenticationConfigurationError()
		}
		subjectID = parsed
	}
	return studyauth.Policy{
		Mode:                 studyauth.AuthenticationMode(cfg.TailnetAuthenticationMode),
		RequiredNodeTag:      cfg.TailnetRequiredNodeTag,
		AppCapability:        tailcfg.PeerCapability(cfg.TailnetApplicationCapability),
		AllowedProfileRefs:   append([]string(nil), cfg.TailnetAllowedProfileRefs...),
		ScopedTokenSubjectID: subjectID,
		ScopedTokenFile:      cfg.TailnetScopedTokenFile,
	}, nil
}

func tailnetAuthenticationConfigurationError() *ConfigurationError {
	return &ConfigurationError{
		Code:    ErrorCodeTailnetStudyAuthenticationInvalid,
		Message: "Tailnet study authentication configuration is invalid",
		Context: map[string]any{"configuration_field": "service.tailnet_study_authentication"},
	}
}

type ErrorEnvelope struct {
	Error APIError `json:"error"`
}

type APIError struct {
	Code       string   `json:"code"`
	Message    string   `json:"message"`
	Candidates []string `json:"candidates,omitempty"`
}

type routerConfiguration struct {
	studySessionService StudySessionService
	studyRequestID      studyRequestIDGenerator
}

// RouterOption extends the server router without widening the legacy Store.
type RouterOption func(*routerConfiguration)

// WithStudySessionService mounts the dedicated authenticated study.v1 surface.
func WithStudySessionService(service StudySessionService) RouterOption {
	return func(configuration *routerConfiguration) {
		configuration.studySessionService = service
	}
}

type studyRequestIDGenerator func() (string, error)

func withStudyRequestIDGenerator(generator studyRequestIDGenerator) RouterOption {
	return func(configuration *routerConfiguration) {
		configuration.studyRequestID = generator
	}
}

func Router(cfg Config, store Store, options ...RouterOption) http.Handler {
	routerConfig := routerConfiguration{studyRequestID: randomStudyRequestID}
	for _, option := range options {
		if option != nil {
			option(&routerConfig)
		}
	}
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	r.Get("/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": cfg.Version})
	})

	r.Group(func(r chi.Router) {
		r.Use(requireBearer(cfg.AuthToken))
		registerAPIRoutes(r, store)
		r.Get("/api/v1/ping", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		})
	})
	if routerConfig.studySessionService != nil {
		r.Route("/api/v1/study", func(studyRouter chi.Router) {
			studyRouter.Use(assignStudyRequestID(routerConfig.studyRequestID))
			studyRouter.Use(recoverStudyPanic)
			studyRouter.Use(requireStudyBearerAuthentication(cfg.AuthToken))
			registerStudyAPIRoutes(studyRouter, routerConfig.studySessionService)
		})
	}

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "route not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	})
	return r
}

func Serve(ctx context.Context, cfg Config) error {
	prepared, err := PrepareConfiguration(cfg)
	if err != nil {
		return err
	}
	return ServePrepared(ctx, prepared)
}

// ServePrepared starts the loopback operator surface and, when configured, a
// separate native-TLS Tailnet study surface from one prevalidated authority.
// Both listeners are opened before either HTTP server begins accepting work.
func ServePrepared(ctx context.Context, prepared *PreparedConfiguration) error {
	if ctx == nil || prepared == nil {
		return &ConfigurationError{
			Code:    ErrorCodeTailnetTransportInvalid,
			Message: "server startup configuration is unavailable",
			Context: map[string]any{"configuration_field": "prepared_configuration"},
		}
	}
	cfg := prepared.config
	if cfg.TailnetEnabled && prepared.tailnetAuthentication == nil {
		return tailnetAuthenticationConfigurationError()
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseDSN)
	if err != nil {
		return err
	}
	poolCfg.ConnConfig.RuntimeParams["application_name"] = "lexinet"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	repository := storage.NewRepository(pool, storage.WithSearchStrategyProvider(cfg.SearchStrategies))
	studyRepository, err := studysession.NewStorageRepositoryAdapter(repository)
	if err != nil {
		return err
	}
	studyScheduler, err := study.NewVarietyFirstV1()
	if err != nil {
		return err
	}
	studyAuthorizer := studysession.StudyAuthorizer(studysession.OperatorStudyAuthorizer{})
	if cfg.TailnetEnabled {
		studyAuthorizer = prepared.tailnetAuthentication
	}
	studyService, err := studysession.NewStudySessionService(
		studyRepository,
		studyAuthorizer,
		studyScheduler,
		studysession.NewSystemStudyAuthoritySource(),
		nil,
	)
	if err != nil {
		return err
	}
	handler := Router(cfg, repository, WithStudySessionService(studyService))
	loopback := NewHTTPServer(handler)
	loopbackLn, err := net.Listen("tcp", cfg.ListenLoopback)
	if err != nil {
		return err
	}

	var tailHTTP *http.Server
	var tailEndpoint *tailnet.NativeTLSEndpoint
	if cfg.TailnetEnabled {
		tailEndpoint, err = tailnet.OpenNativeTLS(ctx, prepared.tailnetTransport)
		if err != nil {
			_ = loopbackLn.Close()
			return err
		}
		tailHandler, routerErr := NewTailnetStudyRouter(
			studyService,
			prepared.tailnetAuthentication,
			tailEndpoint.WhoIsClient(),
		)
		if routerErr != nil {
			_ = tailEndpoint.Close()
			_ = loopbackLn.Close()
			return routerErr
		}
		tailHTTP = NewHTTPServer(tailHandler)
	}

	if tailHTTP != nil {
		return serveHTTPServerLifecycle(
			ctx,
			loopback,
			loopbackLn,
			tailHTTP,
			tailEndpoint.Listener(),
			tailEndpoint,
		)
	}
	return serveHTTPServerLifecycle(ctx, loopback, loopbackLn, nil, nil, nil)
}

type httpServerLifecycle interface {
	Serve(net.Listener) error
	Shutdown(context.Context) error
	Close() error
}

// serveHTTPServerLifecycle is the @canonical http_server_lifecycle boundary.
// It coordinates both HTTP servers as one failure domain: either server exit
// or parent cancellation shuts down, force-closes when required, and joins all
// started servers before returning.
func serveHTTPServerLifecycle(
	ctx context.Context,
	loopbackServer httpServerLifecycle,
	loopbackListener net.Listener,
	tailnetServer httpServerLifecycle,
	tailnetListener net.Listener,
	tailnetEndpoint io.Closer,
) error {
	type serveResult struct {
		err error
	}
	serverCount := 1
	if tailnetServer != nil {
		serverCount++
	}
	errCh := make(chan serveResult, serverCount)
	go func() {
		errCh <- serveResult{err: serveHTTP(loopbackServer, loopbackListener)}
	}()
	if tailnetServer != nil {
		go func() {
			errCh <- serveResult{err: serveHTTP(tailnetServer, tailnetListener)}
		}()
	}

	var triggerError error
	resultsReceived := 0
	select {
	case <-ctx.Done():
		triggerError = ctx.Err()
	case result := <-errCh:
		resultsReceived = 1
		triggerError = result.err
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	type shutdownResult struct {
		tailnet bool
		err     error
	}
	shutdownResultCount := 1
	shutdownResults := make(chan shutdownResult, serverCount)
	go func() {
		shutdownResults <- shutdownResult{err: loopbackServer.Shutdown(shutdownCtx)}
	}()
	if tailnetServer != nil {
		shutdownResultCount++
		go func() {
			shutdownResults <- shutdownResult{tailnet: true, err: tailnetServer.Shutdown(shutdownCtx)}
		}()
	}
	var loopbackShutdownError error
	var tailnetShutdownError error
	for range shutdownResultCount {
		result := <-shutdownResults
		if result.tailnet {
			tailnetShutdownError = result.err
		} else {
			loopbackShutdownError = result.err
		}
	}
	cancelShutdown()
	var loopbackServerCloseError error
	if loopbackShutdownError != nil {
		loopbackServerCloseError = loopbackServer.Close()
	}
	var tailnetServerCloseError error
	if tailnetServer != nil && tailnetShutdownError != nil {
		tailnetServerCloseError = tailnetServer.Close()
	}
	var tailnetEndpointCloseError error
	if tailnetEndpoint != nil {
		tailnetEndpointCloseError = tailnetEndpoint.Close()
	}
	for resultsReceived < serverCount {
		result := <-errCh
		resultsReceived++
		if result.err != nil {
			triggerError = errors.Join(triggerError, result.err)
		}
	}
	return errors.Join(
		triggerError,
		loopbackShutdownError,
		tailnetShutdownError,
		loopbackServerCloseError,
		tailnetServerCloseError,
		tailnetEndpointCloseError,
	)
}

// NewHTTPServer is the single host-service timeout and header-budget authority.
func NewHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 * 1024,
	}
}

var studyFallbackRequestIDSequence atomic.Uint64

func assignStudyRequestID(generator studyRequestIDGenerator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Request-ID", generateStudyRequestID(generator))
			next.ServeHTTP(w, r)
		})
	}
}

func randomStudyRequestID() (string, error) {
	requestID, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return requestID.String(), nil
}

func generateStudyRequestID(generator studyRequestIDGenerator) (requestID string) {
	if generator != nil {
		func() {
			defer func() {
				if recover() != nil {
					requestID = ""
				}
			}()
			generated, err := generator()
			if err == nil && validStudyRequestID(generated) {
				requestID = generated
			}
		}()
	}
	if requestID != "" {
		return requestID
	}

	var fallbackInput [16]byte
	binary.BigEndian.PutUint64(fallbackInput[:8], uint64(time.Now().UTC().UnixNano()))
	binary.BigEndian.PutUint64(fallbackInput[8:], studyFallbackRequestIDSequence.Add(1))
	digest := sha256.Sum256(fallbackInput[:])
	return "study-fallback-" + hex.EncodeToString(digest[:16])
}

func validStudyRequestID(requestID string) bool {
	if len(requestID) < 1 || len(requestID) > 128 || strings.TrimSpace(requestID) != requestID {
		return false
	}
	for _, character := range requestID {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func recoverStudyPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				writeStudyError(w, studyTransportError(model.StudyErrorCodeInternal))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func serveHTTP(srv httpServerLifecycle, ln net.Listener) error {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func requireBearer(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if token == "" {
			return next
		}
		expected := "Bearer " + token
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actual := r.Header.Get("Authorization")
			actualHash := sha256.Sum256([]byte(actual))
			expectedHash := sha256.Sum256([]byte(expected))
			matches := subtle.ConstantTimeCompare(actualHash[:], expectedHash[:]) == 1 && subtle.ConstantTimeEq(int32(len(actual)), int32(len(expected))) == 1
			if !matches {
				writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing or invalid bearer token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorEnvelope{Error: APIError{Code: strings.ToUpper(code), Message: message}})
}
