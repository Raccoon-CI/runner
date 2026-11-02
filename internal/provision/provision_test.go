package provision

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Raccoon-CI/raccoonci/internal/catalog"
	"github.com/Raccoon-CI/raccoonci/internal/nodejs"
	"github.com/Raccoon-CI/raccoonci/internal/scan"
)

// tarGz builds a release archive whose files sit under one top-level directory.
func tarGz(t *testing.T, top string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		h := &tar.Header{Name: top + "/" + name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fakeNode serves nodejs.org's index, checksums and downloads for one build
// per entry of builds (version with v -> LTS codename or "").
type fakeNode struct {
	srv       *httptest.Server
	index     string
	downloads atomic.Int32
	requests  atomic.Int32
	corrupt   atomic.Bool
}

func newFakeNode(t *testing.T) *fakeNode {
	f := &fakeNode{index: `[
		{"version":"v22.5.0","lts":"Jod"},
		{"version":"v21.0.0","lts":false},
		{"version":"v20.20.2","lts":"Iron"},
		{"version":"v20.1.0","lts":false},
		{"version":"v18.20.0","lts":"Hydrogen"}]`}
	builds := map[string][]byte{}
	for _, v := range []string{"v22.5.0", "v20.20.2", "v18.20.0"} {
		top := fmt.Sprintf("node-%s-linux-%s", v, nodejs.Arch())
		builds[v] = tarGz(t, top, map[string]string{"bin/node": "#!/bin/sh\necho " + v + "\n"})
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.URL.Path == "/index.json" {
			w.Write([]byte(f.index))
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		data, ok := builds[parts[0]]
		if len(parts) != 2 || !ok {
			http.NotFound(w, r)
			return
		}
		file := fmt.Sprintf("node-%s-linux-%s.tar.gz", parts[0], nodejs.Arch())
		switch parts[1] {
		case "SHASUMS256.txt":
			fmt.Fprintf(w, "%s  %s\n", sum(data), file)
		case file:
			f.downloads.Add(1)
			if f.corrupt.Load() {
				w.Write(tarGz(t, "x", map[string]string{"bin/node": "evil"}))
				return
			}
			w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNode) provisioner(t *testing.T) *Provisioner {
	return &Provisioner{
		ToolCache:  t.TempDir(),
		Installers: map[string]Installer{"node": NewNode(f.srv.Client(), f.srv.URL)},
		LookPath:   notInstalled,
	}
}

// notInstalled is a LookPath for a machine with nothing on it, so a test does
// not depend on the Node.js or Go of the machine running it.
func notInstalled(string) (string, error) { return "", errors.New("not installed") }

func toolchain(name, version string, implicit bool) scan.Demand {
	return scan.Demand{Kind: catalog.Toolchain, Name: name, Version: version, Implicit: implicit}
}

func TestNodeInstallsIntoToolcacheOnce(t *testing.T) {
	f := newFakeNode(t)
	p := f.provisioner(t)
	ctx := context.Background()

	res := p.Ensure(ctx, []scan.Demand{toolchain("node", "20", true)})
	it := res.Items[0]
	if it.Status != Installed || it.Err != nil || it.Detail != "node 20.20.2" {
		t.Fatalf("first ensure: %+v", it)
	}
	bin := filepath.Join(p.ToolCache, "node", "20.20.2", Arch(), "bin")
	if _, err := os.Stat(filepath.Join(bin, "node")); err != nil {
		t.Errorf("node not installed: %v", err)
	}
	// The marker is what @actions/tool-cache's find() looks for.
	if _, err := os.Stat(filepath.Join(p.ToolCache, "node", "20.20.2", Arch()+".complete")); err != nil {
		t.Errorf("no .complete marker: %v", err)
	}
	if !slices.Equal(res.PathDirs, []string{bin}) {
		t.Errorf("an implicit toolchain must be on PATH: %v", res.PathDirs)
	}

	// Cached specs cost no network, whatever way they are written.
	before := f.requests.Load()
	for _, spec := range []string{"20", "20.x", "20.20.2", "^20", ">=20 <21", "~20.20"} {
		res = p.Ensure(ctx, []scan.Demand{toolchain("node", spec, false)})
		if it := res.Items[0]; it.Status != Cached || it.Detail != "node 20.20.2" {
			t.Errorf("%s: %+v", spec, it)
		}
		if len(res.PathDirs) != 0 {
			t.Errorf("%s: an explicit toolchain (set up by its action) must not be forced onto PATH", spec)
		}
	}
	if f.requests.Load() != before {
		t.Errorf("cached specs made %d requests", f.requests.Load()-before)
	}
}

func TestNodeSpecs(t *testing.T) {
	f := newFakeNode(t)
	inst := NewNode(f.srv.Client(), f.srv.URL)
	for spec, want := range map[string]string{
		"latest": "22.5.0", "current": "22.5.0", "node": "22.5.0", "*": "22.5.0", ">=21": "22.5.0",
		"lts/*": "22.5.0", "lts": "22.5.0", "lts/iron": "20.20.2", "LTS/Iron": "20.20.2",
		"lts/-1": "20.20.2", "lts/-2": "18.20.0",
		"18": "18.20.0", "^18.1": "18.20.0", "20.1.0": "20.1.0", "v20.1.0": "20.1.0", "20": "20.20.2",
	} {
		if got, err := inst.Resolve(context.Background(), spec); err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", spec, got, err, want)
		}
	}
	for _, spec := range []string{"lts/-3", "lts/-0", "lts/nobody", "99", "nonsense", "20.9.9"} {
		if got, err := inst.Resolve(context.Background(), spec); err == nil {
			t.Errorf("Resolve(%q) = %q; want an error", spec, got)
		}
	}
}

func TestBadDownloadLeavesNothingBehind(t *testing.T) {
	f := newFakeNode(t)
	f.corrupt.Store(true)
	p := f.provisioner(t)
	res := p.Ensure(context.Background(), []scan.Demand{toolchain("node", "18", false)})
	it := res.Items[0]
	if it.Status != Failed || it.Err == nil || !strings.Contains(it.Err.Error(), "SHA-256") {
		t.Fatalf("a corrupt download must fail: %+v", it)
	}
	if got := p.cachedVersions("node"); len(got) != 0 {
		t.Errorf("cached after a failure: %v", got)
	}
	entries, _ := os.ReadDir(filepath.Join(p.ToolCache, "node", "18.20.0"))
	if len(entries) != 0 {
		t.Errorf("debris left behind: %v", entries)
	}
}

func TestOfflineUsesWhatIsCached(t *testing.T) {
	f := newFakeNode(t)
	p := f.provisioner(t)
	ctx := context.Background()
	p.Ensure(ctx, []scan.Demand{toolchain("node", "20.20.2", false)})
	f.srv.Close()

	if it := p.Ensure(ctx, []scan.Demand{toolchain("node", "20", false)}).Items[0]; it.Status != Cached {
		t.Errorf("offline, cached: %+v", it)
	}
	it := p.Ensure(ctx, []scan.Demand{toolchain("node", "18", false)}).Items[0]
	if it.Status != Failed || it.Err == nil {
		t.Errorf("offline, not cached: %+v", it)
	}
}

func TestConcurrentJobsShareOneDownload(t *testing.T) {
	f := newFakeNode(t)
	p := f.provisioner(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var statuses []Status
	for range 8 {
		wg.Go(func() {
			res := p.Ensure(context.Background(), []scan.Demand{toolchain("node", "20", true)})
			mu.Lock()
			statuses = append(statuses, res.Items[0].Status)
			mu.Unlock()
		})
	}
	wg.Wait()
	if f.downloads.Load() != 1 {
		t.Errorf("%d downloads, want 1", f.downloads.Load())
	}
	for _, s := range statuses {
		if s != Installed && s != Cached {
			t.Errorf("status %s", s)
		}
	}
}

func TestInterruptedInstallIsReplaced(t *testing.T) {
	f := newFakeNode(t)
	p := f.provisioner(t)
	stale := filepath.Join(p.ToolCache, "node", "20.20.2", Arch())
	os.MkdirAll(stale, 0o755)
	os.WriteFile(filepath.Join(stale, "half-written"), []byte("x"), 0o644) // no .complete marker

	it := p.Ensure(context.Background(), []scan.Demand{toolchain("node", "20.20.2", false)}).Items[0]
	if it.Status != Installed {
		t.Fatalf("%+v", it)
	}
	if _, err := os.Stat(filepath.Join(stale, "half-written")); err == nil {
		t.Error("the half-written install must be replaced, not kept")
	}
	if _, err := os.Stat(filepath.Join(stale, "bin", "node")); err != nil {
		t.Error(err)
	}
}

func TestGoInstaller(t *testing.T) {
	files := func(version string) []byte {
		return tarGz(t, "go", map[string]string{"bin/go": "#!/bin/sh\necho " + version + "\n"})
	}
	archives := map[string][]byte{}
	var entries []string
	add := func(version string, stable bool) {
		name := fmt.Sprintf("%s.linux-%s.tar.gz", version, goArch())
		archives[name] = files(version)
		entries = append(entries, fmt.Sprintf(`{"version":%q,"stable":%v,"files":[
			{"filename":%q,"os":"linux","arch":%q,"sha256":%q,"kind":"archive"},
			{"filename":"%s.src.tar.gz","os":"","arch":"","sha256":"00","kind":"source"}]}`,
			version, stable, name, goArch(), sum(archives[name]), version))
	}
	add("go1.25rc1", false)
	add("go1.24.13", true)
	add("go1.24.1", true)
	add("go1.24", true) // Go names 1.24.0 this way
	add("go1.23.9", true)
	var tampered atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("mode") == "json" {
			w.Write([]byte("[" + strings.Join(entries, ",") + "]"))
			return
		}
		data, ok := archives[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if tampered.Load() {
			data = tarGz(t, "go", map[string]string{"bin/go": "evil"})
		}
		w.Write(data)
	}))
	defer srv.Close()

	inst := NewGo(srv.Client(), srv.URL)
	for spec, want := range map[string]string{
		"1.24": "1.24.13", "1.24.x": "1.24.13", "go1.24": "1.24.13", "1.24.0": "1.24.0", "1.24.1": "1.24.1",
		"1.23": "1.23.9", "^1.23": "1.24.13", ">=1.23 <1.24": "1.23.9", "stable": "1.24.13", "oldstable": "1.23.9", "1": "1.24.13",
	} {
		if got, err := inst.Resolve(context.Background(), spec); err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", spec, got, err, want)
		}
	}
	for _, spec := range []string{"1.25", "1.22", "1.25rc1"} {
		if got, err := inst.Resolve(context.Background(), spec); err == nil {
			t.Errorf("Resolve(%q) = %q; want an error (no stable release)", spec, got)
		}
	}

	p := &Provisioner{ToolCache: t.TempDir(), Installers: map[string]Installer{"go": inst}, LookPath: notInstalled}
	res := p.Ensure(context.Background(), []scan.Demand{toolchain("go", "1.24", true)})
	if it := res.Items[0]; it.Status != Installed || it.Detail != "go 1.24.13" {
		t.Fatalf("%+v", it)
	}
	bin := filepath.Join(p.ToolCache, "go", "1.24.13", Arch(), "bin")
	if b, err := os.ReadFile(filepath.Join(bin, "go")); err != nil || !strings.Contains(string(b), "1.24.13") {
		t.Errorf("go binary: %q %v", b, err)
	}
	if !slices.Equal(res.PathDirs, []string{bin}) {
		t.Errorf("PathDirs = %v", res.PathDirs)
	}

	tampered.Store(true)
	it := p.Ensure(context.Background(), []scan.Demand{toolchain("go", "1.23", false)}).Items[0]
	if it.Status != Failed || !strings.Contains(it.Err.Error(), "SHA-256") {
		t.Errorf("a download that doesn't match go.dev's checksum must fail: %+v", it)
	}
}

func TestToolchainsWeCannotInstall(t *testing.T) {
	p := &Provisioner{ToolCache: t.TempDir(), LookPath: notInstalled}
	res := p.Ensure(context.Background(), []scan.Demand{
		toolchain("python", "3.12", false),  // its setup action will download it
		toolchain("python", "3.12.3", true), // nothing else will provide this one
		toolchain("node", "", false),        // version only known at run time
		toolchain("nosuch", "1", false),
	})
	want := []Status{Skipped, Failed, Skipped, Skipped}
	for i, it := range res.Items {
		if it.Status != want[i] {
			t.Errorf("item %d (%s): %s, want %s (%+v)", i, it.Demand.Name, it.Status, want[i], it)
		}
	}
	if len(res.Failed()) != 1 || !strings.Contains(res.Failed()[0].Err.Error(), "no installer") {
		t.Errorf("Failed() = %+v", res.Failed())
	}
	if len(res.PathDirs) != 0 {
		t.Errorf("PathDirs = %v", res.PathDirs)
	}
}

func TestToolsAreOnlyChecked(t *testing.T) {
	onPath := map[string]string{"docker": "/usr/bin/docker"}
	p := &Provisioner{
		ToolCache: t.TempDir(),
		LookPath: func(cmd string) (string, error) {
			if path, ok := onPath[cmd]; ok {
				return path, nil
			}
			return "", errors.New("not found")
		},
	}
	res := p.Ensure(context.Background(), []scan.Demand{
		{Kind: catalog.Tool, Name: "docker", Implicit: true},
		{Kind: catalog.Tool, Name: "maven", Implicit: true}, // command: mvn
		{Kind: catalog.Tool, Name: "powershell", Implicit: true},
	})
	if res.Items[0].Status != Cached || res.Items[1].Status != Failed || res.Items[2].Status != Failed {
		t.Errorf("%+v", res.Items)
	}
	onPath["mvn"] = "/usr/bin/mvn"
	if p.Ensure(context.Background(), []scan.Demand{{Kind: catalog.Tool, Name: "maven"}}).Items[0].Status != Cached {
		t.Error("maven is found through its command mvn")
	}
}

// fakeApt records what was run and fails what the test says to.
type fakeApt struct {
	mu        sync.Mutex
	calls     [][]string
	installed map[string]bool
	broken    map[string]bool
}

func (f *fakeApt) apt() *Apt {
	return &Apt{
		Sudo: true,
		Run: func(_ context.Context, argv ...string) ([]byte, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls = append(f.calls, argv)
			for _, a := range argv {
				if f.broken[a] {
					return []byte("E: Unable to locate package " + a + "\n"), errors.New("exit status 100")
				}
			}
			return nil, nil
		},
		IsInstalled: func(pkg string) bool { return f.installed[pkg] },
	}
}

func pkg(name string) scan.Demand {
	return scan.Demand{Kind: catalog.Package, Name: name, Implicit: true}
}

func TestPackagesAreInstalledWithDebianNames(t *testing.T) {
	f := &fakeApt{installed: map[string]bool{"curl": true}}
	p := &Provisioner{
		ToolCache: t.TempDir(),
		Apt:       f.apt(),
		LookPath: func(cmd string) (string, error) {
			if cmd == "jq" {
				return "/usr/bin/jq", nil
			}
			return "", errors.New("not found")
		},
	}
	res := p.Ensure(context.Background(), []scan.Demand{pkg("jq"), pkg("curl"), pkg("tree"), pkg("dnsutils"), pkg("netcat")})
	want := []Status{Cached, Cached, Installed, Installed, Installed}
	for i, it := range res.Items {
		if it.Status != want[i] || it.Err != nil {
			t.Errorf("%s: %+v", it.Demand.Name, it)
		}
	}
	if len(f.calls) != 2 {
		t.Fatalf("want apt-get update and one install, got %v", f.calls)
	}
	update := strings.Join(f.calls[0], " ")
	install := strings.Join(f.calls[1], " ")
	if update != "sudo -n env DEBIAN_FRONTEND=noninteractive apt-get update -qq" {
		t.Errorf("update: %s", update)
	}
	// Ubuntu's names become Debian's; what is there already is left alone.
	if install != "sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends tree bind9-dnsutils netcat-openbsd" {
		t.Errorf("install: %s", install)
	}

	// A second job needing the same thing doesn't update again.
	f.installed["tree"] = true
	p.Ensure(context.Background(), []scan.Demand{pkg("tree"), pkg("ftp")})
	if n := len(f.calls); n != 3 || !strings.HasSuffix(strings.Join(f.calls[2], " "), "install -y --no-install-recommends ftp") {
		t.Errorf("calls: %v", f.calls)
	}
}

func TestOneBadPackageDoesNotBlockTheRest(t *testing.T) {
	f := &fakeApt{broken: map[string]bool{"ftp": true}}
	p := &Provisioner{ToolCache: t.TempDir(), Apt: f.apt(), LookPath: func(string) (string, error) { return "", errors.New("no") }}
	res := p.Ensure(context.Background(), []scan.Demand{pkg("tree"), pkg("ftp"), pkg("telnet")})
	want := []Status{Installed, Failed, Installed}
	for i, it := range res.Items {
		if it.Status != want[i] {
			t.Errorf("%s: %+v", it.Demand.Name, it)
		}
	}
	if err := res.Items[1].Err; err == nil || !strings.Contains(err.Error(), "Unable to locate package ftp") {
		t.Errorf("the apt error should be shown: %v", err)
	}
}

func TestPackagesNotInstalledWhenNotAllowed(t *testing.T) {
	p := &Provisioner{ToolCache: t.TempDir(), LookPath: func(string) (string, error) { return "", errors.New("no") }}
	it := p.Ensure(context.Background(), []scan.Demand{pkg("dnsutils")}).Items[0]
	if it.Status != Failed || !strings.Contains(it.Err.Error(), "bind9-dnsutils") || !strings.Contains(it.Err.Error(), "not enabled") {
		t.Errorf("%+v", it)
	}
}

func TestAptUpdateFailureIsReported(t *testing.T) {
	a := &Apt{
		Run: func(_ context.Context, argv ...string) ([]byte, error) {
			return []byte("E: Could not resolve host\n"), errors.New("exit status 100")
		},
		IsInstalled: func(string) bool { return false }, // not whatever this machine happens to have
	}
	p := &Provisioner{ToolCache: t.TempDir(), Apt: a, LookPath: func(string) (string, error) { return "", errors.New("no") }}
	it := p.Ensure(context.Background(), []scan.Demand{pkg("tree")}).Items[0]
	if it.Status != Failed || !strings.Contains(it.Err.Error(), "Could not resolve host") {
		t.Errorf("%+v", it)
	}
}

func TestDescribe(t *testing.T) {
	cases := map[string]Item{
		"installed node 20.20.2":                         {Demand: toolchain("node", "20", false), Status: Installed, Detail: "node 20.20.2"},
		"go 1.24.13 is already available":                {Demand: toolchain("go", "1.24", true), Status: Cached, Detail: "go 1.24.13"},
		"installed dnsutils (bind9-dnsutils)":            {Demand: pkg("dnsutils"), Status: Installed, Detail: "bind9-dnsutils"},
		"installed tree":                                 {Demand: pkg("tree"), Status: Installed, Detail: "tree"},
		"jq is already available":                        {Demand: pkg("jq"), Status: Cached, Detail: "/usr/bin/jq"},
		"python 3.12: its setup action installs it":      {Demand: toolchain("python", "3.12", false), Status: Skipped, Detail: "its setup action installs it"},
		"tree: package tree is needed but not installed": {Demand: pkg("tree"), Status: Failed, Err: errors.New("package tree is needed but not installed")},
	}
	for want, it := range cases {
		if got := it.Describe(); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
