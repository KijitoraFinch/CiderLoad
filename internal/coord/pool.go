package coord

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"
)

type entry struct {
	ID                     string
	Kind                   string
	Held, Waiting, Granted int
}
type state struct {
	Version int
	Limits  map[string]int
	Entries []entry
}
type Session struct {
	dir, id, kind string
	limits        map[string]int
	owner         *os.File
}

// OwnerFile is borrowed for exec inheritance. A delegated curl must retain the
// allocation until it exits, including if its invoking process is killed.
func (s *Session) OwnerFile() *os.File { return s.owner }

// Open registers arrival order before any preparation or transfer starts.
func Open(ctx context.Context, root, kind string, transfers, preparations int) (*Session, error) {
	if kind != "transfer" && kind != "prepare" {
		return nil, errors.New("unknown pool")
	}
	if transfers < 1 || preparations < 1 {
		return nil, errors.New("invalid connection limit")
	}
	if err := PrivateDir(root); err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	s := &Session{dir: root, id: hex.EncodeToString(b), kind: kind, limits: map[string]int{"transfer": transfers, "prepare": preparations}}
	var err error
	s.owner, err = Lock(ctx, filepath.Join(root, s.id+".owner"))
	if err != nil {
		return nil, err
	}
	err = s.update(ctx, func(st *state) error {
		if err := CleanTemps(root); err != nil {
			return err
		}
		if len(st.Entries) == 0 {
			st.Limits = s.limits
		}
		if st.Limits["transfer"] != transfers || st.Limits["prepare"] != preparations {
			return errors.New("connection limits differ from another running CiderLoad process")
		}
		st.Entries = append(st.Entries, entry{ID: s.id, Kind: kind})
		return nil
	})
	if err != nil {
		s.owner.Close()
		os.Remove(s.owner.Name())
		return nil, err
	}
	return s, nil
}

func (s *Session) update(ctx context.Context, fn func(*state) error) error {
	lock, err := Lock(ctx, filepath.Join(s.dir, "pool.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	path := filepath.Join(s.dir, "pool.json")
	st := state{Version: 1, Limits: maps.Clone(s.limits)}
	data, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(data, &st); err != nil {
			return fmt.Errorf("read connection state: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if st.Version != 1 {
		return errors.New("unsupported connection state version")
	}
	live := st.Entries[:0]
	for _, e := range st.Entries {
		if len(e.ID) != 32 {
			return errors.New("invalid owner in connection state")
		}
		if _, err := hex.DecodeString(e.ID); err != nil {
			return err
		}
		if e.Held < 0 || e.Waiting < 0 || e.Granted < 0 || e.Granted > e.Held || st.Limits[e.Kind] < 1 || e.Held > st.Limits[e.Kind] {
			return errors.New("invalid connection state")
		}
		f, err := os.OpenFile(filepath.Join(s.dir, e.ID+".owner"), os.O_RDWR|syscall.O_NOFOLLOW, 0600)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		f.Close()
		if err == nil {
			os.Remove(filepath.Join(s.dir, e.ID+".owner"))
			continue
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		live = append(live, e)
	}
	st.Entries = live
	if err = fn(&st); err != nil {
		return err
	}
	for kind, limit := range st.Limits {
		used := 0
		for _, e := range st.Entries {
			if e.Kind == kind {
				used += e.Held
			}
		}
		if used > limit {
			return errors.New("connection allocation exceeds limit")
		}
		for i := range st.Entries {
			e := &st.Entries[i]
			if e.Kind != kind {
				continue
			}
			n := min(e.Waiting, limit-used)
			e.Waiting -= n
			e.Granted += n
			e.Held += n
			used += n
		}
	}
	updated, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if string(data) == string(updated) {
		return nil
	}
	return AtomicWrite(path, updated, false)
}

func (s *Session) own(st *state) (*entry, error) {
	for i := range st.Entries {
		if st.Entries[i].ID == s.id {
			return &st.Entries[i], nil
		}
	}
	return nil, errors.New("connection session no longer registered")
}

// Acquire returns an idempotent release function. Allocations go to the oldest
// registered session that has outstanding requests, independently per pool.
func (s *Session) Acquire(ctx context.Context) (func() error, error) {
	err := s.update(ctx, func(st *state) error {
		e, err := s.own(st)
		if err != nil {
			return err
		}
		e.Waiting++
		return nil
	})
	if err != nil {
		return nil, err
	}
	for {
		got := false
		err = s.update(ctx, func(st *state) error {
			e, err := s.own(st)
			if err != nil {
				return err
			}
			if e.Granted > 0 {
				e.Granted--
				got = true
			}
			return nil
		})
		if err == nil && got {
			var once sync.Once
			var releaseErr error
			return func() error {
				once.Do(func() {
					releaseErr = s.update(context.Background(), func(st *state) error {
						e, err := s.own(st)
						if err != nil {
							return err
						}
						e.Held--
						return nil
					})
				})
				return releaseErr
			}, nil
		}
		if err == nil {
			err = Wait(ctx, 20*time.Millisecond)
		}
		if err != nil {
			cleanup := s.update(context.Background(), func(st *state) error {
				e, eerr := s.own(st)
				if eerr != nil {
					return eerr
				}
				if e.Waiting > 0 {
					e.Waiting--
				} else if e.Granted > 0 {
					e.Granted--
					e.Held--
				}
				return nil
			})
			return nil, errors.Join(err, cleanup)
		}
	}
}

func (s *Session) Close() error {
	err := s.update(context.Background(), func(st *state) error {
		st.Entries = slices.DeleteFunc(st.Entries, func(e entry) bool { return e.ID == s.id })
		return nil
	})
	if e := s.owner.Close(); err == nil {
		err = e
	}
	if e := os.Remove(s.owner.Name()); err == nil {
		err = e
	}
	return err
}
