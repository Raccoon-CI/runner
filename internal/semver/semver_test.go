package semver

import "testing"

var versions = []string{
	"v0.2.3", "v0.2.9", "v0.3.0", "v1.0.0", "v1.2.3", "v1.2.9", "v1.3.0", "v1.10.0",
	"v2.0.0-rc.1", "v2.0.0", "v2.5.1", "v3.0.0",
}

func TestPick(t *testing.T) {
	tests := []struct{ spec, want string }{
		// What setup-node / setup-go / setup-python users write.
		{"1", "v1.10.0"},
		{"1.2", "v1.2.9"},
		{"1.x", "v1.10.0"},
		{"1.2.x", "v1.2.9"},
		{"1.*", "v1.10.0"},
		{"1.2.3", "v1.2.3"},
		{"v1.2.3", "v1.2.3"},
		{"=1.2.3", "v1.2.3"},
		{"*", "v3.0.0"},
		{"", "v3.0.0"},
		{"latest", "v3.0.0"},
		{"x", "v3.0.0"},
		// Comparators.
		{">=1.3", "v3.0.0"},
		{">1.3.0", "v3.0.0"},
		{">1.3", "v3.0.0"}, // beyond everything 1.3.x covers
		{"<1.3", "v1.2.9"},
		{"<=1.3", "v1.3.0"},
		{"<=1.2", "v1.2.9"},
		{">= 1.2 < 1.3", "v1.2.9"},
		{">=1.2.3 <=1.3.0", "v1.3.0"},
		{"<1", "v0.3.0"},
		// Caret and tilde.
		{"^1.2.3", "v1.10.0"},
		{"^1.2", "v1.10.0"},
		{"^0.2.3", "v0.2.9"},
		{"^0.2", "v0.2.9"},
		{"^0", "v0.3.0"},
		{"^2", "v2.5.1"},
		{"~1.2.3", "v1.2.9"},
		{"~1.2", "v1.2.9"},
		{"~1", "v1.10.0"},
		{"~>1.2.3", "v1.2.9"},
		// Alternatives and hyphen ranges.
		{"1.0.0 || 2.5", "v2.5.1"},
		{"^0.2 || ^1.0", "v1.10.0"},
		{"1.2 - 1.3", "v1.3.0"},
		{"1.2.3 - 1.2.9", "v1.2.9"},
		{"0.2.3 - 0.2", "v0.2.9"},
		// Pre-releases only when named.
		{"2", "v2.5.1"},
		{"2.0.0-rc.1", "v2.0.0-rc.1"},
		{"<2.0.0", "v1.10.0"},
	}
	for _, tt := range tests {
		got, err := Pick(tt.spec, versions)
		if err != nil || got != tt.want {
			t.Errorf("Pick(%q) = %q, %v; want %q", tt.spec, got, err, tt.want)
		}
	}
}

func TestPickFailures(t *testing.T) {
	for _, spec := range []string{"9", "1.4", ">3", "<0.1", "4.0.0", "2.0.0-rc.9", "^3.1", "1.2.3.4", "abc", "1.x.3x", ">=", "1 - ", "lts"} {
		if got, err := Pick(spec, versions); err == nil {
			t.Errorf("Pick(%q) = %q; want an error", spec, got)
		}
	}
}

func TestVersion(t *testing.T) {
	v, ok := Parse("v1.24.13")
	if !ok || v != (Version{1, 24, 13, ""}) || v.String() != "1.24.13" {
		t.Errorf("%+v %v", v, ok)
	}
	if v, ok := Parse("1.2.3-rc.1+build5"); !ok || v.Pre != "rc.1" || v.String() != "1.2.3-rc.1" {
		t.Errorf("%+v %v", v, ok)
	}
	for _, bad := range []string{"1.2", "v1", "1.2.x", "a.b.c", "", "1.2.3.4"} {
		if _, ok := Parse(bad); ok {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
	a, _ := Parse("1.2.3-rc.1")
	b, _ := Parse("1.2.3")
	c, _ := Parse("1.10.0")
	if a.Compare(b) >= 0 || b.Compare(a) <= 0 || b.Compare(b) != 0 || b.Compare(c) >= 0 {
		t.Error("Compare")
	}
}
