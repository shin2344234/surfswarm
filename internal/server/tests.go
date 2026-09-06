package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/shin2344234/surfswarm/internal/protocol"
)

const (
	StatusRunning  = "running"
	StatusStopping = "stopping"
	StatusDone     = "done"
)

// ErrTestRunning is returned when a test is started while another is active.
var ErrTestRunning = errors.New("a test is already running")

// ErrTestNotFound is returned for unknown test ids.
var ErrTestNotFound = errors.New("test not found")

// CreateTestRequest is the API body for POST /api/tests. Empty AgentIDs
// means every online agent. URLList is "browse" (default), "download", or
// "mixed". Zero Threads, DurationS or TimeoutMs take defaults; think times
// are used as given.
type CreateTestRequest struct {
	AgentIDs       []string `json:"agent_ids"`
	Mode           string   `json:"mode"`
	URLList        string   `json:"url_list"`
	Threads        int      `json:"threads"`
	DurationS      int      `json:"duration_s"`
	ThinkMinMs     int      `json:"think_min_ms"`
	ThinkMaxMs     int      `json:"think_max_ms"`
	TimeoutMs      int      `json:"timeout_ms"`
	RequestsPerSec float64  `json:"requests_per_sec"`
	RateMbps       float64  `json:"rate_mbps"`
}

// Tick is one report interval of aggregate results across all agents in a
// test. Requests, Bytes and Errors are cumulative. Mbps and ReqPerSec sum
// what each active agent reported for its last interval, normalized to
// per-second rates; ErrPerSec comes from the change in cumulative errors
// since the previous tick; P50Ms and P95Ms are the agents' interval
// percentiles weighted by their request counts.
type Tick struct {
	TS           int64   `json:"ts"`
	Mbps         float64 `json:"mbps"`
	ReqPerSec    float64 `json:"req_per_sec"`
	ErrPerSec    float64 `json:"err_per_sec"`
	P50Ms        float64 `json:"p50_ms"`
	P95Ms        float64 `json:"p95_ms"`
	Workers      int     `json:"workers"`
	Requests     int64   `json:"requests"`
	Bytes        int64   `json:"bytes"`
	Errors       int64   `json:"errors"`
	ActiveAgents int     `json:"active_agents"`
}

// maxHistory caps the report history kept in memory, per test and per
// agent: two hours at two reports a second.
const maxHistory = 14400

// AgentEvent notes something that happened to one agent during a test: a
// roam to another BSSID, Wi-Fi dropping or returning, or the control
// connection to the server going away and coming back.
type AgentEvent struct {
	TS     int64  `json:"ts"`
	Kind   string `json:"kind"` // roam, wifi_lost, wifi_back, offline, online
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// maxEvents caps the events kept per agent per test.
const maxEvents = 200

// AgentResult is the latest word from one agent in a test.
type AgentResult struct {
	AgentID       string             `json:"agent_id"`
	Name          string             `json:"name"`
	Latest        protocol.Progress  `json:"latest"`
	Done          bool               `json:"done"`
	ErrorsByClass map[string]int64   `json:"errors_by_class,omitempty"`
	Wifi          *protocol.WifiInfo `json:"wifi,omitempty"`
	Events        []AgentEvent       `json:"events,omitempty"`

	seenWifi bool
}

// Test is a run across one or more agents.
type Test struct {
	ID        string                  `json:"id"`
	Spec      protocol.TestSpec       `json:"spec"`
	URLList   string                  `json:"url_list"`
	URLCount  int                     `json:"url_count"`
	AgentIDs  []string                `json:"agent_ids"`
	Status    string                  `json:"status"`
	StartedAt time.Time               `json:"started_at"`
	EndedAt   *time.Time              `json:"ended_at,omitempty"`
	Agents    map[string]*AgentResult `json:"agents"`
	History   []Tick                  `json:"history"`
	// AgentHistory holds each agent's per-second reports, keyed by agent id,
	// so the UI can chart one device on its own.
	AgentHistory map[string][]protocol.Progress `json:"agent_history"`
	// AgentErrors is each agent's log of failed requests, keyed by agent id,
	// newest last, capped at maxErrorLog entries.
	AgentErrors map[string][]protocol.RequestError `json:"agent_errors"`
	Totals      Tick                               `json:"totals"`

	stopRequested time.Time
}

// maxErrorLog caps the failed-request log kept per agent per test.
const maxErrorLog = 2000

type tickEvent struct {
	TestID string `json:"test_id"`
	Tick   Tick   `json:"tick"`
}

type progressEvent struct {
	TestID  string      `json:"test_id"`
	AgentID string      `json:"agent_id"`
	Result  AgentResult `json:"result"`
}

// TestManager owns test state. The spike keeps everything in memory and
// allows one active test at a time.
type TestManager struct {
	hub   *Hub
	subs  *subscribers
	store *ListStore

	mu      sync.Mutex
	tests   map[string]*Test
	order   []string
	current *Test
}

// NewTestManager wires a manager to the hub, UI subscribers, and URL lists.
func NewTestManager(hub *Hub, subs *subscribers, store *ListStore) *TestManager {
	return &TestManager{hub: hub, subs: subs, store: store, tests: map[string]*Test{}}
}

// resolveList picks the URL list for a test: "browse" (default), any list
// by name, or "mixed" for browse and download together. Lists are read
// fresh each time so edits apply to the next test.
func (tm *TestManager) resolveList(name string) (string, []string, error) {
	if name == "" {
		name = "browse"
	}
	var urls []string
	switch name {
	case "mixed":
		browse, _ := tm.store.URLs("browse")
		download, _ := tm.store.URLs("download")
		urls = append(append(urls, browse...), download...)
	default:
		list, ok := tm.store.URLs(name)
		if !ok {
			return "", nil, fmt.Errorf("unknown url_list %q; see /api/lists for the names", name)
		}
		urls = append(urls, list...)
	}
	if len(urls) == 0 {
		return "", nil, fmt.Errorf("url list %q is empty", name)
	}
	return name, urls, nil
}

// Start validates the request, sends start_test to each agent, and begins
// aggregating results.
func (tm *TestManager) Start(req CreateTestRequest) (Test, error) {
	tm.mu.Lock()
	if tm.current != nil && tm.current.Status != StatusDone {
		tm.mu.Unlock()
		return Test{}, ErrTestRunning
	}
	tm.mu.Unlock()

	if req.Mode == "" {
		req.Mode = "get"
	}
	if req.Mode != "get" {
		return Test{}, fmt.Errorf("mode %q is not supported yet (only \"get\")", req.Mode)
	}
	listName, urls, err := tm.resolveList(req.URLList)
	if err != nil {
		return Test{}, err
	}

	ids := req.AgentIDs
	if len(ids) == 0 {
		ids = tm.hub.OnlineIDs()
	}
	if len(ids) == 0 {
		return Test{}, errors.New("no online agents")
	}
	agents := make(map[string]*AgentResult, len(ids))
	for _, id := range ids {
		st, ok := tm.hub.Get(id)
		if !ok {
			return Test{}, fmt.Errorf("unknown agent %q", id)
		}
		if !st.Online {
			return Test{}, fmt.Errorf("agent %q is offline", st.Name)
		}
		agents[id] = &AgentResult{AgentID: id, Name: st.Name}
	}

	if req.RequestsPerSec < 0 || req.RateMbps < 0 {
		return Test{}, errors.New("requests_per_sec and rate_mbps must be zero or positive")
	}
	spec := protocol.TestSpec{
		ID:             newID(),
		Mode:           req.Mode,
		Threads:        req.Threads,
		DurationS:      req.DurationS,
		ThinkMinMs:     req.ThinkMinMs,
		ThinkMaxMs:     req.ThinkMaxMs,
		TimeoutMs:      req.TimeoutMs,
		RequestsPerSec: req.RequestsPerSec,
		RateMbps:       req.RateMbps,
	}
	if spec.Threads <= 0 {
		spec.Threads = 4
	}
	if spec.DurationS <= 0 {
		spec.DurationS = 60
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

	t := &Test{
		ID:           spec.ID,
		Spec:         spec,
		URLList:      listName,
		URLCount:     len(urls),
		AgentIDs:     ids,
		Status:       StatusRunning,
		StartedAt:    time.Now(),
		Agents:       agents,
		History:      []Tick{},
		AgentHistory: make(map[string][]protocol.Progress, len(agents)),
		AgentErrors:  make(map[string][]protocol.RequestError, len(agents)),
	}

	wire := spec
	wire.URLs = urls
	msg, err := protocol.Encode(protocol.TypeStartTest, protocol.StartTest{Test: wire})
	if err != nil {
		return Test{}, err
	}
	for _, id := range ids {
		if !tm.hub.Send(id, msg) {
			log.Printf("test %s: could not send start to agent %s", t.ID, agents[id].Name)
			agents[id].Done = true
		}
	}

	tm.mu.Lock()
	tm.tests[t.ID] = t
	tm.order = append(tm.order, t.ID)
	tm.current = t
	tm.mu.Unlock()

	log.Printf("test %s: started on %d agent(s), threads=%d duration=%ds think=%d-%dms rps=%g rate=%gMbps list=%s urls=%d",
		t.ID, len(ids), spec.Threads, spec.DurationS, spec.ThinkMinMs, spec.ThinkMaxMs, spec.RequestsPerSec, spec.RateMbps, listName, len(urls))
	go tm.tickLoop(t)

	snap, _ := tm.Snapshot(t.ID)
	tm.subs.Broadcast("test", snap)
	return snap, nil
}

// Stop asks every agent in the test to stop. The test is marked done when
// they all report back, or after a short grace period.
func (tm *TestManager) Stop(id string) error {
	tm.mu.Lock()
	t, ok := tm.tests[id]
	if !ok {
		tm.mu.Unlock()
		return ErrTestNotFound
	}
	if t.Status == StatusDone {
		tm.mu.Unlock()
		return nil
	}
	t.Status = StatusStopping
	t.stopRequested = time.Now()
	ids := append([]string(nil), t.AgentIDs...)
	tm.mu.Unlock()

	msg, _ := protocol.Encode(protocol.TypeStopTest, protocol.StopTest{TestID: id})
	for _, aid := range ids {
		tm.hub.Send(aid, msg)
	}
	log.Printf("test %s: stop requested", id)
	snap, _ := tm.Snapshot(id)
	tm.subs.Broadcast("test", snap)
	return nil
}

// OnProgress records a per-second update from an agent.
func (tm *TestManager) OnProgress(agentID string, p protocol.Progress) {
	tm.mu.Lock()
	t, ok := tm.tests[p.TestID]
	if !ok {
		tm.mu.Unlock()
		return
	}
	ar, ok := t.Agents[agentID]
	if !ok {
		tm.mu.Unlock()
		return
	}
	newErrs := p.NewErrors
	if len(newErrs) > 0 {
		entries := append(t.AgentErrors[agentID], newErrs...)
		if len(entries) > maxErrorLog {
			entries = entries[len(entries)-maxErrorLog:]
		}
		t.AgentErrors[agentID] = entries
	}
	noteWifi(ar, p.Wifi, p.TS)
	// The stored copy drops the error batch: it lives in AgentErrors now.
	p.NewErrors = nil
	ar.Latest = p
	if p.Done {
		// The final report carries whole-run averages, not a one-second
		// rate, so it goes into Latest but not the per-second history.
		ar.Done = true
	} else {
		h := t.AgentHistory[agentID]
		if len(h) == 0 || p.TS > h[len(h)-1].TS {
			h = append(h, p)
			if len(h) > maxHistory {
				h = h[len(h)-maxHistory:]
			}
			t.AgentHistory[agentID] = h
		}
	}
	cp := *ar
	cp.Latest.NewErrors = newErrs // the live event still carries this batch
	tm.mu.Unlock()
	tm.subs.Broadcast("progress", progressEvent{TestID: p.TestID, AgentID: agentID, Result: cp})
}

// noteWifi records the latest wireless reading for an agent and logs a
// roam or a lost or restored link as an event.
func noteWifi(ar *AgentResult, w *protocol.WifiInfo, ts int64) {
	prev := ar.Wifi
	switch {
	case !ar.seenWifi:
		// First report sets the baseline without an event.
	case prev != nil && w == nil:
		addEvent(ar, AgentEvent{TS: ts, Kind: "wifi_lost", From: describeWifi(prev)})
	case prev == nil && w != nil:
		addEvent(ar, AgentEvent{TS: ts, Kind: "wifi_back", To: describeWifi(w)})
	case prev != nil && w != nil && prev.BSSID != "" && w.BSSID != "" && prev.BSSID != w.BSSID:
		addEvent(ar, AgentEvent{TS: ts, Kind: "roam", From: describeWifi(prev), To: describeWifi(w)})
	}
	ar.seenWifi = true
	ar.Wifi = w
}

func describeWifi(w *protocol.WifiInfo) string {
	if w == nil {
		return ""
	}
	s := w.BSSID
	if w.Band != "" {
		s += fmt.Sprintf(" (%s GHz ch %d)", w.Band, w.Channel)
	}
	if w.SignalDBm != 0 {
		s += fmt.Sprintf(" %d dBm", w.SignalDBm)
	}
	return s
}

func addEvent(ar *AgentResult, ev AgentEvent) {
	ar.Events = append(ar.Events, ev)
	if len(ar.Events) > maxEvents {
		ar.Events = ar.Events[len(ar.Events)-maxEvents:]
	}
}

// OnPresence notes an agent's control connection dropping or returning
// while a test that includes it is running.
func (tm *TestManager) OnPresence(agentID, name string, online bool) {
	tm.mu.Lock()
	t := tm.current
	if t == nil || t.Status == StatusDone {
		tm.mu.Unlock()
		return
	}
	ar, ok := t.Agents[agentID]
	if !ok || ar.Done {
		tm.mu.Unlock()
		return
	}
	kind := "offline"
	if online {
		kind = "online"
	}
	addEvent(ar, AgentEvent{TS: time.Now().UnixMilli(), Kind: kind, Detail: "control connection"})
	cp := *ar
	id := t.ID
	tm.mu.Unlock()
	log.Printf("test %s: agent %s %s", id, name, kind)
	tm.subs.Broadcast("progress", progressEvent{TestID: id, AgentID: agentID, Result: cp})
}

// Errors returns one agent's failed-request log for a test, or every
// agent's when agentID is empty.
func (tm *TestManager) Errors(testID, agentID string) (map[string][]protocol.RequestError, bool) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	t, ok := tm.tests[testID]
	if !ok {
		return nil, false
	}
	out := map[string][]protocol.RequestError{}
	for id, entries := range t.AgentErrors {
		if agentID != "" && id != agentID {
			continue
		}
		out[id] = append([]protocol.RequestError(nil), entries...)
	}
	return out, true
}

// OnDone records an agent's final summary.
func (tm *TestManager) OnDone(agentID string, d protocol.TestDone) {
	tm.mu.Lock()
	t, ok := tm.tests[d.TestID]
	if !ok {
		tm.mu.Unlock()
		return
	}
	ar, ok := t.Agents[agentID]
	if !ok {
		tm.mu.Unlock()
		return
	}
	ar.Latest = d.Summary
	ar.Latest.Done = true
	ar.Latest.NewErrors = nil
	ar.Done = true
	ar.ErrorsByClass = d.ErrorsByClass
	cp := *ar
	tm.mu.Unlock()
	tm.subs.Broadcast("progress", progressEvent{TestID: d.TestID, AgentID: agentID, Result: cp})
}

// computeTick folds the agents' latest reports into one aggregate tick and
// reports whether every agent has finished. Agents that have not reported
// in the last three seconds count toward totals but not toward rates.
func computeTick(t *Test, now time.Time) (Tick, bool) {
	tick := Tick{TS: now.UnixMilli()}
	allDone := true
	var weightedReqs int64
	var p50Sum, p95Sum float64
	for _, ar := range t.Agents {
		tick.Requests += ar.Latest.Requests
		tick.Bytes += ar.Latest.Bytes
		tick.Errors += ar.Latest.Errors
		if ar.Done {
			continue
		}
		allDone = false
		if now.UnixMilli()-ar.Latest.TS <= 3000 {
			secs := float64(ar.Latest.IntervalMs) / 1000
			if secs <= 0 {
				secs = 1 // older agents reported once a second without saying so
			}
			tick.Mbps += ar.Latest.Mbps
			tick.ReqPerSec += float64(ar.Latest.IntervalRequests) / secs
			tick.Workers += ar.Latest.ActiveWorkers
			tick.ActiveAgents++
			if n := ar.Latest.IntervalRequests; n > 0 {
				weightedReqs += n
				p50Sum += ar.Latest.P50Ms * float64(n)
				p95Sum += ar.Latest.P95Ms * float64(n)
			}
		}
	}
	if weightedReqs > 0 {
		tick.P50Ms = p50Sum / float64(weightedReqs)
		tick.P95Ms = p95Sum / float64(weightedReqs)
	}
	if n := len(t.History); n > 0 {
		prev := t.History[n-1]
		if dt := float64(tick.TS-prev.TS) / 1000; dt > 0 && tick.Errors >= prev.Errors {
			tick.ErrPerSec = float64(tick.Errors-prev.Errors) / dt
		}
	}
	return tick, allDone
}

// tickLoop aggregates every report interval until the test finishes.
func (tm *TestManager) tickLoop(t *Test) {
	ticker := time.NewTicker(protocol.ReportInterval)
	defer ticker.Stop()
	for now := range ticker.C {
		tm.mu.Lock()
		if t.Status == StatusDone {
			tm.mu.Unlock()
			return
		}
		tick, allDone := computeTick(t, now)
		t.History = append(t.History, tick)
		if len(t.History) > maxHistory {
			t.History = t.History[len(t.History)-maxHistory:]
		}
		t.Totals = tick

		finished := allDone
		// Agents keep a test running through a lost connection and flush
		// their reports when they are back, so wait a while for stragglers
		// before closing the test.
		if t.Spec.DurationS > 0 && now.After(t.StartedAt.Add(time.Duration(t.Spec.DurationS)*time.Second+90*time.Second)) {
			finished = true
		}
		if t.Status == StatusStopping && now.After(t.stopRequested.Add(10*time.Second)) {
			finished = true
		}
		if finished {
			t.Status = StatusDone
			end := now
			t.EndedAt = &end
		}
		id := t.ID
		tm.mu.Unlock()

		tm.subs.Broadcast("tick", tickEvent{TestID: id, Tick: tick})
		if finished {
			log.Printf("test %s: finished requests=%d bytes=%d errors=%d", id, tick.Requests, tick.Bytes, tick.Errors)
			snap, _ := tm.Snapshot(id)
			tm.subs.Broadcast("test", snap)
			return
		}
	}
}

// Snapshot returns a deep copy safe to serialize outside the lock.
func (tm *TestManager) Snapshot(id string) (Test, bool) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	t, ok := tm.tests[id]
	if !ok {
		return Test{}, false
	}
	return copyTest(t), true
}

// Current returns the most recently started test, if any.
func (tm *TestManager) Current() (Test, bool) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.current == nil {
		return Test{}, false
	}
	return copyTest(tm.current), true
}

// List returns every test in start order.
func (tm *TestManager) List() []Test {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	out := make([]Test, 0, len(tm.order))
	for _, id := range tm.order {
		out = append(out, copyTest(tm.tests[id]))
	}
	return out
}

func copyTest(t *Test) Test {
	cp := *t
	cp.AgentIDs = append([]string(nil), t.AgentIDs...)
	cp.History = append([]Tick(nil), t.History...)
	cp.Agents = make(map[string]*AgentResult, len(t.Agents))
	for k, v := range t.Agents {
		ar := *v
		cp.Agents[k] = &ar
	}
	cp.AgentHistory = make(map[string][]protocol.Progress, len(t.AgentHistory))
	for k, v := range t.AgentHistory {
		cp.AgentHistory[k] = append([]protocol.Progress(nil), v...)
	}
	cp.AgentErrors = make(map[string][]protocol.RequestError, len(t.AgentErrors))
	for k, v := range t.AgentErrors {
		cp.AgentErrors[k] = append([]protocol.RequestError(nil), v...)
	}
	return cp
}

func newID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
