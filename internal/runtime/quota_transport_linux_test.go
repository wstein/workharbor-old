//go:build linux

package runtime

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func quotaSocketFixture(t *testing.T, serve func(net.Conn)) QuotaSocketConfig {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wh-quota-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "quota.sock")
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); serve(conn) }()
		}
	}()
	var st unix.Stat_t
	if err := unix.Stat(socket, &st); err != nil {
		t.Fatal(err)
	}
	return QuotaSocketConfig{Path: socket, OwnerUID: st.Uid, HelperUID: st.Uid}
}

func quotaReply(c net.Conn, p QuotaProof) {
	var header [4]byte
	if _, err := io.ReadFull(c, header[:]); err != nil {
		return
	}
	n := binary.BigEndian.Uint32(header[:])
	if n > 65536 {
		return
	}
	if _, err := io.CopyN(io.Discard, c, int64(n)); err != nil {
		return
	}
	b, _ := json.Marshal(p)
	responseSize := len(b)
	if responseSize < 1 || responseSize > quotaFrameLimit {
		return
	}
	binary.BigEndian.PutUint32(header[:], uint32(responseSize))
	_, _ = c.Write(header[:])
	_, _ = c.Write(b)
}

func TestQuotaUnixTransportComposesWithPrepareWithoutSendingPaths(t *testing.T) {
	request, proof := quotaTransportFixture()
	request.Mounts = request.Mounts[:1]
	proof.Resources = proof.Resources[:1]
	seen := make(chan map[string]json.RawMessage, 1)
	config := quotaSocketFixture(t, func(c net.Conn) {
		var h [4]byte
		if _, err := io.ReadFull(c, h[:]); err != nil {
			return
		}
		b := make([]byte, binary.BigEndian.Uint32(h[:]))
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(b, &fields)
		seen <- fields
		reply, _ := json.Marshal(proof)
		responseSize := len(reply)
		if responseSize < 1 || responseSize > quotaFrameLimit {
			return
		}
		binary.BigEndian.PutUint32(h[:], uint32(responseSize))
		_, _ = c.Write(h[:])
		_, _ = c.Write(reply)
	})
	resolver, err := NewUnixQuotaResolver(config)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolver.ResolveQuota(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Allocation != request.Allocation {
		t.Fatal("registered proof not returned")
	}
	select {
	case fields := <-seen:
		if len(fields) != 6 || fields["allocation"] == nil || fields["mounts"] != nil || fields["paths"] != nil {
			t.Fatalf("unexpected request grammar: %v", fields)
		}
	case <-time.After(time.Second):
		t.Fatal("no actual socket request")
	}
	// The same production client can feed the trusted Prepare seam. Evidence is
	// still insufficient for admission, even though transport and proof validate.
	rig := newPrepRig(t)
	s := goodSpec()
	s.Network.Name = request.Environment
	s.Mounts = append(cloneSlice(request.Mounts), Mount{Kind: MountBind, Source: rig.project, Target: "/readonly-tools", ReadOnly: true})
	rig.opts.Owns = func(string) bool { return true }
	rig.opts.DockerQuota = &QuotaPreparation{Allocation: request.Allocation, DaemonID: request.DaemonID, HostID: request.HostID, Resolve: resolver}
	p, err := Prepare(rig.opts, s)
	if err != nil {
		t.Fatal(err)
	}
	obs, ok := p.DockerQuotaObservation()
	if !ok || obs.AdmissionSupported() {
		t.Fatal("transport implied admission")
	}
}

func TestQuotaUnixTransportRejectsUntrustedSocketAndProof(t *testing.T) {
	for _, name := range []string{"peer", "owner", "socket mode", "parent mode", "writable ancestor", "symlink socket", "symlink parent", "wrong proof"} {
		t.Run(name, func(t *testing.T) {
			request, proof := quotaTransportFixture()
			if name == "wrong proof" {
				proof.DaemonID = "other"
			}
			config := quotaSocketFixture(t, func(c net.Conn) { quotaReply(c, proof) })
			switch name {
			case "peer":
				config.HelperUID++
			case "owner":
				config.OwnerUID++
			case "writable ancestor":
				nested := filepath.Join(filepath.Dir(config.Path), "nested")
				if err := os.Mkdir(nested, 0o700); err != nil {
					t.Fatal(err)
				}
				next := filepath.Join(nested, "quota.sock")
				if err := os.Rename(config.Path, next); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Dir(config.Path), 0o777); err != nil { //nolint:gosec // Deliberately unsafe test fixture.
					t.Fatal(err)
				}
				config.Path = next
			case "socket mode":
				if err := os.Chmod(config.Path, 0o666); err != nil { //nolint:gosec // Deliberately unsafe test fixture.
					t.Fatal(err)
				}
			case "parent mode":
				if err := os.Chmod(filepath.Dir(config.Path), 0o777); err != nil { //nolint:gosec // Deliberately unsafe test fixture.
					t.Fatal(err)
				}
			case "symlink socket":
				link := filepath.Join(filepath.Dir(config.Path), "link")
				if err := os.Symlink(config.Path, link); err != nil {
					t.Fatal(err)
				}
				config.Path = link
			case "symlink parent":
				link := filepath.Join(filepath.Dir(config.Path), "link-dir")
				if err := os.Symlink(filepath.Dir(config.Path), link); err != nil {
					t.Fatal(err)
				}
				config.Path = filepath.Join(link, "quota.sock")
			case "wrong proof": // evidence was changed before the server started
			}
			resolver, err := NewUnixQuotaResolver(config)
			if err == nil {
				_, err = resolver.ResolveQuota(context.Background(), request)
			}
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("untrusted %s accepted: %v", name, err)
			}
		})
	}
}

func TestQuotaUnixTransportBoundsAndSanitizesProtocol(t *testing.T) {
	for _, name := range []string{"oversize", "truncated", "trailing", "unknown field", "duplicate", "case duplicate", "extra json", "deep json"} {
		t.Run(name, func(t *testing.T) {
			request, proof := quotaTransportFixture()
			config := quotaSocketFixture(t, func(c net.Conn) {
				var header [4]byte
				if _, err := io.ReadFull(c, header[:]); err != nil {
					return
				}
				if _, err := io.CopyN(io.Discard, c, int64(binary.BigEndian.Uint32(header[:]))); err != nil {
					return
				}
				body, _ := json.Marshal(proof)
				switch name {
				case "oversize":
					binary.BigEndian.PutUint32(header[:], quotaFrameLimit+1)
					_, _ = c.Write(header[:])
					return
				case "truncated":
					binary.BigEndian.PutUint32(header[:], 100)
					_, _ = c.Write(header[:])
					_, _ = c.Write([]byte("private-helper-detail"))
					return
				case "unknown field":
					body = append([]byte(`{"private-helper-detail":true,`), body[1:]...)
				case "duplicate":
					body = append([]byte(`{"Version":1,`), body[1:]...)
				case "case duplicate":
					body = append([]byte(`{"version":1,`), body[1:]...)
				case "extra json":
					body = append(body, []byte(` {}`)...)
				case "deep json":
					body = []byte(strings.Repeat("[", 20) + "0" + strings.Repeat("]", 20))
				}
				responseSize := len(body)
				if responseSize < 1 || responseSize > quotaFrameLimit {
					return
				}
				binary.BigEndian.PutUint32(header[:], uint32(responseSize))
				_, _ = c.Write(header[:])
				_, _ = c.Write(body)
				if name == "trailing" {
					_, _ = c.Write([]byte("private-helper-detail"))
				}
			})
			resolver, err := NewUnixQuotaResolver(config)
			if err != nil {
				t.Fatal(err)
			}
			_, err = resolver.ResolveQuota(context.Background(), request)
			if !errors.Is(err, ErrUnsupported) || strings.Contains(err.Error(), "private-helper-detail") {
				t.Fatalf("unsafe protocol accepted/exposed: %v", err)
			}
		})
	}
}

func TestQuotaUnixTransportCancellationAndDeadlineCloseConnection(t *testing.T) {
	for _, name := range []string{"cancel", "deadline", "already cancelled"} {
		t.Run(name, func(t *testing.T) {
			request, _ := quotaTransportFixture()
			accepted := make(chan struct{}, 1)
			closed := make(chan struct{}, 1)
			var release sync.Once
			defer release.Do(func() { close(closed) })
			failedWrite := make(chan bool, 1)
			config := quotaSocketFixture(t, func(c net.Conn) {
				var h [4]byte
				if _, err := io.ReadFull(c, h[:]); err != nil {
					return
				}
				if _, err := io.CopyN(io.Discard, c, int64(binary.BigEndian.Uint32(h[:]))); err != nil {
					return
				}
				accepted <- struct{}{}
				// Request EOF is the half-close, not cancellation. A write after the client
				// cancels must fail rather than leaving a parked helper connection.
				<-closed
				_ = c.SetWriteDeadline(time.Now().Add(time.Second))
				for range 100 {
					if _, err := c.Write(make([]byte, 1024)); err != nil {
						failedWrite <- true
						return
					}
				}
			})
			resolver, err := NewUnixQuotaResolver(config)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 30*time.Millisecond)
				defer stop()
			}
			if name == "already cancelled" {
				cancel()
			}
			done := make(chan error, 1)
			go func() { _, err := resolver.ResolveQuota(ctx, request); done <- err }()
			if name == "cancel" {
				select {
				case <-accepted:
					cancel()
				case <-time.After(time.Second):
					t.Fatal("no request")
				}
			}
			select {
			case err := <-done:
				want := context.Canceled
				if name == "deadline" {
					want = context.DeadlineExceeded
				}
				if !errors.Is(err, want) || !errors.Is(err, ErrUnsupported) {
					t.Fatal("context outcome lost", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not interrupt transport")
			}
			release.Do(func() { close(closed) })
			if name == "cancel" {
				select {
				case <-failedWrite:
				case <-time.After(time.Second):
					t.Fatal("cancelled connection remained writable")
				}
			}
		})
	}
}

func TestQuotaUnixTransportRefusesReplacementAfterRegistration(t *testing.T) {
	request, proof := quotaTransportFixture()
	config := quotaSocketFixture(t, func(c net.Conn) { quotaReply(c, proof) })
	resolver, err := NewUnixQuotaResolver(config)
	if err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Dir(config.Path)
	moved := oldDir + "-moved"
	if err := os.Rename(oldDir, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	if err := os.Mkdir(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", config.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(config.Path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveQuota(context.Background(), request); !errors.Is(err, ErrUnsupported) {
		t.Fatal("replacement endpoint accepted", err)
	}
}

func TestQuotaUnixPinnedDialDoesNotFollowReplacedAncestry(t *testing.T) {
	original := quotaSocketFixture(t, func(c net.Conn) { _, _ = c.Write([]byte("original")) })
	fd, _, _, err := openQuotaSocket(original)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	dir := filepath.Dir(original.Path)
	moved := dir + "-moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	decoy, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", original.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer decoy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := dialQuotaPinned(ctx, fd, filepath.Base(original.Path))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(conn)
	if err != nil || string(data) != "original" {
		t.Fatalf("dial was redirected: %q %v", data, err)
	}
}

func TestQuotaUnixPrepareRejectsHelperDirectoryMountEvenReadOnly(t *testing.T) {
	config := quotaSocketFixture(t, func(net.Conn) { t.Error("mounted helper boundary reached server") })
	resolver, err := NewUnixQuotaResolver(config)
	if err != nil {
		t.Fatal(err)
	}
	rig := newPrepRig(t)
	s := goodSpec()
	s.Mounts = []Mount{{Kind: MountVolume, Source: "task1-home", Target: "/home/agent"}, {Kind: MountBind, Source: filepath.Dir(config.Path), Target: "/tools", ReadOnly: true}}
	rig.opts.DockerQuota = &QuotaPreparation{Allocation: "registered-1", DaemonID: "daemon-1", HostID: "host-1", Resolve: resolver}
	if _, err := Prepare(rig.opts, s); !errors.Is(err, ErrForbiddenMount) {
		t.Fatalf("readonly helper directory admitted: %v", err)
	}
}

type quotaAliasFS struct {
	OSFS
	alias, actual string
}

func (f quotaAliasFS) Stat(path string) (fs.FileInfo, error) {
	if path == f.alias {
		return f.OSFS.Stat(f.actual)
	}
	return f.OSFS.Stat(path)
}

func TestQuotaUnixPrepareRejectsHelperDirectoryIdentityAlias(t *testing.T) {
	for _, ancestor := range []bool{false, true} {
		t.Run(fmt.Sprint(ancestor), func(t *testing.T) {
			config := quotaSocketFixture(t, func(net.Conn) { t.Error("helper alias reached server") })
			nested := filepath.Join(filepath.Dir(config.Path), "private")
			if err := os.Mkdir(nested, 0o700); err != nil {
				t.Fatal(err)
			}
			next := filepath.Join(nested, "quota.sock")
			if err := os.Rename(config.Path, next); err != nil {
				t.Fatal(err)
			}
			config.Path = next
			resolver, err := NewUnixQuotaResolver(config)
			if err != nil {
				t.Fatal(err)
			}
			rig := newPrepRig(t)
			alias := filepath.Join(rig.project, "alias")
			if err := os.Mkdir(alias, 0o700); err != nil {
				t.Fatal(err)
			}
			actual := filepath.Dir(config.Path)
			if ancestor {
				actual = filepath.Dir(actual)
			}
			rig.opts.FS = quotaAliasFS{alias: alias, actual: actual}
			rig.opts.DockerQuota = &QuotaPreparation{Allocation: "registered-1", DaemonID: "daemon-1", HostID: "host-1", Resolve: resolver}
			s := goodSpec()
			s.Mounts = []Mount{{Kind: MountVolume, Source: "task1-home", Target: "/home/agent"}, {Kind: MountBind, Source: alias, Target: "/tools", ReadOnly: true}}
			if _, err := Prepare(rig.opts, s); !errors.Is(err, ErrForbiddenMount) {
				t.Fatalf("directory identity alias admitted: %v", err)
			}
		})
	}
}

func quotaTransportFixture() (QuotaRequest, QuotaProof) {
	request, proof := quotaFixture()
	request.Mounts = request.Mounts[:1]
	proof.Resources = proof.Resources[:1]
	return request, proof
}

func TestQuotaUnixPrepareRejectsDescendantOfHelperIdentityAlias(t *testing.T) {
	config := quotaSocketFixture(t, func(net.Conn) { t.Error("helper descendant reached server") })
	resolver, err := NewUnixQuotaResolver(config)
	if err != nil {
		t.Fatal(err)
	}
	rig := newPrepRig(t)
	alias := filepath.Join(rig.project, "alias")
	child := filepath.Join(alias, "child")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	rig.opts.FS = quotaAliasFS{alias: alias, actual: filepath.Dir(config.Path)}
	rig.opts.DockerQuota = &QuotaPreparation{Allocation: "registered-1", DaemonID: "daemon-1", HostID: "host-1", Resolve: resolver}
	s := goodSpec()
	s.Mounts = []Mount{{Kind: MountVolume, Source: "task1-home", Target: "/home/agent"}, {Kind: MountBind, Source: child, Target: "/tools", ReadOnly: true}}
	if _, err := Prepare(rig.opts, s); !errors.Is(err, ErrForbiddenMount) {
		t.Fatalf("aliased helper descendant admitted: %v", err)
	}
}

type quotaUnavailableFS struct {
	OSFS
	fail string
}

func (f quotaUnavailableFS) Stat(path string) (fs.FileInfo, error) {
	if path == f.fail {
		return nil, errors.New("private-fs-detail")
	}
	return f.OSFS.Stat(path)
}

func TestQuotaUnixMountIdentityUncertaintyFailsClosed(t *testing.T) {
	config := quotaSocketFixture(t, func(net.Conn) { t.Error("uncertain identity reached server") })
	resolver, err := NewUnixQuotaResolver(config)
	if err != nil {
		t.Fatal(err)
	}
	q := resolver.(*unixQuotaResolver)
	source := t.TempDir()
	mount := []Mount{{Kind: MountBind, Source: source, Target: "/tools", ReadOnly: true}}
	for _, fail := range []string{source, filepath.Dir(config.Path), "/tmp"} {
		if err := q.checkQuotaMounts(quotaUnavailableFS{fail: fail}, mount); !errors.Is(err, ErrUnsupported) || strings.Contains(err.Error(), "private-fs-detail") {
			t.Fatal("unknown identity accepted/exposed", err)
		}
	}
	// Replacing the filesystem's view of the parent must not replace the actual
	// pinned parent identity used by the authenticated socket transport.
	if err := q.checkQuotaMounts(quotaAliasFS{alias: filepath.Dir(config.Path), actual: source}, mount); !errors.Is(err, ErrUnsupported) {
		t.Fatal("pinned parent identity overridden", err)
	}
}
