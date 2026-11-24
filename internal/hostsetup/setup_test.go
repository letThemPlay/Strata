package hostsetup

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"

	"github.com/letthemplay/Strata/internal/incus"
)

// fakeIncus is an in-memory Incus API: projects, certificates, storage
// pools and the project's default profile. It records every write.
type fakeIncus struct {
	projects map[string]map[string]any
	certs    []map[string]any
	pools    map[string]storagePool
	networks map[string]incusNetwork
	profile  map[string]any
	writes   []string
	// failPool makes creating this pool fail like Incus would.
	failPool string
}

func newFakeIncus() *fakeIncus {
	return &fakeIncus{
		projects: map[string]map[string]any{},
		pools:    map[string]storagePool{},
		networks: map[string]incusNetwork{
			// Host interfaces appear as unmanaged networks.
			"eth1": {Name: "eth1", Type: "physical"},
			"eth2": {Name: "eth2", Type: "physical"},
		},
		profile: map[string]any{"description": "Default", "config": map[string]any{}, "devices": map[string]any{}},
	}
}

func notFound(method, path string) error {
	return &incus.APIError{Method: method, Path: path, StatusCode: http.StatusNotFound, Message: "not found"}
}

// roundTrip encodes body and decodes into out through JSON, like the API.
func roundTrip(in, out any) error {
	if out == nil {
		return nil
	}
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (f *fakeIncus) Do(_ context.Context, method, path string, body, out any) error {
	p, _, _ := strings.Cut(path, "?")
	if method != http.MethodGet {
		f.writes = append(f.writes, method+" "+p)
	}
	var b map[string]any
	if body != nil {
		if err := roundTrip(body, &b); err != nil {
			return err
		}
	}
	switch {
	case method == http.MethodGet && strings.HasPrefix(p, "/1.0/projects/"):
		pr, ok := f.projects[strings.TrimPrefix(p, "/1.0/projects/")]
		if !ok {
			return notFound(method, path)
		}
		return roundTrip(pr, out)
	case method == http.MethodPost && p == "/1.0/projects":
		f.projects[b["name"].(string)] = b
		return nil
	case method == http.MethodPut && strings.HasPrefix(p, "/1.0/projects/"):
		name := strings.TrimPrefix(p, "/1.0/projects/")
		b["name"] = name
		f.projects[name] = b
		return nil
	case method == http.MethodGet && p == "/1.0/certificates":
		return roundTrip(f.certs, out)
	case method == http.MethodDelete && strings.HasPrefix(p, "/1.0/certificates/"):
		fp := strings.TrimPrefix(p, "/1.0/certificates/")
		f.certs = slices.DeleteFunc(f.certs, func(c map[string]any) bool { return c["fingerprint"] == fp })
		return nil
	case method == http.MethodPost && p == "/1.0/certificates":
		der, err := base64.StdEncoding.DecodeString(b["certificate"].(string))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(der)
		b["fingerprint"] = hex.EncodeToString(sum[:])
		delete(b, "certificate")
		f.certs = append(f.certs, b)
		return nil
	case method == http.MethodGet && p == "/1.0/storage-pools":
		list := []storagePool{}
		for _, sp := range f.pools {
			list = append(list, sp)
		}
		return roundTrip(list, out)
	case method == http.MethodPost && p == "/1.0/storage-pools":
		var sp storagePool
		if err := roundTrip(b, &sp); err != nil {
			return err
		}
		if sp.Name == f.failPool {
			return &incus.APIError{Method: method, Path: path, StatusCode: 500, Message: "zpool create failed"}
		}
		// Incus keeps the original source and rewrites it for zfs/lvm.
		sp.Config["volatile.initial_source"] = sp.Config["source"]
		if sp.Driver != "dir" {
			sp.Config["source"] = sp.Name
		}
		f.pools[sp.Name] = sp
		return nil
	case method == http.MethodGet && p == "/1.0/networks":
		list := []incusNetwork{}
		for _, n := range f.networks {
			list = append(list, n)
		}
		return roundTrip(list, out)
	case method == http.MethodPost && p == "/1.0/networks":
		var n incusNetwork
		if err := roundTrip(b, &n); err != nil {
			return err
		}
		n.Managed = true
		f.networks[n.Name] = n
		return nil
	case method == http.MethodPatch && strings.HasPrefix(p, "/1.0/networks/"):
		name := strings.TrimPrefix(p, "/1.0/networks/")
		n := f.networks[name]
		var patch map[string]string
		if err := roundTrip(b["config"], &patch); err != nil {
			return err
		}
		for k, v := range patch {
			if v == "" {
				delete(n.Config, k)
			} else {
				n.Config[k] = v
			}
		}
		f.networks[name] = n
		return nil
	case p == "/1.0/profiles/default" && method == http.MethodGet:
		return roundTrip(f.profile, out)
	case p == "/1.0/profiles/default" && method == http.MethodPut:
		f.profile = b
		// Track which pool the profile uses, like Incus's used_by.
		const ref = "/1.0/profiles/default?project=strata"
		var devices map[string]map[string]string
		_ = roundTrip(b["devices"], &devices)
		for name, sp := range f.pools {
			sp.UsedBy = slices.DeleteFunc(sp.UsedBy, func(u string) bool { return u == ref })
			if devices["root"]["pool"] == name {
				sp.UsedBy = append(sp.UsedBy, ref)
			}
			f.pools[name] = sp
		}
		for name, n := range f.networks {
			n.UsedBy = slices.DeleteFunc(n.UsedBy, func(u string) bool { return u == ref })
			if devices["eth0"]["network"] == name {
				n.UsedBy = append(n.UsedBy, ref)
			}
			f.networks[name] = n
		}
		return nil
	}
	return fmt.Errorf("fake incus: unexpected %s %s", method, path)
}

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

func symlink(target string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(target), Mode: fs.ModeSymlink}
}

// addDisk adds disk name (major:minor 8:n) with by-id link id and the
// given udev properties.
func addDisk(m fstest.MapFS, name, id string, n int, udev string) {
	m["dev/"+name] = file("")
	m["dev/disk/by-id/"+id] = symlink("../../" + name)
	m["sys/block/"+name+"/dev"] = file(fmt.Sprintf("8:%d\n", n))
	m[fmt.Sprintf("run/udev/data/b8:%d", n)] = file(udev)
}

// hostFS: system on vda; empty disks sdb, sdc, sdd; sde with an ext4
// filesystem; sdf mounted on dirMount.
func hostFS(dirMount string) fstest.MapFS {
	m := fstest.MapFS{
		"proc/mounts": file("/dev/vda2 / ext4 rw 0 0\n/dev/vda1 /boot vfat rw 0 0\n/dev/sdf " +
			dirMount + " ext4 rw 0 0\n"),
		"dev/vda":                      file(""),
		"dev/disk/by-id/virtio-system": symlink("../../vda"),
		"sys/block/vda/dev":            file("253:0\n"),
		"sys/block/vda/vda1/partition": file("1\n"),
		"sys/block/vda/vda2/partition": file("2\n"),
		"run/udev/data/b253:0":         file("E:ID_PART_TABLE_TYPE=gpt\n"),
	}
	addDisk(m, "sdb", "ata-B", 16, "E:ID_SERIAL=B\n")
	addDisk(m, "sdc", "ata-C", 32, "E:ID_SERIAL=C\n")
	addDisk(m, "sdd", "ata-D", 48, "E:ID_SERIAL=D\n")
	addDisk(m, "sde", "ata-E", 64, "E:ID_FS_TYPE=ext4\nE:ID_FS_LABEL=olddata\n")
	addDisk(m, "sdf", "ata-F", 80, "E:ID_FS_TYPE=ext4\n")
	return m
}

func baseConfig(t *testing.T) (Config, string) {
	t.Helper()
	mount := t.TempDir()
	return Config{
		Project:            "strata",
		ProjectDescription: "Strata workloads",
		ProjectConfig:      map[string]string{"restricted": "true", "limits.disk": "2TiB"},
		Agent:              &Identity{Dir: t.TempDir(), Name: "strata-agent", CommonName: "strata-agent@hv-01"},
		Pools: []Pool{
			{Name: "fast", Driver: "zfs", Layout: "mirror", Devices: []string{"/dev/disk/by-id/ata-B", "/dev/disk/by-id/ata-C"}, Limit: "1TiB"},
			{Name: "bulk", Driver: "lvm", Devices: []string{"/dev/disk/by-id/ata-D"}},
			{Name: "scratch", Driver: "dir", Path: mount},
		},
		DefaultPool: "fast",
		RootSize:    "10GiB",
	}, mount
}

func noExec(context.Context, string, ...string) ([]byte, error) {
	return nil, errors.New("unexpected command")
}

// fakeLinks: eth1 (management, addressed), eth2 and eth3 (unaddressed).
func fakeLinks(name string) (Link, error) {
	switch name {
	case "eth1":
		return Link{Exists: true, Addrs: []string{"192.168.1.2/24"}}, nil
	case "eth2", "eth3":
		return Link{Exists: true}, nil
	}
	return Link{}, nil
}

func run(t *testing.T, cfg Config, f *fakeIncus, root fs.FS) error {
	t.Helper()
	s := &Setup{Config: cfg, Incus: f, Root: root, Exec: noExec, Link: fakeLinks}
	return s.Run(context.Background())
}

func projectConfig(t *testing.T, f *fakeIncus) map[string]string {
	t.Helper()
	var cfg map[string]string
	if err := roundTrip(f.projects["strata"]["config"], &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestFreshHost(t *testing.T) {
	defer syscall.Umask(syscall.Umask(0o077))
	cfg, mount := baseConfig(t)
	f := newFakeIncus()
	if err := run(t, cfg, f, hostFS(mount)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Pools, with Incus sources as declared.
	for name, want := range map[string]string{
		"fast":    "mirror=/dev/disk/by-id/ata-B,/dev/disk/by-id/ata-C",
		"bulk":    "/dev/disk/by-id/ata-D",
		"scratch": filepath.Join(mount, "scratch"),
	} {
		if got := f.pools[name].source(); got != want {
			t.Errorf("pool %s source %q, want %q", name, got, want)
		}
	}
	if f.pools["bulk"].Config["lvm.vg_name"] != "bulk" || f.pools["bulk"].Config["lvm.use_thinpool"] != "true" {
		t.Errorf("lvm config %v", f.pools["bulk"].Config)
	}
	if _, ok := f.pools["fast"].Config["source.wipe"]; ok {
		t.Error("source.wipe must never be set")
	}
	// 0711 whatever the umask (unprivileged containers traverse it).
	if fi, err := os.Stat(filepath.Join(mount, "scratch")); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o711 {
		t.Errorf("dir pool directory: %v %v", err, fi)
	}

	// Project: access to exactly the declared pools, limits.
	pc := projectConfig(t, f)
	if pc["restricted.storage-pools.access"] != "bulk,fast,scratch" || pc["limits.disk.pool.fast"] != "1TiB" ||
		pc["limits.disk"] != "2TiB" || pc["restricted"] != "true" {
		t.Errorf("project config %v", pc)
	}
	if _, ok := pc["limits.disk.pool.bulk"]; ok {
		t.Errorf("no limit declared for bulk: %v", pc)
	}

	// Default profile root disk.
	var devices map[string]map[string]string
	_ = roundTrip(f.profile["devices"], &devices)
	if devices["root"]["pool"] != "fast" || devices["root"]["path"] != "/" || devices["root"]["size"] != "10GiB" {
		t.Errorf("default profile devices %v", devices)
	}

	// Identity: key 0600, one restricted trust entry for its certificate.
	fi, err := os.Stat(filepath.Join(cfg.Agent.Dir, "client.key"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key: %v %v", err, fi)
	}
	certPEM, _ := os.ReadFile(filepath.Join(cfg.Agent.Dir, "client.crt"))
	block, _ := pem.Decode(certPEM)
	sum := sha256.Sum256(block.Bytes)
	if len(f.certs) != 1 || f.certs[0]["fingerprint"] != hex.EncodeToString(sum[:]) ||
		f.certs[0]["restricted"] != true || fmt.Sprint(f.certs[0]["projects"]) != "[strata]" {
		t.Errorf("trust %v", f.certs)
	}

	// A second run changes nothing but re-enforces the project.
	f.writes = nil
	if err := run(t, cfg, f, hostFS(mount)); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !slices.Equal(f.writes, []string{"PUT /1.0/projects/strata"}) {
		t.Errorf("second run writes %v", f.writes)
	}
}

func TestPoolRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		pool   Pool
		modify func(fstest.MapFS)
		want   string
	}{
		"filesystem": {
			pool: Pool{Name: "p", Driver: "zfs", Devices: []string{"/dev/disk/by-id/ata-E"}},
			want: `not empty (/dev/sde: ext4 "olddata"); Strata does not erase disks`,
		},
		"partition table": {
			pool:   Pool{Name: "p", Driver: "lvm", Devices: []string{"/dev/disk/by-id/ata-B"}},
			modify: func(m fstest.MapFS) { m["run/udev/data/b8:16"] = file("E:ID_PART_TABLE_TYPE=gpt\n") },
			want:   "/dev/sdb: gpt partition table",
		},
		"system disk": {
			pool: Pool{Name: "p", Driver: "zfs", Devices: []string{"/dev/disk/by-id/virtio-system"}},
			want: "/dev/vda holds the running system",
		},
		"held by device-mapper": {
			pool:   Pool{Name: "p", Driver: "lvm", Devices: []string{"/dev/disk/by-id/ata-B"}},
			modify: func(m fstest.MapFS) { m["sys/block/sdb/holders/dm-3"] = file("") },
			want:   "in use: /dev/sdb is part of /dev/dm-3",
		},
		"mounted": {
			pool: Pool{Name: "p", Driver: "zfs", Devices: []string{"/dev/disk/by-id/ata-F"}},
			want: "in use: /dev/sdf is mounted on",
		},
		"partition, not disk": {
			pool: Pool{Name: "p", Driver: "zfs", Devices: []string{"/dev/disk/by-id/ata-B-part1"}},
			modify: func(m fstest.MapFS) {
				m["dev/disk/by-id/ata-B-part1"] = symlink("../../sdb1")
				m["dev/sdb1"] = file("")
			},
			want: "/dev/sdb1 is not a whole disk",
		},
		"missing device": {
			pool: Pool{Name: "p", Driver: "zfs", Devices: []string{"/dev/disk/by-id/ata-NONE"}},
			want: "does not exist",
		},
		"no udev database": {
			pool: Pool{Name: "p", Driver: "zfs", Devices: []string{"/dev/disk/by-id/ata-B"}},
			modify: func(m fstest.MapFS) {
				for k := range m {
					if strings.HasPrefix(k, "run/udev/") {
						delete(m, k)
					}
				}
			},
			want: "existing data cannot be checked",
		},
		"adopt without a zpool": {
			pool: Pool{Name: "p", Driver: "zfs", Adopt: true, Devices: []string{"/dev/disk/by-id/ata-B"}},
			want: "cannot adopt: no zfs_member signature found",
		},
		"adopt another zpool": {
			pool:   Pool{Name: "p", Driver: "zfs", Adopt: true, Devices: []string{"/dev/disk/by-id/ata-B"}},
			modify: func(m fstest.MapFS) { m["run/udev/data/b8:16"] = file("E:ID_FS_TYPE=zfs_member\nE:ID_FS_LABEL=tank\n") },
			want:   `belongs to zpool "tank", not "p"`,
		},
		"adopt a filesystem": {
			pool: Pool{Name: "p", Driver: "zfs", Adopt: true, Devices: []string{"/dev/disk/by-id/ata-E"}},
			want: "expected only zfs_member",
		},
		"dir not mounted": {
			pool: Pool{Name: "p", Driver: "dir", Path: "/srv/none"},
			want: "nothing is mounted on /srv/none",
		},
		"dir on the system disk": {
			pool: Pool{Name: "p", Driver: "dir", Path: "/boot"},
			want: "/boot is on /dev/vda, which holds the running system",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, mount := baseConfig(t)
			cfg.Pools = []Pool{cfg.Pools[1], tc.pool} // bulk (valid) and the bad one
			root := hostFS(mount)
			if tc.modify != nil {
				tc.modify(root)
			}
			f := newFakeIncus()
			err := run(t, cfg, f, root)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			if _, ok := f.pools["p"]; ok {
				t.Error("refused pool was created")
			}
			if _, ok := f.pools["bulk"]; !ok && name != "no udev database" {
				t.Error("a refusal must not stop the other pools")
			}
			// The project is still enforced.
			if projectConfig(t, f)["restricted"] != "true" {
				t.Error("project not enforced")
			}
		})
	}
}

func TestAdoption(t *testing.T) {
	cfg, mount := baseConfig(t)
	cfg.Pools = []Pool{
		{Name: "tank", Driver: "zfs", Adopt: true, Devices: []string{"/dev/disk/by-id/ata-B"}},
		{Name: "vg0", Driver: "lvm", Adopt: true, Devices: []string{"/dev/disk/by-id/ata-C"}},
	}
	cfg.DefaultPool = "tank"
	root := hostFS(mount)
	// A zpool created on a whole disk lives on its first partition.
	root["sys/block/sdb/sdb1/partition"] = file("1\n")
	root["sys/block/sdb/sdb1/dev"] = file("8:17\n")
	root["run/udev/data/b8:16"] = file("E:ID_PART_TABLE_TYPE=gpt\n")
	root["run/udev/data/b8:17"] = file("E:ID_FS_TYPE=zfs_member\nE:ID_FS_LABEL=tank\n")
	root["run/udev/data/b8:32"] = file("E:ID_FS_TYPE=LVM2_member\n")
	var ran []string
	s := &Setup{Config: cfg, Incus: newFakeIncus(), Root: root, Link: fakeLinks, Exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return []byte("  vg0\n"), nil
	}}
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	f := s.Incus.(*fakeIncus)
	if f.pools["tank"].source() != "tank" || f.pools["vg0"].source() != "vg0" {
		t.Errorf("pools %v", f.pools)
	}
	if _, ok := f.pools["vg0"].Config["lvm.vg_name"]; ok {
		t.Error("an adopted VG keeps its name")
	}
	if !slices.Equal(ran, []string{"pvs --noheadings -o vg_name /dev/sdc"}) {
		t.Errorf("commands %v", ran)
	}
}

func TestExistingPools(t *testing.T) {
	cfg, mount := baseConfig(t)
	f := newFakeIncus()
	f.pools["fast"] = storagePool{Name: "fast", Driver: "lvm", Config: map[string]string{"source": "fast"}}
	f.pools["manual"] = storagePool{Name: "manual", Driver: "dir", Config: map[string]string{"source": "/srv/manual"}}
	err := run(t, cfg, f, hostFS(mount))
	if err == nil || !strings.Contains(err.Error(), "pool fast: exists with driver lvm, declared zfs") {
		t.Fatalf("expected a driver mismatch, got %v", err)
	}
	if f.pools["fast"].Driver != "lvm" {
		t.Error("an existing pool must not be modified")
	}
	pc := projectConfig(t, f)
	if pc["limits.disk.pool.manual"] != "0" || strings.Contains(pc["restricted.storage-pools.access"], "manual") {
		t.Errorf("undeclared pool must be closed to the project: %v", pc)
	}
	if !slices.Contains(f.writes, "PUT /1.0/projects/strata") {
		t.Error("project not enforced")
	}
}

func TestNoPoolsClosesAllPools(t *testing.T) {
	cfg, mount := baseConfig(t)
	cfg.Pools, cfg.DefaultPool = nil, ""
	f := newFakeIncus()
	f.pools["manual"] = storagePool{Name: "manual", Driver: "dir", Config: map[string]string{"source": "/srv/manual"}}
	if err := run(t, cfg, f, hostFS(mount)); err != nil {
		t.Fatal(err)
	}
	pc := projectConfig(t, f)
	if pc["restricted.storage-pools.access"] != NoPoolsSentinel || pc["limits.disk.pool.manual"] != "0" {
		t.Errorf("project config %v", pc)
	}
}

func TestCreateFailureIsReported(t *testing.T) {
	cfg, mount := baseConfig(t)
	f := newFakeIncus()
	f.failPool = "fast"
	err := run(t, cfg, f, hostFS(mount))
	if err == nil || !strings.Contains(err.Error(), "pool fast: create:") {
		t.Fatalf("got %v", err)
	}
	// The default profile is left alone while its pool is missing.
	var devices map[string]map[string]string
	_ = roundTrip(f.profile["devices"], &devices)
	if _, ok := devices["root"]; ok {
		t.Error("root device set for a missing pool")
	}
}

func TestTrustIsCorrected(t *testing.T) {
	cfg, mount := baseConfig(t)
	f := newFakeIncus()
	if err := run(t, cfg, f, hostFS(mount)); err != nil {
		t.Fatal(err)
	}
	fp := f.certs[0]["fingerprint"]
	// Widen the entry and add a stale one under the same name.
	f.certs[0]["restricted"] = false
	f.certs = append(f.certs, map[string]any{"name": "strata-agent", "type": "client", "fingerprint": "abc", "restricted": true, "projects": []string{"strata"}})
	if err := run(t, cfg, f, hostFS(mount)); err != nil {
		t.Fatal(err)
	}
	if len(f.certs) != 1 || f.certs[0]["fingerprint"] != fp || f.certs[0]["restricted"] != true {
		t.Errorf("trust %v", f.certs)
	}
	if !slices.Contains(f.writes, "DELETE /1.0/certificates/abc") {
		t.Errorf("stale entry not removed: %v", f.writes)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	_ = os.WriteFile(good, []byte(`{"project":"strata","pools":[{"name":"p","driver":"dir","path":"/srv"}]}`), 0o600)
	if c, err := LoadConfig(good); err != nil || c.Pools[0].Path != "/srv" {
		t.Errorf("LoadConfig: %+v %v", c, err)
	}
	for _, bad := range []string{`{"project":"default"}`, `{}`, `{`} {
		p := filepath.Join(dir, url.PathEscape(bad)+".json")
		_ = os.WriteFile(p, []byte(bad), 0o600)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestUndeclaredPoolStillInUseStaysOpen(t *testing.T) {
	cfg, mount := baseConfig(t)
	f := newFakeIncus()
	f.pools["old"] = storagePool{
		Name: "old", Driver: "dir", Config: map[string]string{"source": "/srv/old"},
		UsedBy: []string{"/1.0/instances/c1?project=strata"},
	}
	f.pools["other"] = storagePool{
		Name: "other", Driver: "dir", Config: map[string]string{"source": "/srv/other"},
		UsedBy: []string{"/1.0/instances/c2?project=elsewhere", "/1.0/instances/c3"},
	}
	// A cached image does not keep a pool open (Incus does not block on it).
	f.pools["cache"] = storagePool{
		Name: "cache", Driver: "dir", Config: map[string]string{"source": "/srv/cache"},
		UsedBy: []string{"/1.0/images/abc?project=strata"},
	}
	if err := run(t, cfg, f, hostFS(mount)); err != nil {
		t.Fatal(err)
	}
	pc := projectConfig(t, f)
	if pc["restricted.storage-pools.access"] != "bulk,fast,old,scratch" {
		t.Errorf("a pool the project still uses must stay allowed: %v", pc)
	}
	if _, ok := pc["limits.disk.pool.old"]; ok {
		t.Errorf("a pool the project still uses must not get a zero limit: %v", pc)
	}
	if pc["limits.disk.pool.other"] != "0" || pc["limits.disk.pool.cache"] != "0" {
		t.Errorf("a pool used only by other projects must be closed: %v", pc)
	}
}

func TestDefaultPoolMoveClosesThePreviousPool(t *testing.T) {
	cfg, mount := baseConfig(t)
	f := newFakeIncus()
	if err := run(t, cfg, f, hostFS(mount)); err != nil {
		t.Fatal(err)
	}
	// fast is removed from the configuration; scratch becomes the default.
	cfg.Pools = cfg.Pools[1:]
	cfg.DefaultPool = "scratch"
	f.writes = nil
	if err := run(t, cfg, f, hostFS(mount)); err != nil {
		t.Fatal(err)
	}
	pc := projectConfig(t, f)
	if pc["restricted.storage-pools.access"] != "bulk,scratch" || pc["limits.disk.pool.fast"] != "0" {
		t.Errorf("fast must be closed once the profile no longer uses it: %v", pc)
	}
	want := []string{"PUT /1.0/projects/strata", "PUT /1.0/profiles/default", "PUT /1.0/projects/strata"}
	if !slices.Equal(f.writes, want) {
		t.Errorf("writes %v, want %v", f.writes, want)
	}
}

func networksConfig(t *testing.T) Config {
	t.Helper()
	cfg, _ := baseConfig(t)
	cfg.Pools, cfg.DefaultPool, cfg.RootSize = nil, "", ""
	cfg.ManagementInterface = "eth1"
	cfg.Networks = []Network{
		{Name: "lan", Uplink: "eth2"},
		{Name: "v20", Uplink: "eth3", VLAN: 20},
		{Name: "lab"},
	}
	cfg.DefaultNetwork = "lan"
	return cfg
}

func TestNetworksCreated(t *testing.T) {
	cfg := networksConfig(t)
	f := newFakeIncus()
	if err := run(t, cfg, f, hostFS(t.TempDir())); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for name, ext := range map[string]string{"lan": "eth2", "v20": "eth3.20/eth3/20", "lab": ""} {
		n := f.networks[name]
		if !n.Managed || n.Type != "bridge" || n.Config["ipv4.address"] != "none" || n.Config["ipv6.address"] != "none" ||
			n.Config["bridge.external_interfaces"] != ext {
			t.Errorf("network %s: %+v", name, n)
		}
	}
	if got := projectConfig(t, f)["restricted.networks.access"]; got != "lab,lan,v20" {
		t.Errorf("network access %q", got)
	}
	var devices map[string]map[string]string
	_ = roundTrip(f.profile["devices"], &devices)
	if devices["eth0"]["network"] != "lan" || devices["eth0"]["type"] != "nic" {
		t.Errorf("default profile devices %v", devices)
	}

	// Unchanged: nothing but the project is written again.
	f.writes = nil
	if err := run(t, cfg, f, hostFS(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.writes, []string{"PUT /1.0/projects/strata"}) {
		t.Errorf("second run writes %v", f.writes)
	}
}

func TestNetworkConfigIsEnforced(t *testing.T) {
	cfg := networksConfig(t)
	f := newFakeIncus()
	if err := run(t, cfg, f, hostFS(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	// Changed by hand, and the declaration moves lan to a VLAN.
	f.networks["lan"].Config["ipv4.address"] = "10.0.0.1/24"
	cfg.Networks[0] = Network{Name: "lan", Uplink: "eth3", VLAN: 30}
	f.writes = nil
	if err := run(t, cfg, f, hostFS(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	n := f.networks["lan"]
	if n.Config["ipv4.address"] != "none" || n.Config["bridge.external_interfaces"] != "eth3.30/eth3/30" {
		t.Errorf("lan not enforced: %v", n.Config)
	}
	if !slices.Contains(f.writes, "PATCH /1.0/networks/lan") {
		t.Errorf("writes %v", f.writes)
	}
}

func TestNetworkRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		net  Network
		want string
	}{
		"management uplink": {Network{Name: "bad", Uplink: "eth1"}, "uplink eth1 is the management interface"},
		"missing uplink":    {Network{Name: "bad", Uplink: "eth9"}, "uplink eth9 does not exist"},
		"addressed uplink": {
			Network{Name: "bad", Uplink: "eth4"}, "uplink eth4 has IP addresses (10.0.0.5/24)",
		},
		"host interface name": {Network{Name: "eth2"}, "a host interface or a non-bridge network of this name exists"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := networksConfig(t)
			cfg.ManagementInterface = "eth1"
			cfg.Networks = append(cfg.Networks, tc.net)
			f := newFakeIncus()
			s := &Setup{Config: cfg, Incus: f, Root: hostFS(t.TempDir()), Exec: noExec, Link: func(n string) (Link, error) {
				if n == "eth4" {
					return Link{Exists: true, Addrs: []string{"10.0.0.5/24"}}, nil
				}
				return fakeLinks(n)
			}}
			err := s.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			if _, ok := f.networks["lan"]; !ok {
				t.Error("a refusal must not stop the other networks")
			}
			if n, ok := f.networks[tc.net.Name]; ok && n.Managed {
				t.Errorf("refused network created: %+v", n)
			}
		})
	}
}

func TestUndeclaredNetworks(t *testing.T) {
	cfg := networksConfig(t)
	f := newFakeIncus()
	f.networks["manual"] = incusNetwork{Name: "manual", Type: "bridge", Managed: true, Config: map[string]string{}}
	f.networks["old"] = incusNetwork{
		Name: "old", Type: "bridge", Managed: true, Config: map[string]string{},
		UsedBy: []string{"/1.0/instances/c1?project=strata"},
	}
	if err := run(t, cfg, f, hostFS(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	// manual is closed; old is still used by the project and stays open.
	if got := projectConfig(t, f)["restricted.networks.access"]; got != "lab,lan,old,v20" {
		t.Errorf("network access %q", got)
	}
	if _, ok := f.networks["manual"]; !ok {
		t.Error("an undeclared network must not be deleted")
	}

	cfg.Networks, cfg.DefaultNetwork = nil, ""
	delete(f.networks, "old")
	if err := run(t, cfg, f, hostFS(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	// lan is still used by the default profile; the others are closed.
	if got := projectConfig(t, f)["restricted.networks.access"]; got != "lan" {
		t.Errorf("network access %q", got)
	}
}

func TestNoNetworksSentinel(t *testing.T) {
	cfg, mount := baseConfig(t)
	f := newFakeIncus()
	if err := run(t, cfg, f, hostFS(mount)); err != nil {
		t.Fatal(err)
	}
	if got := projectConfig(t, f)["restricted.networks.access"]; got != NoNetworksSentinel {
		t.Errorf("network access %q", got)
	}
}
