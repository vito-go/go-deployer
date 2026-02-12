package main

import (
	"log"
	"net/http"

	"github.com/vito-go/go-deployer"
)

func main() {
	// Create deployment configuration
	cfg, err := deployer.NewConfig(deployer.ConfigParams{
		GithubRepo:     "git@github.com:yourorg/backend.git",
		Env:            "production",
		BuildEntry:     "./cmd/app",
		AppArgs:        "",
		Port:           8080,
		BasePath:       "/deploy",
		FrontendGitURL: "", // Empty = frontend disabled
	})
	if err != nil {
		log.Fatal("Config error:", err)
	}

	// Create and mount deployer
	dep := deployer.NewDeployer(cfg)

	// Create HTTP router and mount routes
	mux := http.NewServeMux()
	dep.Mount(mux)

	// Start server
	log.Printf("Deployment console available at: http://localhost:8080%s/", cfg.BasePath)
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatal("Server error:", err)
	}
}
