package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"lexinet/internal/tailnet"
)

const maximumEnrollmentKeyBytes = 1024

func newTailnetCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{Use: "tailnet", Short: "Native Tailnet identity operations"}
	cmd.AddCommand(newTailnetEnrollCommand(opts, tailnet.Enroll))
	return cmd
}

type enrollFunc func(context.Context, tailnet.Config, string) error

func newTailnetEnrollCommand(opts *Options, enroll enrollFunc) *cobra.Command {
	var stateDir, hostname string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "Enroll the native Tailnet identity directory once (key on stdin)",
		Long: "Reads one single-use Tailscale auth key from stdin, enrolls the node into\n" +
			"--state-dir (created 0700; must be empty), and exits. The key is never taken\n" +
			"from arguments or the environment and is never printed. serve then runs\n" +
			"against the enrolled directory.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if stateDir == "" || hostname == "" {
				return &ExitError{Code: ExitUsage, Err: errors.New("--state-dir and --hostname are required")}
			}
			raw, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), maximumEnrollmentKeyBytes+1))
			if err != nil {
				return &ExitError{Code: ExitUsage, Err: errors.New("could not read the auth key from stdin")}
			}
			if len(raw) > maximumEnrollmentKeyBytes {
				return &ExitError{Code: ExitUsage, Err: errors.New("auth key on stdin is too long")}
			}
			key := strings.TrimSpace(string(raw))
			if key == "" {
				return &ExitError{Code: ExitUsage, Err: errors.New("no auth key on stdin")}
			}
			if err := enroll(cmd.Context(), tailnet.Config{Hostname: hostname, StateDir: stateDir, UpTimeout: timeout}, key); err != nil {
				return &ExitError{Code: ExitValidation, Err: err}
			}
			if opts.JSON {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"ok": true, "hostname": hostname})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "enrolled %s\n", hostname)
			return nil
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "identity state directory (created 0700, must be empty)")
	cmd.Flags().StringVar(&hostname, "hostname", "", "Tailnet hostname for this node")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "enrollment budget (default 30s, max 2m)")
	return cmd
}
