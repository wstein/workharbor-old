package config

import (
	"errors"
	"net/url"
	"path/filepath"
)

// Runtime selects an existing host runtime. Empty means Apple's current
// adapter. Docker requires an explicit existing socket and separate BundleDock
// utility bundle; this configuration never installs or discovers either.
type Runtime struct {
	Backend  string `json:"backend,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Toolbox  string `json:"toolbox,omitempty"`
}

func (r Runtime) IsDocker() bool { return r.Backend == "docker" }

func (r Runtime) validate() error {
	switch r.Backend {
	case "", "apple":
		if r.Endpoint != "" || r.Toolbox != "" {
			return errors.New("endpoint and toolbox require backend docker")
		}
	case "docker":
		u, err := url.Parse(r.Endpoint)
		if err != nil || u.Scheme != "unix" || u.Host != "" || !filepath.IsAbs(u.Path) || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("docker requires an explicit unix:///absolute/socket endpoint")
		}
		if !filepath.IsAbs(r.Toolbox) {
			return errors.New("docker requires an absolute path to the existing BundleDock toolbox")
		}
	default:
		return errors.New("backend must be apple or docker")
	}
	return nil
}
