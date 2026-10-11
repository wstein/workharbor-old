package docker

import (
	"errors"
	"path"
	"strings"
)

// Option configures trusted supervisor behavior, never repository inputs.
type Option func(*Adapter) error

// WithExecPath prepends required immutable utility/tool-store directories to
// the image's executable path for every exec. Caller PATH may add directories
// but cannot erase these prerequisites or the stock language toolchain path.
func WithExecPath(required ...string) Option {
	paths := append([]string(nil), required...)
	return func(a *Adapter) error {
		for _, entry := range paths {
			if !absoluteExecDirectory(entry) {
				return errors.New("docker: trusted executable directories must be absolute and unambiguous")
			}
		}
		a.execPaths = append([]string(nil), paths...)
		return nil
	}
}

func absoluteExecDirectory(value string) bool {
	return path.IsAbs(value) && path.Clean(value) == value && !strings.ContainsAny(value, ":\r\n\x00")
}

func (a *Adapter) execEnvironment(inherited, requested []string) ([]byte, error) {
	data, err := environmentData(requested)
	if err != nil || len(a.execPaths) == 0 {
		return data, err
	}
	base := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, entry := range inherited {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			base = value
		}
	}
	paths := append([]string(nil), a.execPaths...)
	paths = append(paths, strings.Split(base, ":")...)
	out := make([]string, 0, len(requested)+1)
	for _, entry := range requested {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			paths = append(paths, strings.Split(value, ":")...)
		} else {
			out = append(out, entry)
		}
	}
	seen := map[string]bool{}
	unique := make([]string, 0, len(paths))
	for _, entry := range paths {
		if !absoluteExecDirectory(entry) {
			return nil, errors.New("docker: executable paths must use absolute unambiguous directories")
		}
		if !seen[entry] {
			unique = append(unique, entry)
			seen[entry] = true
		}
	}
	out = append(out, "PATH="+strings.Join(unique, ":"))
	return environmentData(out)
}
