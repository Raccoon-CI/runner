// Package nodejs knows nodejs.org: its release index and its downloads
// (verified against SHASUMS256.txt). Runtime builds on it to provide the
// Node.js versions JavaScript actions run on (runs.using: node20, node24),
// and the provisioner uses it to install the Node.js a job asks for.
package nodejs

import (
	"bufio"
	"context"
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

	"raccoonci/internal/archive"
	"raccoonci/internal/catalog"
)

const (
	DefaultBaseURL = "https://nodejs.org/dist"
	// UnofficialBaseURL is the Node.js project's own host for builds it does
	// not release: the musl ones, which images such as Alpine need. GitHub's
	// runner takes its Alpine Node.js from there too. It has the same layout
	// and SHASUMS256.txt as nodejs.org.
	UnofficialBaseURL = "https://unofficial-builds.nodejs.org/download/release"
	maxDownload       = 512 << 20
)

// Release is one entry of nodejs.org's index.
type Release struct {
	Version string   // with the leading v, e.g. v24.11.0
	LTS     string   // the LTS line's codename (Iron, Jod), "" if not an LTS release
	Files   []string // the builds it has, e.g. linux-x64, linux-x64-musl
}

// hasBuild says whether the release has this machine's build of the flavor.
func (r Release) hasBuild(musl bool) bool {
	return slices.Contains(r.Files, "linux-"+Arch()+flavor(musl))
}

func flavor(musl bool) string {
	if musl {
		return "-musl"
	}
	return ""
}

// Index lists the releases on baseURL, newest first.
func Index(ctx context.Context, client *http.Client, baseURL string) ([]Release, error) {
	body, err := get(ctx, client, baseURL+"/index.json")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var raw []struct {
		Version string          `json:"version"`
		LTS     json.RawMessage `json:"lts"` // false, or the codename
		Files   []string        `json:"files"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 16<<20)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("nodejs.org index: %w", err)
	}
	out := make([]Release, len(raw))
	for i, r := range raw {
		out[i].Version, out[i].Files = r.Version, r.Files
		_ = json.Unmarshal(r.LTS, &out[i].LTS) // a string for LTS releases, false otherwise
	}
	return out, nil
}

// Arch is nodejs.org's name for this machine's architecture.
func Arch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm":
		return "armv7l"
	}
	return runtime.GOARCH // arm64, ppc64le, s390x
}

// Fetch downloads Node.js version (with the leading v) for this machine into
// dest, without the archive's top-level directory, so dest/bin/node exists.
// dest only appears once the download has been verified.
func Fetch(ctx context.Context, client *http.Client, baseURL, version, dest string) error {
	return fetch(ctx, client, baseURL, version, dest, false)
}

// FetchMusl is Fetch for the musl build (baseURL is UnofficialBaseURL).
func FetchMusl(ctx context.Context, client *http.Client, baseURL, version, dest string) error {
	return fetch(ctx, client, baseURL, version, dest, true)
}

func fetch(ctx context.Context, client *http.Client, baseURL, version, dest string, musl bool) error {
	file := fmt.Sprintf("node-%s-linux-%s%s.tar.gz", version, Arch(), flavor(musl))
	want, err := checksum(ctx, client, baseURL, version, file)
	if err != nil {
		return err
	}
	body, err := get(ctx, client, fmt.Sprintf("%s/%s/%s", baseURL, version, file))
	if err != nil {
		return err
	}
	defer body.Close()
	if err := archive.ExtractTarGzVerified(body, dest, want, maxDownload); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}

func checksum(ctx context.Context, client *http.Client, baseURL, version, file string) (string, error) {
	body, err := get(ctx, client, fmt.Sprintf("%s/%s/SHASUMS256.txt", baseURL, version))
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

func get(ctx context.Context, client *http.Client, url string) (io.ReadCloser, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// Runtime provides the Node.js JavaScript actions run on.
type Runtime struct {
	Dir     string // holds one directory per installed version, e.g. v24.11.0/
	Client  *http.Client
	BaseURL string
	// Catalog maps runs.using to a Node.js major (GitHub forces retired
	// runtimes onto newer ones); nil means catalog.Default().
	Catalog *catalog.Catalog
	// Musl provides the musl build, for job containers whose image has no
	// glibc. Give it a Dir of its own.
	Musl bool

	mu       sync.Mutex
	resolved map[string]string // major -> node binary
}

func New(dir string) *Runtime {
	return &Runtime{Dir: dir, Client: http.DefaultClient, BaseURL: DefaultBaseURL}
}

// NewMusl is New for the musl build.
func NewMusl(dir string) *Runtime {
	return &Runtime{Dir: dir, Client: http.DefaultClient, BaseURL: UnofficialBaseURL, Musl: true}
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
		if err := fetch(ctx, rt.Client, rt.BaseURL, version, filepath.Join(rt.Dir, version), rt.Musl); err != nil {
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
	releases, err := Index(ctx, rt.Client, rt.BaseURL)
	if err != nil {
		return "", err
	}
	for _, r := range releases { // newest first
		// Not every release has every build, the unofficial ones least of all.
		if strings.HasPrefix(r.Version, "v"+major+".") && (r.hasBuild(rt.Musl) || len(r.Files) == 0) {
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
