package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lexinet/internal/model"
	searchcfg "lexinet/internal/search"
	"lexinet/internal/storage"
	"lexinet/internal/study"
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
}

const ErrorCodeTailnetAuthTokenRequired = "CONFIG.TAILNET_AUTH_TOKEN_REQUIRED"

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

// ValidateConfiguration rejects unsafe effective server configurations before external work.
// It returns a ConfigurationError with a stable code and never includes bearer-token material.
// @canonical server_configuration_validation -- all server startup paths must use this function.
func ValidateConfiguration(cfg Config) error {
	if cfg.TailnetEnabled && strings.TrimSpace(cfg.AuthToken) == "" {
		return &ConfigurationError{
			Code:    ErrorCodeTailnetAuthTokenRequired,
			Message: "service.auth_token must be non-empty when service.tailnet_enabled is true",
			Context: map[string]any{
				"configuration_field": "service.auth_token",
				"tailnet_enabled":     true,
			},
		}
	}
	return nil
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
	if err := ValidateConfiguration(cfg); err != nil {
		return err
	}
	if cfg.ListenLoopback == "" {
		cfg.ListenLoopback = "127.0.0.1:17892"
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
	studyService, err := studysession.NewStudySessionService(
		studyRepository,
		studysession.OperatorStudyAuthorizer{},
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

	errCh := make(chan error, 2)
	go func() {
		errCh <- serveHTTP(loopback, loopbackLn)
	}()

	var tailHTTP *http.Server
	var tailClose func() error
	if cfg.TailnetEnabled {
		port, err := portFromAddr(cfg.ListenLoopback)
		if err != nil {
			_ = loopback.Shutdown(context.Background())
			return err
		}
		ln, closeFn, err := tailnet.Listen(tailnet.Config{
			Hostname: cfg.TailnetHostname,
			StateDir: cfg.TailnetStateDir,
			Port:     port,
		})
		if err != nil {
			_ = loopback.Shutdown(context.Background())
			return err
		}
		tailClose = closeFn
		tailHTTP = NewHTTPServer(handler)
		go func() {
			errCh <- serveHTTP(tailHTTP, ln)
		}()
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		loopErr := loopback.Shutdown(shutdownCtx)
		var tailErr error
		if tailHTTP != nil {
			tailErr = tailHTTP.Shutdown(shutdownCtx)
		}
		if tailClose != nil {
			if err := tailClose(); err != nil && tailErr == nil {
				tailErr = err
			}
		}
		return errors.Join(loopErr, tailErr, ctx.Err())
	case err := <-errCh:
		return err
	}
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

func serveHTTP(srv *http.Server, ln net.Listener) error {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func portFromAddr(addr string) (string, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	return port, nil
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
