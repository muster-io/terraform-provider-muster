// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

// Package client is the provider's client of Muster's API: the client generated from the pinned copy of the
// specification, with the bearer token, the User-Agent, a timeout per request, the retries and the problems.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/muster-io/terraform-provider-muster/internal/client/gen"
)

const (
	// RequestTimeout bounds every attempt of a request, from sending it to reading its whole answer.
	RequestTimeout = 60 * time.Second
	// MaxRateLimitRetries is how often a request answered 429 is sent again, after its Retry-After.
	MaxRateLimitRetries = 5
	// MaxReadRetries is how often a read that failed in transit or with 5xx other than 501 is sent again, with
	// back-off.
	MaxReadRetries = 4
	// MaxRetryAfter caps the wait that a Retry-After asks for, so that a far-off or wrong one cannot stall an apply;
	// the request is sent again after this long at the latest.
	MaxRetryAfter = 60 * time.Second

	firstBackoff      = time.Second
	maxBackoff        = 16 * time.Second
	defaultRetryAfter = time.Second
	redacted          = "[redacted]"
)

// Config is what New needs. Transport, Sleep and Now are replaced by tests.
type Config struct {
	// Endpoint is Muster's public URL, without /api/v1.
	Endpoint string
	Token    string
	// Version is the provider's version, sent in the User-Agent.
	Version   string
	Transport http.RoundTripper
	Sleep     func(ctx context.Context, d time.Duration) error
	Now       func() time.Time
}

// Client is the generated client of Muster's API at <endpoint>/api/v1.
type Client struct {
	*gen.ClientWithResponses
	Endpoint string
}

// New returns the client of the Muster at cfg.Endpoint.
func New(cfg Config) (*Client, error) {
	endpoint, err := ParseEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	next := cfg.Transport
	if next == nil {
		next = http.DefaultTransport.(*http.Transport).Clone()
	}
	retry := &retryTransport{next: &logTransport{next: next}, sleep: cfg.Sleep, now: cfg.Now}
	if retry.sleep == nil {
		retry.sleep = sleep
	}
	if retry.now == nil {
		retry.now = time.Now
	}
	userAgent := "terraform-provider-muster/" + cfg.Version
	token := cfg.Token
	httpClient := &http.Client{
		Transport: retry,
		// A redirect is never followed: it would send the token elsewhere. A 3xx is an error of the API.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	api, err := gen.NewClientWithResponses(endpoint+"/api/v1",
		gen.WithHTTPClient(httpClient),
		gen.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("User-Agent", userAgent)
			return nil
		}))
	if err != nil {
		return nil, fmt.Errorf("the client of %s: %w", endpoint, err)
	}
	return &Client{ClientWithResponses: api, Endpoint: endpoint}, nil
}

// ParseEndpoint checks that endpoint is an http or https URL without user information, a query or a fragment, and
// returns it without a trailing slash. Its errors never repeat the input, which may be a token pasted in the wrong
// place: an http or https URL with a host is shown only by its scheme, host and path, and anything else not at all.
func ParseEndpoint(endpoint string) (string, error) {
	const expected = "expected http(s)://host[:port][/path], without user information, a query or a fragment"
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("the endpoint is not an http or https URL with a host: " + expected)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		shown := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
		return "", fmt.Errorf("%q is not Muster's URL: %s", shown.String(), expected)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// ETag returns the ETag header of an answer.
func ETag(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	return resp.Header.Get("ETag")
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryTransport sends a request again after 429, at most MaxRateLimitRetries times, and a GET that failed in transit
// or with 5xx with back-off, at most MaxReadRetries times; other requests are never sent twice, nor is a GET answered
// 501, which Muster gives to an operation it does not implement. A Retry-After replaces the wait, up to MaxRetryAfter.
type retryTransport struct {
	next  http.RoundTripper
	sleep func(context.Context, time.Duration) error
	now   func() time.Time
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	rateLimited, failed := 0, 0
	for attempt := 0; ; attempt++ {
		resp, err := t.send(req, attempt)
		if err != nil && ctx.Err() != nil {
			return nil, err
		}
		var wait time.Duration
		switch {
		case !replayable:
			return resp, err
		case err == nil && resp.StatusCode == http.StatusTooManyRequests && rateLimited < MaxRateLimitRetries:
			rateLimited++
			wait = retryAfter(resp.Header.Get("Retry-After"), t.now(), defaultRetryAfter)
		case req.Method == http.MethodGet && failed < MaxReadRetries && (err != nil || retryableStatus(resp.StatusCode)):
			wait = backoff(failed)
			if resp != nil {
				wait = retryAfter(resp.Header.Get("Retry-After"), t.now(), wait)
			}
			failed++
		default:
			return resp, err
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
		}
		tflog.Debug(ctx, "Muster API request retried", map[string]any{
			"method": req.Method, "url": req.URL.Redacted(), "attempt": attempt + 2, "wait": wait.String(),
		})
		if err := t.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

func (t *retryTransport) send(req *http.Request, attempt int) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(req.Context(), RequestTimeout)
	r := req.Clone(ctx)
	if attempt > 0 && req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			cancel()
			return nil, err
		}
		r.Body = body
	}
	resp, err := t.next.RoundTrip(r)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func retryableStatus(status int) bool {
	return status >= 500 && status != http.StatusNotImplemented
}

func backoff(failed int) time.Duration {
	d := firstBackoff << failed
	if d > maxBackoff || d <= 0 {
		return maxBackoff
	}
	return d
}

// retryAfter reads a Retry-After header, in seconds or as an HTTP date, capped at MaxRetryAfter, and returns fallback
// when there is none.
func retryAfter(header string, now time.Time, fallback time.Duration) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return fallback
	}
	// Out of range, ParseInt returns the largest or smallest int64, which the cases below handle.
	if seconds, err := strconv.ParseInt(header, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		switch {
		case seconds < 0:
			return 0
		case seconds > int64(MaxRetryAfter/time.Second):
			return MaxRetryAfter
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(header); err == nil {
		return min(max(at.Sub(now), 0), MaxRetryAfter)
	}
	return fallback
}

// logTransport logs every attempt through tflog, the Authorization header as [redacted].
type logTransport struct {
	next http.RoundTripper
}

func (t *logTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	tflog.Debug(ctx, "Muster API request", map[string]any{
		"method": req.Method, "url": req.URL.Redacted(), "headers": redactHeaders(req.Header),
	})
	started := time.Now()
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		tflog.Debug(ctx, "Muster API request failed", map[string]any{
			"method": req.Method, "url": req.URL.Redacted(), "error": redactError(err, req.Header).Error(),
		})
		return nil, err
	}
	tflog.Debug(ctx, "Muster API response", map[string]any{
		"method": req.Method, "url": req.URL.Redacted(), "status": resp.StatusCode,
		"duration_ms": time.Since(started).Milliseconds(),
	})
	return resp, nil
}

func redactHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for name, values := range h {
		switch http.CanonicalHeaderKey(name) {
		case "Authorization", "Cookie", "X-Csrf-Token":
			out[name] = redacted
		default:
			out[name] = strings.Join(values, ", ")
		}
	}
	return out
}

func redactError(err error, h http.Header) error {
	text := err.Error()
	auth := h.Get("Authorization")
	if token, ok := strings.CutPrefix(auth, "Bearer "); ok && token != "" && strings.Contains(text, token) {
		return errors.New(strings.ReplaceAll(text, token, redacted))
	}
	return err
}
