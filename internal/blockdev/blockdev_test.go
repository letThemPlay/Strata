package blockdev

import (
	"errors"
	"io/fs"
	"maps"
	"slices"
	"testing"
	"testing/fstest"
)

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

func symlink(target string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(target), Mode: fs.ModeSymlink}
}

// hostFS is an installed host: the system on vda (vda1 /boot, vda2 /),
// an empty disk sdb, a disk sdc with an ext4 partition and a GPT, a ZFS
// member sdd, an LVM PV sde held by dm-0, and a disk sdf behind a
// device-mapper /srv mount.
func hostFS() fstest.MapFS {
	return fstest.MapFS{
		"proc/mounts": file("/dev/vda2 / ext4 rw 0 0\n/dev/vda1 /boot vfat rw 0 0\n" +
			"tmpfs /run tmpfs rw 0 0\n/dev/mapper/data /srv ext4 rw 0 0\n"),
		"proc/swaps":                   file("Filename Type Size Used Priority\n"),
		"dev/vda":                      file(""),
		"dev/sdb":                      file(""),
		"dev/dm-1":                     file(""),
		"dev/mapper/data":              symlink("../dm-1"),
		"dev/disk/by-id/ata-EMPTY":     symlink("../../sdb"),
		"dev/disk/by-id/virtio-system": symlink("../../vda"),
		"sys/block/vda/dev":            file("253:0\n"),
		"sys/block/vda/vda1/partition": file("1\n"),
		"sys/block/vda/vda1/dev":       file("253:1\n"),
		"sys/block/vda/vda2/partition": file("2\n"),
		"sys/block/vda/vda2/dev":       file("253:2\n"),
		"sys/block/vda/vda2/size":      file("2048\n"),
		"sys/block/sdb/dev":            file("8:16\n"),
		"sys/block/sdc/dev":            file("8:32\n"),
		"sys/block/sdc/sdc1/partition": file("1\n"),
		"sys/block/sdc/sdc1/dev":       file("8:33\n"),
		"sys/block/sdd/dev":            file("8:48\n"),
		"sys/block/sde/dev":            file("8:64\n"),
		"sys/block/sde/holders/dm-0":   file(""),
		"sys/block/sdf/dev":            file("8:80\n"),
		"sys/block/dm-1/slaves/sdf":    file(""),
		"run/udev/data/b253:1":         file("E:ID_FS_TYPE=vfat\n"),
		"run/udev/data/b253:2":         file("E:ID_FS_TYPE=ext4\n"),
		"run/udev/data/b8:32":          file("E:ID_PART_TABLE_TYPE=gpt\n"),
		"run/udev/data/b8:33":          file("E:ID_FS_TYPE=ext4\nE:ID_FS_LABEL=olddata\n"),
		"run/udev/data/b8:48":          file("E:ID_FS_TYPE=zfs_member\nE:ID_FS_LABEL=tank\n"),
		"run/udev/data/b8:64":          file("E:ID_FS_TYPE=LVM2_member\n"),
		"run/udev/data/b8:16":          file("E:ID_SERIAL=EMPTY\n"),
		"sys/block/vda/vda1/size":      file("1024\n"),
		"sys/block/sdc/sdc1/size":      file("1024\n"),
		"sys/block/dm-1/dev":           file("254:1\n"),
		"run/udev/data/b254:1":         file("E:ID_FS_TYPE=ext4\n"),
		"sys/block/sde/size":           file("2048\n"),
		"sys/block/sdb/size":           file("2048\n"),
		"sys/block/vda/size":           file("2048\n"),
		"sys/block/sdc/size":           file("2048\n"),
		"sys/block/sdd/size":           file("2048\n"),
		"sys/block/sdf/size":           file("2048\n"),
		"sys/block/dm-1/size":          file("2048\n"),
	}
}

func TestResolve(t *testing.T) {
	root := hostFS()
	if name, err := Resolve(root, "/dev/disk/by-id/ata-EMPTY"); err != nil || name != "sdb" {
		t.Errorf("Resolve: %q, %v", name, err)
	}
	if _, err := Resolve(root, "/dev/disk/by-id/missing"); err == nil {
		t.Error("missing device resolved")
	}
}

func TestSignaturesAndPartitionTable(t *testing.T) {
	root := hostFS()
	for _, tc := range []struct {
		disk string
		want []string
	}{
		{"sdb", nil},
		{"sdc", []string{`/dev/sdc1: ext4 "olddata"`}},
		{"sdd", []string{`/dev/sdd: zfs_member "tank"`}},
		{"sde", []string{"/dev/sde: LVM2_member"}},
	} {
		parts, err := Partitions(root, tc.disk)
		if err != nil {
			t.Fatal(err)
		}
		sigs, err := Signatures(root, tc.disk, parts)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, s := range sigs {
			got = append(got, s.String())
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.disk, got, tc.want)
		}
	}
	if pt, _ := PartitionTable(root, "sdc"); pt != "gpt" {
		t.Errorf("sdc partition table %q", pt)
	}
	if pt, _ := PartitionTable(root, "sdb"); pt != "" {
		t.Errorf("sdb partition table %q", pt)
	}

	noUdev := hostFS()
	for k := range noUdev {
		if len(k) > 13 && k[:13] == "run/udev/data" {
			delete(noUdev, k)
		}
	}
	if _, err := Signatures(noUdev, "sdb", nil); !errors.Is(err, ErrNoUdevDatabase) {
		t.Errorf("without udev: %v", err)
	}
	if _, err := PartitionTable(noUdev, "sdb"); !errors.Is(err, ErrNoUdevDatabase) {
		t.Errorf("without udev: %v", err)
	}
}

func TestUses(t *testing.T) {
	root := hostFS()
	for disk, want := range map[string][]string{
		"vda": {"/dev/vda1 is mounted on /boot", "/dev/vda2 is mounted on /"},
		"sde": {"/dev/sde is part of /dev/dm-0"},
	} {
		parts, _ := Partitions(root, disk)
		got, err := Uses(root, disk, parts)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", disk, got, want)
		}
	}
}

func TestSystemDisks(t *testing.T) {
	got, err := SystemDisks(hostFS())
	if err != nil {
		t.Fatal(err)
	}
	// vda holds / and /boot; sdf is behind /srv, which is not a system mount.
	if want := map[string]bool{"vda": true}; !maps.Equal(got, want) {
		t.Errorf("system disks %v", got)
	}

	// Root on device-mapper (e.g. LUKS) on a partition of nvme0n1.
	root := hostFS()
	root["proc/mounts"] = file("/dev/mapper/cryptroot / ext4 rw 0 0\n")
	root["dev/mapper/cryptroot"] = symlink("../dm-2")
	root["dev/dm-2"] = file("")
	root["sys/block/dm-2/slaves/nvme0n1p2"] = file("")
	root["sys/block/nvme0n1/nvme0n1p2/partition"] = file("2\n")
	got, err = SystemDisks(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"nvme0n1": true}; !maps.Equal(got, want) {
		t.Errorf("system disks behind dm: %v", got)
	}
}
