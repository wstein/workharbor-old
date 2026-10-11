package docker

import (
	"context"
	"fmt"

	"github.com/wstein/workharbor/internal/runtime"
)

// Exec is explicitly unsupported until the separate streaming implementation.
func (a *Adapter) Exec(ctx context.Context, id string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
	c, err := a.find(ctx, id)
	if err != nil {
		return nil, err
	}
	if !c.State.Running {
		return nil, runtime.ErrNotRunning
	}
	return nil, fmt.Errorf("%w: exec streams", ErrUnsupported)
}

var _ runtime.Adapter = (*Adapter)(nil)
