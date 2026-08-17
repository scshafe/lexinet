package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"lexinet/internal/studysession"
)

func newStudyCommand(opts *Options) *cobra.Command {
	command := &cobra.Command{
		Use:   "study",
		Short: "Manage authoritative study sessions",
		Args:  studyCommandExactArgs(0),
	}
	command.AddCommand(
		newStudySessionCommand(opts),
		newStudySyncCommand(opts),
		newStudyStateCommand(opts),
	)
	return command
}

func newStudySessionCommand(opts *Options) *cobra.Command {
	var requestFile string
	command := &cobra.Command{
		Use:   "session --request FILE|-",
		Short: "Create or resume an authoritative study session",
		Args:  studyCommandExactArgs(0),
		RunE: func(command *cobra.Command, _ []string) error {
			requestJSON, err := readStudyRequestFile(requestFile, command.InOrStdin())
			if err != nil {
				return err
			}
			client, err := studyClientFromOptions(opts)
			if err != nil {
				return err
			}
			responseJSON, err := client.post(command.Context(), "/api/v1/study/sessions", requestJSON, studyClientSuccessCreateSession)
			if err != nil {
				return err
			}
			return writeStudyRawResponse(command.OutOrStdout(), responseJSON)
		},
	}
	command.Flags().StringVar(&requestFile, "request", "", "JSON request file or - for stdin")
	return command
}

func newStudySyncCommand(opts *Options) *cobra.Command {
	var requestFile string
	command := &cobra.Command{
		Use:   "sync SESSION_ID --request FILE|-",
		Short: "Atomically synchronize one study event window",
		Args:  studyCommandExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			sessionID, err := parseStudyCommandSessionID(args[0])
			if err != nil {
				return err
			}
			requestJSON, err := readStudyRequestFile(requestFile, command.InOrStdin())
			if err != nil {
				return err
			}
			client, err := studyClientFromOptions(opts)
			if err != nil {
				return err
			}
			responseJSON, err := client.post(command.Context(), "/api/v1/study/sessions/"+sessionID.String()+"/sync", requestJSON, studyClientSuccessSync)
			if err != nil {
				return err
			}
			return writeStudyRawResponse(command.OutOrStdout(), responseJSON)
		},
	}
	command.Flags().StringVar(&requestFile, "request", "", "JSON request file or - for stdin")
	return command
}

func newStudyStateCommand(opts *Options) *cobra.Command {
	afterPosition := -1
	limit := 16
	command := &cobra.Command{
		Use:   "state SESSION_ID [--after-position N] [--limit N]",
		Short: "Fetch one immutable page from an issued study session",
		Args:  studyCommandExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			sessionID, err := parseStudyCommandSessionID(args[0])
			if err != nil {
				return err
			}
			if afterPosition < -1 || int64(afterPosition) > int64(2_147_483_647) {
				return usageError("--after-position must be between -1 and 2147483647")
			}
			if limit < 1 || limit > studysession.StudyMaximumPageItems {
				return usageError("--limit must be between 1 and %d", studysession.StudyMaximumPageItems)
			}
			client, err := studyClientFromOptions(opts)
			if err != nil {
				return err
			}
			query := url.Values{
				"page_after_position": []string{strconv.Itoa(afterPosition)},
				"page_limit":          []string{strconv.Itoa(limit)},
			}
			responseJSON, err := client.get(command.Context(), "/api/v1/study/sessions/"+sessionID.String(), query, studyClientSuccessSessionState)
			if err != nil {
				return err
			}
			return writeStudyRawResponse(command.OutOrStdout(), responseJSON)
		},
	}
	command.Flags().IntVar(&afterPosition, "after-position", afterPosition, "last issued item position, or -1 from the beginning")
	command.Flags().IntVar(&limit, "limit", limit, "maximum issued items to return")
	return command
}

func studyClientFromOptions(opts *Options) (*studyHTTPClient, error) {
	if opts == nil {
		return nil, &ExitError{Code: ExitValidation, Err: newStudyClientError(studyClientFailureConfiguration, 0, "", "")}
	}
	endpoint, token, err := endpointAndToken(opts)
	if err != nil {
		return nil, &ExitError{Code: ExitValidation, Err: newStudyClientError(studyClientFailureConfiguration, 0, "", "")}
	}
	return newStudyHTTPClient(endpoint, token)
}

func studyCommandExactArgs(expected int) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != expected {
			return usageError("study command accepts %d argument(s), received %d", expected, len(args))
		}
		return nil
	}
}

func parseStudyCommandSessionID(value string) (uuid.UUID, error) {
	if strings.TrimSpace(value) != value {
		return uuid.Nil, usageError("SESSION_ID must be a nonzero UUID")
	}
	sessionID, err := uuid.Parse(value)
	if err != nil || sessionID == uuid.Nil {
		return uuid.Nil, usageError("SESSION_ID must be a nonzero UUID")
	}
	return sessionID, nil
}

func readStudyRequestFile(path string, stdin io.Reader) ([]byte, error) {
	if path == "" {
		return nil, usageError("--request is required")
	}
	var (
		reader io.Reader
		file   *os.File
	)
	if path == "-" {
		if stdin == nil {
			return nil, &ExitError{Code: ExitValidation, Err: errors.New("study request could not be read")}
		}
		reader = stdin
	} else {
		var err error
		file, err = os.Open(path)
		if err != nil {
			return nil, &ExitError{Code: ExitValidation, Err: errors.New("study request could not be read")}
		}
		defer file.Close()
		reader = file
	}

	requestJSON, err := io.ReadAll(io.LimitReader(reader, studysession.StudyMaximumRequestBodyBytes+1))
	if err != nil {
		return nil, &ExitError{Code: ExitValidation, Err: errors.New("study request could not be read")}
	}
	if len(requestJSON) == 0 {
		return nil, &ExitError{Code: ExitValidation, Err: errors.New("study request is empty")}
	}
	if len(requestJSON) > studysession.StudyMaximumRequestBodyBytes {
		return nil, &ExitError{Code: ExitValidation, Err: fmt.Errorf("study request exceeds %d bytes", studysession.StudyMaximumRequestBodyBytes)}
	}
	return requestJSON, nil
}
