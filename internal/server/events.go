package server

import (
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"surfswarm/internal/protocol"
)

// uiConn is one browser tab subscribed to live events.
type uiConn struct {
	conn      *websocket.Conn
	send      chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

func newUIConn(conn *websocket.Conn) *uiConn {
	c := &uiConn{conn: conn, send: make(chan []byte, 256), done: make(chan struct{})}
	go c.writeLoop()
	return c
}

func (c *uiConn) writeLoop() {
	for {
		select {
		case <-c.done:
			return
		case msg := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				c.close()
				return
			}
		}
	}
}

func (c *uiConn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.conn.Close()
	})
}

// subscribers fans events out to every connected UI.
type subscribers struct {
	mu    sync.Mutex
	conns map[*uiConn]struct{}
}

func newSubscribers() *subscribers {
	return &subscribers{conns: map[*uiConn]struct{}{}}
}

func (s *subscribers) add(c *uiConn) {
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
}

func (s *subscribers) remove(c *uiConn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	c.close()
}

// Broadcast sends one event to every UI. A tab that cannot keep up has the
// frame dropped; the next snapshot or tick catches it up.
func (s *subscribers) Broadcast(typ string, data any) {
	msg, err := protocol.Encode(typ, data)
	if err != nil {
		log.Printf("broadcast %s: %v", typ, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		select {
		case c.send <- msg:
		default:
		}
	}
}
