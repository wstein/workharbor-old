package doctor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
)

const (
	toolboxMetadataLimit = 1 << 20
	toolboxEntryLimit    = 4096
)

// BundleDock owns this inventory. Package versions describe declared upstream
// artifacts; checksum paths describe installed commands, without a guessed
// mapping between packages and commands or a guest execution claim.
type toolboxManifest struct {
	Schema   int    `json:"schema"`
	Platform string `json:"platform"`
	Tools    map[string]struct {
		Version      string `json:"version"`
		URL          string `json:"url"`
		SHA256       string `json:"sha256"`
		SHA512       string `json:"sha512"`
		Verification string `json:"verification"`
	} `json:"tools"`
}

func toolboxToken(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		valid := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._+-", c)
		if !valid {
			return false
		}
	}
	return true
}

func toolboxRoot(dir string) (*os.Root, error) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return nil, errors.New("directory unavailable")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(info, actual) {
		_ = root.Close()
		return nil, errors.New("directory changed")
	}
	return root, nil
}

func toolboxRead(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("not a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("file exceeds bound")
	}
	return data, err
}

func toolboxInventory(data []byte) (map[string]string, error) {
	sums := map[string]string{}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) > toolboxEntryLimit {
		return nil, errors.New("too many entries")
	}
	for _, line := range lines {
		digest, name, ok := strings.Cut(line, "  ")
		decoded, err := hex.DecodeString(digest)
		if !ok || err != nil || len(decoded) != 32 || path.Clean(name) != name || strings.HasPrefix(name, "/") || name == "." || strings.HasPrefix(name, "../") {
			return nil, errors.New("invalid inventory")
		}
		for _, part := range strings.Split(name, "/") {
			if !toolboxToken(part) || part == "." || part == ".." {
				return nil, errors.New("unsafe inventory path")
			}
		}
		if _, duplicate := sums[name]; duplicate {
			return nil, errors.New("duplicate inventory path")
		}
		sums[name] = strings.ToLower(digest)
	}
	return sums, nil
}

func toolboxCheck(ctx context.Context, dir string) (Status, string) {
	const unknown = "BundleDock inventory unavailable or unsupported; versions and provenance unverified; guest execution unverified"
	root, err := toolboxRoot(dir)
	if err != nil {
		return NotVerified, unknown
	}
	defer func() { _ = root.Close() }()
	data, err := toolboxRead(root, "manifest.json", toolboxMetadataLimit)
	if err != nil {
		return NotVerified, unknown
	}
	inventory, err := toolboxRead(root, "SHA256SUMS", toolboxMetadataLimit)
	if err != nil {
		return NotVerified, unknown
	}
	sums, err := toolboxInventory(inventory)
	if err != nil {
		return NotVerified, unknown
	}
	if sums["manifest.json"] != fmt.Sprintf("%x", sha256.Sum256(data)) {
		return NotVerified, "BundleDock manifest checksum does not match; versions and provenance unverified; guest execution unverified"
	}
	var manifest toolboxManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&manifest) != nil || decoder.Decode(new(any)) != io.EOF || manifest.Schema != 1 || (manifest.Platform != "linux/arm64" && manifest.Platform != "linux/amd64") || len(manifest.Tools) == 0 || len(manifest.Tools) > 256 {
		return NotVerified, unknown
	}
	var packages []string
	for name, spec := range manifest.Tools {
		if !toolboxToken(name) || !toolboxToken(spec.Version) {
			return NotVerified, unknown
		}
		digest, size := spec.SHA256, 32
		switch spec.Verification {
		case "publisher-sha256", "tls-download-sha256":
		case "publisher-sha512":
			digest, size = spec.SHA512, 64
		default:
			return NotVerified, unknown
		}
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != size {
			return NotVerified, unknown
		}
		packages = append(packages, name+"="+spec.Version+" (declared "+spec.Verification+", artifact digest "+strings.ToLower(digest)+")")
	}
	sort.Strings(packages)
	var names []string
	for name := range sums {
		if strings.HasPrefix(name, "bin/") {
			command := strings.TrimPrefix(name, "bin/")
			if !toolboxToken(command) {
				return NotVerified, unknown
			}
			names = append(names, command)
		}
	}
	sort.Strings(names)
	if len(names) == 0 || len(names) > 256 {
		return NotVerified, unknown
	}
	// Pin the actual bin directory identity before reading any command. Root
	// confinement alone would permit a symlink into another part of the bundle.
	binInfo, err := root.Lstat("bin")
	if err != nil || !binInfo.IsDir() {
		return Warn, "BundleDock commands unavailable: bin directory missing or unsafe; versions and provenance unverified; guest execution unverified"
	}
	bin, err := root.OpenRoot("bin")
	if err != nil {
		return NotVerified, unknown
	}
	defer func() { _ = bin.Close() }()
	actual, err := bin.Stat(".")
	if err != nil || !os.SameFile(binInfo, actual) {
		return NotVerified, unknown
	}
	status := NotVerified
	var commands []string
	budget := int64(512 << 20)
	for _, name := range names {
		if ctx.Err() != nil {
			return NotVerified, "BundleDock inspection interrupted; integrity and guest execution unverified"
		}
		digest, size, err := toolboxCommand(ctx, bin, name, min(budget, 256<<20))
		budget -= size
		if err != nil || digest != sums["bin/"+name] {
			commands = append(commands, name+": unavailable (missing, unsafe, over bound or checksum mismatch)")
			status = Warn
			continue
		}
		commands = append(commands, name+": checksum matches")
	}
	return status, "BundleDock " + manifest.Platform + "; declared packages: " + strings.Join(packages, ", ") + "; installed command inventory: " + strings.Join(commands, ", ") + "; checksums establish consistency with this inventory only; publisher authenticity unverified; guest execution unverified"
}

// Hash an opened regular executable with fixed memory and a shared byte budget.
// Never execute it, and never follow its final component or re-stat another file.
func toolboxCommand(ctx context.Context, root *os.Root, name string, limit int64) (string, int64, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 || info.Size() > limit {
		return "", 0, errors.New("not a bounded executable regular file")
	}
	h := sha256.New()
	reader := io.LimitReader(f, limit+1)
	buffer := make([]byte, 32<<10)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return "", size, err
		}
		n, err := reader.Read(buffer)
		size += int64(n)
		if size > limit {
			return "", size, errors.New("command exceeds bound")
		}
		_, _ = h.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", size, err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}
