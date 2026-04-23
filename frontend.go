package deployer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type frontendBuildInfo struct {
	Version  string `json:"version"`
	Branch   string `json:"branch"`
	BuiltAt  string `json:"builtAt"`
	BuildDir string `json:"buildDir"`
}

func initFrontendRepo(cfg *config) {
	if cfg.frontendGitURL == "" {
		return
	}
	absFrontendDir, _ := filepath.Abs(cfg.frontendDir)
	if _, err := os.Stat(filepath.Join(absFrontendDir, ".git")); err != nil {
		out, err := exec.Command("git", "clone", cfg.frontendGitURL, absFrontendDir).CombinedOutput()
		if err != nil {
			_ = out
		}
	}
}

func (s *Deployer) gitBranchesFrontend(w http.ResponseWriter, r *http.Request) {
	branches, err := fetchGitBranches(s.cfg.frontendDir)
	if err != nil {
		http.Error(w, "Git fetch failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(branches)
}

func (s *Deployer) gitCommitsFrontend(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(fetchGitCommits(s.cfg.frontendDir, r.URL.Query().Get("branch")))
}

// StatusFrontend returns the current frontend build information
func (s *Deployer)statusFrontend(w http.ResponseWriter, r *http.Request) {
	if s.cfg.frontendGitURL == "" {
		http.Error(w, "Frontend repository not configured", http.StatusNotFound)
		return
	}

	// Read build info from metadata file
	buildInfoPath := filepath.Join(s.cfg.metadataDir, "frontend-build.json")
	var buildInfo frontendBuildInfo

	if data, err := os.ReadFile(buildInfoPath); err == nil {
		_ = json.Unmarshal(data, &buildInfo)
	} else {
		// No build info yet - try to get current Git info
		buildInfo = frontendBuildInfo{
			Version:  "none",
			Branch:   "none",
			BuiltAt:  "never",
			BuildDir: "dist",
		}

		// Try to read current branch and commit from Git
		if _, err := os.Stat(filepath.Join(s.cfg.frontendDir, ".git")); err == nil {
			// Get current branch
			if out, err := exec.Command("git", "-C", s.cfg.frontendDir, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
				buildInfo.Branch = strings.TrimSpace(string(out))
			}
			// Get current commit (short hash)
			if out, err := exec.Command("git", "-C", s.cfg.frontendDir, "rev-parse", "--short", "HEAD").Output(); err == nil {
				buildInfo.Version = strings.TrimSpace(string(out))
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"buildInfo":    buildInfo,
		"repoUrl":      s.cfg.frontendGitURL,
		"projectName":  s.cfg.projectName,
		"targetDir":    s.cfg.frontendDistDir,
	})
}

// BuildFrontend starts a new frontend build for the specified branch and commit
// The build runs asynchronously and streams logs to the frontendLogChan
func (s *Deployer) buildFrontend(w http.ResponseWriter, r *http.Request) {
	if s.cfg.frontendGitURL == "" {
		http.Error(w, "Frontend repository not configured", http.StatusNotFound)
		return
	}
	if !s.frontendMu.TryLock() {
		http.Error(w, "A frontend build is already in progress", http.StatusConflict)
		return
	}

	branch, commit := r.URL.Query().Get("branch"), r.URL.Query().Get("commit")

	go func() {
		defer s.frontendMu.Unlock()
		mkdirAll(s.cfg)

		steps := []struct{ n, c, desc, workingDir string }{
			{
				n:          "Sync Frontend Source",
				c:          fmt.Sprintf("git fetch --all && git checkout -f %s && git reset --hard %s", branch, commit),
				desc:       s.cfg.frontendGitURL,
				workingDir: s.cfg.frontendDir,
			},
			{
				n:          "Install Dependencies",
				c:          "npm install",
				desc:       "Installing npm packages",
				workingDir: s.cfg.frontendDir,
			},
			{
				n:          "Build Frontend",
				c:          "npm run build",
				desc:       "Building frontend assets",
				workingDir: s.cfg.frontendDir,
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
		buildOutputDir := filepath.Join(s.cfg.frontendDir, "dist")
		if _, err := os.Stat(buildOutputDir); os.IsNotExist(err) {
			// Try "build" directory
			buildOutputDir = filepath.Join(s.cfg.frontendDir, "build")
		}

		distNew := s.cfg.frontendDistDir + "-new"
		distOld := s.cfg.frontendDistDir + "-old"

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
		if _, err := os.Stat(s.cfg.frontendDistDir); os.IsNotExist(err) {
			// First deployment: just rename dist-new to dist
			swapCmd = fmt.Sprintf("mv %s %s", distNew, s.cfg.frontendDistDir)
		} else {
			// Subsequent deployments: swap with old version
			swapCmd = fmt.Sprintf("mv %s %s && mv %s %s", s.cfg.frontendDistDir, distOld, distNew, s.cfg.frontendDistDir)
		}
		cmd = exec.Command("sh", "-c", swapCmd)
		output, err = cmd.CombinedOutput()
		if err != nil {
			s.frontendLogChan <- "ERR:" + err.Error() + ", output: " + string(output)
			// Try to recover if dist-old exists
			if _, e := os.Stat(distOld); e == nil {
				_ = exec.Command("sh", "-c", fmt.Sprintf("mv %s %s", distOld, s.cfg.frontendDistDir)).Run()
			}
			return
		}

		// Step 3: Clean up old version (async, non-blocking)
		s.frontendLogChan <- "   Cleaning up old version..."
		_ = exec.Command("sh", "-c", fmt.Sprintf("rm -rf %s", distOld)).Run()

		s.frontendLogChan <- "   SUCCESS (zero-downtime deployment)"

		// Save build info
		s.frontendLogChan <- ">> STEP: Save Build Info"
		buildInfo := frontendBuildInfo{
			Version:  commit,
			Branch:   branch,
			BuiltAt:  time.Now().Format(time.DateTime),
			BuildDir: filepath.Base(buildOutputDir),
		}
		buildInfoPath := filepath.Join(s.cfg.metadataDir, "frontend-build.json")
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

func (s *Deployer) buildLogs(w http.ResponseWriter, r *http.Request) {
	streamLogs(w, s.frontendLogChan)
}
