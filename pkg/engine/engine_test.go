package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"raccoonci/internal/provision"
	"raccoonci/internal/runner"
	"raccoonci/internal/runnerimages"
	"raccoonci/internal/value"
	"raccoonci/internal/workflow"
)

type log struct {
	mu     sync.Mutex
	events []runner.Event
}

func (l *log) add(e runner.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

// jobs returns each job instance's final conclusion.
func (l *log) jobs() map[string]runner.Conclusion {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := map[string]runner.Conclusion{}
	for _, e := range l.events {
		if e.Kind == runner.JobFinished {
			m[e.Job] = e.Conclusion
		}
	}
	return m
}

func (l *log) output(job string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.events {
		if e.Kind == runner.Output && e.Job == job {
			out = append(out, e.Text)
		}
	}
	return out
}

func (l *log) errorsOf(job string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.events {
		if e.Kind == runner.Error && e.Job == job {
			out = append(out, e.Text)
		}
	}
	return strings.Join(out, "\n")
}

func runWorkflow(t *testing.T, src string, opts Options) (*Result, *log) {
	t.Helper()
	w, err := workflow.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	l := &log{}
	opts.WorkDir = t.TempDir()
	opts.Github = value.Object{{Key: "repository", Value: "octo/hello"}}
	opts.Runner = runner.Config{
		BaseEnv: []string{"PATH=" + os.Getenv("PATH")},
		OnEvent: l.add,
	}
	if opts.MaxParallel == 0 {
		opts.MaxParallel = 4
	}
	res, err := Run(context.Background(), w, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res, l
}

func TestNeedsOutputsAndDynamicMatrix(t *testing.T) {
	res, l := runWorkflow(t, `
on: push
jobs:
  setup:
    runs-on: ubuntu-latest
    outputs:
      versions: ${{ steps.v.outputs.versions }}
    steps:
      - id: v
        run: echo 'versions=["1.0", "2.0"]' >> "$GITHUB_OUTPUT"
  test:
    needs: setup
    runs-on: ubuntu-latest
    strategy:
      matrix:
        version: ${{ fromJSON(needs.setup.outputs.versions) }}
    steps:
      - run: echo "testing ${{ matrix.version }} (${{ strategy.job-index }}/${{ strategy.job-total }}) after ${{ needs.setup.result }}"
`, Options{})
	if res.Conclusion != runner.Success {
		t.Fatalf("conclusion %s: %v", res.Conclusion, l.jobs())
	}
	for i, v := range []string{"1.0", "2.0"} {
		name := "test (" + v + ")"
		want := fmt.Sprintf("testing %s (%d/2) after success", v, i)
		if got := l.output(name); !slices.Equal(got, []string{want}) {
			t.Errorf("%s: %q", name, got)
		}
	}
}

func TestFailurePropagation(t *testing.T) {
	res, l := runWorkflow(t, `
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: exit 1
  test:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - run: echo nope
  deploy:
    needs: test
    runs-on: ubuntu-latest
    steps:
      - run: echo nope
  report:
    needs: deploy
    if: failure()
    runs-on: ubuntu-latest
    steps:
      - run: echo "reporting, deploy was ${{ needs.deploy.result }}"
  cleanup:
    needs: [build, test]
    if: always()
    runs-on: ubuntu-latest
    steps:
      - run: echo cleanup
  flaky:
    runs-on: ubuntu-latest
    continue-on-error: true
    steps:
      - run: exit 1
  after-flaky:
    needs: flaky
    runs-on: ubuntu-latest
    steps:
      - run: echo after
`, Options{})
	want := map[string]runner.Conclusion{
		"build": runner.Failure, "test": runner.Skipped, "deploy": runner.Skipped,
		"report": runner.Success, "cleanup": runner.Success,
		"flaky": runner.Failure, "after-flaky": runner.Success,
	}
	if got := l.jobs(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("jobs:\n got %v\nwant %v", got, want)
	}
	if got := l.output("report"); !slices.Equal(got, []string{"reporting, deploy was skipped"}) {
		t.Errorf("report output %q", got)
	}
	if res.Conclusion != runner.Failure {
		t.Errorf("run conclusion %s", res.Conclusion)
	}
}

func TestFailFastAndUnavailableOS(t *testing.T) {
	start := time.Now()
	_, l := runWorkflow(t, `
on: push
jobs:
  test:
    runs-on: ${{ matrix.os }}
    strategy:
      matrix:
        os: [ubuntu-latest]
        n: [1, 2]
    steps:
      - run: |
          if [ "${{ matrix.n }}" = 1 ]; then exit 1; fi
          sleep 30
  lenient:
    runs-on: ${{ matrix.os }}
    strategy:
      fail-fast: false
      matrix:
        os: [macos-latest, ubuntu-latest]
    steps:
      - run: echo ran on ${{ matrix.os }}
`, Options{MaxParallel: 8})
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("fail-fast didn't cancel the sleeping instance (took %s)", d)
	}
	got := l.jobs()
	if got["test (ubuntu-latest, 1)"] != runner.Failure {
		t.Errorf("jobs = %v", got)
	}
	if c := got["test (ubuntu-latest, 2)"]; c != runner.Cancelled {
		t.Errorf("sleeping instance: %s, want cancelled", c)
	}
	if !strings.Contains(l.errorsOf("lenient (macos-latest)"), "no macOS runner") {
		t.Errorf("macOS error: %q", l.errorsOf("lenient (macos-latest)"))
	}
	if got["lenient (ubuntu-latest)"] != runner.Success {
		t.Errorf("fail-fast: false should keep the Linux instance: %v", got)
	}
}

// fakeToolchain "installs" a node that prints which version it is.
type fakeToolchain struct {
	installs atomic.Int32
	fail     bool
}

func (f *fakeToolchain) Resolve(_ context.Context, spec string) (string, error) { return spec, nil }
func (f *fakeToolchain) BinDirs() []string                                      { return []string{"bin"} }
func (f *fakeToolchain) Install(_ context.Context, version, dest string) error {
	f.installs.Add(1)
	if f.fail {
		return errors.New("mirror is down")
	}
	if err := os.MkdirAll(filepath.Join(dest, "bin"), 0o755); err != nil {
		return err
	}
	script := "#!/bin/sh\necho fake-node " + version + "\n"
	return os.WriteFile(filepath.Join(dest, "bin", "node"), []byte(script), 0o755)
}

func (l *log) has(kind runner.EventKind, substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		if e.Kind == kind && strings.Contains(e.Text, substr) {
			return true
		}
	}
	return false
}

func provisionOpts(t *testing.T, inst *fakeToolchain) Options {
	images, err := runnerimages.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		Images: images,
		Provision: &provision.Provisioner{
			ToolCache:  t.TempDir(),
			Installers: map[string]provision.Installer{"node": inst},
			LookPath:   func(string) (string, error) { return "/usr/bin/found", nil },
		},
	}
}

func TestProvisionsWhatTheJobUsesWithoutSettingUp(t *testing.T) {
	inst := &fakeToolchain{}
	opts := provisionOpts(t, inst)
	version := opts.Images.ForLabel(nil, "ubuntu-latest").Tools["node"]

	res, l := runWorkflow(t, `
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: node --version
`, opts)
	if res.Conclusion != runner.Success {
		t.Fatalf("conclusion %s: %v", res.Conclusion, l.jobs())
	}
	// GitHub's image has node preinstalled, so the job just calls it. The
	// provisioned one must win over whatever node is on this machine.
	want := []string{"installed node " + version, "fake-node " + version}
	if got := l.output("build"); !slices.Equal(got, want) {
		t.Errorf("output:\n got %q\nwant %q", got, want)
	}
	if inst.installs.Load() != 1 {
		t.Errorf("%d installs", inst.installs.Load())
	}
}

func TestProvisioningFailuresAreWarningsNotFailures(t *testing.T) {
	inst := &fakeToolchain{fail: true}
	res, l := runWorkflow(t, `
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: |
          node --version > /dev/null 2>&1 || true
          echo job ran
`, provisionOpts(t, inst))
	if res.Conclusion != runner.Success {
		t.Fatalf("a failed install must not fail the job: %s", res.Conclusion)
	}
	if !l.has(runner.Warning, "mirror is down") || !l.has(runner.Warning, "node") {
		t.Errorf("the failure should be shown as a warning: %+v", l.events)
	}
	if got := l.output("build"); !slices.Equal(got, []string{"job ran"}) {
		t.Errorf("the job should have run normally: %q", got)
	}
}

func TestSelectJobs(t *testing.T) {
	_, l := runWorkflow(t, `
on: push
jobs:
  a:
    runs-on: ubuntu-latest
    steps: [{run: echo a}]
  b:
    needs: a
    runs-on: ubuntu-latest
    steps: [{run: echo b}]
  c:
    runs-on: ubuntu-latest
    steps: [{run: echo c}]
`, Options{Jobs: []string{"b"}})
	if got := l.jobs(); fmt.Sprint(got) != "map[a:success b:success]" {
		t.Errorf("jobs = %v", got)
	}
}
