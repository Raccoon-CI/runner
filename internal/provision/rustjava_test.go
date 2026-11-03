package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Raccoon-CI/runner/pkg/scan"
)

// A rust-installer component: install.sh copies the component's files under --prefix.
func rustComponent(t *testing.T, name, version string) []byte {
	top := fmt.Sprintf("%s-%s-%s", name, version, rustTriple())
	script := "#!/bin/sh\nset -e\nfor a in \"$@\"; do case $a in --prefix=*) P=${a#--prefix=};; esac; done\n"
	files := map[string]string{}
	switch name {
	case "rust-std":
		script += "mkdir -p \"$P/lib/rustlib\" && echo std > \"$P/lib/rustlib/std\"\n"
	default:
		script += "mkdir -p \"$P/bin\" && printf '#!/bin/sh\\necho " + name + " " + version + "\\n' > \"$P/bin/" + strings.TrimSuffix(name, "-preview") + "\" && chmod +x \"$P/bin/" + name + "\"\n"
	}
	files["install.sh"] = script
	return tarGz(t, top, files)
}

func fakeRust(t *testing.T, versions ...string) (Installer, *int) {
	files := map[string][]byte{}
	for _, v := range versions {
		for _, c := range rustComponents {
			file := fmt.Sprintf("%s-%s-%s.tar.gz", c, v, rustTriple())
			files[file] = rustComponent(t, c, v)
			files[file+".sha256"] = []byte(sum(files[file]) + "  " + file + "\n")
		}
	}
	files["channel-rust-stable.toml"] = []byte("date = \"2026-10-13\"\n[pkg.cargo]\nversion = \"1.99.1 (x 2026)\"\n\n[pkg.rust]\nversion = \"1.99.1 (58cf8f9c3 2026-10-13)\"\n")
	downloads := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[strings.TrimPrefix(r.URL.Path, "/dist/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".tar.gz") {
			*downloads++
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return NewRust(srv.Client(), srv.URL+"/dist"), downloads
}

func TestRustResolve(t *testing.T) {
	inst, _ := fakeRust(t, "1.80.0")
	ctx := context.Background()
	for spec, want := range map[string]string{"stable": "1.99.1", "1.80": "1.80.0", "1.80.1": "1.80.1", "": "1.99.1"} {
		if got, err := inst.Resolve(ctx, spec); err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", spec, got, err, want)
		}
	}
	for _, spec := range []string{"nightly", "beta", ">=1.70", "1"} {
		if _, err := inst.Resolve(ctx, spec); err == nil {
			t.Errorf("Resolve(%q) should be refused", spec)
		}
	}
}

func TestImplicitRustIsInstalledFromItsComponents(t *testing.T) {
	inst, downloads := fakeRust(t, "1.80.0")
	p := &Provisioner{ToolCache: t.TempDir(), Installers: map[string]Installer{"rust": inst}}
	res := p.Ensure(context.Background(), []scan.Demand{toolchain("rust", "1.80.0", true)})
	if it := res.Items[0]; it.Status != Installed || it.Detail != "rust 1.80.0" {
		t.Fatalf("%+v", it)
	}
	root := filepath.Join(p.ToolCache, "rust", "1.80.0", Arch())
	for _, f := range []string{"bin/rustc", "bin/cargo", "bin/rustfmt", "lib/rustlib/std"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if _, err := os.Stat(root + ".complete"); err != nil {
		t.Error("no .complete marker")
	}
	if left, _ := filepath.Glob(filepath.Join(p.ToolCache, "rust", "1.80.0", "*src*")); len(left) != 0 {
		t.Errorf("unpacked component directories are left: %v", left)
	}
	if want := []string{filepath.Join(root, "bin")}; len(res.PathDirs) != 1 || res.PathDirs[0] != want[0] {
		t.Errorf("PathDirs = %v, want %v", res.PathDirs, want)
	}
	if *downloads != len(rustComponents) {
		t.Errorf("%d downloads", *downloads)
	}
}

// rustup and setup-java install by themselves; a tool cache entry for them
// would be a download nobody reads.
func TestSetupActionsOfRustAndJavaAreLeftAlone(t *testing.T) {
	rust, rustDownloads := fakeRust(t, "1.80.0")
	p := &Provisioner{ToolCache: t.TempDir(), Installers: map[string]Installer{"rust": rust}}
	res := p.Ensure(context.Background(), []scan.Demand{toolchain("rust", "1.80.0", false), toolchain("java", "17", false)})
	for _, it := range res.Items {
		if it.Status != Skipped || !strings.Contains(it.Detail, "itself") {
			t.Errorf("%s: %+v", it.Demand.Name, it)
		}
	}
	if *rustDownloads != 0 {
		t.Error("something was downloaded")
	}
}

func TestRustChecksumMismatch(t *testing.T) {
	inst, _ := fakeRust(t, "1.80.0")
	r := inst.(*rustInstaller)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, ".sha256") {
			file := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/"), ".sha256")
			fmt.Fprintf(w, "%064d  %s\n", 0, file)
			return
		}
		w.Write(rustComponent(t, "rustc", "1.80.0"))
	}))
	defer srv.Close()
	r.client, r.baseURL = srv.Client(), srv.URL
	if err := r.Install(context.Background(), "1.80.0", filepath.Join(t.TempDir(), "x")); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("err = %v", err)
	}
}

func fakeAdoptium(t *testing.T) Installer {
	jdk := tarGz(t, "jdk-17.0.9+9", map[string]string{"bin/java": "#!/bin/sh\necho java 17.0.9\n"})
	release := func(name, semver string) map[string]any {
		return map[string]any{
			"release_name": name,
			"version_data": map[string]string{"semver": semver},
			"binaries":     []map[string]any{{"package": map[string]string{"checksum": sum(jdk)}}},
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v3/assets/feature_releases/17/ga":
			// Newest first, with a second build of 17.0.8.
			json.NewEncoder(w).Encode([]any{
				release("jdk-17.0.9+9", "17.0.9+9"),
				release("jdk-17.0.8.1+1", "17.0.8+101"),
				release("jdk-17.0.8+7", "17.0.8+7"),
			})
		case r.URL.Path == "/v3/assets/feature_releases/99/ga":
			http.NotFound(w, r)
		case r.URL.EscapedPath() == "/v3/binary/version/jdk-17.0.9%2B9/linux/"+javaArch()+"/jdk/hotspot/normal/eclipse":
			w.Write(jdk)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return NewJava(srv.Client(), srv.URL)
}

func TestJavaResolve(t *testing.T) {
	inst := fakeAdoptium(t)
	ctx := context.Background()
	for spec, want := range map[string]string{"17": "17.0.9", "17.0": "17.0.9", "17.0.8": "17.0.8", "17.0.20+1": "", "v17": "17.0.9"} {
		got, err := inst.Resolve(ctx, spec)
		if want == "" {
			if err == nil || !strings.Contains(err.Error(), "17.0.9") {
				t.Errorf("Resolve(%q) = %q, %v; want an error naming what exists", spec, got, err)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", spec, got, err, want)
		}
	}
	if _, err := inst.Resolve(ctx, "99"); err == nil || !strings.Contains(err.Error(), "no Temurin 99") {
		t.Errorf("an unknown major: %v", err)
	}
	if _, err := inst.Resolve(ctx, "latest"); err == nil {
		t.Error("latest is not supported")
	}
}

func TestJavaSpec(t *testing.T) {
	for spec, want := range map[string]struct {
		major int
		rng   string
	}{"1.8": {8, "8"}, "1.8.0": {8, "8"}, "17": {17, "17"}, "17.0.20+1": {17, "17.0.20"}, "21.0.3": {21, "21.0.3"}} {
		if m, r, err := javaSpec(spec); err != nil || m != want.major || r != want.rng {
			t.Errorf("javaSpec(%q) = %d, %q, %v", spec, m, r, err)
		}
	}
}

func TestImplicitJavaIsInstalledAndChecked(t *testing.T) {
	p := &Provisioner{ToolCache: t.TempDir(), Installers: map[string]Installer{"java": fakeAdoptium(t)}}
	res := p.Ensure(context.Background(), []scan.Demand{toolchain("java", "17.0.9+9", true)})
	if it := res.Items[0]; it.Status != Installed || it.Detail != "java 17.0.9" {
		t.Fatalf("%+v", it)
	}
	if _, err := os.Stat(filepath.Join(p.ToolCache, "java", "17.0.9", Arch(), "bin", "java")); err != nil {
		t.Error(err)
	}
}
