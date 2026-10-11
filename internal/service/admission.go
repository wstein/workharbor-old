package service

import (
	"context"
	"fmt"

	"github.com/wstein/workharbor/internal/domain"
)

// executionAdmission checks expressly configured usage budgets and durable
// lifecycle state before execution. A run has no wall-clock duration limit.
func (s *Service) executionAdmission(ctx context.Context, task, run domain.ID) error {
	s.checkBudgets(ctx, task, run)
	a, err := s.store.LoadTask(ctx, task)
	if err != nil {
		return err
	}
	r, ok := a.Run(run)
	if !ok || r.State.Terminal() || a.Task().State.Terminal() {
		return fmt.Errorf("run %s: execution refused after terminal transition", run)
	}
	return nil
}
