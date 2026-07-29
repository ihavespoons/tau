package tools

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ihavespoons/tau/config"
)

// grep and find are built on ripgrep and fd rather than reimplemented.
//
// Both respect .gitignore, skip binary files, and are fast enough that a
// search over a large repository stays inside a turn — behaviour a hand-rolled
// walker would spend a great deal of code approximating badly. The cost is a
// dependency tau has to find or fetch, which is what this file is.

// binarySpec describes a tool tau can download.
type binarySpec struct {
	// Name is the key tau refers to it by.
	Name string
	// Repo is the GitHub repository releases are published under.
	Repo string
	// Binary is the executable's name inside the archive.
	Binary string
	// SystemNames are the commands to look for on PATH first, most likely
	// first. Debian ships fd as fdfind, which is why this is a list.
	SystemNames []string
	// Asset builds the release asset name for a platform.
	Asset func(version, goos, goarch string) string
}

var binarySpecs = map[string]binarySpec{
	"rg": {
		Name: "ripgrep", Repo: "BurntSushi/ripgrep", Binary: "rg",
		SystemNames: []string{"rg"},
		Asset: func(version, goos, goarch string) string {
			arch := rustArch(goarch)
			switch goos {
			case "darwin":
				return fmt.Sprintf("ripgrep-%s-%s-apple-darwin.tar.gz", version, arch)
			case "linux":
				// The musl build is statically linked, so it runs on a host
				// whose glibc is older than the one it was built against.
				if goarch == "arm64" {
					return fmt.Sprintf("ripgrep-%s-aarch64-unknown-linux-gnu.tar.gz", version)
				}
				return fmt.Sprintf("ripgrep-%s-x86_64-unknown-linux-musl.tar.gz", version)
			case "windows":
				return fmt.Sprintf("ripgrep-%s-%s-pc-windows-msvc.zip", version, arch)
			}
			return ""
		},
	},
	"fd": {
		Name: "fd", Repo: "sharkdp/fd", Binary: "fd",
		SystemNames: []string{"fd", "fdfind"},
		Asset: func(version, goos, goarch string) string {
			arch := rustArch(goarch)
			switch goos {
			case "darwin":
				return fmt.Sprintf("fd-v%s-%s-apple-darwin.tar.gz", version, arch)
			case "linux":
				return fmt.Sprintf("fd-v%s-%s-unknown-linux-gnu.tar.gz", version, arch)
			case "windows":
				return fmt.Sprintf("fd-v%s-%s-pc-windows-msvc.zip", version, arch)
			}
			return ""
		},
	},
}

func rustArch(goarch string) string {
	if goarch == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}

// ErrOffline reports that a binary is missing and tau was told not to fetch it.
var ErrOffline = errors.New("offline: not downloading")

// offline reports whether tau is forbidden from reaching the network. TAU_ is
// checked first; PI_ is honoured so an existing setup keeps working.
func offline() bool {
	for _, key := range []string{"TAU_OFFLINE", "PI_OFFLINE"} {
		switch strings.ToLower(os.Getenv(key)) {
		case "1", "true", "yes":
			return true
		}
	}
	return false
}

// resolved caches the path found for a tool, so a session does not re-probe
// PATH on every search.
var resolved sync.Map // name → string

// binaryPath returns a usable path for a tool without downloading: tau's own
// bin directory first, then PATH.
//
// tau's copy wins over the system one. It is the version tau downloaded and
// knows the flags of; a system install could be old enough to reject them.
func binaryPath(name string) (string, bool) {
	if cached, ok := resolved.Load(name); ok {
		path, _ := cached.(string)
		return path, path != ""
	}

	spec, ok := binarySpecs[name]
	if !ok {
		return "", false
	}

	local := filepath.Join(config.BinDir(), spec.Binary+exeSuffix())
	if info, err := os.Stat(local); err == nil && !info.IsDir() {
		resolved.Store(name, local)
		return local, true
	}

	for _, candidate := range spec.SystemNames {
		if path, err := exec.LookPath(candidate); err == nil {
			resolved.Store(name, path)
			return path, true
		}
	}
	return "", false
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// downloading serializes installs per tool, so two concurrent searches do not
// both fetch the same archive.
var downloading sync.Map // name → *sync.Mutex

func installLock(name string) *sync.Mutex {
	mu, _ := downloading.LoadOrStore(name, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// ensureBinary returns a usable path, downloading the tool if it is missing.
func ensureBinary(ctx context.Context, name string) (string, error) {
	if path, ok := binaryPath(name); ok {
		return path, nil
	}
	spec, ok := binarySpecs[name]
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	if offline() {
		return "", fmt.Errorf("%s is not installed and %w", spec.Name, ErrOffline)
	}

	mu := installLock(name)
	mu.Lock()
	defer mu.Unlock()

	// Another goroutine may have installed it while this one waited.
	if path, ok := binaryPath(name); ok {
		return path, nil
	}
	if err := install(ctx, spec); err != nil {
		return "", fmt.Errorf("installing %s: %w", spec.Name, err)
	}

	path, ok := binaryPath(name)
	if !ok {
		return "", fmt.Errorf("%s was downloaded but is not where it was expected", spec.Name)
	}
	return path, nil
}

const (
	metadataTimeout = 10 * time.Second
	downloadTimeout = 2 * time.Minute
)

// install downloads the latest release and extracts the binary.
func install(ctx context.Context, spec binarySpec) error {
	version, err := latestVersion(ctx, spec.Repo)
	if err != nil {
		return err
	}

	asset := spec.Asset(version, runtime.GOOS, runtime.GOARCH)
	if asset == "" {
		return fmt.Errorf("no %s build for %s/%s", spec.Name, runtime.GOOS, runtime.GOARCH)
	}
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s%s/%s",
		spec.Repo, tagPrefix(spec), version, asset)

	archive, err := fetch(ctx, url, downloadTimeout)
	if err != nil {
		return err
	}

	binDir := config.BinDir()
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	target := filepath.Join(binDir, spec.Binary+exeSuffix())

	// Extract to a temporary name and rename into place, so a failed or
	// interrupted download never leaves a half-written executable that the
	// next run would happily try to execute.
	tmp := target + ".partial"
	defer func() { _ = os.Remove(tmp) }()

	wanted := spec.Binary + exeSuffix()
	if strings.HasSuffix(asset, ".zip") {
		err = extractZip(archive, wanted, tmp)
	} else {
		err = extractTarGz(archive, wanted, tmp)
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// tagPrefix is the "v" some projects put on their tags and others do not.
func tagPrefix(spec binarySpec) string {
	if spec.Repo == "sharkdp/fd" {
		return "v"
	}
	return ""
}

func latestVersion(ctx context.Context, repo string) (string, error) {
	body, err := fetch(ctx, "https://api.github.com/repos/"+repo+"/releases/latest", metadataTimeout)
	if err != nil {
		return "", err
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return "", err
	}
	if release.TagName == "" {
		return "", errors.New("the latest release has no tag")
	}
	return strings.TrimPrefix(release.TagName, "v"), nil
}

func fetch(ctx context.Context, url string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tau")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	// The cap is a safety net rather than a real limit: these archives are a
	// few megabytes, and anything far larger is not what tau asked for.
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// extractTarGz writes the named file out of a gzipped tarball.
func extractTarGz(archive []byte, name, dest string) error {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// The binary sits inside a versioned directory, so only the base name
		// is matched — and only a regular file is accepted, so a crafted
		// archive cannot write through a symlink.
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != name {
			continue
		}
		return writeFile(dest, tr)
	}
	return fmt.Errorf("%s was not in the archive", name)
}

func extractZip(archive []byte, name, dest string) error {
	zr, err := zip.NewReader(strings.NewReader(string(archive)), int64(len(archive)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || filepath.Base(f.Name) != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		return writeFile(dest, rc)
	}
	return fmt.Errorf("%s was not in the archive", name)
}

func writeFile(dest string, r io.Reader) error {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
