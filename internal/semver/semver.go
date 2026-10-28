// Package semver matches version specs the way the setup-* actions read
// them (npm-style semver ranges): 20, 20.x, 1.24, >=18, ^1.22, ~1.2.3,
// 1.2 - 1.4, and alternatives joined with ||. Pre-release versions never
// satisfy a range; they are only chosen by naming them exactly.
package semver

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Version struct {
	Major, Minor, Patch int
	Pre                 string // "" for releases
}

var versionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

// Parse reads a full version such as 1.2.3 or v1.2.3-rc.1.
func Parse(s string) (Version, bool) {
	m := versionRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, false
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	return Version{n(m[1]), n(m[2]), n(m[3]), m[4]}, true
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// Compare returns -1, 0 or 1. A pre-release sorts before its release.
func (v Version) Compare(o Version) int {
	for _, d := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case v.Pre == o.Pre:
		return 0
	case v.Pre == "":
		return 1
	case o.Pre == "":
		return -1
	case v.Pre < o.Pre:
		return -1
	}
	return 1
}

// comparator is one bound: version op bound.
type comparator struct {
	op string // "=", ">", ">=", "<", "<="
	v  Version
}

func (c comparator) ok(v Version) bool {
	cmp := v.Compare(c.v)
	switch c.op {
	case "=":
		return cmp == 0
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	case "<":
		return cmp < 0
	}
	return cmp <= 0
}

// Range is alternatives (||) of comparator sets that must all hold.
type Range struct {
	alts [][]comparator
}

// Contains reports whether v satisfies the range. Pre-releases never do.
func (r Range) Contains(v Version) bool {
	if v.Pre != "" {
		return false
	}
	for _, alt := range r.alts {
		all := true
		for _, c := range alt {
			if !c.ok(v) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// partial is a possibly incomplete version: a missing or wildcard part is -1.
type partial struct {
	major, minor, patch int
	pre                 string
}

func (p partial) missing() int { // how many trailing parts are open
	switch {
	case p.major < 0:
		return 3
	case p.minor < 0:
		return 2
	case p.patch < 0:
		return 1
	}
	return 0
}

func (p partial) low() Version {
	return Version{max(p.major, 0), max(p.minor, 0), max(p.patch, 0), p.pre}
}

// next is the first version after everything p covers: 1.2 -> 1.3.0.
func (p partial) next() Version {
	switch p.missing() {
	case 1:
		return Version{p.major, p.minor + 1, 0, ""}
	case 2:
		return Version{p.major + 1, 0, 0, ""}
	}
	return Version{p.major, p.minor, p.patch + 1, ""}
}

func parsePartial(s string) (partial, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "=")
	core, pre, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) > 3 || core == "" {
		return partial{}, fmt.Errorf("invalid version %q", s)
	}
	p := partial{-1, -1, -1, pre}
	open := false
	for i, part := range parts {
		wildcard := part == "x" || part == "X" || part == "*"
		if wildcard {
			open = true // everything after a wildcard is open too
		}
		if open {
			if !wildcard {
				return partial{}, fmt.Errorf("invalid version %q", s)
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return partial{}, fmt.Errorf("invalid version %q", s)
		}
		switch i {
		case 0:
			p.major = n
		case 1:
			p.minor = n
		case 2:
			p.patch = n
		}
	}
	return p, nil
}

// ParseRange parses a version spec. An empty spec, "*", "x" and "latest"
// match every release.
func ParseRange(spec string) (Range, error) {
	spec = strings.TrimSpace(spec)
	var r Range
	for _, alt := range strings.Split(spec, "||") {
		cs, err := parseSet(strings.TrimSpace(alt))
		if err != nil {
			return Range{}, fmt.Errorf("%q: %w", spec, err)
		}
		r.alts = append(r.alts, cs)
	}
	return r, nil
}

func parseSet(s string) ([]comparator, error) {
	// Glue operators to their versions (">= 1.2" -> ">=1.2") and find hyphen ranges.
	var toks []string
	for _, f := range strings.Fields(s) {
		if n := len(toks); n > 0 && isOp(toks[n-1]) {
			toks[n-1] += f
		} else {
			toks = append(toks, f)
		}
	}
	var out []comparator
	for i := 0; i < len(toks); i++ {
		if i+2 < len(toks) && toks[i+1] == "-" {
			lo, err1 := parsePartial(toks[i])
			hi, err2 := parsePartial(toks[i+2])
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("invalid hyphen range %s - %s", toks[i], toks[i+2])
			}
			out = append(out, comparator{">=", lo.low()})
			if hi.missing() == 0 {
				out = append(out, comparator{"<=", hi.low()})
			} else {
				out = append(out, comparator{"<", hi.next()})
			}
			i += 2
			continue
		}
		cs, err := parseToken(toks[i])
		if err != nil {
			return nil, err
		}
		out = append(out, cs...)
	}
	return out, nil
}

func isOp(s string) bool {
	switch s {
	case ">", ">=", "<", "<=", "=", "^", "~", "~>":
		return true
	}
	return false
}

func parseToken(tok string) ([]comparator, error) {
	if tok == "latest" || tok == "*" {
		return nil, nil
	}
	op := ""
	for _, o := range []string{">=", "<=", "~>", ">", "<", "=", "^", "~"} {
		if strings.HasPrefix(tok, o) {
			op, tok = o, tok[len(o):]
			break
		}
	}
	p, err := parsePartial(tok)
	if err != nil {
		return nil, err
	}
	any := p.missing() == 3
	switch op {
	case "", "=":
		switch {
		case any:
			return nil, nil
		case p.missing() == 0:
			return []comparator{{"=", p.low()}}, nil
		}
		return []comparator{{">=", p.low()}, {"<", p.next()}}, nil
	case ">":
		if any {
			return []comparator{{"<", Version{}}}, nil // nothing is greater than everything
		}
		if p.missing() == 0 {
			return []comparator{{">", p.low()}}, nil
		}
		return []comparator{{">=", p.next()}}, nil
	case ">=":
		return []comparator{{">=", p.low()}}, nil
	case "<":
		if any {
			return []comparator{{"<", Version{}}}, nil
		}
		return []comparator{{"<", p.low()}}, nil
	case "<=":
		if any {
			return nil, nil
		}
		if p.missing() == 0 {
			return []comparator{{"<=", p.low()}}, nil
		}
		return []comparator{{"<", p.next()}}, nil
	case "~", "~>":
		if any {
			return nil, nil
		}
		hi := Version{p.major, p.minor + 1, 0, ""}
		if p.minor < 0 {
			hi = Version{p.major + 1, 0, 0, ""}
		}
		return []comparator{{">=", p.low()}, {"<", hi}}, nil
	}
	// ^
	if any {
		return nil, nil
	}
	var hi Version
	switch {
	case p.major > 0:
		hi = Version{p.major + 1, 0, 0, ""}
	case p.minor < 0:
		hi = Version{1, 0, 0, ""}
	case p.minor > 0:
		hi = Version{0, p.minor + 1, 0, ""}
	case p.patch < 0:
		hi = Version{0, 1, 0, ""}
	default:
		hi = Version{0, 0, p.patch + 1, ""}
	}
	return []comparator{{">=", p.low()}, {"<", hi}}, nil
}

// Pick returns the highest of versions that satisfies spec, as the string it
// appears in. A spec that names a version exactly (even a pre-release) picks
// that version. It fails if nothing matches.
func Pick(spec string, versions []string) (string, error) {
	if want, ok := Parse(spec); ok {
		for _, s := range versions {
			if v, ok := Parse(s); ok && v.Compare(want) == 0 {
				return s, nil
			}
		}
		return "", fmt.Errorf("no version %s", spec)
	}
	r, err := ParseRange(spec)
	if err != nil {
		return "", err
	}
	best, bestV := "", Version{}
	for _, s := range versions {
		v, ok := Parse(s)
		if !ok || !r.Contains(v) {
			continue
		}
		if best == "" || v.Compare(bestV) > 0 {
			best, bestV = s, v
		}
	}
	if best == "" {
		return "", fmt.Errorf("no version matches %q", spec)
	}
	return best, nil
}
