package controlplane

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"time"
)

// CertInfo holds the self-signed TLS certificate and its SHA-256 fingerprint.
type CertInfo struct {
	TLSCert tls.Certificate
	// FP is the SHA-256 fingerprint of the certificate DER bytes, format "sha256:<hex>".
	// Agents use this for certificate pinning.
	FP string
}

// LoadOrGenerateCert loads a TLS certificate from certPath/keyPath.
// If the files don't exist, generates a new self-signed ECDSA P-256 certificate
// and persists it to disk so the fingerprint stays stable across restarts.
func LoadOrGenerateCert(certPath, keyPath string) (*CertInfo, error) {
	// Try loading from disk first
	if info, err := loadCert(certPath, keyPath); err == nil {
		return info, nil
	}

	// Generate new ECDSA P-256 key
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "deployer-control-plane"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour), // 10 years
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &ecKey.PublicKey, ecKey)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(ecKey)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// Persist to disk
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return nil, fmt.Errorf("write cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return nil, fmt.Errorf("write key: %w", err)
	}

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("build tls cert: %w", err)
	}
	fp := sha256.Sum256(certDER)
	return &CertInfo{
		TLSCert: tlsCert,
		FP:      fmt.Sprintf("sha256:%x", fp),
	}, nil
}

func loadCert(certPath, keyPath string) (*CertInfo, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("invalid cert PEM")
	}
	fp := sha256.Sum256(block.Bytes)
	return &CertInfo{
		TLSCert: tlsCert,
		FP:      fmt.Sprintf("sha256:%x", fp),
	}, nil
}

// PinnedTLSConfig returns a tls.Config that verifies the server certificate
// matches the given SHA-256 fingerprint (certificate pinning).
func PinnedTLSConfig(certFP string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("server presented no certificate")
			}
			fp := sha256.Sum256(rawCerts[0])
			got := fmt.Sprintf("sha256:%x", fp)
			if got != certFP {
				return fmt.Errorf("cert pinning failed (want %s, got %s)", certFP, got)
			}
			return nil
		},
	}
}
