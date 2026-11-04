package download

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ciderload/internal/coord"
	"ciderload/internal/curlargs"
)

type limiter struct {
	tokens       chan struct{}
	active, peak atomic.Int32
}

func (l *limiter) Acquire(ctx context.Context) (func() error, error) {
	select {
	case l.tokens <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	n := l.active.Add(1)
	for p := l.peak.Load(); n > p; p = l.peak.Load() {
		if l.peak.CompareAndSwap(p, n) {
			break
		}
	}
	var once sync.Once
	return func() error { once.Do(func() { l.active.Add(-1); <-l.tokens }); return nil }, nil
}
func config(t *testing.T) Config {
	t.Helper()
	return Config{Connections: 4, ChunkSize: 64 << 10, MinSize: 128 << 10, LockDir: filepath.Join(t.TempDir(), "locks"), Transfer: &limiter{tokens: make(chan struct{}, 4)}, Prepare: &limiter{tokens: make(chan struct{}, 1)}}
}
func options(t *testing.T, url string) curlargs.Options {
	t.Helper()
	return curlargs.Options{URL: url, Output: filepath.Join(t.TempDir(), "archive"), Headers: make(http.Header), Location: true, Fail: true, MaxRedirects: 50}
}
func dataBytes(n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewPCG(13, 71))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

var modified = time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)

func serveData(w http.ResponseWriter, r *http.Request, data []byte) {
	w.Header().Set("ETag", `"v1"`)
	http.ServeContent(w, r, "archive", modified, bytes.NewReader(data))
}

func TestParallelDownloadAndAtomicPublication(t *testing.T) {
	data := dataBytes(2 << 20)
	cfg := config(t)
	var active, peak, probes atomic.Int32
	o := options(t, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Error("range request is not HTTP/1.1")
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
			if _, err := os.Stat(o.Output); !os.IsNotExist(err) {
				t.Error("destination exposed before completion")
			}
			time.Sleep(10 * time.Millisecond)
		}
		serveData(w, r, data)
	}))
	defer srv.Close()
	o.URL = srv.URL
	o.RemoteTime = true
	if err := Run(t.Context(), o, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(o.Output)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("output differs", err)
	}
	if peak.Load() < 2 || peak.Load() > 4 || probes.Load() != 1 {
		t.Fatalf("unexpected concurrency/probes: %d/%d", peak.Load(), probes.Load())
	}
	st, _ := os.Stat(o.Output)
	if !st.ModTime().Equal(modified) {
		t.Fatal("remote timestamp not preserved")
	}
	if _, err = os.Stat(stagingDir(o.Output)); !os.IsNotExist(err) {
		t.Fatal("staging directory not cleaned", err)
	}
	if cfg.Transfer.(*limiter).active.Load() != 0 || cfg.Prepare.(*limiter).active.Load() != 0 {
		t.Fatal("leaked permit")
	}
}

func TestRangeRejectionPreservesDestination(t *testing.T) {
	for _, mode := range []string{"ignore", "wrong-range", "wrong-size", "extra", "encoding", "changed-etag", "changed-time"} {
		t.Run(mode, func(t *testing.T) {
			data := dataBytes(256 << 10)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") == "bytes=0-0" {
					serveData(w, r, data)
					return
				}
				var start, end int
				fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
				if mode == "ignore" {
					w.Write(data)
					return
				}
				total := len(data)
				a, b := start, end
				switch mode {
				case "wrong-range":
					a++
				case "wrong-size":
					total++
				case "encoding":
					w.Header().Set("Content-Encoding", "gzip")
				case "changed-etag":
					w.Header().Set("ETag", `"v2"`)
				case "changed-time":
					w.Header().Set("Last-Modified", modified.Add(time.Hour).Format(http.TimeFormat))
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, total))
				w.WriteHeader(206)
				if mode == "extra" {
					w.(http.Flusher).Flush()
				}
				w.Write(data[start : end+1])
				if mode == "extra" {
					w.Write([]byte("x"))
				}
			}))
			defer srv.Close()
			o := options(t, srv.URL)
			os.WriteFile(o.Output, []byte("previous file"), 0600)
			if err := Run(t.Context(), o, config(t)); !errors.Is(err, Fallback) {
				t.Fatalf("expected fallback, got %v", err)
			}
			got, _ := os.ReadFile(o.Output)
			if string(got) != "previous file" {
				t.Fatal("fallback modified destination")
			}
			if _, err := os.Stat(stagingDir(o.Output)); !os.IsNotExist(err) {
				t.Fatal("invalid ranges retained")
			}
		})
	}
}

func TestShortBodyFails(t *testing.T) {
	data := dataBytes(256 << 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" {
			serveData(w, r, data)
			return
		}
		var start, end int
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		w.WriteHeader(206)
		w.Write(data[start : start+10])
	}))
	defer srv.Close()
	o := options(t, srv.URL)
	err := Run(t.Context(), o, config(t))
	if err == nil || Classify(err).Code != 18 {
		t.Fatalf("expected partial file error, got %v", err)
	}
	if _, err = os.Stat(o.Output); !os.IsNotExist(err) {
		t.Fatal("published truncated download")
	}
}

func TestResumeCompletedRangesAfterCancellation(t *testing.T) {
	data := dataBytes(512 << 10)
	cfg := config(t)
	cfg.Connections = 2
	var phase atomic.Int32
	var mu sync.Mutex
	var resumed []int
	firstComplete := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" {
			serveData(w, r, data)
			return
		}
		var start, end int
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if phase.Load() == 0 && start >= int(cfg.ChunkSize) {
			if start >= 2*int(cfg.ChunkSize) {
				once.Do(func() { close(firstComplete) })
			}
			<-r.Context().Done()
			return
		}
		if phase.Load() == 1 {
			mu.Lock()
			resumed = append(resumed, start)
			mu.Unlock()
		}
		serveData(w, r, data)
	}))
	defer srv.Close()
	o := options(t, srv.URL)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, o, cfg) }()
	<-firstComplete
	// The first worker reaches its next range only after marking the first.
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	phase.Store(1)
	if err := Run(t.Context(), o, cfg); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, start := range resumed {
		if start == 0 {
			t.Fatal("completed first chunk was downloaded again")
		}
	}
	got, _ := os.ReadFile(o.Output)
	if !bytes.Equal(got, data) {
		t.Fatal("resumed data differs")
	}
}

func TestImportCurlPartialFile(t *testing.T) {
	data := dataBytes(256 << 10)
	var smallest atomic.Int64
	smallest.Store(int64(len(data)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-0" {
			var start, end int64
			fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
			for old := smallest.Load(); start < old; old = smallest.Load() {
				if smallest.CompareAndSwap(old, start) {
					break
				}
			}
		}
		serveData(w, r, data)
	}))
	defer srv.Close()
	o := options(t, srv.URL)
	o.Resume = true
	if err := os.WriteFile(o.Output, data[:100<<10], 0640); err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), o, config(t)); err != nil {
		t.Fatal(err)
	}
	if smallest.Load() != 64<<10 {
		t.Fatal("did not retain complete prefix chunk", smallest.Load())
	}
	got, _ := os.ReadFile(o.Output)
	if !bytes.Equal(got, data) {
		t.Fatal("resumed data differs")
	}
	st, _ := os.Stat(o.Output)
	if st.Mode().Perm() != 0640 {
		t.Fatal("existing permissions not retained")
	}
}

func TestCheckpointRepresentationChangeAndCrashTemporary(t *testing.T) {
	for _, change := range []string{"etag", "no-validator", "size"} {
		t.Run(change, func(t *testing.T) {
			o := options(t, "https://example.test/archive")
			r := remote{Size: 256 << 10, ETag: `"old"`, Modified: ""}
			if change == "no-validator" {
				r.ETag = ""
			}
			s, err := openStore(o, r, 64<<10)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.file.WriteAt(bytes.Repeat([]byte("x"), 64<<10), 0); err != nil {
				t.Fatal(err)
			}
			if err = s.mark(0); err != nil {
				t.Fatal(err)
			}
			if err = s.close(); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(stagingDir(o.Output), ".state-interrupted"), []byte("partial JSON"), 0600); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "etag":
				r.ETag = `"new"`
			case "size":
				r.Size += 64 << 10
			}
			s, err = openStore(o, r, 64<<10)
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			for _, done := range s.state.Done {
				if done {
					t.Fatal("reused unverified or changed representation")
				}
			}
			if _, err = os.Stat(filepath.Join(stagingDir(o.Output), ".state-interrupted")); !os.IsNotExist(err) {
				t.Fatal("crash temporary was not removed")
			}
		})
	}
}

func TestSymlinkOutputDelegates(t *testing.T) {
	o := options(t, "https://not-contacted.invalid")
	target := filepath.Join(t.TempDir(), "original")
	os.WriteFile(target, []byte("original"), 0600)
	if err := os.Symlink(target, o.Output); err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), o, config(t)); !errors.Is(err, Fallback) {
		t.Fatal("symlink should use curl", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "original" {
		t.Fatal("modified symlink target")
	}
}

func TestOutputLockWaitHonorsTimeout(t *testing.T) {
	o := options(t, "https://not-contacted.invalid")
	o.MaxTime = 50 * time.Millisecond
	cfg := config(t)
	err := WithOutputLock(t.Context(), o.Output, cfg.LockDir, func(string) error {
		start := time.Now()
		err := Run(t.Context(), o, cfg)
		if err == nil || Classify(err).Code != 28 {
			t.Fatal("lock wait ignored timeout", err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("lock wait did not stop")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCredentialsAndCookiesAcrossRedirects(t *testing.T) {
	data := dataBytes(256 << 10)
	var authLeaked atomic.Bool
	var ranges atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || strings.Contains(r.Header.Get("Cookie"), "secret=1") {
			authLeaked.Store(true)
		}
		if r.Header.Get("Range") != "bytes=0-0" {
			ranges.Add(1)
			if c, err := r.Cookie("session"); err != nil || c.Value != "yes" {
				t.Error("redirect cookie not retained")
			}
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "yes", Path: "/"})
		serveData(w, r, data)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("initial authorization missing")
		}
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()
	o := options(t, source.URL)
	o.Cookie = "secret=1"
	o.Headers.Set("Authorization", "Bearer test")
	if err := Run(t.Context(), o, config(t)); err != nil {
		t.Fatal(err)
	}
	if authLeaked.Load() || ranges.Load() == 0 {
		t.Fatal("credential isolation or parallel transfer failed")
	}
}

func TestTimeoutRetryAndHTTPError(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer srv.Close()
		o := options(t, srv.URL)
		o.MaxTime = 50 * time.Millisecond
		if err := Run(t.Context(), o, config(t)); err == nil || Classify(err).Code != 28 {
			t.Fatal("expected timeout", err)
		}
	})
	t.Run("retry", func(t *testing.T) {
		data := dataBytes(256 << 10)
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.WriteHeader(503)
				return
			}
			serveData(w, r, data)
		}))
		defer srv.Close()
		o := options(t, srv.URL)
		o.Retries = 1
		o.RetryDelay = time.Millisecond
		if err := Run(t.Context(), o, config(t)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("404", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		o := options(t, srv.URL)
		if err := Run(t.Context(), o, config(t)); err == nil || Classify(err).Code != 22 {
			t.Fatal("expected HTTP error", err)
		}
	})
}

func TestTLSAndRedirectRestriction(t *testing.T) {
	data := dataBytes(256 << 10)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveData(w, r, data) }))
	defer server.Close()
	o := options(t, server.URL)
	if err := Run(t.Context(), o, config(t)); err == nil || Classify(err).Code != 60 {
		t.Fatal("accepted untrusted certificate", err)
	}
	o.CACert = filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(o.CACert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)
	if err := Run(t.Context(), o, config(t)); err != nil {
		t.Fatal("did not use explicit CA", err)
	}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed restricted redirect") }))
	defer plain.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, plain.URL, 302) }))
	defer redirect.Close()
	o.URL = redirect.URL
	os.WriteFile(o.CACert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: redirect.Certificate().Raw}), 0600)
	o.HTTPSRedirectOnly = true
	if err := Run(t.Context(), o, config(t)); err == nil || Classify(err).Code != 1 {
		t.Fatal("redirect restriction ignored", err)
	}
}

func TestProcessPoolReleasesAfterDownload(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pool")
	a, err := coord.Open(t.Context(), root, "transfer", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := coord.Open(t.Context(), root, "prepare", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	data := dataBytes(256 << 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveData(w, r, data) }))
	defer srv.Close()
	cfg := config(t)
	cfg.Transfer = a
	cfg.Prepare = b
	if err = Run(t.Context(), options(t, srv.URL), cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	r1, err := a.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r1()
	r2, err := a.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r2()
}

func FuzzContentRange(f *testing.F) {
	for _, s := range []string{"bytes 0-0/1", "bytes 0-100/100", "bytes 0-0/*", "bytes -1-0/5"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a, b, n, err := parseRange(s)
		if err == nil && (a < 0 || b < a || n <= b) {
			t.Fatalf("accepted invalid range %q", s)
		}
	})
}
