package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Hub implements the minimal SignalR JSON-protocol server the SyncClipboard
// clients require (negotiate + WebSockets transport + keepalive pings +
// server->client broadcasts).
//
// Concurrency model: the send channel c.ch is NEVER closed (only c.done is,
// exactly once) so a straggling producer can never hit "send on closed
// channel". Both per-connection goroutines select on c.done and exit.
type Hub struct {
	mu      sync.Mutex
	conns   map[*hubConn]struct{}
	verbose bool
}

type hubConn struct {
	ws   *websocket.Conn
	ch   chan []byte
	done chan struct{}
	once sync.Once
	hub  *Hub
}

func newHub(verbose bool) *Hub {
	return &Hub{conns: make(map[*hubConn]struct{}), verbose: verbose}
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b) // 22 chars, like .NET connection ids
}

type negotiateTransport struct {
	Transport       string   `json:"transport"`
	TransferFormats []string `json:"transferFormats"`
}

type negotiateResponse struct {
	NegotiateVersion    int                  `json:"negotiateVersion"`
	ConnectionID        string               `json:"connectionId"`
	ConnectionToken     string               `json:"connectionToken"`
	AvailableTransports []negotiateTransport `json:"availableTransports"`
}

func (h *Hub) handleNegotiate(w http.ResponseWriter, r *http.Request) {
	resp := negotiateResponse{
		NegotiateVersion: 1,
		ConnectionID:     randomID(),
		ConnectionToken:  randomID(),
		AvailableTransports: []negotiateTransport{
			{Transport: "WebSockets", TransferFormats: []string{"Text", "Binary"}},
			{Transport: "ServerSentEvents", TransferFormats: []string{"Text"}},
			{Transport: "LongPolling", TransferFormats: []string{"Text", "Binary"}},
		},
	}
	b, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (h *Hub) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}

	// SignalR handshake: first frame must carry the json protocol handshake.
	hctx, hcancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, data, err := ws.Read(hctx)
	hcancel()
	if err != nil {
		_ = ws.Close(websocket.StatusPolicyViolation, "no handshake")
		return
	}
	hs := strings.TrimRight(string(data), "\x1e")
	if !strings.Contains(hs, `"protocol"`) || !strings.Contains(hs, `"json"`) {
		_ = ws.Close(websocket.StatusPolicyViolation, "bad handshake")
		return
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = ws.Write(wctx, websocket.MessageText, []byte("{}\x1e"))
	wcancel()
	if err != nil {
		_ = ws.Close(websocket.StatusInternalError, "handshake write failed")
		return
	}

	c := &hubConn{ws: ws, ch: make(chan []byte, 64), done: make(chan struct{}), hub: h}
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	if h.verbose {
		log.Printf("[hub] connection established (total %d)", h.len())
	}

	// writer goroutine: drains c.ch until the connection finishes
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[hub] writer panic recovered: %v", rec)
			}
		}()
		for {
			select {
			case <-c.done:
				return
			case b := <-c.ch:
				wc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				err := c.ws.Write(wc, websocket.MessageText, b)
				cancel()
				if err != nil {
					_ = c.ws.Close(websocket.StatusAbnormalClosure, "write error")
					return
				}
			}
		}
	}()

	// server keepalive pings (SignalR type 6) every 10s
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[hub] keepalive panic recovered: %v", rec)
			}
		}()
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-t.C:
				c.trySend([]byte("{\"type\":6}\x1e"))
			}
		}
	}()

	defer func() {
		c.finish()
		h.mu.Lock()
		delete(h.conns, c)
		h.mu.Unlock()
		_ = ws.Close(websocket.StatusNormalClosure, "")
		if h.verbose {
			log.Printf("[hub] connection closed (total %d)", h.len())
		}
	}()

	// read loop (client pings every 15s; timeout guards dead connections)
	for {
		rc, cancel := context.WithTimeout(context.Background(), 75*time.Second)
		_, data, err := ws.Read(rc)
		cancel()
		if err != nil {
			return
		}
		s := strings.TrimRight(string(data), "\x1e")
		if strings.Contains(s, `"type":7`) { // close message
			return
		}
		// type 6 (ping) and anything else from the client is ignored
	}
}

func (h *Hub) len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// trySend enqueues without blocking; never panics even after finish().
func (c *hubConn) trySend(b []byte) {
	select {
	case c.ch <- b:
	case <-c.done:
	default: // slow consumer: drop frame
	}
}

// finish signals both per-connection goroutines to exit; safe to call twice.
func (c *hubConn) finish() {
	c.once.Do(func() { close(c.done) })
}

// Broadcast sends a SignalR invocation frame to every connected client.
func (h *Hub) Broadcast(target string, arg any) {
	frame, err := marshalJSON(struct {
		Type      int    `json:"type"`
		Target    string `json:"target"`
		Arguments []any  `json:"arguments"`
	}{Type: 1, Target: target, Arguments: []any{arg}})
	if err != nil {
		return
	}
	frame = append(frame, 0x1e)

	h.mu.Lock()
	conns := make([]*hubConn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	for _, c := range conns {
		c.trySend(frame)
	}
}
