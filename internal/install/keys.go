package install

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Limits for downloading keys.
const (
	keyFetchTimeout  = 30 * time.Second
	maxKeyListBytes  = 256 << 10
	maxKeyRedirects  = 5
	keyListMaxReport = 3
)

// KeyList is the result of reading an authorized_keys-style text.
type KeyList struct {
	// Keys are the valid key lines, in order, without duplicates.
	Keys []string
	// Rejected describes lines that are not usable keys, by line number.
	Rejected []string
}

// ParseKeyList reads OpenSSH public keys, one per line. Blank lines and
// '#' comments are skipped. Lines with authorized_keys options (e.g.
// "no-pty ssh-ed25519 ...") or invalid keys are rejected, not modified.
func ParseKeyList(text string) KeyList {
	var kl KeyList
	seen := map[string]bool{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.Join(strings.Fields(line), " ")
		if err := checkSSHKey(line); err != nil {
			kl.Rejected = append(kl.Rejected, fmt.Sprintf("line %d: %v", i+1, err))
			continue
		}
		if !seen[line] {
			seen[line] = true
			kl.Keys = append(kl.Keys, line)
		}
	}
	return kl
}

// Fingerprint returns the OpenSSH SHA256 fingerprint of a key line that
// passed checkSSHKey, as printed by ssh-keygen -l.
func Fingerprint(key string) string {
	fields := strings.Fields(key)
	if len(fields) < 2 {
		return "?"
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "?"
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// DescribeKey summarizes a key for display: type, fingerprint, comment.
func DescribeKey(key string) string {
	fields := strings.SplitN(key, " ", 3)
	s := fields[0] + " " + Fingerprint(key)
	if len(fields) == 3 {
		s += " " + fields[2]
	}
	return s
}

// NewKeyClient returns the HTTP client used to download keys: it times
// out and refuses redirects away from HTTPS.
func NewKeyClient() *http.Client {
	return &http.Client{
		Timeout: keyFetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxKeyRedirects {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing redirect to non-HTTPS URL %s", req.URL.Redacted())
			}
			return nil
		},
	}
}

// FetchKeys downloads an authorized_keys-style list over HTTPS, e.g.
// https://github.com/USER.keys.
func FetchKeys(ctx context.Context, client *http.Client, rawURL string) (KeyList, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return KeyList{}, fmt.Errorf("%q is not an https:// URL", rawURL)
	}
	if u.User != nil {
		return KeyList{}, errors.New("URLs with credentials are not supported")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return KeyList{}, fmt.Errorf("download keys: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return KeyList{}, fmt.Errorf("download keys: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return KeyList{}, fmt.Errorf("download keys from %s: %s", u.Redacted(), resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxKeyListBytes+1))
	if err != nil {
		return KeyList{}, fmt.Errorf("download keys from %s: %w", u.Redacted(), err)
	}
	if len(body) > maxKeyListBytes {
		return KeyList{}, fmt.Errorf("download keys from %s: more than %s", u.Redacted(), FormatBytes(maxKeyListBytes))
	}
	return ParseKeyList(string(body)), nil
}
