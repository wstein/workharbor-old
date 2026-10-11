//go:build linux

package runtime

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const quotaFrameLimit = 64 << 10

type quotaSocketIdentity struct{ dev, ino uint64 }

type unixQuotaResolver struct {
	config         QuotaSocketConfig
	parent, socket quotaSocketIdentity
}

// NewUnixQuotaResolver pins an existing private local endpoint. It never starts
// a helper, creates a socket, grants access or connects a remote/VM bridge.
// Trusted operator changes require constructing a new resolver. Linux procfs
// and SO_PEERCRED are required; missing facilities fail closed.
func NewUnixQuotaResolver(config QuotaSocketConfig) (QuotaResolver, error) {
	fd, parent, socket, err := openQuotaSocket(config)
	if err != nil {
		return nil, err
	}
	_ = unix.Close(fd)
	return &unixQuotaResolver{config: config, parent: parent, socket: socket}, nil
}

func quotaTransportError(ctx context.Context) error {
	cause := ctx.Err()
	if deadline, ok := ctx.Deadline(); cause == nil && ok && !time.Now().Before(deadline) {
		cause = context.DeadlineExceeded
	}
	return errors.Join(quotaUnsupported(), cause)
}

// ResolveQuota sends selectors only. Expected mount paths stay in the local
// verifier; the host must resolve the handle through its operator registry.
func (q *unixQuotaResolver) ResolveQuota(ctx context.Context, r QuotaRequest) (QuotaProof, error) {
	if err := q.checkQuotaMounts(OSFS{}, r.Mounts); err != nil {
		return QuotaProof{}, err
	}
	for _, id := range []string{r.Allocation, r.DaemonID, r.HostID, r.Environment} {
		if !quotaToken.MatchString(id) {
			return QuotaProof{}, quotaUnsupported()
		}
	}
	if r.RequestedBytes == 0 || len(r.Mounts) == 0 || len(r.Mounts) > 16 {
		return QuotaProof{}, quotaUnsupported()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return QuotaProof{}, quotaTransportError(ctx)
	}
	fd, parent, socket, err := openQuotaSocket(q.config)
	if err != nil {
		return QuotaProof{}, quotaTransportError(ctx)
	}
	defer unix.Close(fd)
	if parent != q.parent || socket != q.socket {
		return QuotaProof{}, quotaUnsupported()
	}
	// Connecting through the pinned directory prevents ancestry replacement from
	// redirecting the dial. Socket identity is checked again after connecting.
	conn, err := dialQuotaPinned(ctx, fd, filepath.Base(q.config.Path))
	if err != nil {
		return QuotaProof{}, quotaTransportError(ctx)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return QuotaProof{}, quotaTransportError(ctx)
	}
	if !quotaPeer(conn, q.config.HelperUID) || !sameQuotaSocket(fd, q.config, q.socket) {
		return QuotaProof{}, quotaUnsupported()
	}
	request := struct {
		Version        int    `json:"version"`
		Allocation     string `json:"allocation"`
		DaemonID       string `json:"daemon_id"`
		HostID         string `json:"host_id"`
		Environment    string `json:"environment"`
		RequestedBytes uint64 `json:"requested_bytes"`
	}{1, r.Allocation, r.DaemonID, r.HostID, r.Environment, r.RequestedBytes}
	payload, err := json.Marshal(request)
	if err != nil {
		return QuotaProof{}, quotaUnsupported()
	}
	payloadSize := len(payload)
	if payloadSize < 1 || payloadSize > quotaFrameLimit {
		return QuotaProof{}, quotaUnsupported()
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(payloadSize)) // bounded fixed fields above
	if _, err := io.Copy(conn, io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(payload))); err != nil {
		return QuotaProof{}, quotaTransportError(ctx)
	}
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		return QuotaProof{}, quotaTransportError(ctx)
	}
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return QuotaProof{}, quotaTransportError(ctx)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > quotaFrameLimit {
		return QuotaProof{}, quotaUnsupported()
	}
	payload = make([]byte, size)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return QuotaProof{}, quotaTransportError(ctx)
	}
	// Exactly one response and EOF are required. A second frame or trailing
	// byte is not silently accepted, and a hanging peer reaches the deadline.
	var extra [1]byte
	if n, err := conn.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		return QuotaProof{}, quotaTransportError(ctx)
	}
	if !quotaJSONUnique(payload) {
		return QuotaProof{}, quotaUnsupported()
	}
	var proof QuotaProof
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proof); err != nil {
		return QuotaProof{}, quotaUnsupported()
	}
	observation, err := verifyQuotaProof(r, proof)
	if err != nil {
		return QuotaProof{}, err
	}
	return observation.Proof(), nil
}

func openQuotaSocket(c QuotaSocketConfig) (int, quotaSocketIdentity, quotaSocketIdentity, error) {
	fail := func() (int, quotaSocketIdentity, quotaSocketIdentity, error) {
		return -1, quotaSocketIdentity{}, quotaSocketIdentity{}, quotaUnsupported()
	}
	if !quotaHostPath(c.Path) || strings.HasPrefix(c.Path, "/home/") || strings.HasPrefix(c.Path, "/root/") || strings.HasPrefix(c.Path, "/Users/") {
		return fail()
	}
	parts := strings.Split(strings.TrimPrefix(c.Path, "/"), "/")
	if len(parts) < 2 || len(parts) > 64 {
		return fail()
	}
	fd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail()
	}
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(fd, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			return fail()
		}
		fd = next
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil || !quotaTrustedDirectory(st, c.OwnerUID) {
			_ = unix.Close(fd)
			return fail()
		}
	}
	var parent unix.Stat_t
	if unix.Fstat(fd, &parent) != nil || parent.Uid != c.OwnerUID || parent.Mode&0o7777 != 0o700 {
		_ = unix.Close(fd)
		return fail()
	}
	socket, ok := quotaSocketStat(fd, c)
	if !ok {
		_ = unix.Close(fd)
		return fail()
	}
	return fd, quotaSocketIdentity{parent.Dev, parent.Ino}, socket, nil
}

func quotaTrustedDirectory(st unix.Stat_t, owner uint32) bool {
	if st.Uid != 0 && st.Uid != owner {
		return false
	}
	// Root's sticky temporary directories protect the following owner-controlled
	// entry. A guest-owned or non-sticky writable ancestor is never accepted.
	return st.Mode&0o022 == 0 || st.Uid == 0 && st.Mode&unix.S_ISVTX != 0
}

func quotaSocketStat(fd int, c QuotaSocketConfig) (quotaSocketIdentity, bool) {
	var st unix.Stat_t
	if unix.Fstatat(fd, filepath.Base(c.Path), &st, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Mode&unix.S_IFMT != unix.S_IFSOCK || st.Mode&0o7777 != 0o600 || st.Uid != c.OwnerUID {
		return quotaSocketIdentity{}, false
	}
	return quotaSocketIdentity{st.Dev, st.Ino}, true
}

func sameQuotaSocket(fd int, c QuotaSocketConfig, want quotaSocketIdentity) bool {
	got, ok := quotaSocketStat(fd, c)
	return ok && got == want
}

func quotaPeer(conn net.Conn, uid uint32) bool {
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		return false
	}
	var peer *unix.Ucred
	var readErr error
	err = raw.Control(func(fd uintptr) { peer, readErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	return err == nil && readErr == nil && peer != nil && peer.Uid == uid
}

// Reject duplicate keys at every depth as well as trailing JSON. Unknown fields
// are rejected separately by the typed decoder. The frame bounds all input.
func quotaJSONUnique(payload []byte) bool {
	d := json.NewDecoder(bytes.NewReader(payload))
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 16 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			keys := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return false
				}
				name, ok := key.(string)
				if !ok {
					return false
				}
				name = strings.ToUpper(strings.ToLower(name))
				if keys[name] {
					return false
				}
				keys[name] = true
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim(']')
		}
		return false
	}
	if !value(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func dialQuotaPinned(ctx context.Context, fd int, name string) (net.Conn, error) {
	address := "/proc/self/fd/" + strconv.Itoa(fd) + "/" + name
	return (&net.Dialer{}).DialContext(ctx, "unix", address)
}

func (q *unixQuotaResolver) checkQuotaMounts(fsys FS, mounts []Mount) error {
	if fsys == nil {
		fsys = OSFS{}
	}
	fd, parentID, socketID, err := openQuotaSocket(q.config)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if parentID != q.parent || socketID != q.socket {
		return quotaUnsupported()
	}
	parent := filepath.Dir(q.config.Path)
	parentInfo, err := fsys.Stat(parent)
	if err != nil {
		return quotaUnsupported()
	}
	st, ok := parentInfo.Sys().(*syscall.Stat_t)
	if !ok || st.Dev != q.parent.dev || st.Ino != q.parent.ino {
		return quotaUnsupported()
	}
	forbidden := func() error {
		return fmt.Errorf("%w: quota helper directory must remain outside guest mounts", ErrForbiddenMount)
	}
	for _, m := range mounts {
		if m.Kind != MountBind {
			continue
		}
		source := filepath.Clean(m.Source)
		if source == parent || strings.HasPrefix(parent, source+"/") || strings.HasPrefix(source, parent+"/") || source == "/" {
			return forbidden()
		}
		sourceInfo, err := fsys.Stat(source)
		if err != nil {
			return quotaUnsupported()
		}
		if fsys.SameFile(sourceInfo, parentInfo) {
			return forbidden()
		}
		// An alias of the helper directory or ANY containing directory exposes its
		// socket, even when neither spelling is a prefix of the other.
		for ancestor := filepath.Dir(parent); ; ancestor = filepath.Dir(ancestor) {
			info, err := fsys.Stat(ancestor)
			if err != nil {
				return quotaUnsupported()
			}
			if fsys.SameFile(sourceInfo, info) {
				return forbidden()
			}
			if filepath.Dir(ancestor) == ancestor {
				break
			}
		}
		// Descendants of an aliased helper directory stay outside guests too. Do
		// not compare all pairs of ancestors: unrelated paths share filesystem roots.
		for ancestor := filepath.Dir(source); ; ancestor = filepath.Dir(ancestor) {
			info, err := fsys.Stat(ancestor)
			if err != nil {
				return quotaUnsupported()
			}
			if fsys.SameFile(parentInfo, info) {
				return forbidden()
			}
			if filepath.Dir(ancestor) == ancestor {
				break
			}
		}
	}
	return nil
}
