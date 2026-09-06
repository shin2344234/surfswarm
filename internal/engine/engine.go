// Package engine generates HTTP/HTTPS traffic for one test on one agent.
//
// The spike implements "get" mode: N workers each pick a random URL, fetch it,
// read the whole body, sleep a random think time, and repeat until the test
// duration ends or the context is cancelled.
package engine

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/shin2344234/surfswarm/internal/protocol"
)

// DefaultUserAgent looks like a current desktop browser so that sites serve
// their real pages instead of a bot-detection stub.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

// maxOverallSamples caps the per-request timing samples kept for the final
// summary percentiles.
const maxOverallSamples = 50000

// maxPendingErrors caps how many failure details one progress report carries.
const maxPendingErrors = 200

// Engine runs one test.
type Engine struct {
	spec    protocol.TestSpec
	client  *http.Client
	limiter *rate.Limiter // nil when RateMbps is 0

	requests atomic.Int64
	bytes    atomic.Int64
	errors   atomic.Int64
	winBytes atomic.Int64
	skipped  atomic.Int64
	active   atomic.Int32

	mu          sync.Mutex
	winReqs     int64
	winDurs     []float64
	allDurs     []float64
	errClasses  map[string]int64
	pendingErrs []protocol.RequestError
}

// New prepares an engine for spec, filling in defaults for zero values.
func New(spec protocol.TestSpec) *Engine {
	if spec.Threads <= 0 {
		spec.Threads = 1
	}
	if spec.TimeoutMs <= 0 {
		spec.TimeoutMs = 15000
	}
	if spec.ThinkMinMs < 0 {
		spec.ThinkMinMs = 0
	}
	if spec.ThinkMaxMs < spec.ThinkMinMs {
		spec.ThinkMaxMs = spec.ThinkMinMs
	}
	if spec.UserAgent == "" {
		spec.UserAgent = DefaultUserAgent
	}
	e := &Engine{
		spec:       spec,
		client:     &http.Client{Transport: NewTransport(spec.Threads)},
		errClasses: map[string]int64{},
	}
	if spec.RateMbps > 0 {
		bytesPerSec := spec.RateMbps * 1e6 / 8
		// The burst must cover one read (io.Copy uses 32 KB buffers) and
		// is otherwise a quarter second of allowance, so the rate holds
		// steady at the reporting resolution.
		burst := int(bytesPerSec / 4)
		if burst < 64<<10 {
			burst = 64 << 10
		}
		e.limiter = rate.NewLimiter(rate.Limit(bytesPerSec), burst)
	}
	return e
}

// NewTransport returns the transport the agent uses, sized for n workers.
// Compression is left to the caller: SetBrowserHeaders asks for gzip and the
// body is counted as wire bytes without decompressing.
func NewTransport(n int) *http.Transport {
	if n < 1 {
		n = 1
	}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          n * 4,
		MaxIdleConnsPerHost:   6,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

// SetBrowserHeaders makes a request look like a desktop browser navigation.
func SetBrowserHeaders(req *http.Request, userAgent string) {
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
}

// Run executes the test, calling report every protocol.ReportInterval and
// once more with Done set. It returns when every worker has exited.
func (e *Engine) Run(ctx context.Context, report func(protocol.Progress)) protocol.TestDone {
	start := time.Now()
	var cancel context.CancelFunc
	if e.spec.DurationS > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(e.spec.DurationS)*time.Second)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	// Open loop: a dispatcher hands out slots on a fixed cadence and workers
	// take them; think time is ignored. Closed loop: each worker runs its
	// own fetch/think cycle, with starts staggered so they do not fire in
	// lockstep.
	var sched chan struct{}
	if e.spec.RequestsPerSec > 0 {
		sched = make(chan struct{}, e.spec.Threads)
		go e.dispatch(ctx, sched)
	}
	var wg sync.WaitGroup
	for i := 0; i < e.spec.Threads; i++ {
		wg.Add(1)
		seed := time.Now().UnixNano() + int64(i)*7919
		stagger := time.Duration(0)
		if sched == nil {
			cycle := time.Duration(e.spec.ThinkMinMs+e.spec.ThinkMaxMs) / 2 * time.Millisecond
			if cycle > 2*time.Second {
				cycle = 2 * time.Second
			}
			stagger = cycle * time.Duration(i) / time.Duration(e.spec.Threads)
		}
		go func() {
			defer wg.Done()
			if stagger > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(stagger):
				}
			}
			if sched != nil {
				e.pacedWorker(ctx, sched, rand.New(rand.NewSource(seed)))
			} else {
				e.worker(ctx, rand.New(rand.NewSource(seed)))
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	ticker := time.NewTicker(protocol.ReportInterval)
	defer ticker.Stop()
	last := start
	for {
		select {
		case now := <-ticker.C:
			report(e.snapshot(start, now, now.Sub(last), false))
			last = now
		case <-done:
			now := time.Now()
			final := e.snapshot(start, now, now.Sub(last), true)
			e.client.CloseIdleConnections()
			if final.ElapsedS > 0 {
				final.Mbps = float64(final.Bytes) * 8 / 1e6 / final.ElapsedS
			}
			e.mu.Lock()
			final.P50Ms, final.P95Ms = percentiles(e.allDurs)
			classes := make(map[string]int64, len(e.errClasses))
			for k, v := range e.errClasses {
				classes[k] = v
			}
			e.mu.Unlock()
			report(final)
			return protocol.TestDone{TestID: e.spec.ID, Summary: final, ErrorsByClass: classes}
		}
	}
}

// dispatch releases one slot per 1/RequestsPerSec seconds. A slot nobody is
// free to take is dropped and counted, which shows up as Skipped.
func (e *Engine) dispatch(ctx context.Context, sched chan<- struct{}) {
	interval := time.Duration(float64(time.Second) / e.spec.RequestsPerSec)
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			select {
			case sched <- struct{}{}:
			default:
				e.skipped.Add(1)
			}
		}
	}
}

// pacedWorker waits for a dispatch slot, fetches once, and repeats.
func (e *Engine) pacedWorker(ctx context.Context, sched <-chan struct{}, rng *rand.Rand) {
	if len(e.spec.URLs) == 0 {
		return
	}
	e.active.Add(1)
	defer e.active.Add(-1)
	for {
		select {
		case <-ctx.Done():
			return
		case <-sched:
			e.fetch(ctx, e.spec.URLs[rng.Intn(len(e.spec.URLs))])
		}
	}
}

func (e *Engine) worker(ctx context.Context, rng *rand.Rand) {
	if len(e.spec.URLs) == 0 {
		return
	}
	e.active.Add(1)
	defer e.active.Add(-1)
	for {
		if ctx.Err() != nil {
			return
		}
		e.fetch(ctx, e.spec.URLs[rng.Intn(len(e.spec.URLs))])
		if ctx.Err() != nil {
			return
		}
		think := e.spec.ThinkMinMs
		if e.spec.ThinkMaxMs > e.spec.ThinkMinMs {
			think += rng.Intn(e.spec.ThinkMaxMs - e.spec.ThinkMinMs + 1)
		}
		if think <= 0 {
			continue
		}
		t := time.NewTimer(time.Duration(think) * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// fetch performs one request. TimeoutMs is a stall timeout: the request is
// abandoned when no bytes have arrived for that long, so a large download on
// a slow link keeps going as long as data flows, while a hung server is cut
// off promptly.
func (e *Engine) fetch(ctx context.Context, target string) {
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := time.Duration(e.spec.TimeoutMs) * time.Millisecond
	var stalled atomic.Bool
	timer := time.AfterFunc(stall, func() {
		stalled.Store(true)
		cancel()
	})
	defer timer.Stop()
	start := time.Now()

	req, err := http.NewRequestWithContext(rctx, http.MethodGet, target, nil)
	if err != nil {
		e.record(start, target, 0, "bad_url", err.Error())
		return
	}
	SetBrowserHeaders(req, e.spec.UserAgent)
	stallMsg := fmt.Sprintf("no bytes for %d ms", e.spec.TimeoutMs)

	resp, err := e.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return // test stopped mid-request; not a failure
		}
		if stalled.Load() {
			e.record(start, target, 0, "timeout", stallMsg)
			return
		}
		e.record(start, target, 0, Classify(err), errMessage(err))
		return
	}
	_, rerr := io.Copy(io.Discard, &countingReader{r: resp.Body, e: e, ctx: rctx, timer: timer, stall: stall})
	resp.Body.Close()
	if ctx.Err() != nil {
		return
	}
	if rerr != nil {
		class := "read"
		msg := errMessage(rerr)
		if stalled.Load() {
			class = "timeout"
			msg = stallMsg
		} else if c := Classify(rerr); c != "other" {
			class = c
		}
		e.record(start, target, resp.StatusCode, class, msg)
		return
	}
	class := ClassifyStatus(resp.StatusCode)
	msg := ""
	if class != "" {
		msg = "HTTP " + strconv.Itoa(resp.StatusCode) + " " + http.StatusText(resp.StatusCode)
		if resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.String() != target {
			msg += " after redirect to " + resp.Request.URL.String()
		}
	}
	e.record(start, target, resp.StatusCode, class, msg)
}

// record counts one finished request. class is empty for success; failures
// also go into the pending error log sent with the next progress report.
func (e *Engine) record(start time.Time, url string, status int, class, msg string) {
	ms := float64(time.Since(start).Microseconds()) / 1000
	e.requests.Add(1)
	if class != "" {
		e.errors.Add(1)
	}
	e.mu.Lock()
	e.winReqs++
	e.winDurs = append(e.winDurs, ms)
	if len(e.allDurs) < maxOverallSamples {
		e.allDurs = append(e.allDurs, ms)
	}
	if class != "" {
		e.errClasses[class]++
		if len(e.pendingErrs) < maxPendingErrors {
			e.pendingErrs = append(e.pendingErrs, protocol.RequestError{
				TS: time.Now().UnixMilli(), URL: url, Class: class, Status: status, Ms: ms, Message: msg,
			})
		}
	}
	e.mu.Unlock()
}

// errMessage strips the "Get \"url\":" wrapper the HTTP client adds, since the
// URL is logged separately, and bounds the length.
func errMessage(err error) string {
	if err == nil {
		return ""
	}
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		err = uerr.Err
	}
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}

func (e *Engine) snapshot(start, now time.Time, interval time.Duration, final bool) protocol.Progress {
	e.mu.Lock()
	reqs := e.winReqs
	durs := e.winDurs
	pending := e.pendingErrs
	e.winReqs = 0
	e.winDurs = nil
	e.pendingErrs = nil
	e.mu.Unlock()
	bytes := e.winBytes.Swap(0)
	secs := interval.Seconds()
	if secs <= 0 {
		secs = 1
	}
	p50, p95 := percentiles(durs)
	return protocol.Progress{
		TestID:           e.spec.ID,
		TS:               now.UnixMilli(),
		ElapsedS:         now.Sub(start).Seconds(),
		Requests:         e.requests.Load(),
		Bytes:            e.bytes.Load(),
		Errors:           e.errors.Load(),
		IntervalMs:       interval.Milliseconds(),
		IntervalRequests: reqs,
		IntervalBytes:    bytes,
		Mbps:             float64(bytes) * 8 / 1e6 / secs,
		ActiveWorkers:    int(e.active.Load()),
		P50Ms:            p50,
		P95Ms:            p95,
		Skipped:          e.skipped.Load(),
		Done:             final,
		NewErrors:        pending,
	}
}

// countingReader adds body bytes to the counters as they arrive, so a large
// download shows up on the throughput chart while it is in flight, pushes
// the stall timer back every time data lands, and holds reads to the
// agent's rate limit when one is set.
type countingReader struct {
	r     io.Reader
	e     *Engine
	ctx   context.Context
	timer *time.Timer
	stall time.Duration
}

func (c *countingReader) Read(p []byte) (int, error) {
	if lim := c.e.limiter; lim != nil && len(p) > lim.Burst() {
		p = p[:lim.Burst()]
	}
	n, err := c.r.Read(p)
	if n > 0 {
		c.e.bytes.Add(int64(n))
		c.e.winBytes.Add(int64(n))
		c.timer.Reset(c.stall)
		if lim := c.e.limiter; lim != nil {
			if werr := lim.WaitN(c.ctx, n); werr != nil && err == nil {
				err = werr
			}
		}
	}
	return n, err
}

// CheckResult is the outcome of fetching one URL once, for list checks.
// Class is empty when the fetch succeeded.
type CheckResult struct {
	URL      string  `json:"url"`
	FinalURL string  `json:"final_url,omitempty"`
	Class    string  `json:"class"`
	Status   int     `json:"status,omitempty"`
	Ms       float64 `json:"ms"`
	Bytes    int64   `json:"bytes"`
	Length   int64   `json:"length"`
	Type     string  `json:"type,omitempty"`
	Message  string  `json:"message,omitempty"`
}

type errEnough struct{}

func (errEnough) Error() string { return "enough" }

// limitedBody stops a body read after maxBytes so large files can be
// checked without downloading them whole.
type limitedBody struct {
	r    io.Reader
	left int64
}

func (l *limitedBody) Read(p []byte) (int, error) {
	if l.left <= 0 {
		return 0, errEnough{}
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}

// CheckURL fetches u once the way a test worker would, with a whole-request
// timeout, reading at most maxBytes of the body (0 for all of it).
func CheckURL(client *http.Client, u string, timeout time.Duration, maxBytes int64) CheckResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return CheckResult{URL: u, Class: "bad_url", Message: err.Error()}
	}
	SetBrowserHeaders(req, DefaultUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return CheckResult{URL: u, Class: Classify(err), Message: errMessage(err), Ms: msSince(start)}
	}
	var body io.Reader = resp.Body
	if maxBytes > 0 {
		body = &limitedBody{r: resp.Body, left: maxBytes}
	}
	n, rerr := io.Copy(io.Discard, body)
	resp.Body.Close()
	if errors.As(rerr, &errEnough{}) {
		rerr = nil
	}
	r := CheckResult{
		URL:      u,
		FinalURL: resp.Request.URL.String(),
		Status:   resp.StatusCode,
		Bytes:    n,
		Length:   resp.ContentLength,
		Type:     resp.Header.Get("Content-Type"),
		Ms:       msSince(start),
	}
	if rerr != nil {
		r.Class = Classify(rerr)
		if r.Class == "other" {
			r.Class = "read"
		}
		r.Message = errMessage(rerr)
		return r
	}
	r.Class = ClassifyStatus(resp.StatusCode)
	if r.Class != "" {
		r.Message = "HTTP " + strconv.Itoa(resp.StatusCode) + " " + http.StatusText(resp.StatusCode)
	}
	return r
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000
}

// ClassifyStatus maps an HTTP status to an error class, or "" for success.
func ClassifyStatus(code int) string {
	switch {
	case code == 403 || code == 429:
		return "blocked"
	case code >= 500:
		return "http_5xx"
	case code >= 400:
		return "http_4xx"
	}
	return ""
}

// Classify maps a transport error to a coarse class for reporting.
func Classify(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return "timeout"
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return "tls"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "tls") || strings.Contains(msg, "x509") || strings.Contains(msg, "certificate"):
		return "tls"
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "no route") || strings.Contains(msg, "network is unreachable"):
		return "connect"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Op == "dial" {
			return "connect"
		}
		return "network"
	}
	return "other"
}

func percentiles(v []float64) (p50, p95 float64) {
	if len(v) == 0 {
		return 0, 0
	}
	s := make([]float64, len(v))
	copy(s, v)
	sort.Float64s(s)
	at := func(q float64) float64 { return s[int(q*float64(len(s)-1))] }
	return at(0.5), at(0.95)
}
