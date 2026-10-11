package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wstein/workharbor/internal/runtime"
)

func storagePrepared(t *testing.T) runtime.PreparedSpec {
	t.Helper()
	workspace := t.TempDir()
	s := runtime.Spec{
		Image: "golang:1", Owner: "mine", CPUs: 1, MemoryMB: 256, DiskMB: 1024, Network: runtime.Network{Name: "mine-net", Internal: true}, User: "1000:1000", ReadOnlyRoot: true, CapDrop: []string{"ALL"}, Init: true,
		Mounts: []runtime.Mount{{Kind: runtime.MountVolume, Source: "mine-home", Target: "/home/agent"}, {Kind: runtime.MountBind, Source: workspace, Target: "/workspace"}, {Kind: runtime.MountBind, Source: t.TempDir(), Target: "/tools", ReadOnly: true}},
		Tmpfs:  []string{"/tmp", "/var/tmp/whr/build"},
	}
	p, err := runtime.Prepare(runtime.PrepareOptions{FS: runtime.OSFS{}, Home: t.TempDir(), Roots: []string{filepath.Dir(workspace)}, Owns: func(name string) bool { return name == "mine-home" || name == "mine-exec" }, PrivateExecVolume: &runtime.PrivateExecVolume{Name: "mine-exec", MemoryMB: 2}}, s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStorageEvidenceDoesNotMistakeBtrfsRootfsForDataQuotas(t *testing.T) {
	calls := 0
	endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/v1.45/info" {
			t.Errorf("unexpected operation %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		_, _ = io.WriteString(w, `{"Driver":"btrfs","DriverStatus":[["Btrfs",""],["Backing Filesystem","btrfs"],["Unrelated","private-daemon-value"]],"Name":"private-daemon-value","DockerRootDir":"/private-daemon-value"}`)
	}))
	a, err := New("mine", endpoint, "/tools/whr-shim")
	if err != nil {
		t.Fatal(err)
	}
	p := storagePrepared(t)
	report, err := a.StorageEvidence(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if report.Driver != "btrfs" || report.BackingFilesystem != "btrfs" || report.RequestedDiskMB != 1024 {
		t.Fatalf("report=%+v", report)
	}
	scopes := map[string]StorageScope{}
	for _, scope := range report.Scopes {
		scopes[scope.Target] = scope
		if scope.State == StorageSupported {
			t.Fatalf("unmeasured enforcement claimed: %+v", scope)
		}
	}
	for _, target := range []string{"/", "/home/agent", "/workspace", "/tmp", "/var/tmp/whr/build", runtime.PrivateExecTarget} {
		if _, ok := scopes[target]; !ok {
			t.Errorf("scope missing %s", target)
		}
	}
	if _, ok := scopes["/tools"]; ok {
		t.Fatal("read-only tools counted as writable data")
	}
	if scopes["/"].State != StorageUnknown || !strings.Contains(scopes["/"].Mechanism, "storage-opt size") {
		t.Fatal("rootfs candidate/evidence gap lost")
	}
	if scopes["/home/agent"].State != StorageUnsupported || scopes["/workspace"].State != StorageUnsupported {
		t.Fatal("data quota gap hidden")
	}
	if scopes["/home/agent"].Source != "mine-home" || scopes["/workspace"].Source != p.Spec().Mounts[1].Source {
		t.Fatal("checked resource identity lost")
	}
	if scopes[runtime.PrivateExecTarget].PlannedMemoryMB != 2 || scopes[runtime.PrivateExecTarget].State != StorageUnknown {
		t.Fatal("planned memory bound misrepresented")
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-daemon-value") {
		t.Fatal("unrelated Engine info leaked into diagnostics")
	}
	if _, err := a.Provision(context.Background(), p); !errors.Is(err, runtime.ErrUnsupported) {
		t.Fatalf("quota admission changed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("report/provision performed %d calls, want one read", calls)
	}
}

func TestStorageEvidenceAllowlistAndUnavailableInfo(t *testing.T) {
	for index, body := range []string{`{"Driver":"overlay2","DriverStatus":[["Backing Filesystem","xfs"]]}`, `{"Driver":"secret\nvalue","DriverStatus":[["Backing Filesystem","secret\nvalue"]]}`, `{"Driver":"btrfs","DriverStatus":[["Backing Filesystem","btrfs"],["Backing Filesystem","xfs"]]}`, `failure`} {
		t.Run(fmt.Sprintf("info-%d", index), func(t *testing.T) {
			endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if body == "failure" {
					w.WriteHeader(500)
					_, _ = io.WriteString(w, "secret\nvalue")
					return
				}
				_, _ = io.WriteString(w, body)
			}))
			a, err := New("mine", endpoint, "/tools/shim")
			if err != nil {
				t.Fatal(err)
			}
			report, err := a.StorageEvidence(context.Background(), storagePrepared(t))
			if index == 0 && (report.Driver != "overlay2" || report.BackingFilesystem != "xfs") {
				t.Fatal("allowlisted fields lost")
			}
			if index > 0 && report.BackingFilesystem != "unknown" {
				t.Fatal("ambiguous or invalid backing filesystem accepted")
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("daemon error body printed")
			}
			if body == "failure" && err == nil {
				t.Fatal("unavailable info silently passed")
			}
			data, _ := json.Marshal(report)
			if strings.Contains(string(data), "secret") {
				t.Fatal("unallowlisted metadata printed")
			}
			for _, scope := range report.Scopes {
				if scope.State == StorageSupported {
					t.Fatal("driver status inferred enforcement")
				}
			}
		})
	}
}

func TestStorageEvidenceRejectsUncheckedOrForeignBeforeContact(t *testing.T) {
	endpoint := socketServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached Engine") }))
	a, err := New("other", endpoint, "/tools/shim")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.StorageEvidence(context.Background(), runtime.PreparedSpec{}); !errors.Is(err, runtime.ErrNotPrepared) {
		t.Fatal(err)
	}
	if _, err := a.StorageEvidence(context.Background(), storagePrepared(t)); !errors.Is(err, runtime.ErrNotOwned) {
		t.Fatal(err)
	}
}

func TestStorageEvidenceRefusesSocketAncestorMountBeforeRead(t *testing.T) {
	p := storagePrepared(t)
	socket := filepath.Join(p.Spec().Mounts[1].Source, "engine.sock")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New("mine", "unix://"+socket, "/tools/shim")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.StorageEvidence(context.Background(), p); !errors.Is(err, runtime.ErrForbiddenMount) {
		t.Fatalf("socket ancestor accepted: %v", err)
	}
}

func TestQuotaObservationCannotAdmitBeforeEngineCreation(t *testing.T) {
	calls := 0
	endpoint := socketServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	a, err := New("mine", endpoint, "/tools/whr-shim")
	if err != nil {
		t.Fatal(err)
	}
	s := storagePrepared(t).Spec()
	s.Mounts = []runtime.Mount{{Kind: runtime.MountVolume, Source: "mine-home", Target: "/home/agent"}}
	quota := &runtime.QuotaPreparation{Allocation: "registered-1", DaemonID: "daemon-1", HostID: "host-1", Resolve: runtime.QuotaResolverFunc(func(_ context.Context, q runtime.QuotaRequest) (runtime.QuotaProof, error) {
		return runtime.QuotaProof{Version: 1, Allocation: q.Allocation, DaemonID: q.DaemonID, HostID: q.HostID, Environment: q.Environment, Mode: "full-qgroup", Enabled: true, Consistent: true, Dedicated: true, FSUUID: "11111111-1111-1111-1111-111111111111", ParentQgroup: "1/100", MaxReferencedBytes: q.RequestedBytes, Resources: []runtime.QuotaResourceProof{{Mount: q.Mounts[0], FSUUID: "11111111-1111-1111-1111-111111111111", SubvolumeID: 256, CoveringQgroup: "1/100", HostPath: "/operator/home", HostDevice: 1, HostInode: 100, VolumeDriver: "local", VolumeMountpoint: "/operator/home"}}}, nil
	})}
	p, err := runtime.Prepare(runtime.PrepareOptions{FS: runtime.OSFS{}, Home: t.TempDir(), Owns: func(string) bool { return true }, DockerQuota: quota}, s)
	if err != nil {
		t.Fatal(err)
	}
	obs, ok := p.DockerQuotaObservation()
	if !ok || obs.AdmissionSupported() {
		t.Fatal("healthy snapshot claimed admission")
	}
	if _, err := a.Provision(context.Background(), p); !errors.Is(err, runtime.ErrUnsupported) {
		t.Fatal("positive quota admitted", err)
	}
	if calls != 0 {
		t.Fatalf("incomplete enforcement made %d Engine calls", calls)
	}
}
