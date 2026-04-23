package main

import (
	"log"
	"net/http"

	"github.com/vito-go/go-deployer"
)

func main() {
	dep, err := deployer.New(deployer.ConfigParams{
		GithubRepo: "git@github.com:yourorg/backend.git",
		Env:        "production",
		BuildEntry: "./cmd/app",
		Port:       8080,
		BasePath:   "/deploy",
	})
	if err != nil {
		log.Fatal("Config error:", err)
	}

	mux := http.NewServeMux()
	dep.Mount(mux)

	log.Printf("Deployment console available at: http://localhost:8080/deploy/")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatal("Server error:", err)
	}
}
