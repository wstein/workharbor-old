package serve

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wstein/workharbor/internal/agent"
	"github.com/wstein/workharbor/internal/baseimage"
	"github.com/wstein/workharbor/internal/config"
	"github.com/wstein/workharbor/internal/domain"
	"github.com/wstein/workharbor/internal/runtime"
	"github.com/wstein/workharbor/internal/service"
	"github.com/wstein/workharbor/internal/toolstore"
)

func TestDockerFactoryRequiresLinuxShimAndExplicitStockImage(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(name, 0o700) //nolint:gosec // restore disposable immutable fixture directories for cleanup
			}
			return err
		})
	})
	c := &config.Config{Runtime: config.Runtime{Backend: "docker", Endpoint: "unix:///tmp/absent-engine.sock", Toolbox: t.TempDir()}, Roots: config.Roots{ToolStore: root}}
	bin := ToolsMount + "/profiles/test/bin"
	if _, err := configuredRuntime(c, bin); err == nil {
		t.Fatal("missing stock image accepted")
	}
	c.Environment.Image = "golang:1.25"
	if _, err := configuredRuntime(c, bin); err == nil {
		t.Fatal("missing guest shim accepted")
	}
	// A source fixture is enough to validate the ELF/platform prerequisite; no
	// executable artifact is built or run by this factory test.
	source := filepath.Join(t.TempDir(), "shim")
	data := make([]byte, 64)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(data[16:], 2)
	binary.LittleEndian.PutUint16(data[18:], 183)
	binary.LittleEndian.PutUint32(data[20:], 1)
	binary.LittleEndian.PutUint16(data[52:], 64)
	if err := os.WriteFile(source, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store := &toolstore.Store{Root: root}
	entry, err := store.AddFile("whr-shim", "test", "linux-arm64", source)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Profile("test", entry); err != nil {
		t.Fatal(err)
	}
	rt, err := configuredRuntime(c, bin)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Name() != "docker" {
		t.Fatalf("selected %s", rt.Name())
	}
	if _, ok := rt.(baseimage.Builder); ok {
		t.Fatal("Docker falsely advertises image builder")
	}
	// Build the actual production dependencies, then reconcile through the
	// selected Unix HTTP adapter. No Apple executable/proxy or daemon is used.
	socketDir, err := os.MkdirTemp("", "whr-factory-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "engine.sock")
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var listed atomic.Bool
	var receivedPath atomic.Value
	server := &http.Server{Handler: factoryExecutionEngine(t, &listed, &receivedPath), ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "api.token")
	if err := os.WriteFile(tokenFile, []byte("offline-factory-test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Runtime.Endpoint = "unix://" + socket
	c.GitHub = config.GitHub{AppID: 1, KeyFile: keyFile}
	c.APITokenFile = tokenFile
	c.AgentAllowedTools = []string{"Read"}
	c.Repositories = []config.Repository{{Name: "fixture/project"}}
	deps, closeFn, err := Build(c, "/absent/whr", t.TempDir(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeFn)
	if deps.Runtime.Name() != "docker" || deps.BuildDir != DockerBuild || deps.Agent.Name() != "claude-code" {
		t.Fatal("production dependency factory lost selected runtime/cache/agent")
	}
	supervisor := service.New(deps.Store, deps.Runtime, deps.Agent, nil, serviceConfig(deps, t.Logf))
	t.Cleanup(supervisor.Shutdown)
	if _, err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !listed.Load() {
		t.Fatal("supervisor did not use configured Docker socket adapter")
	}
	// Exercise the actual production Prepare closure and selected adapter. The
	// existing fake container avoids pretending unsupported quota admission works.
	prepared, err := deps.Prepare(deps.Spec(domain.Workspace{ID: "w"}))
	if err != nil {
		t.Fatal(err)
	}
	if plan, ok := prepared.PrivateExecVolume(); !ok || plan.Name != "whr-exec-w" || plan.MemoryMB != 2 {
		t.Fatalf("production private plan=%+v %v", plan, ok)
	}
	if _, err := deps.Runtime.Provision(context.Background(), prepared); !errors.Is(err, runtime.ErrUnsupported) {
		t.Fatalf("quota gate=%v", err)
	}
	stream, err := deps.Runtime.Exec(context.Background(), "abcdef", runtime.ExecRequest{Cmd: []string{"git", "--version"}})
	if err != nil {
		t.Fatal(err)
	}
	out, _, code, err := runtime.Collect(stream)
	if err != nil || code != 0 || string(out) != "service-output\n" {
		t.Fatalf("service exec code=%d error=%v", code, err)
	}
	pathData, _ := receivedPath.Load().(string)
	if !strings.Contains(pathData, "PATH=/opt/bundledock/toolbox/bin:/tools/profiles/test/bin:/usr/local/go/bin:/usr/bin:/bin\n") {
		t.Fatalf("factory service executable paths lost: %s", pathData)
	}
}

func TestDockerSpecSeparatesUtilityBundlePersistentHomeAndDisposableCache(t *testing.T) {
	o := SpecOptions{Owner: Owner, Backend: "docker", ToolStore: "/agent-store", Toolbox: "/bundle", Env: config.Environment{Image: "golang:1.25"}.Resolved()}
	s := o.For(domain.Workspace{ID: "w"})
	if s.Image != "golang:1.25" || s.Egress != nil || !s.Network.Internal {
		t.Fatalf("stock/internal Docker spec = %+v", s)
	}
	foundToolbox, foundHome := false, false
	for _, m := range s.Mounts {
		if m.Target == "/opt/bundledock/toolbox" {
			foundToolbox = m.Source == "/bundle" && m.ReadOnly
		}
		if m.Target == GuestHome {
			foundHome = m.Kind == runtime.MountVolume && !m.ReadOnly
		}
		if m.Target == GuestBuild {
			t.Fatal("Docker reused persistent Apple build volume")
		}
	}
	if !foundToolbox || !foundHome {
		t.Fatal("missing separate utility bundle or persistent login home")
	}
	foundCache := false
	for _, d := range s.Tmpfs {
		if d == DockerBuild {
			foundCache = true
		}
	}
	if !foundCache {
		t.Fatal("Docker cache is not disposable outside workspace")
	}
}

func TestBuildSelectsDockerBeforeAppleProxyOrImageBuilder(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "store"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "profiles", "test", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := &config.Config{Runtime: config.Runtime{Backend: "docker", Endpoint: "unix:///tmp/absent-engine.sock", Toolbox: t.TempDir()}, Roots: config.Roots{ToolStore: root}, AgentAllowedTools: []string{"Read"}}
	if _, _, err := Build(c, "/nonexistent/whr", t.TempDir(), func(string, ...any) {}); err == nil || !strings.Contains(err.Error(), "existing stock language image") {
		t.Fatalf("production factory failed before Docker selection: %v", err)
	}
}

func TestDockerGeneratedSpecPassesPrepareWithoutRepositoryPATHOverride(t *testing.T) {
	o := SpecOptions{Owner: Owner, Backend: "docker", ToolStore: t.TempDir(), Toolbox: t.TempDir(), Env: config.Environment{Image: "golang:1.25"}.Resolved()}
	spec := o.For(domain.Workspace{ID: "w"})
	if err := spec.Validate(); err != nil {
		t.Fatalf("generated Docker spec invalid: %v", err)
	}
	if _, err := runtime.Prepare(runtime.PrepareOptions{FS: runtime.OSFS{}, Home: t.TempDir(), Roots: []string{o.ToolStore, o.Toolbox}, Owns: ownsVolumes(spec)}, spec); err != nil {
		t.Fatalf("generated Docker spec cannot prepare: %v", err)
	}
	c := &config.Config{Runtime: config.Runtime{Backend: "docker"}}
	start := AgentSpecFor(c, agent.AuthSubscription)(domain.Task{}, domain.Run{})
	for _, entry := range start.Env {
		if strings.HasPrefix(entry, "PATH=") {
			t.Fatal("agent PATH discarded stock-image toolchain entries")
		}
	}
}

func TestFactoryAloneAppliesDockerOutboundChoice(t *testing.T) {
	o := SpecOptions{Owner: Owner, Backend: "docker", ToolStore: t.TempDir(), Toolbox: t.TempDir(), Env: config.Environment{Image: "golang:1.25"}.Resolved()}
	s := o.For(domain.Workspace{ID: "w"})
	opts := runtime.PrepareOptions{FS: runtime.OSFS{}, Home: t.TempDir(), Roots: []string{o.ToolStore, o.Toolbox}, Owns: ownsVolumes(s)}
	dockerConfig := &config.Config{Runtime: config.Runtime{Backend: "docker"}}
	p, err := prepareRuntimeSpec(dockerConfig, opts, s)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := p.DockerOutboundNetwork(); !ok || n.Internal || n.Name != s.Network.Name {
		t.Fatalf("factory outbound=%+v %v", n, ok)
	}
	p, err = prepareRuntimeSpec(&config.Config{}, opts, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.DockerOutboundNetwork(); ok || !p.Spec().Network.Internal {
		t.Fatal("Apple factory widened network")
	}
}

func TestDockerFactoryPreparationSealsOnlyItsPrivateExecVolume(t *testing.T) {
	o := SpecOptions{Owner: Owner, Backend: "docker", ToolStore: t.TempDir(), Toolbox: t.TempDir(), Env: config.Environment{Image: "golang:1.25"}.Resolved()}
	s := o.For(domain.Workspace{ID: "w"})
	opts := runtime.PrepareOptions{FS: runtime.OSFS{}, Home: t.TempDir(), Roots: []string{o.ToolStore, o.Toolbox}, Owns: ownsVolumes(s)}
	c := &config.Config{Runtime: config.Runtime{Backend: "docker"}}
	p, err := prepareRuntimeSpec(c, opts, s)
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := p.PrivateExecVolume()
	if !ok || plan.Name != "whr-exec-w" || plan.MemoryMB != 2 || plan.Association != "whr-net-w" {
		t.Fatalf("factory private plan=%+v %v", plan, ok)
	}
	s.Mounts = append(s.Mounts, runtime.Mount{Kind: runtime.MountVolume, Source: plan.Name, Target: "/alias"})
	if _, err := prepareRuntimeSpec(c, opts, s); err == nil {
		t.Fatal("repository mount aliased private volume")
	}
}
