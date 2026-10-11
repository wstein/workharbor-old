package cli

import (
	"strings"
	"testing"
)

func TestSetupRuntimeStepsAreReadOnly(t *testing.T) {
	for _, step := range []string{"runtime", "container-start", "container-kernel", "brew-pin"} {
		for _, flag := range []string{"--dry-run", "--yes"} {
			t.Run(step+flag, func(t *testing.T) {
				r := answersRig(t, true)
				_, _, output := r.run("setup", "--user", "werner", "--only", step, flag)
				if len(r.host.ran) != 0 || r.host.asked != 0 {
					t.Fatalf("runtime mutation or prompt: ran=%v asked=%d", r.host.ran, r.host.asked)
				}
				for _, forbidden := range []string{"brew pin container", "kernel set --recommended", "brew install container", "system start --disable-kernel-install"} {
					if strings.Contains(output, forbidden) {
						t.Errorf("runtime mutation printed: %s", forbidden)
					}
				}
			})
		}
	}
}
