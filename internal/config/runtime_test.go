package config

import (
	"strings"
	"testing"
)

func TestRuntimeSelectionRejectsImplicitOrRemoteDockerEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime Runtime
		want    bool
	}{
		{"default Apple", Runtime{}, true},
		{"explicit Apple", Runtime{Backend: "apple"}, true},
		{"explicit Docker", Runtime{Backend: "docker", Endpoint: "unix:///tmp/engine.sock", Toolbox: "/opt/bundledock/toolbox"}, true},
		{"unknown", Runtime{Backend: "remote"}, false},
		{"implicit socket", Runtime{Backend: "docker", Toolbox: "/tools"}, false},
		{"remote socket", Runtime{Backend: "docker", Endpoint: "tcp://localhost:2375", Toolbox: "/tools"}, false},
		{"missing toolbox", Runtime{Backend: "docker", Endpoint: "unix:///tmp/engine.sock"}, false},
		{"Apple extra Docker fields", Runtime{Backend: "apple", Endpoint: "unix:///tmp/engine.sock"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Runtime: tc.runtime}
			err := c.Validate()
			bad := err != nil && strings.Contains(err.Error(), "runtime:")
			if bad == tc.want {
				t.Fatalf("runtime validation accepted=%v want=%v: %v", !bad, tc.want, err)
			}
		})
	}
}
