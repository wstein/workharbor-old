package runtime

import (
	"errors"
	"testing"
)

func TestOnlyTrustedDockerChoiceSealsOwnedOutboundNetwork(t *testing.T) {
	r := newPrepRig(t)
	s := goodSpec()
	s.Network.Name = "wh-net-task1"
	request := &DockerOutbound{Name: s.Network.Name, Owner: s.Owner}
	r.opts.DockerOutbound = request
	p, err := Prepare(r.opts, s)
	if err != nil {
		t.Fatal(err)
	}
	request.Name = "host"
	n, ok := p.DockerOutboundNetwork()
	if !ok || n.Internal || n.Name != "wh-net-task1" || p.Spec().Network != n {
		t.Fatalf("sealed network=%+v %v", n, ok)
	}
	n.Name = "bridge"
	specCopy := p.Spec()
	specCopy.Network.Name = "host"
	again, _ := p.DockerOutboundNetwork()
	if again.Name != "wh-net-task1" || p.Spec().Network.Name != again.Name {
		t.Fatal("outbound plan was widened through alias")
	}
}

func TestTrustedOutboundChoiceCannotBypassOtherHardening(t *testing.T) {
	for _, kind := range []string{"wrong owner", "wrong name", "shared network", "writable root", "foreign volume", "zero quota", "repository external"} {
		t.Run(kind, func(t *testing.T) {
			r := newPrepRig(t)
			s := goodSpec()
			s.Network.Name = "wh-net-task1"
			r.opts.DockerOutbound = &DockerOutbound{Name: s.Network.Name, Owner: s.Owner}
			switch kind {
			case "wrong owner":
				r.opts.DockerOutbound.Owner = "other"
			case "wrong name":
				r.opts.DockerOutbound.Name = "wh-net-task2"
			case "shared network":
				s.Network.Name = "bridge"
				r.opts.DockerOutbound.Name = "bridge"
			case "writable root":
				s.ReadOnlyRoot = false
			case "foreign volume":
				s.Mounts = []Mount{{Kind: MountVolume, Source: "other-home", Target: "/home/agent"}}
			case "zero quota":
				s.DiskMB = 0
			case "repository external":
				s.Network.Internal = false
			}
			if _, err := Prepare(r.opts, s); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("choice bypassed %s: %v", kind, err)
			}
		})
	}
}
