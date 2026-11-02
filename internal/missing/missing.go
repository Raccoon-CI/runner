// Package missing remembers commands a job could not find (exit code 127), so
// the next run of the same job can install what provides them first.
//
// The scan is only a heuristic and can miss a command, for instance one
// called from a script in the repository. When a step then fails with
// "command not found", the runner reports the name here. The file is an
// estimate like sched.History: a damaged one reads as empty.
package missing

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
)

// keep bounds how many commands are remembered per job.
const keep = 32

// validName is what a command name may look like; anything else (paths,
// options, shell syntax) is not remembered, since the names end up in a file
// and in the scan.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// Store maps a job to the commands it could not find, in a file.
type Store struct {
	path string
	mu   sync.Mutex
	d    map[string][]string
}

// Open reads the file at path; a missing or damaged file is an empty store.
func Open(path string) (*Store, error) {
	s := &Store{path: path, d: map[string][]string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(b, &s.d) != nil || s.d == nil {
		s.d = map[string][]string{}
	}
	return s, nil
}

// Key names a job: the workflow file and the job's id.
func Key(workflow, job string) string { return workflow + "\x00" + job }

// Add remembers that key could not find cmd. It reports whether that is new.
func (s *Store) Add(key, cmd string) (bool, error) {
	if !validName.MatchString(cmd) {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.Contains(s.d[key], cmd) {
		return false, nil
	}
	l := append(s.d[key], cmd)
	if len(l) > keep {
		l = l[len(l)-keep:]
	}
	s.d[key] = l
	return true, s.save()
}

// Commands lists what key could not find before.
func (s *Store) Commands(key string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.d[key])
}

func (s *Store) save() error {
	b, err := json.Marshal(s.d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".missing-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return werr
	}
	return os.Rename(tmp.Name(), s.path)
}
