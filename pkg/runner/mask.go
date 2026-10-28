package runner

import (
	"slices"
	"strings"
	"sync"
)

// masker replaces secrets in log output with ***. Secrets come from the
// secrets context and from ::add-mask:: commands.
type masker struct {
	mu       sync.Mutex
	secrets  []string
	replacer *strings.Replacer
}

func (m *masker) add(secret string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Multi-line secrets rarely show up whole in one log line, so each line
	// is masked on its own too.
	for _, s := range append(strings.Split(secret, "\n"), secret) {
		s = strings.TrimSpace(s)
		if s == "" || slices.Contains(m.secrets, s) {
			continue
		}
		m.secrets = append(m.secrets, s)
	}
	// strings.Replacer tries the pairs in order, so longer secrets go first
	// and a secret that contains another is masked whole.
	slices.SortFunc(m.secrets, func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, 2*len(m.secrets))
	for _, s := range m.secrets {
		pairs = append(pairs, s, "***")
	}
	m.replacer = strings.NewReplacer(pairs...)
}

func (m *masker) mask(s string) string {
	m.mu.Lock()
	r := m.replacer
	m.mu.Unlock()
	if r == nil {
		return s
	}
	return r.Replace(s)
}

// contains reports whether s contains a secret.
func (m *masker) contains(s string) bool {
	return m.mask(s) != s
}
