package runtime

import (
	"path"
	"strings"
)

// PrivateExecTarget is a fixed auxiliary mount outside repositories, persistent
// login homes and the read-only tool stores. Its parent is the read-only root.
const PrivateExecTarget = "/whr-exec"

// PrivateExecVolume is a trusted supervisor request, never repository driver
// options. Prepare checks ownership, bounds and overlap before sealing it.
type PrivateExecVolume struct {
	Name     string
	MemoryMB int
}

// PrivateExecPlan is the backend-neutral, bounded memory storage requirement.
// A backend that cannot provide it must reject it; disk fallback is forbidden.
type PrivateExecPlan struct {
	Name        string
	MemoryMB    int
	Owner       string
	Association string
}

// PrivateExecVolume returns a value copy of the trusted preparation's plan.
func (p PreparedSpec) PrivateExecVolume() (PrivateExecPlan, bool) {
	if p.privateExec == nil {
		return PrivateExecPlan{}, false
	}
	return *p.privateExec, true
}

func preparePrivateExec(opts PrepareOptions, s *Spec) (*PrivateExecPlan, error) {
	request := opts.PrivateExecVolume
	if request == nil {
		return nil, nil
	}
	var problems []string
	if !volumeRe.MatchString(request.Name) || opts.Owns == nil || !opts.Owns(request.Name) {
		problems = append(problems, "private exec volume does not belong to this environment")
	}
	if request.MemoryMB < 2 || request.MemoryMB > 16 {
		problems = append(problems, "private exec memory bound must be from 2 to 16 MiB")
	}
	if s.Network.Name == "" {
		problems = append(problems, "private exec volume requires an explicit environment association")
	}
	overlaps := func(target string) bool {
		target = path.Clean(target)
		return target == PrivateExecTarget || strings.HasPrefix(target, PrivateExecTarget+"/") || strings.HasPrefix(PrivateExecTarget, target+"/") || target == "/"
	}
	for _, mount := range s.Mounts {
		if mount.Kind == MountVolume && mount.Source == request.Name {
			problems = append(problems, "private exec volume cannot be aliased by another mount")
		}
		if overlaps(mount.Target) {
			problems = append(problems, "private exec target overlaps another prepared mount")
		}
	}
	for _, target := range s.Tmpfs {
		if overlaps(target) {
			problems = append(problems, "private exec target overlaps another prepared tmpfs")
		}
	}
	if len(problems) > 0 {
		return nil, &SpecError{Problems: problems}
	}
	s.Mounts = append(s.Mounts, Mount{Kind: MountVolume, Source: request.Name, Target: PrivateExecTarget})
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &PrivateExecPlan{Name: request.Name, MemoryMB: request.MemoryMB, Owner: s.Owner, Association: s.Network.Name}, nil
}
