package doctor

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestSetupNeverInstallsOrPinsRuntime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if strings.Contains(DefaultBrewfile, `brew "container"`) {
		t.Error("setup Brewfile installs a runtime")
	}
	for _, c := range Checks(hostDeps(scripted{})) {
		if c.Name != "brew-pin" && c.Name != "container-kernel" && c.Name != "container-start" {
			continue
		}
		if c.Fix != nil {
			t.Errorf("%s still offers a runtime mutation: %+v", c.Name, c.Fix)
		}
	}
	d := hostDeps(scripted{})
	c := steps(t, d)["brew-packages"]
	if err := c.Fix.Do(context.Background(), &answers{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(brewfilePath()) })
	b, err := os.ReadFile(brewfilePath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `brew "container"`) {
		t.Error("executable setup Brewfile installs a runtime")
	}
}
