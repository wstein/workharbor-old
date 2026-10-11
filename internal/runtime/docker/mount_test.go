package docker

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rt "github.com/wstein/workharbor/internal/runtime"
)

func TestConfiguredSocketAncestorsCannotBeMounted(t *testing.T) {
	endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("forbidden mount contacted Engine")
		w.WriteHeader(500)
	}))
	socket := strings.TrimPrefix(endpoint, "unix://")
	link := filepath.Join(t.TempDir(), "engine-link")
	if err := os.Symlink(socket, link); err != nil {
		t.Fatal(err)
	}
	for _, selected := range []string{socket, link} {
		for _, source := range []string{filepath.Dir(socket), filepath.Dir(link)} {
			// Only ancestors of the selected pathname or its canonical target conflict.
			if selected == socket && source == filepath.Dir(link) {
				continue
			}
			a, err := New("mine", "unix://"+selected, "/tools/whr-shim")
			if err != nil {
				t.Fatal(err)
			}
			s := rt.Spec{Image: "stock", Owner: "mine", CPUs: 1, MemoryMB: 256, DiskMB: 1024, Network: rt.Network{Name: "net", Internal: true}, User: "1000:1000", ReadOnlyRoot: true, CapDrop: []string{"ALL"}, Init: true, Mounts: []rt.Mount{{Source: source, Target: "/data", ReadOnly: true}}}
			p, err := rt.Prepare(rt.PrepareOptions{FS: rt.OSFS{}, Home: t.TempDir()}, s)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.Provision(context.Background(), p); !errors.Is(err, rt.ErrForbiddenMount) {
				t.Fatalf("endpoint %s mount %s = %v", selected, source, err)
			}
		}
	}
}
