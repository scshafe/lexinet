package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"

	"golang.org/x/term"

	"lexinet/internal/config"
)

type ServiceError struct {
	StatusCode int
	Code       string
	Message    string
	Candidates []string
}

func (e *ServiceError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

func doJSON(ctx context.Context, method, endpoint, path, token string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf := new(bytes.Buffer)
		if err := json.NewEncoder(buf).Encode(in); err != nil {
			return err
		}
		body = buf
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(endpoint, "/")+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &ExitError{Code: ExitUnreachable, Err: fmt.Errorf("endpoint unreachable: %w", err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		serviceErr := decodeServiceError(resp)
		return &ExitError{Code: serviceExitCode(resp.StatusCode, serviceErr.Code), Err: serviceErr}
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		dec := json.NewDecoder(resp.Body)
		if err := dec.Decode(out); err != nil {
			return err
		}
	}
	return nil
}

func decodeServiceError(resp *http.Response) *ServiceError {
	serviceErr := &ServiceError{StatusCode: resp.StatusCode, Message: "service returned " + resp.Status}
	var envelope struct {
		Error struct {
			Code       string   `json:"code"`
			Message    string   `json:"message"`
			Candidates []string `json:"candidates,omitempty"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope); err == nil && envelope.Error.Code != "" {
		serviceErr.Code = envelope.Error.Code
		serviceErr.Message = envelope.Error.Message
		serviceErr.Candidates = envelope.Error.Candidates
	}
	return serviceErr
}

func serviceExitCode(status int, serviceCode string) int {
	if serviceCode == "IDEA_REF_AMBIGUOUS" {
		return ExitAmbiguous
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ExitAuth
	case http.StatusNotFound:
		return ExitNotFound
	case http.StatusBadRequest, http.StatusConflict:
		return ExitValidation
	default:
		return ExitUnexpected
	}
}

func bodyFromFlag(value string) (string, error) {
	return bodyFromReader(value, os.Stdin)
}

func bodyFromReader(value string, r io.Reader) (string, error) {
	if value != "-" {
		return value, nil
	}
	b, err := io.ReadAll(io.LimitReader(r, 10<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func endpointAndToken(opts *Options) (string, string, error) {
	cfg, _, err := config.Load()
	if err != nil {
		return "", "", err
	}
	token, err := cfg.Token(opts.TokenFile)
	if err != nil {
		return "", "", err
	}
	return cfg.Endpoint(opts.Endpoint), token, nil
}

func apiPath(pattern string, parts ...string) string {
	for _, part := range parts {
		pattern = strings.Replace(pattern, "{}", url.PathEscape(part), 1)
	}
	return pattern
}

func stdinIsTerminal() bool {
	return term.IsTerminal(int(syscall.Stdin))
}
