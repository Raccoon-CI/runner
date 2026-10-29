package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"raccoonci/internal/scan"
)

// fakeMachine lays out a toolchain the way a template ships it: the real
// install in one directory, reached through a symlink on PATH.
func fakeMachine(t *testing.T, root, command string) (onPath string) {
	t.Helper()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(bin, command)
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	onPath = filepath.Join(t.TempDir(), command)
	if err := os.Symlink(real, onPath); err != nil {
		t.Fatal(err)
	}
	return onPath
}

func machineWith(t *testing.T, command, output string, fresh *int) *Provisioner {
	root := filepath.Join(t.TempDir(), "opt", command)
	onPath := fakeMachine(t, root, command)
	return &Provisioner{
		ToolCache: t.TempDir(),
		LookPath: func(c string) (string, error) {
			if c == command {
				return onPath, nil
			}
			return "", errors.New("not installed")
		},
		Probe: func(_ context.Context, cmd string, args []string) (string, error) {
			if filepath.Base(cmd) != command {
				t.Errorf("probe ran %s", cmd)
			}
			return output, nil
		},
		// No installer: a download would be a failure here.
		Installers: map[string]Installer{},
	}
}

func TestSystemToolchainIsAdopted(t *testing.T) {
	p := machineWith(t, "go", "go version go1.27.1 linux/amd64\n", nil)
	ctx := context.Background()

	// The installer is missing on purpose: the answer must not need it.
	p.Installers["go"] = failingInstaller{}
	res := p.Ensure(ctx, []scan.Demand{toolchain("go", "1.27", false), toolchain("go", "1.27.x", true)})
	adopted := 0
	for _, it := range res.Items {
		if it.Status != Cached || !strings.Contains(it.Detail, "go 1.27.1") {
			t.Fatalf("%+v", it)
		}
		if strings.Contains(it.Detail, "already on this machine") {
			adopted++ // the other one finds the entry this one just made
		}
	}
	if adopted == 0 {
		t.Error("nobody adopted the machine's Go")
	}

	// It is in the tool cache in toolkit's layout: <tool>/<version>/<arch>[.complete],
	// with the commands where setup-go looks.
	dir := filepath.Join(p.ToolCache, "go", "1.27.1")
	if _, err := os.Stat(filepath.Join(dir, Arch()+".complete")); err != nil {
		t.Errorf("no .complete marker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, Arch(), "bin", "go")); err != nil {
		t.Errorf("the adopted install has no bin/go: %v", err)
	}
	if !slices.Equal(res.PathDirs, []string{filepath.Join(dir, Arch(), "bin")}) {
		t.Errorf("an implicit toolchain must be on PATH: %v", res.PathDirs)
	}
}

func TestSystemToolchainThatDoesNotFitIsNotUsed(t *testing.T) {
	p := machineWith(t, "go", "go version go1.27.1 linux/amd64\n", nil)
	p.Installers["go"] = failingInstaller{}
	// A newer patch than the machine has: the download is needed (and here it fails).
	it := p.Ensure(context.Background(), []scan.Demand{toolchain("go", "1.27.3", false)}).Items[0]
	if it.Status != Failed || !strings.Contains(it.Err.Error(), "no network in this test") {
		t.Fatalf("%+v", it)
	}
	if _, err := os.Stat(filepath.Join(p.ToolCache, "go")); err == nil {
		t.Error("a copy that does not satisfy the spec was put in the tool cache")
	}
}

func TestSystemProbeOutputThatIsNotAVersionIsIgnored(t *testing.T) {
	for _, out := range []string{"", "command not found", "go version devel +abc Mon Oct 1\n"} {
		p := machineWith(t, "go", out, nil)
		p.Installers["go"] = failingInstaller{}
		it := p.Ensure(context.Background(), []scan.Demand{toolchain("go", "1.27", false)}).Items[0]
		if it.Status != Failed {
			t.Errorf("%q: %+v", out, it)
		}
	}
}

func TestAdoptedCopyThatVanishedIsNotCached(t *testing.T) {
	p := machineWith(t, "node", "v22.23.3\n", nil)
	p.Installers["node"] = failingInstaller{}
	if it := p.Ensure(context.Background(), []scan.Demand{toolchain("node", "22", false)}).Items[0]; it.Status != Cached {
		t.Fatalf("%+v", it)
	}
	link := filepath.Join(p.ToolCache, "node", "22.23.3", Arch())
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	// The machine lost its Node.js (a different template): the stale entry must
	// not be handed out, or the setup action would find a dead link.
	p.LookPath = notInstalled
	it := p.Ensure(context.Background(), []scan.Demand{toolchain("node", "22", false)}).Items[0]
	if it.Status != Failed {
		t.Errorf("a dangling adopted copy was reported as %+v", it)
	}
}

type failingInstaller struct{}

func (failingInstaller) Resolve(context.Context, string) (string, error) {
	return "", errors.New("no network in this test")
}
func (failingInstaller) Install(context.Context, string, string) error {
	return errors.New("no network in this test")
}
func (failingInstaller) BinDirs() []string { return []string{"bin"} }
