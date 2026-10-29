package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"raccoonci/internal/archive"
	"raccoonci/internal/semver"
)

// DefaultAdoptiumURL is the API of Eclipse Adoptium, which publishes Temurin.
const DefaultAdoptiumURL = "https://api.adoptium.net"

const maxJDK = 2 << 30

// javaInstaller installs Temurin (HotSpot, JDK, Linux) from Adoptium. Its
// versions are semver without the build number (17.0.9): when a version has
// several builds (17.0.8+7 and 17.0.8.1+1), the newest is used.
type javaInstaller struct {
	client  *http.Client
	baseURL string

	mu      sync.Mutex
	byMajor map[int][]javaRelease
}

type javaRelease struct {
	Name    string // jdk-17.0.9+9
	Version string // 17.0.9
	SHA256  string
}

// NewJava returns the Java installer; baseURL is api.adoptium.net.
func NewJava(client *http.Client, baseURL string) Installer {
	return &javaInstaller{client: client, baseURL: baseURL, byMajor: map[int][]javaRelease{}}
}

func (j *javaInstaller) BinDirs() []string { return []string{"bin"} }

// javaArch is Adoptium's name for this machine's architecture.
func javaArch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x64"
}

var (
	javaOldRE   = regexp.MustCompile(`^1\.([5-8])(?:\.0)?$`)
	javaMajorRE = regexp.MustCompile(`^(\d+)(?:\.|$)`)
)

// javaSpec reads a version spec into the major version to list and the semver
// range to pick from it. 1.8 is Java 8, and a build suffix (17.0.20+1, as the
// runner images write versions) is dropped.
func javaSpec(spec string) (major int, rng string, err error) {
	spec = strings.TrimPrefix(strings.TrimSpace(spec), "v")
	spec, _, _ = strings.Cut(spec, "+")
	if m := javaOldRE.FindStringSubmatch(spec); m != nil {
		spec = m[1]
	}
	m := javaMajorRE.FindStringSubmatch(spec)
	if m == nil {
		return 0, "", fmt.Errorf("Java %q: give a version such as 17 or 17.0.9", spec)
	}
	major, _ = strconv.Atoi(m[1])
	return major, spec, nil
}

func (j *javaInstaller) list(ctx context.Context, major int) ([]javaRelease, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if rs, ok := j.byMajor[major]; ok {
		return rs, nil
	}
	q := url.Values{
		"architecture": {javaArch()}, "heap_size": {"normal"}, "image_type": {"jdk"},
		"os": {"linux"}, "page_size": {"50"}, "sort_order": {"DESC"}, "vendor": {"eclipse"},
	}
	u := fmt.Sprintf("%s/v3/assets/feature_releases/%d/ga?%s", j.baseURL, major, q.Encode())
	body, err := httpBody(ctx, j.client, u, 16<<20)
	if err != nil {
		if strings.HasSuffix(err.Error(), "404 Not Found") {
			j.byMajor[major] = nil // no such major version
			return nil, nil
		}
		return nil, err
	}
	var raw []struct {
		ReleaseName string `json:"release_name"`
		VersionData struct {
			Semver string `json:"semver"`
		} `json:"version_data"`
		Binaries []struct {
			Package struct {
				Checksum string `json:"checksum"`
			} `json:"package"`
		} `json:"binaries"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("Adoptium release list: %w", err)
	}
	seen := map[string]bool{}
	var out []javaRelease
	for _, r := range raw {
		v, _, _ := strings.Cut(r.VersionData.Semver, "+")
		if len(r.Binaries) == 0 || seen[v] {
			continue // newest first: the first build of a version is the one kept
		}
		if _, ok := semver.Parse(v); !ok {
			continue
		}
		seen[v] = true
		out = append(out, javaRelease{Name: r.ReleaseName, Version: v, SHA256: r.Binaries[0].Package.Checksum})
	}
	j.byMajor[major] = out
	return out, nil
}

func (j *javaInstaller) Resolve(ctx context.Context, spec string) (string, error) {
	major, rng, err := javaSpec(spec)
	if err != nil {
		return "", err
	}
	rs, err := j.list(ctx, major)
	if err != nil {
		return "", err
	}
	versions := make([]string, len(rs))
	for i, r := range rs {
		versions[i] = r.Version
	}
	if v, err := semver.Pick(rng, versions); err == nil {
		return v, nil
	}
	if len(versions) == 0 {
		return "", fmt.Errorf("Adoptium has no Temurin %d for linux/%s", major, javaArch())
	}
	return "", fmt.Errorf("no Temurin matches %q; newest of %d: %s", rng, major, strings.Join(versions[:min(len(versions), 5)], ", "))
}

func (j *javaInstaller) Install(ctx context.Context, version, dest string) error {
	major, _, err := javaSpec(version)
	if err != nil {
		return err
	}
	rs, err := j.list(ctx, major)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Version != version {
			continue
		}
		// The release name has a +, which PathEscape leaves alone.
		name := strings.ReplaceAll(url.PathEscape(r.Name), "+", "%2B")
		u := fmt.Sprintf("%s/v3/binary/version/%s/linux/%s/jdk/hotspot/normal/eclipse", j.baseURL, name, javaArch())
		body, err := httpStream(ctx, j.client, u) // redirects to the release on GitHub
		if err != nil {
			return err
		}
		defer body.Close()
		if err := archive.ExtractTarGzVerified(body, dest, r.SHA256, maxJDK); err != nil {
			return fmt.Errorf("%s: %w", r.Name, err)
		}
		return nil
	}
	return fmt.Errorf("Temurin %s isn't in Adoptium's list for %d", version, major)
}
