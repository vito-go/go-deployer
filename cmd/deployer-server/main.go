package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"github.com/vito-go/go-deployer/controlplane"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("cannot determine home directory: %v", err)
	}
	defaultCDNDir := filepath.Join(home, ".go-deployer", "cdn")

	addr := flag.String("addr", ":2053", "listen address")
	token := flag.String("token", "", "authentication token for agent WebSocket connections")
	user := flag.String("user", "", "admin username")
	pass := flag.String("pass", "", "admin password")
	guestUser := flag.String("ro-user", "admin", "read-only user username")
	guestPass := flag.String("ro-pass", "mychat2026", "read-only user password")
	cdnDir := flag.String("cdn-dir", defaultCDNDir, "CDN file directory for build artifacts")
	flag.Parse()

	if err := os.MkdirAll(*cdnDir, 0755); err != nil {
		log.Fatalf("create CDN directory: %v", err)
	}

	server := controlplane.NewServer(*addr, *token, *user, *pass, *guestUser, *guestPass, *cdnDir)
	log.Printf("Control plane starting on %s (CDN: %s)", *addr, *cdnDir)
	if *token != "" {
		log.Printf("Agent token auth enabled")
	}
	if *user != "" {
		log.Printf("Dashboard login enabled (user: %s)", *user)
	}

	tlsDir := filepath.Join(home, ".go-deployer")
	if err := os.MkdirAll(tlsDir, 0700); err != nil {
		log.Fatalf("create TLS directory: %v", err)
	}
	certPath := filepath.Join(tlsDir, "deployer.crt")
	keyPath := filepath.Join(tlsDir, "deployer.key")
	fp, err := server.StartWithAutoTLS(certPath, keyPath)
	if err != nil {
		log.Fatal(err)
	}
	_ = fp
}
