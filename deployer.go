package deployer

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vito-go/mylog"
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
	GithubRepo     string // Git repository URL (required)
	Env            string // Environment name like "production", "staging" (required)
	BuildEntry     string // Build entry point like "./cmd/app" or "." (required)
	AppArgs        string // Additional arguments to pass when starting the application (optional)
	Port           uint   // Port of the deployed application (required, must support SO_REUSEPORT for zero-downtime)
	BasePath       string // Base path for routes like "/deploy" or "/admin/deploy" (required)
	FrontendGitURL string // Frontend repository Git URL (optional, empty = disabled)
}

// Config encapsulates global configuration for the deployer
type Config struct {
	ProjectName     string
	BuildEntry      string    // e.g. cmd/cuti-api-go
	GithubRepo      string    // Git repository URL (supports both HTTPS and SSH)
	SourceDir       string    // Local directory for git repository clone
	TargetBase      string    // Base directory for compiled binaries
	WorkingDir      string    // Working directory for backend process
	AppArgs         string    // Additional arguments to pass when starting the application
	MetadataDir     string    // Instance metadata directory for JSON files
	Port            uint      // Port of the deployed application (NOT the deployer itself) - used for process detection and instance management. Application must support SO_REUSEPORT for zero-downtime deployment.
	BasePath        string    // Base path for all routes (e.g., "/deploy" or "/admin/deploy")
	StartTime       time.Time // Server start time
	FrontendGitURL  string    // Frontend repository Git URL
	FrontendDir     string    // Local directory for frontend repository clone
	FrontendDistDir string    // Frontend build output directory (worker/dist)
}

// NewConfig creates a Config instance from the given parameters
// It automatically derives project name from the repository URL and sets up directory paths
func NewConfig(params ConfigParams) (*Config, error) {
	// Validate required fields
	if params.BuildEntry == "" {
		return nil, errors.New("build entry cannot be empty")
	}
	if params.GithubRepo == "" {
		return nil, errors.New("github repository URL cannot be empty")
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

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get user home directory: %w", err)
	}

	// Extract project name from repository URL
	ss := strings.Split(params.GithubRepo, "/")
	if len(ss) == 0 {
		return nil, fmt.Errorf("invalid repository URL: %s", params.GithubRepo)
	}
	projectName := strings.TrimSuffix(ss[len(ss)-1], ".git")

	// Normalize BasePath: ensure it starts with / and doesn't end with /
	basePath := params.BasePath
	if !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}
	basePath = strings.TrimSuffix(basePath, "/")

	// Create base directory
	baseDir := filepath.Join(home, "."+projectName, params.Env)
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base directory %s: %w", baseDir, err)
	}

	return &Config{
		ProjectName:     projectName,
		BuildEntry:      params.BuildEntry,
		GithubRepo:      params.GithubRepo,
		SourceDir:       filepath.Join(baseDir, "repo"),
		TargetBase:      filepath.Join(baseDir, "bin"),
		WorkingDir:      filepath.Join(baseDir, "worker"),
		MetadataDir:     filepath.Join(baseDir, "metadata"),
		AppArgs:         params.AppArgs,
		Port:            params.Port,
		BasePath:        basePath,
		StartTime:       time.Now(),
		FrontendGitURL:  params.FrontendGitURL,
		FrontendDir:     filepath.Join(baseDir, "frontend-repo"),
		FrontendDistDir: filepath.Join(baseDir, "worker", "dist"),
	}, nil
}

// InstanceInfo represents runtime information about a deployed instance
type InstanceInfo struct {
	Pid        int    `json:"pid"`        // Process ID
	Version    string `json:"version"`    // Git commit hash (short)
	Uptime     string `json:"uptime"`     // Time elapsed since deployment
	DeployedAt string `json:"deployedAt"` // Deployment timestamp
}

// Deployer manages application deployment and running instances
type Deployer struct {
	cfg             *Config
	logChan         chan string
	frontendLogChan chan string
}

// initRepo initializes the git repository by cloning if it doesn't exist
func initRepo(cfg *Config) {
	absSource, _ := filepath.Abs(cfg.SourceDir)
	if _, err := os.Stat(filepath.Join(absSource, ".git")); err != nil {
		mylog.Printf("[INIT] Repo not found. Cloning: %s -> %s", cfg.GithubRepo, absSource)
		out, err := exec.Command("git", "clone", cfg.GithubRepo, absSource).CombinedOutput()
		if err != nil {
			mylog.Ctx(context.Background()).Warnf("[INIT] Clone failed: %v, output: %s", err, string(out))
		}
	}
}
func mkdirAll(cfg *Config) {
	// Create necessary directories
	_ = os.MkdirAll(cfg.MetadataDir, 0755)
	_ = os.MkdirAll(cfg.TargetBase, 0755)
	_ = os.MkdirAll(cfg.WorkingDir, 0755)
	_ = os.MkdirAll(cfg.SourceDir, 0755)
	_ = os.MkdirAll(cfg.FrontendDir, 0755)
	_ = os.MkdirAll(cfg.FrontendDistDir, 0755)
}

// NewDeployer creates a new Deployer instance with the given config
func NewDeployer(cfg *Config) *Deployer {
	if cfg == nil {
		panic("deployer config cannot be nil")
	}

	// Create necessary directories
	mkdirAll(cfg)

	// Initialize source repository asynchronously
	go initRepo(cfg)
	go initFrontendRepo(cfg)

	s := &Deployer{
		cfg:             cfg,
		logChan:         make(chan string, 100),
		frontendLogChan: make(chan string, 100),
	}

	// Start heartbeat goroutine to maintain instance metadata
	go s.heartbeatLoop()

	return s
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
	metaFile := filepath.Join(s.cfg.MetadataDir, fmt.Sprintf("%d.json", myPid))
	for {
		// Ensure metadata directory exists (in case it was deleted)
		_ = os.MkdirAll(s.cfg.MetadataDir, 0755)
		info := InstanceInfo{
			Pid:        myPid,
			Version:    version,
			Uptime:     time.Since(s.cfg.StartTime).Round(time.Second).String(),
			DeployedAt: s.cfg.StartTime.Format(time.DateTime),
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
	out, _ := exec.Command("sh", "-c", fmt.Sprintf("lsof -t -i:%d -sTCP:LISTEN", s.cfg.Port)).Output()
	activePids := strings.Fields(string(out))
	pidMap := make(map[string]bool)
	for _, p := range activePids {
		pidMap[p] = true
	}

	var instances []InstanceInfo
	files, _ := os.ReadDir(s.cfg.MetadataDir)

	for _, f := range files {
		path := filepath.Join(s.cfg.MetadataDir, f.Name())
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
			var info InstanceInfo
			if err := json.Unmarshal(data, &info); err == nil {
				instances = append(instances, info)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"instances":   instances,
		"myPid":       myPid,
		"repoUrl":     s.cfg.GithubRepo,
		"projectName": s.cfg.ProjectName,
		"port":        s.cfg.Port,
	})
}

// GitBranches returns the list of available Git branches from remote origin
func (s *Deployer)gitBranches(w http.ResponseWriter, r *http.Request) {
	_ = exec.Command("git", "-C", s.cfg.SourceDir, "fetch", "--all").Run()
	//git fetch --all --prune
	err := exec.Command("git", "-C", s.cfg.SourceDir, "fetch", "--all", "--prune").Run()
	if err != nil {
		mylog.Printf("[GIT] Fetch prune failed: %v", err)
		http.Error(w, "Git fetch failed", http.StatusInternalServerError)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	//out, _ := exec.Command("git", "-C", s.cfg.SourceDir, "for-each-ref", "--format=%(refname:lstrip=3)", "refs/remotes/origin/").Output()
	out, _ := exec.Command("git", "-C", s.cfg.SourceDir, "branch", "-r").Output()
	branches := []string{}

	for _, line := range strings.Split(string(out), "\n") {
		b := strings.TrimSpace(line)
		ss := strings.SplitN(b, "/", 2)
		if len(ss) != 2 {
			continue
		}
		b = ss[1]
		if strings.Contains(b, "->") {
			continue
		}
		if strings.HasPrefix(b, "HEAD") {
			continue
		}
		if b != "" && b != "HEAD" {
			branches = append(branches, b)
		}
	}
	// 把main或 master 放到最前面
	var branchNames []string
	for _, b := range branches {
		if b == "main" || b == "master" {
			branchNames = append([]string{b}, branchNames...)
		} else {
			branchNames = append(branchNames, b)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(branchNames)
}

// GitCommits returns the recent commit history for a specific branch
func (s *Deployer)gitCommits(w http.ResponseWriter, r *http.Request) {
	branch := r.URL.Query().Get("branch")
	out, _ := exec.Command("git", "-C", s.cfg.SourceDir, "log", "origin/"+branch, "-n", "10", "--pretty=format:%h|%an|%ar|%s").Output()
	var commits []map[string]string
	for _, line := range strings.Split(string(out), "\n") {
		if p := strings.Split(line, "|"); len(p) >= 4 {
			commits = append(commits, map[string]string{"hash": p[0], "author": p[1], "date": p[2], "msg": p[3]})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(commits)
}

// cleanupOldBinaries removes old binary files, keeping only the most recent N versions
// This prevents disk space waste from accumulating historical binaries
func (s *Deployer)cleanupOldBinaries(keepCount int) {
	files, err := os.ReadDir(s.cfg.TargetBase)
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
		if strings.HasPrefix(f.Name(), s.cfg.ProjectName+"-") {
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
		oldPath := filepath.Join(s.cfg.TargetBase, binaries[i].name)
		_ = os.Remove(oldPath)
		mylog.Printf("[CLEANUP] Removed old binary: %s", binaries[i].name)
	}
}

// Deploy starts a new deployment for the specified branch and commit
// The deployment runs asynchronously and streams logs to the logChan
func (s *Deployer)deploy(w http.ResponseWriter, r *http.Request) {
	branch, commit := r.URL.Query().Get("branch"), r.URL.Query().Get("commit")
	binName := fmt.Sprintf("%s-%s", s.cfg.ProjectName, commit)
	targetPath, _ := filepath.Abs(filepath.Join(s.cfg.TargetBase, binName))
	mkdirAll(s.cfg)

	go func() {
		// Determine build entry: use "." if empty, otherwise use the specified path
		fInfo, err := os.Stat(filepath.Join(s.cfg.SourceDir, s.cfg.BuildEntry))
		if err != nil {
			s.logChan <- "ERR: Build entry path error: " + err.Error()
			return
		}

		var buildWorkingDir string
		if fInfo.IsDir() {
			buildWorkingDir = filepath.Join(s.cfg.SourceDir, s.cfg.BuildEntry)
		} else {
			buildWorkingDir = filepath.Dir(filepath.Join(s.cfg.SourceDir, s.cfg.BuildEntry))
		}

		steps := []struct{ n, c, desc, workingDir string }{
			{n: "Sync Source",
				c: fmt.Sprintf("git fetch --all && git checkout -f %s && git reset --hard %s", branch, commit), desc: s.cfg.GithubRepo,

				workingDir: s.cfg.SourceDir,
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
			err = cmd.Start()
			if err != nil {
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

		// Ensure the binary has executable permissions
		s.logChan <- ">> STEP: Set Executable Permissions"
		if err := os.Chmod(targetPath, 0755); err != nil {
			s.logChan <- "ERR: Failed to set executable permissions: " + err.Error()
			return
		}
		s.logChan <- "   SUCCESS"

		fullCmd := fmt.Sprintf("nohup %s %s > /dev/null 2>&1 &", targetPath, s.cfg.AppArgs)
		newCmd := exec.Command("sh", "-c", fullCmd)
		newCmd.Dir = s.cfg.WorkingDir
		newCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		s.logChan <- ">> RUN: " + fullCmd
		if err := newCmd.Start(); err != nil {
			s.logChan <- "ERR: " + err.Error()
		} else {
			_ = newCmd.Wait()

			// Clean up old binaries (keep only the last 5 versions)
			s.logChan <- ">> STEP: Cleanup Old Binaries"
			s.cleanupOldBinaries(5)
			s.logChan <- "   Keeping only the 5 most recent binaries"

			s.logChan <- "EOF"
		}
	}()
	_, _ = w.Write([]byte("OK"))
}

// DeployLogs streams deployment logs to the client via Server-Sent Events (SSE)
func (s *Deployer)deployLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	f, _ := w.(http.Flusher)
	for msg := range s.logChan {
		mylog.Info(msg)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", msg)
		f.Flush()
		if strings.HasPrefix(msg, "EOF") || strings.HasPrefix(msg, "ERR") {
			break
		}
	}
}

// Kill terminates a specified instance by PID
// Prevents killing the last running instance to maintain service availability
func (s *Deployer)kill(w http.ResponseWriter, r *http.Request) {
	pid, _ := strconv.Atoi(r.URL.Query().Get("pid"))
	out, _ := exec.Command("sh", "-c", fmt.Sprintf("lsof -t -i:%d -sTCP:LISTEN", s.cfg.Port)).Output()
	if len(strings.Fields(string(out))) <= 1 {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Action Denied: Last instance standing."))
		return
	}

	// Permission check: only allow killing PIDs managed by this cluster
	infoPath := filepath.Join(s.cfg.MetadataDir, fmt.Sprintf("%d.json", pid))
	if _, err := os.Stat(infoPath); os.IsNotExist(err) {
		http.Error(w, "Permission Denied: PID not managed by cluster.", http.StatusForbidden)
		return
	}
	mylog.Printf("[SECURITY] Kill request for PID %d accepted from %s", pid, r.RemoteAddr)
	_ = os.Remove(filepath.Join(s.cfg.MetadataDir, fmt.Sprintf("%d.json", pid)))
	_, _ = w.Write([]byte("OK"))
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}()
}

// IndexPage serves the navigation homepage
func (s *Deployer)indexPage(w http.ResponseWriter, r *http.Request) {
	data, err := embedFS.ReadFile("index.html")
	if err != nil {
		http.Error(w, "Page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// HomeBackend serves the backend deployment dashboard HTML page
func (s *Deployer)homeBackend(w http.ResponseWriter, r *http.Request) {
	initRepo(s.cfg)
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
	if s.cfg.FrontendGitURL == "" {
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
	_ = os.Remove(filepath.Join(s.cfg.MetadataDir, fmt.Sprintf("%d.json", myPid)))
}

// Mount registers all deployer routes to the given router with optional pre-handlers
// router: HTTP router that implements Router interface (e.g., http.ServeMux, chi.Router, gin.Engine)
// preHandlers: middleware functions executed before each route handler (auth, logging, etc.)
func (s *Deployer)Mount(router Router, preHandlers ...PreHandler) {
	base := s.cfg.BasePath

	// Register navigation homepage
	handleFunc(router, fmt.Sprintf("GET %s/", base), s.indexPage, preHandlers...)

	// Mount backend and frontend routes
	s.mountBackend(router, base, preHandlers...)
	if s.cfg.FrontendGitURL != "" {
		s.mountFrontend(router, base, preHandlers...)
	}
}

// mountBackend registers backend deployment routes
func (s *Deployer)mountBackend(router Router, base string, preHandlers ...PreHandler) {
	handleFunc(router, fmt.Sprintf("GET %s/backend/", base), s.homeBackend, preHandlers...)
	handleFunc(router, fmt.Sprintf("GET %s/backend/status", base), s.status, preHandlers...)
	handleFunc(router, fmt.Sprintf("GET %s/backend/git/branches", base), s.gitBranches, preHandlers...)
	handleFunc(router, fmt.Sprintf("GET %s/backend/git/commits", base), s.gitCommits, preHandlers...)
	handleFunc(router, fmt.Sprintf("POST %s/backend/deploy", base), s.deploy, preHandlers...)
	handleFunc(router, fmt.Sprintf("GET %s/backend/deploy/logs", base), s.deployLogs, preHandlers...)
	handleFunc(router, fmt.Sprintf("POST %s/backend/kill", base), s.kill, preHandlers...)
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
