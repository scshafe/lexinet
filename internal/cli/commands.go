package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"lexinet/internal/config"
	"lexinet/internal/server"
	"lexinet/internal/storage"
)

func newVersionCommand(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.JSON {
				return writeJSON(cmd.OutOrStdout(), map[string]string{"version": opts.Version})
			}
			fmt.Fprintln(cmd.OutOrStdout(), opts.Version)
			return nil
		},
	}
}

func newPingCommand(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "ping",
		Short: "Check service health",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := config.Load()
			if err != nil {
				return err
			}
			endpoint := cfg.Endpoint(opts.Endpoint)
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/healthz", nil)
			if err != nil {
				return &ExitError{Code: ExitUsage, Err: err}
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return &ExitError{Code: ExitUnreachable, Err: fmt.Errorf("endpoint unreachable: %w", err)}
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return &ExitError{Code: ExitUnreachable, Err: fmt.Errorf("endpoint returned %s", resp.Status)}
			}
			if opts.JSON {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"ok": true, "endpoint": endpoint, "status": resp.StatusCode})
			}
			fmt.Fprintln(cmd.OutOrStdout(), "ok")
			return nil
		},
	}
}

func newMigrateCommand(opts *Options) *cobra.Command {
	var db string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply embedded database migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := config.Load()
			if err != nil {
				return err
			}
			dsn := db
			if dsn == "" {
				if dsn, err = cfg.DatabaseDSN(); err != nil {
					return &ExitError{Code: ExitValidation, Err: err}
				}
			}
			if err := storage.Migrate(cmd.Context(), dsn); err != nil {
				return err
			}
			if opts.JSON {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"ok": true})
			}
			fmt.Fprintln(cmd.OutOrStdout(), "migrations applied")
			return nil
		},
	}
	cmd.Flags().StringVar(&db, "db", "", "database DSN")
	return cmd
}

type serveCommandRuntime struct {
	migrate func(context.Context, string) error
	prepare func(server.Config) (*server.PreparedConfiguration, error)
	serve   func(context.Context, *server.PreparedConfiguration) error
}

func productionServeCommandRuntime() serveCommandRuntime {
	return serveCommandRuntime{
		migrate: storage.Migrate,
		prepare: server.PrepareConfiguration,
		serve:   server.ServePrepared,
	}
}

func newServeCommand(opts *Options, runtime serveCommandRuntime) *cobra.Command {
	var bind string
	var port int
	var db string
	var tailnetMode string
	var skipMigrate bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the lexinet HTTP service",
		RunE: func(cmd *cobra.Command, args []string) error {
			if runtime.prepare == nil || runtime.migrate == nil || runtime.serve == nil {
				return &ExitError{Code: ExitValidation, Err: errors.New("serve runtime is unavailable")}
			}
			cfg, _, err := config.Load()
			if err != nil {
				return err
			}
			if db != "" {
				cfg.Service.Database.DSN = db
			} else if cfg.Service.Database.DSN, err = cfg.DatabaseDSN(); err != nil {
				return &ExitError{Code: ExitValidation, Err: err}
			}
			if bind != "" {
				cfg.Service.ListenLoopback = bind
			}
			if port != 0 {
				host, _, err := net.SplitHostPort(cfg.Service.ListenLoopback)
				if err != nil {
					return &ExitError{Code: ExitUsage, Err: err}
				}
				cfg.Service.ListenLoopback = net.JoinHostPort(host, strconv.Itoa(port))
			}
			switch strings.ToLower(tailnetMode) {
			case "", "auto":
			case "on", "true", "1":
				cfg.Service.TailnetEnabled = true
			case "off", "false", "0":
				cfg.Service.TailnetEnabled = false
			default:
				return &ExitError{Code: ExitUsage, Err: fmt.Errorf("invalid --tailnet %q", tailnetMode)}
			}
			effectiveServerConfig := server.Config{
				Version:                      opts.Version,
				ListenLoopback:               cfg.Service.ListenLoopback,
				AuthToken:                    cfg.Service.AuthToken,
				DatabaseDSN:                  cfg.Service.Database.DSN,
				SearchStrategies:             cfg.Search.StrategyProvider(),
				TailnetEnabled:               cfg.Service.TailnetEnabled,
				TailnetHostname:              cfg.Service.TailnetHostname,
				TailnetStateDir:              cfg.Service.TailnetStateDir,
				TailnetAuthenticationMode:    cfg.Service.TailnetStudyAuthentication.Mode,
				TailnetRequiredNodeTag:       cfg.Service.TailnetStudyAuthentication.RequiredNodeTag,
				TailnetApplicationCapability: cfg.Service.TailnetStudyAuthentication.ApplicationCapability,
				TailnetAllowedProfileRefs:    append([]string(nil), cfg.Service.TailnetStudyAuthentication.AllowedProfileRefs...),
				TailnetScopedTokenSubjectID:  cfg.Service.TailnetStudyAuthentication.ScopedTokenSubjectID,
				TailnetScopedTokenFile:       cfg.Service.TailnetStudyAuthentication.ScopedTokenFile,
			}
			preparedServer, err := runtime.prepare(effectiveServerConfig)
			if err != nil {
				return &ExitError{Code: ExitValidation, Err: err}
			}
			if !skipMigrate {
				if err := runtime.migrate(cmd.Context(), cfg.Service.Database.DSN); err != nil {
					return err
				}
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			fmt.Fprintf(cmd.ErrOrStderr(), "serving loopback on %s\n", cfg.Service.ListenLoopback)
			return runtime.serve(ctx, preparedServer)
		},
	}
	cmd.Flags().StringVar(&bind, "bind", "", "loopback listen address")
	cmd.Flags().IntVar(&port, "port", 0, "listen port")
	cmd.Flags().StringVar(&db, "db", "", "database DSN")
	cmd.Flags().StringVar(&tailnetMode, "tailnet", "", "tailnet listener mode: on, off, or auto")
	cmd.Flags().BoolVar(&skipMigrate, "skip-migrate", false, "skip embedded migration on service start")
	return cmd
}

func newConfigCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Manage lexinet configuration"}
	cmd.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print config path",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := config.Path()
			if err != nil {
				return err
			}
			if opts.JSON {
				return writeJSON(cmd.OutOrStdout(), map[string]string{"path": path})
			}
			fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show config with secrets redacted",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := config.Load()
			if err != nil {
				return err
			}
			if os.Getenv("LEXINET_TOKEN") != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: LEXINET_TOKEN is set; token files take precedence and config show redacts persisted tokens")
			}
			redacted := cfg.Redacted()
			if opts.JSON {
				return writeJSON(cmd.OutOrStdout(), redacted)
			}
			return toml.NewEncoder(cmd.OutOrStdout()).Encode(redacted)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a config value",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := config.Load()
			if err != nil {
				return err
			}
			if err := cfg.Set(args[0], args[1]); err != nil {
				return &ExitError{Code: ExitUsage, Err: err}
			}
			if err := cfg.Save(path); err != nil {
				return err
			}
			if opts.JSON {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"ok": true, "key": args[0]})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "set %s\n", args[0])
			return nil
		},
	})
	return cmd
}
