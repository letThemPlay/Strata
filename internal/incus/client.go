// Package incus is Strata's client for the Incus API (ADR-0015).
//
// The node agent reaches Incus over loopback HTTPS with a TLS client
// identity that Incus restricts to Strata's project (ADR-0017). It never
// uses the Incus Unix socket. The client accepts only the server
// certificate it was given (pinned), so no certificate authority is
// involved.
//
// Only the calls Strata needs are implemented; this slice needs just the
// server status.
package incus

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultTimeout bounds a single API request.
const DefaultTimeout = 10 * time.Second

// maxResponseBytes bounds the size of an API response the client reads.
const maxResponseBytes = 4 << 20

// Config describes how to reach Incus.
type Config struct {
	// Address is the loopback host:port of the Incus HTTPS listener, e.g.
	// "127.0.0.1:8443".
	Address string
	// Project is the Incus project requests are made in.
	Project string
	// CertFile and KeyFile hold the client identity (PEM).
	CertFile string
	KeyFile  string
	// ServerCertFile holds the Incus server certificate (PEM). It is the
	// only certificate the client accepts.
	ServerCertFile string
	// Timeout bounds each request; zero means DefaultTimeout.
	Timeout time.Duration
}

// Client is an Incus API client.
type Client struct {
	base    url.URL
	project string
	http    *http.Client
}

// New loads the identity and server certificate and returns a client.
func New(cfg Config) (*Client, error) {
	host, _, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("incus: address %q: %w", cfg.Address, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("incus: address %q is not a loopback IP address", cfg.Address)
	}
	if cfg.Project == "" {
		return nil, errors.New("incus: project not set")
	}
	identity, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("incus: load client identity: %w", err)
	}
	pinned, err := loadCertificate(cfg.ServerCertFile)
	if err != nil {
		return nil, fmt.Errorf("incus: load server certificate: %w", err)
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}

	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{identity},
		// Chain and host name verification are replaced by pinning: the
		// server must present exactly the configured certificate.
		// VerifyConnection also runs for resumed sessions.
		InsecureSkipVerify: true, //nolint:gosec // Verified by VerifyConnection (pinning).
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !bytes.Equal(cs.PeerCertificates[0].Raw, pinned.Raw) {
				return errors.New("server certificate does not match the pinned Incus certificate")
			}
			return nil
		},
	}
	return &Client{
		base:    url.URL{Scheme: "https", Host: cfg.Address},
		project: cfg.Project,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig:   tlsConfig,
				Proxy:             nil, // Never use a proxy for loopback.
				DisableKeepAlives: true,
			},
		},
	}, nil
}

// NewUnix returns a client for the Incus Unix socket at socket. Access to
// it is root-equivalent: only strata-host, run as root by NixOS, uses it.
// The agent never does (ADR-0017). project may be empty.
func NewUnix(socket, project string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		base:    url.URL{Scheme: "http", Host: "incus"},
		project: project,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
				Proxy:             nil,
				DisableKeepAlives: true,
			},
		},
	}
}

// Close releases idle connections.
func (c *Client) Close() {
	c.http.CloseIdleConnections()
}

// Server describes the Incus server as seen by the client.
type Server struct {
	// APIVersion is the Incus API version, e.g. "1.0".
	APIVersion string
	// Trusted reports whether Incus accepted the client identity.
	Trusted bool
	// Version is the Incus server version; Incus reveals it only to
	// trusted clients.
	Version string
}

// Server returns the server status (GET /1.0) in the client's project.
func (c *Client) Server(ctx context.Context) (Server, error) {
	var meta struct {
		APIVersion  string `json:"api_version"`
		Auth        string `json:"auth"`
		Environment struct {
			ServerVersion string `json:"server_version"`
		} `json:"environment"`
	}
	if err := c.get(ctx, "/1.0", &meta); err != nil {
		return Server{}, err
	}
	if meta.APIVersion == "" {
		return Server{}, errors.New("incus: GET /1.0: response has no api_version")
	}
	return Server{
		APIVersion: meta.APIVersion,
		Trusted:    meta.Auth == "trusted",
		Version:    meta.Environment.ServerVersion,
	}, nil
}

// response is the Incus API response envelope.
type response struct {
	Type      string          `json:"type"`
	ErrorCode int             `json:"error_code"`
	Error     string          `json:"error"`
	Operation string          `json:"operation"`
	Metadata  json.RawMessage `json:"metadata"`
}

// get performs a GET request and decodes the metadata into out.
func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodGet, path, nil, out)
}

// Do performs an API request in the client's project (if it has one) and
// decodes the metadata of the result into out (if not nil). path may carry
// a query string. A body is encoded as JSON. For asynchronous operations
// (e.g. creating a storage pool) Do waits for the operation to finish and
// returns its error, if any.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	r, status, err := c.roundTrip(ctx, method, path, body)
	if err != nil {
		return err
	}
	switch {
	case r.Type == "error" || status >= http.StatusBadRequest:
		msg := r.Error
		if msg == "" {
			msg = http.StatusText(status)
		}
		return &APIError{Method: method, Path: path, StatusCode: status, Message: msg}
	case r.Type == "async" && method != http.MethodGet && r.Operation != "":
		return c.wait(ctx, method, path, r.Operation, out)
	case r.Type != "sync":
		return fmt.Errorf("incus: %s %s: unexpected response type %q", method, path, r.Type)
	}
	if out == nil || len(r.Metadata) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.Metadata, out); err != nil {
		return fmt.Errorf("incus: %s %s: malformed metadata: %w", method, path, err)
	}
	return nil
}

// wait waits for the operation op started by method path.
func (c *Client) wait(ctx context.Context, method, path, op string, out any) error {
	var result struct {
		Status   string          `json:"status"`
		Err      string          `json:"err"`
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := c.Do(ctx, http.MethodGet, op+"/wait", nil, &result); err != nil {
		return fmt.Errorf("incus: %s %s: wait for %s: %w", method, path, op, err)
	}
	if result.Status != "Success" {
		msg := result.Err
		if msg == "" {
			msg = "operation " + result.Status
		}
		return &APIError{Method: method, Path: path, StatusCode: http.StatusOK, Message: msg}
	}
	if out != nil && len(result.Metadata) > 0 && string(result.Metadata) != "null" {
		if err := json.Unmarshal(result.Metadata, out); err != nil {
			return fmt.Errorf("incus: %s %s: malformed operation metadata: %w", method, path, err)
		}
	}
	return nil
}

// roundTrip sends one request and decodes the response envelope.
func (c *Client) roundTrip(ctx context.Context, method, path string, body any) (response, int, error) {
	u := c.base
	p, query, _ := strings.Cut(path, "?")
	u.Path = p
	q, err := url.ParseQuery(query)
	if err != nil {
		return response{}, 0, fmt.Errorf("incus: %s %s: %w", method, path, err)
	}
	if c.project != "" && q.Get("project") == "" {
		q.Set("project", c.project)
	}
	u.RawQuery = q.Encode()

	var reqBody io.Reader = http.NoBody
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return response{}, 0, fmt.Errorf("incus: %s %s: encode request: %w", method, path, err)
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reqBody)
	if err != nil {
		return response{}, 0, fmt.Errorf("incus: %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return response{}, 0, fmt.Errorf("incus: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return response{}, 0, fmt.Errorf("incus: %s %s: read response: %w", method, path, err)
	}
	if len(data) > maxResponseBytes {
		return response{}, 0, fmt.Errorf("incus: %s %s: response exceeds %d bytes", method, path, maxResponseBytes)
	}
	var r response
	if err := json.Unmarshal(data, &r); err != nil {
		return response{}, 0, fmt.Errorf("incus: %s %s: HTTP %d: malformed response: %w", method, path, resp.StatusCode, err)
	}
	return r, resp.StatusCode, nil
}

// APIError is an error returned by the Incus API.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("incus: %s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Message)
}

// IsNotFound reports whether err is an Incus "not found" error.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// loadCertificate reads the first PEM certificate from file.
func loadCertificate(file string) (*x509.Certificate, error) {
	data, err := os.ReadFile(file) //nolint:gosec // Path from the service configuration.
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: no PEM certificate", file)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return cert, nil
}
