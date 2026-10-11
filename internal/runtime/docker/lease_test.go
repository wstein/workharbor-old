package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wstein/workharbor/internal/runtime"
)

func TestLeaseConflictDoesNotReclaimStoppedOrForeignClaims(t *testing.T) {
	for _, tc := range []struct {
		name, owner, role, volume, environment string
		want                                   error
	}{
		{"stopped claim of another environment", "mine", leaseRole, "home", "other", runtime.ErrVolumeBusy},
		{"foreign owner", "other", leaseRole, "home", "env", runtime.ErrNotOwned},
		{"wrong role", "mine", "environment", "home", "env", runtime.ErrNotOwned},
		{"wrong volume", "mine", leaseRole, "other", "env", runtime.ErrNotOwned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.WriteHeader(409)
					return
				}
				if r.Method != http.MethodGet {
					t.Error("a conflicting claim was mutated")
					w.WriteHeader(500)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"Id": "claim", "State": map[string]any{"Running": false}, "Config": map[string]any{"Labels": map[string]string{runtime.OwnerLabel: tc.owner, roleLabel: tc.role, leaseVolumeLabel: tc.volume, leaseEnvironmentLabel: tc.environment}}})
			}))
			a, err := New("mine", endpoint, "/tools/whr-shim")
			if err != nil {
				t.Fatal(err)
			}
			var c container
			c.ID = "env"
			c.Mounts = append(c.Mounts, struct {
				Type, Name, Source, Destination string
				RW                              bool
			}{Type: "volume", Name: "home", RW: true})
			if err := a.acquireLeases(context.Background(), c); !errors.Is(err, tc.want) {
				t.Fatalf("claim=%v want=%v", err, tc.want)
			}
		})
	}
}

func TestStoppedEnvironmentReleasesOnlyItsExactOwnedLease(t *testing.T) {
	deleted := []string{}
	endpoint := socketServer(t, transitionEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/containers/json") {
			_, _ = io.WriteString(w, `[{"Id":"lease1"},{"Id":"lease2"},{"Id":"lease3"}]`)
			return
		}
		bits := strings.Split(r.URL.Path, "/")
		id := bits[len(bits)-2]
		if r.Method == http.MethodDelete {
			deleted = append(deleted, bits[len(bits)-1])
			w.WriteHeader(204)
			return
		}
		if id == "env" {
			_, _ = io.WriteString(w, `{"Id":"env","Config":{"Labels":{"workharbor.owner":"mine","workharbor.role":"environment"}},"State":{"Running":false}}`)
			return
		}
		owner, environment := "mine", "env"
		if id == "lease2" {
			owner = "other"
		}
		if id == "lease3" {
			environment = "other"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "State": map[string]any{"Running": false}, "Config": map[string]any{"Labels": map[string]string{runtime.OwnerLabel: owner, roleLabel: leaseRole, leaseVolumeLabel: "home", leaseEnvironmentLabel: environment}}})
	})))
	a, err := New("mine", endpoint, "/tools/whr-shim")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Stop(context.Background(), "env"); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "lease1" {
		t.Fatalf("deleted=%v", deleted)
	}
}
