// Package curlargs recognizes curl invocations whose semantics the downloader
// implements. An unrecognized invocation is passed, unchanged, to real curl.
package curlargs

import (
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Options struct {
	URL, Output, UserAgent, Referer, Cookie, User, CACert                string
	Headers                                                              http.Header
	Location, Fail, RemoteTime, Resume, Silent, ShowError, DisableConfig bool
	HTTPSRedirectOnly                                                    bool
	MaxRedirects, Retries                                                int
	ConnectTimeout, MaxTime, RetryDelay, RetryMaxTime                    time.Duration
	maxTimeArg                                                           int
	maxTimeInline                                                        bool
}

// Parse returns false for both unsupported and malformed options. Curl remains
// the authority for their syntax and diagnostics, including option ordering.
func Parse(args []string) (Options, bool) {
	o := Options{Headers: make(http.Header), MaxRedirects: 50, maxTimeArg: -1}
	if len(args) > 0 {
		o.DisableConfig = args[0] == "--disable" || args[0] == "-q"
	}
	var urls []string
	globoff, end := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" && !end {
			end = true
			continue
		}
		if end || !strings.HasPrefix(a, "-") || a == "-" {
			urls = append(urls, a)
			continue
		}
		name, value, hasValue := strings.Cut(a, "=")
		if !strings.HasPrefix(a, "--") {
			name, value, hasValue = a, "", false
			if len(a) > 2 {
				if strings.ContainsRune("oAUHebu", rune(a[1])) {
					name, value, hasValue = a[:2], a[2:], true
				} else {
					// Only value-free short options can be combined here.
					for _, c := range a[1:] {
						switch c {
						case 'f':
							o.Fail = true
						case 'L':
							o.Location = true
						case 's':
							o.Silent = true
						case 'S':
							o.ShowError = true
						case 'R':
							o.RemoteTime = true
						case 'g':
							globoff = true
						default:
							return o, false
						}
					}
					continue
				}
			}
		}
		flag := true
		switch name {
		case "--disable", "-q":
		case "--globoff", "-g":
			globoff = true
		case "--fail", "-f":
			o.Fail = true
		case "--location", "-L":
			o.Location = true
		case "--remote-time", "-R":
			o.RemoteTime = true
		case "--silent", "-s":
			o.Silent = true
		case "--show-error", "-S":
			o.ShowError = true
		case "--progress-bar", "-#", "--http1.1":
		default:
			flag = false
		}
		if flag {
			if hasValue {
				return o, false
			}
			continue
		}
		if !hasValue {
			i++
			if i == len(args) {
				return o, false
			}
			value = args[i]
		}
		switch name {
		case "--url":
			urls = append(urls, value)
		case "--output", "-o":
			o.Output = value
		case "--user-agent", "-A":
			o.UserAgent = value
		case "--referer", "-e":
			if strings.Contains(value, ";auto") {
				return o, false
			}
			o.Referer = value
		case "--cookie", "-b":
			if value != "/dev/null" && !strings.Contains(value, "=") {
				return o, false
			}
			o.Cookie = value
		case "--user", "-u":
			if !strings.Contains(value, ":") {
				return o, false
			}
			o.User = value
		case "--cacert":
			o.CACert = value
		case "--continue-at", "-C":
			if value != "-" {
				return o, false
			}
			o.Resume = true
		case "--proto-redir":
			if value != "=https" {
				return o, false
			}
			o.HTTPSRedirectOnly = true
		case "--header", "-H":
			key, v, ok := strings.Cut(value, ":")
			if !ok || strings.TrimSpace(key) != key || key == "" || strings.ContainsAny(value, "\r\n") {
				return o, false
			}
			key = http.CanonicalHeaderKey(key)
			// These alter framing, representation, or conditional GET semantics.
			switch key {
			case "Range", "If-Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "Accept-Encoding", "Host", "Content-Length", "Transfer-Encoding", "Connection":
				return o, false
			}
			if strings.TrimSpace(v) == "" {
				return o, false
			}
			o.Headers.Add(key, strings.TrimSpace(v))
		case "--retry", "--max-redirs":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 || n > 100000 {
				return o, false
			}
			if name == "--retry" {
				o.Retries = n
			} else {
				o.MaxRedirects = n
			}
		case "--connect-timeout", "--max-time", "--retry-delay", "--retry-max-time":
			if name == "--retry-delay" || name == "--retry-max-time" {
				if _, err := strconv.ParseUint(value, 10, 63); err != nil {
					return o, false
				}
			}
			n, err := strconv.ParseFloat(value, 64)
			if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) || n >= float64(math.MaxInt64)/float64(time.Second) {
				return o, false
			}
			d := time.Duration(n * float64(time.Second))
			if n > 0 && d <= 0 {
				return o, false
			}
			switch name {
			case "--connect-timeout":
				o.ConnectTimeout = d
			case "--max-time":
				o.MaxTime = d
				o.maxTimeArg, o.maxTimeInline = i, hasValue
			case "--retry-delay":
				o.RetryDelay = d
			case "--retry-max-time":
				o.RetryMaxTime = d
			}
		default:
			return o, false
		}
	}
	if len(urls) != 1 || o.Output == "" || o.Output == "-" || o.Output == "/dev/null" {
		return o, false
	}
	u, err := url.Parse(urls[0])
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return o, false
	}
	if !globoff && strings.ContainsAny(urls[0], "{}[]") {
		return o, false
	}
	o.URL = urls[0]
	return o, true
}

// ArgsWithMaxTime changes only the effective timeout, including when -- is
// present or some unrelated argument happens to resemble an option.
func (o Options) ArgsWithMaxTime(args []string, remaining time.Duration) []string {
	if o.maxTimeArg < 0 || o.maxTimeArg >= len(args) {
		return args
	}
	out := append([]string(nil), args...)
	value := strconv.FormatFloat(max(0.000001, remaining.Seconds()), 'f', 6, 64)
	if o.maxTimeInline {
		value = "--max-time=" + value
	}
	out[o.maxTimeArg] = value
	return out
}
