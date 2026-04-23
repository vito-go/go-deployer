package controlplane

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vito-go/go-deployer/protocol"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// AgentConn represents a connected agent instance.
type AgentConn struct {
	ServiceName string
	Group       string
	BinaryDir   string
	RemoteAddr  string
	CountryCode string
	Host        string
	PID         int
	Port        uint
	Version      string
	CommitHash   string
	CommitTime   string
	GoVersion    string
	StartTimeMs  int64
	ExePath      string
	AppArgs      string
	CPUPercent   float64
	MemRSS       uint64
	MemUsed      uint64
	MemTotal     uint64
	NumGoroutine int
	conn        *websocket.Conn
	mu          sync.Mutex
	// pendingRequests stores channels waiting for responses keyed by request ID
	pendingRequests map[string]chan protocol.Message
	pendingMu       sync.Mutex
}

func (ac *AgentConn) sendJSON(msg protocol.Message) error {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.conn.WriteJSON(msg)
}

// SendCommand sends a command to the agent and returns immediately.
func (ac *AgentConn) SendCommand(msgType string, id string, data interface{}) error {
	raw, _ := json.Marshal(data)
	return ac.sendJSON(protocol.Message{Type: msgType, ID: id, Data: raw})
}

// SendRequest sends a command and waits for a response with the same ID (up to timeout).
func (ac *AgentConn) SendRequest(msgType string, id string, data interface{}, timeout time.Duration) (*protocol.Message, error) {
	ch := make(chan protocol.Message, 1)
	ac.pendingMu.Lock()
	ac.pendingRequests[id] = ch
	ac.pendingMu.Unlock()

	defer func() {
		ac.pendingMu.Lock()
		delete(ac.pendingRequests, id)
		ac.pendingMu.Unlock()
	}()

	if err := ac.SendCommand(msgType, id, data); err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		return &resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for response")
	}
}

func (ac *AgentConn) deliverResponse(msg protocol.Message) bool {
	ac.pendingMu.Lock()
	ch, ok := ac.pendingRequests[msg.ID]
	ac.pendingMu.Unlock()
	if ok {
		select {
		case ch <- msg:
		default:
		}
		return true
	}
	return false
}

type logSub struct {
	ch        chan string
	createdAt time.Time
}

// Hub manages all agent WebSocket connections.
type Hub struct {
	mu     sync.RWMutex
	agents map[string]*AgentConn // key: "serviceName/host/pid"
	token  string

	// logSubscribers: requestID -> logSub for streaming logs to SSE clients
	logMu   sync.RWMutex
	logSubs map[string]*logSub

	// ptySubs: sessionID -> browser WebSocket for PTY output forwarding
	ptyMu   sync.RWMutex
	ptySubs map[string]*websocket.Conn

	// resourceSubs: serviceName -> list of notification channels
	resMu   sync.RWMutex
	resSubs map[string][]chan struct{}
	resCount map[string]int // subscriber count per service
}

// NewHub creates a new Hub.
func NewHub(token string) *Hub {
	h := &Hub{
		agents:  make(map[string]*AgentConn),
		token:   token,
		logSubs:  make(map[string]*logSub),
		ptySubs:  make(map[string]*websocket.Conn),
		resSubs:  make(map[string][]chan struct{}),
		resCount: make(map[string]int),
	}
	go h.cleanupOrphanedSubs()
	return h
}

// cleanupOrphanedSubs removes log subscriptions older than 5 minutes.
func (h *Hub) cleanupOrphanedSubs() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		h.logMu.Lock()
		for id, sub := range h.logSubs {
			if time.Since(sub.createdAt) > 5*time.Minute {
				close(sub.ch)
				delete(h.logSubs, id)
			}
		}
		h.logMu.Unlock()
	}
}

func agentKey(serviceName, host string, pid int) string {
	return fmt.Sprintf("%s/%s/%d", serviceName, host, pid)
}

// Services returns a map of serviceName -> list of agent connections.
func (h *Hub) Services() map[string][]*AgentConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	services := make(map[string][]*AgentConn)
	for _, ac := range h.agents {
		services[ac.ServiceName] = append(services[ac.ServiceName], ac)
	}
	return services
}

// AgentsByService returns all agents for a given service name.
func (h *Hub) AgentsByService(serviceName string) []*AgentConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var result []*AgentConn
	for _, ac := range h.agents {
		if ac.ServiceName == serviceName {
			result = append(result, ac)
		}
	}
	return result
}

// GetAgent returns a specific agent by service/host/pid.
func (h *Hub) GetAgent(serviceName, host string, pid int) *AgentConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.agents[agentKey(serviceName, host, pid)]
}

// SubscribeLogs creates or returns an existing log subscription for a request ID.
func (h *Hub) SubscribeLogs(requestID string) chan string {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	if sub, ok := h.logSubs[requestID]; ok {
		return sub.ch
	}
	ch := make(chan string, 100)
	h.logSubs[requestID] = &logSub{ch: ch, createdAt: time.Now()}
	return ch
}

// UnsubscribeLogs removes a log subscription.
func (h *Hub) UnsubscribeLogs(requestID string) {
	h.logMu.Lock()
	if sub, ok := h.logSubs[requestID]; ok {
		close(sub.ch)
		delete(h.logSubs, requestID)
	}
	h.logMu.Unlock()
}

func (h *Hub) publishLog(requestID, line string) {
	h.logMu.RLock()
	sub, ok := h.logSubs[requestID]
	h.logMu.RUnlock()
	if ok {
		select {
		case sub.ch <- line:
		default:
		}
	}
}

// SubscribeResources creates an SSE subscription for resource updates of a service.
// When the first subscriber connects, tells agents to start reporting resources.
func (h *Hub) SubscribeResources(serviceName string) chan struct{} {
	ch := make(chan struct{}, 1)
	h.resMu.Lock()
	h.resSubs[serviceName] = append(h.resSubs[serviceName], ch)
	h.resCount[serviceName]++
	firstSub := h.resCount[serviceName] == 1
	h.resMu.Unlock()

	if firstSub {
		// Tell all agents of this service to start resource reporting
		for _, ac := range h.AgentsByService(serviceName) {
			_ = ac.SendCommand(protocol.TypeResourceStart, "", nil)
		}
	}
	return ch
}

// UnsubscribeResources removes an SSE resource subscription.
// When the last subscriber disconnects, tells agents to stop reporting resources.
func (h *Hub) UnsubscribeResources(serviceName string, ch chan struct{}) {
	h.resMu.Lock()
	subs := h.resSubs[serviceName]
	for i, s := range subs {
		if s == ch {
			h.resSubs[serviceName] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	h.resCount[serviceName]--
	lastSub := h.resCount[serviceName] <= 0
	if lastSub {
		h.resCount[serviceName] = 0
	}
	close(ch)
	h.resMu.Unlock()

	if lastSub {
		for _, ac := range h.AgentsByService(serviceName) {
			_ = ac.SendCommand(protocol.TypeResourceStop, "", nil)
		}
	}
}

func (h *Hub) notifyResourceSubs(serviceName string) {
	h.resMu.RLock()
	subs := h.resSubs[serviceName]
	h.resMu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default: // non-blocking, skip if subscriber hasn't consumed the last notification
		}
	}
}

// SubscribePty registers a browser WebSocket to receive PTY output for a session.
func (h *Hub) SubscribePty(sessionID string, conn *websocket.Conn) {
	h.ptyMu.Lock()
	h.ptySubs[sessionID] = conn
	h.ptyMu.Unlock()
}

// UnsubscribePty removes a PTY subscription.
func (h *Hub) UnsubscribePty(sessionID string) {
	h.ptyMu.Lock()
	delete(h.ptySubs, sessionID)
	h.ptyMu.Unlock()
}

func (h *Hub) forwardPtyToClient(msg protocol.Message) {
	h.ptyMu.RLock()
	conn, ok := h.ptySubs[msg.ID]
	h.ptyMu.RUnlock()
	if !ok {
		log.Printf("[pty] drop %s for unknown session=%s (no browser subscriber)", msg.Type, msg.ID)
		return
	}

	// Forward the message directly to browser WebSocket
	if msg.Type == protocol.TypePtyClose {
		log.Printf("[pty] forward close session=%s", msg.ID)
		_ = conn.WriteJSON(map[string]string{"type": "close"})
		return
	}

	var data protocol.PtyOutputData
	_ = json.Unmarshal(msg.Data, &data)
	_ = conn.WriteJSON(map[string]string{"type": "output", "data": data.Data})
}

// forwardPtyLogToClient surfaces agent-side log lines (errors from pty.Start,
// etc.) on the terminal WebSocket so the user sees them in xterm instead of
// staring at a blank window.
func (h *Hub) forwardPtyLogToClient(sessionID, line string) {
	h.ptyMu.RLock()
	conn, ok := h.ptySubs[sessionID]
	h.ptyMu.RUnlock()
	if !ok {
		log.Printf("[pty] drop log for unknown session=%s: %s", sessionID, line)
		return
	}
	// Prefix so it's clearly a control-plane/agent message, not shell output.
	_ = conn.WriteJSON(map[string]string{
		"type": "output",
		"data": "\r\n\x1b[31m[agent] " + line + "\x1b[0m\r\n",
	})
}

func cfOrRemote(r *http.Request, fallback string) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	return fallback
}

// HandleWebSocket upgrades HTTP to WebSocket and manages the agent connection.
func (h *Hub) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Auth check
	if h.token != "" {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+h.token {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	// Wait for register message
	var firstMsg protocol.Message
	if err := conn.ReadJSON(&firstMsg); err != nil || firstMsg.Type != protocol.TypeRegister {
		conn.Close()
		return
	}

	var reg protocol.RegisterData
	_ = json.Unmarshal(firstMsg.Data, &reg)

	ac := &AgentConn{
		ServiceName:     reg.ServiceName,
		Group:           reg.Group,
		BinaryDir:       reg.BinaryDir,
		RemoteAddr:      cfOrRemote(r, conn.RemoteAddr().String()),
		CountryCode:     r.Header.Get("CF-IPCountry"),
		Host:            reg.Host,
		PID:             reg.PID,
		Port:            reg.Port,
		Version:         reg.Version,
		CommitHash:      reg.CommitHash,
		CommitTime:      reg.CommitTime,
		GoVersion:       reg.GoVersion,
		StartTimeMs:     reg.StartTimeMs,
		ExePath:         reg.ExePath,
		AppArgs:         reg.AppArgs,
		conn:            conn,
		pendingRequests: make(map[string]chan protocol.Message),
	}

	key := agentKey(reg.ServiceName, reg.Host, reg.PID)
	h.mu.Lock()
	h.agents[key] = ac
	h.mu.Unlock()

	// Keepalive ping every 30s to prevent Cloudflare idle timeout (default 100s)
	pingStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingStop:
				return
			case <-ticker.C:
				ac.mu.Lock()
				_ = conn.WriteMessage(websocket.PingMessage, nil)
				ac.mu.Unlock()
			}
		}
	}()

	defer func() {
		close(pingStop)
		h.mu.Lock()
		if h.agents[key] == ac {
			delete(h.agents, key)
		}
		h.mu.Unlock()
		conn.Close()
	}()

	// Read loop
	for {
		var msg protocol.Message
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}

		switch msg.Type {
		case protocol.TypeLog:
			var logData protocol.LogData
			_ = json.Unmarshal(msg.Data, &logData)
			// PTY session logs (e.g. "ERR: Failed to start PTY: ...") have
			// session IDs like "pty-<nanos>". They aren't deploy logs — they
			// must be surfaced on the terminal WebSocket, otherwise errors
			// from pty.Start are silently dropped and the user sees a blank
			// terminal.
			if strings.HasPrefix(msg.ID, "pty-") {
				log.Printf("[pty] agent log session=%s host=%s pid=%d: %s",
					msg.ID, ac.Host, ac.PID, logData.Line)
				h.forwardPtyLogToClient(msg.ID, logData.Line)
				continue
			}
			h.publishLog(msg.ID, logData.Line)

		case protocol.TypeResourceData:
			var rd protocol.ResourceData
			_ = json.Unmarshal(msg.Data, &rd)
			ac.mu.Lock()
			ac.CPUPercent = rd.CPUPercent
			ac.MemRSS = rd.MemRSS
			ac.MemUsed = rd.MemUsed
			ac.MemTotal = rd.MemTotal
			ac.NumGoroutine = rd.NumGoroutine
			ac.mu.Unlock()
			h.notifyResourceSubs(ac.ServiceName)

		case protocol.TypePtyOutput, protocol.TypePtyClose:
			h.forwardPtyToClient(msg)

		case protocol.TypeFiles, protocol.TypeComplete:
			ac.deliverResponse(msg)

		default:
			log.Printf("[hub] unknown msg type=%q id=%s from host=%s pid=%d (agent may be stale)",
				msg.Type, msg.ID, ac.Host, ac.PID)
		}
	}
}
