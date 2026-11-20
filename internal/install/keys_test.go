package install

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// makeKey returns a well-formed key line of type typ whose key data
// embeds name as its type and seed as key material.
func makeKey(typ, name string, seed byte, comment string) string {
	blob := binary.BigEndian.AppendUint32(nil, uint32(len(name))) //nolint:gosec // Short test key type names.
	blob = append(blob, name...)
	blob = binary.BigEndian.AppendUint32(blob, 32)
	for i := range 32 {
		blob = append(blob, seed+byte(i))
	}
	k := typ + " " + base64.StdEncoding.EncodeToString(blob)
	if comment != "" {
		k += " " + comment
	}
	return k
}

func TestCheckSSHKey(t *testing.T) {
	ok := []string{
		testKey,
		makeKey("ssh-ed25519", "ssh-ed25519", 1, ""),
		makeKey("ecdsa-sha2-nistp256", "ecdsa-sha2-nistp256", 1, "a b c"),
	}
	for _, k := range ok {
		if err := checkSSHKey(k); err != nil {
			t.Errorf("checkSSHKey(%q): %v", k, err)
		}
	}
	bad := map[string]string{
		"type mismatch":  makeKey("ssh-rsa", "ssh-ed25519", 1, ""),
		"invalid base64": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJ",
		"truncated":      "ssh-ed25519 AAA=",
		"short name":     "ssh-ed25519 AAAAZm9v",
		"options prefix": "no-pty " + testKey,
		"private key":    "-----BEGIN OPENSSH PRIVATE KEY-----",
	}
	for name, k := range bad {
		if err := checkSSHKey(k); err == nil {
			t.Errorf("%s: checkSSHKey(%q) accepted", name, k)
		}
	}
}

func TestValidateRejectsMismatchedKeyType(t *testing.T) {
	a := mustParse(t, dhcpAnswers)
	a.Admin.AuthorizedKeys = []string{makeKey("ssh-rsa", "ssh-ed25519", 1, "")}
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "not a ssh-rsa key") {
		t.Errorf("Validate: %v", err)
	}
}

func TestFingerprintMatchesSSHKeygen(t *testing.T) {
	// ssh-keygen -lf of testKey.
	const want = "SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU"
	if got := Fingerprint(testKey); got != want {
		t.Errorf("Fingerprint = %s, want %s", got, want)
	}
	if got := DescribeKey(testKey); got != "ssh-ed25519 "+want+" admin@ws" {
		t.Errorf("DescribeKey = %q", got)
	}
}

func TestParseKeyList(t *testing.T) {
	other := makeKey("ssh-ed25519", "ssh-ed25519", 7, "other@ws")
	text := "# my keys\r\n" + testKey + "\r\n\n  " + other + "  \n" + testKey + "\nno-pty " + other + "\ngarbage\n"
	kl := ParseKeyList(text)
	if len(kl.Keys) != 2 || kl.Keys[0] != testKey || kl.Keys[1] != other {
		t.Errorf("keys: %q", kl.Keys)
	}
	if len(kl.Rejected) != 2 || !strings.HasPrefix(kl.Rejected[0], "line 6:") || !strings.HasPrefix(kl.Rejected[1], "line 7:") {
		t.Errorf("rejected: %q", kl.Rejected)
	}
}

func TestFetchKeys(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user.keys", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testKey + "\n"))
	})
	mux.HandleFunc("/big.keys", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("#", maxKeyListBytes+1)))
	})
	mux.HandleFunc("/to-http", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/user.keys", http.StatusFound)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	client := NewKeyClient()
	client.Transport = srv.Client().Transport
	ctx := context.Background()

	kl, err := FetchKeys(ctx, client, srv.URL+"/user.keys")
	if err != nil || len(kl.Keys) != 1 || kl.Keys[0] != testKey {
		t.Errorf("FetchKeys: %v, %+v", err, kl)
	}

	failures := map[string]string{
		"http://example.invalid/user.keys":       "not an https:// URL",
		"https://user:pw@example.invalid/x.keys": "credentials",
		srv.URL + "/missing":                     "404",
		srv.URL + "/big.keys":                    "more than",
		srv.URL + "/to-http":                     "non-HTTPS",
	}
	for u, want := range failures {
		if _, err := FetchKeys(ctx, client, u); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("FetchKeys(%s) = %v, want error containing %q", u, err, want)
		}
	}
}
