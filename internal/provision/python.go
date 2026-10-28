package provision

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"

	"encoding/json"

	"raccoonci/internal/archive"
	"raccoonci/internal/semver"
)

// Where python-build-standalone publishes. Its latest-release.json names the
// newest release's tag; the builds and their checksums are in that release.
// (GitHub's own Python builds, which setup-python downloads, are made for
// Ubuntu: setup-python refuses to install them on Debian.)
const (
	DefaultPythonLatestURL  = "https://raw.githubusercontent.com/astral-sh/python-build-standalone/latest-release/latest-release.json"
	DefaultPythonReleaseURL = "https://github.com/astral-sh/python-build-standalone/releases/download"
)

const maxPythonDownload = 1 << 30

type pythonInstaller struct {
	client     *http.Client
	latestURL  string
	releaseURL string

	mu     sync.Mutex
	tag    string
	builds map[string]pythonBuild // version -> build
}

type pythonBuild struct{ file, sha256 string }

// NewPython returns the Python installer. It installs CPython from
// python-build-standalone: self-contained builds that run on any glibc
// Linux, which is why they work on Debian where GitHub's Ubuntu builds don't.
func NewPython(client *http.Client, latestURL, releaseURL string) Installer {
	return &pythonInstaller{client: client, latestURL: latestURL, releaseURL: releaseURL}
}

func (p *pythonInstaller) BinDirs() []string { return []string{"bin"} }

func pythonTriple() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64-unknown-linux-gnu"
	}
	return "x86_64-unknown-linux-gnu"
}

func (p *pythonInstaller) get(ctx context.Context, u string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return resp.Body, nil
}

// load reads the latest release's list of builds, once.
func (p *pythonInstaller) load(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.builds != nil {
		return nil
	}
	body, err := p.get(ctx, p.latestURL)
	if err != nil {
		return err
	}
	var latest struct {
		Tag string `json:"tag"`
	}
	err = json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&latest)
	body.Close()
	if err != nil || latest.Tag == "" || strings.ContainsAny(latest.Tag, "/\\") {
		return fmt.Errorf("python-build-standalone's latest-release.json: unexpected content (%v)", err)
	}
	sums, err := p.get(ctx, fmt.Sprintf("%s/%s/SHA256SUMS", p.releaseURL, latest.Tag))
	if err != nil {
		return err
	}
	defer sums.Close()
	text, err := io.ReadAll(io.LimitReader(sums, 8<<20))
	if err != nil {
		return err
	}
	// "<sha256>  cpython-3.12.16+20261026-x86_64-unknown-linux-gnu-install_only.tar.gz";
	// pre-releases (3.16.0a1) don't match the version pattern, and so are left out.
	re := regexp.MustCompile(`^([0-9a-f]{64})\s+(cpython-(\d+\.\d+\.\d+)\+\d+-` + regexp.QuoteMeta(pythonTriple()) + `-install_only\.tar\.gz)$`)
	builds := map[string]pythonBuild{}
	for _, line := range strings.Split(string(text), "\n") {
		if m := re.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			builds[m[3]] = pythonBuild{file: m[2], sha256: m[1]}
		}
	}
	if len(builds) == 0 {
		return fmt.Errorf("release %s of python-build-standalone has no %s builds", latest.Tag, pythonTriple())
	}
	p.tag, p.builds = latest.Tag, builds
	return nil
}

func (p *pythonInstaller) versions() []string {
	out := make([]string, 0, len(p.builds))
	for v := range p.builds {
		out = append(out, v)
	}
	return out
}

// Resolve understands semver ranges as setup-python does (3.12, 3.x,
// >=3.9). The latest release only has the newest patch of each minor
// version, so an exact older patch is reported as missing, with the one
// that is there.
func (p *pythonInstaller) Resolve(ctx context.Context, spec string) (string, error) {
	if err := p.load(ctx); err != nil {
		return "", err
	}
	v, err := semver.Pick(spec, p.versions())
	if err == nil {
		return v, nil
	}
	if want, ok := semver.Parse(spec); ok {
		if near, nerr := semver.Pick(fmt.Sprintf("%d.%d", want.Major, want.Minor), p.versions()); nerr == nil {
			return "", fmt.Errorf("Python %s isn't available (python-build-standalone's latest release has %s)", spec, near)
		}
	}
	return "", fmt.Errorf("no Python matches %q in python-build-standalone's latest release (%s); versions there: %s", spec, p.tag, strings.Join(sortedVersions(p.versions()), ", "))
}

func sortedVersions(vs []string) []string {
	slices.SortFunc(vs, func(a, b string) int {
		x, _ := semver.Parse(a)
		y, _ := semver.Parse(b)
		return x.Compare(y)
	})
	return vs
}

func (p *pythonInstaller) Install(ctx context.Context, version, dest string) error {
	if err := p.load(ctx); err != nil {
		return err
	}
	b, ok := p.builds[version]
	if !ok {
		return fmt.Errorf("Python %s isn't in python-build-standalone's release %s", version, p.tag)
	}
	// The + in the file name is escaped, as in the URLs GitHub shows.
	name := strings.ReplaceAll(url.PathEscape(b.file), "+", "%2B")
	body, err := p.get(ctx, fmt.Sprintf("%s/%s/%s", p.releaseURL, p.tag, name))
	if err != nil {
		return err
	}
	defer body.Close()
	if err := archive.ExtractTarGzVerified(body, dest, b.sha256, maxPythonDownload); err != nil {
		return fmt.Errorf("%s: %w", b.file, err)
	}
	return nil
}
