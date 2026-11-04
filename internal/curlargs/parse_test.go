package curlargs

import (
	"testing"
	"time"
)

func TestHomebrewInvocation(t *testing.T) {
	a := []string{"--disable", "--cookie", "/dev/null", "--globoff", "--show-error", "--user-agent", "Homebrew/test", "--header", "Accept-Language: en", "--fail", "--progress-bar", "--silent", "--retry", "3", "--remote-time", "--output", "/tmp/archive.incomplete", "--location", "--header", "Authorization: Bearer test", "https://example.test/blob"}
	o, ok := Parse(a)
	if !ok {
		t.Fatal("did not recognize Homebrew invocation")
	}
	if o.Output != "/tmp/archive.incomplete" || !o.DisableConfig || !o.Location || !o.Fail || !o.RemoteTime || !o.Silent || o.Retries != 3 || o.Headers.Get("Authorization") != "Bearer test" {
		t.Fatalf("incorrect options: %+v", o)
	}
}

func TestOptions(t *testing.T) {
	o, ok := Parse([]string{"-q", "-fsSL", "-oout", "--connect-timeout=1.25", "--max-time", "5", "--continue-at", "-", "--proto-redir", "=https", "https://example.test/a"})
	if !ok || o.ConnectTimeout != 1250*time.Millisecond || o.MaxTime != 5*time.Second || !o.Resume || !o.HTTPSRedirectOnly || !o.ShowError {
		t.Fatalf("incorrect options: %+v %v", o, ok)
	}
}

func TestRemainingTimeoutPreservesArguments(t *testing.T) {
	for _, args := range [][]string{
		{"-q", "-A", "--max-time", "--max-time", "10", "-o", "out", "--", "https://example.test/a"},
		{"-q", "--max-time=10", "-o", "out", "https://example.test/a"},
	} {
		o, ok := Parse(args)
		if !ok {
			t.Fatal("fixture not recognized")
		}
		adjusted := o.ArgsWithMaxTime(args, 250*time.Millisecond)
		got, ok := Parse(adjusted)
		if !ok || got.MaxTime != 250*time.Millisecond || got.UserAgent != o.UserAgent || got.URL != o.URL {
			t.Fatal("changed unrelated arguments", adjusted)
		}
		original, _ := Parse(args)
		if original.MaxTime != 10*time.Second {
			t.Fatal("mutated original invocation")
		}
	}
}

func TestDelegateUnsupported(t *testing.T) {
	for _, a := range [][]string{
		{"--head", "https://example.test"},
		{"--output", "-", "https://example.test"},
		{"-o", "out", "https://a.test", "https://b.test"},
		{"--compressed", "-o", "out", "https://example.test"},
		{"--config", "custom", "-o", "out", "https://example.test"},
		{"-H", "Range: bytes=0-1", "-o", "out", "https://example.test"},
		{"-H", "If-None-Match: tag", "-o", "out", "https://example.test"},
		{"--retry", "NaN", "-o", "out", "https://example.test"},
		{"--max-time", "NaN", "-o", "out", "https://example.test"},
		{"--max-time", "1e30", "-o", "out", "https://example.test"},
		{"--max-time", "9223372036.854776", "-o", "out", "https://example.test"},
		{"--retry-delay", "0.1", "-o", "out", "https://example.test"},
		{"--retry-max-time", "0.1", "-o", "out", "https://example.test"},
		{"--silent=true", "-o", "out", "https://example.test"},
		{"--output"},
		{"--user", "name", "-o", "out", "https://example.test"},
		{"--cookie", "cookies.txt", "-o", "out", "https://example.test"},
		{"-o", "out", "ftp://example.test/a"},
	} {
		t.Run(a[0]+a[len(a)-1], func(t *testing.T) {
			if _, ok := Parse(a); ok {
				t.Fatalf("should delegate %q", a)
			}
		})
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"--output", "--retry=3", "-fsSL", "--header=Range: bytes=0-1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { Parse([]string{s, "-o", "out", "https://example.test/a"}) })
}
