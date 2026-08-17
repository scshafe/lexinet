package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"lexinet/internal/server"
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
	Compact   bool
	Quiet     bool
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
	root.PersistentFlags().BoolVar(&opts.Compact, "compact", false, "emit compact JSON")
	root.PersistentFlags().BoolVarP(&opts.Quiet, "quiet", "q", false, "emit only IDs where possible")
	root.PersistentFlags().BoolVar(&opts.NoColor, "no-color", false, "disable colored output")
	root.PersistentFlags().StringVar(&opts.Endpoint, "endpoint", "", "lexinet service endpoint")
	root.PersistentFlags().StringVar(&opts.TokenFile, "token-file", "", "file containing bearer token")
	root.AddCommand(
		newStudyCommand(opts),
		newSubmitCommand(opts),
		newSubmissionCommand(opts),
		newIdeaCommand(opts),
		newTopicCommand(opts),
		newAcronymCommand(opts),
		newNameCommand(opts),
		newBodyCommand(opts, "def"),
		newBodyCommand(opts, "example"),
		newBodyCommand(opts, "note"),
		newLinkCommand(opts),
		newTraverseCommand(opts),
		newSearchCommand(opts),
		newServeCommand(opts, productionServeCommandRuntime()),
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
		var configurationError *server.ConfigurationError
		if errors.As(err, &configurationError) {
			_ = writeJSON(w, map[string]any{
				"error": map[string]any{
					"code":      configurationError.Code,
					"message":   configurationError.Message,
					"context":   configurationError.Context,
					"exit_code": code,
				},
			})
			return
		}
		var studyClientError *StudyClientError
		if errors.As(err, &studyClientError) {
			safeStudyClientError := studyClientError.sanitized()
			errorValue := map[string]any{
				"message":   safeStudyClientError.Error(),
				"retryable": safeStudyClientError.Retryable,
				"exit_code": code,
			}
			if safeStudyClientError.StatusCode != 0 {
				errorValue["status"] = safeStudyClientError.StatusCode
			}
			if safeStudyClientError.Code.Valid() {
				errorValue["code"] = safeStudyClientError.Code
			}
			if safeStudyClientError.RequestID != "" {
				errorValue["request_id"] = safeStudyClientError.RequestID
			}
			if safeStudyClientError.RetryAfter != "" {
				errorValue["retry_after"] = safeStudyClientError.RetryAfter
			}
			_ = writeJSON(w, map[string]any{"error": errorValue})
			return
		}
		var serviceErr *ServiceError
		if errors.As(err, &serviceErr) {
			errorValue := map[string]any{
				"code":      serviceErr.Code,
				"message":   serviceErr.Message,
				"exit_code": code,
			}
			if len(serviceErr.Candidates) > 0 {
				errorValue["candidates"] = serviceErr.Candidates
			}
			_ = writeJSON(w, map[string]any{"error": errorValue})
			return
		}
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
