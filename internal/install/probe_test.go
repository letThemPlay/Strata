package install

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

func symlink(target string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(target), Mode: fs.ModeSymlink}
}

// machineFS is a live installer system with a 32 GiB target disk vda
// (/dev/disk/by-id/virtio-system), the installation medium sr0 mounted on
// /iso, and a NIC eth0.
func machineFS() fstest.MapFS {
	return fstest.MapFS{
		"proc/cpuinfo":                 file("processor\t: 0\nvendor_id\t: GenuineIntel\nflags\t\t: fpu vmx\n\n"),
		"proc/meminfo":                 file("MemTotal:        4194304 kB\n"),
		"proc/mounts":                  file("tmpfs / tmpfs rw 0 0\n/dev/sr0 /iso iso9660 ro 0 0\n"),
		"dev/vda":                      file(""),
		"dev/sr0":                      file(""),
		"dev/disk/by-id/virtio-system": symlink("../../vda"),
		"dev/disk/by-id/virtio-part1":  symlink("../../vda1"),
		"dev/vda1":                     file(""),
		"sys/block/vda/size":           file("67108864\n"), // 32 GiB
		"sys/block/vda/ro":             file("0\n"),
		"sys/block/vda/removable":      file("0\n"),
		"sys/block/vda/device/model":   file("QEMU HARDDISK\n"),
		"sys/block/sr0/size":           file("2097152\n"),
		"sys/class/net/eth0/address":   file("52:54:00:12:34:56\n"),
		"sys/class/net/lo/address":     file("00:00:00:00:00:00\n"),
	}
}

func probeAnswers(t *testing.T) Answers {
	t.Helper()
	a := mustParse(t, dhcpAnswers)
	a.Disk.Device = "/dev/disk/by-id/virtio-system"
	return a
}

func TestProbeAcceptsEmptyDisk(t *testing.T) {
	d, err := Probe(machineFS(), probeAnswers(t))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if d.Name != "vda" || d.SizeBytes != 32<<30 || d.Model != "QEMU HARDDISK" || len(d.Partitions) != 0 {
		t.Errorf("unexpected disk info: %+v", d)
	}
}

func TestProbeListsPartitions(t *testing.T) {
	fsys := machineFS()
	fsys["sys/block/vda/vda1/partition"] = file("1\n")
	fsys["sys/block/vda/vda1/size"] = file("2097152\n")
	d, err := Probe(fsys, probeAnswers(t))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(d.Partitions) != 1 || d.Partitions[0].Name != "vda1" || d.Partitions[0].SizeBytes != 1<<30 {
		t.Errorf("partitions: %+v", d.Partitions)
	}
}

func TestProbeRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(fstest.MapFS, *Answers)
		wantErr string
	}{
		{"missing device", func(_ fstest.MapFS, a *Answers) { a.Disk.Device = "/dev/disk/by-id/nope" }, "does not exist"},
		{"partition instead of disk", func(_ fstest.MapFS, a *Answers) { a.Disk.Device = "/dev/disk/by-id/virtio-part1" }, "not a whole disk"},
		{"installation medium", func(_ fstest.MapFS, a *Answers) { a.Disk.Device = "/dev/sr0" }, "not a whole disk"},
		{"too small", func(f fstest.MapFS, _ *Answers) { f["sys/block/vda/size"] = file("16777216\n") }, "at least 16.0 GiB"},
		{"read-only", func(f fstest.MapFS, _ *Answers) { f["sys/block/vda/ro"] = file("1\n") }, "read-only"},
		{"mounted partition", func(f fstest.MapFS, _ *Answers) {
			f["sys/block/vda/vda1/partition"] = file("1\n")
			f["proc/mounts"] = file("/dev/vda1 /iso iso9660 ro 0 0\n")
		}, "is mounted on /iso"},
		{"swap partition", func(f fstest.MapFS, _ *Answers) {
			f["sys/block/vda/vda1/partition"] = file("1\n")
			f["proc/swaps"] = file("Filename Type Size Used Priority\n/dev/vda1 partition 1024 0 -2\n")
		}, "used as swap"},
		{"LVM on partition", func(f fstest.MapFS, _ *Answers) {
			f["sys/block/vda/vda1/partition"] = file("1\n")
			f["sys/block/vda/vda1/holders/dm-0"] = file("")
		}, "part of /dev/dm-0"},
		{"symlink loop", func(f fstest.MapFS, a *Answers) {
			f["dev/disk/by-id/loop"] = symlink("loop")
			a.Disk.Device = "/dev/disk/by-id/loop"
		}, "too many levels"},
		{"symlink out of /dev", func(f fstest.MapFS, a *Answers) {
			f["dev/disk/by-id/etc"] = symlink("../../../etc/passwd")
			f["etc/passwd"] = file("")
			a.Disk.Device = "/dev/disk/by-id/etc"
		}, "directly under /dev"},
		{"missing interface", func(_ fstest.MapFS, a *Answers) { a.Network.Interface = "eno1" }, `no interface named "eno1" (found: eth0)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys, a := machineFS(), probeAnswers(t)
			tt.mutate(fsys, &a)
			_, err := Probe(fsys, a)
			if err == nil {
				t.Fatal("Probe succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[uint64]string{
		512:      "512 B",
		1536:     "1.5 KiB",
		16 << 30: "16.0 GiB",
		3 << 40:  "3.0 TiB",
	} {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
