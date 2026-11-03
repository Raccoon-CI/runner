package provision

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/Raccoon-CI/runner/internal/nodejs"
	"github.com/Raccoon-CI/runner/internal/semver"
)

// nodeInstaller installs Node.js from nodejs.org.
type nodeInstaller struct {
	client  *http.Client
	baseURL string

	mu    sync.Mutex
	index []nodejs.Release
}

// NewNode returns the Node.js installer; baseURL is nodejs.org/dist.
func NewNode(client *http.Client, baseURL string) Installer {
	return &nodeInstaller{client: client, baseURL: baseURL}
}

func (n *nodeInstaller) BinDirs() []string { return []string{"bin"} }

func (n *nodeInstaller) releases(ctx context.Context) ([]nodejs.Release, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.index == nil {
		idx, err := nodejs.Index(ctx, n.client, n.baseURL)
		if err != nil {
			return nil, err
		}
		n.index = idx
	}
	return n.index, nil
}

// Resolve understands what setup-node does: semver ranges, plus latest,
// current, node, and the LTS aliases lts/* (newest LTS line), lts/<codename>
// and lts/-N (the Nth LTS line before the newest).
func (n *nodeInstaller) Resolve(ctx context.Context, spec string) (string, error) {
	releases, err := n.releases(ctx)
	if err != nil {
		return "", err
	}
	spec = strings.TrimSpace(spec)
	lower := strings.ToLower(spec)
	pickBy := func(keep func(nodejs.Release) bool) (string, error) {
		for _, r := range releases { // newest first
			if keep(r) {
				return strings.TrimPrefix(r.Version, "v"), nil
			}
		}
		return "", fmt.Errorf("no Node.js release matches %q", spec)
	}
	switch {
	case lower == "latest" || lower == "current" || lower == "node":
		return pickBy(func(nodejs.Release) bool { return true })
	case lower == "lts/*" || lower == "lts":
		return pickBy(func(r nodejs.Release) bool { return r.LTS != "" })
	case strings.HasPrefix(lower, "lts/"):
		arg := lower[len("lts/"):]
		if back, err := strconv.Atoi(arg); err == nil && back < 0 {
			// Codenames in order of appearance, newest line first.
			var lines []string
			for _, r := range releases {
				if r.LTS != "" && (len(lines) == 0 || lines[len(lines)-1] != r.LTS) {
					lines = append(lines, r.LTS)
				}
			}
			if -back >= len(lines) {
				return "", fmt.Errorf("there are only %d LTS lines, not %d", len(lines), -back+1)
			}
			arg = strings.ToLower(lines[-back])
		}
		return pickBy(func(r nodejs.Release) bool { return strings.ToLower(r.LTS) == arg })
	}
	versions := make([]string, len(releases))
	for i, r := range releases {
		versions[i] = r.Version
	}
	v, err := semver.Pick(spec, versions)
	return strings.TrimPrefix(v, "v"), err
}

func (n *nodeInstaller) Install(ctx context.Context, version, dest string) error {
	return nodejs.Fetch(ctx, n.client, n.baseURL, "v"+version, dest)
}
