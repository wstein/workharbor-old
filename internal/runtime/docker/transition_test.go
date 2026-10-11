package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wstein/workharbor/internal/runtime"
)

func TestStopOrDeleteCannotReleaseLeaseDuringAnotherClientsStart(t *testing.T) {
	for _, action := range []string{"Stop", "Delete"} {
		t.Run(action, func(t *testing.T) {
			var mu sync.Mutex
			running := map[string]bool{"aaaa": false, "bbbb": false}
			helpers := map[string]container{}
			next := 0
			startEntered, finishStart := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(finishStart) }) }
			defer release()
			endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				route := strings.TrimPrefix(r.URL.Path, "/v1.45")
				if route == "/containers/create" {
					var body struct{ Labels map[string]string }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					name := r.URL.Query().Get("name")
					mu.Lock()
					defer mu.Unlock()
					if _, exists := helpers[name]; exists {
						w.WriteHeader(409)
						return
					}
					next++
					var helper container
					helper.ID = fmt.Sprintf("helper%d", next)
					helper.Config.Labels = body.Labels
					helpers[name] = helper
					_ = json.NewEncoder(w).Encode(map[string]string{"Id": helper.ID})
					return
				}
				if route == "/containers/json" {
					mu.Lock()
					defer mu.Unlock()
					list := []map[string]string{{"Id": "aaaa"}, {"Id": "bbbb"}}
					for _, h := range helpers {
						list = append(list, map[string]string{"Id": h.ID})
					}
					_ = json.NewEncoder(w).Encode(list)
					return
				}
				bits := strings.Split(route, "/")
				id := bits[len(bits)-2]
				if strings.HasSuffix(route, "/start") {
					if id == "aaaa" {
						close(startEntered)
						<-finishStart
					}
					mu.Lock()
					running[id] = true
					mu.Unlock()
					w.WriteHeader(204)
					return
				}
				if strings.HasSuffix(route, "/json") {
					mu.Lock()
					defer mu.Unlock()
					if state, exists := running[id]; exists {
						_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "Image": "sha256:stock", "Config": map[string]any{"Image": "stock", "User": "1000:1000", "Labels": labels("mine", environmentRole)}, "State": map[string]bool{"Running": state}, "Mounts": []map[string]any{{"Type": "volume", "Name": "home", "RW": true}}})
						return
					}
					for name, h := range helpers {
						if h.ID == id || name == id {
							_ = json.NewEncoder(w).Encode(h)
							return
						}
					}
					w.WriteHeader(404)
					return
				}
				if r.Method == http.MethodDelete {
					target := bits[len(bits)-1]
					mu.Lock()
					defer mu.Unlock()
					for name, h := range helpers {
						if h.ID == target {
							delete(helpers, name)
							w.WriteHeader(204)
							return
						}
					}
					w.WriteHeader(404)
					return
				}
				t.Errorf("unexpected route %s", route)
				w.WriteHeader(500)
				_, _ = io.WriteString(w, "{}")
			}))
			a, err := New("mine", endpoint, "/tools/whr-shim")
			if err != nil {
				t.Fatal(err)
			}
			b, err := New("mine", endpoint, "/tools/whr-shim")
			if err != nil {
				t.Fatal(err)
			}
			c, err := New("mine", endpoint, "/tools/whr-shim")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			aResult := make(chan error, 1)
			go func() { aResult <- a.Start(ctx, "aaaa") }()
			select {
			case <-startEntered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var stopErr error
			if action == "Stop" {
				stopErr = b.Stop(ctx, "aaaa")
			} else {
				stopErr = b.Delete(ctx, "aaaa")
			}
			otherErr := c.Start(ctx, "bbbb")
			release()
			if err := <-aResult; err != nil {
				t.Fatal(err)
			}
			if stopErr != nil && !errors.Is(stopErr, runtime.ErrVolumeBusy) {
				t.Fatalf("concurrent Stop = %v", stopErr)
			}
			if !errors.Is(otherErr, runtime.ErrVolumeBusy) {
				t.Fatalf("other Start = %v; another client released an in-flight writer claim", otherErr)
			}
		})
	}
}

func TestUncertainStartRetainsTransitionAndWriterClaims(t *testing.T) {
	var mu sync.Mutex
	helpers := map[string]container{}
	endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := strings.TrimPrefix(r.URL.Path, "/v1.45")
		mu.Lock()
		defer mu.Unlock()
		switch route {
		case "/containers/create":
			name := r.URL.Query().Get("name")
			if _, exists := helpers[name]; exists {
				w.WriteHeader(409)
				return
			}
			var body struct{ Labels map[string]string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			var h container
			h.ID = name + "-exact"
			h.Config.Labels = body.Labels
			helpers[name] = h
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": h.ID})
		case "/containers/env/json":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "env", "Config": map[string]any{"Image": "stock", "User": "1000:1000", "Labels": labels("mine", environmentRole)}, "Mounts": []map[string]any{{"Type": "volume", "Name": "home", "RW": true}}})
		case "/containers/json":
			list := []map[string]string{{"Id": "env"}}
			for _, h := range helpers {
				list = append(list, map[string]string{"Id": h.ID})
			}
			_ = json.NewEncoder(w).Encode(list)
		case "/containers/env/start":
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		case "/networks":
			_, _ = io.WriteString(w, "[]")
		case "/volumes":
			_, _ = io.WriteString(w, `{"Volumes":[]}`)
		default:
			for _, h := range helpers {
				if r.Method == http.MethodGet && route == "/containers/"+h.ID+"/json" {
					_ = json.NewEncoder(w).Encode(h)
					return
				}
			}
			t.Errorf("unexpected request %s %s", r.Method, route)
			w.WriteHeader(500)
		}
	}))
	a, err := New("mine", endpoint, "/tools/whr-shim")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background(), "env"); err == nil || !strings.Contains(err.Error(), "transition retained") {
		t.Fatalf("uncertain Start = %v", err)
	}
	b, err := New("mine", endpoint, "/tools/whr-shim")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(context.Background(), "env"); !errors.Is(err, runtime.ErrVolumeBusy) {
		t.Fatalf("Stop after uncertain launch = %v", err)
	}
	inventory, err := b.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Helpers) != 2 {
		t.Fatalf("orphan transition/writer claims hidden: %+v", inventory)
	}
}
