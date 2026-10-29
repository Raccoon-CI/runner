package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"raccoonci/internal/archive"
	"raccoonci/internal/nodejs"
	"raccoonci/internal/semver"
)

// DefaultGoURL is where Go publishes its releases.
const DefaultGoURL = "https://go.dev/dl"

const maxGoDownload = 1 << 30

// goInstaller installs Go from go.dev/dl.
type goInstaller struct {
	client  *http.Client
	baseURL string

	mu       sync.Mutex
	releases []goRelease
}

type goRelease struct {
	Version string `json:"version"` // go1.24.1, go1.24 (= 1.24.0), go1.25rc1
	Stable  bool   `json:"stable"`
	Files   []struct {
		Filename string `json:"filename"`
		OS       string `json:"os"`
		Arch     string `json:"arch"`
		SHA256   string `json:"sha256"`
		Kind     string `json:"kind"`
	} `json:"files"`
}

// NewGo returns the Go installer; baseURL is go.dev/dl.
func NewGo(client *http.Client, baseURL string) Installer {
	return &goInstaller{client: client, baseURL: baseURL}
}

func (g *goInstaller) BinDirs() []string { return []string{"bin"} }

var goVersionRE = regexp.MustCompile(`^go(\d+)\.(\d+)(?:\.(\d+))?$`)

// normalize turns Go's version names into semver: go1.24 is 1.24.0. Release
// candidates and betas are not stable and are left out.
func normalizeGo(name string) (string, bool) {
	m := goVersionRE.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	patch := m[3]
	if patch == "" {
		patch = "0"
	}
	return m[1] + "." + m[2] + "." + patch, true
}

func (g *goInstaller) list(ctx context.Context) ([]goRelease, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.releases != nil {
		return g.releases, nil
	}
	url := g.baseURL + "/?mode=json&include=all"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	var all []goRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&all); err != nil {
		return nil, fmt.Errorf("go.dev release list: %w", err)
	}
	for _, r := range all {
		if _, ok := normalizeGo(r.Version); ok && r.Stable {
			g.releases = append(g.releases, r)
		}
	}
	return g.releases, nil
}

// Resolve understands what setup-go does: semver ranges (1.24 is the newest
// 1.24.x), plus stable (the newest release) and oldstable (the newest of the
// minor release before it).
func (g *goInstaller) Resolve(ctx context.Context, spec string) (string, error) {
	releases, err := g.list(ctx)
	if err != nil {
		return "", err
	}
	versions := make([]string, len(releases))
	for i, r := range releases {
		versions[i], _ = normalizeGo(r.Version)
	}
	spec = strings.TrimPrefix(strings.TrimSpace(spec), "go")
	switch strings.ToLower(spec) {
	case "stable":
		return semver.Pick("*", versions)
	case "oldstable":
		newest, err := semver.Pick("*", versions)
		if err != nil {
			return "", err
		}
		v, _ := semver.Parse(newest)
		return semver.Pick(fmt.Sprintf("<%d.%d.0", v.Major, v.Minor), versions)
	}
	return semver.Pick(spec, versions)
}

func (g *goInstaller) Install(ctx context.Context, version, dest string) error {
	releases, err := g.list(ctx)
	if err != nil {
		return err
	}
	for _, r := range releases {
		if v, _ := normalizeGo(r.Version); v != version {
			continue
		}
		for _, f := range r.Files {
			if f.Kind != "archive" || f.OS != "linux" || f.Arch != goArch() {
				continue
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+"/"+f.Filename, nil)
			if err != nil {
				return err
			}
			resp, err := g.client.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("GET %s: %s", req.URL, resp.Status)
			}
			if err := archive.ExtractTarGzVerified(resp.Body, dest, f.SHA256, maxGoDownload); err != nil {
				return fmt.Errorf("%s: %w", f.Filename, err)
			}
			return nil
		}
		return fmt.Errorf("go%s has no linux/%s download", version, goArch())
	}
	return fmt.Errorf("go%s isn't in go.dev's release list", version)
}

// goArch is go.dev's name for this machine's architecture.
func goArch() string {
	if runtime.GOARCH == "arm" {
		return "armv6l"
	}
	return runtime.GOARCH
}

// DefaultInstallers are the installers Raccoon CI ships, by the names the
// catalog refers to.
func DefaultInstallers(client *http.Client) map[string]Installer {
	if client == nil {
		client = http.DefaultClient
	}
	return map[string]Installer{
		"node":   NewNode(client, nodejs.DefaultBaseURL),
		"go":     NewGo(client, DefaultGoURL),
		"python": NewPython(client, DefaultPythonLatestURL, DefaultPythonReleaseURL),
		"rust":   NewRust(client, DefaultRustURL),
		"java":   NewJava(client, DefaultAdoptiumURL),
	}
}
