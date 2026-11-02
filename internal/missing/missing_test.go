package missing

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestStore(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "m.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	k := Key("ci.yml", "build")
	if added, err := s.Add(k, "jq"); !added || err != nil {
		t.Fatalf("add: %v %v", added, err)
	}
	if added, _ := s.Add(k, "jq"); added {
		t.Error("repeat reported as new")
	}
	for _, bad := range []string{"", "../x", "-rf", "a b", "a;b", "$(x)"} {
		if added, _ := s.Add(k, bad); added {
			t.Errorf("accepted %q", bad)
		}
	}
	s2, _ := Open(p)
	if got := s2.Commands(k); !slices.Equal(got, []string{"jq"}) {
		t.Errorf("reloaded: %v", got)
	}
	if len(s2.Commands(Key("ci.yml", "other"))) != 0 {
		t.Error("other job sees it")
	}
	os.WriteFile(p, []byte("{broken"), 0o644)
	if s3, err := Open(p); err != nil || len(s3.Commands(k)) != 0 {
		t.Errorf("damaged file: %v", err)
	}
}
