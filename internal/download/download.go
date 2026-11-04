// Package download implements parallel HTTP/1.1 ranges. It leaves verification
// of the completed artifact to Homebrew, and validates range framing itself.
package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"ciderload/internal/coord"
	"ciderload/internal/curlargs"
)

type Limiter interface {
	Acquire(context.Context) (func() error, error)
}
type Config struct {
	Connections        int
	ChunkSize, MinSize int64
	LockDir            string
	Transfer, Prepare  Limiter
	Progress           func(completed, total int64)
}

func httpTime(s string) (time.Time, error) { return http.ParseTime(s) }

// Run owns the output lock across preparation, retries, and range workers.
// For Fallback the caller must run curl through WithOutputLock to keep the
// destination serialized with other CiderLoad processes.
func Run(ctx context.Context, o curlargs.Options, c Config) error {
	if c.Connections < 1 || c.ChunkSize < 1 || c.MinSize < 1 || c.Transfer == nil || c.Prepare == nil {
		return errors.New("invalid downloader configuration")
	}
	started := time.Now()
	lockCtx, lockCancel := context.WithCancel(ctx)
	if o.MaxTime > 0 {
		lockCancel()
		lockCtx, lockCancel = context.WithDeadline(ctx, started.Add(o.MaxTime))
	}
	defer lockCancel()
	return WithOutputLock(lockCtx, o.Output, c.LockDir, func(output string) error {
		o.Output = output
		for attempt := 0; ; attempt++ {
			attemptCtx, cancel := context.WithCancel(ctx)
			if o.MaxTime > 0 {
				cancel()
				start := time.Now()
				if attempt == 0 {
					start = started
				}
				attemptCtx, cancel = context.WithDeadline(ctx, start.Add(o.MaxTime))
			}
			err := runAttempt(attemptCtx, o, c)
			deadline, hasDeadline := attemptCtx.Deadline()
			cancel()
			if err == nil {
				return nil
			}
			if errors.Is(err, Fallback) {
				if e := discard(o.Output); e != nil {
					return e
				}
				if hasDeadline {
					if time.Until(deadline) <= 0 {
						return context.DeadlineExceeded
					}
					return &TimedFallback{Deadline: deadline}
				}
				return Fallback
			}
			e := Classify(err)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if attempt >= o.Retries || !e.Retry {
				return err
			}
			delay := o.RetryDelay
			if delay == 0 {
				delay = time.Second * time.Duration(1<<min(attempt, 10))
				delay = min(delay, 600*time.Second)
			}
			delay = max(delay, e.RetryAfter)
			if o.RetryMaxTime > 0 && time.Since(started)+delay >= o.RetryMaxTime {
				return err
			}
			if err = coord.Wait(ctx, delay); err != nil {
				return err
			}
		}
	})
}

func WithOutputLock(ctx context.Context, output, lockDir string, fn func(string) error) error {
	return WithOutputLockFile(ctx, output, lockDir, func(path string, _ *os.File) error { return fn(path) })
}

// WithOutputLockFile exposes the borrowed lock for a final exec of real curl.
func WithOutputLockFile(ctx context.Context, output, lockDir string, fn func(string, *os.File) error) error {
	abs, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return err
	}
	abs = filepath.Join(parent, filepath.Base(abs))
	if err = coord.PrivateDir(lockDir); err != nil {
		return err
	}
	lock, err := coord.Lock(ctx, filepath.Join(lockDir, key(abs)+".lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	return fn(abs, lock)
}

func runAttempt(ctx context.Context, o curlargs.Options, c Config) (result error) {
	if st, err := os.Lstat(o.Output); err == nil && (!st.Mode().IsRegular() || st.Mode().Perm()&0222 == 0) {
		return Fallback
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	jar, err := newJar()
	if err != nil {
		return err
	}
	if o.Cookie == "" {
		jar = nil
	}
	release, err := c.Prepare.Acquire(ctx)
	if err != nil {
		return err
	}
	r, probeErr := probe(ctx, o, jar)
	releaseErr := release()
	if releaseErr != nil {
		return releaseErr
	}
	if probeErr != nil {
		return probeErr
	}
	if r.Size < c.MinSize || c.Connections == 1 {
		return Fallback
	}
	// Bound the number of chunks for very large artifacts while retaining
	// enough work units to balance unequal connection speeds.
	chunk := max(c.ChunkSize, (r.Size+999_999)/1_000_000)
	s, err := openStore(o, r, chunk)
	if err != nil {
		return err
	}
	defer func() {
		if e := s.close(); result == nil {
			result = e
		}
	}()
	jobs := make(chan int, len(s.state.Done))
	var completed atomic.Int64
	for i, done := range s.state.Done {
		if !done {
			jobs <- i
		} else {
			completed.Add(min(chunk, r.Size-int64(i)*chunk))
		}
	}
	close(jobs)
	workers := min(c.Connections, len(jobs))
	workCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			if err := worker(workCtx, o, c, r, s, jar, jobs, &completed); err != nil {
				cancel(err)
			}
		})
	}
	// Progress callbacks run on this goroutine, never in parallel with one
	// another or while a file-state lock is held.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
wait:
	for {
		select {
		case <-done:
			break wait
		case <-ticker.C:
			if c.Progress != nil {
				c.Progress(completed.Load(), r.Size)
			}
		}
	}
	if err = context.Cause(workCtx); err != nil {
		return err
	}
	if c.Progress != nil {
		c.Progress(r.Size, r.Size)
	}
	return s.commit(o, r)
}

func worker(ctx context.Context, o curlargs.Options, c Config, r remote, s *store, jar http.CookieJar, jobs <-chan int, completed *atomic.Int64) (result error) {
	// Each worker holds one transfer slot and reuses its HTTP connection.
	// It returns the slot as soon as it runs out of work, even while other
	// workers on this file are still finishing their last ranges.
	release, err := c.Transfer.Acquire(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if e := release(); result == nil {
			result = e
		}
	}()
	tr, err := newTransport(o)
	if err != nil {
		return err
	}
	defer tr.CloseIdleConnections()
	client := newClient(o, tr, jar)
	for i := range jobs {
		if err = ctx.Err(); err != nil {
			return err
		}
		start := int64(i) * s.state.Chunk
		end := min(start+s.state.Chunk, r.Size) - 1
		req, err := request(ctx, o, r.URL, r.Headers, start, end, validator(r))
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		err = readChunk(resp, o, r, s.file, start, end)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if err = s.mark(i); err != nil {
			return err
		}
		completed.Add(end - start + 1)
	}
	return nil
}

func readChunk(resp *http.Response, o curlargs.Options, r remote, out *os.File, start, end int64) error {
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return Fallback
	}
	if err := statusError(resp, o); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusPartialContent || encoded(resp) {
		return Fallback
	}
	a, b, size, err := parseRange(resp.Header.Get("Content-Range"))
	length := end - start + 1
	if err != nil || a != start || b != end || size != r.Size || (resp.ContentLength != -1 && resp.ContentLength != length) {
		return Fallback
	}
	if etag := resp.Header.Get("ETag"); r.ETag != "" && etag != "" && etag != r.ETag {
		return Fallback
	}
	if modified := resp.Header.Get("Last-Modified"); r.Modified != "" && modified != "" && modified != r.Modified {
		return Fallback
	}
	n, err := io.CopyN(io.NewOffsetWriter(out, start), resp.Body, length)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	if n != length {
		return io.ErrUnexpectedEOF
	}
	// Chunked responses have no Content-Length. Require EOF before marking
	// the range complete, without writing any excess into the next range.
	var extra [1]byte
	nExtra, err := io.ReadFull(resp.Body, extra[:])
	if nExtra != 0 {
		return Fallback
	}
	if err != io.EOF {
		return fmt.Errorf("read range trailer: %w", err)
	}
	return nil
}
