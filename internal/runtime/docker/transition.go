package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	rt "github.com/wstein/workharbor/internal/runtime"
)

const (
	transitionRole        = "environment-transition"
	transitionHolderLabel = "workharbor.transition-holder"
)

func transitionName(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "whr-transition-" + hex.EncodeToString(sum[:])
}

// transition fences inspection, writer reservations and Engine mutations across
// supervisor processes. A stopped helper is never evidence that its holder has
// finished. Uncertain outcomes retain the guard for explicit operator recovery.
func (a *Adapter) transition(ctx context.Context, id string, deleting bool, operation func(container) error) error {
	c, err := a.find(ctx, id)
	if deleting && errors.Is(err, rt.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	holder, err := token()
	if err != nil {
		return err
	}
	ls := labels(a.owner, transitionRole)
	ls[leaseEnvironmentLabel], ls[transitionHolderLabel] = id, holder
	body := map[string]any{"Image": c.Config.Image, "User": c.Config.User, "Labels": ls, "Entrypoint": []string{"true"}, "HostConfig": map[string]any{"NetworkMode": "none", "ReadonlyRootfs": true, "CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"}}}
	var created struct {
		ID string `json:"Id"`
	}
	if err := a.request(ctx, http.MethodPost, "/containers/create?name="+transitionName(id), body, &created); err != nil {
		var api *apiError
		if errors.As(err, &api) && api.status == http.StatusConflict {
			return fmt.Errorf("docker: environment transition already reserved: %w", rt.ErrVolumeBusy)
		}
		return fmt.Errorf("docker: transition reservation may require operator inspection: %w", err)
	}
	if created.ID == "" {
		return errors.New("docker: transition reservation has uncertain identity; operator inspection required")
	}
	// Initial inspection only establishes ownership before creating a helper. All
	// state decisions occur after atomic acquisition, including absence/deletion.
	c, err = a.find(ctx, id)
	if deleting && errors.Is(err, rt.ErrNotFound) {
		err = nil
	} else if err == nil {
		err = operation(c)
	}
	if uncertainTransition(err) {
		return fmt.Errorf("docker: transition retained; operator inspection required: %w", err)
	}
	if releaseErr := a.releaseTransition(ctx, created.ID, id, holder); releaseErr != nil {
		return errors.Join(err, fmt.Errorf("docker: transition release uncertain; operator inspection required: %w", releaseErr))
	}
	return err
}

func uncertainTransition(err error) bool {
	if err == nil {
		return false
	}
	var api *apiError
	if errors.As(err, &api) {
		return api.status >= 500
	}
	// Ownership uncertainty fails closed. Definite local refusals do not mutate
	// the environment, so their transition guard can be released safely.
	return !errors.Is(err, rt.ErrVolumeBusy) && !errors.Is(err, rt.ErrRunning) && !errors.Is(err, rt.ErrNotFound)
}

func (a *Adapter) releaseTransition(ctx context.Context, exactID, environment, holder string) error {
	var checked container
	if err := a.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(exactID)+"/json", nil, &checked); err != nil {
		return err
	}
	if checked.ID != exactID || checked.State.Running || !owned(checked.Config.Labels, a.owner, transitionRole) || checked.Config.Labels[leaseEnvironmentLabel] != environment || checked.Config.Labels[transitionHolderLabel] != holder {
		return rt.ErrNotOwned
	}
	return a.request(ctx, http.MethodDelete, "/containers/"+url.PathEscape(exactID), nil, nil)
}
