package runtime

import (
	"errors"
	"testing"
)

func TestPrepareSealsBoundedPrivateExecVolumeAndMount(t *testing.T) {
	r := newPrepRig(t)
	request := &PrivateExecVolume{Name: "task1-exec", MemoryMB: 2}
	r.opts.PrivateExecVolume = request
	p, err := Prepare(r.opts, goodSpec())
	if err != nil {
		t.Fatal(err)
	}
	request.MemoryMB = 0
	plan, ok := p.PrivateExecVolume()
	if !ok || plan.Name != "task1-exec" || plan.MemoryMB != 2 || plan.Owner != "wh" || plan.Association != "net1" {
		t.Fatalf("sealed plan = %+v %v", plan, ok)
	}
	s := p.Spec()
	if len(s.Mounts) != 1 || s.Mounts[0] != (Mount{Kind: MountVolume, Source: "task1-exec", Target: PrivateExecTarget}) {
		t.Fatalf("plan mount = %+v", s.Mounts)
	}
	plan.MemoryMB = 0
	again, _ := p.PrivateExecVolume()
	if again.MemoryMB != 2 {
		t.Fatal("caller mutated sealed plan")
	}
}

func TestPrepareRejectsUnownedUnboundedOrOverlappingExecVolume(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request PrivateExecVolume
		target  string
	}{
		{"unowned", PrivateExecVolume{Name: "other", MemoryMB: 2}, ""},
		{"zero bound", PrivateExecVolume{Name: "task1-exec"}, ""},
		{"negative bound", PrivateExecVolume{Name: "task1-exec", MemoryMB: -1}, ""},
		{"excessive bound", PrivateExecVolume{Name: "task1-exec", MemoryMB: 1024}, ""},
		{"same target", PrivateExecVolume{Name: "task1-exec", MemoryMB: 2}, PrivateExecTarget},
		{"target descendant", PrivateExecVolume{Name: "task1-exec", MemoryMB: 2}, PrivateExecTarget + "/nested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPrepRig(t)
			r.opts.PrivateExecVolume = &tc.request
			spec := goodSpec()
			if tc.target != "" {
				spec.Tmpfs = []string{tc.target}
			}
			if _, err := Prepare(r.opts, spec); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("unsafe exec plan = %v", err)
			}
		})
	}
}

func TestPrepareDoesNotAliasPrivateExecStorageThroughAnotherMount(t *testing.T) {
	r := newPrepRig(t)
	r.opts.PrivateExecVolume = &PrivateExecVolume{Name: "task1-exec", MemoryMB: 2}
	spec := goodSpec()
	spec.Mounts = []Mount{{Kind: MountVolume, Source: "task1-exec", Target: "/alias"}}
	if _, err := Prepare(r.opts, spec); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("private volume alias accepted: %v", err)
	}
}
