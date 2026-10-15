package console

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/wstein/workharbor/internal/baseimage"
)

const toolbox = "/opt/bundledock/toolbox"

// rcFiles names, per console image, the files that load the BundleDock shared
// shell layer: the environment line (zshenv and profile.d) and the per-shell
// init files. Alpine has no bash.
var rcFiles = map[baseimage.Distro]struct{ zshenv, zshrc, bashrc string }{
	baseimage.Fedora: {"/etc/zshenv", "/etc/zshrc", "/etc/bashrc"},
	baseimage.Ubuntu: {"/etc/zsh/zshenv", "/etc/zsh/zshrc", "/etc/bash.bashrc"},
	Alpine:           {"/etc/zsh/zshenv", "/etc/zsh/zshrc", ""},
}

var (
	printfRe = regexp.MustCompile(`printf '%s\\n'((?: '[^']*')+) >>? (\S+)`)
	argRe    = regexp.MustCompile(`'([^']*)'`)
)

// written returns what each RUN printf writes, keyed by destination file.
func written(t *testing.T, d baseimage.Distro) map[string][]string {
	t.Helper()
	cf, err := Containerfile(d)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, m := range printfRe.FindAllStringSubmatch(string(cf), -1) {
		for _, a := range argRe.FindAllStringSubmatch(m[1], -1) {
			out[m[2]] = append(out[m[2]], a[1])
		}
	}
	return out
}

func TestConsoleImagesLoadTheSharedShellLayerOnlyWhenTheToolboxIsMounted(t *testing.T) {
	t.Parallel()
	for d, rc := range rcFiles {
		w := written(t, d)
		env := w["/etc/profile.d/whr-toolbox.sh"]
		if len(env) != 1 || !containsLine(w[rc.zshenv], env[0]) {
			t.Fatalf("%s: the toolbox environment line must go to profile.d and %s: %v", d, rc.zshenv, w)
		}
		if !strings.Contains(env[0], "[ -d "+toolbox+"/share ]") {
			t.Errorf("%s: environment line is not guarded by the mounted toolbox: %s", d, env[0])
		}
		zsh := "[ -r " + toolbox + "/share/shell/init.zsh ] && . " + toolbox + "/share/shell/init.zsh"
		if !containsLine(w[rc.zshrc], zsh) {
			t.Errorf("%s: %s does not source init.zsh, guarded", d, rc.zshrc)
		}
		if rc.bashrc != "" {
			bash := "[ -r " + toolbox + "/share/shell/init.bash ] && . " + toolbox + "/share/shell/init.bash"
			if !containsLine(w[rc.bashrc], bash) {
				t.Errorf("%s: %s does not source init.bash, guarded", d, rc.bashrc)
			}
		}
	}
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// TestToolboxEnvironmentLine runs the image's environment line in each shell
// that reads it, with the toolbox path pointed at a temporary directory: absent
// toolbox, no change; mounted toolbox, its bin last on PATH (never shadowing the
// git wrapper) and its share first in XDG_DATA_DIRS; nested shells add nothing.
func TestToolboxEnvironmentLine(t *testing.T) {
	t.Parallel()
	lines := written(t, baseimage.Fedora)["/etc/profile.d/whr-toolbox.sh"]
	if len(lines) != 1 {
		t.Fatalf("want one toolbox environment line in profile.d, got %v", lines)
	}
	line := lines[0]
	for _, sh := range []string{"sh", "bash", "zsh"} {
		t.Run(sh, func(t *testing.T) {
			if _, err := exec.LookPath(sh); err != nil {
				t.Skipf("%s not installed", sh)
			}
			for _, mounted := range []bool{false, true} {
				dir := filepath.Join(t.TempDir(), "toolbox")
				if mounted {
					if err := os.MkdirAll(filepath.Join(dir, "share"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				rc := filepath.Join(t.TempDir(), "rc")
				if err := os.WriteFile(rc, []byte(strings.ReplaceAll(line, toolbox, dir)+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				script := ". " + rc + "; . " + rc + `; printf '%s\n%s\n' "$PATH" "$XDG_DATA_DIRS"`
				cmd := exec.Command(sh, "-c", script)
				cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
				out, err := cmd.Output()
				if err != nil {
					t.Fatalf("%s: %v", sh, err)
				}
				lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
				path, xdg := lines[0], lines[1]
				if !mounted {
					if path != "/usr/local/bin:/usr/bin:/bin" || xdg != "" {
						t.Errorf("%s: no toolbox, yet PATH=%q XDG_DATA_DIRS=%q", sh, path, xdg)
					}
					continue
				}
				if path != "/usr/local/bin:/usr/bin:/bin:"+dir+"/bin" {
					t.Errorf("%s: toolbox bin must be appended once: %q", sh, path)
				}
				if xdg != dir+"/share:/usr/local/share:/usr/share" {
					t.Errorf("%s: XDG_DATA_DIRS %q", sh, xdg)
				}
			}
		})
	}
}
