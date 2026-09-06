// Package server is the surfswarm control server: it accepts agent
// connections, runs tests across them, and serves the web UI.
package server

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/shin2344234/surfswarm/internal/protocol"
)

//go:embed static/index.html
var indexHTML []byte

//go:embed static/lists.html
var listsHTML []byte

//go:embed data/browse_urls.txt
var browseURLList string

//go:embed data/download_urls.txt
var downloadURLList string

//go:embed data/max_urls.txt
var maxURLList string

// faviconSVG is a small swarm mark so browser tabs are identifiable.
const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="6" fill="#0f1419"/><circle cx="10" cy="11" r="3.2" fill="#3fb950"/><circle cx="21" cy="9" r="2.6" fill="#3fb950"/><circle cx="16" cy="19" r="3.6" fill="#3fb950"/><circle cx="24" cy="22" r="2.4" fill="#58a6ff"/><circle cx="8" cy="23" r="2.4" fill="#58a6ff"/></svg>`

// Config holds server settings.
type Config struct {
	// Token, when set, must be presented by agents as a bearer token.
	Token string
	// DataDir is where edited and custom URL lists are saved. Empty keeps
	// edits in memory only.
	DataDir string
	// UIPassword, when set, protects the pages and API with HTTP basic
	// auth (any user name). The agent endpoint is covered by Token instead.
	UIPassword string
}

// Server holds all state for one process.
type Server struct {
	cfg       Config
	hub       *Hub
	tests     *TestManager
	store     *ListStore
	subs      *subscribers
	upgrader  websocket.Upgrader
	uiSession string
}

// New builds a server with the bundled URL lists plus any saved edits.
func New(cfg Config) (*Server, error) {
	dir := ""
	if cfg.DataDir != "" {
		dir = filepath.Join(cfg.DataDir, "lists")
	}
	store, err := NewListStore(dir, map[string]string{
		"browse":   browseURLList,
		"download": downloadURLList,
		"max":      maxURLList,
	})
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:   cfg,
		store: store,
		subs:  newSubscribers(),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
	s.hub = NewHub(func() { s.subs.Broadcast("agents", s.hub.List()) })
	s.tests = NewTestManager(s.hub, s.subs, store)
	s.hub.OnPresence(s.tests.OnPresence)
	s.uiSession = randomToken()
	return s, nil
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// requireUIAuth guards everything except the agent endpoint with basic
// auth when a UI password is set. After one successful login the browser
// gets a session cookie, which also covers the websocket handshake.
func (s *Server) requireUIAuth(next http.Handler) http.Handler {
	if s.cfg.UIPassword == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent" {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie("surfswarm_ui"); err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.uiSession)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if _, pw, ok := r.BasicAuth(); ok && subtle.ConstantTimeCompare([]byte(pw), []byte(s.cfg.UIPassword)) == 1 {
			http.SetCookie(w, &http.Cookie{Name: "surfswarm_ui", Value: s.uiSession, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="surfswarm"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// ListSizes reports how many URLs each list holds.
func (s *Server) ListSizes() map[string]int {
	out := map[string]int{}
	for _, li := range s.store.Info() {
		out[li.Name] = li.Count
	}
	return out
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	page := func(body []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(body)
		}
	}
	mux.HandleFunc("GET /{$}", page(indexHTML))
	mux.HandleFunc("GET /lists", page(listsHTML))
	favicon := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write([]byte(faviconSVG))
	}
	mux.HandleFunc("GET /favicon.ico", favicon)
	mux.HandleFunc("GET /favicon.svg", favicon)
	mux.HandleFunc("GET /agent", s.handleAgent)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/agents", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.hub.List())
	})
	mux.HandleFunc("GET /api/urls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.store.URLMap())
	})

	// URL lists.
	mux.HandleFunc("GET /api/lists", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.store.Info())
	})
	mux.HandleFunc("GET /api/lists/{name}", func(w http.ResponseWriter, r *http.Request) {
		text, info, ok := s.store.Text(r.PathValue("name"))
		if !ok {
			writeError(w, http.StatusNotFound, "list not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"info": info, "text": text})
	})
	mux.HandleFunc("PUT /api/lists/{name}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		info, problems, err := s.store.Save(r.PathValue("name"), body.Text)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(problems) > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the list was not saved", "problems": problems})
			return
		}
		log.Printf("list %s saved: %d URLs", info.Name, info.Count)
		writeJSON(w, http.StatusOK, info)
	})
	mux.HandleFunc("DELETE /api/lists/{name}", func(w http.ResponseWriter, r *http.Request) {
		info, existed, err := s.store.Reset(r.PathValue("name"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !existed {
			writeError(w, http.StatusNotFound, "list not found")
			return
		}
		log.Printf("list %s reset or removed", r.PathValue("name"))
		writeJSON(w, http.StatusOK, info)
	})
	mux.HandleFunc("POST /api/lists/{name}/check", func(w http.ResponseWriter, r *http.Request) {
		job, ok := s.store.StartCheck(r.PathValue("name"))
		if !ok {
			writeError(w, http.StatusNotFound, "list not found")
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	})
	mux.HandleFunc("GET /api/lists/{name}/check", func(w http.ResponseWriter, r *http.Request) {
		job, ok := s.store.Check(r.PathValue("name"))
		if !ok {
			writeError(w, http.StatusNotFound, "no check has run for this list")
			return
		}
		writeJSON(w, http.StatusOK, job)
	})

	// Tests.
	mux.HandleFunc("GET /api/tests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.tests.List())
	})
	mux.HandleFunc("POST /api/tests", s.handleCreateTest)
	mux.HandleFunc("GET /api/tests/{id}", func(w http.ResponseWriter, r *http.Request) {
		t, ok := s.tests.Snapshot(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, ErrTestNotFound.Error())
			return
		}
		writeJSON(w, http.StatusOK, t)
	})
	mux.HandleFunc("GET /api/tests/{id}/errors", func(w http.ResponseWriter, r *http.Request) {
		errs, ok := s.tests.Errors(r.PathValue("id"), r.URL.Query().Get("agent"))
		if !ok {
			writeError(w, http.StatusNotFound, ErrTestNotFound.Error())
			return
		}
		writeJSON(w, http.StatusOK, errs)
	})
	mux.HandleFunc("POST /api/tests/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := s.tests.Stop(id); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		t, _ := s.tests.Snapshot(id)
		writeJSON(w, http.StatusOK, t)
	})
	return s.requireUIAuth(mux)
}

func (s *Server) handleCreateTest(w http.ResponseWriter, r *http.Request) {
	var req CreateTestRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	t, err := s.tests.Start(req)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrTestRunning) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

// handleAgent is the websocket endpoint agents connect to.
func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Token != "" {
		if r.Header.Get("Authorization") != "Bearer "+s.cfg.Token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("agent upgrade from %s failed: %v", r.RemoteAddr, err)
		return
	}

	// First frame must be hello.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return
	}
	env, err := protocol.Decode(data)
	if err != nil || env.Type != protocol.TypeHello {
		log.Printf("agent from %s did not send hello first", r.RemoteAddr)
		conn.Close()
		return
	}
	var hello protocol.Hello
	if err := json.Unmarshal(env.Data, &hello); err != nil || hello.Agent.ID == "" {
		log.Printf("agent from %s sent a bad hello", r.RemoteAddr)
		conn.Close()
		return
	}
	if hello.Protocol != protocol.Version {
		log.Printf("agent %s speaks protocol %d, server speaks %d", hello.Agent.Name, hello.Protocol, protocol.Version)
	}

	ac := s.hub.Register(hello.Agent, conn, r.RemoteAddr)
	defer s.hub.Unregister(ac)
	s.hub.SetWifi(ac, hello.Wifi)
	log.Printf("agent %s connected from %s (%s/%s, %s)", hello.Agent.Name, r.RemoteAddr, hello.Agent.OS, hello.Agent.Arch, hello.Agent.Version)
	if hello.RunningTest != "" {
		log.Printf("agent %s is still running test %s; expecting queued reports", hello.Agent.Name, hello.RunningTest)
	}

	for {
		// Agents heartbeat every 5 s; four missed beats means it is gone.
		_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			log.Printf("agent %s disconnected: %v", hello.Agent.Name, err)
			return
		}
		s.hub.Touch(ac)
		env, err := protocol.Decode(data)
		if err != nil {
			log.Printf("agent %s sent a bad frame: %v", hello.Agent.Name, err)
			continue
		}
		switch env.Type {
		case protocol.TypePong:
		case protocol.TypeHeartbeat:
			var hb protocol.Heartbeat
			if err := json.Unmarshal(env.Data, &hb); err == nil {
				s.hub.SetWifi(ac, hb.Wifi)
			}
		case protocol.TypeProgress:
			var p protocol.Progress
			if err := json.Unmarshal(env.Data, &p); err == nil {
				s.hub.SetWifi(ac, p.Wifi)
				s.tests.OnProgress(hello.Agent.ID, p)
			}
		case protocol.TypeTestDone:
			var d protocol.TestDone
			if err := json.Unmarshal(env.Data, &d); err == nil {
				s.tests.OnDone(hello.Agent.ID, d)
			}
		default:
			log.Printf("agent %s sent unknown type %q", hello.Agent.Name, env.Type)
		}
	}
}

type snapshotEvent struct {
	Agents []AgentStatus `json:"agents"`
	Test   *Test         `json:"test"`
}

// handleEvents streams agent presence and test progress to the UI.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := newUIConn(conn)
	s.subs.add(c)
	defer s.subs.remove(c)

	snap := snapshotEvent{Agents: s.hub.List()}
	if t, ok := s.tests.Current(); ok {
		snap.Test = &t
	}
	if msg, err := protocol.Encode("snapshot", snap); err == nil {
		c.send <- msg
	}

	// Read loop exists only to notice when the tab goes away.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func parseURLList(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}
