package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lexinet/internal/storage"
	"lexinet/internal/tailnet"
)

type Config struct {
	Version         string
	ListenLoopback  string
	AuthToken       string
	DatabaseDSN     string
	TailnetEnabled  bool
	TailnetHostname string
	TailnetStateDir string
}

type ErrorEnvelope struct {
	Error APIError `json:"error"`
}

type APIError struct {
	Code       string   `json:"code"`
	Message    string   `json:"message"`
	Candidates []string `json:"candidates,omitempty"`
}

func Router(cfg Config, store Store) http.Handler {
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

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "route not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	})
	return r
}

func Serve(ctx context.Context, cfg Config) error {
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
	handler := Router(cfg, storage.NewRepository(pool))
	loopback := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
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
		tailHTTP = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
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
