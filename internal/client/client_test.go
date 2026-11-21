// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflogtest"

	"github.com/muster-io/terraform-provider-muster/internal/client/gen"
)

const testToken = "mstr_sat_secret-token-value"

type recorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	method, path, auth, userAgent, body string
}

func (r *recorder) add(req *http.Request) int {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, recordedRequest{
		method: req.Method, path: req.URL.Path, auth: req.Header.Get("Authorization"),
		userAgent: req.Header.Get("User-Agent"), body: string(body),
	})
	return len(r.requests)
}

type sleeper struct {
	mu    sync.Mutex
	waits []time.Duration
	err   error
}

func (s *sleeper) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waits = append(s.waits, d)
	return s.err
}

func newTestClient(t *testing.T, url string, s *sleeper) *Client {
	t.Helper()
	c, err := New(Config{Endpoint: url + "/", Token: testToken, Version: "1.2.3", Sleep: s.sleep,
		Now: func() time.Time { return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func statusJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"version":"0.2.0","replicas":[],"recovery":{"active":false},"delivery_queues":[],`+
		`"broken_destinations":[],"integrations_attention":[],"template_errors":[],"active_storms":[],`+
		`"outgoing_heartbeat":{}}`)
}

func problem(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestClient_sendsBearerTokenAndUserAgent(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		statusJSON(w)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL, &sleeper{}).GetSystemStatusWithResponse(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON200 == nil || resp.JSON200.Version != "0.2.0" {
		t.Fatalf("answer %d %s", resp.StatusCode(), resp.Body)
	}
	got := rec.requests[0]
	if got.path != "/api/v1/system-status" || got.auth != "Bearer "+testToken ||
		got.userAgent != "terraform-provider-muster/1.2.3" {
		t.Fatalf("request %+v", got)
	}
}

func TestParseEndpoint(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://muster.example.org", "https://muster.example.org"},
		{"https://muster.example.org/", "https://muster.example.org"},
		{"http://127.0.0.1:8080/muster/", "http://127.0.0.1:8080/muster"},
	} {
		if got, err := ParseEndpoint(tc.in); err != nil || got != tc.want {
			t.Errorf("ParseEndpoint(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"", "muster.example.org", "ftp://muster.example.org", "https://", "https://u:p@h",
		"https://h/?a=b", "https://h/#x", "://"} {
		if _, err := ParseEndpoint(in); err == nil {
			t.Errorf("ParseEndpoint(%q) accepted", in)
		}
	}
	if _, err := New(Config{Endpoint: "nope"}); err == nil {
		t.Error("New accepted an endpoint without a scheme")
	}
}

func TestParseEndpoint_neverRepeatsASecret(t *testing.T) {
	const (
		secret  = "mstr_sat_hunter2-secret"
		generic = "the endpoint is not an http or https URL with a host"
	)
	for _, tc := range []struct{ in, shown string }{
		{"https://admin:" + secret + "@muster.example.org", `"https://muster.example.org" is not Muster's URL`},
		{"https://" + secret + "@h", `"https://h" is not Muster's URL`},
		{"https://" + secret + ":pw@h/muster", `"https://h/muster" is not Muster's URL`},
		{"https://muster.example.org/?token=" + secret, `"https://muster.example.org/" is not Muster's URL`},
		{"https://muster.example.org/#" + secret, `"https://muster.example.org/" is not Muster's URL`},
		{secret, generic},
		{secret + ":x", generic},
		{"token:" + secret, generic},
		{"ftp://" + secret + "@h", generic},
		{"https://admin:" + secret + "@muster.example.org:port", generic},
		{"\x7fhttps://admin:" + secret + "@h", generic},
	} {
		_, err := ParseEndpoint(tc.in)
		if err == nil {
			t.Fatalf("ParseEndpoint(%q) accepted", tc.in)
		}
		if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), tc.shown) {
			t.Errorf("ParseEndpoint(%q): %v, want %s without the secret", tc.in, err, tc.shown)
		}
	}
}

func TestClient_neverFollowsARedirect(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	s := &sleeper{}

	resp, err := newTestClient(t, srv.URL, s).GetSystemStatusWithResponse(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.requests) != 1 || rec.requests[0].path != "/api/v1/system-status" || len(s.waits) != 0 {
		t.Fatalf("requests %+v, waits %v", rec.requests, s.waits)
	}
	err = CheckResponse(resp.HTTPResponse, resp.Body)
	if StatusOf(err) != http.StatusFound || !strings.Contains(err.Error(), "GET /api/v1/system-status: 302") {
		t.Fatalf("error %v", err)
	}
}

func TestClient_retries429AfterRetryAfterAndResendsTheBody(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := rec.add(r); n <= 2 {
			w.Header().Set("Retry-After", "7")
			problem(w, http.StatusTooManyRequests, `{"type":"x","title":"Rate limited","status":429}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"SA1","name":"terraform","role":"admin","status":"active","token_count":0,`+
			`"created_at":"2026-11-21T12:00:00Z","etag":"\"1\""}`)
	}))
	defer srv.Close()
	s := &sleeper{}

	resp, err := newTestClient(t, srv.URL, s).CreateServiceAccountWithResponse(t.Context(),
		gen.ServiceAccountInput{Name: "terraform", Role: gen.RoleNameAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode())
	}
	if len(rec.requests) != 3 {
		t.Fatalf("%d requests, want 3", len(rec.requests))
	}
	for _, r := range rec.requests {
		if r.method != http.MethodPost || !strings.Contains(r.body, `"name":"terraform"`) {
			t.Fatalf("request %+v", r)
		}
	}
	if len(s.waits) != 2 || s.waits[0] != 7*time.Second || s.waits[1] != 7*time.Second {
		t.Fatalf("waits %v", s.waits)
	}
}

func TestClient_retries429AtMostFiveTimes(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		problem(w, http.StatusTooManyRequests, `{"type":"x","title":"Rate limited","status":429}`)
	}))
	defer srv.Close()
	s := &sleeper{}

	resp, err := newTestClient(t, srv.URL, s).GetSystemStatusWithResponse(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusTooManyRequests || len(rec.requests) != 1+MaxRateLimitRetries {
		t.Fatalf("status %d after %d requests", resp.StatusCode(), len(rec.requests))
	}
	for _, w := range s.waits {
		if w != defaultRetryAfter {
			t.Fatalf("waits %v, want %v without Retry-After", s.waits, defaultRetryAfter)
		}
	}
	err = CheckResponse(resp.HTTPResponse, resp.Body)
	if StatusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("error %v", err)
	}
}

func TestClient_retriesAGetOn5xxWithBackoff(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch rec.add(r) {
		case 1:
			w.WriteHeader(http.StatusBadGateway)
		case 2:
			w.WriteHeader(http.StatusInternalServerError)
		case 3:
			w.Header().Set("Retry-After", "Sat, 21 Nov 2026 12:00:09 GMT")
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			statusJSON(w)
		}
	}))
	defer srv.Close()
	s := &sleeper{}

	resp, err := newTestClient(t, srv.URL, s).GetSystemStatusWithResponse(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusOK || len(rec.requests) != 4 {
		t.Fatalf("status %d after %d requests", resp.StatusCode(), len(rec.requests))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 9 * time.Second}
	if len(s.waits) != 3 || s.waits[0] != want[0] || s.waits[1] != want[1] || s.waits[2] != want[2] {
		t.Fatalf("waits %v, want %v", s.waits, want)
	}
}

func TestClient_givesUpAGetAfterMaxReadRetries(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL, &sleeper{}).GetSystemStatusWithResponse(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusBadGateway || len(rec.requests) != 1+MaxReadRetries {
		t.Fatalf("status %d after %d requests", resp.StatusCode(), len(rec.requests))
	}
}

func TestClient_neverRetriesAGetOn501(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		problem(w, http.StatusNotImplemented, `{"type":"x","title":"Not Implemented","status":501}`)
	}))
	defer srv.Close()
	s := &sleeper{}

	resp, err := newTestClient(t, srv.URL, s).GetSystemStatusWithResponse(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusNotImplemented || len(rec.requests) != 1 || len(s.waits) != 0 {
		t.Fatalf("status %d after %d requests, waits %v", resp.StatusCode(), len(rec.requests), s.waits)
	}
}

func TestClient_capsTheWaitOfRetryAfter(t *testing.T) {
	for _, header := range []string{"3600", "Sun, 22 Nov 2026 12:00:00 GMT", "99999999999999999999"} {
		rec := &recorder{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rec.add(r) == 1 {
				w.Header().Set("Retry-After", header)
				problem(w, http.StatusTooManyRequests, `{"type":"x","title":"Rate limited","status":429}`)
				return
			}
			statusJSON(w)
		}))
		s := &sleeper{}
		resp, err := newTestClient(t, srv.URL, s).GetSystemStatusWithResponse(t.Context())
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode() != http.StatusOK || len(s.waits) != 1 || s.waits[0] != MaxRetryAfter {
			t.Fatalf("Retry-After %q: status %d, waits %v, want [%v]", header, resp.StatusCode(), s.waits,
				MaxRetryAfter)
		}
	}
}

// writeMethods are the methods that are never sent twice after a 5xx or an error in transit.
var writeMethods = []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// send sends a request with a JSON body, which the transport could send again, through c's HTTP client, and returns
// the answer's status.
func send(t *testing.T, c *Client, method string) (int, error) {
	t.Helper()
	doer := c.ClientInterface.(*gen.Client).Client
	req, err := http.NewRequestWithContext(t.Context(), method, c.Endpoint+"/api/v1/service-accounts/SA1",
		strings.NewReader(`{"name":"terraform"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.GetBody == nil {
		t.Fatal("the body cannot be sent again: the test would not prove anything")
	}
	resp, err := doer.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func TestClient_neverRetriesAWriteOn5xx(t *testing.T) {
	for _, method := range writeMethods {
		for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway,
			http.StatusServiceUnavailable} {
			rec := &recorder{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.add(r)
				w.WriteHeader(status)
			}))
			s := &sleeper{}
			got, err := send(t, newTestClient(t, srv.URL, s), method)
			srv.Close()
			if err != nil {
				t.Fatal(err)
			}
			if got != status || len(rec.requests) != 1 || rec.requests[0].method != method || len(s.waits) != 0 {
				t.Fatalf("%s %d: status %d after %d requests", method, status, got, len(rec.requests))
			}
		}
	}

	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	resp, err := newTestClient(t, srv.URL, &sleeper{}).CreateServiceAccountWithResponse(t.Context(),
		gen.ServiceAccountInput{Name: "terraform", Role: gen.RoleNameAdmin})
	if err != nil || resp.StatusCode() != http.StatusBadGateway || len(rec.requests) != 1 {
		t.Fatalf("generated POST: %v after %d requests", err, len(rec.requests))
	}
}

type flakyTransport struct {
	mu       sync.Mutex
	failures int
	calls    int
	methods  []string
}

func (f *flakyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.methods = append(f.methods, req.Method)
	if f.calls <= f.failures {
		return nil, errors.New("connection reset by peer")
	}
	rec := httptest.NewRecorder()
	statusJSON(rec)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

func TestClient_retriesAGetOnATransientError(t *testing.T) {
	ft := &flakyTransport{failures: 2}
	s := &sleeper{}
	c, err := New(Config{Endpoint: "https://muster.example.org", Token: testToken, Transport: ft, Sleep: s.sleep})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.GetSystemStatusWithResponse(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusOK || ft.calls != 3 {
		t.Fatalf("status %d after %d calls", resp.StatusCode(), ft.calls)
	}
}

func TestClient_neverRetriesAWriteOnATransientError(t *testing.T) {
	for _, method := range writeMethods {
		ft := &flakyTransport{failures: 1}
		c, err := New(Config{Endpoint: "https://muster.example.org", Token: testToken, Transport: ft,
			Sleep: (&sleeper{}).sleep})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := send(t, c, method); err == nil || ft.calls != 1 || ft.methods[0] != method {
			t.Fatalf("%s: error %v after %d calls", method, err, ft.calls)
		}
	}
}

func TestClient_stopsWhenTheWaitIsCancelled(t *testing.T) {
	ft := &flakyTransport{failures: 5}
	cancelled := &sleeper{err: context.Canceled}
	c, err := New(Config{Endpoint: "https://muster.example.org", Token: testToken, Transport: ft,
		Sleep: cancelled.sleep})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetSystemStatusWithResponse(t.Context()); !errors.Is(err, context.Canceled) || ft.calls != 1 {
		t.Fatalf("error %v after %d calls", err, ft.calls)
	}
}

type deadlineTransport struct{ remaining time.Duration }

func (d *deadlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if deadline, ok := req.Context().Deadline(); ok {
		d.remaining = time.Until(deadline)
	}
	rec := httptest.NewRecorder()
	statusJSON(rec)
	return rec.Result(), nil
}

func TestClient_boundsEveryAttemptBySixtySeconds(t *testing.T) {
	dt := &deadlineTransport{}
	c, err := New(Config{Endpoint: "https://muster.example.org", Token: testToken, Transport: dt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetSystemStatusWithResponse(t.Context()); err != nil {
		t.Fatal(err)
	}
	if dt.remaining <= RequestTimeout-10*time.Second || dt.remaining > RequestTimeout {
		t.Fatalf("deadline in %v, want about %v", dt.remaining, RequestTimeout)
	}
}

func TestClient_logsRequestsWithoutTheToken(t *testing.T) {
	var out bytes.Buffer
	ctx := tflogtest.RootLogger(t.Context(), &out)
	ft := &flakyTransport{failures: 1}
	c, err := New(Config{Endpoint: "https://muster.example.org", Token: testToken, Transport: ft,
		Sleep: (&sleeper{}).sleep})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetSystemStatusWithResponse(ctx); err != nil {
		t.Fatal(err)
	}
	logged := out.String()
	if strings.Contains(logged, testToken) {
		t.Fatalf("the token was logged:\n%s", logged)
	}
	for _, want := range []string{`"Authorization":"[redacted]"`, "Muster API request", "Muster API response",
		"Muster API request failed", "Muster API request retried", "/api/v1/system-status"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log without %s:\n%s", want, logged)
		}
	}
}

func TestRedactError(t *testing.T) {
	h := http.Header{"Authorization": []string{"Bearer " + testToken}}
	if got := redactError(errors.New("boom "+testToken), h).Error(); got != "boom [redacted]" {
		t.Fatalf("got %q", got)
	}
	plain := errors.New("boom")
	if !errors.Is(redactError(plain, h), plain) {
		t.Fatal("an error without the token was replaced")
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"", 3 * time.Second},
		{"12", 12 * time.Second},
		{"-1", 0},
		{"Sat, 21 Nov 2026 12:00:30 GMT", 30 * time.Second},
		{"Sat, 21 Nov 2026 11:59:30 GMT", 0},
		{"soon", 3 * time.Second},
		{"60", MaxRetryAfter},
		{"61", MaxRetryAfter},
		{"-99999999999999999999", 0},
		{"Sat, 21 Nov 2026 13:00:00 GMT", MaxRetryAfter},
	} {
		if got := retryAfter(tc.header, now, 3*time.Second); got != tc.want {
			t.Errorf("retryAfter(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		16 * time.Second}
	for i, w := range want {
		if got := backoff(i); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i, got, w)
		}
	}
	if backoff(80) != maxBackoff {
		t.Error("backoff overflows")
	}
}

func TestSleep(t *testing.T) {
	if err := sleep(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if err := sleep(t.Context(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
}

func TestETag(t *testing.T) {
	if ETag(nil) != "" {
		t.Fatal("ETag of no answer")
	}
	resp := &http.Response{Header: http.Header{"Etag": []string{`"7"`}}}
	if ETag(resp) != `"7"` {
		t.Fatal(ETag(resp))
	}
}
