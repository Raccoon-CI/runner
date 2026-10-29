package provision

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"raccoonci/internal/archive"
)

// DefaultRustURL is where the Rust project publishes its releases.
const DefaultRustURL = "https://static.rust-lang.org/dist"

// maxRustComponent caps one component's unpacked size (rust-std and rustc are
// several hundred MB).
const maxRustComponent = 2 << 30

// rustComponents are what a job that runs cargo needs: the compiler, cargo,
// the standard library, and the two tools workflows run most (cargo fmt and
// cargo clippy). rustup's default profile has the same set, less the docs.
var rustComponents = []string{"rustc", "cargo", "rust-std", "rustfmt", "clippy"}

// rustInstaller installs Rust from static.rust-lang.org, component by
// component, each checked against the SHA-256 published next to it. rustup is
// not used: it would be one more thing to download and trust, and it keeps its
// toolchains in its own directory rather than the tool cache.
type rustInstaller struct {
	client  *http.Client
	baseURL string
}

// NewRust returns the Rust installer; baseURL is static.rust-lang.org/dist.
func NewRust(client *http.Client, baseURL string) Installer {
	return &rustInstaller{client: client, baseURL: baseURL}
}

func (r *rustInstaller) BinDirs() []string { return []string{"bin"} }

// rustTriple is the target triple of this machine's Rust builds.
func rustTriple() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64-unknown-linux-gnu"
	}
	return "x86_64-unknown-linux-gnu"
}

var (
	rustVersionRE = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+))?$`)
	// [pkg.rust]\nversion = "1.99.1 (58cf8f9c3 2026-10-13)"
	rustStableRE = regexp.MustCompile(`(?m)^\[pkg\.rust\]\s*\nversion = "(\d+\.\d+\.\d+)[ "]`)
)

// Resolve understands "stable" and exact versions (1.80.0, or 1.80 for
// 1.80.0, as rustup reads it). beta, nightly and ranges are not supported:
// there is no list of releases to choose from, and prereleases are not kept.
func (r *rustInstaller) Resolve(ctx context.Context, spec string) (string, error) {
	spec = strings.ToLower(strings.TrimSpace(spec))
	if m := rustVersionRE.FindStringSubmatch(spec); m != nil {
		patch := m[3]
		if patch == "" {
			patch = "0"
		}
		return m[1] + "." + m[2] + "." + patch, nil
	}
	switch spec {
	case "", "stable", "latest", "*":
		body, err := httpBody(ctx, r.client, r.baseURL+"/channel-rust-stable.toml", 16<<20)
		if err != nil {
			return "", err
		}
		m := rustStableRE.FindSubmatch(body)
		if m == nil {
			return "", fmt.Errorf("%s/channel-rust-stable.toml has no Rust version", r.baseURL)
		}
		return string(m[1]), nil
	}
	return "", fmt.Errorf("Rust %q: only stable and exact versions (1.80, 1.80.0) are supported", spec)
}

func (r *rustInstaller) Install(ctx context.Context, version, dest string) error {
	triple := rustTriple()
	for _, c := range rustComponents {
		file := fmt.Sprintf("%s-%s-%s.tar.gz", c, version, triple)
		sumText, err := httpBody(ctx, r.client, r.baseURL+"/"+file+".sha256", 4<<10)
		if err != nil {
			return fmt.Errorf("Rust %s: %w", version, err)
		}
		// "<sha256>  <file>"
		f := strings.Fields(string(sumText))
		if len(f) < 2 || f[1] != file || len(f[0]) != 64 {
			return fmt.Errorf("%s.sha256 does not name %s", file, file)
		}
		body, err := httpStream(ctx, r.client, r.baseURL+"/"+file)
		if err != nil {
			return err
		}
		// Each component unpacks next to dest and installs into it with its own
		// install.sh (rust-installer: it records the component for later
		// upgrades and uninstalls). Only the checked archive is run.
		src := dest + ".src-" + c
		err = archive.ExtractTarGzVerified(body, src, f[0], maxRustComponent)
		body.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		cmd := exec.CommandContext(ctx, "sh", filepath.Join(src, "install.sh"), "--prefix="+dest, "--disable-ldconfig")
		out, err := cmd.CombinedOutput()
		os.RemoveAll(src)
		if err != nil {
			return fmt.Errorf("installing %s: %w: %s", file, err, strings.TrimSpace(tail(string(out), 400)))
		}
	}
	return nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func httpStream(ctx context.Context, c *http.Client, u string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return resp.Body, nil
}

func httpBody(ctx context.Context, c *http.Client, u string, max int64) ([]byte, error) {
	body, err := httpStream(ctx, c, u)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, max))
}
