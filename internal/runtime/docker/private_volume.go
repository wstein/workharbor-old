package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"

	rt "github.com/wstein/workharbor/internal/runtime"
)

const (
	privateVolumeRole       = "private-exec-volume"
	privateAssociationLabel = "workharbor.private-association"
	privateSizeLabel        = "workharbor.private-memory-mb"
	privateNonceLabel       = "workharbor.private-identity"
	privateVolumeLabel      = "workharbor.private-volume"
)

var errPrivateCleanup = errors.New("private execution storage cleanup requires operator inspection")

func privateVolumeOptions(p rt.PrivateExecPlan) map[string]string {
	return map[string]string{"type": "tmpfs", "device": "tmpfs", "o": fmt.Sprintf("size=%d,uid=0,gid=0,mode=0711,nosuid,nodev,noexec", int64(p.MemoryMB)<<20)}
}

func (a *Adapter) checkPrivateVolume(v named, p rt.PrivateExecPlan) error {
	if p.Owner != a.owner || p.Association == "" || p.MemoryMB < 2 || p.MemoryMB > 16 || v.Name != p.Name || !owned(v.Labels, a.owner, privateVolumeRole) || v.Labels[privateAssociationLabel] != p.Association || v.Labels[privateSizeLabel] != strconv.Itoa(p.MemoryMB) || v.Labels[privateNonceLabel] == "" {
		return rt.ErrNotOwned
	}
	if v.Driver != "local" || !reflect.DeepEqual(v.Options, privateVolumeOptions(p)) {
		return fmt.Errorf("%w: private execution storage must be bounded root-protected memory", ErrUnsupported)
	}
	return nil
}

// ensurePrivateVolume never adopts even a matching orphan. A newly created
// volume must echo this creation's identity; Engine create is name-idempotent.
func (a *Adapter) ensurePrivateVolume(ctx context.Context, p rt.PrivateExecPlan) error {
	var v named
	err := a.request(ctx, http.MethodGet, "/volumes/"+url.PathEscape(p.Name), nil, &v)
	if err == nil {
		if err := a.checkPrivateVolume(v, p); err != nil {
			return err
		}
		return fmt.Errorf("docker: private execution storage already reserved; operator inspection required: %w", rt.ErrVolumeBusy)
	}
	if !missing(err) {
		return err
	}
	nonce, err := token()
	if err != nil {
		return err
	}
	ls := labels(a.owner, privateVolumeRole)
	ls[privateAssociationLabel], ls[privateSizeLabel], ls[privateNonceLabel] = p.Association, strconv.Itoa(p.MemoryMB), nonce
	body := map[string]any{"Name": p.Name, "Driver": "local", "DriverOpts": privateVolumeOptions(p), "Labels": ls}
	if err := a.request(ctx, http.MethodPost, "/volumes/create", body, &v); err != nil {
		return err
	}
	if err := a.checkPrivateVolume(v, p); err != nil {
		return err
	}
	if v.Labels[privateNonceLabel] != nonce {
		return rt.ErrVolumeBusy
	}
	return nil
}

func privateContainerPlan(c container) (rt.PrivateExecPlan, bool, error) {
	name := c.Config.Labels[privateVolumeLabel]
	if name == "" {
		return rt.PrivateExecPlan{}, false, nil
	}
	size, err := strconv.Atoi(c.Config.Labels[privateSizeLabel])
	if err != nil || c.Config.Labels[privateNonceLabel] == "" {
		return rt.PrivateExecPlan{}, true, rt.ErrNotOwned
	}
	p := rt.PrivateExecPlan{Name: name, MemoryMB: size, Owner: c.Config.Labels[rt.OwnerLabel], Association: c.Config.Labels[networkLabel]}
	count := 0
	for _, m := range c.Mounts {
		if m.Destination == rt.PrivateExecTarget || (m.Type == "volume" && m.Name == name) {
			if m.Type != "volume" || m.Name != name || m.Destination != rt.PrivateExecTarget || !m.RW {
				return p, true, rt.ErrNotOwned
			}
			count++
		}
	}
	if count != 1 {
		return p, true, rt.ErrNotOwned
	}
	return p, true, nil
}

func (a *Adapter) inspectPrivateVolume(ctx context.Context, c container) (named, bool, error) {
	p, present, err := privateContainerPlan(c)
	if err != nil || !present {
		return named{}, present, err
	}
	var v named
	if err := a.request(ctx, http.MethodGet, "/volumes/"+url.PathEscape(p.Name), nil, &v); err != nil {
		return v, true, err
	}
	if err := a.checkPrivateVolume(v, p); err != nil {
		return v, true, err
	}
	if v.Labels[privateNonceLabel] != c.Config.Labels[privateNonceLabel] {
		return v, true, rt.ErrNotOwned
	}
	return v, true, nil
}

// Only the exact environment's fenced deletion may release its private volume.
// Generic orphan cleanup cannot prove that a Provision has finished reserving it.
func (a *Adapter) deletePrivateVolume(ctx context.Context, c container) error {
	v, present, err := a.inspectPrivateVolume(ctx, c)
	if !present {
		return err
	}
	if err == nil {
		err = a.request(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(v.Name), nil, nil)
	}
	if missing(err) {
		return nil
	}
	if err != nil {
		return errors.Join(errPrivateCleanup, err)
	}
	return nil
}
