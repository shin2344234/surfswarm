package engine

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/shin2344234/surfswarm/internal/protocol"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"dns", &net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}, "dns"},
		{"dns wrapped", &url.Error{Op: "Get", URL: "https://x.invalid/", Err: &net.DNSError{Err: "no such host"}}, "dns"},
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"tls text", errors.New("tls: handshake failure"), "tls"},
		{"x509 text", errors.New("x509: certificate signed by unknown authority"), "tls"},
		{"refused", &url.Error{Err: errors.New("dial tcp 10.0.0.1:443: connect: connection refused")}, "connect"},
		{"dial op", &net.OpError{Op: "dial", Err: errors.New("boom")}, "connect"},
		{"read op", &net.OpError{Op: "read", Err: errors.New("boom")}, "network"},
		{"h2 reset", errors.New("stream error: stream ID 1; INTERNAL_ERROR; received from peer"), "other"},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("%s: Classify = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestClassifyStatus(t *testing.T) {
	cases := map[int]string{200: "", 302: "", 403: "blocked", 429: "blocked", 404: "http_4xx", 500: "http_5xx", 503: "http_5xx"}
	for code, want := range cases {
		if got := ClassifyStatus(code); got != want {
			t.Errorf("status %d: got %q, want %q", code, got, want)
		}
	}
}

func TestPercentiles(t *testing.T) {
	if p50, p95 := percentiles(nil); p50 != 0 || p95 != 0 {
		t.Fatalf("empty: %v %v", p50, p95)
	}
	if p50, p95 := percentiles([]float64{7}); p50 != 7 || p95 != 7 {
		t.Fatalf("single: %v %v", p50, p95)
	}
	v := make([]float64, 100)
	for i := range v {
		v[99-i] = float64(i + 1) // reversed on purpose; percentiles must sort
	}
	p50, p95 := percentiles(v)
	if p50 != 50 || p95 != 95 {
		t.Fatalf("1..100: p50=%v p95=%v", p50, p95)
	}
}

func TestErrMessage(t *testing.T) {
	inner := errors.New("connection reset by peer")
	if got := errMessage(&url.Error{Op: "Get", URL: "https://x/", Err: inner}); got != inner.Error() {
		t.Fatalf("wrapper not stripped: %q", got)
	}
	long := errors.New(strings.Repeat("a", 500))
	if got := errMessage(long); len(got) != 300 {
		t.Fatalf("not truncated: %d", len(got))
	}
	if errMessage(nil) != "" {
		t.Fatal("nil should be empty")
	}
}

func TestSetBrowserHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	SetBrowserHeaders(req, "UA/1")
	if req.Header.Get("User-Agent") != "UA/1" || req.Header.Get("Accept-Encoding") == "" || req.Header.Get("Accept-Language") == "" {
		t.Fatalf("headers: %v", req.Header)
	}
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	body := strings.Repeat("x", 4096)
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		// An explicit length keeps the response unchunked, like a static file.
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/blocked", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no bots", http.StatusForbidden) })
	mux.HandleFunc("/stall", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestRunCountsRequestsAndLogsErrors(t *testing.T) {
	srv := newTestServer(t)
	spec := protocol.TestSpec{ID: "t1", Mode: "get", Threads: 4, DurationS: 1, ThinkMinMs: 5, ThinkMaxMs: 10, TimeoutMs: 5000, URLs: []string{srv.URL + "/ok", srv.URL + "/blocked"}}
	var reports []protocol.Progress
	done := New(spec).Run(context.Background(), func(p protocol.Progress) { reports = append(reports, p) })

	if done.Summary.Requests == 0 {
		t.Fatal("no requests recorded")
	}
	if done.Summary.Bytes == 0 {
		t.Fatal("no bytes counted")
	}
	if done.ErrorsByClass["blocked"] == 0 {
		t.Fatalf("expected blocked errors, got %v", done.ErrorsByClass)
	}
	if done.Summary.Errors != done.ErrorsByClass["blocked"] {
		t.Fatalf("errors %d != blocked %d", done.Summary.Errors, done.ErrorsByClass["blocked"])
	}
	// Local requests can round to 0 ms on a coarse clock, so only check the
	// percentiles are ordered.
	if !done.Summary.Done || done.Summary.Mbps <= 0 || done.Summary.P95Ms < done.Summary.P50Ms {
		t.Fatalf("summary: %+v", done.Summary)
	}
	if len(reports) == 0 || !reports[len(reports)-1].Done {
		t.Fatalf("final report missing: %d reports", len(reports))
	}
	logged := 0
	for _, p := range reports {
		for _, e := range p.NewErrors {
			logged++
			if e.URL != srv.URL+"/blocked" || e.Status != 403 || e.Class != "blocked" || !strings.Contains(e.Message, "403") {
				t.Fatalf("bad error entry: %+v", e)
			}
		}
	}
	// Each report carries at most maxPendingErrors entries, so a local
	// server that fails hundreds of requests a second logs fewer than it
	// counts; every logged entry must still be a real failure.
	if logged == 0 || int64(logged) > done.Summary.Errors {
		t.Fatalf("logged %d errors, counted %d", logged, done.Summary.Errors)
	}
}

func TestStallTimeoutIsCountedAsTimeout(t *testing.T) {
	srv := newTestServer(t)
	spec := protocol.TestSpec{ID: "t2", Threads: 1, DurationS: 2, TimeoutMs: 300, URLs: []string{srv.URL + "/stall"}}
	done := New(spec).Run(context.Background(), func(protocol.Progress) {})
	if done.ErrorsByClass["timeout"] == 0 {
		t.Fatalf("expected timeouts, got %v", done.ErrorsByClass)
	}
}

func TestRunStopsWhenCancelled(t *testing.T) {
	srv := newTestServer(t)
	spec := protocol.TestSpec{ID: "t3", Threads: 2, DurationS: 0, ThinkMinMs: 10, ThinkMaxMs: 20, URLs: []string{srv.URL + "/ok"}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start := time.Now()
	done := New(spec).Run(ctx, func(protocol.Progress) {})
	if time.Since(start) > 3*time.Second {
		t.Fatal("did not stop promptly after cancel")
	}
	if done.Summary.Requests == 0 {
		t.Fatal("expected some requests before cancel")
	}
}

func TestRateLimitHoldsThroughput(t *testing.T) {
	big := strings.Repeat("y", 512<<10) // 512 KB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "524288")
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()
	// 8 Mbps is 1 MB/s; over two seconds with two workers expect about 2 MB,
	// give or take the initial burst.
	spec := protocol.TestSpec{ID: "rl", Threads: 2, DurationS: 2, RateMbps: 8, TimeoutMs: 10000, URLs: []string{srv.URL}}
	done := New(spec).Run(context.Background(), func(protocol.Progress) {})
	mb := float64(done.Summary.Bytes) / 1e6
	if mb < 1.5 || mb > 3.0 {
		t.Fatalf("rate limit: moved %.2f MB in 2 s at 8 Mbps", mb)
	}
}

func TestOpenLoopCadence(t *testing.T) {
	srv := newTestServer(t)
	spec := protocol.TestSpec{ID: "ol", Threads: 4, DurationS: 2, RequestsPerSec: 10, ThinkMinMs: 5000, ThinkMaxMs: 5000, TimeoutMs: 5000, URLs: []string{srv.URL + "/ok"}}
	done := New(spec).Run(context.Background(), func(protocol.Progress) {})
	// Think time is ignored in open loop, so this is about 20 requests, not the
	// one or two a 5 s think time would allow.
	if done.Summary.Requests < 14 || done.Summary.Requests > 26 {
		t.Fatalf("open loop at 10 req/s for 2 s made %d requests", done.Summary.Requests)
	}
	if done.Summary.Skipped != 0 {
		t.Fatalf("no dispatches should be skipped with idle workers, got %d", done.Summary.Skipped)
	}
	// One worker that cannot keep up must report skipped slots.
	slow := protocol.TestSpec{ID: "ol2", Threads: 1, DurationS: 1, RequestsPerSec: 50, TimeoutMs: 5000, URLs: []string{srv.URL + "/stall"}}
	done = New(slow).Run(context.Background(), func(protocol.Progress) {})
	if done.Summary.Skipped == 0 {
		t.Fatal("expected skipped dispatches when the only worker is stuck")
	}
}

func TestCheckURL(t *testing.T) {
	srv := newTestServer(t)
	client := &http.Client{Transport: NewTransport(2)}
	r := CheckURL(client, srv.URL+"/ok", 5*time.Second, 1024)
	if r.Class != "" || r.Status != 200 || r.Bytes != 1024 || r.Length != 4096 {
		t.Fatalf("ok check: %+v", r)
	}
	r = CheckURL(client, srv.URL+"/blocked", 5*time.Second, 0)
	if r.Class != "blocked" || r.Status != 403 || !strings.Contains(r.Message, "Forbidden") {
		t.Fatalf("blocked check: %+v", r)
	}
	r = CheckURL(client, "http://127.0.0.1:1/", 2*time.Second, 0)
	if r.Class != "connect" {
		t.Fatalf("refused check: %+v", r)
	}
}
