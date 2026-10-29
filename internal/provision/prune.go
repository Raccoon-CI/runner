package provision

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Prune keeps the tool cache within maxBytes by removing the versions that
// were used longest ago, and returns what it removed as "<tool>/<version>".
// maxBytes <= 0 means no limit.
//
// A version's last use is the modification time of its .complete marker,
// which the provisioner refreshes whenever it picks the version (and install
// sets). Versions used within grace are never removed, so the limit is soft:
// a cache full of recent versions stays over it. grace must be longer than a
// job can run, because a running job does not refresh anything.
//
// Leftovers of interrupted installs (hidden entries such as .part-* and
// .download-*) older than grace are removed whatever the size.
func Prune(toolCache string, maxBytes int64, grace time.Duration, now time.Time) (removed []string, err error) {
	tools, err := os.ReadDir(toolCache)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type entry struct {
		id   string
		path string
		size int64
		used time.Time
	}
	var entries []entry
	var total int64
	for _, t := range tools {
		if !t.IsDir() {
			continue
		}
		versions, err := os.ReadDir(filepath.Join(toolCache, t.Name()))
		if err != nil {
			continue
		}
		for _, v := range versions {
			if !v.IsDir() {
				continue
			}
			dir := filepath.Join(toolCache, t.Name(), v.Name())
			sweepDebris(dir, now, grace)
			e := entry{id: t.Name() + "/" + v.Name(), path: dir, size: dirSize(dir), used: lastUsed(dir)}
			total += e.size
			entries = append(entries, e)
		}
	}
	if maxBytes <= 0 || total <= maxBytes {
		return nil, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].used.Before(entries[j].used) })
	for _, e := range entries {
		if total <= maxBytes {
			break
		}
		if now.Sub(e.used) < grace {
			break // sorted by age: everything after this is newer still
		}
		if err := evict(e.path); err != nil {
			return removed, err
		}
		total -= e.size
		removed = append(removed, e.id)
	}
	return removed, nil
}

// lastUsed is the newest .complete marker's time, or the directory's own.
func lastUsed(dir string) time.Time {
	var newest time.Time
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".complete") {
			if info, err := e.Info(); err == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
	}
	if newest.IsZero() {
		if info, err := os.Stat(dir); err == nil {
			return info.ModTime()
		}
	}
	return newest
}

// sweepDebris removes hidden entries (interrupted installs) older than grace.
func sweepDebris(dir string, now time.Time, grace time.Duration) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) >= grace {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

// dirSize adds up the regular files under dir. Symlinks are not followed, so
// an adopted install (a directory of links to the machine's own copy) counts
// as nearly nothing, which it is.
func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// evict removes a version. The markers go first, so that a process looking
// at the cache while this runs sees an incomplete version, not a broken one.
func evict(dir string) error {
	markers, _ := filepath.Glob(filepath.Join(dir, "*.complete"))
	for _, m := range markers {
		if err := os.Remove(m); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return os.RemoveAll(dir)
}
