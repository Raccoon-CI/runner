package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Raccoon-CI/raccoonci/internal/expr"
)

// hashFiles implements hashFiles(pattern...): a SHA-256 over the SHA-256 of
// every matching file under the workspace, or "" when nothing matches. Like
// @actions/glob, `*` matches dotfiles, a pattern naming a directory covers
// everything below it, `!` excludes, and later patterns win. Files that
// resolve outside the workspace are ignored.
func hashFiles(workspace string) expr.Func {
	return func(args []any) (any, error) {
		if len(args) == 0 {
			return nil, errors.New("hashFiles() takes at least 1 argument")
		}
		var patterns []string
		for _, a := range args {
			for _, p := range strings.Split(expr.ToString(a), "\n") {
				if p = strings.TrimSpace(p); p != "" && !strings.HasPrefix(p, "#") {
					patterns = append(patterns, p)
				}
			}
		}
		root, err := os.OpenRoot(workspace)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		var files []string
		err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || p == "." {
				return err
			}
			if d.IsDir() || !matchesPatterns(p, patterns, workspace) {
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				fi, err := fs.Stat(root.FS(), p)
				if err != nil || !fi.Mode().IsRegular() { // broken, escaping, or a directory
					return nil
				}
			} else if !d.Type().IsRegular() {
				return nil
			}
			files = append(files, p)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("hashFiles: %w", err)
		}
		if len(files) == 0 {
			return "", nil
		}
		slices.Sort(files)
		total := sha256.New()
		for _, p := range files {
			f, err := root.Open(p)
			if err != nil {
				continue // escapes the workspace through a symlink
			}
			h := sha256.New()
			_, err = io.Copy(h, f)
			f.Close()
			if err != nil {
				return nil, fmt.Errorf("hashFiles: %w", err)
			}
			total.Write(h.Sum(nil))
		}
		return hex.EncodeToString(total.Sum(nil)), nil
	}
}

// matchesPatterns reports whether the workspace-relative path p is selected.
func matchesPatterns(p string, patterns []string, workspace string) bool {
	selected := false
	for _, pat := range patterns {
		negate := strings.HasPrefix(pat, "!")
		pat = strings.TrimPrefix(pat, "!")
		if filepath.IsAbs(pat) {
			rel, err := filepath.Rel(workspace, pat)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			pat = rel
		}
		pat = strings.TrimSuffix(path.Clean(strings.TrimPrefix(pat, "./")), "/")
		if matchWithAncestors(strings.Split(pat, "/"), strings.Split(p, "/")) {
			selected = !negate
		}
	}
	return selected
}

// matchWithAncestors matches p or any directory above it, since a pattern
// that matches a directory covers its contents.
func matchWithAncestors(pat, p []string) bool {
	for n := len(p); n > 0; n-- {
		if matchSegments(pat, p[:n]) {
			return true
		}
	}
	return false
}

func matchSegments(pat, p []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(p); i++ {
				if matchSegments(pat[1:], p[i:]) {
					return true
				}
			}
			return false
		}
		if len(p) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], p[0]); !ok {
			return false
		}
		pat, p = pat[1:], p[1:]
	}
	return len(p) == 0
}
