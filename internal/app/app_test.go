package app

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func cleanEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"http_proxy", "https_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "CURL_CA_BUNDLE", "SSL_CERT_FILE", "SSL_CERT_DIR", "SSLKEYLOGFILE", "CIDERLOAD_CURL", "CIDERLOAD_CONNECTIONS", "CIDERLOAD_TOTAL_CONNECTIONS", "CIDERLOAD_PREPARE_CONNECTIONS", "_CIDERLOAD_DELEGATING", "CIDERLOAD_TRACE"} {
		t.Setenv(name, "")
	}
	for _, name := range []string{"CURL", "CONNECTIONS", "TOTAL_CONNECTIONS", "PREPARE_CONNECTIONS", "TRACE", "STATE_DIR"} {
		name = "HOMEBREW_CIDERLOAD_" + name
		old, exists := os.LookupEnv(name)
		os.Unsetenv(name)
		t.Cleanup(func() {
			if exists {
				os.Setenv(name, old)
			} else {
				os.Unsetenv(name)
			}
		})
	}
	t.Setenv("CIDERLOAD_STATE_DIR", filepath.Join(t.TempDir(), "state"))
}

func TestDelegationPreservesArgumentsOutputAndExit(t *testing.T) {
	cleanEnv(t)
	fake := filepath.Join(t.TempDir(), "curl")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\000' \"$@\"\nprintf 'diagnostic' >&2\nexit 37\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CIDERLOAD_CURL", fake)
	args := []string{"--unsupported", "spaces and \"quotes\"", "$(not-a-command)", "https://example.test/a?token=private"}
	var out, stderr bytes.Buffer
	code := Run(t.Context(), args, nil, &out, &stderr)
	if code != 37 || out.String() != strings.Join(args, "\x00")+"\x00" || stderr.String() != "diagnostic" {
		t.Fatalf("delegation changed behavior: %d %q %q", code, out.String(), stderr.String())
	}
}

func TestActualCurlFallback(t *testing.T) {
	for _, mode := range []string{"no-range", "empty", "small", "http-error-body", "range-changes"} {
		t.Run(mode, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv("CIDERLOAD_TRACE", "1")
			data := []byte("ordinary curl content")
			if mode == "empty" {
				data = nil
			}
			if mode == "small" {
				data = bytes.Repeat([]byte("s"), (40<<20)-1)
			}
			if mode == "range-changes" {
				data = bytes.Repeat([]byte("12345678"), 5<<20)
			}
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if mode == "small" || mode == "empty" || (mode == "range-changes" && r.Header.Get("Range") == "bytes=0-0") {
					w.Header().Set("ETag", `"test"`)
					http.ServeContent(w, r, "data", time.Time{}, bytes.NewReader(data))
					return
				}
				if mode == "http-error-body" {
					w.WriteHeader(503)
				}
				w.Write(data)
			}))
			defer srv.Close()
			output := filepath.Join(t.TempDir(), "out")
			args := []string{"-q", "-sS", "-L", "--output", output, srv.URL}
			if mode != "http-error-body" {
				args = append(args, "--fail")
			}
			var stdout, stderr bytes.Buffer
			if code := Run(t.Context(), args, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("fallback failed: %d %s", code, stderr.String())
			}
			got, err := os.ReadFile(output)
			if err != nil || !bytes.Equal(got, data) || stdout.Len() != 0 {
				t.Fatal("incorrect delegated output", err)
			}
			if requests.Load() < 2 || !strings.Contains(stderr.String(), "response requires ordinary curl") {
				t.Fatal("fixture did not exercise fallback")
			}
		})
	}
}

func TestApplicationAcceleratesHomebrewArguments(t *testing.T) {
	cleanEnv(t)
	t.Setenv("CIDERLOAD_TRACE", "1")
	data := bytes.Repeat([]byte("parallel"), 5<<20) // Exactly 40 MiB exercises the threshold.
	var ranges atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" || r.UserAgent() != "Homebrew/test" {
			t.Error("Homebrew headers not preserved")
		}
		if r.Header.Get("Range") == "" {
			t.Error("body was not accelerated")
		}
		ranges.Add(1)
		w.Header().Set("ETag", `"fixture"`)
		http.ServeContent(w, r, "blob", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	output := filepath.Join(t.TempDir(), "bottle.incomplete")
	args := []string{"--disable", "--cookie", "/dev/null", "--globoff", "--show-error", "--user-agent", "Homebrew/test", "--header", "Accept-Language: en", "--header", "Authorization: Bearer fixture", "--fail", "--progress-bar", "--silent", "--retry", "3", "--remote-time", "--output", output, "--location", srv.URL}
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), args, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("download failed: %d %s", code, stderr.String())
	}
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("incorrect output", err)
	}
	if ranges.Load() < 4 || !strings.Contains(stderr.String(), "parallel download complete") || stdout.Len() != 0 {
		t.Fatal("did not exercise parallel path", stderr.String())
	}
}

func TestErrorCodesAndNoSecretDiagnostics(t *testing.T) {
	cleanEnv(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	var stdout, stderr bytes.Buffer
	args := []string{"-q", "-fsS", "-o", filepath.Join(t.TempDir(), "out"), srv.URL + "?token=private"}
	if code := Run(t.Context(), args, nil, &stdout, &stderr); code != 22 {
		t.Fatalf("HTTP error code %d", code)
	}
	if strings.Contains(stderr.String(), "private") || !strings.Contains(stderr.String(), "404") {
		t.Fatal("incorrect diagnostic", stderr.String())
	}
}

func TestConfigAndIdentity(t *testing.T) {
	cleanEnv(t)
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), []string{"--version"}, nil, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "curl ") {
		t.Fatal("curl identity not preserved")
	}
	if _, err := os.Stat(os.Getenv("CIDERLOAD_STATE_DIR")); !os.IsNotExist(err) {
		t.Fatal("version query created coordination state")
	}
	t.Setenv("CIDERLOAD_TOTAL_CONNECTIONS", "0")
	stdout.Reset()
	stderr.Reset()
	if code := Run(t.Context(), []string{"-q", "-o", "out", "https://example.test"}, nil, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "CIDERLOAD_TOTAL_CONNECTIONS") {
		t.Fatal("invalid configuration not diagnosed")
	}
}

func TestHomebrewConfigurationPrefix(t *testing.T) {
	cleanEnv(t)
	t.Setenv("CIDERLOAD_CONNECTIONS", "2")
	t.Setenv("HOMEBREW_CIDERLOAD_CONNECTIONS", "7")
	t.Setenv("HOMEBREW_CIDERLOAD_STATE_DIR", filepath.Join(t.TempDir(), "brew-state"))
	c, err := readConfig()
	if err != nil || c.connections != 7 || c.root != os.Getenv("HOMEBREW_CIDERLOAD_STATE_DIR") {
		t.Fatal("Homebrew configuration was not honored", c, err)
	}
}

func TestProxyAndCurlConfigAreDelegated(t *testing.T) {
	cleanEnv(t)
	fake := filepath.Join(t.TempDir(), "curl")
	os.WriteFile(fake, []byte("#!/bin/sh\nprintf delegated"), 0700)
	t.Setenv("CIDERLOAD_CURL", fake)
	t.Setenv("https_proxy", "http://proxy.example.test")
	var out, stderr bytes.Buffer
	code := Run(t.Context(), []string{"-q", "-o", "unused", "https://example.test"}, nil, &out, &stderr)
	if code != 0 || out.String() != "delegated" {
		t.Fatal(fmt.Sprint(code), out.String(), stderr.String())
	}
}

func TestFallbackSharesRemainingTimeout(t *testing.T) {
	cleanEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(180 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Length", "4")
		w.Write([]byte("data"))
	}))
	defer srv.Close()
	var out, stderr bytes.Buffer
	start := time.Now()
	code := Run(t.Context(), []string{"-q", "-fsS", "--max-time", "0.3", "-o", filepath.Join(t.TempDir(), "out"), srv.URL}, nil, &out, &stderr)
	if code != 28 {
		t.Fatalf("fallback reset timeout: %d %s", code, stderr.String())
	}
	if time.Since(start) > time.Second {
		t.Fatal("timeout was not enforced")
	}
}
