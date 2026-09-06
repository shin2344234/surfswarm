package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"surfswarm/internal/engine"
	"surfswarm/internal/protocol"
	"surfswarm/internal/wifi"
)

// outboxSize bounds frames kept while the server is unreachable: at two
// progress reports a second that is twenty minutes of a running test.
const outboxSize = 2400

// wifiPollInterval is how often the wireless link is re-read when the
// platform tool is quick; slow tools stretch it (see pollWifi).
const wifiPollInterval = 500 * time.Millisecond

// agent owns the connection to the server and the test that may be running.
// A test's lifetime is independent of the connection: the control channel
// usually rides on the same Wi-Fi being loaded, so it is expected to drop
// exactly when results matter most. Reports are queued while disconnected
// and flushed on reconnect.
type agent struct {
	info   protocol.AgentInfo
	server string
	token  string
	out    *outbox

	mu         sync.Mutex
	testID     string
	testCancel context.CancelFunc

	wifiMu sync.Mutex
	wifi   *protocol.WifiInfo
}

func newAgent(cfg config) *agent {
	hostname, _ := os.Hostname()
	info := protocol.AgentInfo{
		ID:       loadOrCreateID(cfg.StateDir),
		Name:     cfg.Name,
		Hostname: hostname,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Version:  version,
		IPs:      localIPs(),
	}
	log.Printf("surfswarm-agent %s: name=%s os=%s/%s id=%s server=%s", version, info.Name, info.OS, info.Arch, info.ID, cfg.Server)
	return &agent{info: info, server: cfg.Server, token: cfg.Token, out: newOutbox(outboxSize)}
}

// runForever keeps a session open to the server, reconnecting with backoff,
// and keeps the Wi-Fi reading fresh.
func (a *agent) runForever(ctx context.Context) {
	go a.pollWifi(ctx)
	backoff := time.Second
	for {
		connected, err := a.session(ctx)
		if ctx.Err() != nil {
			a.stopTest()
			return
		}
		if connected {
			backoff = time.Second
		}
		log.Printf("disconnected: %v; reconnecting in %s (%d frames queued)", err, backoff, a.out.len())
		select {
		case <-ctx.Done():
			a.stopTest()
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// pollWifi refreshes the wireless link reading twice a second when the
// platform tool is quick (iw and netsh are). When it is slow, as
// system_profiler on macOS is, the interval stretches to four times the
// read so the agent is not running it back to back.
func (a *agent) pollWifi(ctx context.Context) {
	warned := false
	for {
		start := time.Now()
		w, err := wifi.Current()
		took := time.Since(start)
		if err != nil && !warned {
			log.Printf("wi-fi telemetry unavailable: %v", err)
			warned = true
		}
		a.wifiMu.Lock()
		a.wifi = w
		a.wifiMu.Unlock()
		wait := wifiPollInterval
		if took > 200*time.Millisecond {
			wait = 4 * took
			if wait > time.Minute {
				wait = time.Minute
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (a *agent) currentWifi() *protocol.WifiInfo {
	a.wifiMu.Lock()
	defer a.wifiMu.Unlock()
	if a.wifi == nil {
		return nil
	}
	w := *a.wifi
	return &w
}

func (a *agent) runningTest() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.testID
}

// session runs one connection until it fails. It reports whether the
// websocket handshake succeeded so the caller can reset its backoff.
func (a *agent) session(ctx context.Context) (bool, error) {
	hdr := http.Header{}
	if a.token != "" {
		hdr.Set("Authorization", "Bearer "+a.token)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, a.server, hdr)
	if err != nil {
		if resp != nil {
			return false, fmt.Errorf("%w (HTTP %d)", err, resp.StatusCode)
		}
		return false, err
	}
	defer conn.Close()
	log.Printf("connected to %s", a.server)

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	hello, _ := protocol.Encode(protocol.TypeHello, protocol.Hello{
		Protocol:    protocol.Version,
		Agent:       a.info,
		Wifi:        a.currentWifi(),
		RunningTest: a.runningTest(),
	})
	if err := write(conn, hello); err != nil {
		return true, err
	}

	writeErr := make(chan error, 1)
	go func() {
		hb := time.NewTicker(5 * time.Second)
		defer hb.Stop()
		flush := func() error {
			for {
				frame, ok := a.out.pop()
				if !ok {
					return nil
				}
				if err := write(conn, frame); err != nil {
					a.out.unpop(frame)
					return err
				}
			}
		}
		if err := flush(); err != nil {
			writeErr <- err
			return
		}
		for {
			select {
			case <-sctx.Done():
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				writeErr <- nil
				return
			case <-a.out.wake:
				if err := flush(); err != nil {
					writeErr <- err
					return
				}
			case <-hb.C:
				msg, _ := protocol.Encode(protocol.TypeHeartbeat, protocol.Heartbeat{TS: time.Now().UnixMilli(), Wifi: a.currentWifi()})
				if err := write(conn, msg); err != nil {
					writeErr <- err
					return
				}
			}
		}
	}()

	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			a.handle(ctx, data)
		}
	}()

	select {
	case err = <-readErr:
	case err = <-writeErr:
	case <-ctx.Done():
		err = ctx.Err()
	}
	return true, err
}

func write(conn *websocket.Conn, msg []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, msg)
}

// handle acts on a frame from the server. Tests are started against the
// agent's root context, not the session's, so they outlive a disconnect.
func (a *agent) handle(ctx context.Context, data []byte) {
	env, err := protocol.Decode(data)
	if err != nil {
		log.Printf("bad frame from server: %v", err)
		return
	}
	switch env.Type {
	case protocol.TypeStartTest:
		var st protocol.StartTest
		if err := json.Unmarshal(env.Data, &st); err != nil {
			log.Printf("bad start_test: %v", err)
			return
		}
		a.startTest(ctx, st.Test)
	case protocol.TypeStopTest:
		var sp protocol.StopTest
		_ = json.Unmarshal(env.Data, &sp)
		log.Printf("test %s: stop requested", sp.TestID)
		a.stopTest()
	case protocol.TypePing:
		msg, _ := protocol.Encode(protocol.TypePong, nil)
		a.out.push(msg)
	default:
		log.Printf("ignoring unknown message type %q", env.Type)
	}
}

func (a *agent) startTest(ctx context.Context, spec protocol.TestSpec) {
	if len(spec.URLs) == 0 {
		log.Printf("test %s: no URLs supplied; ignoring", spec.ID)
		return
	}
	if spec.Mode != "" && spec.Mode != "get" {
		log.Printf("test %s: mode %q not supported by this agent; ignoring", spec.ID, spec.Mode)
		return
	}
	if a.runningTest() == spec.ID {
		log.Printf("test %s: already running; ignoring duplicate start", spec.ID)
		return
	}
	a.stopTest()
	tctx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.testID = spec.ID
	a.testCancel = cancel
	a.mu.Unlock()
	log.Printf("test %s: start mode=%s threads=%d duration=%ds think=%d-%dms timeout=%dms urls=%d",
		spec.ID, spec.Mode, spec.Threads, spec.DurationS, spec.ThinkMinMs, spec.ThinkMaxMs, spec.TimeoutMs, len(spec.URLs))

	go func() {
		defer cancel()
		eng := engine.New(spec)
		done := eng.Run(tctx, func(p protocol.Progress) {
			p.Wifi = a.currentWifi()
			msg, _ := protocol.Encode(protocol.TypeProgress, p)
			a.out.push(msg)
		})
		msg, _ := protocol.Encode(protocol.TypeTestDone, done)
		a.out.push(msg)
		s := done.Summary
		log.Printf("test %s: done requests=%d bytes=%d errors=%d avg=%.2f Mbps p50=%.0fms p95=%.0fms classes=%v",
			spec.ID, s.Requests, s.Bytes, s.Errors, s.Mbps, s.P50Ms, s.P95Ms, done.ErrorsByClass)
		a.mu.Lock()
		if a.testID == spec.ID {
			a.testID = ""
			a.testCancel = nil
		}
		a.mu.Unlock()
	}()
}

func (a *agent) stopTest() {
	a.mu.Lock()
	cancel := a.testCancel
	a.testCancel = nil
	a.testID = ""
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// outbox is a bounded queue of frames waiting for a connection. When full,
// the oldest frame is dropped so a long outage cannot exhaust memory.
type outbox struct {
	mu      sync.Mutex
	items   [][]byte
	max     int
	dropped int
	wake    chan struct{}
}

func newOutbox(max int) *outbox {
	return &outbox{max: max, wake: make(chan struct{}, 1)}
}

func (o *outbox) push(b []byte) {
	o.mu.Lock()
	if len(o.items) >= o.max {
		o.items = o.items[1:]
		o.dropped++
	}
	o.items = append(o.items, b)
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *outbox) pop() ([]byte, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.items) == 0 {
		return nil, false
	}
	b := o.items[0]
	o.items = o.items[1:]
	return b, true
}

// unpop puts a frame back at the front after a failed write.
func (o *outbox) unpop(b []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.items = append([][]byte{b}, o.items...)
}

func (o *outbox) len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.items)
}
