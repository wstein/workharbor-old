package docker

import (
	"strings"
	"testing"
)

func TestTrustedExecPathsPreserveImageToolchainsAndCannotBeErased(t *testing.T) {
	a, err := New("mine", "unix:///tmp/unused.sock", "/tools/profile/bin/whr-shim", WithExecPath("/opt/bundledock/toolbox/bin", "/tools/profile/bin"))
	if err != nil {
		t.Fatal(err)
	}
	for _, requested := range [][]string{nil, {"PATH=/custom/bin", "HOME=/home/agent"}} {
		data, err := a.execEnvironment([]string{"PATH=/usr/local/go/bin:/usr/bin:/bin"}, requested)
		if err != nil {
			t.Fatal(err)
		}
		want := "PATH=/opt/bundledock/toolbox/bin:/tools/profile/bin:/usr/local/go/bin:/usr/bin:/bin"
		if !strings.Contains(string(data), want) {
			t.Fatalf("required or stock-image paths lost: %s", data)
		}
		if requested != nil && !strings.Contains(string(data), ":/custom/bin\n") {
			t.Fatal("caller extra path lost")
		}
	}
}

func TestTrustedExecPathsRejectRelativeOrMalformedValuesWithoutPrinting(t *testing.T) {
	const marker = "value-must-stay-private"
	if _, err := New("mine", "unix:///tmp/unused.sock", "/tools/shim", WithExecPath(marker)); err == nil || strings.Contains(err.Error(), marker) {
		t.Fatalf("constructor error=%v", err)
	}
	a, _ := New("mine", "unix:///tmp/unused.sock", "/tools/shim", WithExecPath("/opt/bundledock/toolbox/bin"))
	for _, requested := range [][]string{{"PATH=" + marker}, {"PATH="}, {"PATH=/bin", "PATH=/other"}} {
		if _, err := a.execEnvironment([]string{"PATH=/bin"}, requested); err == nil || strings.Contains(err.Error(), marker) {
			t.Fatalf("unsafe caller PATH error=%v", err)
		}
	}
	if _, err := a.execEnvironment([]string{"PATH=" + marker}, nil); err == nil || strings.Contains(err.Error(), marker) {
		t.Fatalf("unsafe image PATH error=%v", err)
	}
}
