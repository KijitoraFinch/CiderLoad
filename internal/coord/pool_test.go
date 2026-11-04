package coord

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T, root, kind string, total, prep int) *Session {
	t.Helper()
	s, err := Open(t.Context(), root, kind, total, prep)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestFIFOAndIndependentPreparation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pool")
	a := openTest(t, root, "transfer", 1, 1)
	b := openTest(t, root, "transfer", 1, 1)
	c := openTest(t, root, "transfer", 1, 1)
	release, err := a.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	gotB := make(chan func() error, 1)
	gotC := make(chan func() error, 1)
	// Enqueue the newer session first; registration order is authoritative.
	go func() {
		r, e := c.Acquire(t.Context())
		if e == nil {
			gotC <- r
		}
	}()
	go func() {
		r, e := b.Acquire(t.Context())
		if e == nil {
			gotB <- r
		}
	}()
	waitFor(t, func() bool {
		waiting := 0
		a.update(t.Context(), func(s *state) error {
			for _, e := range s.Entries {
				waiting += e.Waiting
			}
			return nil
		})
		return waiting == 2
	})
	p := openTest(t, root, "prepare", 1, 1)
	pr, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = pr(); err != nil {
		t.Fatal(err)
	}
	if err = release(); err != nil {
		t.Fatal(err)
	}
	var rb func() error
	select {
	case rb = <-gotB:
	case <-gotC:
		t.Fatal("later request received first slot")
	case <-time.After(3 * time.Second):
		t.Fatal("slot was not handed off")
	}
	select {
	case <-gotC:
		t.Fatal("exceeded limit")
	default:
	}
	if err = rb(); err != nil {
		t.Fatal(err)
	}
	select {
	case rc := <-gotC:
		if err = rc(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("later request starved")
	}
}

func TestCanceledWaitDoesNotLeak(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pool")
	s := openTest(t, root, "transfer", 1, 1)
	r, err := s.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()
	if _, err = s.Acquire(ctx); err == nil {
		t.Fatal("expected cancellation")
	}
	if err = r(); err != nil {
		t.Fatal(err)
	}
	r, err = s.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r()
	r()
	if err = s.update(t.Context(), func(st *state) error {
		e, _ := s.own(st)
		if e.Held != 0 || e.Waiting != 0 || e.Granted != 0 {
			return fmt.Errorf("leaked allocation: %+v", e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestKilledOwnerReleasesSlots(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pool")
	cmd := exec.Command(os.Args[0], "-test.run=^TestPoolProcess$")
	cmd.Env = append(os.Environ(), "CIDERLOAD_TEST_POOL="+root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	scan := bufio.NewScanner(stdout)
	if !scan.Scan() || scan.Text() != "holding" {
		t.Fatal("child did not acquire its slot")
	}
	s := openTest(t, root, "transfer", 1, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	if _, err = s.Acquire(ctx); err == nil {
		t.Fatal("child's slot was ignored")
	}
	cancel()
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	ctx, cancel = context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	r, err := s.Acquire(ctx)
	if err != nil {
		t.Fatal("slot not reclaimed:", err)
	}
	r()
}

func TestPoolProcess(t *testing.T) {
	root := os.Getenv("CIDERLOAD_TEST_POOL")
	if root == "" {
		return
	}
	s, err := Open(context.Background(), root, "transfer", 1, 1)
	if err != nil {
		os.Exit(2)
	}
	if _, err = s.Acquire(context.Background()); err != nil {
		os.Exit(3)
	}
	fmt.Println("holding")
	for {
		time.Sleep(time.Hour)
	}
}

func TestRejectDifferentLimitsAndCorruptState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pool")
	openTest(t, root, "transfer", 2, 1)
	if s, err := Open(t.Context(), root, "transfer", 3, 1); err == nil {
		s.Close()
		t.Fatal("accepted inconsistent limits")
	}
	other := filepath.Join(t.TempDir(), "pool")
	if err := PrivateDir(other); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "pool.json"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), other, "transfer", 1, 1); err == nil {
		t.Fatal("silently discarded corrupt coordination state")
	}
}

func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
