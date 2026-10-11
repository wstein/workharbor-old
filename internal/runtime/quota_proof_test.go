package runtime

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestQuotaProofRefusesUnhealthyOrWrongAllocationEvidence(t *testing.T) {
	for _, name := range []string{"unknown", "disabled", "inconsistent", "rescan", "wrong daemon", "wrong host", "wrong allocation", "rootfs only", "wrong filesystem", "wrong subvolume", "wrong mount", "individual limits", "oversized aggregate", "nested"} {
		t.Run(name, func(t *testing.T) {
			request, proof := quotaFixture()
			switch name {
			case "unknown":
				proof.Version = 0
			case "disabled":
				proof.Enabled = false
			case "inconsistent":
				proof.Consistent = false
			case "rescan":
				proof.RescanRunning = true
			case "wrong daemon":
				proof.DaemonID = "other"
			case "wrong host":
				proof.HostID = "other"
			case "wrong allocation":
				proof.Allocation = "other"
			case "rootfs only":
				proof.Resources = nil
			case "wrong filesystem":
				proof.Resources[1].FSUUID = "other"
			case "wrong subvolume":
				proof.Resources[1].SubvolumeID = 0
			case "wrong mount":
				proof.Resources[1].Mount.Source = "/other"
			case "individual limits":
				proof.ParentQgroup = ""
			case "oversized aggregate":
				proof.MaxReferencedBytes = request.RequestedBytes + 1
			case "nested":
				proof.Resources[1].NestedSubvolumesObserved = 1
			}
			if _, err := verifyQuotaProof(request, proof); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("%s accepted: %v", name, err)
			}
		})
	}
}

func quotaFixture() (QuotaRequest, QuotaProof) {
	mounts := []Mount{{Kind: MountVolume, Source: "wh-home", Target: "/home/agent"}, {Kind: MountBind, Source: "/workspaces/task", Target: "/workspace"}}
	request := QuotaRequest{Allocation: "registered-1", DaemonID: "daemon-1", HostID: "host-1", Environment: "wh-net-task", RequestedBytes: 1024 << 20, Mounts: mounts}
	proof := QuotaProof{Version: 1, Allocation: request.Allocation, DaemonID: request.DaemonID, HostID: request.HostID, Environment: request.Environment, Mode: "full-qgroup", Enabled: true, Consistent: true, FSUUID: "11111111-1111-1111-1111-111111111111", ParentQgroup: "1/100", MaxReferencedBytes: request.RequestedBytes, Dedicated: true}
	for i, m := range mounts {
		proof.Resources = append(proof.Resources, QuotaResourceProof{Mount: m, FSUUID: proof.FSUUID, SubvolumeID: uint64(256 + i), CoveringQgroup: proof.ParentQgroup, HostPath: m.Source, HostDevice: 1, HostInode: uint64(100 + i)})
	}
	proof.Resources[0].HostPath = "/operator/home"
	proof.Resources[0].VolumeDriver = "local"
	proof.Resources[0].VolumeMountpoint = proof.Resources[0].HostPath
	return request, proof
}

func TestQuotaProofObservationDoesNotClaimOngoingNestedContainment(t *testing.T) {
	request, proof := quotaFixture()
	observation, err := verifyQuotaProof(request, proof)
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Proof().Resources) != len(proof.Resources) {
		t.Fatal("valid observation omitted exact resource evidence")
	}
	if observation.AdmissionSupported() {
		t.Fatal("inspection snapshot claimed ongoing containment")
	}
	proof.Resources[0].Mount.Source = "changed"
	request.Mounts[0].Source = "changed"
	if observation.Proof().Resources[0].Mount.Source != "wh-home" {
		t.Fatal("proof aliases caller data")
	}
	proofCopy := observation.Proof()
	proofCopy.Resources[0].Mount.Source = "changed-again"
	if observation.Proof().Resources[0].Mount.Source != "wh-home" {
		t.Fatal("proof accessor aliases sealed data")
	}
}

func TestPrepareQuotaResolverIsTrustedAndCannotBypassHardening(t *testing.T) {
	r := newPrepRig(t)
	s := goodSpec()
	calls := 0
	r.opts.DockerQuota = &QuotaPreparation{Allocation: "registered-1", DaemonID: "daemon-1", HostID: "host-1", Resolve: QuotaResolverFunc(func(context.Context, QuotaRequest) (QuotaProof, error) { calls++; return QuotaProof{}, nil })}
	s.ReadOnlyRoot = false
	if _, err := Prepare(r.opts, s); !errors.Is(err, ErrInvalidSpec) {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("invalid spec reached quota resolver")
	}
	s = goodSpec()
	s.Mounts = []Mount{{Kind: MountVolume, Source: "task1-home", Target: "/home/agent"}}
	if _, err := Prepare(r.opts, s); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("missing proof accepted: %v", err)
	}
	if calls != 1 {
		t.Fatalf("trusted resolver calls=%d", calls)
	}
	if _, err := Prepare(newPrepRig(t).opts, goodSpec()); err != nil {
		t.Fatal("generic preparation changed", err)
	}
}

func TestQuotaProofRefusesMalformedOrAmbiguousInventory(t *testing.T) {
	for _, name := range []string{"mode", "zero limit", "not dedicated", "bad uuid", "level zero", "noncanonical group", "duplicate subvolume", "duplicate target", "negative nested", "relative host", "volume driver", "volume identity", "extra resource", "readonly resource", "bad request identity"} {
		t.Run(name, func(t *testing.T) {
			r, p := quotaFixture()
			switch name {
			case "mode":
				p.Mode = "simple-quota"
			case "zero limit":
				p.MaxReferencedBytes = 0
			case "not dedicated":
				p.Dedicated = false
			case "bad uuid":
				p.FSUUID = "unknown"
			case "level zero":
				p.ParentQgroup = "0/100"
			case "noncanonical group":
				p.ParentQgroup = "01/100"
			case "duplicate subvolume":
				p.Resources[1].SubvolumeID = p.Resources[0].SubvolumeID
			case "duplicate target":
				r.Mounts[1].Target = r.Mounts[0].Target
				p.Resources[1].Mount = r.Mounts[1]
			case "negative nested":
				p.Resources[0].NestedSubvolumesObserved = -1
			case "relative host":
				p.Resources[0].HostPath = "relative"
			case "volume driver":
				p.Resources[0].VolumeDriver = "plugin"
			case "volume identity":
				p.Resources[0].VolumeMountpoint = "/other"
			case "extra resource":
				p.Resources = append(p.Resources, p.Resources[0])
			case "readonly resource":
				r.Mounts[0].ReadOnly = true
				p.Resources[0].Mount = r.Mounts[0]
			case "bad request identity":
				r.Allocation = "operator-secret\n"
			}
			if _, err := verifyQuotaProof(r, p); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("accepted %s: %v", name, err)
			}
		})
	}
}

func TestPrepareQuotaObservationSealsExactMountsWithoutAdmission(t *testing.T) {
	r := newPrepRig(t)
	s := goodSpec()
	s.Mounts = []Mount{{Kind: MountVolume, Source: "task1-home", Target: "/home/agent"}, {Kind: MountBind, Source: r.project, Target: "/workspace"}, {Kind: MountBind, Source: r.cache, Target: "/cache", ReadOnly: true}}
	r.opts.PrivateExecVolume = &PrivateExecVolume{Name: "task1-exec", MemoryMB: 2}
	var received QuotaRequest
	var response QuotaProof
	r.opts.DockerQuota = &QuotaPreparation{Allocation: "registered-1", DaemonID: "daemon-1", HostID: "host-1", Resolve: QuotaResolverFunc(func(ctx context.Context, req QuotaRequest) (QuotaProof, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("resolver has no deadline")
		}
		received = req
		_, response = quotaFixture()
		response.Environment = req.Environment
		response.Resources[0].Mount = req.Mounts[0]
		response.Resources[1].Mount = req.Mounts[1]
		response.Resources[1].HostPath = req.Mounts[1].Source
		info, err := os.Stat(req.Mounts[1].Source)
		if err != nil {
			t.Fatal(err)
		}
		st := info.Sys().(*syscall.Stat_t)
		response.Resources[1].HostDevice, response.Resources[1].HostInode = uint64(st.Dev), st.Ino //nolint:unconvert // Dev is int32 on darwin and uint64 on linux.
		req.Mounts[0].Source = "resolver-mutated"
		return response, nil
	})}
	p, err := Prepare(r.opts, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(received.Mounts) != 2 {
		t.Fatalf("wrong disk scopes: %+v", received.Mounts)
	}
	obs, ok := p.DockerQuotaObservation()
	if !ok || obs.AdmissionSupported() {
		t.Fatal("observation missing or admitted")
	}
	response.Resources[0].Mount.Source = "response-mutated"
	proofCopy := obs.Proof()
	proofCopy.Resources[0].Mount.Source = "accessor-mutated"
	again, _ := p.DockerQuotaObservation()
	if again.Proof().Resources[0].Mount.Source != "task1-home" || p.Spec().Mounts[0].Source != "task1-home" {
		t.Fatal("sealed proof or spec mutated")
	}
	// A local/daemon source that merely has the same path string is insufficient.
	response.Resources[1].HostInode++
	r.opts.DockerQuota.Resolve = QuotaResolverFunc(func(context.Context, QuotaRequest) (QuotaProof, error) {
		response.Resources[0].Mount.Source = "task1-home"
		return response, nil
	})
	if _, err := Prepare(r.opts, s); !errors.Is(err, ErrUnsupported) {
		t.Fatal("wrong host mount identity accepted", err)
	}
}

func TestPrepareQuotaRejectsBadConfigurationBeforeResolverAndSanitizesErrors(t *testing.T) {
	r := newPrepRig(t)
	s := goodSpec()
	s.Mounts = []Mount{{Kind: MountVolume, Source: "task1-home", Target: "/home/agent"}}
	calls := 0
	r.opts.DockerQuota = &QuotaPreparation{Allocation: "untrusted/path", DaemonID: "daemon-1", HostID: "host-1", Resolve: QuotaResolverFunc(func(context.Context, QuotaRequest) (QuotaProof, error) {
		calls++
		return QuotaProof{}, errors.New("helper-sensitive-value")
	})}
	if _, err := Prepare(r.opts, s); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("invalid selector reached helper")
	}
	r.opts.DockerQuota.Allocation = "registered-1"
	if _, err := Prepare(r.opts, s); !errors.Is(err, ErrUnsupported) || strings.Contains(err.Error(), "helper-sensitive") {
		t.Fatal("raw helper error escaped", err)
	}
	s.DiskMB = int(^uint(0) >> 1)
	calls = 0
	if _, err := Prepare(r.opts, s); !errors.Is(err, ErrUnsupported) || calls != 0 {
		t.Fatal("overflow reached helper", err, calls)
	}
}
