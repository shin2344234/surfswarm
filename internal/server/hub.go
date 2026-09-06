package server

import (
	"log"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/shin2344234/surfswarm/internal/protocol"
)

// AgentConn is one live websocket to an agent. Writes go through a single
// goroutine because gorilla connections allow only one concurrent writer.
type AgentConn struct {
	Info       protocol.AgentInfo
	RemoteAddr string
	conn       *websocket.Conn
	send       chan []byte
	done       chan struct{}
	closeOnce  sync.Once
}

// Send queues a frame. It returns false if the connection is gone or the
// queue is full; a full queue means the agent is not keeping up and the frame
// is dropped rather than blocking the caller.
func (ac *AgentConn) Send(msg []byte) bool {
	select {
	case <-ac.done:
		return false
	default:
	}
	select {
	case ac.send <- msg:
		return true
	case <-ac.done:
		return false
	default:
		log.Printf("agent %s: send queue full, dropping frame", ac.Info.Name)
		return false
	}
}

// Close tears down the socket. Safe to call more than once.
func (ac *AgentConn) Close() {
	ac.closeOnce.Do(func() {
		close(ac.done)
		ac.conn.Close()
	})
}

func (ac *AgentConn) writeLoop() {
	for {
		select {
		case <-ac.done:
			return
		case msg := <-ac.send:
			_ = ac.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := ac.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				log.Printf("agent %s: write failed: %v", ac.Info.Name, err)
				ac.Close()
				return
			}
		}
	}
}

// AgentStatus is the API and UI view of an agent, online or not.
type AgentStatus struct {
	protocol.AgentInfo
	RemoteAddr  string             `json:"remote_addr"`
	Online      bool               `json:"online"`
	ConnectedAt time.Time          `json:"connected_at"`
	LastSeen    time.Time          `json:"last_seen"`
	Wifi        *protocol.WifiInfo `json:"wifi,omitempty"`
}

type agentState struct {
	status AgentStatus
	conn   *AgentConn
}

// Hub tracks every agent that has connected since the server started.
// Agents that disconnect stay listed as offline. Persistence comes later.
type Hub struct {
	mu       sync.RWMutex
	agents   map[string]*agentState
	onChange func()
	presence func(id, name string, online bool)
}

// NewHub returns an empty hub. onChange runs after any presence change.
func NewHub(onChange func()) *Hub {
	return &Hub{agents: map[string]*agentState{}, onChange: onChange}
}

// OnPresence registers a callback for individual connect and disconnect
// events, used to note agent outages during a test.
func (h *Hub) OnPresence(fn func(id, name string, online bool)) {
	h.presence = fn
}

// SetWifi records the latest wireless reading from an agent.
func (h *Hub) SetWifi(ac *AgentConn, w *protocol.WifiInfo) {
	h.mu.Lock()
	if st, ok := h.agents[ac.Info.ID]; ok && st.conn == ac {
		st.status.Wifi = w
	}
	h.mu.Unlock()
}

// Register records a new connection. If the same agent id is already
// connected, the older socket is closed.
func (h *Hub) Register(info protocol.AgentInfo, conn *websocket.Conn, remote string) *AgentConn {
	ac := &AgentConn{
		Info:       info,
		RemoteAddr: remote,
		conn:       conn,
		send:       make(chan []byte, 64),
		done:       make(chan struct{}),
	}
	go ac.writeLoop()

	now := time.Now()
	h.mu.Lock()
	var old *AgentConn
	if st, ok := h.agents[info.ID]; ok && st.conn != nil {
		old = st.conn
	}
	h.agents[info.ID] = &agentState{
		status: AgentStatus{AgentInfo: info, RemoteAddr: remote, Online: true, ConnectedAt: now, LastSeen: now},
		conn:   ac,
	}
	h.mu.Unlock()

	if old != nil {
		log.Printf("agent %s reconnected from %s; closing previous connection", info.Name, remote)
		old.Close()
	}
	if h.presence != nil {
		h.presence(info.ID, info.Name, true)
	}
	h.onChange()
	return ac
}

// Unregister marks the agent offline if ac is still its current connection.
func (h *Hub) Unregister(ac *AgentConn) {
	h.mu.Lock()
	changed := false
	if st, ok := h.agents[ac.Info.ID]; ok && st.conn == ac {
		st.conn = nil
		st.status.Online = false
		st.status.LastSeen = time.Now()
		changed = true
	}
	h.mu.Unlock()
	ac.Close()
	if changed {
		if h.presence != nil {
			h.presence(ac.Info.ID, ac.Info.Name, false)
		}
		h.onChange()
	}
}

// Touch updates last-seen for a connection that just sent a frame.
func (h *Hub) Touch(ac *AgentConn) {
	h.mu.Lock()
	if st, ok := h.agents[ac.Info.ID]; ok && st.conn == ac {
		st.status.LastSeen = time.Now()
	}
	h.mu.Unlock()
}

// Send delivers a frame to an online agent.
func (h *Hub) Send(agentID string, msg []byte) bool {
	h.mu.RLock()
	var ac *AgentConn
	if st, ok := h.agents[agentID]; ok {
		ac = st.conn
	}
	h.mu.RUnlock()
	if ac == nil {
		return false
	}
	return ac.Send(msg)
}

// Get returns the status of one agent.
func (h *Hub) Get(agentID string) (AgentStatus, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	st, ok := h.agents[agentID]
	if !ok {
		return AgentStatus{}, false
	}
	return st.status, true
}

// List returns every known agent sorted by name.
func (h *Hub) List() []AgentStatus {
	h.mu.RLock()
	out := make([]AgentStatus, 0, len(h.agents))
	for _, st := range h.agents {
		out = append(out, st.status)
	}
	h.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// OnlineIDs returns the ids of agents currently connected, sorted by name.
func (h *Hub) OnlineIDs() []string {
	var ids []string
	for _, st := range h.List() {
		if st.Online {
			ids = append(ids, st.ID)
		}
	}
	return ids
}
