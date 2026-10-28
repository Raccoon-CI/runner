// Package nodejs provides the Node.js runtimes JavaScript actions run on
// (runs.using: node20, node24), downloaded from nodejs.org on first use and
// verified against its SHASUMS256.txt.
package nodejs

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

	"racoonci/internal/archive"
	"racoonci/internal/catalog"
)

const maxDownload = 512 << 20

type Runtime struct {
	Dir     string // holds one directory per installed version, e.g. v24.11.0/
	Client  *http.Client
	BaseURL string
	// Catalog maps runs.using to a Node.js major (GitHub forces retired
	// runtimes onto newer ones); nil means catalog.Default().
	Catalog *catalog.Catalog

	mu       sync.Mutex
	resolved map[string]string // major -> node binary
}

func New(dir string) *Runtime {
	return &Runtime{Dir: dir, Client: http.DefaultClient, BaseURL: "https://nodejs.org/dist"}
}

// Binary returns the node binary for runs.using, downloading the newest
// release of that major version once per Runtime. Offline, it falls back to
// the newest one already installed.
func (rt *Runtime) Binary(ctx context.Context, using string) (string, error) {
	cat := rt.Catalog
	if cat == nil {
		cat = catalog.Default()
	}
	major, err := cat.NodeMajor(using)
	if err != nil {
		return "", err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if bin, ok := rt.resolved[major]; ok {
		return bin, nil
	}
	version, err := rt.latest(ctx, major)
	if err != nil {
		if v := rt.installed(major); v != "" {
			return rt.remember(major, v), nil
		}
		return "", err
	}
	if _, err := os.Stat(rt.binary(version)); err != nil {
		if err := rt.install(ctx, version); err != nil {
			return "", err
		}
	}
	return rt.remember(major, version), nil
}

func (rt *Runtime) remember(major, version string) string {
	if rt.resolved == nil {
		rt.resolved = map[string]string{}
	}
	rt.resolved[major] = rt.binary(version)
	return rt.resolved[major]
}

func (rt *Runtime) binary(version string) string {
	return filepath.Join(rt.Dir, version, "bin", "node")
}

// latest returns the newest release of a major version, e.g. "v24.11.0".
func (rt *Runtime) latest(ctx context.Context, major string) (string, error) {
	body, err := rt.get(ctx, rt.BaseURL+"/index.json")
	if err != nil {
		return "", err
	}
	defer body.Close()
	var releases []struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 16<<20)).Decode(&releases); err != nil {
		return "", fmt.Errorf("nodejs.org index: %w", err)
	}
	for _, r := range releases { // newest first
		if strings.HasPrefix(r.Version, "v"+major+".") {
			return r.Version, nil
		}
	}
	return "", fmt.Errorf("no Node.js %s release on %s", major, rt.BaseURL)
}

// installed returns the newest installed version of a major, or "".
func (rt *Runtime) installed(major string) string {
	entries, _ := os.ReadDir(rt.Dir)
	var best []int
	bestName := ""
	for _, e := range entries {
		v := parseVersion(e.Name())
		if v == nil || strconv.Itoa(v[0]) != major {
			continue
		}
		if _, err := os.Stat(rt.binary(e.Name())); err != nil {
			continue
		}
		if best == nil || slices.Compare(v, best) > 0 {
			best, bestName = v, e.Name()
		}
	}
	return bestName
}

func parseVersion(s string) []int {
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 || !strings.HasPrefix(s, "v") {
		return nil
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return out
}

func arch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm":
		return "armv7l"
	}
	return runtime.GOARCH // arm64, ppc64le, s390x
}

func (rt *Runtime) install(ctx context.Context, version string) error {
	file := fmt.Sprintf("node-%s-linux-%s.tar.gz", version, arch())
	want, err := rt.checksum(ctx, version, file)
	if err != nil {
		return err
	}
	body, err := rt.get(ctx, fmt.Sprintf("%s/%s/%s", rt.BaseURL, version, file))
	if err != nil {
		return err
	}
	defer body.Close()

	if err := os.MkdirAll(rt.Dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(rt.Dir, ".download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	h := sha256.New()
	tee := io.TeeReader(io.LimitReader(body, maxDownload), h)
	if err := archive.ExtractTarGz(tee, tmp, maxDownload); err != nil {
		return fmt.Errorf("unpack %s: %w", file, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil { // the rest of the gzip stream
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s: SHA-256 is %s, SHASUMS256.txt says %s", file, got, want)
	}
	dest := filepath.Join(rt.Dir, version)
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

func (rt *Runtime) checksum(ctx context.Context, version, file string) (string, error) {
	body, err := rt.get(ctx, fmt.Sprintf("%s/%s/SHASUMS256.txt", rt.BaseURL, version))
	if err != nil {
		return "", err
	}
	defer body.Close()
	sc := bufio.NewScanner(io.LimitReader(body, 1<<20))
	for sc.Scan() {
		if sum, name, ok := strings.Cut(sc.Text(), "  "); ok && name == file {
			return sum, nil
		}
	}
	return "", fmt.Errorf("%s is not in SHASUMS256.txt for %s", file, version)
}

func (rt *Runtime) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := rt.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}
