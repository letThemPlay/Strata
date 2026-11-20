package install

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func lines(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

func newPrompter(input string) (*Prompter, *bytes.Buffer) {
	var out bytes.Buffer
	return NewPrompter(context.Background(), strings.NewReader(input), &out), &out
}

func TestPrompterAskRepromptsAndDefaults(t *testing.T) {
	p, out := newPrompter(lines("BAD", "", "ok-1"))
	got, err := p.Ask("Name", "", checkNodeID)
	if err != nil || got != "ok-1" {
		t.Fatalf("Ask = %q, %v", got, err)
	}
	if n := strings.Count(out.String(), "is not a valid node ID"); n != 2 {
		t.Errorf("expected 2 errors, output:\n%s", out)
	}

	p, _ = newPrompter(lines(""))
	if got, err := p.Ask("Name", "dflt", func(string) error { return nil }); err != nil || got != "dflt" {
		t.Errorf("default: %q, %v", got, err)
	}
}

func TestPrompterEOFAndCancel(t *testing.T) {
	p, _ := newPrompter("partial")
	if got, _ := p.Line(); got != "partial" {
		t.Errorf("last line without newline: %q", got)
	}
	if _, err := p.Line(); !errors.Is(err, ErrInputClosed) {
		t.Errorf("after EOF: %v", err)
	}
	if _, err := p.Line(); !errors.Is(err, ErrInputClosed) {
		t.Errorf("again after EOF: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r, w := io.Pipe()
	defer func() { _ = w.Close() }()
	p = NewPrompter(ctx, r, io.Discard)
	cancel()
	if _, err := p.Line(); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
}

func TestPrompterChooseAndConfirm(t *testing.T) {
	p, out := newPrompter(lines("0", "2", "x", "3", "", "maybe", "Y"))
	i, err := p.Choose("Pick:", []string{"a", "b", "c"}, []string{"", "broken", ""}, 0)
	if err != nil || i != 2 {
		t.Fatalf("Choose = %d, %v", i, err)
	}
	if !strings.Contains(out.String(), "unavailable: broken") || !strings.Contains(out.String(), "2 is unavailable") {
		t.Errorf("output:\n%s", out)
	}
	if ok, err := p.Confirm("Sure?", false); err != nil || ok {
		t.Errorf("empty answer should take default false: %v %v", ok, err)
	}
	if ok, err := p.Confirm("Sure?", false); err != nil || !ok {
		t.Errorf("Y: %v %v", ok, err)
	}
}

func TestPrompterReadSharesLines(t *testing.T) {
	p, _ := newPrompter(lines("first", "second", "third"))
	if l, _ := p.Line(); l != "first" {
		t.Fatalf("Line = %q", l)
	}
	buf := make([]byte, 100)
	n, err := p.Read(buf)
	if err != nil || string(buf[:n]) != "second\n" {
		t.Errorf("Read = %q, %v", buf[:n], err)
	}
	if l, _ := p.Line(); l != "third" {
		t.Errorf("Line after Read = %q", l)
	}
}

// wizardFS extends machineFS with a too-small disk sdb (no by-id name),
// a physical NIC eno1 that is up and a virtual bridge br0.
func wizardFS() fstest.MapFS {
	fsys := machineFS()
	fsys["dev/sdb"] = file("")
	fsys["sys/block/sdb/size"] = file("2097152\n") // 1 GiB
	fsys["dev/disk/by-id/wwn-0x5000"] = symlink("../../vda")
	fsys["sys/class/net/eth0/device"] = file("")
	fsys["sys/class/net/eth0/operstate"] = file("down\n")
	fsys["sys/class/net/eno1/address"] = file("52:54:00:aa:bb:cc\n")
	fsys["sys/class/net/eno1/operstate"] = file("up\n")
	fsys["sys/class/net/eno1/device"] = file("")
	fsys["sys/class/net/br0/address"] = file("52:54:00:00:00:01\n")
	return fsys
}

func TestDiskCandidates(t *testing.T) {
	disks, err := DiskCandidates(wizardFS())
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 2 {
		t.Fatalf("disks: %+v", disks)
	}
	sdb, vda := disks[0], disks[1]
	if sdb.Device != "/dev/sdb" || sdb.Stable() || sdb.Problem == nil || !strings.Contains(sdb.Problem.Error(), "at least") {
		t.Errorf("sdb: %+v", sdb)
	}
	// The model/serial name is preferred over the WWN.
	if vda.Device != "/dev/disk/by-id/virtio-system" || !vda.Stable() || vda.Problem != nil || vda.Info.SizeBytes != 32<<30 {
		t.Errorf("vda: %+v", vda)
	}
}

func TestInterfaceCandidatesPrefersPhysical(t *testing.T) {
	nics, err := InterfaceCandidates(wizardFS())
	if err != nil {
		t.Fatal(err)
	}
	if len(nics) != 2 || nics[0].Name != "eno1" || nics[1].Name != "eth0" {
		t.Errorf("nics: %+v", nics)
	}
	nics, _ = InterfaceCandidates(machineFS()) // eth0 is not physical there
	if len(nics) != 1 || nics[0].Name != "eth0" {
		t.Errorf("fallback: %+v", nics)
	}
}

func runWizard(t *testing.T, input string, w *Wizard) (Answers, string, error) {
	t.Helper()
	p, out := newPrompter(input)
	w.P = p
	if w.Root == nil {
		w.Root = wizardFS()
	}
	if w.ReadFile == nil {
		w.ReadFile = os.ReadFile
	}
	a, err := w.Run(context.Background())
	return a, out.String(), err
}

func TestWizardDHCP(t *testing.T) {
	input := lines(
		"hv-03", // node ID
		"",      // admin: default
		"1",     // paste a key
		"not-a-key",
		testKey,
		"",  // use these keys: yes
		"",  // add more: no
		"",  // disk: the only usable one is the default
		"",  // NIC: default eno1 (up)
		"1", // DHCP
	)
	a, out, err := runWizard(t, input, &Wizard{})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out)
	}
	want := Answers{
		APIVersion: APIVersion,
		Node:       Node{ID: "hv-03"},
		Admin:      Admin{Name: DefaultAdminName, AuthorizedKeys: []string{testKey}},
		Disk:       Disk{Device: "/dev/disk/by-id/virtio-system", Layout: DefaultLayout},
		Network:    Network{Interface: "eno1", Addressing: "dhcp"},
	}
	if !equalAnswers(a, want) {
		t.Errorf("answers = %+v\nwant %+v", a, want)
	}
	for _, s := range []string{"SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU", "unavailable: disk.device: /dev/sdb is 1.0 GiB"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
}

func TestWizardStaticWithKeysFromFileAndURL(t *testing.T) {
	other := makeKey("ssh-ed25519", "ssh-ed25519", 9, "url@ws")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(other + "\n" + testKey + "\n"))
	}))
	defer srv.Close()
	keyFile := filepath.Join(t.TempDir(), "keys")
	if err := os.WriteFile(keyFile, []byte("# comment\n"+testKey+"\nbroken line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	input := lines(
		"hv-04", "ops",
		"2", "/nonexistent", // unreadable file: back to the menu
		"2", keyFile, "y", "y", // one key, add more
		"3", "http://insecure", srv.URL+"/keys", "y", "n", // only the new key is offered
		"1", "2", // sdb is unavailable; vda
		"2", // eth0
		"2", // static
		"",  // addresses required
		"192.0.2.10/24, 2001:db8::10/64",
		"192.0.2.300", "192.0.2.1",
		"",                        // no IPv6 gateway
		"192.0.2.53 2001:db8::53", // DNS
	)
	w := &Wizard{HTTP: srv.Client()}
	a, out, err := runWizard(t, input, w)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out)
	}
	want := Answers{
		APIVersion: APIVersion,
		Node:       Node{ID: "hv-04"},
		Admin:      Admin{Name: "ops", AuthorizedKeys: []string{testKey, other}},
		Disk:       Disk{Device: "/dev/disk/by-id/virtio-system", Layout: DefaultLayout},
		Network: Network{
			Interface: "eth0", Addressing: "static",
			Addresses: []string{"192.0.2.10/24", "2001:db8::10/64"},
			Gateway4:  "192.0.2.1",
			DNS:       []string{"192.0.2.53", "2001:db8::53"},
		},
	}
	if !equalAnswers(a, want) {
		t.Errorf("answers = %+v\nwant %+v", a, want)
	}
	for _, s := range []string{"no such file", "skipped line 3", "found 1 key(s)", "url@ws", "at least one is required", "not an IPv4 address"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
}

func TestWizardRequiresAKey(t *testing.T) {
	input := lines("hv-05", "", "1", testKey, "n") // decline the only key
	_, out, err := runWizard(t, input, &Wizard{})
	if !errors.Is(err, ErrInputClosed) || !strings.Contains(out, "At least one key is required") {
		t.Errorf("err = %v\n%s", err, out)
	}
}

func TestWizardFailsWithoutUsableDisk(t *testing.T) {
	fsys := wizardFS()
	fsys["proc/mounts"] = file("/dev/vda /data ext4 rw 0 0\n")
	input := lines("hv-06", "", "1", testKey, "", "")
	_, out, err := runWizard(t, input, &Wizard{Root: fsys})
	if err == nil || !strings.Contains(err.Error(), "no usable disk") || !strings.Contains(out, "is in use") {
		t.Errorf("err = %v\n%s", err, out)
	}
}

func equalAnswers(a, b Answers) bool {
	x, _ := MarshalAnswers(a)
	y, _ := MarshalAnswers(b)
	return bytes.Equal(x, y)
}
