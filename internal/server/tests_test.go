package server

import (
	"testing"
	"time"

	"github.com/shin2344234/surfswarm/internal/protocol"
)

func TestNoteWifiEvents(t *testing.T) {
	ar := &AgentResult{}
	noteWifi(ar, &protocol.WifiInfo{BSSID: "aa", Band: "5", Channel: 44, SignalDBm: -50}, 1)
	if len(ar.Events) != 0 {
		t.Fatalf("baseline should not log: %+v", ar.Events)
	}
	noteWifi(ar, &protocol.WifiInfo{BSSID: "aa", SignalDBm: -60}, 2) // same BSSID, signal change only
	noteWifi(ar, &protocol.WifiInfo{BSSID: "bb", Band: "5", Channel: 149, SignalDBm: -70}, 3)
	noteWifi(ar, nil, 4)
	noteWifi(ar, &protocol.WifiInfo{BSSID: "bb"}, 5)
	noteWifi(ar, &protocol.WifiInfo{}, 6) // unknown BSSID must not count as a roam
	var kinds []string
	for _, e := range ar.Events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"roam", "wifi_lost", "wifi_back"}
	if len(kinds) != len(want) {
		t.Fatalf("events %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("events %v, want %v", kinds, want)
		}
	}
	if ar.Events[0].From == "" || ar.Events[0].To == "" || ar.Events[0].TS != 3 {
		t.Fatalf("roam event: %+v", ar.Events[0])
	}
}

func newTestWithAgents(names ...string) (*TestManager, *Test) {
	tm := NewTestManager(NewHub(func() {}), newSubscribers(), nil)
	t := &Test{
		ID:           "t1",
		Status:       StatusRunning,
		StartedAt:    time.Now(),
		Agents:       map[string]*AgentResult{},
		History:      []Tick{},
		AgentHistory: map[string][]protocol.Progress{},
		AgentErrors:  map[string][]protocol.RequestError{},
	}
	for _, n := range names {
		t.Agents[n] = &AgentResult{AgentID: n, Name: n}
	}
	tm.tests[t.ID] = t
	tm.current = t
	return tm, t
}

func TestOnProgressHistoryAndErrorLog(t *testing.T) {
	tm, tt := newTestWithAgents("a1")
	tm.OnProgress("a1", protocol.Progress{TestID: "t1", TS: 1000, Requests: 2, Bytes: 100,
		NewErrors: []protocol.RequestError{{TS: 900, URL: "https://x/", Class: "blocked", Status: 403}}})
	tm.OnProgress("a1", protocol.Progress{TestID: "t1", TS: 2000, Requests: 4, Bytes: 300})
	tm.OnProgress("a1", protocol.Progress{TestID: "t1", TS: 1500, Requests: 3}) // out of order: ignored for history
	tm.OnProgress("a1", protocol.Progress{TestID: "t1", TS: 3000, Requests: 5, Done: true, Mbps: 9})
	tm.OnProgress("zz", protocol.Progress{TestID: "t1", TS: 3000}) // unknown agent: ignored
	tm.OnProgress("a1", protocol.Progress{TestID: "nope", TS: 3000})

	ar := tt.Agents["a1"]
	if len(tt.AgentHistory["a1"]) != 2 {
		t.Fatalf("history: %d points, want 2 (final report and out-of-order excluded)", len(tt.AgentHistory["a1"]))
	}
	if len(tt.AgentErrors["a1"]) != 1 || tt.AgentErrors["a1"][0].Status != 403 {
		t.Fatalf("error log: %+v", tt.AgentErrors["a1"])
	}
	if !ar.Done || ar.Latest.Mbps != 9 || ar.Latest.NewErrors != nil {
		t.Fatalf("latest: %+v", ar.Latest)
	}
}

func TestComputeTick(t *testing.T) {
	_, tt := newTestWithAgents("fast", "slow", "finished", "stale")
	now := time.Now()
	recent := now.Add(-time.Second).UnixMilli()
	tt.Agents["fast"].Latest = protocol.Progress{TS: recent, Requests: 50, Bytes: 5000, Errors: 1, Mbps: 10, IntervalRequests: 6, P50Ms: 100, P95Ms: 200, ActiveWorkers: 4}
	tt.Agents["slow"].Latest = protocol.Progress{TS: recent, Requests: 20, Bytes: 1000, Errors: 0, Mbps: 2, IntervalRequests: 2, P50Ms: 300, P95Ms: 600, ActiveWorkers: 4}
	tt.Agents["finished"].Latest = protocol.Progress{TS: recent, Requests: 30, Bytes: 3000, Errors: 2, Mbps: 7, Done: true}
	tt.Agents["finished"].Done = true
	tt.Agents["stale"].Latest = protocol.Progress{TS: now.Add(-10 * time.Second).UnixMilli(), Requests: 5, Bytes: 500, Mbps: 99, IntervalRequests: 9, ActiveWorkers: 4}
	tt.History = []Tick{{TS: now.Add(-time.Second).UnixMilli(), Errors: 1}}

	tick, allDone := computeTick(tt, now)
	if allDone {
		t.Fatal("not all done")
	}
	if tick.Requests != 105 || tick.Bytes != 9500 || tick.Errors != 3 {
		t.Fatalf("totals: %+v", tick)
	}
	if tick.Mbps != 12 || tick.ReqPerSec != 8 || tick.Workers != 8 || tick.ActiveAgents != 2 {
		t.Fatalf("rates should exclude finished and stale agents: %+v", tick)
	}
	// Weighted by interval requests: (100*6 + 300*2) / 8 = 150; (200*6 + 600*2) / 8 = 300.
	if tick.P50Ms != 150 || tick.P95Ms != 300 {
		t.Fatalf("latency: %+v", tick)
	}
	if tick.ErrPerSec < 1.9 || tick.ErrPerSec > 2.1 {
		t.Fatalf("err/s from delta 1 -> 3 over ~1s: %v", tick.ErrPerSec)
	}

	// Half-second intervals are normalized to per-second rates.
	tt.Agents["fast"].Latest.IntervalMs = 500
	tt.Agents["slow"].Latest.IntervalMs = 500
	if tick, _ := computeTick(tt, now); tick.ReqPerSec != 16 {
		t.Fatalf("req/s with 500 ms intervals: %v", tick.ReqPerSec)
	}

	for _, ar := range tt.Agents {
		ar.Done = true
	}
	if _, allDone := computeTick(tt, now); !allDone {
		t.Fatal("all done expected")
	}
}

func TestOnPresenceLogsEventDuringTest(t *testing.T) {
	tm, tt := newTestWithAgents("a1")
	tm.OnPresence("a1", "a1", false)
	tm.OnPresence("a1", "a1", true)
	tm.OnPresence("other", "other", false) // not in the test
	ev := tt.Agents["a1"].Events
	if len(ev) != 2 || ev[0].Kind != "offline" || ev[1].Kind != "online" {
		t.Fatalf("events: %+v", ev)
	}
	tt.Status = StatusDone
	tm.OnPresence("a1", "a1", false)
	if len(tt.Agents["a1"].Events) != 2 {
		t.Fatal("no events after the test is done")
	}
}
