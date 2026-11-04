package download

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"ciderload/internal/coord"
	"ciderload/internal/curlargs"
)

type checkpoint struct {
	Version        int
	Request        string // Hash only: never persist signed URLs or credentials.
	Size, Chunk    int64
	ETag, Modified string
	Done           []bool
}
type store struct {
	dir, output string
	file        *os.File
	state       checkpoint
	mu          sync.Mutex
	lastSave    time.Time
	dirty       bool
}

func key(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func stagingDir(output string) string {
	return filepath.Join(filepath.Dir(output), ".ciderload-"+key(output)[:24])
}

func openStore(o curlargs.Options, r remote, chunk int64) (*store, error) {
	if chunk < 1 || r.Size < 1 || (r.Size-1)/chunk > 1_000_000 {
		return nil, errors.New("file has too many ranges")
	}
	fingerprint, _ := json.Marshal(struct {
		URL, User, Cookie string
		Headers           any
	}{o.URL, o.User, o.Cookie, o.Headers})
	want := checkpoint{Version: 1, Request: key(string(fingerprint)), Size: r.Size, Chunk: chunk, ETag: r.ETag, Modified: r.Modified, Done: make([]bool, 1+(r.Size-1)/chunk)}
	s := &store{dir: stagingDir(o.Output), output: o.Output, state: want}
	if err := coord.PrivateDir(s.dir); err != nil {
		return nil, err
	}
	if err := coord.CleanTemps(s.dir); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "state.json"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var prev checkpoint
	reuse := err == nil && json.Unmarshal(data, &prev) == nil && prev.Version == 1 && prev.Request == want.Request && prev.Size == want.Size && prev.Chunk == want.Chunk && len(prev.Done) == len(want.Done)
	// Cross-process resume needs evidence that the representation is unchanged.
	reuse = reuse && validator(r) != "" && validator(r) == validator(remote{ETag: prev.ETag, Modified: prev.Modified})
	path := filepath.Join(s.dir, "data")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0666)
	if err != nil {
		return nil, err
	}
	s.file = f
	defer func() {
		if s.file == nil {
			f.Close()
		}
	}()
	fail := func(err error) (*store, error) { s.file = nil; return nil, err }
	st, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if !st.Mode().IsRegular() {
		return fail(errors.New("staging data is not a regular file"))
	}
	reuse = reuse && st.Size() == r.Size
	if reuse {
		s.state = prev
		return s, nil
	}
	if err = f.Truncate(0); err != nil {
		return fail(err)
	}
	if err = f.Truncate(r.Size); err != nil {
		return fail(err)
	}
	if dst, err := os.Stat(o.Output); err == nil {
		if err = f.Chmod(dst.Mode().Perm()); err != nil {
			return fail(err)
		}
		if o.Resume && dst.Size() > 0 {
			if dst.Size() > r.Size {
				return fail(failure(33, "local file is larger than remote file", nil))
			}
			// Import complete chunks only. Re-fetching the final partial chunk
			// allows a single bitmap to describe both sequential and split state.
			count := dst.Size() / chunk
			if dst.Size() == r.Size {
				count = int64(len(s.state.Done))
			}
			prefix := min(count*chunk, r.Size)
			in, err := os.Open(o.Output)
			if err != nil {
				return fail(err)
			}
			_, copyErr := io.CopyN(io.NewOffsetWriter(f, 0), in, prefix)
			in.Close()
			if copyErr != nil {
				return fail(copyErr)
			}
			for i := int64(0); i < count; i++ {
				s.state.Done[i] = true
			}
		}
	} else if !os.IsNotExist(err) {
		return fail(err)
	}
	s.dirty = true
	if err = s.save(); err != nil {
		return fail(err)
	}
	return s, nil
}

func (s *store) mark(i int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Done[i] = true
	s.dirty = true
	if time.Since(s.lastSave) >= time.Second {
		return s.save()
	}
	return nil
}

// save is serialized with bitmap updates. Bytes reach storage before their
// completion record; interrupted writes can only cause work to be repeated.
func (s *store) save() error {
	if !s.dirty {
		return nil
	}
	if err := s.file.Sync(); err != nil {
		return err
	}
	data, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	if err = coord.AtomicWrite(filepath.Join(s.dir, "state.json"), data, true); err != nil {
		return err
	}
	s.dirty = false
	s.lastSave = time.Now()
	return nil
}

func (s *store) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.save()
	if e := s.file.Close(); err == nil {
		err = e
	}
	s.file = nil
	return err
}

func (s *store) commit(o curlargs.Options, r remote) error {
	for _, done := range s.state.Done {
		if !done {
			return errors.New("cannot publish an incomplete download")
		}
	}
	if err := s.close(); err != nil {
		return err
	}
	path := filepath.Join(s.dir, "data")
	if o.RemoteTime {
		if date, err := httpTime(r.Modified); err == nil {
			if err = os.Chtimes(path, time.Now(), date); err != nil {
				return err
			}
		}
	}
	if err := os.Rename(path, s.output); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.dir, "state.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Remove(s.dir)
}

func discard(output string) error {
	path := stagingDir(output)
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("invalid staging directory: %s", path)
	}
	if err := coord.CleanTemps(path); err != nil {
		return err
	}
	// Delete only our known artifacts, never arbitrary directory contents.
	for _, name := range []string{"data", "state.json"} {
		if err = os.Remove(filepath.Join(path, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Remove(path)
}
