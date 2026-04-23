package deployer

import (
	"bufio"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed index.html backend.html frontend.html
var embedFS embed.FS

// PreHandler is a function that preprocesses HTTP requests before the main handler
// Common uses: authentication, authorization, logging, rate limiting, etc.
// It receives the HTTP request and returns:
//   - newR: modified request (can add context values, modify headers, etc.)
//   - next: whether to continue processing (false = stop the request chain)
type PreHandler func(w http.ResponseWriter, r *http.Request) (newR *http.Request, next bool)

// ConfigParams contains parameters for creating a new Config
type ConfigParams struct {
	ProjectName    string // Project name (optional, auto-derived from GithubRepo if empty)
	GithubRepo     string // Git repository URL (optional, but at least one of GithubRepo or BinaryURL must be set)
	Env            string // Environment name like "production", "staging" (required)
	BuildEntry     string // Build entry point like "./cmd/app" or "." (required when GithubRepo is set)
	AppArgs        string // Additional arguments to pass when starting the application (optional)
	Port           uint   // Port of the deployed application (required, must support SO_REUSEPORT for zero-downtime)
	BasePath       string // Base path for routes like "/deploy" or "/admin/deploy" (required)
	BinaryURL      string // Pre-built binary download URL (optional, e.g. S3/CDN link). When set, enables URL-based deployment
	FrontendGitURL string // Frontend repository Git URL (optional, empty = disabled)
}

// config encapsulates internal configuration for the deployer
type config struct {
	projectName     string
	buildEntry      string
	githubRepo      string
	sourceDir       string
	targetBase      string
	workingDir      string
	appArgs         string
	metadataDir     string
	port            uint
	basePath        string
	binaryURL       string
	frontendGitURL  string
	frontendDir     string
	frontendDistDir string
}

func newConfig(params ConfigParams) (*config, error) {
	if params.GithubRepo == "" && params.BinaryURL == "" {
		return nil, errors.New("at least one of GithubRepo or BinaryURL must be set")
	}
	if params.GithubRepo != "" && params.BuildEntry == "" {
		return nil, errors.New("build entry cannot be empty when GithubRepo is set")
	}
	if params.GithubRepo == "" && params.BinaryURL != "" &&
		(strings.Contains(params.BinaryURL, "{branch}") || strings.Contains(params.BinaryURL, "{commit}")) {
		return nil, errors.New("GithubRepo is required when BinaryURL contains {branch} or {commit} placeholders")
	}
	if params.Env == "" {
		return nil, errors.New("environment name cannot be empty")
	}
	if params.Port == 0 {
		return nil, errors.New("port cannot be zero")
	}
	if params.BasePath == "" {
		return nil, errors.New("base path cannot be empty")
	}

	projectName := params.ProjectName
	if projectName == "" && params.GithubRepo != "" {
		ss := strings.Split(params.GithubRepo, "/")
		if len(ss) == 0 {
			return nil, fmt.Errorf("invalid repository URL: %s", params.GithubRepo)
		}
		projectName = strings.TrimSuffix(ss[len(ss)-1], ".git")
	}
	if projectName == "" {
		return nil, errors.New("ProjectName is required when GithubRepo is not set")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get user home directory: %w", err)
	}

	basePath := params.BasePath
	if !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}
	basePath = strings.TrimSuffix(basePath, "/")

	baseDir := filepath.Join(home, "."+projectName, params.Env)
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base directory %s: %w", baseDir, err)
	}

	return &config{
		projectName:     projectName,
		buildEntry:      params.BuildEntry,
		githubRepo:      params.GithubRepo,
		sourceDir:       filepath.Join(baseDir, "repo"),
		targetBase:      filepath.Join(baseDir, "bin"),
		workingDir:      filepath.Join(baseDir, "worker"),
		metadataDir:     filepath.Join(baseDir, "metadata"),
		appArgs:         params.AppArgs,
		port:            params.Port,
		basePath:        basePath,
		binaryURL:       params.BinaryURL,
		frontendGitURL:  params.FrontendGitURL,
		frontendDir:     filepath.Join(baseDir, "frontend-repo"),
		frontendDistDir: filepath.Join(baseDir, "worker", "dist"),
	}, nil
}

type instanceInfo struct {
	Pid        int    `json:"pid"`
	Version    string `json:"version"`
	Uptime     string `json:"uptime"`
	DeployedAt string `json:"deployedAt"`
}

// Deployer manages application deployment and running instances
type Deployer struct {
	cfg             *config
	startTime       time.Time
	logChan         chan string
	frontendLogChan chan string
	deployMu        sync.Mutex
	frontendMu      sync.Mutex

	// Windows-only:tracked child 进程。Linux/Unix 下不用(SO_REUSEPORT 让新老
	// 进程自行共存切换);Windows 无 SO_REUSEPORT,deployer 必须当 supervisor
	// 显式 kill old → spawn new。
	childMu  sync.Mutex
	childPID int
}

func initRepo(cfg *config) {
	absSource, _ := filepath.Abs(cfg.sourceDir)
	if _, err := os.Stat(filepath.Join(absSource, ".git")); err != nil {
		out, err := exec.Command("git", "clone", cfg.githubRepo, absSource).CombinedOutput()
		if err != nil {
			_ = out
		}
	}
}

func mkdirAll(cfg *config) {
	_ = os.MkdirAll(cfg.metadataDir, 0755)
	_ = os.MkdirAll(cfg.targetBase, 0755)
	_ = os.MkdirAll(cfg.workingDir, 0755)
	_ = os.MkdirAll(cfg.sourceDir, 0755)
	_ = os.MkdirAll(cfg.frontendDir, 0755)
	_ = os.MkdirAll(cfg.frontendDistDir, 0755)
}

// fetchGitBranches fetches and returns branch list from a git repo directory.
func fetchGitBranches(repoDir string) ([]string, error) {
	if err := exec.Command("git", "-C", repoDir, "fetch", "--all", "--prune").Run(); err != nil {
		return nil, err
	}
	out, _ := exec.Command("git", "-C", repoDir, "branch", "-r").Output()

	var branches []string
	for _, line := range strings.Split(string(out), "\n") {
		b := strings.TrimSpace(line)
		ss := strings.SplitN(b, "/", 2)
		if len(ss) != 2 {
			continue
		}
		b = ss[1]
		if strings.Contains(b, "->") || strings.HasPrefix(b, "HEAD") || b == "" {
			continue
		}
		branches = append(branches, b)
	}

	// Put main/master first
	var result []string
	for _, b := range branches {
		if b == "main" || b == "master" {
			result = append([]string{b}, result...)
		} else {
			result = append(result, b)
		}
	}
	return result, nil
}

// fetchGitCommits returns recent commits for a branch from a git repo directory.
func fetchGitCommits(repoDir, branch string) []map[string]string {
	out, _ := exec.Command("git", "-C", repoDir, "log", "origin/"+branch, "-n", "10", "--pretty=format:%h|%an|%ar|%s").Output()
	var commits []map[string]string
	for _, line := range strings.Split(string(out), "\n") {
		if p := strings.Split(line, "|"); len(p) >= 4 {
			commits = append(commits, map[string]string{"hash": p[0], "author": p[1], "date": p[2], "msg": p[3]})
		}
	}
	return commits
}

// streamLogs writes SSE events from a channel to the HTTP response.
func streamLogs(w http.ResponseWriter, ch chan string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	f, _ := w.(http.Flusher)
	for msg := range ch {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", msg)
		f.Flush()
		if strings.HasPrefix(msg, "EOF") || strings.HasPrefix(msg, "ERR") {
			break
		}
	}
}

// startBinary 的平台分派:
//   - Unix: nohup + SO_REUSEPORT 让新老进程共存几秒,零停机(starter_unix.go)
//   - Windows: 无 SO_REUSEPORT,deployer 作 supervisor kill old → spawn new,
//     约 3-5s downtime,集群 failover 兜底(starter_windows.go)
//
// platformStart 由各 platform 文件实现。chmod + cleanup 是共享前后置。
func (s *Deployer) startBinary(targetPath string) {
	s.logChan <- ">> STEP: Set Executable Permissions"
	if err := os.Chmod(targetPath, 0755); err != nil {
		s.logChan <- "ERR: Failed to set executable permissions: " + err.Error()
		return
	}
	s.logChan <- "   SUCCESS"

	if err := s.platformStart(targetPath); err != nil {
		s.logChan <- "ERR: " + err.Error()
		return
	}

	s.logChan <- ">> STEP: Cleanup Old Binaries"
	s.cleanupOldBinaries(5)
	s.logChan <- "   Keeping only the 5 most recent binaries"
	s.logChan <- "EOF"
}

// New creates a new Deployer instance from the given parameters.
func New(params ConfigParams) (*Deployer, error) {
	cfg, err := newConfig(params)
	if err != nil {
		return nil, err
	}

	mkdirAll(cfg)

	if cfg.githubRepo != "" {
		go initRepo(cfg)
	}
	if cfg.frontendGitURL != "" {
		go initFrontendRepo(cfg)
	}

	s := &Deployer{
		cfg:             cfg,
		startTime:       time.Now(),
		logChan:         make(chan string, 100),
		frontendLogChan: make(chan string, 100),
	}

	go s.heartbeatLoop()

	return s, nil
}

// heartbeatLoop periodically updates and saves instance metadata to disk
// This runs in a background goroutine and updates the metadata file every 3 seconds
func (s *Deployer)heartbeatLoop() {
	myPid := os.Getpid()
	version := "unknown"
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, st := range bi.Settings {
			if st.Key == "vcs.revision" {
				version = st.Value
				if len(version) > 7 {
					version = version[:7]
				}
			}
		}
	}
	metaFile := filepath.Join(s.cfg.metadataDir, fmt.Sprintf("%d.json", myPid))
	for {
		_ = os.MkdirAll(s.cfg.metadataDir, 0755)
		info := instanceInfo{
			Pid:        myPid,
			Version:    version,
			Uptime:     time.Since(s.startTime).Round(time.Second).String(),
			DeployedAt: s.startTime.Format(time.DateTime),
		}
		data, _ := json.Marshal(info)
		_ = os.WriteFile(metaFile, data, 0644)
		time.Sleep(3 * time.Second)
	}
}

// Status returns all active instances listening on the configured port
// It also cleans up metadata files for dead processes
func (s *Deployer)status(w http.ResponseWriter, r *http.Request) {
	myPid := os.Getpid()

	// Get all active PIDs listening on the configured port
	out, _ := exec.Command("sh", "-c", fmt.Sprintf("lsof -t -i:%d -sTCP:LISTEN", s.cfg.port)).Output()
	activePids := strings.Fields(string(out))
	pidMap := make(map[string]bool)
	for _, p := range activePids {
		pidMap[p] = true
	}

	var instances []instanceInfo
	files, _ := os.ReadDir(s.cfg.metadataDir)

	for _, f := range files {
		path := filepath.Join(s.cfg.metadataDir, f.Name())
		pidStr := strings.TrimSuffix(f.Name(), ".json")

		// Skip non-PID files (e.g., frontend-build.json)
		if _, err := strconv.Atoi(pidStr); err != nil {
			continue // Not a PID file, skip it
		}

		// Check if this PID is in the active list
		// Special case: treat current process as alive even if not yet in lsof output
		if !pidMap[pidStr] && pidStr != strconv.Itoa(myPid) {
			_ = os.Remove(path) // Process no longer exists, remove stale file
			continue
		}

		// Read and parse metadata file
		if data, err := os.ReadFile(path); err == nil {
			var info instanceInfo
			if err := json.Unmarshal(data, &info); err == nil {
				instances = append(instances, info)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"instances":   instances,
		"myPid":       myPid,
		"repoUrl":     s.cfg.githubRepo,
		"projectName": s.cfg.projectName,
		"port":        s.cfg.port,
		"binaryURL":   s.cfg.binaryURL,
	})
}

func (s *Deployer) gitBranches(w http.ResponseWriter, r *http.Request) {
	branches, err := fetchGitBranches(s.cfg.sourceDir)
	if err != nil {
		http.Error(w, "Git fetch failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(branches)
}

func (s *Deployer) gitCommits(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(fetchGitCommits(s.cfg.sourceDir, r.URL.Query().Get("branch")))
}

// cleanupOldBinaries removes old binary files, keeping only the most recent N versions
// This prevents disk space waste from accumulating historical binaries
func (s *Deployer)cleanupOldBinaries(keepCount int) {
	files, err := os.ReadDir(s.cfg.targetBase)
	if err != nil {
		return
	}

	// Filter and collect binary files with their modification time
	type fileInfo struct {
		name    string
		modTime time.Time
	}
	var binaries []fileInfo

	for _, f := range files {
		if f.IsDir() {
			continue
		}
		// Only consider files that match the project name pattern
		if strings.HasPrefix(f.Name(), s.cfg.projectName+"-") {
			info, err := f.Info()
			if err != nil {
				continue
			}
			binaries = append(binaries, fileInfo{
				name:    f.Name(),
				modTime: info.ModTime(),
			})
		}
	}

	// If we have fewer binaries than keepCount, no need to clean up
	if len(binaries) <= keepCount {
		return
	}

	// Sort by modification time (newest first)
	for i := 0; i < len(binaries)-1; i++ {
		for j := i + 1; j < len(binaries); j++ {
			if binaries[i].modTime.Before(binaries[j].modTime) {
				binaries[i], binaries[j] = binaries[j], binaries[i]
			}
		}
	}

	// Remove old binaries (keep only the most recent N)
	for i := keepCount; i < len(binaries); i++ {
		oldPath := filepath.Join(s.cfg.targetBase, binaries[i].name)
		_ = os.Remove(oldPath)
		_ = binaries[i].name
	}
}

// deployFromURL starts a new deployment by downloading a pre-built binary from the configured BinaryURL
// Supports any HTTP/HTTPS URL including S3 pre-signed URLs
func (s *Deployer) deployFromURL(w http.ResponseWriter, r *http.Request) {
	if s.cfg.binaryURL == "" {
		http.Error(w, "BinaryURL not configured", http.StatusNotFound)
		return
	}
	if !s.deployMu.TryLock() {
		http.Error(w, "A deployment is already in progress", http.StatusConflict)
		return
	}

	downloadURL := s.cfg.binaryURL
	branch := r.URL.Query().Get("branch")
	commit := r.URL.Query().Get("commit")
	downloadURL = strings.ReplaceAll(downloadURL, "{branch}", branch)
	downloadURL = strings.ReplaceAll(downloadURL, "{commit}", commit)

	// Use commit in binName if available, otherwise use timestamp
	var binName string
	if commit != "" {
		binName = fmt.Sprintf("%s-url-%s", s.cfg.projectName, commit)
	} else {
		binName = fmt.Sprintf("%s-url-%d", s.cfg.projectName, time.Now().UnixMilli())
	}
	targetPath, _ := filepath.Abs(filepath.Join(s.cfg.targetBase, binName))
	mkdirAll(s.cfg)

	go func() {
		defer s.deployMu.Unlock()
		s.logChan <- fmt.Sprintf("[Go-Cluster] %s Ready to deploy from URL...", time.Now().Format(time.DateTime))

		start := time.Now()
		s.logChan <- ">> STEP: Download Binary"
		s.logChan <- "   URL: " + downloadURL
		s.logChan <- "   Target: " + targetPath
		cmd := exec.Command("curl", "-fSL", "--progress-bar", "-o", targetPath, downloadURL)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			s.logChan <- "ERR:" + err.Error()
			return
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			s.logChan <- "ERR:" + err.Error()
			return
		}
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			s.logChan <- "   " + sc.Text()
		}
		if err := cmd.Wait(); err != nil {
			s.logChan <- "ERR: Download failed: " + err.Error()
			return
		}
		s.logChan <- fmt.Sprintf("   SUCCESS (%v)", time.Since(start).Round(time.Millisecond))

		s.startBinary(targetPath)
	}()
	_, _ = w.Write([]byte("OK"))
}

// Deploy starts a new deployment for the specified branch and commit
// The deployment runs asynchronously and streams logs to the logChan
func (s *Deployer) deploy(w http.ResponseWriter, r *http.Request) {
	if !s.deployMu.TryLock() {
		http.Error(w, "A deployment is already in progress", http.StatusConflict)
		return
	}

	branch, commit := r.URL.Query().Get("branch"), r.URL.Query().Get("commit")
	binName := fmt.Sprintf("%s-%s", s.cfg.projectName, commit)
	targetPath, _ := filepath.Abs(filepath.Join(s.cfg.targetBase, binName))
	mkdirAll(s.cfg)

	go func() {
		defer s.deployMu.Unlock()

		fInfo, err := os.Stat(filepath.Join(s.cfg.sourceDir, s.cfg.buildEntry))
		if err != nil {
			s.logChan <- "ERR: Build entry path error: " + err.Error()
			return
		}

		var buildWorkingDir string
		if fInfo.IsDir() {
			buildWorkingDir = filepath.Join(s.cfg.sourceDir, s.cfg.buildEntry)
		} else {
			buildWorkingDir = filepath.Dir(filepath.Join(s.cfg.sourceDir, s.cfg.buildEntry))
		}

		steps := []struct{ n, c, desc, workingDir string }{
			{n: "Sync Source",
				c:          fmt.Sprintf("git fetch --all && git checkout -f %s && git reset --hard %s", branch, commit),
				desc:       s.cfg.githubRepo,
				workingDir: s.cfg.sourceDir,
			},
			{n: "Build Binary",
				c:          fmt.Sprintf(`go build -ldflags "-s -w" -trimpath -o %s ./`, targetPath),
				desc:       targetPath,
				workingDir: buildWorkingDir,
			},
		}
		s.logChan <- fmt.Sprintf("[Go-Cluster] %s Ready to deploy...", time.Now().Format(time.DateTime))

		for _, step := range steps {
			start := time.Now()
			s.logChan <- ">> STEP: " + step.n + ": " + step.desc
			s.logChan <- step.c
			cmd := exec.Command("sh", "-c", step.c)
			cmd.Dir = step.workingDir
			s.logChan <- "   Working Dir: " + cmd.Dir
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				s.logChan <- "ERR:" + err.Error()
				return
			}
			cmd.Stderr = cmd.Stdout
			if err := cmd.Start(); err != nil {
				s.logChan <- "ERR:" + err.Error()
				return
			}
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				s.logChan <- "   " + sc.Text()
			}
			if err := cmd.Wait(); err != nil {
				s.logChan <- "ERR:" + err.Error()
				return
			}
			s.logChan <- fmt.Sprintf("   SUCCESS (%v)", time.Since(start).Round(time.Millisecond))
		}

		s.startBinary(targetPath)
	}()
	_, _ = w.Write([]byte("OK"))
}

func (s *Deployer) deployLogs(w http.ResponseWriter, r *http.Request) {
	streamLogs(w, s.logChan)
}

// Kill terminates a specified instance by PID
// Prevents killing the last running instance to maintain service availability
func (s *Deployer) kill(w http.ResponseWriter, r *http.Request) {
	pid, _ := strconv.Atoi(r.URL.Query().Get("pid"))
	// Unix 下端口上通常有多个 listener(SO_REUSEPORT);Windows 只有 1 个。
	// 保留"last instance standing"保护:不让用户把唯一的实例 kill 掉。
	if countPortListeners(s.cfg.port) <= 1 {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Action Denied: Last instance standing."))
		return
	}

	// Permission check: only allow killing PIDs managed by this cluster
	infoPath := filepath.Join(s.cfg.metadataDir, fmt.Sprintf("%d.json", pid))
	if _, err := os.Stat(infoPath); os.IsNotExist(err) {
		http.Error(w, "Permission Denied: PID not managed by cluster.", http.StatusForbidden)
		return
	}
	_ = os.Remove(filepath.Join(s.cfg.metadataDir, fmt.Sprintf("%d.json", pid)))
	_, _ = w.Write([]byte("OK"))
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = killManagedPID(pid)
	}()
}

func (s *Deployer) indexPage(w http.ResponseWriter, r *http.Request) {
	data, err := embedFS.ReadFile("index.html")
	if err != nil {
		http.Error(w, "Page not found", http.StatusNotFound)
		return
	}
	tmpl, err := template.New("index").Parse(string(data))
	if err != nil {
		http.Error(w, "Template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.Execute(w, map[string]bool{
		"ShowFrontend": s.cfg.frontendGitURL != "",
	})
}

// HomeBackend serves the backend deployment dashboard HTML page
func (s *Deployer)homeBackend(w http.ResponseWriter, r *http.Request) {
	if s.cfg.githubRepo != "" {
		initRepo(s.cfg)
	}
	data, err := embedFS.ReadFile("backend.html")
	if err != nil {
		http.Error(w, "Page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// HomeFrontend serves the frontend build dashboard HTML page
func (s *Deployer)homeFrontend(w http.ResponseWriter, r *http.Request) {
	if s.cfg.frontendGitURL == "" {
		http.Error(w, "Frontend repository not configured", http.StatusNotFound)
		return
	}
	initFrontendRepo(s.cfg)
	data, err := embedFS.ReadFile("frontend.html")
	if err != nil {
		http.Error(w, "Page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// Cleanup removes the instance metadata file for this process
func (s *Deployer)Cleanup() {
	myPid := os.Getpid()
	_ = os.Remove(filepath.Join(s.cfg.metadataDir, fmt.Sprintf("%d.json", myPid)))
}

// Mount registers all deployer routes to the given router with optional pre-handlers
// router: HTTP router that implements Router interface (e.g., http.ServeMux, chi.Router, gin.Engine)
// preHandlers: middleware functions executed before each route handler (auth, logging, etc.)
func (s *Deployer)Mount(router Router, preHandlers ...PreHandler) {
	base := s.cfg.basePath

	// Register navigation homepage
	handleFunc(router, fmt.Sprintf("GET %s/", base), s.indexPage, preHandlers...)

	// Mount backend and frontend routes
	s.mountBackend(router, base, preHandlers...)
	if s.cfg.frontendGitURL != "" {
		s.mountFrontend(router, base, preHandlers...)
	}
}

// mountBackend registers backend deployment routes
func (s *Deployer)mountBackend(router Router, base string, preHandlers ...PreHandler) {
	handleFunc(router, fmt.Sprintf("GET %s/backend/", base), s.homeBackend, preHandlers...)
	handleFunc(router, fmt.Sprintf("GET %s/backend/status", base), s.status, preHandlers...)
	handleFunc(router, fmt.Sprintf("GET %s/backend/deploy/logs", base), s.deployLogs, preHandlers...)
	handleFunc(router, fmt.Sprintf("POST %s/backend/kill", base), s.kill, preHandlers...)
	if s.cfg.githubRepo != "" {
		handleFunc(router, fmt.Sprintf("GET %s/backend/git/branches", base), s.gitBranches, preHandlers...)
		handleFunc(router, fmt.Sprintf("GET %s/backend/git/commits", base), s.gitCommits, preHandlers...)
		handleFunc(router, fmt.Sprintf("POST %s/backend/deploy", base), s.deploy, preHandlers...)
	}
	if s.cfg.binaryURL != "" {
		handleFunc(router, fmt.Sprintf("POST %s/backend/deploy-url", base), s.deployFromURL, preHandlers...)
	}
}

// mountFrontend registers frontend build routes
func (s *Deployer)mountFrontend(router Router, base string, preHandlers ...PreHandler) {
	handleFunc(router, fmt.Sprintf("GET %s/frontend/", base), s.homeFrontend, preHandlers...)
	handleFunc(router, fmt.Sprintf("GET %s/frontend/status", base), s.statusFrontend, preHandlers...)
	handleFunc(router, fmt.Sprintf("GET %s/frontend/git/branches", base), s.gitBranchesFrontend, preHandlers...)
	handleFunc(router, fmt.Sprintf("GET %s/frontend/git/commits", base), s.gitCommitsFrontend, preHandlers...)
	handleFunc(router, fmt.Sprintf("POST %s/frontend/build", base), s.buildFrontend, preHandlers...)
	handleFunc(router, fmt.Sprintf("GET %s/frontend/build/logs", base), s.buildLogs, preHandlers...)
}

// Router is an interface for HTTP routers that support route registration
// Compatible with http.ServeMux, chi.Router, gin.Engine, etc.
type Router interface {
	HandleFunc(pattern string, handler func(w http.ResponseWriter, r *http.Request))
}

func handleFunc(router Router, pattern string, handler func(w http.ResponseWriter, r *http.Request), preHandlers ...PreHandler) {

	router.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		newR := r
		for _, pre := range preHandlers {
			var next bool
			newR, next = pre(w, newR)
			if !next {
				return
			}
		}
		handler(w, newR)
	})
}
