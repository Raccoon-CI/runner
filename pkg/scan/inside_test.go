package scan

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/Raccoon-CI/raccoonci/internal/actions"
	"github.com/Raccoon-CI/raccoonci/pkg/expr"
	"github.com/Raccoon-CI/raccoonci/pkg/workflow"
)

func scanWith(t *testing.T, jobYAML string, repo fs.FS, resolve func(string) (*actions.Action, bool)) *Result {
	t.Helper()
	w, err := workflow.Parse([]byte("on: push\njobs:\n  j:\n" + indent(jobYAML, "    ")))
	if err != nil {
		t.Fatal(err)
	}
	return Job(Input{Workflow: w, Job: w.Jobs[0], Context: &expr.Context{Contexts: map[string]any{}},
		Image: ubuntu24, Repo: repo, Action: resolve})
}

func actionsByRef(t *testing.T, m map[string]string) func(string) (*actions.Action, bool) {
	return func(uses string) (*actions.Action, bool) {
		src, ok := m[uses]
		if !ok {
			return nil, false
		}
		a, err := actions.Parse([]byte(src))
		if err != nil {
			t.Fatalf("%s: %v", uses, err)
		}
		return a, true
	}
}

const compositeGo = `
name: build
inputs:
  go-version:
    default: '1.22'
  extra:
    default: ''
runs:
  using: composite
  steps:
    - uses: actions/setup-go@v5
      with:
        go-version: ${{ inputs.go-version }}
    - run: make build
      shell: bash
`

func TestCompositeActionSteps(t *testing.T) {
	resolve := actionsByRef(t, map[string]string{"acme/build@v1": compositeGo})
	// The inner setup-go gets the version the caller passes, and the inner
	// make is covered by the toolchain the action set up.
	r := scanWith(t, `
runs-on: ubuntu-latest
steps:
  - uses: acme/build@v1
    with:
      go-version: '1.23'
`, nil, resolve)
	assertDemands(t, r, "toolchain go@1.23, package make (implicit)")

	// Without `with:` the action's own default applies.
	r = scanWith(t, `
runs-on: ubuntu-latest
steps:
  - uses: acme/build@v1
`, nil, resolve)
	assertDemands(t, r, "toolchain go@1.22, package make (implicit)")

	// An input only known at run time leaves no wrong version behind.
	r = scanWith(t, `
runs-on: ubuntu-latest
steps:
  - uses: acme/build@v1
    with:
      go-version: ${{ steps.pick.outputs.v }}
`, nil, resolve)
	for _, d := range r.Demands {
		if d.Kind == Toolchain && d.Name == "go" && d.Version != "" {
			t.Errorf("a version appeared from nowhere: %+v", d)
		}
	}

	// Not downloadable: the scan stays quiet.
	r = scanWith(t, "runs-on: ubuntu-latest\nsteps:\n  - uses: acme/build@v1\n", nil, nil)
	assertDemands(t, r, "")
}

func TestCompositeActionCallingAnother(t *testing.T) {
	resolve := actionsByRef(t, map[string]string{
		"acme/outer@v1": `
runs:
  using: composite
  steps:
    - uses: acme/inner@v1
`,
		"acme/inner@v1": `
runs:
  using: composite
  steps:
    - run: docker build .
      shell: bash
`,
		"acme/loop@v1": `
runs:
  using: composite
  steps:
    - uses: acme/loop@v1
    - run: make
      shell: bash
`,
	})
	r := scanWith(t, "runs-on: ubuntu-latest\nsteps:\n  - uses: acme/outer@v1\n", nil, resolve)
	assertDemands(t, r, "tool docker (implicit)")
	// A composite action that calls itself must not recurse forever.
	r = scanWith(t, "runs-on: ubuntu-latest\nsteps:\n  - uses: acme/loop@v1\n", nil, resolve)
	assertDemands(t, r, "package make (implicit)")
}

// Steps guarded for other systems or switched-off inputs are not scanned; a
// condition that can't be decided yet keeps the step in.
func TestCompositeStepConditions(t *testing.T) {
	resolve := actionsByRef(t, map[string]string{"acme/upload@v1": `
inputs:
  fast:
    default: 'true'
runs:
  using: composite
  steps:
    - run: pwsh -c upload
      shell: bash
      if: runner.os == 'Windows'
    - run: curl -s https://example.com
      shell: bash
      if: runner.os == 'Linux'
    - run: docker pull x
      shell: bash
      if: inputs.fast != 'true'
    - run: make
      shell: bash
      if: steps.earlier.outputs.go == 'yes'
`})
	r := scanWith(t, "runs-on: ubuntu-latest\nsteps:\n  - uses: acme/upload@v1\n", nil, resolve)
	assertDemands(t, r, "package curl (implicit), package make (implicit)")
}

func TestDockerActionNeedsDocker(t *testing.T) {
	resolve := actionsByRef(t, map[string]string{"acme/box@v2": "runs:\n  using: docker\n  image: Dockerfile\n"})
	r := scanWith(t, "runs-on: ubuntu-latest\nsteps:\n  - uses: acme/box@v2\n", nil, resolve)
	assertDemands(t, r, "tool docker")
}

func TestJavaScriptActionNeedsNothingExtra(t *testing.T) {
	resolve := actionsByRef(t, map[string]string{"acme/js@v1": "runs:\n  using: node20\n  main: index.js\n"})
	r := scanWith(t, "runs-on: ubuntu-latest\nsteps:\n  - uses: acme/js@v1\n", nil, resolve)
	assertDemands(t, r, "")
}

func TestLocalCompositeActionIsReadFromTheRepo(t *testing.T) {
	repo := fstest.MapFS{
		".github/actions/lint/action.yml": {Data: []byte(`
runs:
  using: composite
  steps:
    - uses: actions/setup-node@v4
      with:
        node-version: 20
    - run: npm ci
      shell: bash
`)},
	}
	r := scanWith(t, "runs-on: ubuntu-latest\nsteps:\n  - uses: ./.github/actions/lint\n", repo, nil)
	assertDemands(t, r, "toolchain node@20")
	// Escapes and missing files are ignored, not followed.
	r = scanWith(t, "runs-on: ubuntu-latest\nsteps:\n  - uses: ./../outside\n  - uses: ./nope\n", repo, nil)
	assertDemands(t, r, "")
}

// Catalog actions keep their own rules: setup-go is not looked into.
func TestKnownActionsAreNotResolved(t *testing.T) {
	resolve := func(uses string) (*actions.Action, bool) {
		t.Errorf("resolved %s, which the catalog already knows", uses)
		return nil, false
	}
	scanWith(t, "runs-on: ubuntu-latest\nsteps:\n  - uses: actions/setup-go@v5\n    with:\n      go-version: '1.23'\n  - uses: actions/checkout@v4\n", nil, resolve)
}
