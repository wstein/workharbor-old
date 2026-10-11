package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wstein/workharbor/internal/runtime"
)

func socketServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "engine.sock")
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "unix://" + socket
}

func TestExactIdentityAndOwnershipBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name, requested, returned, owner string
		want                             error
	}{
		{"abbreviated ID", "abcd", "abcdef", "mine", runtime.ErrNotFound},
		{"foreign owner", "abcdef", "abcdef", "other", runtime.ErrNotOwned},
		{"wrong role", "abcdef", "abcdef", "mine", runtime.ErrNotOwned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutations := 0
			endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations++
					t.Errorf("unexpected mutation %s", r.URL.Path)
					return
				}
				role := "environment"
				if tc.name == "wrong role" {
					role = "helper"
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"Id":"` + tc.returned + `","Config":{"Labels":{"workharbor.owner":"` + tc.owner + `","workharbor.role":"` + role + `"}},"State":{"Running":false}}`))
			}))
			a, err := New("mine", endpoint, "/tools/whr-shim")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.Inspect(context.Background(), tc.requested); !errors.Is(err, tc.want) {
				t.Fatalf("Inspect = %v, want %v", err, tc.want)
			}
			_ = a.Delete(context.Background(), tc.requested)
			if mutations != 0 {
				t.Fatalf("mutations = %d", mutations)
			}
		})
	}
}

func TestProvisionRejectsUnpreparedWithoutContactingSocket(t *testing.T) {
	a, err := New("mine", "unix://"+filepath.Join(t.TempDir(), "absent.sock"), "/tools/whr-shim")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Provision(context.Background(), runtime.PreparedSpec{}); !errors.Is(err, runtime.ErrNotPrepared) {
		t.Fatal(err)
	}
}

func TestConstructorAcceptsOnlyExplicitLocalSocket(t *testing.T) {
	for _, endpoint := range []string{"", "tcp://localhost:2375", "http://localhost", "unix://relative", "unix:///run/docker.sock?x=1"} {
		if _, err := New("mine", endpoint, "/tools/whr-shim"); err == nil {
			t.Errorf("accepted %q", endpoint)
		}
	}
}

func TestUnsupportedQuotaFailsBeforeResourceCreation(t *testing.T) {
	a, err := New("mine", "unix://"+filepath.Join(t.TempDir(), "absent.sock"), "/tools/whr-shim")
	if err != nil {
		t.Fatal(err)
	}
	spec := runtime.Spec{Image: "golang:1", Owner: "mine", CPUs: 1, MemoryMB: 256, DiskMB: 1024, Network: runtime.Network{Name: "test-net", Internal: true}, User: "1000:1000", ReadOnlyRoot: true, CapDrop: []string{"ALL"}, Init: true}
	prep, err := runtime.Prepare(runtime.PrepareOptions{FS: runtime.OSFS{}, Home: t.TempDir()}, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Provision(context.Background(), prep); !errors.Is(err, runtime.ErrUnsupported) {
		t.Fatalf("quota = %v, want ErrUnsupported", err)
	}
}

func TestDeleteRetryFinishesOwnedNetworkBeforeForgettingContainer(t *testing.T) {
	var mu sync.Mutex
	containerPresent, networkPresent := true, true
	failures := 1
	endpoint := socketServer(t, transitionEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			_, _ = io.WriteString(w, "[]")
			return
		case strings.HasSuffix(r.URL.Path, "/containers/abcdef/json"):
			if !containerPresent {
				w.WriteHeader(404)
				return
			}
			_, _ = io.WriteString(w, `{"Id":"abcdef","Config":{"Labels":{"workharbor.owner":"mine","workharbor.role":"environment","workharbor.network":"net"}},"State":{"Running":false}}`)
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/containers/abcdef"):
			containerPresent = false
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/networks/net/disconnect"):
			w.WriteHeader(200)
		case strings.HasSuffix(r.URL.Path, "/networks/net"):
			if !networkPresent {
				w.WriteHeader(404)
				return
			}
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"Id":"net","Name":"net","Labels":{"workharbor.owner":"mine","workharbor.role":"network"},"Containers":{"abcdef":{}}}`)
				return
			}
			if failures > 0 {
				failures--
				w.WriteHeader(409)
				return
			}
			networkPresent = false
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})))
	a, err := New("mine", endpoint, "/tools/whr-shim")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(context.Background(), "abcdef"); err == nil {
		t.Fatal("expected first network deletion to fail")
	}
	if err := a.Delete(context.Background(), "abcdef"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if containerPresent || networkPresent {
		t.Fatal("retry leaked an owned container or network")
	}
}

func TestWritableVolumeHasOneWriterAcrossAdapterClients(t *testing.T) {
	var mu sync.Mutex
	running := map[string]bool{"aaaa": false, "bbbb": false}
	leases := map[string]map[string]string{}
	endpoint := socketServer(t, transitionEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/containers/create") {
			var body struct{ Labels map[string]string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			name := r.URL.Query().Get("name")
			mu.Lock()
			defer mu.Unlock()
			if leases[name] != nil {
				w.WriteHeader(409)
				return
			}
			leases[name] = body.Labels
			_, _ = io.WriteString(w, `{"Id":"lease-id"}`)
			return
		}
		if strings.Contains(r.URL.Path, "/containers/whr-volume-lease-") {
			bits := strings.Split(r.URL.Path, "/")
			name := bits[len(bits)-2]
			mu.Lock()
			ls := leases[name]
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "lease-id", "Config": map[string]any{"Labels": ls}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/containers/json") {
			_, _ = io.WriteString(w, `[{"Id":"aaaa"},{"Id":"bbbb"}]`)
			return
		}
		bits := strings.Split(r.URL.Path, "/")
		id := bits[len(bits)-2]
		if strings.HasSuffix(r.URL.Path, "/start") {
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			running[id] = true
			mu.Unlock()
			w.WriteHeader(204)
			return
		}
		mu.Lock()
		state := running[id]
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "Config": map[string]any{"Labels": labels("mine", environmentRole)}, "State": map[string]bool{"Running": state}, "Mounts": []map[string]any{{"Type": "volume", "Name": "home", "Destination": "/home/agent", "RW": true}}})
	})))
	a, err := New("mine", endpoint, "/tools/whr-shim")
	if err != nil {
		t.Fatal(err)
	}
	b, err := New("mine", endpoint, "/tools/whr-shim")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- a.Start(context.Background(), "aaaa") }()
	go func() { <-start; results <- b.Start(context.Background(), "bbbb") }()
	close(start)
	successes, busy := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if errors.Is(err, runtime.ErrVolumeBusy) {
			busy++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || busy != 1 {
		t.Fatalf("starts=%d busy=%d; writable volume must have one writer", successes, busy)
	}
}
