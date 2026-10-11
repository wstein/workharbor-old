package docker

import (
	"context"
	"errors"
	"net/http"

	rt "github.com/wstein/workharbor/internal/runtime"
)

// StorageState describes evidence for enforcement on one resource, never a
// whole-environment promise inferred from the daemon's storage driver.
type StorageState string

const (
	StorageSupported   StorageState = "supported"
	StorageUnsupported StorageState = "unsupported"
	StorageUnknown     StorageState = "unknown"
)

// StorageScope identifies an exact prepared writable resource. A planned
// memory bound is separate from disk quota enforcement and live measurement.
type StorageScope struct {
	Kind            string       `json:"kind"`
	Target          string       `json:"target"`
	Source          string       `json:"source,omitempty"`
	ReadOnly        bool         `json:"read_only,omitempty"`
	State           StorageState `json:"state"`
	Mechanism       string       `json:"mechanism"`
	Evidence        string       `json:"evidence"`
	PlannedMemoryMB int          `json:"planned_memory_mb,omitempty"`
}

// StorageReport is read-only diagnostic evidence. It neither seals a quota
// plan nor changes Provision admission. The legacy DiskMB requirement remains
// mandatory; no scope currently has verified disk enforcement in this adapter.
type StorageReport struct {
	Driver            string         `json:"driver"`
	BackingFilesystem string         `json:"backing_filesystem"`
	RequestedDiskMB   int            `json:"requested_disk_mb"`
	Scopes            []StorageScope `json:"scopes"`
}

// StorageEvidence reads only Engine /info, retaining a strict allowlist of
// driver metadata. Host Btrfs, Driver=btrfs and DriverStatus do not establish
// enabled/consistent qgroups or exact mounted-resource coverage. In particular,
// Docker rootfs storage-opt size cannot bound named volumes or workspace binds.
// No probe creates a resource, applies storage options, or enables host quotas.
func (a *Adapter) StorageEvidence(ctx context.Context, p rt.PreparedSpec) (StorageReport, error) {
	if !p.Prepared() {
		return StorageReport{}, rt.ErrNotPrepared
	}
	s := p.Spec()
	if s.Owner != a.owner {
		return StorageReport{}, rt.ErrNotOwned
	}
	if err := a.checkSocketMounts(s.Mounts); err != nil {
		return StorageReport{}, err
	}
	report := StorageReport{Driver: "unknown", BackingFilesystem: "unknown", RequestedDiskMB: s.DiskMB, Scopes: []StorageScope{{Kind: "rootfs", Target: "/", ReadOnly: s.ReadOnlyRoot, State: StorageUnknown, Mechanism: "no verified rootfs disk quota mechanism", Evidence: "read-only root is prepared; actual storage quota enforcement is unverified"}}}
	private, hasPrivate := p.PrivateExecVolume()
	for _, m := range s.Mounts {
		if m.ReadOnly {
			continue
		}
		scope := StorageScope{Kind: string(m.Kind), Target: m.Target, Source: m.Source, State: StorageUnsupported, Mechanism: "no trusted mounted-resource quota controller", Evidence: "current socket adapter cannot set or verify a disk limit on this exact prepared resource"}
		if hasPrivate && m.Kind == rt.MountVolume && m.Source == private.Name && m.Target == rt.PrivateExecTarget {
			scope.State = StorageUnknown
			scope.PlannedMemoryMB = private.MemoryMB
			scope.Mechanism = "sealed bounded private tmpfs volume plan"
			scope.Evidence = "planned memory bound only; actual volume identity/options and live enforcement are not inspected by this report"
		}
		report.Scopes = append(report.Scopes, scope)
	}
	for _, target := range s.Tmpfs {
		report.Scopes = append(report.Scopes, StorageScope{Kind: "tmpfs", Target: target, State: StorageUnknown, Mechanism: "kernel tmpfs", Evidence: "prepared temporary memory mount; no explicit DiskMB limit or live enforcement is established by Engine info"})
	}
	var info struct {
		Driver       string
		DriverStatus [][]string
	}
	if err := a.request(ctx, http.MethodGet, "/info", nil, &info); err != nil {
		return report, errors.Join(ctx.Err(), errors.New("docker: Engine storage information unavailable; quota enforcement remains unverified"))
	}
	switch info.Driver {
	case "btrfs", "overlay2", "overlayfs", "zfs", "windowsfilter", "vfs", "fuse-overlayfs":
		report.Driver = info.Driver
	}
	// Duplicated or malformed evidence stays unknown rather than accepting
	// whichever row happens to be last. No other DriverStatus values are exposed.
	seen := false
	for _, row := range info.DriverStatus {
		if len(row) != 2 || row[0] != "Backing Filesystem" {
			continue
		}
		if seen {
			report.BackingFilesystem = "unknown"
			break
		}
		seen = true
		switch row[1] {
		case "btrfs", "xfs", "extfs", "ext4", "zfs":
			report.BackingFilesystem = row[1]
		}
	}
	switch report.Driver {
	case "btrfs":
		report.Scopes[0].Mechanism = "storage-opt size candidate (Btrfs rootfs only)"
		report.Scopes[0].Evidence = "qgroup enabled/consistent state and exact rootfs limit unverified; applying size may enable host quotas; mounted data is separate"
	case "overlay2":
		report.Scopes[0].Mechanism = "storage-opt size candidate (overlay2 rootfs only)"
		report.Scopes[0].Evidence = "XFS project quota support and exact rootfs limit unverified; backing filesystem alone is insufficient; mounted data is separate"
	case "zfs", "windowsfilter":
		report.Scopes[0].Mechanism = "storage-opt size candidate (rootfs only)"
		report.Scopes[0].Evidence = "driver-specific support and exact rootfs limit unverified; mounted data is separate"
	}
	return report, nil
}
