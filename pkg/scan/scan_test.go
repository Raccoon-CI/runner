package scan

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Raccoon-CI/raccoonci/pkg/catalog"
	"github.com/Raccoon-CI/raccoonci/pkg/expr"
	"github.com/Raccoon-CI/raccoonci/pkg/runnerimages"
	"github.com/Raccoon-CI/raccoonci/pkg/workflow"
)

var ubuntu24 = &runnerimages.Defaults{
	Image:    "ubuntu24",
	Tools:    map[string]string{"node": "22.23.3", "python": "3.12.3", "go": "1.24.13", "java": "17.0.20+1"},
	Packages: []string{"curl", "gcc", "jq", "make", "shellcheck"},
	Software: []string{"docker client", "maven", "powershell", "yarn"},
}

// scanSteps scans a job made of the given steps YAML, using the first matrix
// combination as context.
func scanSteps(t *testing.T, jobYAML string, image *runnerimages.Defaults, repo fs.FS) *Result {
	t.Helper()
	w, err := workflow.Parse([]byte("on: push\njobs:\n  j:\n" + indent(jobYAML, "    ")))
	if err != nil {
		t.Fatal(err)
	}
	j := w.Jobs[0]
	var m any
	if j.Strategy != nil {
		m = j.Strategy.Matrix
	}
	combos, err := workflow.ExpandMatrix(m)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &expr.Context{Contexts: map[string]any{"matrix": combos[len(combos)-1]}}
	return Job(Input{Workflow: w, Job: j, Context: ctx, Image: image, Repo: repo})
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.Trim(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}

func format(r *Result) string {
	var parts []string
	for _, d := range r.Demands {
		s := d.Kind.String() + " " + d.Name
		if d.Version != "" {
			s += "@" + d.Version
		}
		if d.Implicit {
			s += " (implicit)"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func assertDemands(t *testing.T, r *Result, want string) {
	t.Helper()
	if got := format(r); got != want {
		t.Errorf("demands:\n got: %s\nwant: %s\nreasons: %v", got, want, reasons(r))
	}
}

func reasons(r *Result) []string {
	var out []string
	for _, d := range r.Demands {
		out = append(out, d.Reason)
	}
	return out
}

func assertWarning(t *testing.T, r *Result, substr string) {
	t.Helper()
	for _, w := range r.Warnings {
		if strings.Contains(w, substr) {
			return
		}
	}
	t.Errorf("no warning containing %q in %q", substr, r.Warnings)
}

func TestImplicitToolsOfCarelessWorkflow(t *testing.T) {
	r := scanSteps(t, `
runs-on: ubuntu-latest
steps:
  - uses: actions/checkout@v4
  - run: |
      npm ci
      npm test | tee out.txt
      jq .version package.json
      sudo apt-get install -y shellcheck && shellcheck build.sh
`, ubuntu24, nil)
	assertDemands(t, r, "toolchain node@22.23.3 (implicit), package jq (implicit)")
	if !strings.Contains(r.Demands[0].Reason, "step 2 runs npm without setting up Node.js") {
		t.Errorf("reason = %q", r.Demands[0].Reason)
	}
}

func TestExplicitSetupCoversLaterUse(t *testing.T) {
	r := scanSteps(t, `
runs-on: ubuntu-latest
steps:
  - uses: actions/setup-node@v4
    with:
      node-version: 20
  - run: npm ci
`, ubuntu24, nil)
	assertDemands(t, r, "toolchain node@20")
}

func TestUseBeforeSetupIsImplicit(t *testing.T) {
	r := scanSteps(t, `
runs-on: ubuntu-latest
steps:
  - run: node --version
  - uses: actions/setup-node@v4
    with:
      node-version: 20
`, ubuntu24, nil)
	assertDemands(t, r, "toolchain node@22.23.3 (implicit), toolchain node@20")
}

// The image's python3 is used first, then the same version is asked for by
// name: one demand, which must not be treated as "something like it will do".
func TestExplicitVersionPinsAnImplicitDemand(t *testing.T) {
	r := scanSteps(t, `
runs-on: ubuntu-latest
steps:
  - run: python3 --version
  - uses: actions/setup-python@v5
    with:
      python-version: 3.12.3
`, ubuntu24, nil)
	assertDemands(t, r, "toolchain python@3.12.3 (implicit)")
	if !r.Demands[0].Declared {
		t.Error("the explicit setup-python must mark the demand as Declared")
	}

	// A different version stays a separate demand, and nothing is Declared.
	r = scanSteps(t, `
runs-on: ubuntu-latest
steps:
  - run: python3 --version
  - uses: actions/setup-python@v5
    with:
      python-version: "3.11"
`, ubuntu24, nil)
	assertDemands(t, r, "toolchain python@3.12.3 (implicit), toolchain python@3.11")
	if r.Demands[0].Declared || r.Demands[1].Declared {
		t.Errorf("unexpected Declared: %+v", r.Demands)
	}
}

func TestMatrixVersionAndFloatWarning(t *testing.T) {
	r := scanSteps(t, `
runs-on: ubuntu-latest
strategy:
  matrix:
    python: [3.9, 3.10]
steps:
  - uses: actions/setup-python@v5
    with:
      python-version: ${{ matrix.python }}
  - run: pip install -r requirements.txt
`, ubuntu24, nil)
	assertDemands(t, r, "toolchain python@3.1")
	assertWarning(t, r, "YAML reads 3.10 as 3.1")
}

func TestNoFloatWarningForIntendedVersions(t *testing.T) {
	r := scanSteps(t, `
runs-on: ubuntu-latest
steps:
  - uses: actions/setup-python@v5
    with:
      python-version: 3.9
  - uses: actions/setup-go@v5
    with:
      go-version: 1.24
`, ubuntu24, nil)
	if len(r.Warnings) > 0 {
		t.Errorf("unexpected warnings: %q", r.Warnings)
	}
}

func TestVersionFiles(t *testing.T) {
	repo := fstest.MapFS{
		".nvmrc":          {Data: []byte("# pinned\nv20.11.0\n")},
		"go.mod":          {Data: []byte("module x\n\ngo 1.23\n\ntoolchain go1.23.4\n")},
		".python-version": {Data: []byte("3.13\n")},
		".tool-versions":  {Data: []byte("nodejs 18.20.0\nruby 3.3.1\n")},
	}
	r := scanSteps(t, `
runs-on: ubuntu-latest
steps:
  - uses: actions/setup-node@v4
    with:
      node-version-file: .nvmrc
  - uses: actions/setup-go@v5
    with:
      go-version-file: go.mod
  - uses: actions/setup-python@v5
  - uses: ruby/setup-ruby@v1
  - uses: actions/setup-java@v4
    with:
      java-version-file: .java-version
`, ubuntu24, repo)
	assertDemands(t, r, "toolchain node@20.11.0, toolchain go@1.23.4, toolchain python@3.13, toolchain ruby@3.3.1")
	assertWarning(t, r, ".java-version, which doesn't exist")
}

func TestVersionsKnownOnlyAtRunTime(t *testing.T) {
	r := scanSteps(t, `
runs-on: ubuntu-latest
steps:
  - uses: actions/setup-node@v4
    with:
      node-version: ${{ needs.setup.outputs.node }}
  - uses: actions/setup-go@v5
    with:
      go-version-file: go.mod
`, ubuntu24, nil)
	assertDemands(t, r, "toolchain node, toolchain go")
	for _, d := range r.Demands {
		if !strings.Contains(d.Reason, "run time") {
			t.Errorf("reason %q should say it's resolved at run time", d.Reason)
		}
	}
}

func TestSetupWithoutVersionUsesPreinstalled(t *testing.T) {
	r := scanSteps(t, `
runs-on: ubuntu-latest
steps:
  - uses: actions/setup-node@v4
    with:
      cache: npm
  - uses: dtolnay/rust-toolchain@1.80.0
`, ubuntu24, nil)
	assertDemands(t, r, "toolchain node@22.23.3 (implicit), toolchain rust@1.80.0")
}

func TestOpaqueBuildUsesRepoManifests(t *testing.T) {
	repo := fstest.MapFS{"go.mod": {Data: []byte("module x\n")}, "Makefile": {Data: []byte("test:\n\tgo test ./...\n")}}
	job := `
runs-on: ubuntu-latest
steps:
  - uses: actions/checkout@v4
  - run: make test
`
	assertDemands(t, scanSteps(t, job, ubuntu24, repo), "package make (implicit), toolchain go@1.24.13 (implicit)")
	// Without the repo, only the command itself is known.
	assertDemands(t, scanSteps(t, job, ubuntu24, nil), "package make (implicit)")
}

func TestUbuntuOnlyWarnings(t *testing.T) {
	r := scanSteps(t, `
runs-on: ubuntu-latest
steps:
  - run: |
      echo "deb https://example.com/apt $(lsb_release -cs) main" | sudo tee /etc/apt/sources.list.d/x.list
      sudo add-apt-repository -y ppa:deadsnakes/ppa
`, ubuntu24, nil)
	assertWarning(t, r, "lsb_release reports Debian's 'trixie'")
	assertWarning(t, r, "Ubuntu PPA ppa:deadsnakes/ppa")
}

func TestWrappersFunctionsAndRequiredToolchains(t *testing.T) {
	r := scanSteps(t, `
runs-on: ubuntu-latest
steps:
  - run: |
      jq() { cat; }
      jq < in.json
      sudo -E env FOO=1 timeout 5m docker build .
      yarn install
      ./gradlew build
`, ubuntu24, nil)
	assertDemands(t, r, "tool docker (implicit), toolchain node@22.23.3 (implicit), tool yarn (implicit)")
}

func TestShells(t *testing.T) {
	r := scanSteps(t, `
runs-on: ubuntu-latest
defaults:
  run:
    shell: pwsh
steps:
  - run: Write-Host hi
  - run: print("hi")
    shell: python
`, ubuntu24, nil)
	assertDemands(t, r, "tool powershell (implicit), toolchain python@3.12.3 (implicit)")
}

func TestContainerJobOnlyNeedsDocker(t *testing.T) {
	r := scanSteps(t, `
runs-on: ubuntu-latest
container: node:22
steps:
  - run: npm test
`, ubuntu24, nil)
	assertDemands(t, r, "tool docker")
}

func TestNoImageOnlyExplicit(t *testing.T) {
	r := scanSteps(t, `
runs-on: [self-hosted, gpu]
steps:
  - uses: actions/setup-python@v5
    with:
      python-version: "3.12"
  - run: npm test && jq . x
`, nil, nil)
	assertDemands(t, r, "toolchain python@3.12")
}

func TestUnparseableScriptWarns(t *testing.T) {
	r := scanSteps(t, `
runs-on: ubuntu-latest
steps:
  - run: "if then fi ((("
`, ubuntu24, nil)
	assertWarning(t, r, "couldn't parse the script")
}

func TestExpressionsInScripts(t *testing.T) {
	r := scanSteps(t, `
runs-on: ubuntu-latest
strategy:
  matrix:
    tool: [jq]
steps:
  - run: ${{ matrix.tool }} . data.json && ${{ github.workspace }}/bin/x
`, ubuntu24, nil)
	assertDemands(t, r, "package jq (implicit)")
}

// A new toolchain needs only catalog entries, no code.
func TestCatalogOverlayAddsToolchain(t *testing.T) {
	cat, err := catalog.Default().Overlay([]byte(`
toolchains:
  zig:
    title: Zig
    commands: [zig]
    manifests: [build.zig]
    asdf: [zig]
actions:
  mlugg/setup-zig: {setup: zig, version-input: version, default-files: [.tool-versions]}
`))
	if err != nil {
		t.Fatal(err)
	}
	w, err := workflow.Parse([]byte(`
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - uses: mlugg/setup-zig@v2
      - run: zig build test
`))
	if err != nil {
		t.Fatal(err)
	}
	repo := fstest.MapFS{".tool-versions": {Data: []byte("zig 0.15.1\n")}}
	r := Job(Input{Workflow: w, Job: w.Jobs[0], Image: ubuntu24, Repo: repo, Catalog: cat})
	assertDemands(t, r, "toolchain zig@0.15.1")
}

func ExampleJob() {
	w, _ := workflow.Parse([]byte(`
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: npm ci && npm test
`))
	r := Job(Input{Workflow: w, Job: w.Jobs[0], Image: ubuntu24})
	for _, d := range r.Demands {
		fmt.Printf("%s %s %s: %s\n", d.Kind, d.Name, d.Version, d.Reason)
	}
	// Output: toolchain node 22.23.3: step 1 runs npm without setting up Node.js
}

func TestMissingFromEarlierRunBecomesDemand(t *testing.T) {
	w, err := workflow.Parse([]byte("on: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: ./scripts/release.sh\n"))
	if err != nil {
		t.Fatal(err)
	}
	in := Input{Workflow: w, Job: w.Jobs[0], Image: ubuntu24}
	assertDemands(t, Job(in), "")
	// jq is in the image; "no-such-tool" is not in the catalog and is ignored.
	in.Missing = []string{"jq", "no-such-tool"}
	r := Job(in)
	assertDemands(t, r, "package jq (implicit)")
	if !strings.Contains(r.Demands[0].Reason, "an earlier run of this job") {
		t.Errorf("reason = %q", r.Demands[0].Reason)
	}
}
