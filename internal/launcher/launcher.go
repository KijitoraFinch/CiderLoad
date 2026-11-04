// Package launcher prepares curl selection, then hands the process to Homebrew.
package launcher

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"ciderload/internal/coord"
)

//go:embed select.sh
var selection string

type Plan struct {
	Path string
	Env  []string
}

const Help = `Usage: cidl <brew arguments...>

Examples:
  cidl install wget
  cidl upgrade
  cidl fetch --force-bottle llvm

Arguments, output and exit status are passed to Homebrew.
Install cidl and ciderload in the same directory.

Settings:
  CIDL_BREW        Homebrew executable (default: brew on PATH)
  CIDL_DOWNLOADER  Download engine (default: ciderload beside cidl)
  HOMEBREW_CIDERLOAD_*  CiderLoad download settings

--help and --version belong to Homebrew. --cidl-help shows this help.
`

// Prepare only modifies caches owned by this user. Homebrew's installed files
// and its startup scripts are never rewritten or evaluated as modified text.
func Prepare(args, environ []string) (Plan, error) {
	env := environment(environ)
	brew, err := executable(value(env, "CIDL_BREW", "brew"))
	if err != nil {
		return Plan{}, fmt.Errorf("find Homebrew: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return Plan{}, err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return Plan{}, err
	}
	if sameFile(brew, self) {
		return Plan{}, fmt.Errorf("CIDL_BREW points to cidl; choose the real brew executable")
	}
	engine, err := executable(value(env, "CIDL_DOWNLOADER", filepath.Join(filepath.Dir(self), "ciderload")))
	if err != nil {
		return Plan{}, fmt.Errorf("find ciderload: %w (install it beside cidl, or set CIDL_DOWNLOADER)", err)
	}
	if sameFile(engine, self) || sameFile(engine, brew) {
		return Plan{}, fmt.Errorf("CIDL_DOWNLOADER must point to the download engine")
	}

	// Fast queries use Homebrew itself to resolve symlinked installations and
	// brew.env configuration. They do not perform curl selection or downloads.
	prefix, err := query(brew, env, "--prefix")
	if err != nil {
		return Plan{}, err
	}
	repo, err := query(brew, env, "--repository")
	if err != nil {
		return Plan{}, err
	}
	cache, err := query(brew, env, "--cache")
	if err != nil {
		return Plan{}, err
	}
	if original := get(env, "HOMEBREW_CIDL_ORIGINAL_CACHE"); original != "" {
		cache = original
	}
	for _, path := range []string{prefix, repo, cache} {
		if !filepath.IsAbs(path) {
			return Plan{}, fmt.Errorf("Homebrew returned a non-absolute path: %q", path)
		}
	}
	// Bash function identifiers cannot contain whitespace or shell syntax.
	// Standard Homebrew prefixes and ordinary custom prefixes satisfy this.
	if strings.ContainsAny(prefix, " \t\r\n'\"`$;(){}\\|&<>*?[]!") {
		return Plan{}, fmt.Errorf("Homebrew prefix cannot be used for shell-local curl selection: %q", prefix)
	}
	env, overrides, err := readBrewSettings(prefix, env)
	if err != nil {
		return Plan{}, err
	}
	if v := overrides["HOMEBREW_CURL_PATH"]; v != "" && !sameFile(v, engine) {
		return Plan{}, fmt.Errorf("brew.env overrides HOMEBREW_CURL_PATH; remove that setting when using cidl")
	}
	if overrides["HOMEBREW_FORCE_BREWED_CURL"] != "" {
		return Plan{}, fmt.Errorf("brew.env forces brewed curl; remove HOMEBREW_FORCE_BREWED_CURL when using cidl")
	}

	// Let Homebrew make its own update/no-update decision before recording a
	// selection keyed to the installed helper's metadata. Afterwards an update
	// must not invalidate that selection between preparation and the command.
	if err := updateIfNeeded(brew, repo, args, env); err != nil {
		return Plan{}, err
	}
	env = put(env, "HOMEBREW_AUTO_UPDATE_CHECKED", "1")

	root := get(env, "HOMEBREW_CIDERLOAD_STATE_DIR")
	if root == "" {
		userCache, err := os.UserCacheDir()
		if err != nil {
			return Plan{}, err
		}
		root = filepath.Join(userCache, "ciderload")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return Plan{}, err
	}
	delegate := value(env, "HOMEBREW_CIDERLOAD_CURL", "/usr/bin/curl")
	identity := delegate
	if path, err := executable(delegate); err == nil {
		if st, err := os.Stat(path); err == nil {
			identity += fmt.Sprintf("\x00%s:%d:%d", path, st.Size(), st.ModTime().UnixNano())
		}
	}
	key := sha256.Sum256([]byte(prefix + "\x00" + repo + "\x00" + cache + "\x00" + engine + "\x00" + identity))
	view := filepath.Join(root, "homebrew", fmt.Sprintf("%x", key[:16]))
	if err := cacheView(view, cache); err != nil {
		return Plan{}, err
	}
	env = put(env, "HOMEBREW_CACHE", view)
	env = put(env, "HOMEBREW_CIDL_ORIGINAL_CACHE", cache)
	env = put(env, "HOMEBREW_CURL_PATH", engine)
	env = put(env, "HOMEBREW_FORCE_BREWED_CURL", "")
	// A brew.env can take precedence over command environment variables. Catch
	// that before running the requested command with an unintended curl/cache.
	effectiveCache, err := query(brew, env, "--cache")
	if err != nil {
		return Plan{}, err
	}
	if !sameFile(effectiveCache, view) {
		return Plan{}, fmt.Errorf("brew.env overrides HOMEBREW_CACHE (%s); cidl requires its own tool-selection cache", effectiveCache)
	}
	prepEnv := put(put(env, "HOMEBREW_LIBRARY", filepath.Join(repo, "Library")), "HOMEBREW_PREFIX", prefix)
	if _, err := output("/bin/bash", []string{"-c", selection}, prepEnv); err != nil {
		return Plan{}, fmt.Errorf("prepare Homebrew curl: %w", err)
	}
	return Plan{Path: brew, Env: env}, nil
}

// The package bytes and API metadata stay in the user's existing cache. Only
// tool-selection and small derived caches are private to this launcher.
func cacheView(view, original string) error {
	if err := coord.PrivateDir(view); err != nil {
		return err
	}
	for _, name := range []string{"downloads", "api", "api-source"} {
		target := filepath.Join(original, name)
		if err := os.MkdirAll(target, 0755); err != nil {
			return err
		}
		link := filepath.Join(view, name)
		if err := os.Symlink(target, link); err != nil && !os.IsExist(err) {
			return err
		}
		if !sameFile(link, target) {
			return fmt.Errorf("cache link does not point to %s: %s", target, link)
		}
	}
	return nil
}

func updateIfNeeded(brew, repo string, args, env []string) error {
	// Reuse Homebrew's command classification instead of duplicating its list.
	command := ""
	for _, arg := range args {
		if arg == "--help" || arg == "-h" || arg == "help" {
			return nil
		}
		if !strings.HasPrefix(arg, "-") {
			if command == "" {
				command = arg
			}
		}
	}
	if command == "" {
		return nil
	}
	script := `source "$1/Library/Homebrew/utils/auto-update.sh" || exit 2
declare -F setup-auto-update >/dev/null || exit 2
HOMEBREW_COMMAND="$2"
setup-auto-update
printf '%s' "${HOMEBREW_AUTO_UPDATE_COMMAND:-}"
`
	needed, err := output("/bin/bash", []string{"-c", script, "cidl", repo, command}, env)
	if err != nil {
		return fmt.Errorf("classify Homebrew command: %w", err)
	}
	if strings.TrimSpace(needed) == "" {
		return nil
	}
	cmd := exec.Command(brew, "update-if-needed")
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Homebrew auto-update: %w", err)
	}
	return nil
}

// brew.env contains literal assignments, not shell code. Mirror the documented
// file order only for settings that affect downloader identity and selection.
func readBrewSettings(prefix string, env []string) ([]string, map[string]string, error) {
	overrides := make(map[string]string)
	read := func(path string) error {
		f, err := os.Open(path)
		if os.IsNotExist(err) || os.IsPermission(err) {
			return nil
		}
		if err != nil {
			return err
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			name, val, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
			if ok && (strings.HasPrefix(name, "HOMEBREW_CIDERLOAD_") || name == "HOMEBREW_CURL_PATH" || name == "HOMEBREW_FORCE_BREWED_CURL" || name == "HOMEBREW_SYSTEM_ENV_TAKES_PRIORITY") {
				env = put(env, name, val)
				overrides[name] = val
			}
		}
		return scanner.Err()
	}
	if err := read("/etc/homebrew/brew.env"); err != nil {
		return nil, nil, err
	}
	systemFirst := get(env, "HOMEBREW_SYSTEM_ENV_TAKES_PRIORITY") != ""
	config := get(env, "XDG_CONFIG_HOME")
	if config == "" {
		config = get(env, "HOMEBREW_XDG_CONFIG_HOME")
	}
	userFile := filepath.Join(get(env, "HOME"), ".homebrew", "brew.env")
	if config != "" {
		userFile = filepath.Join(config, "homebrew", "brew.env")
	}
	for _, path := range []string{filepath.Join(prefix, "etc/homebrew/brew.env"), userFile} {
		if err := read(path); err != nil {
			return nil, nil, err
		}
	}
	if systemFirst {
		if err := read("/etc/homebrew/brew.env"); err != nil {
			return nil, nil, err
		}
	}
	return env, overrides, nil
}

func query(brew string, env []string, arg string) (string, error) {
	out, err := output(brew, []string{arg}, env)
	return strings.TrimSuffix(out, "\n"), err
}

func output(path string, args, env []string) (string, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", filepath.Base(path), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func executable(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

func sameFile(a, b string) bool {
	x, e1 := os.Stat(a)
	y, e2 := os.Stat(b)
	return e1 == nil && e2 == nil && os.SameFile(x, y)
}

func environment(env []string) []string {
	result := append([]string(nil), env...)
	for _, entry := range env {
		name, val, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(name, "CIDERLOAD_") {
			long := "HOMEBREW_" + name
			found := false
			for _, item := range env {
				if strings.HasPrefix(item, long+"=") {
					found = true
					break
				}
			}
			if !found {
				result = put(result, long, val)
			}
		}
	}
	return result
}

func get(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], name+"="); ok {
			return value
		}
	}
	return ""
}

func value(env []string, name, fallback string) string {
	if v := get(env, name); v != "" {
		return v
	}
	return fallback
}

func put(env []string, name, val string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, name+"=") {
			result = append(result, entry)
		}
	}
	return append(result, name+"="+val)
}
