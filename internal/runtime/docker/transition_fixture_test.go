package docker

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// transitionEngine extends lifecycle fixtures with atomic helper resources.
func transitionEngine(t *testing.T, next http.Handler) http.Handler {
	t.Helper()
	var mu sync.Mutex
	helpers := map[string]container{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create") {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var body struct{ Labels map[string]string }
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			if body.Labels[roleLabel] == transitionRole {
				mu.Lock()
				defer mu.Unlock()
				name := r.URL.Query().Get("name")
				if _, exists := helpers[name]; exists {
					w.WriteHeader(409)
					return
				}
				var helper container
				helper.ID = name + "-exact"
				helper.Config.Labels = body.Labels
				helpers[name] = helper
				_ = json.NewEncoder(w).Encode(map[string]string{"Id": helper.ID})
				return
			}
		}
		mu.Lock()
		for name, h := range helpers {
			route := "/v1.45/containers/" + h.ID
			if r.Method == http.MethodGet && r.URL.Path == route+"/json" {
				_ = json.NewEncoder(w).Encode(h)
				mu.Unlock()
				return
			}
			if r.Method == http.MethodDelete && r.URL.Path == route {
				delete(helpers, name)
				mu.Unlock()
				w.WriteHeader(204)
				return
			}
		}
		mu.Unlock()
		next.ServeHTTP(w, r)
	})
}
