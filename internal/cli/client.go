package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

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
		return &ExitError{Code: ExitUnreachable, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		code := ExitValidation
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			code = ExitAuth
		case http.StatusNotFound:
			code = ExitNotFound
		}
		return &ExitError{Code: code, Err: fmt.Errorf("service returned %s", resp.Status)}
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		dec := json.NewDecoder(resp.Body)
		if err := dec.Decode(out); err != nil {
			return err
		}
	}
	return nil
}

func bodyFromFlag(value string) (string, error) {
	if value != "-" {
		return value, nil
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 10<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}
