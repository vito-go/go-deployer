package main

import (
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/vito-go/go-deployer"
)

// Logging middleware for audit trail
func loggingHandler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	log.Printf("[ACCESS] %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
	return r, true
}

// IP whitelist middleware
func ipWhitelistHandler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	// Extract IP (handle X-Forwarded-For in production)
	ip := r.RemoteAddr

	// Simple whitelist check (use proper CIDR matching in production)
	allowedIPs := []string{"127.0.0.1", "::1"}

	for _, allowedIP := range allowedIPs {
		if ip == allowedIP {
			return r, true
		}
	}

	log.Printf("[BLOCKED] Unauthorized IP: %s", ip)
	http.Error(w, "Access denied", http.StatusForbidden)
	return r, false
}

func main() {
	// Full-stack configuration with both backend and frontend
	cfg, err := deployer.NewConfig(deployer.ConfigParams{
		GithubRepo:     "git@github.com:yourorg/backend.git",
		Env:            "production",
		BuildEntry:     "./cmd/api",
		AppArgs:        "--port=8080 --verbose",
		Port:           8080,
		BasePath:       "/admin/deploy",
		FrontendGitURL: "git@github.com:yourorg/frontend.git",
	})
	if err != nil {
		log.Fatal("Config error:", err)
	}

	// Create deployer
	dep := deployer.NewDeployer(cfg)
	defer dep.Cleanup()

	// Create router and mount routes with multiple PreHandlers (logging + IP whitelist)
	mux := http.NewServeMux()
	dep.Mount(mux, loggingHandler, ipWhitelistHandler)

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down gracefully...")
		dep.Cleanup()
		os.Exit(0)
	}()

	log.Printf("🚀 Full-stack deployment console ready")
	log.Printf("   Backend deployment: http://localhost:8080%s/backend/", cfg.BasePath)
	log.Printf("   Frontend build:     http://localhost:8080%s/frontend/", cfg.BasePath)
	log.Printf("   Press Ctrl+C to stop")

	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatal("Server error:", err)
	}
}
