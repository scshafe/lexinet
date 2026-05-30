package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

const (
	ExitOK          = 0
	ExitUnexpected  = 1
	ExitUsage       = 2
	ExitValidation  = 3
	ExitAmbiguous   = 4
	ExitUnreachable = 5
	ExitAuth        = 6
	ExitNotFound    = 7
)

type Options struct {
	JSON      bool
	NoColor   bool
	Endpoint  string
	TokenFile string
	Version   string
}

type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

func Execute(version string) int {
	opts := &Options{Version: version}
	cmd := NewRoot(opts)
	if err := cmd.Execute(); err != nil {
		code := exitCode(err)
		writeCLIError(os.Stderr, opts.JSON, code, err)
		return code
	}
	return ExitOK
}

func NewRoot(opts *Options) *cobra.Command {
	root := &cobra.Command{
		Use:           "lexinet",
		Short:         "Idea-centric technical lexicon network",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &ExitError{Code: ExitUsage, Err: err}
	})
	root.PersistentFlags().BoolVar(&opts.JSON, "json", false, "emit JSON output")
	root.PersistentFlags().BoolVar(&opts.NoColor, "no-color", false, "disable colored output")
	root.PersistentFlags().StringVar(&opts.Endpoint, "endpoint", "", "lexinet service endpoint")
	root.PersistentFlags().StringVar(&opts.TokenFile, "token-file", "", "file containing bearer token")
	root.AddCommand(
		newServeCommand(opts),
		newMigrateCommand(opts),
		newPingCommand(opts),
		newVersionCommand(opts),
		newConfigCommand(opts),
	)
	return root
}

func exitCode(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) && ee.Code != 0 {
		return ee.Code
	}
	return ExitUnexpected
}

func writeCLIError(w io.Writer, jsonOut bool, code int, err error) {
	if jsonOut {
		_ = writeJSON(w, map[string]any{
			"error": map[string]any{
				"code":      code,
				"message":   err.Error(),
				"exit_code": code,
			},
		})
		return
	}
	fmt.Fprintln(w, err.Error())
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}
