// Package protocol defines the JSON messages exchanged between agents and the
// server over a single websocket. Every frame is an Envelope; Data holds the
// type-specific payload.
package protocol

import "encoding/json"

// Version is bumped whenever a message shape changes incompatibly.
const Version = 1

// Agent to server.
const (
	TypeHello     = "hello"
	TypeHeartbeat = "heartbeat"
	TypeProgress  = "progress"
	TypeTestDone  = "test_done"
	TypePong      = "pong"
)

// Server to agent.
const (
	TypeStartTest = "start_test"
	TypeStopTest  = "stop_test"
	TypePing      = "ping"
)

// Envelope wraps every message on the wire.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Encode builds a wire frame for the given type and payload.
func Encode(typ string, data any) ([]byte, error) {
	env := Envelope{Type: typ}
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		env.Data = b
	}
	return json.Marshal(env)
}

// Decode parses a wire frame. The caller unmarshals Data based on Type.
func Decode(b []byte) (Envelope, error) {
	var env Envelope
	err := json.Unmarshal(b, &env)
	return env, err
}

// AgentInfo identifies an agent. ID is generated once per install and persisted.
type AgentInfo struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Hostname string   `json:"hostname"`
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	Version  string   `json:"version"`
	IPs      []string `json:"ips"`
}

// WifiInfo describes the wireless link an agent is on, when it has one.
// Signal is in dBm (negative); on Windows it is converted from a percentage.
type WifiInfo struct {
	Interface string  `json:"interface,omitempty"`
	SSID      string  `json:"ssid,omitempty"`
	BSSID     string  `json:"bssid,omitempty"`
	Band      string  `json:"band,omitempty"` // "2.4", "5", or "6"
	Channel   int     `json:"channel,omitempty"`
	WidthMHz  int     `json:"width_mhz,omitempty"`
	FreqMHz   int     `json:"freq_mhz,omitempty"`
	SignalDBm int     `json:"signal_dbm,omitempty"`
	NoiseDBm  int     `json:"noise_dbm,omitempty"`
	TxMbps    float64 `json:"tx_mbps,omitempty"`
	RxMbps    float64 `json:"rx_mbps,omitempty"`
	PHY       string  `json:"phy,omitempty"`
}

// Hello is the first frame an agent sends after connecting. RunningTest is
// set when the agent reconnects with a test still in progress.
type Hello struct {
	Protocol    int       `json:"protocol"`
	Agent       AgentInfo `json:"agent"`
	Wifi        *WifiInfo `json:"wifi,omitempty"`
	RunningTest string    `json:"running_test,omitempty"`
}

// Heartbeat is sent every few seconds while idle or busy.
type Heartbeat struct {
	TS   int64     `json:"ts"`
	Wifi *WifiInfo `json:"wifi,omitempty"`
}

// TestSpec is everything an agent needs to run a test.
type TestSpec struct {
	ID         string   `json:"id"`
	Mode       string   `json:"mode"` // "get" in the spike; "page" and "bulk" later
	Threads    int      `json:"threads"`
	DurationS  int      `json:"duration_s"` // 0 means run until stopped
	ThinkMinMs int      `json:"think_min_ms"`
	ThinkMaxMs int      `json:"think_max_ms"`
	TimeoutMs  int      `json:"timeout_ms"`
	UserAgent  string   `json:"user_agent,omitempty"`
	URLs       []string `json:"urls,omitempty"`
}

// StartTest tells an agent to begin a test.
type StartTest struct {
	Test TestSpec `json:"test"`
}

// StopTest tells an agent to stop a running test early.
type StopTest struct {
	TestID string `json:"test_id"`
}

// RequestError describes one failed request, for the per-device error log.
type RequestError struct {
	TS      int64   `json:"ts"`
	URL     string  `json:"url"`
	Class   string  `json:"class"`
	Status  int     `json:"status,omitempty"`
	Ms      float64 `json:"ms"`
	Message string  `json:"message,omitempty"`
}

// Progress is sent once per second while a test runs, and once more with
// Done set when it finishes. Requests, Bytes and Errors are cumulative;
// the Interval fields and Mbps cover the last reporting interval only.
// NewErrors lists the failures recorded since the previous report.
type Progress struct {
	TestID           string  `json:"test_id"`
	TS               int64   `json:"ts"`
	ElapsedS         float64 `json:"elapsed_s"`
	Requests         int64   `json:"requests"`
	Bytes            int64   `json:"bytes"`
	Errors           int64   `json:"errors"`
	IntervalRequests int64   `json:"interval_requests"`
	IntervalBytes    int64   `json:"interval_bytes"`
	Mbps             float64 `json:"mbps"`
	ActiveWorkers    int     `json:"active_workers"`
	P50Ms            float64 `json:"p50_ms"`
	P95Ms            float64 `json:"p95_ms"`
	Done             bool    `json:"done"`

	NewErrors []RequestError `json:"new_errors,omitempty"`
	Wifi      *WifiInfo      `json:"wifi,omitempty"`
}

// TestDone carries the final summary for a test. Summary.Mbps is the average
// over the whole run and its percentiles cover every request.
type TestDone struct {
	TestID        string           `json:"test_id"`
	Summary       Progress         `json:"summary"`
	ErrorsByClass map[string]int64 `json:"errors_by_class"`
}
