package runnercmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Raccoon-CI/lib/pkg/catalog"
)

func TestTheCatalogSentWithAJobIsLaidOverTheBuiltInOne(t *testing.T) {
	base := catalog.Default()
	var warn bytes.Buffer

	if got := withSentCatalog(base, nil, &warn); got != base || warn.Len() != 0 {
		t.Error("nothing sent must be the built-in catalog, silently")
	}
	got := withSentCatalog(base, []byte("toolchains:\n  zig: {title: Zig, commands: [zig]}\n"), &warn)
	if _, ok := got.Command("zig"); !ok || warn.Len() != 0 {
		t.Errorf("sent catalog not applied: %q", warn.String())
	}
	if _, ok := got.Command("npm"); !ok {
		t.Error("the built-in catalog was replaced, not overlaid")
	}
	if _, ok := base.Command("zig"); ok {
		t.Error("the built-in catalog was changed")
	}

	got = withSentCatalog(base, []byte("toolchains: [broken"), &warn)
	if got != base || !strings.Contains(warn.String(), "using the built-in catalog") {
		t.Errorf("a bad catalog must fall back with a warning: %q", warn.String())
	}
}
