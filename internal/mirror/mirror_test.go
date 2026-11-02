package mirror

import "testing"

func TestBase(t *testing.T) {
	env := map[string]string{
		Prefix + "GO":            "https://mirror.example/golang/",
		Prefix + "NODE":          "ftp://x",
		Prefix + "RUST":          "https://user:pw@m.example",
		Prefix + "JAVA":          "  ",
		Prefix + "PYTHON_LATEST": "http://127.0.0.1:8080/p",
	}
	get := func(k string) string { return env[k] }
	for name, want := range map[string]string{
		"GO":            "https://mirror.example/golang",
		"NODE":          "def",
		"RUST":          "def",
		"JAVA":          "def",
		"UNSET":         "def",
		"PYTHON_LATEST": "http://127.0.0.1:8080/p",
	} {
		if got := base(get, name, "def"); got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
}
