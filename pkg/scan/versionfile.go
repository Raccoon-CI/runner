package scan

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/Raccoon-CI/raccoonci/pkg/catalog"
)

// readVersionFile extracts the toolchain version a version file pins, the
// way the matching setup-* action reads it; the catalog says which format
// the file has. ok is false when the file doesn't exist or pins nothing.
func (s *scanner) readVersionFile(name, toolchain string) (version string, ok bool) {
	b, err := readLimited(s.in.Repo, path.Clean(strings.TrimPrefix(name, "./")))
	if err != nil {
		return "", false
	}
	vf := s.cat.VersionFile(path.Base(name))
	switch vf.Format {
	case catalog.FormatGoMod:
		return goModVersion(b)
	case catalog.FormatJSON:
		return jsonField(b, vf.Fields)
	case catalog.FormatToolVersions:
		var names []string
		if tc := s.cat.Toolchains[toolchain]; tc != nil {
			names = tc.Asdf
		}
		return toolVersions(b, names)
	}
	return firstLine(b)
}

// firstLine returns the first line that isn't a comment, without a leading v
// (.nvmrc, .node-version, .python-version, .ruby-version, ...).
func firstLine(b []byte) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			return strings.TrimPrefix(line, "v"), true
		}
	}
	return "", false
}

// maxVersionFile bounds reads of repo files, which come from untrusted
// pull requests.
const maxVersionFile = 1 << 20

func readLimited(repo fs.FS, name string) ([]byte, error) {
	f, err := repo.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxVersionFile))
}

// goModVersion prefers the toolchain directive, as setup-go does.
func goModVersion(b []byte) (string, bool) {
	var goDirective string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "toolchain":
			return strings.TrimPrefix(fields[1], "go"), true
		case "go":
			goDirective = fields[1]
		}
	}
	return goDirective, goDirective != ""
}

// jsonField returns the first of the dotted fields (volta.node, engines.node)
// that holds a non-empty string.
func jsonField(b []byte, fields []string) (string, bool) {
	var doc any
	if json.Unmarshal(b, &doc) != nil {
		return "", false
	}
	for _, f := range fields {
		v := doc
		for _, key := range strings.Split(f, ".") {
			m, _ := v.(map[string]any)
			v = m[key]
		}
		if s, ok := v.(string); ok && s != "" {
			return s, true
		}
	}
	return "", false
}

// toolVersions reads asdf's .tool-versions, where a toolchain may go by
// several names (nodejs or node).
func toolVersions(b []byte, names []string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		for _, name := range names {
			if fields[0] == name {
				return fields[1], true
			}
		}
	}
	return "", false
}
