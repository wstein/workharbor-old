package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// QuotaRequest names an operator-registered allocation, never a host path or
// command selector supplied by a repository. Mounts are the ordinary checked
// writable mounts, excluding the separately bounded private memory volume.
type QuotaRequest struct {
	Allocation, DaemonID, HostID, Environment string
	RequestedBytes                            uint64
	Mounts                                    []Mount
}

// QuotaResourceProof is daemon-host evidence for one exact writable mount.
// VolumeMountpoint and VolumeDriver record the claimed Engine binding; fresh
// Engine verification is still required before any future admission.
// HostDevice and HostInode bind a resolved bind mount to the local identity too.
// NestedSubvolumesObserved is an observation, not ongoing confinement.
type QuotaResourceProof struct {
	Mount                          Mount
	FSUUID                         string
	SubvolumeID                    uint64
	CoveringQgroup, HostPath       string
	HostDevice, HostInode          uint64
	VolumeDriver, VolumeMountpoint string
	NestedSubvolumesObserved       int
}

// QuotaProof records a read-only host-helper snapshot. The operator installs and
// controls that helper outside the guest; this API cannot enable quotas, change
// limits, allocate paths or grant privileges. Version 1 describes a dedicated
// full-qgroup referenced-byte aggregate covering all requested data mounts.
type QuotaProof struct {
	Version                                                               int
	Allocation, DaemonID, HostID, Environment, Mode, FSUUID, ParentQgroup string
	Enabled, Consistent, RescanRunning, Dedicated                         bool
	MaxReferencedBytes                                                    uint64
	Resources                                                             []QuotaResourceProof
}

// QuotaObservation preserves checked evidence without enabling admission.
// A healthy snapshot does not prevent future nested subvolume creation, cover
// tmpfs or prove actual target enforcement. All those gates remain unmet.
type QuotaObservation struct{ proof QuotaProof }

// Proof returns a value copy, including the resource inventory.
func (q QuotaObservation) Proof() QuotaProof {
	p := q.proof
	p.Resources = cloneSlice(p.Resources)
	return p
}

// AdmissionSupported remains false until resource rechecks, ongoing nested
// containment and every writable memory scope are implemented and reviewed.
func (QuotaObservation) AdmissionSupported() bool { return false }

// QuotaResolver is the only host-helper operation: read registered evidence.
// Implementations must honor cancellation and enforce a bounded wire response.
// There is deliberately no arbitrary path, command or quota mutation method.
type QuotaResolver interface {
	ResolveQuota(context.Context, QuotaRequest) (QuotaProof, error)
}

// QuotaResolverFunc adapts a trusted read-only resolver.
type QuotaResolverFunc func(context.Context, QuotaRequest) (QuotaProof, error)

func (f QuotaResolverFunc) ResolveQuota(c context.Context, r QuotaRequest) (QuotaProof, error) {
	return f(c, r)
}

// QuotaPreparation is trusted supervisor configuration, never Spec metadata.
// Expected identities must come from operator configuration, not the response.
// This source seam does not install or connect a production host helper.
type QuotaPreparation struct {
	Allocation, DaemonID, HostID string
	Resolve                      QuotaResolver
}

var (
	quotaToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	quotaUUID  = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)
)

func quotaUnsupported() error {
	return fmt.Errorf("%w: incomplete or inconsistent registered quota evidence", ErrUnsupported)
}

func verifyQuotaProof(r QuotaRequest, p QuotaProof) (QuotaObservation, error) {
	for _, id := range []string{r.Allocation, r.DaemonID, r.HostID, r.Environment} {
		if !quotaToken.MatchString(id) {
			return QuotaObservation{}, quotaUnsupported()
		}
	}
	if r.RequestedBytes == 0 || len(r.Mounts) == 0 || len(r.Mounts) > 16 ||
		p.Version != 1 || p.Allocation != r.Allocation || p.DaemonID != r.DaemonID ||
		p.HostID != r.HostID || p.Environment != r.Environment || p.Mode != "full-qgroup" ||
		!p.Enabled || !p.Consistent || p.RescanRunning || !p.Dedicated ||
		p.MaxReferencedBytes == 0 || p.MaxReferencedBytes > r.RequestedBytes ||
		!quotaUUID.MatchString(p.FSUUID) || !aggregateQgroup(p.ParentQgroup) || len(p.Resources) != len(r.Mounts) {
		return QuotaObservation{}, quotaUnsupported()
	}
	seenTargets, seenSubvolumes := map[string]bool{}, map[uint64]bool{}
	for i, m := range r.Mounts {
		e := p.Resources[i]
		if m.ReadOnly || (m.Kind != MountBind && m.Kind != MountVolume) || e.Mount != m ||
			seenTargets[m.Target] || seenSubvolumes[e.SubvolumeID] || e.FSUUID != p.FSUUID ||
			e.SubvolumeID < 256 || e.CoveringQgroup != p.ParentQgroup ||
			e.HostDevice == 0 || e.HostInode == 0 || !quotaHostPath(e.HostPath) || e.NestedSubvolumesObserved != 0 {
			return QuotaObservation{}, quotaUnsupported()
		}
		if m.Kind == MountBind {
			if e.HostPath != m.Source || e.VolumeDriver != "" || e.VolumeMountpoint != "" {
				return QuotaObservation{}, quotaUnsupported()
			}
		} else if e.VolumeDriver != "local" || e.VolumeMountpoint != e.HostPath {
			return QuotaObservation{}, quotaUnsupported()
		}
		seenTargets[m.Target], seenSubvolumes[e.SubvolumeID] = true, true
	}
	return QuotaObservation{proof: QuotaObservation{proof: p}.Proof()}, nil
}

func aggregateQgroup(s string) bool {
	a, b, ok := strings.Cut(s, "/")
	level, err := strconv.ParseUint(a, 10, 16)
	id, idErr := strconv.ParseUint(b, 10, 48)
	return ok && err == nil && idErr == nil && level > 0 && id > 0 &&
		s == strconv.FormatUint(level, 10)+"/"+strconv.FormatUint(id, 10)
}

func quotaHostPath(s string) bool {
	return len(s) <= 4096 && filepath.IsAbs(s) && filepath.Clean(s) == s && s != "/" && !strings.ContainsAny(s, "\x00\r\n")
}

func prepareQuota(opts PrepareOptions, s Spec, private *PrivateExecPlan) (*QuotaObservation, error) {
	if opts.DockerQuota == nil {
		return nil, nil
	}
	config := *opts.DockerQuota
	if config.Resolve == nil || s.DiskMB < 1 || int64(s.DiskMB) > int64(^uint64(0)>>20) {
		return nil, quotaUnsupported()
	}
	r := QuotaRequest{
		Allocation: config.Allocation, DaemonID: config.DaemonID, HostID: config.HostID,
		Environment: s.Network.Name, RequestedBytes: uint64(s.DiskMB) << 20,
	}
	for _, m := range s.Mounts {
		if !m.ReadOnly && (private == nil || m.Target != PrivateExecTarget) {
			r.Mounts = append(r.Mounts, m)
		}
	}
	for _, id := range []string{r.Allocation, r.DaemonID, r.HostID, r.Environment} {
		if !quotaToken.MatchString(id) {
			return nil, quotaUnsupported()
		}
	}
	if len(r.Mounts) == 0 || len(r.Mounts) > 16 {
		return nil, quotaUnsupported()
	}
	// A repository has already passed every ordinary mount and hardening check.
	// The resolver receives its own copy so it cannot rewrite the checked plan.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resolverRequest := r
	resolverRequest.Mounts = cloneSlice(r.Mounts)
	p, err := config.Resolve.ResolveQuota(ctx, resolverRequest)
	if err != nil || ctx.Err() != nil {
		return nil, quotaUnsupported()
	}
	observation, err := verifyQuotaProof(r, p)
	if err != nil {
		return nil, err
	}
	for _, e := range observation.proof.Resources {
		if e.Mount.Kind != MountBind {
			continue
		}
		// No remapping or same-string-only assumption across devcontainer/daemon
		// hosts. Unsupported stat identity/topology is refused, never bypassed.
		info, statErr := opts.FS.Stat(e.Mount.Source)
		if statErr != nil {
			return nil, quotaUnsupported()
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || uint64(st.Dev) != e.HostDevice || st.Ino != e.HostInode { //nolint:unconvert // Dev is int32 on darwin and uint64 on linux.
			return nil, quotaUnsupported()
		}
	}
	return &observation, nil
}

// DockerQuotaObservation returns copied snapshot evidence, never permission to
// provision. Docker still refuses every positive requested disk quota.
func (p PreparedSpec) DockerQuotaObservation() (QuotaObservation, bool) {
	if p.dockerQuota == nil {
		return QuotaObservation{}, false
	}
	return QuotaObservation{proof: p.dockerQuota.Proof()}, true
}
