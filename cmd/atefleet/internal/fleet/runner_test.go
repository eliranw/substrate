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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPRunnerRun(t *testing.T) {
	var gotCommand []string
	var gotTimeout string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/process" {
			t.Errorf("path = %q, want /process", r.URL.Path)
		}
		var body struct {
			Command []string `json:"command"`
			Timeout string   `json:"timeout"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		gotCommand = body.Command
		gotTimeout = body.Timeout
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"stdout":   "hello\n",
			"stderr":   "warn\n",
			"exitCode": 7,
			"error":    "boom",
		})
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	runner := NewHTTPRunner(nil)
	res, err := runner.Run(context.Background(), addr, []string{"echo", "hi"}, 30*time.Second)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if gotCommand[0] != "echo" || gotCommand[1] != "hi" || len(gotCommand) != 2 {
		t.Errorf("posted command = %v, want [echo hi]", gotCommand)
	}
	if gotTimeout != "30s" {
		t.Errorf("posted timeout = %q, want 30s", gotTimeout)
	}
	if res.Stdout != "hello\n" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "hello\n")
	}
	if res.Stderr != "warn\n" {
		t.Errorf("Stderr = %q, want %q", res.Stderr, "warn\n")
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
	if res.RunError != "boom" {
		t.Errorf("RunError = %q, want %q", res.RunError, "boom")
	}
}

func TestHTTPRunnerRunOmitsZeroTimeout(t *testing.T) {
	gotTimeout := "unset"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if v, ok := raw["timeout"]; ok {
			gotTimeout = string(v)
		} else {
			gotTimeout = ""
		}
		json.NewEncoder(w).Encode(map[string]any{"stdout": "", "stderr": "", "exitCode": 0})
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	runner := NewHTTPRunner(nil)
	if _, err := runner.Run(context.Background(), addr, []string{"true"}, 0); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotTimeout != "" {
		t.Errorf("timeout field present (%q), want omitted for zero timeout", gotTimeout)
	}
}

func TestHTTPRunnerRunNon2xx(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Bad request", http.StatusBadRequest)
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	runner := NewHTTPRunner(nil)
	if _, err := runner.Run(context.Background(), addr, []string{"echo"}, 0); err == nil {
		t.Fatal("want error for non-2xx response")
	}
}

func TestHTTPRunnerRunMalformedBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not json"))
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	runner := NewHTTPRunner(nil)
	if _, err := runner.Run(context.Background(), addr, []string{"echo"}, 0); err == nil {
		t.Fatal("want error for malformed response body")
	}
}

func TestHTTPRunnerRunRetriesThenFails(t *testing.T) {
	// Point at an address with nothing listening: connection refused should be
	// retried and eventually surface a wrapped error rather than hanging.
	runner := NewHTTPRunner(&http.Client{Timeout: 500 * time.Millisecond})
	start := time.Now()
	if _, err := runner.Run(context.Background(), "127.0.0.1:1", []string{"echo"}, 0); err == nil {
		t.Fatal("want error when nothing is listening")
	}
	if time.Since(start) > 30*time.Second {
		t.Fatalf("retry took too long: %v", time.Since(start))
	}
}
