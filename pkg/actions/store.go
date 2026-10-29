package actions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"raccoonci/internal/archive"
)

// Ref is a `uses: owner/repo[/path]@ref` reference to an action on GitHub.
type Ref struct {
	Owner, Repo string
	Path        string // subdirectory holding action.yml, "" for the root
	Version     string // tag, branch or commit SHA
}

func (r Ref) String() string {
	s := r.Owner + "/" + r.Repo
	if r.Path != "" {
		s += "/" + r.Path
	}
	return s + "@" + r.Version
}

// Repository is owner/repo.
func (r Ref) Repository() string { return r.Owner + "/" + r.Repo }

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ParseRef parses a remote action reference. Local (./) and docker://
// references are not remote; callers handle them before calling ParseRef.
func ParseRef(uses string) (Ref, error) {
	at := strings.LastIndex(uses, "@")
	if at < 0 {
		return Ref{}, fmt.Errorf("%q: expected owner/repo@ref, ./path or docker://image", uses)
	}
	r := Ref{Version: uses[at+1:]}
	parts := strings.Split(uses[:at], "/")
	if len(parts) < 2 || r.Version == "" {
		return Ref{}, fmt.Errorf("%q: expected owner/repo@ref, ./path or docker://image", uses)
	}
	for _, p := range parts {
		if !namePattern.MatchString(p) || p == "." || p == ".." {
			return Ref{}, fmt.Errorf("%q: invalid path element %q", uses, p)
		}
	}
	if strings.Contains(r.Version, "..") || strings.ContainsAny(r.Version, " \\") {
		return Ref{}, fmt.Errorf("%q: invalid ref %q", uses, r.Version)
	}
	r.Owner, r.Repo, r.Path = parts[0], parts[1], strings.Join(parts[2:], "/")
	return r, nil
}

// maxActionSize caps the unpacked size of one action repository, so a
// hostile tarball can't fill the disk.
const maxActionSize = 1 << 30

// Store downloads action repositories, once per Store. Tags like v4 move, so
// a Store should live for one workflow run, like the GitHub runner's
// per-job download, rather than caching across runs.
type Store struct {
	Dir    string
	Client *http.Client
	// Token, if set, authenticates downloads through the API (needed for
	// private actions). Without it, public tarballs come from codeload,
	// which doesn't count against the API's 60 requests/hour.
	Token        string
	APIBase      string
	CodeloadBase string

	mu      sync.Mutex
	fetches map[string]*fetch
}

type fetch struct {
	done chan struct{}
	dir  string
	err  error
}

func NewStore(dir, token string) *Store {
	return &Store{
		Dir:          dir,
		Client:       http.DefaultClient,
		Token:        token,
		APIBase:      "https://api.github.com",
		CodeloadBase: "https://codeload.github.com",
	}
}

// Lookup returns a function that reads the metadata of the action a `uses:`
// reference (owner/repo[/path]@ref) names, downloading it if needed. It is
// what the scan uses to look inside actions; false means it could not be
// had (bad reference, network, no action.yml).
func (s *Store) Lookup(ctx context.Context) func(uses string) (*Action, bool) {
	return func(uses string) (*Action, bool) {
		ref, err := ParseRef(uses)
		if err != nil {
			return nil, false
		}
		dir, err := s.Fetch(ctx, ref)
		if err != nil {
			return nil, false
		}
		a, err := Load(filepath.Join(dir, filepath.FromSlash(ref.Path)))
		return a, err == nil
	}
}

// Fetch downloads the repository r points into and returns its directory.
// Concurrent calls for the same repository and version share one download.
func (s *Store) Fetch(ctx context.Context, r Ref) (string, error) {
	key := r.Owner + "/" + r.Repo + "@" + r.Version
	s.mu.Lock()
	if s.fetches == nil {
		s.fetches = map[string]*fetch{}
	}
	f, ok := s.fetches[key]
	if !ok {
		f = &fetch{done: make(chan struct{})}
		s.fetches[key] = f
		go func() {
			// Detached from ctx: other callers may be waiting for it.
			f.dir, f.err = s.download(context.WithoutCancel(ctx), r)
			close(f.done)
		}()
	}
	s.mu.Unlock()
	select {
	case <-f.done:
		return f.dir, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *Store) download(ctx context.Context, r Ref) (string, error) {
	dest := filepath.Join(s.Dir, r.Owner, r.Repo, strings.ReplaceAll(r.Version, "/", "_"))
	var u string
	if s.Token != "" {
		u = fmt.Sprintf("%s/repos/%s/%s/tarball/%s", s.APIBase, r.Owner, r.Repo, escapeRef(r.Version))
	} else {
		u = fmt.Sprintf("%s/%s/%s/tar.gz/%s", s.CodeloadBase, r.Owner, r.Repo, escapeRef(r.Version))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	if s.Token != "" {
		// Go drops this header when the API redirects to codeload.
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", r.Repository()+"@"+r.Version, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("download %s@%s: %s", r.Repository(), r.Version, resp.Status)
		if resp.StatusCode == http.StatusNotFound {
			msg += " (no such repository or ref; private actions need a token)"
		}
		return "", errors.New(msg)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".download-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := archive.ExtractTarGz(resp.Body, tmp, maxActionSize); err != nil {
		return "", fmt.Errorf("unpack %s@%s: %w", r.Repository(), r.Version, err)
	}
	if err := os.RemoveAll(dest); err != nil { // left by an earlier Store
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
