package docker

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"

	rt "github.com/wstein/workharbor/internal/runtime"
)

// checkSocketMounts supplements Prepare's general deny-list with this selected
// endpoint, including its canonical target. A read-only parent still exposes a
// socket's daemon authority, so no ancestor may enter the guest.
func (a *Adapter) checkSocketMounts(mounts []rt.Mount) error {
	endpoint, _ := url.Parse(a.endpoint) // validated by New
	targets := []string{endpoint.Path}
	if resolved, err := filepath.EvalSymlinks(endpoint.Path); err == nil {
		targets = append(targets, resolved)
	} else if parent, err := filepath.EvalSymlinks(filepath.Dir(endpoint.Path)); err == nil {
		targets = append(targets, filepath.Join(parent, filepath.Base(endpoint.Path)))
	}
	for _, m := range mounts {
		if m.Kind == rt.MountVolume {
			continue
		}
		for _, target := range targets {
			for ancestor := filepath.Clean(target); ; ancestor = filepath.Dir(ancestor) {
				equal := strings.EqualFold(filepath.Clean(m.Source), ancestor)
				if sourceInfo, err := os.Stat(m.Source); err == nil {
					if targetInfo, err := os.Stat(ancestor); err == nil {
						equal = os.SameFile(sourceInfo, targetInfo)
					}
				}
				if equal {
					return &rt.MountError{Source: m.Source, Resolved: m.Source, Reason: rt.ReasonRuntimeSocket}
				}
				if filepath.Dir(ancestor) == ancestor {
					break
				}
			}
		}
	}
	return nil
}
