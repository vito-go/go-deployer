package main

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/vito-go/go-deployer"
)

// Simple authentication handler - checks for Bearer token
func authHandler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	auth := r.Header.Get("Authorization")

	// Check if Authorization header is present
	if auth == "" {
		http.Error(w, "Missing Authorization header", http.StatusUnauthorized)
		return r, false
	}

	// Validate Bearer token
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == "" || token == auth { // No "Bearer " prefix
		http.Error(w, "Invalid Authorization format", http.StatusUnauthorized)
		return r, false
	}

	// Simple token validation (replace with JWT validation in production)
	if token != "your-secret-token" {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return r, false
	}

	// Add user info to request context
	ctx := context.WithValue(r.Context(), "authenticated", true)
	newR := r.WithContext(ctx)

	log.Printf("Authenticated request: %s %s", r.Method, r.URL.Path)
	return newR, true
}

func main() {
	dep, err := deployer.New(deployer.ConfigParams{
		GithubRepo:     "git@github.com:yourorg/backend.git",
		Env:            "production",
		BuildEntry:     "./cmd/app",
		Port:           8080,
		BasePath:       "/deploy",
		FrontendGitURL: "git@github.com:yourorg/frontend.git",
	})
	if err != nil {
		log.Fatal("Config error:", err)
	}
	defer dep.Cleanup()

	mux := http.NewServeMux()
	dep.Mount(mux, authHandler)
	log.Printf("Deployment console with auth at: http://localhost:8080/deploy/")
	log.Println("Use: curl -H 'Authorization: Bearer your-secret-token' ...")

	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatal("Server error:", err)
	}
}
