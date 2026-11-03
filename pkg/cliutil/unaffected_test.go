package cliutil

import (
	"testing"
	"testing/fstest"

	"github.com/Raccoon-CI/lib/pkg/catalog"
	"github.com/Raccoon-CI/lib/pkg/workflow"
)

// The directories package discovery skips are the catalog in use, not only the
// built-in one: `raccoonci run -affected` follows a -catalog overlay too.
func TestUnaffectedFollowsTheCatalogsSkipDirs(t *testing.T) {
	repo := fstest.MapFS{
		"package.json":               {Data: []byte(`{"name":"root","workspaces":["packages/*"]}`)},
		"packages/a/package.json":    {Data: []byte(`{"name":"a","dependencies":{"in-a-venv":"*"}}`)},
		"packages/b/package.json":    {Data: []byte(`{"name":"b"}`)},
		"packages/venv/package.json": {Data: []byte(`{"name":"in-a-venv"}`)},
	}
	w, err := workflow.Parse([]byte(`on: push
jobs:
  ja:
    runs-on: ubuntu-latest
    steps:
      - run: echo
        working-directory: packages/a
  jb:
    runs-on: ubuntu-latest
    steps:
      - run: echo
        working-directory: packages/b
`))
	if err != nil {
		t.Fatal(err)
	}
	jobB := func(cat *catalog.Catalog) bool {
		t.Helper()
		fn, err := Unaffected(w, repo, ".github/workflows/ci.yml", []string{"packages/venv/x.js"}, cat)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range w.Jobs {
			if j.ID == "jb" {
				_, leftOut := fn(j)
				return leftOut
			}
		}
		t.Fatal("no job jb")
		return false
	}

	// Built in: venv is skipped, so the changed file belongs to nothing known
	// and no job is left out.
	if jobB(catalog.Default()) {
		t.Error("built-in skip-dirs: jb was left out although the changed file is unknown to the repository's packages")
	}
	// A catalog that does not skip venv: packages/venv is a package only
	// packages/a depends on, so the unrelated jb is left out.
	cat, err := catalog.Default().Overlay([]byte("skip-dirs: [.git]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !jobB(cat) {
		t.Error("catalog without venv in skip-dirs: jb should be left out")
	}
}
