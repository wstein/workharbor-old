package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	rt "github.com/wstein/workharbor/internal/runtime"
)

func privatePlan() rt.PrivateExecPlan {
	return rt.PrivateExecPlan{Name: "task-exec", MemoryMB: 2, Owner: "mine", Association: "task-net"}
}

func TestPrivateVolumeCreatesOnlyBoundedRootProtectedMemoryStorage(t *testing.T) {
	var created named
	endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(404)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1.45/volumes/create" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		var body struct {
			Name, Driver       string
			DriverOpts, Labels map[string]string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		want := map[string]string{"type": "tmpfs", "device": "tmpfs", "o": "size=2097152,uid=0,gid=0,mode=0711,nosuid,nodev,noexec"}
		if body.Name != "task-exec" || body.Driver != "local" || !reflect.DeepEqual(body.DriverOpts, want) {
			t.Errorf("unsafe volume request: %+v", body)
		}
		created = named{Name: body.Name, Driver: body.Driver, Options: body.DriverOpts, Labels: body.Labels}
		_ = json.NewEncoder(w).Encode(created)
	}))
	a, _ := New("mine", endpoint, "/tools/whr-shim")
	if err := a.ensurePrivateVolume(context.Background(), privatePlan()); err != nil {
		t.Fatal(err)
	}
	if created.Labels[privateAssociationLabel] != "task-net" || created.Labels[privateNonceLabel] == "" {
		t.Fatalf("missing exclusive association: %+v", created.Labels)
	}
}

func TestPrivateVolumeNeverAdoptsExistingOrUnexpectedResources(t *testing.T) {
	for _, kind := range []string{"orphan", "foreign", "disk fallback", "unbounded", "wrong association", "name alias", "create collision"} {
		t.Run(kind, func(t *testing.T) {
			plan := privatePlan()
			ls := labels("mine", privateVolumeRole)
			ls[privateAssociationLabel], ls[privateSizeLabel], ls[privateNonceLabel] = "task-net", "2", "old-owner"
			v := named{Name: plan.Name, Driver: "local", Options: privateVolumeOptions(plan), Labels: ls}
			switch kind {
			case "foreign":
				v.Labels[rt.OwnerLabel] = "someone-else"
			case "disk fallback":
				v.Options["type"] = "none"
			case "unbounded":
				v.Options["o"] = "uid=0,gid=0,mode=0711"
			case "wrong association":
				v.Labels[privateAssociationLabel] = "other-net"
			case "name alias":
				v.Name = "other-name"
			}
			mutations := 0
			endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && kind == "create collision" {
					w.WriteHeader(404)
					return
				}
				if r.Method != http.MethodGet {
					mutations++
					if kind != "create collision" {
						t.Error("existing resource was mutated")
					}
				}
				_ = json.NewEncoder(w).Encode(v)
			}))
			a, _ := New("mine", endpoint, "/tools/whr-shim")
			err := a.ensurePrivateVolume(context.Background(), plan)
			if err == nil {
				t.Fatal("adopted an existing or mismatched private volume")
			}
			if kind == "orphan" && !errors.Is(err, rt.ErrVolumeBusy) {
				t.Fatalf("orphan=%v", err)
			}
			if kind != "create collision" && mutations != 0 {
				t.Fatal("unexpected mutation")
			}
		})
	}
}

func privateContainer() container {
	var c container
	c.ID = "abcdef"
	c.Config.Labels = labels("mine", environmentRole)
	c.Config.Labels[networkLabel] = "task-net"
	c.Config.Labels[privateVolumeLabel] = "task-exec"
	c.Config.Labels[privateSizeLabel] = "2"
	c.Config.Labels[privateNonceLabel] = "identity"
	c.Mounts = append(c.Mounts, struct {
		Type, Name, Source, Destination string
		RW                              bool
	}{Type: "volume", Name: "task-exec", Destination: rt.PrivateExecTarget, RW: true})
	return c
}

func TestPrivateVolumeRequiresExactContainerAssociationBeforeRelease(t *testing.T) {
	for _, kind := range []string{"match", "replacement identity", "aliased mount", "read only mount", "interrupted deletion"} {
		t.Run(kind, func(t *testing.T) {
			c := privateContainer()
			v := named{Name: "task-exec", Driver: "local", Options: privateVolumeOptions(privatePlan()), Labels: labels("mine", privateVolumeRole)}
			v.Labels[privateAssociationLabel], v.Labels[privateSizeLabel], v.Labels[privateNonceLabel] = "task-net", "2", "identity"
			switch kind {
			case "replacement identity":
				v.Labels[privateNonceLabel] = "replacement"
			case "aliased mount":
				c.Mounts[0].Destination = "/other"
			case "read only mount":
				c.Mounts[0].RW = false
			}
			deletes := 0
			endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deletes++
					if kind == "interrupted deletion" {
						w.WriteHeader(500)
					} else {
						w.WriteHeader(204)
					}
					return
				}
				_ = json.NewEncoder(w).Encode(v)
			}))
			a, _ := New("mine", endpoint, "/tools/whr-shim")
			err := a.deletePrivateVolume(context.Background(), c)
			if kind == "match" {
				if err != nil || deletes != 1 {
					t.Fatalf("release=%v deletes=%d", err, deletes)
				}
				return
			}
			if err == nil || !uncertainTransition(err) {
				t.Fatalf("uncertain private cleanup must retain guard: %v", err)
			}
			if kind != "interrupted deletion" && deletes != 0 {
				t.Fatal("deleted a resource without exact authority")
			}
		})
	}
}

func TestGenericCleanupCannotStealInFlightPrivateReservation(t *testing.T) {
	endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("private orphan was deleted")
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(named{Name: "task-exec", Labels: labels("mine", privateVolumeRole)})
	}))
	a, _ := New("mine", endpoint, "/tools/whr-shim")
	if err := a.RemoveVolume(context.Background(), "task-exec"); !errors.Is(err, rt.ErrVolumeBusy) {
		t.Fatalf("cleanup=%v", err)
	}
}

func TestDeleteMissingEnvironmentStillReportsRetainedTransition(t *testing.T) {
	endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("mutated retained guard")
			w.WriteHeader(500)
			return
		}
		if r.URL.Path == "/v1.45/containers/abcdef/json" {
			w.WriteHeader(404)
			return
		}
		if r.URL.Path != "/v1.45/containers/"+transitionName("abcdef")+"/json" {
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		c := container{ID: "retained-guard"}
		c.Config.Labels = labels("mine", transitionRole)
		c.Config.Labels[leaseEnvironmentLabel] = "abcdef"
		_ = json.NewEncoder(w).Encode(c)
	}))
	a, _ := New("mine", endpoint, "/tools/whr-shim")
	if err := a.Delete(context.Background(), "abcdef"); !errors.Is(err, rt.ErrVolumeBusy) {
		t.Fatalf("retained partial deletion reported complete: %v", err)
	}
}
