package agent

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vito-go/go-deployer/controlplane"
	"github.com/vito-go/go-deployer/protocol"
)

// Config is the configuration for the agent SDK.
type Config struct {
	ServerHost  string // Control plane host, e.g. "api.myproxy.life:2053"
	ServiceName string // Logical service name, e.g. "user-api"
	BinaryDir   string // CDN subdirectory name for binary files, e.g. "mychat-server"
	AppArgs     string // Additional arguments for starting the application
	Port        uint   // Port of the deployed application
	Token       string // Authentication token
	CertFP      string // Control plane certificate fingerprint for cert pinning
	Group       string // Service group, e.g. "production", "staging" (optional)
}

type agentClient struct {
	cfg         Config
	conn        *websocket.Conn
	mu          sync.Mutex
	deployMu    sync.Mutex
	version     string
	commitHash  string
	commitTime  string
	goVersion   string
	hostname    string
	startTime   time.Time
	memTotal    uint64
	targetDir   string
	workDir     string
	ptySessions    map[string]*ptySession
	ptyMu          sync.Mutex
	resourceStop   chan struct{} // signal to stop resource reporting
	resourceActive bool
	resourceMu     sync.Mutex
}

// Register connects to the control plane and starts the agent loop.
// This is non-blocking — it runs in background goroutines.
func Register(cfg Config) {
	hostname, _ := os.Hostname()
	a := &agentClient{
		cfg:         cfg,
		hostname:    hostname,
		startTime:   time.Now(),
		ptySessions: make(map[string]*ptySession),
	}

	a.version = "unknown"
	a.goVersion = runtime.Version()
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, st := range bi.Settings {
			switch st.Key {
			case "vcs.revision":
				a.commitHash = st.Value
				a.version = st.Value
				if len(a.version) > 7 {
					a.version = a.version[:7]
				}
			case "vcs.time":
				a.commitTime = st.Value
			}
		}
	}
	a.memTotal = getSystemMemory()

	// Setup local directories
	home, _ := os.UserHomeDir()
	baseDir := filepath.Join(home, ".deployer-agent", cfg.ServiceName)
	a.targetDir = filepath.Join(baseDir, "bin")
	a.workDir = filepath.Join(baseDir, "worker")
	_ = os.MkdirAll(a.targetDir, 0755)
	_ = os.MkdirAll(a.workDir, 0755)

	go a.connectLoop()
}

func exePath() string {
	p, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	p, _ = filepath.EvalSymlinks(p)
	return p
}

func (a *agentClient) connectLoop() {
	backoff := time.Second
	for {
		err := a.connect()
		if err != nil {
			_ = err
			time.Sleep(backoff)
			if backoff < 30*time.Second {
				backoff = backoff * 2
			}
		} else {
			// Connected successfully then disconnected, reset backoff
			backoff = time.Second
		}
	}
}

func (a *agentClient) connect() error {
	header := make(http.Header)
	if a.cfg.Token != "" {
		header.Set("Authorization", "Bearer "+a.cfg.Token)
	}

	dialer := *websocket.DefaultDialer
	if a.cfg.CertFP != "" {
		dialer.TLSClientConfig = controlplane.PinnedTLSConfig(a.cfg.CertFP)
	}

	wsURL := "wss://" + a.cfg.ServerHost + "/ws"
	conn, _, err := dialer.Dial(wsURL, header)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()

	defer func() {
		conn.Close()
		a.mu.Lock()
		a.conn = nil
		a.mu.Unlock()
	}()

	// Send registration
	if err := a.sendMsg(protocol.TypeRegister, "", protocol.RegisterData{
		ServiceName: a.cfg.ServiceName,
		Group:       a.cfg.Group,
		BinaryDir:   a.cfg.BinaryDir,
		Host:        a.hostname,
		PID:         os.Getpid(),
		Port:        a.cfg.Port,
		Version:     a.version,
		CommitHash:  a.commitHash,
		CommitTime:  a.commitTime,
		GoVersion:   a.goVersion,
		StartTimeMs: a.startTime.UnixMilli(),
		ExePath:     exePath(),
		AppArgs:     strings.Join(os.Args[1:], " "),
	}); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	// Read commands
	for {
		var msg protocol.Message
		if err := conn.ReadJSON(&msg); err != nil {
			return fmt.Errorf("read: %w", err)
		}
		go func(m protocol.Message) {
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					// Report the panic back to the control plane so it's
					// visible in logs / terminal, instead of silently dying.
					_ = a.sendMsg(protocol.TypeLog, m.ID, protocol.LogData{
						Line: fmt.Sprintf("ERR: panic in handler type=%s: %v\n%s", m.Type, r, stack),
					})
				}
			}()
			a.handleCommand(m)
		}(msg)
	}
}

func (a *agentClient) handleResourceStart() {
	a.resourceMu.Lock()
	defer a.resourceMu.Unlock()
	if a.resourceActive {
		return // already reporting
	}
	a.resourceActive = true
	a.resourceStop = make(chan struct{})
	go a.resourceLoop(a.resourceStop)
}

func (a *agentClient) handleResourceStop() {
	a.resourceMu.Lock()
	defer a.resourceMu.Unlock()
	if !a.resourceActive {
		return
	}
	a.resourceActive = false
	close(a.resourceStop)
}

func (a *agentClient) resourceLoop(stop chan struct{}) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	pid := os.Getpid()

	// Send immediately on start
	a.sendResourceData(pid)

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			a.sendResourceData(pid)
		}
	}
}

func (a *agentClient) sendResourceData(pid int) {
	cpuPercent, memRSS := collectProcessStats(pid)
	_ = a.sendMsg(protocol.TypeResourceData, "", protocol.ResourceData{
		PID:          pid,
		CPUPercent:   cpuPercent,
		MemRSS:       memRSS,
		MemUsed:      getSystemMemUsed(),
		MemTotal:     a.memTotal,
		NumGoroutine: runtime.NumGoroutine(),
	})
}

func (a *agentClient) sendMsg(msgType string, id string, data interface{}) error {
	raw, _ := json.Marshal(data)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == nil {
		return fmt.Errorf("not connected")
	}
	return a.conn.WriteJSON(protocol.Message{Type: msgType, ID: id, Data: raw})
}

func (a *agentClient) sendLog(id string, line string) {
	// Don't prefix EOF/ERR — they are control signals, not output
	if strings.HasPrefix(line, "EOF") || strings.HasPrefix(line, "ERR") {
		_ = a.sendMsg(protocol.TypeLog, id, protocol.LogData{Line: line})
		return
	}
	prefixed := fmt.Sprintf("[%s:%d] %s", a.hostname, os.Getpid(), line)
	_ = a.sendMsg(protocol.TypeLog, id, protocol.LogData{Line: prefixed})
}

func (a *agentClient) handleCommand(msg protocol.Message) {
	switch msg.Type {
	case protocol.TypeDeploy:
		a.handleDeploy(msg)
	case protocol.TypeRestart:
		a.handleRestart(msg)
	case protocol.TypeRollback:
		a.handleRollback(msg)
	case protocol.TypeExec:
		a.handleExec(msg)
	case protocol.TypeComplete:
		a.handleComplete(msg)
	case protocol.TypeResourceStart:
		a.handleResourceStart()
	case protocol.TypeResourceStop:
		a.handleResourceStop()
	case protocol.TypePtyStart:
		a.handlePtyStart(msg)
	case protocol.TypePtyInput:
		a.handlePtyInput(msg)
	case protocol.TypePtyResize:
		a.handlePtyResize(msg)
	case protocol.TypePtyClose:
		a.handlePtyClose(msg)
	case protocol.TypeKill:
		a.handleKill(msg)
	default:
	}
}

func (a *agentClient) cdnBaseURL() string {
	return "https://" + a.cfg.ServerHost
}

// downloadFile fetches url to dst over HTTPS (TLS verification skipped, matching
// the previous `curl -k`). It streams to a temp file then renames into place so
// a partial download can never be mistaken for a good binary, and reports
// periodic progress via sendLog.
func (a *agentClient) downloadFile(msgID, url, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	client := &http.Client{
		Timeout: 30 * time.Minute,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}

	tmp := dst + ".downloading"
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("open %s: %w", tmp, err)
	}

	pw := &progressWriter{
		total: resp.ContentLength,
		last:  time.Now(),
		onTick: func(done, total int64) {
			if total > 0 {
				a.sendLog(msgID, fmt.Sprintf("   %.1f%% (%s / %s)",
					float64(done)*100/float64(total), humanBytes(done), humanBytes(total)))
			} else {
				a.sendLog(msgID, fmt.Sprintf("   %s", humanBytes(done)))
			}
		},
	}
	if _, err := io.Copy(f, io.TeeReader(resp.Body, pw)); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

type progressWriter struct {
	total  int64
	done   int64
	last   time.Time
	onTick func(done, total int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n := len(b)
	p.done += int64(n)
	if time.Since(p.last) >= time.Second {
		p.last = time.Now()
		p.onTick(p.done, p.total)
	}
	return n, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (a *agentClient) handleDeploy(msg protocol.Message) {
	if !a.deployMu.TryLock() {
		a.sendLog(msg.ID, "ERR: A deployment is already in progress")
		return
	}
	defer a.deployMu.Unlock()

	var req protocol.DeployData
	_ = json.Unmarshal(msg.Data, &req)

	if req.FileName == "" {
		a.sendLog(msg.ID, "ERR: No file specified")
		return
	}

	appArgs := req.AppArgs
	if appArgs == "" {
		appArgs = a.cfg.AppArgs
	}

	a.sendLog(msg.ID, fmt.Sprintf("[DEPLOY] %s Starting deployment...", time.Now().Format(time.DateTime)))

	downloadURL := a.cdnBaseURL() + "/cdn/d/" + a.cfg.BinaryDir + "/" + req.FileHash
	targetPath, _ := filepath.Abs(filepath.Join(a.targetDir, req.FileName))

	// Remove old file. On Windows, running exes can't be overwritten but can be renamed,
	// so rename to .old first (then os.Remove which may fail silently if still running).
	if _, err := os.Stat(targetPath); err == nil {
		oldPath := targetPath + ".old"
		os.Remove(oldPath)
		if err := os.Rename(targetPath, oldPath); err != nil {
			os.Remove(targetPath)
		}
	}

	start := time.Now()
	a.sendLog(msg.ID, ">> STEP: Download Binary")
	a.sendLog(msg.ID, "   URL: "+downloadURL)
	if err := a.downloadFile(msg.ID, downloadURL, targetPath); err != nil {
		a.sendLog(msg.ID, "ERR: Download failed: "+err.Error())
		return
	}
	a.sendLog(msg.ID, fmt.Sprintf("   SUCCESS (%v)", time.Since(start).Round(time.Millisecond)))

	a.startBinary(msg.ID, targetPath, appArgs)
}

func (a *agentClient) startBinary(id, targetPath, appArgs string) {
	a.sendLog(id, ">> STEP: Set Executable Permissions")
	if err := os.Chmod(targetPath, 0755); err != nil {
		a.sendLog(id, "ERR: "+err.Error())
		return
	}
	a.sendLog(id, "   SUCCESS")

	a.sendLog(id, fmt.Sprintf(">> RUN: %s %s", targetPath, appArgs))
	if err := startBackgroundProcess(targetPath, appArgs, a.workDir); err != nil {
		a.sendLog(id, "ERR: "+err.Error())
		return
	}

	// Cleanup old binaries
	a.cleanupOldBinaries(5)

	a.sendLog(id, "EOF")
}

func (a *agentClient) handleRestart(msg protocol.Message) {
	var req protocol.RestartData
	_ = json.Unmarshal(msg.Data, &req)

	appArgs := req.AppArgs
	if appArgs == "" {
		appArgs = a.cfg.AppArgs
	}

	a.sendLog(msg.ID, fmt.Sprintf("[RESTART] %s Restarting service...", time.Now().Format(time.DateTime)))

	// Find the current binary path
	exePath, err := os.Executable()
	if err != nil {
		a.sendLog(msg.ID, "ERR: Cannot determine executable path: "+err.Error())
		return
	}
	exePath, _ = filepath.EvalSymlinks(exePath)

	a.sendLog(msg.ID, ">> STEP: Start New Instance")
	a.sendLog(msg.ID, "   Binary: "+exePath)

	if err := startBackgroundProcess(exePath, appArgs, a.workDir); err != nil {
		a.sendLog(msg.ID, "ERR: "+err.Error())
		return
	}

	a.sendLog(msg.ID, "   SUCCESS")
	a.sendLog(msg.ID, "EOF")
}

func (a *agentClient) handleRollback(msg protocol.Message) {
	if !a.deployMu.TryLock() {
		a.sendLog(msg.ID, "ERR: A deployment is already in progress")
		return
	}
	defer a.deployMu.Unlock()

	a.sendLog(msg.ID, fmt.Sprintf("[ROLLBACK] %s Starting rollback...", time.Now().Format(time.DateTime)))

	// Scan targetDir for previous binary
	files, err := os.ReadDir(a.targetDir)
	if err != nil {
		a.sendLog(msg.ID, "ERR: Cannot read binary directory: "+err.Error())
		return
	}

	type binInfo struct {
		path    string
		modTime time.Time
	}
	var binaries []binInfo
	for _, f := range files {
		if f.IsDir() || !strings.HasPrefix(f.Name(), a.cfg.ServiceName+"-") {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		binaries = append(binaries, binInfo{
			path:    filepath.Join(a.targetDir, f.Name()),
			modTime: info.ModTime(),
		})
	}

	if len(binaries) < 2 {
		a.sendLog(msg.ID, "ERR: No previous version available for rollback")
		return
	}

	// Sort newest first
	for i := 0; i < len(binaries)-1; i++ {
		for j := i + 1; j < len(binaries); j++ {
			if binaries[i].modTime.Before(binaries[j].modTime) {
				binaries[i], binaries[j] = binaries[j], binaries[i]
			}
		}
	}

	// Pick the second newest (the one before the current deploy)
	prev := binaries[1]
	a.sendLog(msg.ID, ">> Rolling back to: "+filepath.Base(prev.path))

	a.startBinary(msg.ID, prev.path, a.cfg.AppArgs)
}

func (a *agentClient) handleExec(msg protocol.Message) {
	var req protocol.ExecData
	_ = json.Unmarshal(msg.Data, &req)

	cmd := shellCommand(req.Command)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	} else {
		cmd.Dir = a.workDir
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		a.sendLog(msg.ID, "ERR: "+err.Error())
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		a.sendLog(msg.ID, "ERR: "+err.Error())
		return
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		// Send raw output without [host:pid] prefix for clean terminal display
		_ = a.sendMsg(protocol.TypeLog, msg.ID, protocol.LogData{Line: sc.Text()})
	}
	if err := cmd.Wait(); err != nil {
		a.sendLog(msg.ID, "ERR: "+err.Error())
		return
	}
	a.sendLog(msg.ID, "EOF")
}

func (a *agentClient) handleComplete(msg protocol.Message) {
	var req protocol.CompleteData
	_ = json.Unmarshal(msg.Data, &req)

	input := req.Input
	parts := strings.Fields(input)
	var word string
	if len(parts) > 0 && !strings.HasSuffix(input, " ") {
		word = parts[len(parts)-1]
	}

	dir := a.workDir
	if req.Cwd != "" {
		dir = req.Cwd
	}
	isCommand := len(parts) <= 1 && !strings.HasSuffix(input, " ")
	candidates := shellComplete(word, dir, isCommand)

	_ = a.sendMsg(protocol.TypeComplete, msg.ID, protocol.CompleteResult{Candidates: candidates})
}

func (a *agentClient) handleKill(msg protocol.Message) {
	var req protocol.KillData
	_ = json.Unmarshal(msg.Data, &req)

	myPid := os.Getpid()
	if req.PID == myPid {
		a.sendLog(msg.ID, fmt.Sprintf("[KILL] Terminating self (PID %d) in 500ms", myPid))
		a.sendLog(msg.ID, "EOF")
		go func() {
			time.Sleep(500 * time.Millisecond)
			selfTerminate()
		}()
		return
	}

	a.sendLog(msg.ID, fmt.Sprintf("[KILL] Killing PID %d", req.PID))
	if err := killProcess(req.PID); err != nil {
		a.sendLog(msg.ID, "ERR: "+err.Error())
		return
	}
	a.sendLog(msg.ID, "EOF")
}

// runCmd executes a shell command, streams output as logs, returns success.
func (a *agentClient) runCmd(id, dir, cmdStr string) bool {
	a.sendLog(id, "   "+cmdStr)
	cmd := shellCommand(cmdStr)
	cmd.Dir = dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		a.sendLog(id, "ERR: "+err.Error())
		return false
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		a.sendLog(id, "ERR: "+err.Error())
		return false
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		a.sendLog(id, "   "+sc.Text())
	}
	if err := cmd.Wait(); err != nil {
		a.sendLog(id, "ERR: "+err.Error())
		return false
	}
	a.sendLog(id, "   SUCCESS")
	return true
}

func (a *agentClient) cleanupOldBinaries(keepCount int) {
	files, err := os.ReadDir(a.targetDir)
	if err != nil {
		return
	}
	type fileInfo struct {
		name    string
		modTime time.Time
	}
	var binaries []fileInfo
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if strings.HasPrefix(f.Name(), a.cfg.ServiceName+"-") {
			info, err := f.Info()
			if err != nil {
				continue
			}
			binaries = append(binaries, fileInfo{name: f.Name(), modTime: info.ModTime()})
		}
	}
	if len(binaries) <= keepCount {
		return
	}
	// Sort newest first
	for i := 0; i < len(binaries)-1; i++ {
		for j := i + 1; j < len(binaries); j++ {
			if binaries[i].modTime.Before(binaries[j].modTime) {
				binaries[i], binaries[j] = binaries[j], binaries[i]
			}
		}
	}
	for i := keepCount; i < len(binaries); i++ {
		_ = os.Remove(filepath.Join(a.targetDir, binaries[i].name))
	}
}
