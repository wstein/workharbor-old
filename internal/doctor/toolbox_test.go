package doctor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wstein/workharbor/internal/config"
)

func toolboxFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := []byte("fixture bytes, never executed")
	manifest := []byte(`{"schema":1,"platform":"linux/arm64","tools":{"ripgrep":{"version":"14.1.1","url":"https://example.invalid/archive","sha256":"` + strings.Repeat("a", 64) + `","verification":"tls-download-sha256"}}}`)
	for name, data := range map[string][]byte{"manifest.json": manifest, "bin/rg": command, "SHA256SUMS": fmt.Appendf(nil, "%x  manifest.json\n%x  bin/rg\n", sha256.Sum256(manifest), sha256.Sum256(command))} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(dir, "bin/rg"), 0o700); err != nil { //nolint:gosec // executable fixture, never run
		t.Fatal(err)
	}
	return dir
}

func TestToolboxReportsDeclaredPackagesAndMeasuredFilesSeparately(t *testing.T) {
	dir := toolboxFixture(t)
	status, detail := toolboxCheck(context.Background(), dir)
	if status != NotVerified {
		t.Fatalf("status=%s detail=%s", status, detail)
	}
	for _, want := range []string{"ripgrep=14.1.1", "tls-download-sha256", "rg: checksum matches", "publisher authenticity unverified", "guest execution unverified"} {
		if !strings.Contains(detail, want) {
			t.Errorf("missing %q in %s", want, detail)
		}
	}
	if strings.Contains(detail, "ripgrep: unavailable") {
		t.Fatal("package name guessed as command")
	}
}

func TestToolboxReportsMissingCorruptAndUnsafeInventory(t *testing.T) {
	for _, mode := range []string{"missing command", "corrupt command", "missing metadata", "unknown schema", "unsafe path", "symlink command", "unknown provenance"} {
		t.Run(mode, func(t *testing.T) {
			dir := toolboxFixture(t)
			want := Warn
			switch mode {
			case "missing command":
				_ = os.Remove(filepath.Join(dir, "bin/rg"))
			case "corrupt command":
				_ = os.WriteFile(filepath.Join(dir, "bin/rg"), []byte("changed"), 0o600)
			case "symlink command":
				_ = os.Remove(filepath.Join(dir, "bin/rg"))
				_ = os.Symlink("../manifest.json", filepath.Join(dir, "bin/rg"))
			case "missing metadata":
				_ = os.Remove(filepath.Join(dir, "manifest.json"))
				want = NotVerified
			case "unknown schema":
				_ = os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"schema":42}`), 0o600)
				want = NotVerified
			case "unsafe path":
				_ = os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Repeat("a", 64)+"  ../outside\n"), 0o600)
				want = NotVerified
			case "unknown provenance":
				data, _ := os.ReadFile(filepath.Join(dir, "manifest.json")) //nolint:gosec // fixture below t.TempDir
				data = []byte(strings.ReplaceAll(string(data), "tls-download-sha256", "made-up-proof"))
				_ = os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600) //nolint:gosec // fixed fixture path below t.TempDir
				sums, _ := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))           //nolint:gosec // fixture below t.TempDir
				lines := strings.Split(string(sums), "\n")
				lines[0] = fmt.Sprintf("%x  manifest.json", sha256.Sum256(data))
				_ = os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Join(lines, "\n")), 0o600) //nolint:gosec // fixed fixture path below t.TempDir
				want = NotVerified
			}
			got, detail := toolboxCheck(context.Background(), dir)
			if got != want {
				t.Fatalf("status=%s want=%s detail=%s", got, want, detail)
			}
			if got == Warn && !strings.Contains(detail, "rg: unavailable") {
				t.Fatal(detail)
			}
		})
	}
}

func TestToolboxDoctorCheckOnlyAppearsForDocker(t *testing.T) {
	r := newRig(t)
	r.write(t)
	if _, ok := steps(t, r.deps())["bundledock-toolbox"]; ok {
		t.Fatal("Apple checks changed")
	}
	r.cfg.Runtime = config.Runtime{Backend: "docker", Endpoint: "unix:///tmp/test.sock", Toolbox: toolboxFixture(t)}
	r.write(t)
	check, ok := steps(t, r.deps())["bundledock-toolbox"]
	if !ok {
		t.Fatal("Docker toolbox check missing")
	}
	got, detail := check.Run(context.Background())
	if got != NotVerified || !strings.Contains(detail, "ripgrep=14.1.1") {
		t.Fatalf("%s %s", got, detail)
	}
}

func TestToolboxRejectsUnsafeMetadataAndBounds(t *testing.T) {
	for _, mode := range []string{"metadata symlink", "bin symlink", "metadata bound", "duplicate path", "control name", "entry bound", "command bound", "nonexecutable", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			dir := toolboxFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "metadata symlink":
				_ = os.Rename(filepath.Join(dir, "manifest.json"), filepath.Join(dir, "other"))
				_ = os.Symlink("other", filepath.Join(dir, "manifest.json"))
			case "bin symlink":
				_ = os.Rename(filepath.Join(dir, "bin"), filepath.Join(dir, "other"))
				_ = os.Symlink("other", filepath.Join(dir, "bin"))
			case "metadata bound":
				_ = os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(strings.Repeat("x", toolboxMetadataLimit+1)), 0o600)
			case "duplicate path":
				_ = os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Repeat(strings.Repeat("a", 64)+"  bin/rg\n", 2)), 0o600)
			case "control name":
				_ = os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Repeat("a", 64)+"  bin/evil\x1bname\n"), 0o600)
			case "entry bound":
				_ = os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Repeat(strings.Repeat("a", 64)+"  bin/rg\n", toolboxEntryLimit+1)), 0o600)
			case "command bound":
				_ = os.Truncate(filepath.Join(dir, "bin/rg"), (256<<20)+1)
			case "nonexecutable":
				_ = os.Chmod(filepath.Join(dir, "bin/rg"), 0o600)
			case "cancelled":
				cancel()
			}
			got, detail := toolboxCheck(ctx, dir)
			if got == OK || !strings.Contains(detail, "unverified") {
				t.Fatalf("unsafe state reported verified: %s %s", got, detail)
			}
			if strings.ContainsAny(detail, "\x1b\r\n\t") {
				t.Fatal("untrusted control characters reached report")
			}
		})
	}
}

func TestToolboxCommandUsesPinnedDirectoryAfterPathReplacement(t *testing.T) {
	dir := toolboxFixture(t)
	bin, err := toolboxRoot(filepath.Join(dir, "bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bin.Close() }()
	if err := os.Rename(filepath.Join(dir, "bin"), filepath.Join(dir, "old-bin")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "rg"), []byte("outside sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "bin")); err != nil {
		t.Fatal(err)
	}
	digest, _, err := toolboxCommand(context.Background(), bin, "rg", 1024)
	want := fmt.Sprintf("%x", sha256.Sum256([]byte("fixture bytes, never executed")))
	if err != nil || digest != want {
		t.Fatalf("replacement escaped pinned descriptor: digest=%s err=%v", digest, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := toolboxCommand(ctx, bin, "rg", 1024); err == nil {
		t.Fatal("hash ignored cancellation")
	}
}
