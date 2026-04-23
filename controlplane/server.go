package controlplane

import (
	"cmp"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vito-go/go-deployer/protocol"
)

//go:embed dashboard.html terminal.html
var dashboardFS embed.FS

// Server is the control plane HTTP server.
type Server struct {
	hub           *Hub
	addr          string
	username      string
	password      string
	guestUser     string
	guestPassword string
	certInfo      *CertInfo
	cdnDir        string
	cdnSalt       string // random salt for CDN download URL hashing, changes on restart
}

// NewServer creates a new control plane server.
func NewServer(addr, token, username, password, guestUser, guestPassword, cdnDir string) *Server {
	salt := make([]byte, 16)
	rand.Read(salt)
	return &Server{
		hub:           NewHub(token),
		addr:          addr,
		username:      username,
		password:      password,
		guestUser:     guestUser,
		guestPassword: guestPassword,
		cdnDir:        cdnDir,
		cdnSalt:       hex.EncodeToString(salt),
	}
}

// StartWithAutoTLS generates a self-signed certificate, starts HTTPS, and returns the
// certificate fingerprint for agent cert pinning. Certificate files are persisted to
// certPath/keyPath so the fingerprint stays stable across restarts.
func (s *Server) StartWithAutoTLS(certPath, keyPath string) (certFP string, err error) {
	info, err := LoadOrGenerateCert(certPath, keyPath)
	if err != nil {
		return "", fmt.Errorf("auto TLS: %w", err)
	}
	s.certInfo = info
	s.startCDNCleanup()
	log.Printf("Certificate fingerprint: %s", info.FP)

	mux := http.NewServeMux()
	s.mountRoutes(mux)

	srv := &http.Server{
		Addr:    s.addr,
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{info.TLSCert},
		},
	}

	return info.FP, srv.ListenAndServeTLS("", "")
}


func (s *Server) checkAuth(r *http.Request) (isAdmin, isGuest bool) {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false, false
	}
	if s.username != "" && user == s.username && pass == s.password {
		return true, false
	}
	if s.guestUser != "" && user == s.guestUser && pass == s.guestPassword {
		return false, true
	}
	return false, false
}

// basicAuth allows both admin and guest users (read-only access).
func (s *Server) basicAuth(next http.HandlerFunc) http.HandlerFunc {
	if s.username == "" && s.guestUser == "" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		isAdmin, isGuest := s.checkAuth(r)
		if !isAdmin && !isGuest {
			w.Header().Set("WWW-Authenticate", `Basic realm="Deployer Control Plane"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// adminAuth allows only admin users (deploy, restart, kill, delete, etc).
func (s *Server) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	if s.username == "" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		isAdmin, _ := s.checkAuth(r)
		if !isAdmin {
			http.Error(w, "Forbidden: admin only", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) mountRoutes(mux *http.ServeMux) {
	// WebSocket endpoint for agents (uses token auth, not basic auth)
	mux.HandleFunc("GET /ws", s.hub.HandleWebSocket)

	// Role check
	mux.HandleFunc("GET /api/role", s.basicAuth(func(w http.ResponseWriter, r *http.Request) {
		isAdmin, _ := s.checkAuth(r)
		role := "guest"
		if isAdmin {
			role = "admin"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"role": role})
	}))

	// API endpoints — read (admin + guest)
	mux.HandleFunc("GET /api/services", s.basicAuth(s.apiServices))
	mux.HandleFunc("GET /api/services/{name}", s.basicAuth(s.apiServiceDetail))
	mux.HandleFunc("GET /api/services/{name}/files", s.basicAuth(s.apiFiles))
	mux.HandleFunc("GET /api/services/{name}/complete", s.basicAuth(s.apiComplete))
	mux.HandleFunc("GET /api/services/{name}/resources", s.basicAuth(s.apiResources))
	mux.HandleFunc("GET /api/services/{name}/logs", s.basicAuth(s.apiLogs))

	// API endpoints — write (admin only)
	mux.HandleFunc("POST /api/services/{name}/deploy", s.adminAuth(s.apiDeploy))
	mux.HandleFunc("POST /api/services/{name}/restart", s.adminAuth(s.apiRestart))
	mux.HandleFunc("POST /api/services/{name}/kill", s.adminAuth(s.apiKill))
	mux.HandleFunc("POST /api/services/{name}/rollback", s.adminAuth(s.apiRollback))
	mux.HandleFunc("POST /api/services/{name}/exec", s.adminAuth(s.apiExec))
	mux.HandleFunc("POST /api/batch/deploy", s.adminAuth(s.apiBatchDeploy))

	// PTY WebSocket proxy (admin only)
	mux.HandleFunc("GET /ws/pty", s.handlePtyProxy)

	// CDN — list (admin + guest), delete (admin only), download by MD5 (public)
	mux.HandleFunc("GET /api/cdn/list", s.apiCDNList)
	mux.HandleFunc("DELETE /api/cdn/file", s.adminAuth(s.apiCDNDelete))
	mux.HandleFunc("GET /cdn/d/{dir}/{hash}", s.apiCDNDownload)
	mux.HandleFunc("GET /cdn/d/{dir}/{hash}/{filename}", s.apiCDNDownload)

	// Dashboard & Terminal
	mux.HandleFunc("GET /terminal", s.basicAuth(s.terminalPage))
	mux.HandleFunc("GET /", s.basicAuth(s.dashboard))
}

// --- Dashboard ---

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	data, err := dashboardFS.ReadFile("dashboard.html")
	if err != nil {
		http.Error(w, "Dashboard not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) terminalPage(w http.ResponseWriter, r *http.Request) {
	data, err := dashboardFS.ReadFile("terminal.html")
	if err != nil {
		http.Error(w, "Terminal page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handlePtyProxy(w http.ResponseWriter, r *http.Request) {
	// Basic auth check for PTY
	if s.username != "" || s.password != "" {
		user, pass, ok := r.BasicAuth()
		if !ok || user != s.username || pass != s.password {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	serviceName := r.URL.Query().Get("service")
	host := r.URL.Query().Get("host")
	pidStr := r.URL.Query().Get("pid")
	pid, _ := strconv.Atoi(pidStr)

	log.Printf("[pty] request service=%s host=%s pid=%d remote=%s", serviceName, host, pid, r.RemoteAddr)

	ac := s.hub.GetAgent(serviceName, host, pid)
	if ac == nil {
		log.Printf("[pty] agent not found service=%s host=%s pid=%d", serviceName, host, pid)
		http.Error(w, "Agent not found", http.StatusNotFound)
		return
	}
	// Log what agent binary we're actually talking to — stale builds that
	// predate pty support silently drop TypePtyStart via the default branch.
	log.Printf("[pty] agent version=%q commit=%q go=%q exe=%q uptimeStartMs=%d",
		ac.Version, ac.CommitHash, ac.GoVersion, ac.ExePath, ac.StartTimeMs)

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[pty] ws upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	sessionID := fmt.Sprintf("pty-%d", time.Now().UnixNano())
	// Clear audit-style session banner: who opened it, which target, when.
	log.Printf("[pty-audit] OPEN session=%s service=%s host=%s pid=%d agentIP=%s browserIP=%s user=%q",
		sessionID, serviceName, host, pid, ac.RemoteAddr, r.RemoteAddr, s.username)

	// Per-session command logger: xterm forwards keystrokes one at a time,
	// so we accumulate them and emit one log line per Enter.
	cmdCount := 0
	inputLog := &ptyInputLogger{
		onCommand: func(line string) {
			cmdCount++
			log.Printf("[pty-audit] CMD  session=%s service=%s host=%s agentIP=%s n=%d cmd=%q",
				sessionID, serviceName, host, ac.RemoteAddr, cmdCount, line)
		},
		onSignal: func(sig string) {
			log.Printf("[pty-audit] SIG  session=%s service=%s host=%s agentIP=%s sig=%s",
				sessionID, serviceName, host, ac.RemoteAddr, sig)
		},
	}
	defer func() {
		log.Printf("[pty-audit] CLOSE session=%s service=%s host=%s agentIP=%s commands=%d",
			sessionID, serviceName, host, ac.RemoteAddr, cmdCount)
	}()

	// Register browser WebSocket for PTY output
	s.hub.SubscribePty(sessionID, conn)
	defer s.hub.UnsubscribePty(sessionID)

	// Start PTY on agent
	if err := ac.SendCommand(protocol.TypePtyStart, sessionID, nil); err != nil {
		log.Printf("[pty] session=%s SendCommand(PtyStart) failed: %v", sessionID, err)
		// Tell the browser why — otherwise xterm just hangs.
		_ = conn.WriteJSON(map[string]string{
			"type": "output",
			"data": "\r\n\x1b[31m[control-plane] failed to send PtyStart to agent: " + err.Error() + "\x1b[0m\r\n",
		})
		return
	}
	log.Printf("[pty] session=%s PtyStart sent to agent", sessionID)

	// Send initial resize if provided
	cols := r.URL.Query().Get("cols")
	rows := r.URL.Query().Get("rows")
	if cols != "" && rows != "" {
		c, _ := strconv.Atoi(cols)
		rr, _ := strconv.Atoi(rows)
		_ = ac.SendCommand(protocol.TypePtyResize, sessionID, protocol.PtyResizeData{Cols: uint16(c), Rows: uint16(rr)})
	}

	// Read from browser WebSocket, forward to agent
	for {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			break
		}
		msgType, _ := msg["type"].(string)
		switch msgType {
		case "input":
			data, _ := msg["data"].(string)
			inputLog.feed(data)
			_ = ac.SendCommand(protocol.TypePtyInput, sessionID, protocol.PtyInputData{Data: data})
		case "resize":
			cols, _ := msg["cols"].(float64)
			rows, _ := msg["rows"].(float64)
			_ = ac.SendCommand(protocol.TypePtyResize, sessionID, protocol.PtyResizeData{Cols: uint16(cols), Rows: uint16(rows)})
		}
	}

	// Browser disconnected, close PTY
	log.Printf("[pty] session=%s browser disconnected, sending PtyClose", sessionID)
	_ = ac.SendCommand(protocol.TypePtyClose, sessionID, nil)
}

// ptyInputLogger reconstructs user commands from the character-by-character
// keystroke stream that xterm.js sends. It handles backspace, Ctrl-C/Ctrl-D,
// and swallows CSI escape sequences (arrow keys, history navigation) so the
// logged command line reflects what the user actually typed at the prompt.
//
// Caveats: tab completion happens shell-side, so what we log is the *typed*
// line, not the expanded one. That's fine for audit — it still shows intent.
type ptyInputLogger struct {
	buf       []rune
	escState  int // 0 = normal, 1 = after ESC, 2 = inside CSI/SS3
	onCommand func(line string)
	onSignal  func(sig string)
}

func (p *ptyInputLogger) feed(data string) {
	for _, r := range data {
		switch p.escState {
		case 1:
			// Just saw ESC. If next is '[' or 'O' we're entering a
			// CSI/SS3 sequence; otherwise it's a short ESC+char sequence
			// (e.g. Alt+key) which we also drop.
			if r == '[' || r == 'O' {
				p.escState = 2
			} else {
				p.escState = 0
			}
			continue
		case 2:
			// CSI final byte is 0x40..0x7E; everything before (params,
			// intermediates) stays inside the sequence.
			if r >= 0x40 && r <= 0x7e {
				p.escState = 0
			}
			continue
		}

		switch r {
		case '\x1b': // ESC
			p.escState = 1
		case '\r', '\n':
			line := string(p.buf)
			p.buf = p.buf[:0]
			if line != "" {
				p.onCommand(line)
			} else {
				p.onCommand("") // log empty Enter too — useful for audit timing
			}
		case '\x7f', '\b': // Backspace / DEL
			if len(p.buf) > 0 {
				p.buf = p.buf[:len(p.buf)-1]
			}
		case '\x03': // Ctrl-C
			p.onSignal("^C line=" + string(p.buf))
			p.buf = p.buf[:0]
		case '\x04': // Ctrl-D
			p.onSignal("^D line=" + string(p.buf))
			p.buf = p.buf[:0]
		case '\x15': // Ctrl-U (kill line)
			p.buf = p.buf[:0]
		case '\x17': // Ctrl-W (kill word)
			// Trim trailing spaces, then trailing non-spaces.
			for len(p.buf) > 0 && p.buf[len(p.buf)-1] == ' ' {
				p.buf = p.buf[:len(p.buf)-1]
			}
			for len(p.buf) > 0 && p.buf[len(p.buf)-1] != ' ' {
				p.buf = p.buf[:len(p.buf)-1]
			}
		default:
			// Accept printables and tab. Drop other C0 controls silently.
			if r >= 0x20 || r == '\t' {
				p.buf = append(p.buf, r)
			}
		}
	}
}

func sortInstances(instances []instanceInfo) {
	slices.SortFunc(instances, func(a, b instanceInfo) int {
		if c := cmp.Compare(a.Host, b.Host); c != 0 {
			return c
		}
		return cmp.Compare(a.PID, b.PID)
	})
}

func calcUptime(startTimeMs int64) string {
	if startTimeMs <= 0 {
		return "-"
	}
	d := time.Since(time.UnixMilli(startTimeMs)).Round(time.Second)
	days := int(d.Hours()) / 24
	d -= time.Duration(days) * 24 * time.Hour
	hours := int(d.Hours())
	d -= time.Duration(hours) * time.Hour
	mins := int(d.Minutes())
	d -= time.Duration(mins) * time.Minute
	secs := int(d.Seconds())
	if days > 0 {
		return fmt.Sprintf("%dd%dh%dm%ds", days, hours, mins, secs)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh%dm%ds", hours, mins, secs)
	}
	if mins > 0 {
		return fmt.Sprintf("%dm%ds", mins, secs)
	}
	return fmt.Sprintf("%ds", secs)
}

// --- API Handlers ---

type serviceInfo struct {
	Name      string         `json:"name"`
	Group     string         `json:"group,omitempty"`
	Instances []instanceInfo `json:"instances"`
	BinaryDir string         `json:"binaryDir,omitempty"`
}

type instanceInfo struct {
	Host        string `json:"host"`
	RemoteAddr  string `json:"remoteAddr"`
	CountryCode string `json:"countryCode,omitempty"`
	PID         int    `json:"pid"`
	Port       uint   `json:"port"`
	Version      string  `json:"version"`
	CommitHash   string  `json:"commitHash,omitempty"`
	CommitTime   string  `json:"commitTime,omitempty"`
	GoVersion    string  `json:"goVersion,omitempty"`
	StartTimeMs  int64   `json:"startTimeMs"`
	ExePath      string  `json:"exePath"`
	Uptime       string  `json:"uptime"`
	AppArgs      string  `json:"appArgs,omitempty"`
	CPUPercent   float64 `json:"cpuPercent"`
	MemRSS       uint64  `json:"memRSS"`
	MemUsed      uint64  `json:"memUsed"`
	MemTotal     uint64  `json:"memTotal"`
	NumGoroutine int     `json:"numGoroutine"`
}

func (s *Server) apiServices(w http.ResponseWriter, r *http.Request) {
	services := s.hub.Services()
	var result []serviceInfo
	for name, agents := range services {
		si := serviceInfo{Name: name}
		for _, ac := range agents {
			if si.Group == "" {
				si.Group = ac.Group
			}
			if si.BinaryDir == "" {
				si.BinaryDir = ac.BinaryDir
			}
			si.Instances = append(si.Instances, instanceInfo{
				Host:       ac.Host,
				RemoteAddr:  ac.RemoteAddr,
				CountryCode: ac.CountryCode,
				PID:      ac.PID,
				Port:     ac.Port,
				Version:  ac.Version,
				CommitHash:   ac.CommitHash,
				CommitTime:   ac.CommitTime,
				GoVersion:    ac.GoVersion,
				StartTimeMs:    ac.StartTimeMs,
				ExePath:      ac.ExePath,
				Uptime:       calcUptime(ac.StartTimeMs),
				AppArgs:      ac.AppArgs,
				CPUPercent:   ac.CPUPercent,
				MemRSS:       ac.MemRSS,
				MemUsed:      ac.MemUsed,
				MemTotal:     ac.MemTotal,
				NumGoroutine: ac.NumGoroutine,
			})
		}
		sortInstances(si.Instances)
		result = append(result, si)
	}
	slices.SortFunc(result, func(a, b serviceInfo) int {
		return cmp.Compare(a.Name, b.Name)
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) apiServiceDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	agents := s.hub.AgentsByService(name)
	if len(agents) == 0 {
		http.Error(w, "Service not found", http.StatusNotFound)
		return
	}

	si := serviceInfo{Name: name}
	for _, ac := range agents {
		if si.Group == "" {
			si.Group = ac.Group
		}
		if si.BinaryDir == "" {
			si.BinaryDir = ac.BinaryDir
		}
		si.Instances = append(si.Instances, instanceInfo{
			Host:         ac.Host,
			RemoteAddr:   ac.RemoteAddr,
			CountryCode: ac.CountryCode,
			PID:          ac.PID,
			Port:         ac.Port,
			Version:      ac.Version,
			CommitHash:   ac.CommitHash,
			CommitTime:   ac.CommitTime,
			GoVersion:    ac.GoVersion,
			StartTimeMs:    ac.StartTimeMs,
			ExePath:      ac.ExePath,
			Uptime:       calcUptime(ac.StartTimeMs),
			AppArgs:      ac.AppArgs,
			CPUPercent:   ac.CPUPercent,
			MemRSS:       ac.MemRSS,
			MemUsed:      ac.MemUsed,
			MemTotal:     ac.MemTotal,
			NumGoroutine: ac.NumGoroutine,
		})
	}

	sortInstances(si.Instances)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(si)
}

func (s *Server) apiFiles(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	agents := s.hub.AgentsByService(name)
	if len(agents) == 0 {
		http.Error(w, "Service not found", http.StatusNotFound)
		return
	}

	// Get BinaryDir from the first agent's registration
	binaryDir := agents[0].BinaryDir
	if binaryDir == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]struct{}{})
		return
	}

	targetDir := filepath.Join(s.cdnDir, filepath.Clean(binaryDir))
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]struct{}{})
		return
	}

	type fileItem struct {
		Name string `json:"name"`
		Hash string `json:"hash"`
	}
	var files []fileItem
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		files = append(files, fileItem{
			Name: e.Name(),
			Hash: cdnFileMD5(s.cdnSalt, e.Name()),
		})
	}
	sort.Slice(files, func(i, j int) bool {
		fi, _ := os.Stat(filepath.Join(targetDir, files[i].Name))
		fj, _ := os.Stat(filepath.Join(targetDir, files[j].Name))
		if fi == nil || fj == nil {
			return files[i].Name > files[j].Name
		}
		return fi.ModTime().After(fj.ModTime())
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(files)
}

func (s *Server) apiDeploy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	fileName := r.URL.Query().Get("file")
	appArgs := r.URL.Query().Get("appArgs")
	host := r.URL.Query().Get("host")
	pidStr := r.URL.Query().Get("pid")

	reqID := fmt.Sprintf("deploy-%d", time.Now().UnixNano())

	// If host+pid specified, deploy to specific instance. Otherwise deploy to all.
	var targets []*AgentConn
	if host != "" && pidStr != "" {
		pid, _ := strconv.Atoi(pidStr)
		ac := s.hub.GetAgent(name, host, pid)
		if ac == nil {
			http.Error(w, "Agent not found", http.StatusNotFound)
			return
		}
		targets = []*AgentConn{ac}
	} else {
		targets = s.hub.AgentsByService(name)
	}

	if len(targets) == 0 {
		http.Error(w, "No agents found for service", http.StatusNotFound)
		return
	}

	// Subscribe BEFORE sending command to avoid losing early logs
	s.hub.SubscribeLogs(reqID)

	fileHash := cdnFileMD5(s.cdnSalt, fileName)
	for _, ac := range targets {
		if err := ac.SendCommand(protocol.TypeDeploy, reqID, protocol.DeployData{
			FileName: fileName,
			FileHash: fileHash,
			AppArgs:  appArgs,
		}); err != nil {
			_ = err
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"requestId": reqID})
}

func (s *Server) apiRestart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	host := r.URL.Query().Get("host")
	pidStr := r.URL.Query().Get("pid")
	appArgs := r.URL.Query().Get("appArgs")

	if host == "" || pidStr == "" {
		http.Error(w, "host and pid are required", http.StatusBadRequest)
		return
	}

	pid, _ := strconv.Atoi(pidStr)
	ac := s.hub.GetAgent(name, host, pid)
	if ac == nil {
		http.Error(w, "Agent not found", http.StatusNotFound)
		return
	}

	reqID := fmt.Sprintf("restart-%d", time.Now().UnixNano())
	s.hub.SubscribeLogs(reqID)

	if err := ac.SendCommand(protocol.TypeRestart, reqID, protocol.RestartData{AppArgs: appArgs}); err != nil {
		s.hub.UnsubscribeLogs(reqID)
		http.Error(w, "Failed to send restart: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"requestId": reqID})
}

func (s *Server) apiKill(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	host := r.URL.Query().Get("host")
	pidStr := r.URL.Query().Get("pid")

	if host == "" || pidStr == "" {
		http.Error(w, "host and pid are required", http.StatusBadRequest)
		return
	}

	pid, _ := strconv.Atoi(pidStr)
	ac := s.hub.GetAgent(name, host, pid)
	if ac == nil {
		http.Error(w, "Agent not found", http.StatusNotFound)
		return
	}

	reqID := fmt.Sprintf("kill-%d", time.Now().UnixNano())
	s.hub.SubscribeLogs(reqID)

	if err := ac.SendCommand(protocol.TypeKill, reqID, protocol.KillData{PID: pid}); err != nil {
		s.hub.UnsubscribeLogs(reqID)
		http.Error(w, "Failed to send kill: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"requestId": reqID})
}

func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request) {
	requestID := r.URL.Query().Get("requestId")
	if requestID == "" {
		http.Error(w, "requestId is required", http.StatusBadRequest)
		return
	}

	ch := s.hub.SubscribeLogs(requestID)
	defer s.hub.UnsubscribeLogs(requestID)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	f, _ := w.(http.Flusher)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return // client disconnected
		case line, ok := <-ch:
			if !ok {
				return // channel closed
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", line)
			f.Flush()
			if strings.HasPrefix(line, "EOF") || strings.HasPrefix(line, "ERR") {
				return
			}
		}
	}
}

type resourceSnapshot struct {
	Host         string  `json:"host"`
	RemoteAddr   string  `json:"remoteAddr"`
	CountryCode  string  `json:"countryCode,omitempty"`
	PID          int     `json:"pid"`
	CPUPercent   float64 `json:"cpuPercent"`
	MemRSS       uint64  `json:"memRSS"`
	MemUsed      uint64  `json:"memUsed"`
	MemTotal     uint64  `json:"memTotal"`
	NumGoroutine int     `json:"numGoroutine"`
}

func (s *Server) apiResources(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	ch := s.hub.SubscribeResources(name)
	defer s.hub.UnsubscribeResources(name, ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	f, _ := w.(http.Flusher)

	ctx := r.Context()

	sendSnapshot := func() {
		agents := s.hub.AgentsByService(name)
		var snapshots []resourceSnapshot
		for _, ac := range agents {
			ac.mu.Lock()
			snapshots = append(snapshots, resourceSnapshot{
				Host:         ac.Host,
				RemoteAddr:   ac.RemoteAddr,
				CountryCode:  ac.CountryCode,
				PID:          ac.PID,
				CPUPercent:   ac.CPUPercent,
				MemRSS:       ac.MemRSS,
				MemUsed:      ac.MemUsed,
				MemTotal:     ac.MemTotal,
				NumGoroutine: ac.NumGoroutine,
			})
			ac.mu.Unlock()
		}
		data, _ := json.Marshal(map[string]interface{}{"instances": snapshots})
		fmt.Fprintf(w, "data: %s\n\n", data)
		f.Flush()
	}

	// Send initial snapshot
	sendSnapshot()

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			sendSnapshot()
		}
	}
}

func (s *Server) apiRollback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	host := r.URL.Query().Get("host")
	pidStr := r.URL.Query().Get("pid")

	reqID := fmt.Sprintf("rollback-%d", time.Now().UnixNano())

	var targets []*AgentConn
	if host != "" && pidStr != "" {
		pid, _ := strconv.Atoi(pidStr)
		ac := s.hub.GetAgent(name, host, pid)
		if ac == nil {
			http.Error(w, "Agent not found", http.StatusNotFound)
			return
		}
		targets = []*AgentConn{ac}
	} else {
		targets = s.hub.AgentsByService(name)
	}

	if len(targets) == 0 {
		http.Error(w, "No agents found for service", http.StatusNotFound)
		return
	}

	s.hub.SubscribeLogs(reqID)

	for _, ac := range targets {
		if err := ac.SendCommand(protocol.TypeRollback, reqID, protocol.RollbackData{}); err != nil {
			_ = err
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"requestId": reqID})
}

func (s *Server) apiExec(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	host := r.URL.Query().Get("host")
	pidStr := r.URL.Query().Get("pid")
	cmd := r.URL.Query().Get("cmd")
	cwd := r.URL.Query().Get("cwd")

	if host == "" || pidStr == "" {
		http.Error(w, "host and pid are required", http.StatusBadRequest)
		return
	}
	if cmd == "" {
		http.Error(w, "cmd is required", http.StatusBadRequest)
		return
	}

	pid, _ := strconv.Atoi(pidStr)
	ac := s.hub.GetAgent(name, host, pid)
	if ac == nil {
		http.Error(w, "Agent not found", http.StatusNotFound)
		return
	}

	reqID := fmt.Sprintf("exec-%d", time.Now().UnixNano())
	s.hub.SubscribeLogs(reqID)

	if err := ac.SendCommand(protocol.TypeExec, reqID, protocol.ExecData{Command: cmd, Cwd: cwd}); err != nil {
		s.hub.UnsubscribeLogs(reqID)
		http.Error(w, "Failed to send exec: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"requestId": reqID})
}

func (s *Server) apiComplete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	host := r.URL.Query().Get("host")
	pidStr := r.URL.Query().Get("pid")
	input := r.URL.Query().Get("input")
	cwd := r.URL.Query().Get("cwd")

	if host == "" || pidStr == "" {
		http.Error(w, "host and pid are required", http.StatusBadRequest)
		return
	}

	pid, _ := strconv.Atoi(pidStr)
	ac := s.hub.GetAgent(name, host, pid)
	if ac == nil {
		http.Error(w, "Agent not found", http.StatusNotFound)
		return
	}

	reqID := fmt.Sprintf("comp-%d", time.Now().UnixNano())
	resp, err := ac.SendRequest(protocol.TypeComplete, reqID, protocol.CompleteData{Input: input, Cwd: cwd}, 5*time.Second)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.CompleteResult{})
		return
	}

	var result protocol.CompleteResult
	_ = json.Unmarshal(resp.Data, &result)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) apiBatchDeploy(w http.ResponseWriter, r *http.Request) {
	servicesParam := r.URL.Query().Get("services") // comma-separated
	fileName := r.URL.Query().Get("file")
	appArgs := r.URL.Query().Get("appArgs")

	if servicesParam == "" || fileName == "" {
		http.Error(w, "services and file are required", http.StatusBadRequest)
		return
	}

	serviceNames := strings.Split(servicesParam, ",")
	reqID := fmt.Sprintf("batch-%d", time.Now().UnixNano())

	s.hub.SubscribeLogs(reqID)

	fileHash := cdnFileMD5(s.cdnSalt, fileName)
	sent := 0
	for _, svcName := range serviceNames {
		svcName = strings.TrimSpace(svcName)
		agents := s.hub.AgentsByService(svcName)
		for _, ac := range agents {
			if err := ac.SendCommand(protocol.TypeDeploy, reqID, protocol.DeployData{
				FileName: fileName,
				FileHash: fileHash,
				AppArgs:  appArgs,
			}); err != nil {
				_ = err
			} else {
				sent++
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"requestId": reqID,
		"sent":      sent,
	})
}

// GET /api/cdn/list?dir=mychat-server&prefix=mychat-server-
func (s *Server) apiCDNList(w http.ResponseWriter, r *http.Request) {
	subDir := r.URL.Query().Get("dir")
	prefix := r.URL.Query().Get("prefix")
	targetDir := s.cdnDir
	if subDir != "" {
		targetDir = filepath.Join(s.cdnDir, filepath.Clean(subDir))
	}
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type cdnFile struct {
		Name string `json:"name"`
		Hash string `json:"hash"`
	}
	var files []cdnFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if prefix == "" || strings.HasPrefix(e.Name(), prefix) {
			files = append(files, cdnFile{
				Name: e.Name(),
				Hash: cdnFileMD5(s.cdnSalt, e.Name()),
			})
		}
	}
	sort.Slice(files, func(i, j int) bool {
		fi, _ := os.Stat(filepath.Join(targetDir, files[i].Name))
		fj, _ := os.Stat(filepath.Join(targetDir, files[j].Name))
		if fi == nil || fj == nil {
			return files[i].Name > files[j].Name
		}
		return fi.ModTime().After(fj.ModTime())
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(files)
}

// DELETE /api/cdn/file?dir=mychat-server&file=mychat-server-74f782b
func (s *Server) apiCDNDelete(w http.ResponseWriter, r *http.Request) {
	subDir := r.URL.Query().Get("dir")
	fileName := r.URL.Query().Get("file")
	if fileName == "" {
		http.Error(w, "file is required", http.StatusBadRequest)
		return
	}
	// Prevent path traversal
	if strings.Contains(fileName, "/") || strings.Contains(fileName, "\\") || fileName == ".." {
		http.Error(w, "invalid file name", http.StatusBadRequest)
		return
	}
	targetDir := s.cdnDir
	if subDir != "" {
		targetDir = filepath.Join(s.cdnDir, filepath.Clean(subDir))
	}
	filePath := filepath.Join(targetDir, fileName)
	if err := os.Remove(filePath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func cdnFileMD5(salt, fileName string) string {
	h := md5.Sum([]byte(salt + fileName))
	return hex.EncodeToString(h[:])
}

// GET /cdn/d/{dir}/{hash} — public download by MD5(salt + filename).
// If no {filename} segment in URL, redirects to .../hash/realname so that
// wget/curl save with the correct filename without extra flags.
func (s *Server) apiCDNDownload(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Clean(r.PathValue("dir"))
	hash := r.PathValue("hash")
	targetDir := filepath.Join(s.cdnDir, dir)
	files, err := os.ReadDir(targetDir)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if cdnFileMD5(s.cdnSalt, f.Name()) == hash {
			// If accessed without filename suffix, redirect so wget uses
			// the real filename from the final URL segment.
			if r.PathValue("filename") == "" {
				http.Redirect(w, r, r.URL.Path+"/"+f.Name(), http.StatusFound)
				return
			}
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", f.Name()))
			http.ServeFile(w, r, filepath.Join(targetDir, f.Name()))
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func (s *Server) startCDNCleanup() {
	go func() {
		for {
			s.cleanupCDN(10)
			time.Sleep(time.Hour)
		}
	}()
}

func (s *Server) cleanupCDN(keep int) {
	entries, err := os.ReadDir(s.cdnDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(s.cdnDir, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		type fileEntry struct {
			name    string
			modTime time.Time
		}
		var list []fileEntry
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			list = append(list, fileEntry{name: f.Name(), modTime: info.ModTime()})
		}
		if len(list) <= keep {
			continue
		}
		sort.Slice(list, func(i, j int) bool {
			return list[i].modTime.After(list[j].modTime)
		})
		for _, f := range list[keep:] {
			os.Remove(filepath.Join(dir, f.name))
			log.Printf("[CDN] Cleanup: removed %s/%s", e.Name(), f.name)
		}
	}
}
