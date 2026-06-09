// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// SubtaskRunner posts a one-shot command to an actor's sandbox and returns the
// result. It is a seam so the RunSubtask handler can be tested without a live
// worker pod.
type SubtaskRunner interface {
	// Run POSTs the command to the actor's sandbox /process endpoint and
	// returns the result.
	Run(ctx context.Context, addr string, command []string, timeout time.Duration) (RunResult, error)
}

// RunResult is the decoded outcome of a one-shot command.
type RunResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	RunError string
}

// processRequest mirrors demos/sandbox ProcessRequest (the fields atefleet sets).
type processRequest struct {
	Command []string `json:"command"`
	Timeout string   `json:"timeout,omitempty"`
}

// processResponse mirrors demos/sandbox ProcessResponse.
type processResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
	Error    string `json:"error"`
}

const (
	// runnerMaxAttempts bounds connection retries: a freshly resumed worker may
	// need a moment before its HTTP server accepts connections.
	runnerMaxAttempts = 5
	// runnerBackoff is the pause between connection retries.
	runnerBackoff = 500 * time.Millisecond
	// runnerDefaultTimeout is the default client timeout when none is injected.
	runnerDefaultTimeout = 60 * time.Second
)

type httpRunner struct {
	client *http.Client
}

// NewHTTPRunner returns the production SubtaskRunner. A nil client yields a
// default client with a sane timeout.
func NewHTTPRunner(client *http.Client) SubtaskRunner {
	if client == nil {
		client = &http.Client{Timeout: runnerDefaultTimeout}
	}
	return &httpRunner{client: client}
}

func (h *httpRunner) Run(ctx context.Context, addr string, command []string, timeout time.Duration) (RunResult, error) {
	reqBody := processRequest{Command: command}
	if timeout > 0 {
		reqBody.Timeout = timeout.String()
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return RunResult{}, fmt.Errorf("marshal process request: %w", err)
	}

	url := "http://" + addr + "/process"

	var lastErr error
	for attempt := 1; attempt <= runnerMaxAttempts; attempt++ {
		res, err := h.do(ctx, url, body)
		if err == nil {
			return res, nil
		}
		lastErr = err
		// Only connection-level failures are worth retrying; a decoded non-2xx
		// or malformed body is deterministic and returned immediately.
		if !isRetryable(err) {
			return RunResult{}, err
		}
		if attempt == runnerMaxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return RunResult{}, ctx.Err()
		case <-time.After(runnerBackoff):
		}
	}
	return RunResult{}, fmt.Errorf("post to %s after %d attempts: %w", url, runnerMaxAttempts, lastErr)
}

func (h *httpRunner) do(ctx context.Context, url string, body []byte) (RunResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return RunResult{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		// Transport-level error (e.g. connection refused): retryable.
		return RunResult{}, retryableError{err}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return RunResult{}, fmt.Errorf("sandbox returned %s: %s", resp.Status, bytes.TrimSpace(snippet))
	}

	var pr processResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return RunResult{}, fmt.Errorf("decode process response: %w", err)
	}
	return RunResult{
		Stdout:   pr.Stdout,
		Stderr:   pr.Stderr,
		ExitCode: pr.ExitCode,
		RunError: pr.Error,
	}, nil
}

// retryableError marks a transport-level failure that Run should retry.
type retryableError struct{ err error }

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }

func isRetryable(err error) bool {
	var re retryableError
	return errors.As(err, &re)
}
