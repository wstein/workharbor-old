package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	rt "github.com/wstein/workharbor/internal/runtime"
)

func TestNetworkCreationVerifiesExactOwnedBridgeBeforeUse(t *testing.T) {
	for _, kind := range []string{"match", "wrong identity", "foreign", "wrong mode", "shared scope", "wrong driver", "attached elsewhere"} {
		t.Run(kind, func(t *testing.T) {
			inspected := false
			endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					_ = json.NewEncoder(w).Encode(map[string]string{"Id": "exact-network"})
					return
				}
				if r.URL.Path == "/v1.45/networks/wh-net-task" {
					w.WriteHeader(404)
					return
				}
				if r.URL.Path != "/v1.45/networks/exact-network" {
					t.Errorf("unexpected %s", r.URL.Path)
					w.WriteHeader(500)
					return
				}
				inspected = true
				body := map[string]any{"Id": "exact-network", "Name": "wh-net-task", "Driver": "bridge", "Scope": "local", "Internal": false, "Labels": labels("mine", "network")}
				switch kind {
				case "wrong identity":
					body["Id"] = "replacement"
				case "foreign":
					body["Labels"] = labels("other", "network")
				case "wrong mode":
					body["Internal"] = true
				case "shared scope":
					body["Scope"] = "swarm"
				case "wrong driver":
					body["Driver"] = "host"
				case "attached elsewhere":
					body["Containers"] = map[string]any{"foreign": map[string]any{}}
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			a, _ := New("mine", endpoint, "/tools/whr-shim")
			err := a.ensureNetwork(context.Background(), rt.Network{Name: "wh-net-task"})
			if kind == "match" {
				if err != nil || !inspected {
					t.Fatalf("network not verified: %v", err)
				}
			} else if err == nil {
				t.Fatal("accepted unexpected network")
			}
		})
	}
}
