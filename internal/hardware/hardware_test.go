package hardware

import (
	"encoding/json"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

const intelCPUInfo = `processor	: 0
vendor_id	: GenuineIntel
model name	: Intel(R) Xeon(R) Silver 4314 CPU @ 2.40GHz
flags		: fpu vme de pse vmx sse sse2

processor	: 1
vendor_id	: GenuineIntel
model name	: Intel(R) Xeon(R) Silver 4314 CPU @ 2.40GHz
flags		: fpu vme de pse vmx sse sse2
`

const amdCPUInfo = `processor	: 0
vendor_id	: AuthenticAMD
model name	: AMD EPYC 7313 16-Core Processor
flags		: fpu vme de pse svm sse sse2
`

const noVirtCPUInfo = `processor	: 0
vendor_id	: GenuineIntel
model name	: QEMU Virtual CPU
flags		: fpu vme de pse sse sse2
`

// aarch64 kernels do not report vendor_id or model name.
const armCPUInfo = `processor	: 0
BogoMIPS	: 50.00
Features	: fp asimd evtstrm
CPU implementer	: 0x41

processor	: 1
BogoMIPS	: 50.00
`

const memInfo = `MemTotal:       65536000 kB
MemFree:        1024 kB
`

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

// baseFS returns a minimal valid host filesystem.
func baseFS(cpuinfo string) fstest.MapFS {
	return fstest.MapFS{
		"proc/cpuinfo": file(cpuinfo),
		"proc/meminfo": file(memInfo),
	}
}

// fullFS returns a realistic Intel hypervisor host.
func fullFS() fstest.MapFS {
	fsys := baseFS(intelCPUInfo)
	for k, v := range map[string]string{
		"sys/class/dmi/id/sys_vendor":   "Supermicro\n",
		"sys/class/dmi/id/product_name": "SYS-1029P\n",

		"sys/class/net/lo/address":         "00:00:00:00:00:00\n",
		"sys/class/net/eno1/address":       "3c:ec:ef:00:00:01\n",
		"sys/class/net/eno1/operstate":     "up\n",
		"sys/class/net/eno1/device/vendor": "0x8086\n",
		"sys/class/net/vmbr0/address":      "3c:ec:ef:00:00:01\n",
		"sys/class/net/vmbr0/operstate":    "up\n",

		"sys/block/sda/size":                 "1953525168\n",
		"sys/block/sda/removable":            "0\n",
		"sys/block/sda/queue/rotational":     "1\n",
		"sys/block/sda/device/model":         "ST1000NM0033  \n",
		"sys/block/nvme0n1/size":             "3907029168\n",
		"sys/block/nvme0n1/removable":        "0\n",
		"sys/block/nvme0n1/queue/rotational": "0\n",
		"sys/block/loop0/size":               "100\n",
		"sys/block/dm-0/size":                "100\n",
		"sys/block/zram0/size":               "100\n",
		"sys/block/sr0/size":                 "100\n",

		"sys/kernel/iommu_groups/0/type": "DMA\n",
		"sys/kernel/iommu_groups/1/type": "DMA\n",
		"sys/kernel/iommu_groups/2/type": "DMA\n",
	} {
		fsys[k] = file(v)
	}
	fsys["sys/firmware/efi"] = &fstest.MapFile{Mode: fs.ModeDir | 0o555}
	fsys["dev/kvm"] = file("")
	return fsys
}

func TestDiscoverFullHost(t *testing.T) {
	t.Parallel()

	got, err := Discover(fullFS())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	want := Report{
		APIVersion: APIVersion,
		CPU: CPU{
			Vendor:       "GenuineIntel",
			Model:        "Intel(R) Xeon(R) Silver 4314 CPU @ 2.40GHz",
			LogicalCount: 2,
		},
		Memory:   Memory{TotalBytes: 65536000 * 1024},
		Firmware: Firmware{UEFI: true},
		System:   System{Vendor: "Supermicro", Product: "SYS-1029P"},
		Virtualization: Virtualization{
			Extension:   ExtensionIntelVTx,
			KVMDevice:   true,
			IOMMUGroups: 3,
		},
		NetworkInterfaces: []NetworkInterface{
			{Name: "eno1", MACAddress: "3c:ec:ef:00:00:01", OperState: "up", Physical: true},
			{Name: "vmbr0", MACAddress: "3c:ec:ef:00:00:01", OperState: "up", Physical: false},
		},
		BlockDevices: []BlockDevice{
			{Name: "nvme0n1", SizeBytes: 3907029168 * 512},
			{Name: "sda", SizeBytes: 1953525168 * 512, Model: "ST1000NM0033", Rotational: true},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Discover mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestDiscoverCPUVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cpuinfo   string
		wantVend  string
		wantCount int
		wantExt   string
	}{
		{"intel", intelCPUInfo, "GenuineIntel", 2, ExtensionIntelVTx},
		{"amd", amdCPUInfo, "AuthenticAMD", 1, ExtensionAMDV},
		{"no virtualization", noVirtCPUInfo, "GenuineIntel", 1, ""},
		{"aarch64 without vendor", armCPUInfo, "", 2, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Discover(baseFS(tc.cpuinfo))
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if got.CPU.Vendor != tc.wantVend || got.CPU.LogicalCount != tc.wantCount ||
				got.Virtualization.Extension != tc.wantExt {
				t.Errorf("got vendor=%q count=%d ext=%q, want %q %d %q",
					got.CPU.Vendor, got.CPU.LogicalCount, got.Virtualization.Extension,
					tc.wantVend, tc.wantCount, tc.wantExt)
			}
		})
	}
}

func TestDiscoverMinimalHost(t *testing.T) {
	t.Parallel()

	// Only the required files exist: everything optional must be zero-valued.
	got, err := Discover(baseFS(amdCPUInfo))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.Firmware.UEFI || got.Virtualization.KVMDevice || got.Virtualization.IOMMUGroups != 0 {
		t.Errorf("optional fields not zero: %+v", got)
	}
	if len(got.NetworkInterfaces) != 0 || len(got.BlockDevices) != 0 {
		t.Errorf("expected no devices, got %+v / %+v", got.NetworkInterfaces, got.BlockDevices)
	}
	if got.System != (System{}) {
		t.Errorf("System = %+v, want zero", got.System)
	}
}

func TestDiscoverErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(fstest.MapFS)
		wantErr string
	}{
		{"missing cpuinfo", func(f fstest.MapFS) { delete(f, "proc/cpuinfo") }, "read /proc/cpuinfo"},
		{"empty cpuinfo", func(f fstest.MapFS) { f["proc/cpuinfo"] = file("") }, "no processor entries"},
		{"missing meminfo", func(f fstest.MapFS) { delete(f, "proc/meminfo") }, "read /proc/meminfo"},
		{"meminfo without MemTotal", func(f fstest.MapFS) { f["proc/meminfo"] = file("MemFree: 1 kB\n") }, "MemTotal not found"},
		{"meminfo bad unit", func(f fstest.MapFS) { f["proc/meminfo"] = file("MemTotal: 1 MB\n") }, "unexpected MemTotal format"},
		{"meminfo bad number", func(f fstest.MapFS) { f["proc/meminfo"] = file("MemTotal: x kB\n") }, "MemTotal"},
		{"bad disk size", func(f fstest.MapFS) { f["sys/block/sda/size"] = file("lots\n") }, "parse /sys/block/sda/size"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := baseFS(intelCPUInfo)
			tc.mutate(fsys)
			_, err := Discover(fsys)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Discover error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	t.Parallel()

	in, err := Discover(fullFS())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"apiVersion":"`+APIVersion+`"`) {
		t.Errorf("serialized report lacks apiVersion: %s", data)
	}
	var out Report
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip mismatch\n in: %+v\nout: %+v", in, out)
	}
}
