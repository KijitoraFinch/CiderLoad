package download

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"ciderload/internal/curlargs"
	"golang.org/x/net/publicsuffix"
)

type remote struct {
	Size           int64
	ETag, Modified string
	URL            string
	Headers        http.Header
}

func newTransport(o curlargs.Options) (*http.Transport, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.CACert != "" {
		pem, err := os.ReadFile(o.CACert)
		if err != nil {
			return nil, failure(77, "could not read CA certificate file", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, failure(77, "CA certificate file contains no certificates", nil)
		}
		tlsConfig.RootCAs = pool
	}
	dialTimeout := o.ConnectTimeout
	if dialTimeout == 0 {
		dialTimeout = 300 * time.Second
	}
	p := new(http.Protocols)
	p.SetHTTP1(true)
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Protocols: p, TLSClientConfig: tlsConfig,
		DialContext: dialer.DialContext,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// One connect budget covers DNS, TCP and TLS together, as in curl.
			ctx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			tc := tlsConfig.Clone()
			tc.ServerName, _, err = net.SplitHostPort(addr)
			if err != nil {
				conn.Close()
				return nil, err
			}
			tlsConn := tls.Client(conn, tc)
			if err = tlsConn.HandshakeContext(ctx); err != nil {
				conn.Close()
				return nil, err
			}
			return tlsConn, nil
		},
		TLSHandshakeTimeout: dialTimeout, DisableCompression: true,
		MaxIdleConns: 1, MaxIdleConnsPerHost: 1, MaxConnsPerHost: 1,
		IdleConnTimeout: 30 * time.Second,
	}, nil
}

func newClient(o curlargs.Options, tr *http.Transport, jar http.CookieJar) *http.Client {
	return &http.Client{Transport: tr, Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if !o.Location {
			return http.ErrUseLastResponse
		}
		if len(via) > o.MaxRedirects {
			return failure(47, "maximum redirects exceeded", nil)
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return failure(1, "redirect protocol is not supported", nil)
		}
		if o.HTTPSRedirectOnly && req.URL.Scheme != "https" {
			return failure(1, "redirect protocol is restricted to HTTPS", nil)
		}
		// Curl scopes credentials to the initial origin, including port and
		// scheme. Go's default also permits subdomains, so tighten it here.
		if !sameOrigin(req.URL, via[0].URL) {
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			req.URL.User = nil
		}
		// Curl does not invent a Referer header on redirects without ;auto.
		req.Header.Del("Referer")
		if o.Referer != "" {
			req.Header.Set("Referer", o.Referer)
		}
		return nil
	}}
}

func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Hostname(), b.Hostname()) && a.Scheme == b.Scheme && port(a) == port(b)
}

func request(ctx context.Context, o curlargs.Options, rawURL string, headers http.Header, start, end int64, validator string) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	r.Header = headers.Clone()
	if r.Header.Get("Accept") == "" {
		r.Header.Set("Accept", "*/*")
	}
	if o.UserAgent != "" && r.Header.Get("User-Agent") == "" {
		r.Header.Set("User-Agent", o.UserAgent)
	}
	if _, ok := r.Header["User-Agent"]; !ok {
		r.Header["User-Agent"] = []string{""}
	}
	if o.Referer != "" {
		r.Header.Set("Referer", o.Referer)
	}
	r.Header.Set("Accept-Encoding", "identity")
	r.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	if validator != "" {
		r.Header.Set("If-Range", validator)
	}
	return r, nil
}

func baseHeaders(o curlargs.Options) http.Header {
	h := o.Headers.Clone()
	if o.Cookie != "" && o.Cookie != "/dev/null" {
		h.Set("Cookie", o.Cookie)
	}
	if o.User != "" && h.Get("Authorization") == "" {
		u, p, _ := strings.Cut(o.User, ":")
		r := &http.Request{Header: h}
		r.SetBasicAuth(u, p)
	}
	return h
}

func probe(ctx context.Context, o curlargs.Options, jar http.CookieJar) (remote, error) {
	tr, err := newTransport(o)
	if err != nil {
		return remote{}, err
	}
	defer tr.CloseIdleConnections()
	req, err := request(ctx, o, o.URL, baseHeaders(o), 0, 0, "")
	if err != nil {
		return remote{}, err
	}
	resp, err := newClient(o, tr, jar).Do(req)
	if err != nil {
		return remote{}, err
	}
	defer resp.Body.Close()
	// Empty resources and servers rejecting Range still work with a plain GET.
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable || resp.StatusCode == http.StatusBadRequest {
		return remote{}, Fallback
	}
	if err = statusError(resp, o); err != nil {
		return remote{}, err
	}
	if resp.StatusCode != http.StatusPartialContent || encoded(resp) {
		return remote{}, Fallback
	}
	start, end, total, err := parseRange(resp.Header.Get("Content-Range"))
	if err != nil || start != 0 || end != 0 || total < 1 || (resp.ContentLength != -1 && resp.ContentLength != 1) {
		return remote{}, Fallback
	}
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 2))
	if err != nil {
		return remote{}, err
	}
	if n != 1 {
		return remote{}, Fallback
	}
	// Cookies added by the jar are regenerated for each range request. Only
	// caller-supplied cookies surviving the redirect chain are retained.
	h := resp.Request.Header.Clone()
	h.Del("Range")
	h.Del("If-Range")
	h.Del("Cookie")
	initial, _ := url.Parse(o.URL)
	if sameOrigin(initial, resp.Request.URL) {
		if c := baseHeaders(o).Get("Cookie"); c != "" {
			h.Set("Cookie", c)
		}
	}
	return remote{Size: total, ETag: resp.Header.Get("ETag"), Modified: resp.Header.Get("Last-Modified"), URL: resp.Request.URL.String(), Headers: h}, nil
}

func newJar() (http.CookieJar, error) {
	return cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
}

func parseRange(s string) (start, end, total int64, err error) {
	if !strings.HasPrefix(s, "bytes ") {
		return 0, 0, 0, errors.New("missing byte range")
	}
	rangePart, size, ok := strings.Cut(strings.TrimPrefix(s, "bytes "), "/")
	if !ok {
		return 0, 0, 0, errors.New("invalid byte range")
	}
	a, b, ok := strings.Cut(rangePart, "-")
	if !ok {
		return 0, 0, 0, errors.New("invalid byte range")
	}
	for _, part := range []string{a, b, size} {
		if part == "" {
			return 0, 0, 0, errors.New("empty byte range component")
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, 0, 0, errors.New("invalid byte range component")
			}
		}
	}
	start, err = strconv.ParseInt(a, 10, 64)
	if err != nil {
		return
	}
	end, err = strconv.ParseInt(b, 10, 64)
	if err != nil {
		return
	}
	total, err = strconv.ParseInt(size, 10, 64)
	if err == nil && (start < 0 || end < start || total <= end) {
		err = errors.New("inconsistent byte range")
	}
	return
}

func encoded(r *http.Response) bool {
	v := r.Header.Get("Content-Encoding")
	return v != "" && !strings.EqualFold(v, "identity")
}
func validator(r remote) string {
	if r.ETag != "" && !strings.HasPrefix(r.ETag, "W/") {
		return r.ETag
	}
	if _, err := http.ParseTime(r.Modified); err == nil {
		return r.Modified
	}
	return ""
}
func statusError(r *http.Response, o curlargs.Options) error {
	if r.StatusCode < 400 {
		return nil
	}
	retry := false
	switch r.StatusCode {
	case 408, 429, 500, 502, 503, 504:
		retry = true
	}
	if !o.Fail {
		return Fallback
	}
	delay := time.Duration(0)
	if n, err := strconv.ParseInt(r.Header.Get("Retry-After"), 10, 32); err == nil && n > 0 {
		delay = time.Duration(n) * time.Second
	} else if date, err := http.ParseTime(r.Header.Get("Retry-After")); err == nil {
		delay = max(0, time.Until(date))
	}
	return &Error{Code: 22, Message: fmt.Sprintf("server returned HTTP %d", r.StatusCode), Retry: retry, RetryAfter: delay}
}
