package hostsetup

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// identityValidity is the lifetime of the agent's certificate.
const identityValidity = 10 * 365 * 24 * time.Hour

// ensureIdentity creates the agent's key and certificate if they are
// missing and makes sure Incus trusts exactly that certificate, restricted
// to the project.
func (s *Setup) ensureIdentity(ctx context.Context) error {
	id := s.Config.Agent
	certDER, created, err := loadOrCreateIdentity(id.Dir, id.CommonName)
	if err != nil {
		return fmt.Errorf("agent identity: %w", err)
	}
	if created {
		s.Log.InfoContext(ctx, "agent identity created", "dir", id.Dir)
	}
	sum := sha256.Sum256(certDER)
	fp := hex.EncodeToString(sum[:])

	var entries []struct {
		Name        string   `json:"name"`
		Type        string   `json:"type"`
		Restricted  bool     `json:"restricted"`
		Projects    []string `json:"projects"`
		Fingerprint string   `json:"fingerprint"`
	}
	if err := s.Incus.Do(ctx, http.MethodGet, "/1.0/certificates?recursion=1", nil, &entries); err != nil {
		return fmt.Errorf("list trusted certificates: %w", err)
	}
	trusted := false
	for _, e := range entries {
		if e.Name != id.Name && e.Fingerprint != fp {
			continue
		}
		if e.Fingerprint == fp && e.Type == "client" && e.Restricted && slices.Equal(e.Projects, []string{s.Config.Project}) {
			trusted = true
			continue
		}
		// Stale (another certificate under this name) or wider permissions.
		if err := s.Incus.Do(ctx, http.MethodDelete, "/1.0/certificates/"+e.Fingerprint, nil, nil); err != nil {
			return fmt.Errorf("remove trust entry %s: %w", e.Fingerprint, err)
		}
		s.Log.WarnContext(ctx, "trust entry removed", "fingerprint", e.Fingerprint, "name", e.Name,
			"restricted", e.Restricted, "projects", e.Projects)
	}
	if trusted {
		return nil
	}
	body := map[string]any{
		"type":        "client",
		"name":        id.Name,
		"certificate": base64.StdEncoding.EncodeToString(certDER),
		"restricted":  true,
		"projects":    []string{s.Config.Project},
	}
	if err := s.Incus.Do(ctx, http.MethodPost, "/1.0/certificates", body, nil); err != nil {
		return fmt.Errorf("trust agent identity: %w", err)
	}
	s.Log.InfoContext(ctx, "agent identity trusted", "fingerprint", fp, "project", s.Config.Project)
	return nil
}

// loadOrCreateIdentity returns the DER certificate in dir, creating an EC
// P-384 key and a self-signed client certificate if either file is
// missing. The key is written with mode 0600.
func loadOrCreateIdentity(dir, commonName string) (certDER []byte, created bool, err error) {
	keyFile, certFile := filepath.Join(dir, "client.key"), filepath.Join(dir, "client.crt")
	if certPEM, err := os.ReadFile(certFile); err == nil { //nolint:gosec // Fixed path.
		if _, keyErr := os.Stat(keyFile); keyErr == nil {
			block, _ := pem.Decode(certPEM)
			if block == nil || block.Type != "CERTIFICATE" {
				return nil, false, fmt.Errorf("%s: no PEM certificate", certFile)
			}
			return block.Bytes, false, nil
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, false, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, false, fmt.Errorf("generate serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(identityValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, false, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, false, fmt.Errorf("encode key: %w", err)
	}
	if err := writeAtomic(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, false, err
	}
	if err := writeAtomic(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, false, err
	}
	return der, true, nil
}

func writeAtomic(file string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", file, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, werr := tmp.Write(data)
	cerr := tmp.Chmod(mode)
	serr := tmp.Sync()
	clerr := tmp.Close()
	if err := errors.Join(werr, cerr, serr, clerr); err != nil {
		return fmt.Errorf("write %s: %w", file, err)
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return fmt.Errorf("write %s: %w", file, err)
	}
	return nil
}
