package doctor

import (
	"context"
	"strings"
	"testing"
)

func TestDoctorReportsWebListenAddress(t *testing.T) {
	for _, tt := range []struct {
		listen string
		status Status
	}{
		{"127.0.0.1:8787", OK}, {"10.44.0.1:8787", OK}, {"[fd42::1]:8787", OK}, {"0.0.0.0:8787", Fail}, {"[::]:8787", Fail},
	} {
		t.Run(tt.listen, func(t *testing.T) {
			r := newRig(t)
			r.cfg.Listen = tt.listen
			r.write(t)
			var check Check
			for _, c := range Checks(r.deps()) {
				if c.Name == "config" {
					check = c
				}
			}
			status, detail := check.Run(context.Background())
			if status != tt.status || !strings.Contains(detail, tt.listen) && status == OK {
				t.Errorf("config = %s %q, want %s and effective listen address", status, detail, tt.status)
			}
		})
	}
}

func TestPublicURLAcceptsSpecificVPNListener(t *testing.T) {
	d := publicDeps(t, `{"listen":"10.44.0.1:8787","public_url":"https://w.example"}`, scripted{})
	status, detail := d.publicURLStep().Run(context.Background())
	if status == Fail || !strings.Contains(detail, "10.44.0.1:8787") || !strings.Contains(detail, "not checked") || strings.Contains(detail, "listening on loopback") {
		t.Fatalf("public-url = %s %q", status, detail)
	}
}

func TestPublicURLWithoutANameKeepsAppSetupOnLoopback(t *testing.T) {
	d := publicDeps(t, `{"listen":"10.44.0.1:8787"}`, scripted{})
	status, detail := d.publicURLStep().Run(context.Background())
	if status != NotVerified || !strings.Contains(detail, "--listen 127.0.0.1:8787 --local") {
		t.Fatalf("public-url = %s %q, want the loopback app setup override", status, detail)
	}
}

func TestDirectVPNListenerCountsAsRemoteAccessWithoutPublicURL(t *testing.T) {
	for _, address := range []string{"10.44.0.1:8787", "[fd42::1]:8787"} {
		for _, tc := range []struct {
			account, membership string
			want                Status
		}{
			{"dedicated", notAdmin, OK}, {"dedicated", isAdmin, Fail}, {"shared", notAdmin, Fail}, {"shared", isAdmin, Fail},
		} {
			d := accountDeps(t, scripted{adminKey: tc.membership}, map[string]any{"listen": address, "account": tc.account})
			got, detail := status(steps(t, d)["account"])
			if got != tc.want {
				t.Errorf("%s %s %s: %s %q, want %s", address, tc.account, tc.membership, got, detail, tc.want)
			}
		}
	}
}
