package runner

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"raccoonci/internal/expr"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		line string
		want *command
	}{
		{"::error file=app.js,line=1,title=A%2C B%3A C::Missing%0Asemicolon %25", &command{
			name: "error", props: map[string]string{"file": "app.js", "line": "1", "title": "A, B: C"}, data: "Missing\nsemicolon %",
		}},
		{"  ::add-mask::hunter2", &command{name: "add-mask", props: map[string]string{}, data: "hunter2"}},
		{"::set-output name=x::", &command{name: "set-output", props: map[string]string{"name": "x"}, data: ""}},
		{"::warning bad,=x,y=::w", &command{name: "warning", props: map[string]string{}, data: "w"}},
		{"##[add-matcher]/path/tsc.json", &command{name: "add-matcher", props: map[string]string{}, data: "/path/tsc.json"}},
		{"##[set-output name=a%3Bb;]v%5D", &command{name: "set-output", props: map[string]string{"name": "a;b"}, data: "v]"}},
		{"##[section]Starting", nil},
		{"::unknown::x", nil},
		{"::error", nil},
		{"text ::error::x", nil},
	}
	for _, tt := range tests {
		c, ok := parseCommand(tt.line)
		if tt.want == nil {
			if ok {
				t.Errorf("%q: parsed as %+v, want plain output", tt.line, c)
			}
			continue
		}
		if !ok || !reflect.DeepEqual(c, *tt.want) {
			t.Errorf("%q: got %+v (ok=%v), want %+v", tt.line, c, ok, *tt.want)
		}
	}
}

func TestParseKeyValueFile(t *testing.T) {
	got, err := parseKeyValueFile([]byte("a=1\n\nb=x=y\r\nmulti<<EOF\nline 1\nline=2\nEOF\nempty<<END\nEND\nc=<<x\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"a", "1"}, {"b", "x=y"}, {"multi", "line 1\nline=2"}, {"empty", ""}, {"c", "<<x"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	for _, bad := range []string{"=x", "novalue", "k<<EOF\nnever closed", "<<EOF\nEOF"} {
		if _, err := parseKeyValueFile([]byte(bad)); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestMasker(t *testing.T) {
	var m masker
	m.add("abc")
	m.add("abcdef")
	m.add("line one\nline two")
	m.add("  ")
	got := m.mask("abcdef abc line two; line one\nline two")
	if want := "*** *** ***; ***"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if m.contains("nothing here") {
		t.Error("contains: false positive")
	}
}

func TestShellCommand(t *testing.T) {
	path := os.Getenv("PATH")
	argv, _, err := shellCommand("bash", "echo", "/tmp/s.sh", path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(argv[1:], " "); got != "--noprofile --norc -eo pipefail /tmp/s.sh" {
		t.Errorf("bash: %s", got)
	}
	argv, _, err = shellCommand("", "echo", "/tmp/s.sh", path)
	if err != nil || strings.Join(argv[1:], " ") != "-e /tmp/s.sh" {
		t.Errorf("default shell: %v %v", argv, err)
	}
	argv, _, err = shellCommand(`bash -c "source {0}"`, "echo", "/tmp/s", path)
	if err != nil || !reflect.DeepEqual(argv[1:], []string{"-c", "source /tmp/s"}) {
		t.Errorf("custom shell: %q %v", argv, err)
	}
	if _, _, err := shellCommand("cmd", "", "", path); err == nil || !strings.Contains(err.Error(), "Windows") {
		t.Errorf("cmd: %v", err)
	}
	if _, _, err := shellCommand("perl -w", "", "", path); err == nil || !strings.Contains(err.Error(), "{0}") {
		t.Errorf("custom shell without {0}: %v", err)
	}
	if got := scriptExt("pwsh"); got != ".ps1" {
		t.Errorf("pwsh ext %q", got)
	}
}

func TestHashFiles(t *testing.T) {
	ws := t.TempDir()
	write := func(name, data string) {
		p := filepath.Join(ws, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package-lock.json", "root")
	write("app/package-lock.json", "app")
	write("app/node_modules/x/package-lock.json", "dep")
	write("src/.hidden", "h")
	if err := os.Symlink("/etc/hostname", filepath.Join(ws, "escape.json")); err != nil {
		t.Fatal(err)
	}
	h := hashFiles(ws)
	hash := func(patterns ...any) string {
		t.Helper()
		v, err := h(patterns)
		if err != nil {
			t.Fatal(err)
		}
		return v.(string)
	}
	all := hash("**/package-lock.json")
	if len(all) != 64 {
		t.Fatalf("hash = %q", all)
	}
	if got := hash("**/package-lock.json", "!**/node_modules/**"); got == all || got == "" {
		t.Errorf("negation had no effect")
	}
	if hash("**/package-lock.json") != all {
		t.Error("not deterministic")
	}
	if hash("src") == "" || hash("src/*") == "" {
		t.Error("directories and * should include dotfiles below them")
	}
	if got := hash("*.json"); got != hash("package-lock.json") {
		t.Error("a symlink out of the workspace must be ignored")
	}
	if got := hash("nothing/**"); got != "" {
		t.Errorf("no match should be empty, got %q", got)
	}
	var _ expr.Func = h
}
