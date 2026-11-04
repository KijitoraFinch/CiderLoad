// Package app is the curl-compatible command boundary.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ciderload/internal/coord"
	"ciderload/internal/curlargs"
	"ciderload/internal/download"
)

var Version = "dev"

type config struct {
	root, curl                  string
	connections, total, prepare int
	trace                       bool
	exec                        bool
}

func readConfig() (config, error) {
	c := config{root: setting("STATE_DIR"), curl: setting("CURL"), connections: 8, total: 16, prepare: 4, trace: setting("TRACE") == "1"}
	if c.root == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return c, err
		}
		c.root = filepath.Join(cache, "ciderload")
	}
	if c.curl == "" {
		c.curl = "/usr/bin/curl"
	}
	for name, dst := range map[string]*int{"CIDERLOAD_CONNECTIONS": &c.connections, "CIDERLOAD_TOTAL_CONNECTIONS": &c.total, "CIDERLOAD_PREPARE_CONNECTIONS": &c.prepare} {
		if v := setting(strings.TrimPrefix(name, "CIDERLOAD_")); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 256 {
				return c, fmt.Errorf("%s must be between 1 and 256", name)
			}
			*dst = n
		}
	}
	return c, nil
}

// Homebrew starts with a filtered environment that retains HOMEBREW_*.
// The shorter spelling also works when invoking CiderLoad directly.
func setting(name string) string {
	if v, ok := os.LookupEnv("HOMEBREW_CIDERLOAD_" + name); ok {
		return v
	}
	return os.Getenv("CIDERLOAD_" + name)
}

func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return run(ctx, args, stdin, stdout, stderr, false)
}

// RunProcess may replace the current process with curl. The same PID and
// inherited locks make signal handling and process death unambiguous.
func RunProcess(ctx context.Context, args []string) int {
	return run(ctx, args, os.Stdin, os.Stdout, os.Stderr, true)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, execMode bool) int {
	if len(args) == 1 {
		switch args[0] {
		case "--ciderload-version":
			fmt.Fprintln(stdout, "CiderLoad "+Version)
			return 0
		case "--ciderload-help":
			fmt.Fprintln(stdout, help)
			return 0
		}
	}
	if os.Getenv("_CIDERLOAD_DELEGATING") == "1" {
		fmt.Fprintln(stderr, "ciderload: CIDERLOAD_CURL resolves back to CiderLoad; choose the real curl executable")
		return 2
	}
	c, err := readConfig()
	if err != nil {
		fmt.Fprintln(stderr, "ciderload:", err)
		return 2
	}
	c.exec = execMode
	o, eligible := curlargs.Parse(args)
	if eligible && environmentNeedsCurl(o) {
		eligible = false
	}
	// Curl owns combinations whose retry clock must span a possible switch
	// between engines. Delegating them preserves its exact timeout policy.
	if o.RetryMaxTime > 0 || (o.MaxTime > 0 && o.Retries > 0) {
		eligible = false
	}
	if c.connections == 1 {
		eligible = false
	}
	trace := func(format string, a ...any) {
		if c.trace {
			fmt.Fprintf(stderr, "ciderload: "+format+"\n", a...)
		}
	}
	// Curl identity and help calls neither allocate slots nor touch the network.
	if identity(args) {
		return delegate(ctx, c, args, stdin, stdout, stderr, nil)
	}
	kind := "transfer"
	if !eligible && preparation(args) {
		kind = "prepare"
	}
	poolRoot := filepath.Join(c.root, "pool")
	session, err := coord.Open(ctx, poolRoot, kind, c.total, c.prepare)
	if err != nil {
		fmt.Fprintln(stderr, "ciderload:", err)
		return 2
	}
	defer session.Close()
	var fallbackDeadline time.Time
	if eligible {
		prep, err := coord.Open(ctx, poolRoot, "prepare", c.total, c.prepare)
		if err != nil {
			fmt.Fprintln(stderr, "ciderload:", err)
			return 2
		}
		defer prep.Close()
		shown := false
		cfg := download.Config{Connections: c.connections, ChunkSize: 4 << 20, MinSize: 40 << 20, LockDir: filepath.Join(c.root, "outputs"), Transfer: session, Prepare: prep}
		if !o.Silent {
			cfg.Progress = func(n, total int64) {
				shown = true
				fmt.Fprintf(stderr, "\rCiderLoad %6.2f%%  %d / %d bytes", 100*float64(n)/float64(total), n, total)
			}
		}
		trace("parallel candidate; up to %d transfer connections", c.connections)
		err = download.Run(ctx, o, cfg)
		if shown {
			fmt.Fprintln(stderr)
		}
		if err == nil {
			trace("parallel download complete")
			return 0
		}
		if !errors.Is(err, download.Fallback) {
			e := download.Classify(err)
			if !o.Silent || o.ShowError {
				fmt.Fprintf(stderr, "curl: (%d) %s\n", e.Code, e.Message)
			}
			return e.Code
		}
		trace("response requires ordinary curl")
		var timed *download.TimedFallback
		if errors.As(err, &timed) {
			fallbackDeadline = timed.Deadline
		}
	} else {
		trace("delegating curl invocation (%s pool)", kind)
	}
	code := 0
	if !fallbackDeadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, fallbackDeadline)
		defer cancel()
	}
	run := func(_ string, outputLock *os.File) error {
		release, err := session.Acquire(ctx)
		if err != nil {
			return err
		}
		delegatedArgs := args
		if !fallbackDeadline.IsZero() && o.MaxTime > 0 {
			delegatedArgs = o.ArgsWithMaxTime(args, time.Until(fallbackDeadline))
		}
		locks := []*os.File{session.OwnerFile()}
		if outputLock != nil {
			locks = append(locks, outputLock)
		}
		code = delegate(ctx, c, delegatedArgs, stdin, stdout, stderr, locks)
		releaseErr := release()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return releaseErr
	}
	if eligible {
		err = download.WithOutputLockFile(ctx, o.Output, filepath.Join(c.root, "outputs"), run)
	} else {
		err = run("", nil)
	}
	if err != nil {
		e := download.Classify(err)
		fmt.Fprintf(stderr, "curl: (%d) %s\n", e.Code, e.Message)
		return e.Code
	}
	return code
}

func delegate(ctx context.Context, c config, args []string, stdin io.Reader, stdout, stderr io.Writer, locks []*os.File) int {
	path, err := exec.LookPath(c.curl)
	if err == nil {
		self, _ := os.Executable()
		a, e1 := os.Stat(self)
		b, e2 := os.Stat(path)
		if e1 == nil && e2 == nil && os.SameFile(a, b) {
			err = errors.New("CIDERLOAD_CURL must refer to the real curl executable")
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "ciderload:", err)
		return 2
	}
	if ctx.Err() != nil {
		return 42
	}
	env := append(os.Environ(), "_CIDERLOAD_DELEGATING=1")
	if c.exec {
		// These descriptors belong to private lock files, not downloaded data.
		// Kernel locks survive exec and are released with the curl process.
		for _, f := range locks {
			_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_SETFD, 0)
			if errno != 0 {
				fmt.Fprintln(stderr, "ciderload: could not inherit transfer lock:", errno)
				return 2
			}
		}
		err = syscall.Exec(path, append([]string{path}, args...), env)
		for _, f := range locks {
			syscall.CloseOnExec(int(f.Fd()))
		}
		fmt.Fprintln(stderr, "ciderload: could not execute curl:", err)
		return 2
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = env
	cmd.ExtraFiles = locks
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exit.ExitCode()
	}
	if ctx.Err() != nil {
		return 42
	}
	fmt.Fprintln(stderr, "ciderload: could not execute curl:", err)
	return 2
}

func identity(args []string) bool {
	if len(args) == 1 {
		switch args[0] {
		case "--version", "-V", "--help", "-h", "--manual", "-M":
			return true
		}
	}
	return len(args) == 2 && (args[0] == "--help" || args[0] == "-h")
}

func preparation(args []string) bool {
	for _, a := range args {
		if a == "--head" || a == "-I" {
			return true
		}
	}
	for _, a := range args {
		if a == "--output" || a == "-o" || a == "--remote-name" || a == "-O" || strings.HasPrefix(a, "--output=") || strings.HasPrefix(a, "-o") {
			return false
		}
	}
	return true
}

// Configuration with semantics owned by curl is delegated as one intact
// invocation. In particular, do not silently ignore proxy or trust settings.
func environmentNeedsCurl(o curlargs.Options) bool {
	for _, name := range []string{"http_proxy", "https_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "CURL_CA_BUNDLE", "SSL_CERT_FILE", "SSL_CERT_DIR", "SSLKEYLOGFILE"} {
		if os.Getenv(name) != "" {
			return true
		}
	}
	if o.DisableConfig {
		return false
	}
	var files []string
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(home, ".curlrc"))
	}
	if home := os.Getenv("CURL_HOME"); home != "" {
		files = append(files, filepath.Join(home, ".curlrc"))
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		files = append(files, filepath.Join(xdg, "curlrc"))
	}
	for _, path := range files {
		if _, err := os.Stat(path); err == nil || !os.IsNotExist(err) {
			return true
		}
	}
	return false
}

const help = `CiderLoad accelerates Homebrew HTTP(S) downloads using parallel HTTP/1.1 ranges.

Use curl arguments; unsupported invocations are forwarded to the real curl.
  cidl install <formula>
  ciderload -q -fL --output archive.tar.gz https://example.org/archive.tar.gz

Environment:
  HOMEBREW_CIDERLOAD_CONNECTIONS          Per-file connections (default 8)
  HOMEBREW_CIDERLOAD_TOTAL_CONNECTIONS    Connections across processes (default 16)
  HOMEBREW_CIDERLOAD_PREPARE_CONNECTIONS  Concurrent preparation requests (default 4)
  HOMEBREW_CIDERLOAD_STATE_DIR            Private coordination directory
  HOMEBREW_CIDERLOAD_CURL                 Real curl executable (default /usr/bin/curl)
  HOMEBREW_CIDERLOAD_TRACE=1              Explain routing decisions on stderr

Direct invocations also accept these names without the HOMEBREW_ prefix.

--version reports the underlying curl version for Homebrew compatibility.
--ciderload-version reports CiderLoad's version.`
