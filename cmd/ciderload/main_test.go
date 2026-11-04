package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The test executable supplies the exact main/signal path to subprocesses,
// avoiding a separate build and exercising race instrumentation in children.
func TestCLIProcess(t *testing.T) {
	if os.Getenv("CIDERLOAD_CLI_TEST") != "1" {
		return
	}
	for i, a := range os.Args {
		if a == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			main()
			return
		}
	}
	os.Exit(99)
}

func command(t *testing.T, state string, args ...string) *exec.Cmd {
	t.Helper()
	a := append([]string{"-test.run=^TestCLIProcess$", "--"}, args...)
	cmd := exec.Command(os.Args[0], a...)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "http_proxy", "https_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "CURL_CA_BUNDLE", "SSL_CERT_FILE", "SSL_CERT_DIR", "SSLKEYLOGFILE":
			continue
		}
		if strings.HasPrefix(name, "CIDERLOAD_") || strings.HasPrefix(name, "HOMEBREW_CIDERLOAD_") || name == "_CIDERLOAD_DELEGATING" {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "CIDERLOAD_CLI_TEST=1", "CIDERLOAD_STATE_DIR="+state, "CIDERLOAD_TOTAL_CONNECTIONS=3", "CIDERLOAD_CONNECTIONS=4", "CIDERLOAD_PREPARE_CONNECTIONS=2", "CIDERLOAD_TRACE=1")
	t.Cleanup(func() {
		if cmd.Process != nil && cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	return cmd
}

func TestMultipleProcessesRespectTransferLimitAndPrepareSeparately(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefgh"), 5<<20)
	gate := make(chan struct{})
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	var active, peak, probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			return
		}
		if r.Header.Get("Range") == "bytes=0-0" {
			probes.Add(1)
		} else {
			n := active.Add(1)
			defer active.Add(-1)
			for p := peak.Load(); n > p; p = peak.Load() {
				if peak.CompareAndSwap(p, n) {
					break
				}
			}
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		w.Header().Set("ETag", `"data"`)
		http.ServeContent(w, r, "data", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	state := filepath.Join(root, "state")
	a := command(t, state, "-q", "-fsS", "-o", filepath.Join(root, "a"), srv.URL)
	b := command(t, state, "-q", "-fsS", "-o", filepath.Join(root, "b"), srv.URL)
	var logA, logB bytes.Buffer
	a.Stderr = &logA
	b.Stderr = &logB
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return active.Load() == 3 })
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return probes.Load() == 2 })
	head := command(t, state, "-q", "-sS", "--head", srv.URL)
	if out, err := head.Output(); err != nil || !bytes.Contains(out, []byte("200 OK")) {
		t.Fatalf("preparation blocked or failed: %s %v", out, err)
	}
	if peak.Load() > 3 {
		t.Fatal("global transfer limit exceeded before release")
	}
	close(gate)
	if err := a.Wait(); err != nil {
		t.Fatal(err, logA.String())
	}
	if err := b.Wait(); err != nil {
		t.Fatal(err, logB.String())
	}
	if peak.Load() != 3 {
		t.Fatal("unexpected total concurrency", peak.Load())
	}
	for _, name := range []string{"a", "b"} {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatal("incorrect process output", name, err)
		}
	}
}

func TestSignalStopsWorkersAndReleasesProcessSlots(t *testing.T) {
	data := bytes.Repeat([]byte("12345678"), 5<<20)
	var phase, active atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-0" && phase.Load() == 0 {
			active.Add(1)
			defer active.Add(-1)
			<-r.Context().Done()
			return
		}
		w.Header().Set("ETag", `"data"`)
		http.ServeContent(w, r, "data", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	output := filepath.Join(root, "out")
	state := filepath.Join(root, "state")
	cmd := command(t, state, "-q", "-fsS", "-o", output, srv.URL)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return active.Load() > 0 })
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil || cmd.ProcessState.ExitCode() != 143 {
		t.Fatal("wrong signal status", err, cmd.ProcessState)
	}
	await(t, func() bool { return active.Load() == 0 })
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("interrupted process exposed sparse output")
	}
	phase.Store(1)
	next := command(t, state, "-q", "-fsS", "-o", output, srv.URL)
	if out, err := next.CombinedOutput(); err != nil {
		t.Fatal("slots did not recover", err, string(out))
	}
	got, _ := os.ReadFile(output)
	if !bytes.Equal(got, data) {
		t.Fatal("subsequent process output differs")
	}
}

func TestDelegatedCurlHardKillReleasesAllocation(t *testing.T) {
	var active, phase atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if phase.Load() == 0 {
			active.Add(1)
			defer active.Add(-1)
			<-r.Context().Done()
			return
		}
		w.Write([]byte("done"))
	}))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	state := filepath.Join(root, "state")
	// --compressed goes directly to curl; all three slots are held by three
	// processes. Killing them must stop the actual sockets as well as owners.
	var commands []*exec.Cmd
	for i := 0; i < 3; i++ {
		cmd := command(t, state, "-q", "-fsS", "--compressed", "-o", filepath.Join(root, fmt.Sprint(i)), srv.URL)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
	}
	await(t, func() bool { return active.Load() == 3 })
	fourth := command(t, state, "-q", "-fsS", "--compressed", "-o", filepath.Join(root, "fourth"), srv.URL)
	if err := fourth.Start(); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		if active.Load() > 3 {
			t.Fatal("curl did not retain its inherited transfer lock")
		}
		data, err := os.ReadFile(filepath.Join(state, "pool", "pool.json"))
		if err != nil {
			return false
		}
		var st struct{ Entries []struct{ Waiting int } }
		if json.Unmarshal(data, &st) != nil {
			return false
		}
		for _, e := range st.Entries {
			if e.Waiting > 0 {
				return true
			}
		}
		return false
	})
	for _, cmd := range commands {
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		cmd.Wait()
	}
	await(t, func() bool { return active.Load() == 1 })
	if err := fourth.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	fourth.Wait()
	await(t, func() bool { return active.Load() == 0 })
	phase.Store(1)
	next := command(t, state, "-q", "-fsS", "--compressed", "-o", filepath.Join(root, "next"), srv.URL)
	if out, err := next.CombinedOutput(); err != nil {
		t.Fatal("delegated locks not reclaimed", err, string(out))
	}
	got, _ := os.ReadFile(filepath.Join(root, "next"))
	if string(got) != "done" {
		t.Fatal("delegated output differs")
	}
}

func await(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("process did not reach expected state")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
