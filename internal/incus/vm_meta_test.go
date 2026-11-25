package incus

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/letthemplay/Strata/internal/workload"
)

func cfgOf(f *fakeVMServer) map[string]any { return f.instances["web-01"]["config"].(map[string]any) }

func TestMetaIsRecordedOnlyWhenNotDefault(t *testing.T) {
	// A CLI VM (and every VM from before M3) has neither key.
	f, c := createdVM(t, false)
	if _, ok := cfgOf(f)[keyOwner]; ok {
		t.Error("owner key set for a CLI VM")
	}
	st, err := c.GetVM(context.Background(), "web-01")
	if err != nil || st.Owner != workload.OwnerCLI || st.Protected || len(st.Drift) != 0 {
		t.Errorf("status %+v %v", st, err)
	}

	f2, c2 := newFakeVMs(t)
	if err := c2.CreateVM(context.Background(), testVM(), workload.Meta{Owner: workload.OwnerGitOps, Protected: true}); err != nil {
		t.Fatal(err)
	}
	if cfgOf(f2)[keyOwner] != "gitops" || cfgOf(f2)[keyProtected] != "true" {
		t.Errorf("config %v", cfgOf(f2))
	}
	st, err = c2.GetVM(context.Background(), "web-01")
	if err != nil || st.Owner != workload.OwnerGitOps || !st.Protected || len(st.Drift) != 0 {
		t.Errorf("status %+v %v", st, err)
	}
}

func TestDriftIsReported(t *testing.T) {
	f, c := createdVM(t, false)
	cfgOf(f)["limits.cpu"] = "16"
	cfgOf(f)["cloud-init.user-data"] = "#cloud-config\n"
	f.instances["web-01"]["devices"].(map[string]any)["usb"] = map[string]any{"type": "usb"}
	list, err := c.ListVMs(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("list %+v %v", list, err)
	}
	want := []string{`config limits.cpu: "16", want "2"`, "unexpected device usb"}
	for _, w := range want {
		if !slices.Contains(list[0].Drift, w) {
			t.Errorf("drift %q lacks %q", list[0].Drift, w)
		}
	}
	// The spec has cloud-init keys, so changed user data is drift too.
	if !slices.ContainsFunc(list[0].Drift, func(d string) bool { return strings.HasPrefix(d, "config cloud-init.user-data") }) {
		t.Errorf("drift %q", list[0].Drift)
	}
}

func TestRestoreVM(t *testing.T) {
	f, c := createdVM(t, true)
	cfgOf(f)["limits.cpu"] = "16"
	f.instances["web-01"]["devices"].(map[string]any)["usb"] = map[string]any{"type": "usb"}
	if err := c.RestoreVM(context.Background(), "web-01", 30); err != nil {
		t.Fatalf("RestoreVM: %v", err)
	}
	if got := strings.Join(f.calls, ","); got != "stop,put,start" {
		t.Errorf("calls %s", got)
	}
	st, err := c.GetVM(context.Background(), "web-01")
	if err != nil || len(st.Drift) != 0 || st.Status != "Running" || cfgOf(f)["volatile.uuid"] != "keep-me" {
		t.Errorf("after restore %+v %v %v", st, err, cfgOf(f))
	}
	// Nothing to restore: nothing is done.
	f.calls = nil
	if err := c.RestoreVM(context.Background(), "web-01", 30); err != nil || len(f.calls) != 0 {
		t.Errorf("second restore: %v %v", err, f.calls)
	}
}

func TestRestoreFailureStartsAgain(t *testing.T) {
	f, c := createdVM(t, true)
	cfgOf(f)["limits.cpu"] = "16"
	f.failPut = true
	if err := c.RestoreVM(context.Background(), "web-01", 30); err == nil {
		t.Fatal("restore succeeded")
	}
	if got := strings.Join(f.calls, ","); got != "stop,put,start" || f.instances["web-01"]["status"] != "Running" {
		t.Errorf("calls %s status %v", got, f.instances["web-01"]["status"])
	}
}

func TestSetMeta(t *testing.T) {
	f, c := createdVM(t, true)
	ctx := context.Background()
	if err := c.SetMeta(ctx, "web-01", workload.Meta{Owner: workload.OwnerGitOps, Protected: true}); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if cfgOf(f)[keyOwner] != "gitops" || cfgOf(f)[keyProtected] != "true" || f.instances["web-01"]["status"] != "Running" {
		t.Errorf("config %v", cfgOf(f))
	}
	if strings.Join(f.calls, ",") != "put" {
		t.Errorf("calls %v (a meta change must not restart the VM)", f.calls)
	}
	if err := c.SetMeta(ctx, "web-01", workload.Meta{Owner: workload.OwnerCLI}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfgOf(f)[keyOwner]; ok {
		t.Errorf("owner key kept for cli: %v", cfgOf(f))
	}
	if err := c.SetMeta(ctx, "web-01", workload.Meta{Owner: "root"}); !errors.Is(err, workload.ErrRefused) {
		t.Errorf("unknown owner: %v", err)
	}
	cfgOf(f)["limits.cpu"] = "16"
	if err := c.SetMeta(ctx, "web-01", workload.Meta{Owner: workload.OwnerGitOps}); !errors.Is(err, workload.ErrConflict) {
		t.Errorf("drifted VM: %v", err)
	}
}

func TestHost(t *testing.T) {
	f, c := createdVM(t, false)
	f.pools = []string{"fast", "bulk"}
	f.networks = []map[string]any{{"name": "lan", "managed": true}, {"name": "eno1", "managed": false}}
	f.volumes["fast/other"] = true
	h, err := c.Host(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.Pools, []string{"bulk", "fast"}) || !slices.Equal(h.Networks, []string{"lan"}) ||
		!slices.Equal(h.Volumes, []string{"bulk/web-01-data", "fast/other"}) {
		t.Errorf("host %+v", h)
	}
}

func TestLastSegment(t *testing.T) {
	for in, want := range map[string]string{
		"/1.0/storage-pools/fast":                          "fast",
		"/1.0/storage-pools/a/volumes/custom/b%20c?x=1":    "b c",
		"/1.0/storage-pools/a/volumes/custom/web-01-data/": "web-01-data",
	} {
		if got, err := lastSegment(in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	if _, err := lastSegment("/"); err == nil {
		t.Error("accepted /")
	}
}
