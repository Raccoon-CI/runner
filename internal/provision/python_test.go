package provision

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Raccoon-CI/raccoonci/internal/scan"
)

// pythonArchive builds a python-build-standalone install_only archive.
func pythonArchive(t *testing.T, version string) []byte {
	t.Helper()
	minor := strings.Join(strings.Split(version, ".")[:2], ".")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "#!/bin/sh\necho Python " + version + "\n"
	for _, h := range []*tar.Header{
		{Name: "python/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "python/bin/python" + minor, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))},
		{Name: "python/bin/python3", Typeflag: tar.TypeSymlink, Linkname: "python" + minor},
		{Name: "python/bin/python", Typeflag: tar.TypeSymlink, Linkname: "python" + minor},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

type fakePython struct {
	srv      *httptest.Server
	requests atomic.Int32
	corrupt  atomic.Bool
}

func newFakePython(t *testing.T) *fakePython {
	f := &fakePython{}
	builds := map[string][]byte{}
	var sums strings.Builder
	triple := pythonTriple()
	for _, v := range []string{"3.11.18", "3.12.16", "3.13.18"} {
		file := fmt.Sprintf("cpython-%s+20261026-%s-install_only.tar.gz", v, triple)
		builds[file] = pythonArchive(t, v)
		fmt.Fprintf(&sums, "%s  %s\n", sum(builds[file]), file)
	}
	// Things the installer must not pick: other platforms, other flavours, pre-releases.
	for _, file := range []string{
		"cpython-3.14.10+20261026-aarch64-apple-darwin-install_only.tar.gz",
		"cpython-3.14.10+20261026-" + triple + "-install_only_stripped.tar.gz",
		"cpython-3.14.10+20261026-" + triple + "-debug-full.tar.zst",
		"cpython-3.16.0a1+20261026-" + triple + "-install_only.tar.gz",
	} {
		fmt.Fprintf(&sums, "%064d  %s\n", 7, file)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		switch {
		case r.URL.Path == "/latest.json":
			w.Write([]byte(`{"version":1,"tag":"20261026"}`))
		case r.URL.Path == "/rel/20261026/SHA256SUMS":
			w.Write([]byte(sums.String()))
		case strings.HasPrefix(r.URL.Path, "/rel/20261026/"):
			data, ok := builds[strings.TrimPrefix(r.URL.Path, "/rel/20261026/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if f.corrupt.Load() {
				data = pythonArchive(t, "9.9.9")
			}
			w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePython) installer() Installer {
	return NewPython(f.srv.Client(), f.srv.URL+"/latest.json", f.srv.URL+"/rel")
}

func TestPythonResolve(t *testing.T) {
	inst := newFakePython(t).installer()
	ctx := context.Background()
	for spec, want := range map[string]string{
		"3.12": "3.12.16", "3.12.16": "3.12.16", "3.x": "3.13.18", "3": "3.13.18", "~3.11": "3.11.18",
		">=3.11 <3.13": "3.12.16", "^3.11": "3.13.18", "3.13": "3.13.18",
	} {
		if got, err := inst.Resolve(ctx, spec); err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", spec, got, err, want)
		}
	}
	// An exact patch that isn't in the release says which one is.
	if _, err := inst.Resolve(ctx, "3.12.3"); err == nil || !strings.Contains(err.Error(), "latest release has 3.12.16") {
		t.Errorf("3.12.3: %v", err)
	}
	for _, spec := range []string{"3.9", "3.14", "3.16", "3.16.0a1", "2.7"} {
		if got, err := inst.Resolve(ctx, spec); err == nil {
			t.Errorf("Resolve(%q) = %q; want an error (not in the release, or a pre-release)", spec, got)
		}
	}
	if _, err := inst.Resolve(ctx, "3.9"); err == nil || !strings.Contains(err.Error(), "3.11.18, 3.12.16, 3.13.18") {
		t.Errorf("the error should list what is available: %v", err)
	}
}

func TestPythonLandsWhereSetupPythonLooks(t *testing.T) {
	f := newFakePython(t)
	p := &Provisioner{ToolCache: t.TempDir(), Installers: map[string]Installer{"python": f.installer()}}
	ctx := context.Background()

	// Python's directory in the tool cache is Python, capitalised, as setup-python expects.
	res := p.Ensure(ctx, []scan.Demand{toolchain("python", "3.12", false)})
	if it := res.Items[0]; it.Status != Installed || it.Detail != "python 3.12.16" {
		t.Fatalf("%+v", it)
	}
	root := filepath.Join(p.ToolCache, "Python", "3.12.16", Arch())
	for _, name := range []string{"bin/python3.12", "bin/python", "bin/python3"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(root + ".complete"); err != nil {
		t.Errorf("no .complete marker: %v", err)
	}
	if len(res.PathDirs) != 0 {
		t.Errorf("an explicit toolchain is put on PATH by its setup action, not by us: %v", res.PathDirs)
	}
}

func TestImplicitPythonAcceptsTheSameMinor(t *testing.T) {
	f := newFakePython(t)
	p := &Provisioner{ToolCache: t.TempDir(), Installers: map[string]Installer{"python": f.installer()}}
	ctx := context.Background()

	// The job just calls python3; the image has 3.12.3, which isn't published
	// any more. The newest 3.12 does the same job.
	res := p.Ensure(ctx, []scan.Demand{toolchain("python", "3.12.3", true)})
	it := res.Items[0]
	if it.Status != Installed || it.Detail != "python 3.12.16" || it.Err != nil {
		t.Fatalf("implicit: %+v", it)
	}
	bin := filepath.Join(p.ToolCache, "Python", "3.12.16", Arch(), "bin")
	if len(res.PathDirs) != 1 || res.PathDirs[0] != bin {
		t.Errorf("an implicit toolchain goes on PATH: %v", res.PathDirs)
	}

	// Next time it is already there, and nothing is asked of the network.
	before := f.requests.Load()
	if it := p.Ensure(ctx, []scan.Demand{toolchain("python", "3.12.3", true)}).Items[0]; it.Status != Cached {
		t.Errorf("second run: %+v", it)
	}
	if f.requests.Load() != before {
		t.Errorf("%d requests for a cached python", f.requests.Load()-before)
	}

	// A version the workflow named is never swapped for another: setup-python
	// would not find 3.12.3 under a 3.12.16 directory anyway. That holds
	// whether the demand is explicit, or implicit but also named later.
	explicit := toolchain("python", "3.12.3", false)
	declared := toolchain("python", "3.12.3", true)
	declared.Declared = true
	for name, d := range map[string]scan.Demand{"explicit": explicit, "declared": declared} {
		p2 := &Provisioner{ToolCache: t.TempDir(), Installers: map[string]Installer{"python": f.installer()}}
		it := p2.Ensure(ctx, []scan.Demand{d}).Items[0]
		if it.Status != Failed || !strings.Contains(it.Err.Error(), "latest release has 3.12.16") {
			t.Errorf("%s pin: %+v", name, it)
		}
	}
}

func TestPythonChecksumMismatch(t *testing.T) {
	f := newFakePython(t)
	f.corrupt.Store(true)
	p := &Provisioner{ToolCache: t.TempDir(), Installers: map[string]Installer{"python": f.installer()}}
	it := p.Ensure(context.Background(), []scan.Demand{toolchain("python", "3.13", false)}).Items[0]
	if it.Status != Failed || !strings.Contains(it.Err.Error(), "SHA-256") {
		t.Errorf("%+v", it)
	}
	if got := p.cachedVersions("Python"); len(got) != 0 {
		t.Errorf("cached after a failure: %v", got)
	}
}
