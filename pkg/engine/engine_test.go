package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"github.com/Raccoon-CI/raccoonci/internal/actions"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/provision"
	"github.com/Raccoon-CI/raccoonci/internal/runner"
	"github.com/Raccoon-CI/raccoonci/pkg/runnerimages"
	"github.com/Raccoon-CI/raccoonci/pkg/value"
	"github.com/Raccoon-CI/raccoonci/pkg/workflow"
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

// With Remote set the engine resolves matrices and needs but runs nothing
// itself: each instance goes to Remote, and what Remote returns (here: a
// machine standing in, using RunInstance) feeds later jobs' needs.
func TestRemoteGetsEachResolvedInstance(t *testing.T) {
	src := `
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
      - run: echo "testing ${{ matrix.version }} after ${{ needs.setup.result }}"
`
	w, err := workflow.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	machine := &log{}
	base := Options{
		WorkDir: t.TempDir(),
		Github:  value.Object{{Key: "repository", Value: "octo/hello"}},
		Runner:  runner.Config{BaseEnv: []string{"PATH=" + os.Getenv("PATH")}, OnEvent: machine.add},
	}
	var mu sync.Mutex
	var got []string
	opts := base
	opts.MaxParallel = 4
	opts.Runner.OnEvent = func(runner.Event) { t.Error("the controller ran something itself") }
	opts.Remote = func(ctx context.Context, in RemoteInstance) (runner.Result, error) {
		mu.Lock()
		got = append(got, in.Name)
		mu.Unlock()
		own := base // a machine of its own: its own work directory
		own.WorkDir = t.TempDir()
		r := RunInstance(ctx, w, own, in.Job, in.Matrix, in.Strategy, in.Needs)
		return runner.Result{Conclusion: r.Conclusion, Outputs: r.Outputs}, nil
	}
	res, err := Run(context.Background(), w, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Conclusion != runner.Success {
		t.Fatalf("conclusion %s: %v", res.Conclusion, machine.jobs())
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"setup", "test (1.0)", "test (2.0)"}) {
		t.Errorf("instances sent: %v", got)
	}
	if out := machine.output("test (2.0)"); !slices.Equal(out, []string{"testing 2.0 after success"}) {
		t.Errorf("test (2.0): %q", out)
	}
}

func TestRemoteErrorFailsTheInstance(t *testing.T) {
	w, _ := workflow.Parse([]byte("on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"))
	l := &log{}
	res, err := Run(context.Background(), w, Options{
		WorkDir: t.TempDir(),
		Github:  value.Object{{Key: "repository", Value: "octo/hello"}},
		Runner:  runner.Config{OnEvent: l.add},
		Remote: func(context.Context, RemoteInstance) (runner.Result, error) {
			return runner.Result{}, errors.New("the machine went away")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Conclusion != runner.Failure || !strings.Contains(l.errorsOf("a"), "the machine went away") {
		t.Errorf("conclusion %s, errors %q", res.Conclusion, l.errorsOf("a"))
	}
}

// A job left out because the changes do not concern it counts as succeeded for
// the jobs that need it, and nothing of it runs.
func TestUnaffectedJobIsLeftOutAndItsDependentsGoOn(t *testing.T) {
	res, l := runWorkflow(t, `
on: push
jobs:
  front:
    runs-on: ubuntu-latest
    steps: [{run: 'echo front ran'}]
  back:
    runs-on: ubuntu-latest
    steps: [{run: 'echo back ran'}]
  deploy:
    needs: [front, back]
    runs-on: ubuntu-latest
    steps: [{run: 'echo deploy after ${{ needs.front.result }} and ${{ needs.back.result }}'}]
`, Options{Unaffected: func(j *workflow.Job) (string, bool) {
		if j.ID == "front" {
			return "nothing it depends on changed", true
		}
		return "", false
	}})
	if res.Conclusion != runner.Success {
		t.Fatalf("conclusion %s: %v", res.Conclusion, l.jobs())
	}
	if got := l.output("front"); len(got) != 0 {
		t.Errorf("the unaffected job ran: %q", got)
	}
	if !l.has(runner.Notice, "not run: nothing it depends on changed") {
		t.Errorf("no notice: %+v", l.events)
	}
	if got := l.output("deploy"); !slices.Equal(got, []string{"deploy after success and success"}) {
		t.Errorf("deploy: %q", got)
	}
	for _, j := range res.Jobs {
		if (j.ID == "front") != (j.Unchanged != "") {
			t.Errorf("%s: Unchanged = %q", j.ID, j.Unchanged)
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

// An instance for an OS we don't have fails at once; it does not queue for a
// slot behind instances that can run. Instances take slots in no fixed order,
// so there are enough of them that some would queue behind the slow job.
func TestUnavailableOSDoesNotWaitForASlot(t *testing.T) {
	start := time.Now()
	var mu sync.Mutex
	var macDone time.Duration
	w, err := workflow.Parse([]byte(`
on: push
jobs:
  slow:
    runs-on: ubuntu-latest
    steps:
      - run: sleep 3
  mac:
    runs-on: macos-latest
    strategy:
      fail-fast: false
      matrix:
        n: [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30]
    steps:
      - run: echo never
`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), w, Options{
		WorkDir:     t.TempDir(),
		Github:      value.Object{{Key: "repository", Value: "octo/hello"}},
		MaxParallel: 1,
		Runner: runner.Config{
			BaseEnv: []string{"PATH=" + os.Getenv("PATH")},
			OnEvent: func(e runner.Event) {
				if e.Kind == runner.JobFinished && strings.HasPrefix(e.Job, "mac") {
					mu.Lock()
					macDone = max(macDone, time.Since(start))
					mu.Unlock()
				}
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if macDone == 0 || macDone > time.Second {
		t.Errorf("the last macOS instance was reported after %s; none should wait for the 3 s job holding the only slot", macDone)
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

// A composite action from the network calls node without setting it up. The
// workflow itself never mentions node, so only a scan that looks inside the
// action knows to install it.
func TestProvisionsWhatAThirdPartyActionNeeds(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	action := "name: tool\nruns:\n  using: composite\n  steps:\n    - run: node --version\n      shell: bash\n"
	tw.WriteHeader(&tar.Header{Name: "acme-tool-abc/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "acme-tool-abc/action.yml", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(action))})
	tw.Write([]byte(action))
	tw.Close()
	gz.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/acme/tool/tar.gz/v1" {
			w.Write(buf.Bytes())
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	store := actions.NewStore(t.TempDir(), "")
	store.CodeloadBase, store.APIBase = srv.URL, srv.URL

	inst := &fakeToolchain{}
	opts := provisionOpts(t, inst)
	version := opts.Images.ForLabel(nil, "ubuntu-latest").Tools["node"]
	w, err := workflow.Parse([]byte("on: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: acme/tool@v1\n"))
	if err != nil {
		t.Fatal(err)
	}
	l := &log{}
	opts.WorkDir = t.TempDir()
	opts.Github = value.Object{{Key: "repository", Value: "octo/hello"}}
	opts.Runner = runner.Config{BaseEnv: []string{"PATH=" + os.Getenv("PATH")}, OnEvent: l.add, Actions: store}
	res, err := Run(context.Background(), w, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Conclusion != runner.Success {
		t.Fatalf("conclusion %s: %v", res.Conclusion, l.jobs())
	}
	if got := l.output("build"); !slices.Contains(got, "fake-node "+version) {
		t.Errorf("the action's node was not the provisioned one: %q", got)
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
