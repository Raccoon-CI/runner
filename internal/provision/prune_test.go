package provision

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"raccoonci/internal/scan"
)

// cached makes a complete tool cache entry of size bytes, last used at used.
func cached(t *testing.T, root, tool, version string, size int, used time.Time) string {
	t.Helper()
	dir := filepath.Join(root, tool, version, Arch())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob"), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := dir + ".complete"
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(marker, used, used); err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(dir)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestPruneRemovesTheLeastRecentlyUsedFirst(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	old := cached(t, root, "go", "1.20.0", 400, now.Add(-72*time.Hour))
	mid := cached(t, root, "go", "1.21.0", 400, now.Add(-48*time.Hour))
	recent := cached(t, root, "node", "20.1.0", 400, now.Add(-2*time.Hour))

	removed, err := Prune(root, 900, 24*time.Hour, now) // 1200 bytes held; one must go
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"go/1.20.0"}) {
		t.Fatalf("removed %v", removed)
	}
	if exists(old) || !exists(mid) || !exists(recent) {
		t.Errorf("old=%v mid=%v recent=%v", exists(old), exists(mid), exists(recent))
	}
}

func TestPruneNeverTouchesWhatWasUsedRecently(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	a := cached(t, root, "go", "1.22.0", 1000, now.Add(-time.Hour))
	b := cached(t, root, "node", "22.0.0", 1000, now.Add(-2*time.Hour))
	removed, err := Prune(root, 10, 24*time.Hour, now) // far over the limit, but all of it is recent
	if err != nil || len(removed) != 0 {
		t.Fatalf("removed %v, %v", removed, err)
	}
	if !exists(a) || !exists(b) {
		t.Error("a recently used version was removed")
	}
}

func TestPruneWithoutALimitKeepsEverything(t *testing.T) {
	root := t.TempDir()
	d := cached(t, root, "go", "1.20.0", 1000, time.Now().Add(-1000*time.Hour))
	if removed, err := Prune(root, 0, time.Hour, time.Now()); err != nil || len(removed) != 0 || !exists(d) {
		t.Fatalf("removed %v, %v", removed, err)
	}
	// A cache that does not exist yet is fine.
	if _, err := Prune(filepath.Join(root, "none"), 10, time.Hour, time.Now()); err != nil {
		t.Error(err)
	}
}

func TestPruneSweepsInterruptedInstalls(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	dir := cached(t, root, "go", "1.20.0", 10, now)
	stale := filepath.Join(dir, ".part-123")
	fresh := filepath.Join(dir, ".part-456")
	for _, p := range []string{stale, fresh} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-48 * time.Hour)
	os.Chtimes(stale, old, old)
	if _, err := Prune(root, 0, 24*time.Hour, now); err != nil {
		t.Fatal(err)
	}
	if exists(stale) {
		t.Error("an old leftover stayed")
	}
	if !exists(fresh) {
		t.Error("an install that may still be running was removed")
	}
}

// An adopted install is a directory of links to the machine's own copy:
// removing it frees nothing, and the size it counts is that of the links.
func TestPruneCountsLinksAsNothing(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	target := filepath.Join(t.TempDir(), "go")
	os.WriteFile(target, make([]byte, 5000), 0o755)
	arch := filepath.Join(root, "go", "1.24.0", Arch())
	os.MkdirAll(filepath.Join(arch, "bin"), 0o755)
	os.Symlink(target, filepath.Join(arch, "bin", "go"))
	marker := arch + ".complete"
	os.WriteFile(marker, nil, 0o644)
	old := now.Add(-100 * time.Hour)
	os.Chtimes(marker, old, old)
	if removed, _ := Prune(root, 100, 24*time.Hour, now); len(removed) != 0 {
		t.Errorf("a link-only install counted as big: %v", removed)
	}
	if !exists(target) {
		t.Error("the machine's own copy is gone")
	}
}

// Choosing a cached version counts as using it.
func TestEnsureRefreshesTheLastUse(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-100 * time.Hour)
	cached(t, root, "go", "1.24.0", 10, old)
	p := &Provisioner{ToolCache: root, Installers: map[string]Installer{"go": failingInstaller{}}}
	res := p.Ensure(context.Background(), []scan.Demand{toolchain("go", "1.24", false)})
	if res.Items[0].Status != Cached {
		t.Fatalf("%+v", res.Items[0])
	}
	if got := lastUsed(filepath.Join(root, "go", "1.24.0")); time.Since(got) > time.Minute {
		t.Errorf("last use is still %v", got)
	}
}
