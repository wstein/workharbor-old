package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"

	"github.com/wstein/workharbor/internal/runtime"
)

const (
	leaseRole             = "volume-lease"
	leaseVolumeLabel      = "workharbor.lease-volume"
	leaseEnvironmentLabel = "workharbor.lease-environment"
)

func leaseName(volume string) string {
	sum := sha256.Sum256([]byte(volume))
	return "whr-volume-lease-" + hex.EncodeToString(sum[:])
}

// acquireLeases uses Engine's atomic container-name uniqueness across clients
// and supervisor processes. A stopped lease is held, never evidence of staleness:
// its holder may be between acquiring the lease and starting the environment.
// Failed starts retain claims until an explicit Stop/Delete of their exact
// environment, so a retry cannot race another process's start.
func (a *Adapter) acquireLeases(ctx context.Context, c container) error {
	var volumes []string
	for _, m := range c.Mounts {
		if m.Type == "volume" && m.RW {
			volumes = append(volumes, m.Name)
		}
	}
	sort.Strings(volumes)
	for _, volume := range volumes {
		ls := labels(a.owner, leaseRole)
		ls[leaseVolumeLabel] = volume
		ls[leaseEnvironmentLabel] = c.ID
		body := map[string]any{"Image": c.Config.Image, "Labels": ls, "User": c.Config.User, "Entrypoint": []string{"true"}, "HostConfig": map[string]any{"NetworkMode": "none", "ReadonlyRootfs": true, "CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"}}}
		var created struct {
			ID string `json:"Id"`
		}
		err := a.request(ctx, http.MethodPost, "/containers/create?name="+leaseName(volume), body, &created)
		if err == nil {
			continue
		}
		var api *apiError
		if !errors.As(err, &api) || api.status != http.StatusConflict {
			return err
		}
		var existing container
		if err := a.request(ctx, http.MethodGet, "/containers/"+leaseName(volume)+"/json", nil, &existing); err != nil {
			return err
		}
		if !owned(existing.Config.Labels, a.owner, leaseRole) || existing.Config.Labels[leaseVolumeLabel] != volume {
			return runtime.ErrNotOwned
		}
		if existing.Config.Labels[leaseEnvironmentLabel] != c.ID {
			return runtime.ErrVolumeBusy
		}
	}
	return nil
}

// releaseLeases is called only after this exact owned environment is known to be
// stopped. It never guesses whether another environment's claim is stale.
func (a *Adapter) releaseLeases(ctx context.Context, id string) error {
	all, err := a.containers(ctx)
	if err != nil {
		return err
	}
	for _, lease := range all {
		if !owned(lease.Config.Labels, a.owner, leaseRole) || lease.Config.Labels[leaseEnvironmentLabel] != id {
			continue
		}
		volume := lease.Config.Labels[leaseVolumeLabel]
		if volume == "" || lease.State.Running {
			return runtime.ErrVolumeBusy
		}
		// Recheck exact ID and labels immediately before deletion, including the
		// deterministic name's resource association recorded at creation.
		var checked container
		if err := a.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(lease.ID)+"/json", nil, &checked); missing(err) {
			continue
		} else if err != nil {
			return err
		}
		if checked.ID != lease.ID || !owned(checked.Config.Labels, a.owner, leaseRole) || checked.Config.Labels[leaseEnvironmentLabel] != id || checked.Config.Labels[leaseVolumeLabel] != volume {
			return runtime.ErrNotOwned
		}
		if checked.State.Running {
			return runtime.ErrVolumeBusy
		}
		if err := a.request(ctx, http.MethodDelete, "/containers/"+url.PathEscape(checked.ID), nil, nil); err != nil && !missing(err) {
			return err
		}
	}
	return nil
}
