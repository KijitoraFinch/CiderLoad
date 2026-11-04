package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheViewSharesPayloadsAndIsolatesSelection(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "ordinary cache")
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, view := range []string{a, b} {
		if err := cacheView(view, original); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(view, "tool-selection"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(view, "tool-selection/curl"), []byte(view), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"downloads", "api", "api-source"} {
		if err := os.WriteFile(filepath.Join(a, name, "payload"), []byte("shared"), 0600); err != nil {
			t.Fatal(err)
		}
		if !sameFile(filepath.Join(a, name, "payload"), filepath.Join(b, name, "payload")) || !sameFile(filepath.Join(a, name, "payload"), filepath.Join(original, name, "payload")) {
			t.Fatal("download/API cache was duplicated", name)
		}
	}
	if sameFile(filepath.Join(a, "tool-selection/curl"), filepath.Join(b, "tool-selection/curl")) {
		t.Fatal("different launchers share their selection")
	}
	if _, err := os.Stat(filepath.Join(original, "tool-selection")); !os.IsNotExist(err) {
		t.Fatal("ordinary selection cache was touched", err)
	}
	if err := cacheView(a, original); err != nil {
		t.Fatal("cache could not be reused", err)
	}
}

func TestCacheViewRejectsUnexpectedDestination(t *testing.T) {
	root := t.TempDir()
	view := filepath.Join(root, "view")
	if err := os.MkdirAll(filepath.Join(view, "downloads"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := cacheView(view, filepath.Join(root, "ordinary")); err == nil {
		t.Fatal("accepted an unrelated download cache")
	}
}

func TestShortSettingsReachHomebrewAndLongNamesWin(t *testing.T) {
	env := environment([]string{"CIDERLOAD_TRACE=1", "CIDERLOAD_CONNECTIONS=2", "HOMEBREW_CIDERLOAD_CONNECTIONS=5", "HOMEBREW_CIDERLOAD_CURL=", "CIDERLOAD_CURL=/other/curl"})
	if get(env, "HOMEBREW_CIDERLOAD_TRACE") != "1" || get(env, "HOMEBREW_CIDERLOAD_CONNECTIONS") != "5" || get(env, "HOMEBREW_CIDERLOAD_CURL") != "" {
		t.Fatal("settings changed", env)
	}
}

func TestBrewSettingsAreLiteralAndUserSettingsWin(t *testing.T) {
	root := t.TempDir()
	prefix, config := filepath.Join(root, "prefix"), filepath.Join(root, "config")
	for path, data := range map[string]string{
		filepath.Join(prefix, "etc/homebrew/brew.env"): "HOMEBREW_CIDERLOAD_CONNECTIONS=3\n",
		filepath.Join(config, "homebrew/brew.env"):     "HOMEBREW_CIDERLOAD_CONNECTIONS=5\nHOMEBREW_CIDERLOAD_CURL=$(not-executed)\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	env, _, err := readBrewSettings(prefix, []string{"XDG_CONFIG_HOME=" + config})
	if err != nil || get(env, "HOMEBREW_CIDERLOAD_CONNECTIONS") != "5" || get(env, "HOMEBREW_CIDERLOAD_CURL") != "$(not-executed)" {
		t.Fatal("brew.env was not treated as literal settings", env, err)
	}
}

// This opt-in integration check uses the installed Homebrew but only a private
// cache and config. It runs no downloads or package operations.
func TestActualHomebrewHandoff(t *testing.T) {
	brew := os.Getenv("CIDL_TEST_BREW")
	if brew == "" {
		t.Skip("set CIDL_TEST_BREW to run against a Homebrew checkout")
	}
	root := t.TempDir()
	engine := filepath.Join(root, "curl with spaces;literal")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\nexec /usr/bin/curl \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	env := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(name, "HOMEBREW_") && !strings.HasPrefix(name, "CIDER") && !strings.HasPrefix(name, "CIDL_") {
			env = append(env, entry)
		}
	}
	for name, val := range map[string]string{
		"CIDL_BREW": brew, "CIDL_DOWNLOADER": engine,
		"HOMEBREW_CACHE": filepath.Join(root, "cache"), "HOMEBREW_CIDERLOAD_STATE_DIR": filepath.Join(root, "state"),
		"HOMEBREW_NO_AUTO_UPDATE": "1", "HOMEBREW_NO_ANALYTICS": "1", "HOMEBREW_NO_INSTALL_FROM_API": "1",
		"HOMEBREW_DEVELOPER": "1", "HOMEBREW_TEMP": root, "XDG_CONFIG_HOME": filepath.Join(root, "config"),
	} {
		env = put(env, name, val)
	}
	args := []string{"ruby", "-r", "utils/curl", "-e", `puts Utils::Curl.curl_path; puts ENV.fetch("HOMEBREW_SYSTEM_CURL_TOO_OLD", "system curl usable"); warn "fixture stderr"; exit 37`}
	for repeat := 0; repeat < 2; repeat++ {
		plan, err := Prepare(args, env)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(plan.Path, args...)
		cmd.Env = plan.Env
		out, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 37 || !strings.Contains(string(out), engine+"\n") || !strings.Contains(string(out), "system curl usable") || !strings.Contains(string(out), "fixture stderr") {
			t.Fatalf("handoff changed behavior: %s %v", out, err)
		}
		if get(plan.Env, "HOMEBREW_CACHE") == get(env, "HOMEBREW_CACHE") {
			t.Fatal("ordinary tool-selection cache reused")
		}
	}
}
