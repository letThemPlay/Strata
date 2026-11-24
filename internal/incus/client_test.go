package incus

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pair is a self-signed certificate and its key, in PEM and parsed form.
type pair struct {
	certPEM, keyPEM []byte
	tls             tls.Certificate
}

func newPair(t *testing.T, cn string) pair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	p := pair{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}
	if p.tls, err = tls.X509KeyPair(p.certPEM, p.keyPEM); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeIncus serves GET /1.0 like Incus: "trusted" (with the server
// version) for the trusted client certificate, "untrusted" otherwise.
type fakeIncus struct {
	srv         *httptest.Server
	server      pair
	trusted     []byte // DER of the trusted client certificate
	handler     http.HandlerFunc
	lastProject string
}

func newFakeIncus(t *testing.T, trusted pair) *fakeIncus {
	t.Helper()
	f := &fakeIncus{server: newPair(t, "incus"), trusted: trusted.tls.Certificate[0]}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastProject = r.URL.Query().Get("project")
		if f.handler != nil {
			f.handler(w, r)
			return
		}
		if r.URL.Path != "/1.0" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"error","error":"not found","error_code":404}`))
			return
		}
		if len(r.TLS.PeerCertificates) > 0 && bytes.Equal(r.TLS.PeerCertificates[0].Raw, f.trusted) {
			_, _ = w.Write([]byte(`{"type":"sync","status":"Success","status_code":200,"metadata":{"api_version":"1.0","auth":"trusted","environment":{"server_version":"7.0.2"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"sync","status":"Success","status_code":200,"metadata":{"api_version":"1.0","auth":"untrusted"}}`))
	}))
	f.srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{f.server.tls},
		ClientAuth:   tls.RequestClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// config returns a client configuration for the fake server using the
// given client identity and pinned server certificate.
func config(t *testing.T, f *fakeIncus, client, pinned pair) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		Address:        strings.TrimPrefix(f.srv.URL, "https://"),
		Project:        "strata",
		CertFile:       writeFile(t, dir, "client.crt", client.certPEM),
		KeyFile:        writeFile(t, dir, "client.key", client.keyPEM),
		ServerCertFile: writeFile(t, dir, "server.crt", pinned.certPEM),
		Timeout:        2 * time.Second,
	}
}

func TestServerTrusted(t *testing.T) {
	agent := newPair(t, "strata-agent")
	f := newFakeIncus(t, agent)
	c, err := New(config(t, f, agent, f.server))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := c.Server(context.Background())
	if err != nil {
		t.Fatalf("Server: %v", err)
	}
	if s != (Server{APIVersion: "1.0", Trusted: true, Version: "7.0.2"}) {
		t.Errorf("server: %+v", s)
	}
	if f.lastProject != "strata" {
		t.Errorf("project query: %q", f.lastProject)
	}
}

func TestServerUntrustedIdentity(t *testing.T) {
	f := newFakeIncus(t, newPair(t, "someone-else"))
	c, err := New(config(t, f, newPair(t, "strata-agent"), f.server))
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Server(context.Background())
	if err != nil {
		t.Fatalf("Server: %v", err)
	}
	if s.Trusted || s.Version != "" {
		t.Errorf("untrusted identity reported as %+v", s)
	}
}

func TestServerCertificateMustMatchPin(t *testing.T) {
	agent := newPair(t, "strata-agent")
	f := newFakeIncus(t, agent)
	c, err := New(config(t, f, agent, newPair(t, "impostor")))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Server(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not match the pinned Incus certificate") {
		t.Errorf("expected a pinning failure, got %v", err)
	}
}

func TestServerErrors(t *testing.T) {
	agent := newPair(t, "strata-agent")
	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		want    string
	}{
		"API error": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"type":"error","error":"Certificate is restricted","error_code":403}`))
			},
			want: "GET /1.0: HTTP 403: Certificate is restricted",
		},
		"malformed JSON": {
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
			want:    "malformed response",
		},
		"async response": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"type":"async","metadata":{}}`))
			},
			want: `unexpected response type "async"`,
		},
		"no api_version": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"type":"sync","metadata":{"auth":"trusted"}}`))
			},
			want: "no api_version",
		},
		"oversized": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(bytes.Repeat([]byte(" "), maxResponseBytes+10))
			},
			want: "exceeds",
		},
		"timeout": {
			handler: func(_ http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
			},
			want: "Client.Timeout",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeIncus(t, agent)
			f.handler = tc.handler
			cfg := config(t, f, agent, f.server)
			cfg.Timeout = 300 * time.Millisecond
			c, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Server(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected %q, got %v", tc.want, err)
			}
			if name == "API error" {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
					t.Errorf("not an APIError: %#v", err)
				}
			}
		})
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	agent := newPair(t, "strata-agent")
	f := newFakeIncus(t, agent)
	good := config(t, f, agent, f.server)
	for name, mutate := range map[string]func(*Config){
		"remote address":     func(c *Config) { c.Address = "192.0.2.1:8443" },
		"host name":          func(c *Config) { c.Address = "localhost:8443" },
		"no port":            func(c *Config) { c.Address = "127.0.0.1" },
		"no project":         func(c *Config) { c.Project = "" },
		"missing key":        func(c *Config) { c.KeyFile = filepath.Join(t.TempDir(), "none") },
		"missing server":     func(c *Config) { c.ServerCertFile = filepath.Join(t.TempDir(), "none") },
		"server not PEM":     func(c *Config) { c.ServerCertFile = writeFile(t, t.TempDir(), "s.crt", []byte("x")) },
		"key does not match": func(c *Config) { c.KeyFile = writeFile(t, t.TempDir(), "k", newPair(t, "x").keyPEM) },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := good
			mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Error("expected an error")
			}
		})
	}
}
