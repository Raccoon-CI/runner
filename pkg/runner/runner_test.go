package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/docker"
	"github.com/Raccoon-CI/raccoonci/internal/value"
	"github.com/Raccoon-CI/raccoonci/internal/workflow"
)

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) add(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// output returns the step output lines, joined by newlines.
func (r *recorder) output() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var lines []string
	for _, e := range r.events {
		if e.Kind == Output {
			lines = append(lines, e.Text)
		}
	}
	return strings.Join(lines, "\n")
}

// dump lists every event, errors and warnings included, for failure messages.
func (r *recorder) dump() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var lines []string
	for _, e := range r.events {
		switch e.Kind {
		case Output:
			lines = append(lines, e.Text)
		case Error:
			lines = append(lines, "ERROR: "+e.Text)
		case Warning:
			lines = append(lines, "WARNING: "+e.Text)
		}
	}
	return strings.Join(lines, "\n")
}

func (r *recorder) has(kind EventKind, substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Kind == kind && strings.Contains(e.Text, substr) {
			return true
		}
	}
	return false
}

// conclusions maps step names to their conclusions.
func (r *recorder) conclusions() map[string]Conclusion {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := map[string]Conclusion{}
	for _, e := range r.events {
		if e.Kind == StepFinished {
			m[e.Step] = e.Conclusion
		}
	}
	return m
}

type testJob struct {
	yaml     string
	secrets  map[string]string
	files    map[string]string // written into the workspace by actions/checkout
	ctx      context.Context
	configFn func(*Config)
}

func run(t *testing.T, tj testJob) (*Result, *recorder) {
	t.Helper()
	w, err := workflow.Parse([]byte(tj.yaml))
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	cfg := Config{
		WorkDir: t.TempDir(),
		BaseEnv: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()},
		OnEvent: rec.add,
		Node: func(context.Context, string) (string, error) {
			return exec.LookPath("node")
		},
		Checkout: func(_ context.Context, ws string, _ value.Object) (bool, error) {
			for name, data := range tj.files {
				p := filepath.Join(ws, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return false, err
				}
				if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
					return false, err
				}
			}
			return true, nil
		},
	}
	if tj.configFn != nil {
		tj.configFn(&cfg)
	}
	ctx := tj.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := Run(ctx, cfg, &Job{
		Workflow: w,
		Job:      w.Jobs[0],
		Name:     w.Jobs[0].ID,
		Github: value.Object{
			{Key: "repository", Value: "octo/hello"},
			{Key: "sha", Value: "abc123"},
			{Key: "event_name", Value: "push"},
			{Key: "event", Value: value.Object{{Key: "ref", Value: "refs/heads/main"}}},
		},
		Secrets: tj.secrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res, rec
}

func TestOutputsEnvAndPath(t *testing.T) {
	res, rec := run(t, testJob{yaml: `
on: push
env:
  LEVEL: workflow
  OVERRIDE: workflow
jobs:
  j:
    runs-on: ubuntu-latest
    env:
      OVERRIDE: job
    outputs:
      greeting: ${{ steps.produce.outputs.greeting }}
      multi: ${{ steps.produce.outputs.multi }}
    steps:
      - id: produce
        run: |
          echo "greeting=hello" >> "$GITHUB_OUTPUT"
          { echo "multi<<EOF"; echo "a"; echo "b"; echo "EOF"; } >> "$GITHUB_OUTPUT"
          echo "FROM_ENV_FILE=yes" >> "$GITHUB_ENV"
          mkdir -p "$RUNNER_TEMP/bin"
          printf '#!/bin/sh\necho tool-ran\n' > "$RUNNER_TEMP/bin/mytool"
          chmod +x "$RUNNER_TEMP/bin/mytool"
          echo "$RUNNER_TEMP/bin" >> "$GITHUB_PATH"
      - env:
          STEP: step
          FROM_EXPR: ${{ steps.produce.outputs.greeting }}-${{ env.OVERRIDE }}
        run: |
          echo "$LEVEL $OVERRIDE $STEP $FROM_EXPR $FROM_ENV_FILE"
          mytool
          echo "ws=$(basename "$GITHUB_WORKSPACE") ci=$CI actions=$GITHUB_ACTIONS sha=$GITHUB_SHA os=$RUNNER_OS"
          test "$(pwd)" = "$GITHUB_WORKSPACE"
          grep -q refs/heads/main "$GITHUB_EVENT_PATH"
`})
	if res.Conclusion != Success {
		t.Fatalf("conclusion %s; events:\n%s", res.Conclusion, rec.output())
	}
	want := "workflow job step hello-job yes\ntool-ran\nws=hello ci=true actions=true sha=abc123 os=Linux"
	if got := rec.output(); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
	if got := fmt.Sprint(res.Outputs); got != "[{greeting hello} {multi a\nb}]" {
		t.Errorf("outputs = %s", got)
	}
}

func TestConditionsAndContinueOnError(t *testing.T) {
	res, rec := run(t, testJob{yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - id: soft
        name: soft failure
        run: exit 3
        continue-on-error: true
      - name: after soft
        run: echo "outcome=${{ steps.soft.outcome }} conclusion=${{ steps.soft.conclusion }}"
      - name: hard failure
        run: |
          echo partial=1 >> "$GITHUB_OUTPUT"
          false
        id: hard
      - name: skipped
        run: echo should not run
      - name: on failure
        if: failure()
        run: echo "failure ran, partial=${{ steps.hard.outputs.partial }}"
      - name: always
        if: always() && job.status == 'failure'
        run: echo always ran
      - name: plain expression is gated by success
        if: true
        run: echo nope
`})
	if res.Conclusion != Failure {
		t.Errorf("conclusion = %s", res.Conclusion)
	}
	want := map[string]Conclusion{
		"soft failure": Success, "after soft": Success, "hard failure": Failure, "skipped": Skipped,
		"on failure": Success, "always": Success, "plain expression is gated by success": Skipped,
	}
	if got := rec.conclusions(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("conclusions:\n got %v\nwant %v", got, want)
	}
	if got := rec.output(); got != "outcome=failure conclusion=success\nfailure ran, partial=1\nalways ran" {
		t.Errorf("output:\n%s", got)
	}
	if !rec.has(Error, "exit code 3") {
		t.Error("missing exit code error")
	}
}

func TestMaskingAndCommands(t *testing.T) {
	_, rec := run(t, testJob{
		secrets: map[string]string{"TOKEN": "s3cr3t-value"},
		yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - id: s
        run: |
          echo "token is ${{ secrets.TOKEN }}"
          echo "::add-mask::generated-pw"
          echo "pw is generated-pw"
          echo "::error file=app.js,line=3::bad generated-pw"
          echo "::set-output name=old::still works"
          echo "::stop-commands::pause123"
          echo "::error::not a command now"
          echo "::pause123::"
          echo "::group::Title"
          echo "::endgroup::"
          echo "::debug::dbg"
      - run: echo "${{ steps.s.outputs.old }}"
`})
	want := "token is ***\npw is ***\n::error::not a command now\nstill works"
	if got := rec.output(); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
	if !rec.has(Error, "bad ***") || !rec.has(GroupStart, "Title") || !rec.has(Debug, "dbg") {
		t.Errorf("missing command events: %+v", rec.events)
	}
	if !rec.has(Warning, "set-output` command is deprecated") {
		t.Error("missing set-output deprecation warning")
	}
}

func TestDisabledCommandsAndNodeOptions(t *testing.T) {
	res, rec := run(t, testJob{yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - name: set-env
        run: echo "::set-env name=X::1"
      - name: node options
        if: always()
        run: echo "NODE_OPTIONS=--require /tmp/evil.js" >> "$GITHUB_ENV"
`})
	if res.Conclusion != Failure {
		t.Errorf("conclusion = %s", res.Conclusion)
	}
	if c := rec.conclusions(); c["set-env"] != Failure || c["node options"] != Failure {
		t.Errorf("conclusions = %v", c)
	}
	if !rec.has(Error, "`set-env` command is disabled") || !rec.has(Error, "NODE_OPTIONS") {
		t.Error("missing errors")
	}
}

func TestShells(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	res, rec := run(t, testJob{yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    defaults:
      run:
        shell: sh
    steps:
      - run: test -z "$BASH_VERSION" && echo "sh $(basename "$0" | sed 's/[0-9]*//g')"
      - shell: python
        run: print("py", 1 + 1)
      - shell: bash -c "echo custom; . {0}"
        run: echo sourced
      - shell: bash
        run: |
          false | true
          echo pipefail-off-would-print-this
`})
	if res.Conclusion != Failure {
		t.Errorf("bash should fail through pipefail, got %s", res.Conclusion)
	}
	if got, want := rec.output(), "sh script_.sh\npy 2\ncustom\nsourced"; got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
}

func TestStepTimeoutAndBackgroundProcesses(t *testing.T) {
	start := time.Now()
	res, rec := run(t, testJob{yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - name: background
        run: sleep 60 &
      - name: slow
        timeout-minutes: 0.02
        run: sleep 60
      - name: after timeout
        if: always()
        run: echo still here
`})
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("took %s: a background process or the timeout blocked the job", d)
	}
	c := rec.conclusions()
	if c["background"] != Success || c["slow"] != Failure || c["after timeout"] != Success {
		t.Errorf("conclusions = %v", c)
	}
	if res.Conclusion != Failure || !rec.has(Error, "timed out") {
		t.Errorf("conclusion %s, timeout error reported: %v", res.Conclusion, rec.has(Error, "timed out"))
	}
}

func TestCancelRunsAlwaysSteps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	res, rec := run(t, testJob{ctx: ctx, yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - name: long
        run: sleep 60
      - name: normal
        run: echo nope
      - name: cleanup
        if: always()
        run: echo "cleaning up, status=${{ job.status }}"
`})
	if res.Conclusion != Cancelled {
		t.Errorf("conclusion = %s", res.Conclusion)
	}
	c := rec.conclusions()
	if c["long"] != Cancelled || c["normal"] != Skipped || c["cleanup"] != Success {
		t.Errorf("conclusions = %v", c)
	}
	if rec.output() != "cleaning up, status=cancelled" {
		t.Errorf("output = %q", rec.output())
	}
}

func TestLocalActions(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("no node")
	}
	res, rec := run(t, testJob{
		files: map[string]string{
			"js/action.yml": `
name: js
inputs:
  who: {default: "${{ github.repository }}"}
  extra input: {default: x}
outputs:
  said: {description: what it said}
runs:
  using: node24
  pre: pre.js
  main: main.js
  post: post.js
`,
			"js/pre.js": `const fs = require('fs');
fs.appendFileSync(process.env.GITHUB_STATE, 'from_pre=1\n');
console.log('pre');`,
			"js/main.js": `const fs = require('fs');
console.log('hello ' + process.env.INPUT_WHO + ' ' + process.env.INPUT_EXTRA_INPUT + ' state=' + process.env.STATE_from_pre);
fs.appendFileSync(process.env.GITHUB_OUTPUT, 'said=hi\n');
fs.appendFileSync(process.env.GITHUB_STATE, 'from_main=2\n');`,
			"js/post.js": `console.log('post state=' + process.env.STATE_from_main);`,
			"comp/action.yml": `
name: comp
inputs:
  name: {required: true}
outputs:
  result:
    value: ${{ steps.inner.outputs.r }}
runs:
  using: composite
  steps:
    - id: inner
      shell: bash
      run: |
        echo "inner ${{ inputs.name }} $CALLER_ENV $(basename "$GITHUB_ACTION_PATH")"
        echo "r=composite-${{ inputs.name }}" >> "$GITHUB_OUTPUT"
    - uses: ./js
      with:
        who: nested
`,
		},
		yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - id: js
        uses: ./js
        with:
          unknown: 1
      - id: comp
        uses: ./comp
        env:
          CALLER_ENV: from-caller
        with:
          name: World
      - run: echo "${{ steps.js.outputs.said }} ${{ steps.comp.outputs.result }}"
`})
	if res.Conclusion != Success {
		t.Fatalf("conclusion = %s\n%s\n%+v", res.Conclusion, rec.output(), rec.events)
	}
	// Local actions have no pre (they aren't checked out yet when pre runs).
	want := strings.Join([]string{
		"hello octo/hello x state=undefined",
		"inner World from-caller comp",
		"hello nested x state=undefined",
		"hi composite-World",
		"post state=2",
		"post state=2",
	}, "\n")
	if got := rec.output(); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
	if !rec.has(Warning, "Unexpected input(s) 'unknown'") {
		t.Error("missing unexpected input warning")
	}
}

// A job that needs a container cannot run without docker: it fails before
// any step, and says why, instead of running its steps on the host.
func TestContainerJobWithoutDockerFailsBeforeSteps(t *testing.T) {
	for name, yaml := range map[string]string{
		"container": "container: node:22",
		"services":  "services:\n      db:\n        image: redis",
	} {
		res, rec := run(t, testJob{
			yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    ` + yaml + `
    steps:
      - run: echo ran-on-the-host
`,
			configFn: func(c *Config) { c.Docker = &docker.Client{Bin: "/nonexistent/docker"} },
		})
		if res.Conclusion != Failure || !rec.has(Error, "docker") {
			t.Errorf("%s: conclusion %s, output %s", name, res.Conclusion, rec.output())
		}
		if rec.has(Output, "ran-on-the-host") {
			t.Errorf("%s: the steps ran on the host", name)
		}
	}
}
