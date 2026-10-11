package runtime

import "strings"

// DockerOutbound is a trusted supervisor choice applying D62 to one concrete
// owned environment network. Repository and agent specs cannot request it.
// This permits outbound attachment; destination recording/allowlisting is a
// separate capability and is not implemented by this choice.
type DockerOutbound struct {
	Name  string
	Owner string
}

// DockerOutboundNetwork returns the sealed concrete outbound network, if any.
func (p PreparedSpec) DockerOutboundNetwork() (Network, bool) {
	if p.dockerOutbound == nil {
		return Network{}, false
	}
	return *p.dockerOutbound, true
}

func prepareDockerOutbound(opts PrepareOptions, s *Spec) (*Network, error) {
	request := opts.DockerOutbound
	if request == nil {
		return nil, nil
	}
	if request.Owner == "" || request.Owner != s.Owner || request.Name != s.Network.Name || !networkRe.MatchString(request.Name) || !strings.HasPrefix(request.Name, request.Owner+"-net-") || request.Name == request.Owner+"-net-" || s.Egress != nil {
		return nil, &SpecError{Problems: []string{"trusted Docker outbound choice requires one concrete owned environment network without an egress sidecar"}}
	}
	s.Network.Internal = false
	n := s.Network
	return &n, nil
}
