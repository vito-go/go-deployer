package deployer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vito-go/mylog"
)

// FrontendBuildInfo represents information about a frontend build
type FrontendBuildInfo struct {
	Version  string `json:"version"`  // Git commit hash (short)
	Branch   string `json:"branch"`   // Branch name
	BuiltAt  string `json:"builtAt"`  // Build timestamp
	BuildDir string `json:"buildDir"` // Build output directory (e.g., "dist")
}

// initFrontendRepo initializes the frontend git repository by cloning if it doesn't exist
func initFrontendRepo(cfg *Config) {
	if cfg.FrontendGitURL == "" {
		return // Frontend not configured
	}
	absFrontendDir, _ := filepath.Abs(cfg.FrontendDir)
	if _, err := os.Stat(filepath.Join(absFrontendDir, ".git")); err != nil {
		mylog.Printf("[FRONTEND] Repo not found. Cloning: %s -> %s", cfg.FrontendGitURL, absFrontendDir)
		out, err := exec.Command("git", "clone", cfg.FrontendGitURL, absFrontendDir).CombinedOutput()
		if err != nil {
			mylog.Ctx(context.Background()).Warnf("[FRONTEND] Clone failed: %v, output: %s", err, string(out))
		}
	}
}

// GitBranchesFrontend returns the list of available Git branches from the frontend repository
func (s *Deployer)gitBranchesFrontend(w http.ResponseWriter, r *http.Request) {
	if s.cfg.FrontendGitURL == "" {
		http.Error(w, "Frontend repository not configured", http.StatusNotFound)
		return
	}

	_ = exec.Command("git", "-C", s.cfg.FrontendDir, "fetch", "--all").Run()
	err := exec.Command("git", "-C", s.cfg.FrontendDir, "fetch", "--all", "--prune").Run()
	if err != nil {
		mylog.Printf("[FRONTEND] Fetch prune failed: %v", err)
		http.Error(w, "Git fetch failed", http.StatusInternalServerError)
		return
	}

	out, _ := exec.Command("git", "-C", s.cfg.FrontendDir, "branch", "-r").Output()
	branches := []string{}

	for _, line := range strings.Split(string(out), "\n") {
		b := strings.TrimSpace(line)
		ss := strings.SplitN(b, "/", 2)
		if len(ss) != 2 {
			continue
		}
		b = ss[1]
		if strings.Contains(b, "->") || strings.HasPrefix(b, "HEAD") {
			continue
		}
		if b != "" && b != "HEAD" {
			branches = append(branches, b)
		}
	}

	// Put main or master at the front
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

// GitCommitsFrontend returns the recent commit history for a specific branch in the frontend repository
func (s *Deployer)gitCommitsFrontend(w http.ResponseWriter, r *http.Request) {
	if s.cfg.FrontendGitURL == "" {
		http.Error(w, "Frontend repository not configured", http.StatusNotFound)
		return
	}

	branch := r.URL.Query().Get("branch")
	out, _ := exec.Command("git", "-C", s.cfg.FrontendDir, "log", "origin/"+branch, "-n", "10", "--pretty=format:%h|%an|%ar|%s").Output()
	var commits []map[string]string
	for _, line := range strings.Split(string(out), "\n") {
		if p := strings.Split(line, "|"); len(p) >= 4 {
			commits = append(commits, map[string]string{"hash": p[0], "author": p[1], "date": p[2], "msg": p[3]})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(commits)
}

// StatusFrontend returns the current frontend build information
func (s *Deployer)statusFrontend(w http.ResponseWriter, r *http.Request) {
	if s.cfg.FrontendGitURL == "" {
		http.Error(w, "Frontend repository not configured", http.StatusNotFound)
		return
	}

	// Read build info from metadata file
	buildInfoPath := filepath.Join(s.cfg.MetadataDir, "frontend-build.json")
	var buildInfo FrontendBuildInfo

	if data, err := os.ReadFile(buildInfoPath); err == nil {
		_ = json.Unmarshal(data, &buildInfo)
	} else {
		// No build info yet - try to get current Git info
		buildInfo = FrontendBuildInfo{
			Version:  "none",
			Branch:   "none",
			BuiltAt:  "never",
			BuildDir: "dist",
		}

		// Try to read current branch and commit from Git
		if _, err := os.Stat(filepath.Join(s.cfg.FrontendDir, ".git")); err == nil {
			// Get current branch
			if out, err := exec.Command("git", "-C", s.cfg.FrontendDir, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
				buildInfo.Branch = strings.TrimSpace(string(out))
			}
			// Get current commit (short hash)
			if out, err := exec.Command("git", "-C", s.cfg.FrontendDir, "rev-parse", "--short", "HEAD").Output(); err == nil {
				buildInfo.Version = strings.TrimSpace(string(out))
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"buildInfo":    buildInfo,
		"repoUrl":      s.cfg.FrontendGitURL,
		"projectName":  s.cfg.ProjectName,
		"targetDir":    s.cfg.FrontendDistDir,
	})
}

// BuildFrontend starts a new frontend build for the specified branch and commit
// The build runs asynchronously and streams logs to the frontendLogChan
func (s *Deployer)buildFrontend(w http.ResponseWriter, r *http.Request) {
	if s.cfg.FrontendGitURL == "" {
		http.Error(w, "Frontend repository not configured", http.StatusNotFound)
		return
	}

	branch, commit := r.URL.Query().Get("branch"), r.URL.Query().Get("commit")

	go func() {
		mkdirAll(s.cfg)

		steps := []struct{ n, c, desc, workingDir string }{
			{
				n:          "Sync Frontend Source",
				c:          fmt.Sprintf("git fetch --all && git checkout -f %s && git reset --hard %s", branch, commit),
				desc:       s.cfg.FrontendGitURL,
				workingDir: s.cfg.FrontendDir,
			},
			{
				n:          "Install Dependencies",
				c:          "npm install",
				desc:       "Installing npm packages",
				workingDir: s.cfg.FrontendDir,
			},
			{
				n:          "Build Frontend",
				c:          "npm run build",
				desc:       "Building frontend assets",
				workingDir: s.cfg.FrontendDir,
			},
		}

		s.frontendLogChan <- fmt.Sprintf("[Frontend Build] %s Ready to build...", time.Now().Format(time.DateTime))

		for _, step := range steps {
			start := time.Now()
			s.frontendLogChan <- ">> STEP: " + step.n + ": " + step.desc
			s.frontendLogChan <- step.c
			cmd := exec.Command("sh", "-c", step.c)
			cmd.Dir = step.workingDir
			s.frontendLogChan <- "   Working Dir: " + cmd.Dir
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				s.frontendLogChan <- "ERR:" + err.Error()
				return
			}
			cmd.Stderr = cmd.Stdout
			err = cmd.Start()
			if err != nil {
				s.frontendLogChan <- "ERR:" + err.Error()
				return
			}
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				s.frontendLogChan <- "   " + sc.Text()
			}
			if err := cmd.Wait(); err != nil {
				s.frontendLogChan <- "ERR:" + err.Error()
				return
			}
			s.frontendLogChan <- fmt.Sprintf("   SUCCESS (%v)", time.Since(start).Round(time.Millisecond))
		}

		// Deploy built files to worker/dist (atomic deployment)
		s.frontendLogChan <- ">> STEP: Deploy to worker/dist (atomic)"
		// Determine build output directory (commonly "dist" or "build")
		buildOutputDir := filepath.Join(s.cfg.FrontendDir, "dist")
		if _, err := os.Stat(buildOutputDir); os.IsNotExist(err) {
			// Try "build" directory
			buildOutputDir = filepath.Join(s.cfg.FrontendDir, "build")
		}

		distNew := s.cfg.FrontendDistDir + "-new"
		distOld := s.cfg.FrontendDistDir + "-old"

		// Step 1: Copy to temporary directory
		s.frontendLogChan <- "   Copying to temporary directory..."
		copyCmd := fmt.Sprintf("rm -rf %s && cp -r %s %s", distNew, buildOutputDir, distNew)
		cmd := exec.Command("sh", "-c", copyCmd)
		output, err := cmd.CombinedOutput()
		if err != nil {
			s.frontendLogChan <- "ERR:" + err.Error() + ", output: " + string(output)
			return
		}

		// Step 2: Atomic swap (mv is atomic on same filesystem)
		s.frontendLogChan <- "   Atomic swap..."
		var swapCmd string
		if _, err := os.Stat(s.cfg.FrontendDistDir); os.IsNotExist(err) {
			// First deployment: just rename dist-new to dist
			swapCmd = fmt.Sprintf("mv %s %s", distNew, s.cfg.FrontendDistDir)
		} else {
			// Subsequent deployments: swap with old version
			swapCmd = fmt.Sprintf("mv %s %s && mv %s %s", s.cfg.FrontendDistDir, distOld, distNew, s.cfg.FrontendDistDir)
		}
		cmd = exec.Command("sh", "-c", swapCmd)
		output, err = cmd.CombinedOutput()
		if err != nil {
			s.frontendLogChan <- "ERR:" + err.Error() + ", output: " + string(output)
			// Try to recover if dist-old exists
			if _, e := os.Stat(distOld); e == nil {
				_ = exec.Command("sh", "-c", fmt.Sprintf("mv %s %s", distOld, s.cfg.FrontendDistDir)).Run()
			}
			return
		}

		// Step 3: Clean up old version (async, non-blocking)
		s.frontendLogChan <- "   Cleaning up old version..."
		_ = exec.Command("sh", "-c", fmt.Sprintf("rm -rf %s", distOld)).Run()

		s.frontendLogChan <- "   SUCCESS (zero-downtime deployment)"

		// Save build info
		s.frontendLogChan <- ">> STEP: Save Build Info"
		buildInfo := FrontendBuildInfo{
			Version:  commit,
			Branch:   branch,
			BuiltAt:  time.Now().Format(time.DateTime),
			BuildDir: filepath.Base(buildOutputDir),
		}
		buildInfoPath := filepath.Join(s.cfg.MetadataDir, "frontend-build.json")
		data, _ := json.Marshal(buildInfo)
		if err := os.WriteFile(buildInfoPath, data, 0644); err != nil {
			s.frontendLogChan <- "ERR: Failed to save build info: " + err.Error()
			return
		}
		s.frontendLogChan <- "   SUCCESS"

		s.frontendLogChan <- "EOF"
	}()

	_, _ = w.Write([]byte("OK"))
}

// BuildLogs streams frontend build logs to the client via Server-Sent Events (SSE)
func (s *Deployer)buildLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	f, _ := w.(http.Flusher)
	for msg := range s.frontendLogChan {
		mylog.Info(msg)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", msg)
		f.Flush()
		if strings.HasPrefix(msg, "EOF") || strings.HasPrefix(msg, "ERR") {
			break
		}
	}
}
