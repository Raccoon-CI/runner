// Package runner runs one job instance on the machine it is on, the way
// GitHub's runner does: run: steps through GitHub's shell command lines,
// JavaScript and composite actions, workflow commands (::set-output::,
// ::add-mask::, ...), file commands ($GITHUB_ENV, $GITHUB_OUTPUT, ...),
// step conditions, timeouts, and secret masking in logs.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"raccoonci/internal/actions"
	"raccoonci/internal/catalog"
	"raccoonci/internal/docker"
	"raccoonci/internal/expr"
	"raccoonci/internal/runevent"
	"raccoonci/internal/runnerimages"
	"raccoonci/internal/value"
	"raccoonci/internal/workflow"
)

// The names of what a runner says (events, conclusions, results) live in
// package runevent, which holds only data; they are re-exported here.
type (
	Conclusion = runevent.Conclusion
	EventKind  = runevent.EventKind
	Event      = runevent.Event
	Result     = runevent.Result
)

const (
	Success   = runevent.Success
	Failure   = runevent.Failure
	Cancelled = runevent.Cancelled
	Skipped   = runevent.Skipped
)

// Job is one job instance with the contexts known when it starts.
type Job struct {
	Workflow *workflow.Workflow
	Job      *workflow.Job
	Name     string // display name, e.g. "test (3.12)"

	// Github is the github context of the run (event, sha, ref, repository,
	// ...). The runner adds the job- and step-specific keys.
	Github   value.Object
	Matrix   value.Object
	Strategy value.Object
	Needs    value.Object
	Inputs   value.Object
	Vars     value.Object
	Secrets  map[string]string

	// Image is the GitHub image runs-on names, if any. It sets ImageOS and
	// ImageVersion, which some actions read.
	Image *runnerimages.Defaults
}

type Config struct {
	// WorkDir holds the job's workspace and temp directory.
	WorkDir string
	// ToolCache is $RUNNER_TOOL_CACHE, shared between jobs.
	ToolCache string
	// Actions downloads remote actions.
	Actions *actions.Store
	// Catalog says which action is the checkout; nil means catalog.Default().
	Catalog *catalog.Catalog
	// Node returns the node binary for a JavaScript action's runs.using
	// (node20, node24, ...).
	Node func(ctx context.Context, using string) (string, error)
	// NodeMusl is Node for job containers whose image uses musl (Alpine),
	// which the glibc builds cannot run on. Without it such a container
	// fails JavaScript actions with an explanation.
	NodeMusl func(ctx context.Context, using string) (string, error)
	// BaseEnv is the environment every step starts from; os.Environ() if nil.
	BaseEnv []string
	// Checkout, if set, replaces actions/checkout of the workflow's own
	// repository, e.g. to copy a local working tree. It reports false to let
	// the real action run.
	Checkout func(ctx context.Context, workspace string, with value.Object) (bool, error)
	// PrependPath directories go in front of every step's PATH, as if an
	// earlier step had added them to $GITHUB_PATH: where the provisioner put
	// the toolchains the job uses without setting them up.
	PrependPath []string
	// ServiceEnv is added to every step's environment: the URLs and tokens of
	// the runtime services (artifacts, cache, OIDC).
	ServiceEnv map[string]string
	// ContainerServices makes the runtime services reachable from a job
	// container, whose loopback is not the host's. It is called once the
	// job's docker network exists, with the host's address on that network
	// and the network's subnet, and returns the ServiceEnv for the
	// container's steps and a func that stops serving there. Without it, or
	// if it fails (a warning), container steps get ServiceEnv unchanged.
	ContainerServices func(gateway netip.Addr, subnet netip.Prefix) (env map[string]string, stop func(), err error)
	// Docker runs container jobs and service containers; nil means the docker
	// found on PATH, looked for only when a job needs it.
	Docker *docker.Client
	// Mounts are host directories (the Node.js runtimes, the downloaded
	// actions) that steps in a job container need to reach at the same path.
	// They are mounted read-only.
	Mounts     []string
	RunnerName string
	OnEvent    func(Event)
}

const (
	JobStarted   = runevent.JobStarted
	StepStarted  = runevent.StepStarted
	Output       = runevent.Output // a line of step output, with secrets masked
	Debug        = runevent.Debug
	Notice       = runevent.Notice
	Warning      = runevent.Warning
	Error        = runevent.Error
	GroupStart   = runevent.GroupStart
	GroupEnd     = runevent.GroupEnd
	StepFinished = runevent.StepFinished // Conclusion is set; Skipped steps get no StepStarted
	JobFinished  = runevent.JobFinished
)

const (
	defaultJobTimeout = 360 * time.Minute
	// cancelGrace is how long always() and post steps may run after a job is
	// cancelled or times out.
	cancelGrace = 5 * time.Minute
	// maxSummary is GitHub's limit per step.
	maxSummary = 1 << 20
)

type jobRun struct {
	cfg       Config
	job       *Job
	workDir   string
	workspace string
	temp      string
	toolCache string
	github    value.Object
	runner    value.Object
	masker    masker

	jobCtx context.Context // cancelled on cancellation or the job timeout

	mu          sync.Mutex // guards the fields below, for output goroutines
	env         value.Object
	path        []string // from $GITHUB_PATH, most recent first
	summary     strings.Builder
	failed      bool
	cancelled   bool
	timedOut    bool
	graceCtx    context.Context
	graceCancel context.CancelFunc
	posts       []*post
	pgids       []int
	actionIDs   map[string]int
	fileSeq     int
	remoteActs  map[string]string // uses -> downloaded repository directory
	preStates   map[*workflow.Step]map[string]string

	// Containers (see containers.go).
	dkr          *docker.Client
	builtImages  map[string]bool // Docker action images built from a Dockerfile (docker_action.go)
	rootless     bool
	network      string
	netMade      bool
	created      []string          // containers to remove, job container included
	jobCID       string            // the job container, if the job has one
	cBase        []string          // its environment: the image's, plus the spec's
	cServiceEnv  map[string]string // ServiceEnv as seen from the job container
	cServiceStop func()
	ctxContainer value.Object
	ctxServices  value.Object
	lookups      map[string]string
	nodeChecks   map[string]error // node binary -> why it cannot run in the job container, or nil
	muslChecked  bool
	musl         bool // the job container's image uses musl
}

// Run runs the job. Step failures are reported in the Result; the error is
// only for problems with the machine itself, like an unwritable WorkDir.
func Run(ctx context.Context, cfg Config, job *Job) (*Result, error) {
	if cfg.Catalog == nil {
		cfg.Catalog = catalog.Default()
	}
	r := &jobRun{cfg: cfg, job: job, actionIDs: map[string]int{}, remoteActs: map[string]string{}, path: slices.Clone(cfg.PrependPath)}
	if err := r.setupDirs(); err != nil {
		return nil, err
	}
	defer r.killLeftovers()
	defer r.stopContainers() // runs first: removing the containers kills what is in them
	defer func() {
		if r.graceCancel != nil {
			r.graceCancel()
		}
	}()
	return r.run(ctx), nil
}

func (r *jobRun) setupDirs() error {
	name := "workspace"
	if repo, ok := r.job.Github.Get("repository"); ok {
		if _, n, ok := strings.Cut(expr.ToString(repo), "/"); ok && n != "" && n != "." && n != ".." {
			name = n
		}
	}
	// Steps run in other directories, so every path they get is absolute.
	workDir, err := filepath.Abs(r.cfg.WorkDir)
	if err != nil {
		return err
	}
	r.workDir = workDir
	r.workspace = filepath.Join(workDir, name, name) // like the runner's _work/repo/repo
	r.temp = filepath.Join(workDir, "_temp")
	r.toolCache = r.cfg.ToolCache
	if r.toolCache == "" {
		r.toolCache = filepath.Join(workDir, "_tool")
	}
	if r.toolCache, err = filepath.Abs(r.toolCache); err != nil {
		return err
	}
	if err := os.RemoveAll(r.temp); err != nil {
		return err
	}
	for _, d := range []string{r.workspace, r.toolCache, filepath.Join(r.temp, "_github_workflow"), filepath.Join(r.temp, "_runner_file_commands")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (r *jobRun) run(ctx context.Context) *Result {
	j := r.job.Job
	r.emit(Event{Kind: JobStarted})
	res := &Result{Conclusion: Success}
	err := r.prepare()
	timeout := defaultJobTimeout
	if err == nil {
		var v float64
		if v, err = r.evalNumber(j.TimeoutMinutes, r.exprCtx(rootScope(r), r.env, nil)); err == nil && v > 0 {
			timeout = time.Duration(v * float64(time.Minute))
		}
	}
	jobCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r.jobCtx = jobCtx
	if err == nil {
		err = r.fetchActions(jobCtx)
	}
	if err == nil {
		err = r.startContainers(jobCtx)
	}
	if err != nil {
		r.emit(Event{Kind: Error, Text: err.Error()})
		r.stopContainers()
		res.Conclusion = Failure
		r.emit(Event{Kind: JobFinished, Conclusion: res.Conclusion})
		return res
	}

	top := rootScope(r)
	r.runPres(jobCtx, top)
	for _, s := range j.Steps {
		r.checkCancelled()
		r.runStep(jobCtx, top, s)
	}
	r.checkCancelled()
	r.runPosts(jobCtx)
	r.stopContainers()

	res.Outputs = r.jobOutputs(top)
	r.mu.Lock()
	res.Summary = r.summary.String()
	switch {
	case r.timedOut:
		res.Conclusion = Failure
	case r.cancelled:
		res.Conclusion = Cancelled
	case r.failed:
		res.Conclusion = Failure
	}
	r.mu.Unlock()
	if r.timedOut {
		r.emit(Event{Kind: Error, Text: fmt.Sprintf("The job has exceeded the maximum execution time of %s", timeout)})
	}
	r.emit(Event{Kind: JobFinished, Conclusion: res.Conclusion})
	return res
}

// prepare builds the contexts and the job's env and writes the event
// payload: the "Set up job" step on GitHub, together with fetchActions.
func (r *jobRun) prepare() error {
	j := r.job.Job
	for _, s := range r.job.Secrets {
		r.masker.add(s)
	}

	r.github = slices.Clone(r.job.Github)
	eventPath := filepath.Join(r.temp, "_github_workflow", "event.json")
	event, _ := r.github.Get("event")
	if event == nil {
		event = value.Object{}
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if err := os.WriteFile(eventPath, payload, 0o644); err != nil {
		return err
	}
	for k, v := range map[string]any{
		"job":        j.ID,
		"workspace":  r.workspace,
		"event_path": eventPath,
		"token":      r.job.Secrets["GITHUB_TOKEN"],
		"action":     "",
	} {
		r.github = r.github.Set(k, v)
	}
	r.runner = value.Object{
		{Key: "name", Value: r.cfg.RunnerName},
		{Key: "os", Value: "Linux"},
		{Key: "arch", Value: runnerArch()},
		{Key: "temp", Value: r.temp},
		{Key: "tool_cache", Value: r.toolCache},
		{Key: "workspace", Value: filepath.Dir(r.workspace)},
		{Key: "environment", Value: "self-hosted"},
		{Key: "debug", Value: map[bool]string{true: "1"}[r.debug()]},
	}

	// Workflow env, then job env, each evaluated with what it may see.
	var env value.Object
	for _, raw := range []any{r.job.Workflow.Env, j.Env} {
		ev, err := r.evalEnv(raw, r.exprCtx(rootScope(r), env, nil))
		if err != nil {
			return fmt.Errorf("env: %w", err)
		}
		for _, f := range ev {
			env = env.Set(f.Key, f.Value)
		}
	}
	r.env = env
	return nil
}

// fetchActions downloads the job's actions up front, as GitHub does, so a
// typo in a uses: fails the job before anything runs.
func (r *jobRun) fetchActions(ctx context.Context) error {
	for _, s := range r.job.Job.Steps {
		if !isRemote(s.Uses) || (r.cfg.Checkout != nil && r.isCheckout(s.Uses)) {
			continue // the Checkout hook may make the download unnecessary
		}
		if _, err := r.fetch(ctx, s.Uses); err != nil {
			return err
		}
	}
	return nil
}

func (r *jobRun) debug() bool {
	for _, src := range []map[string]string{r.job.Secrets, objectStrings(r.job.Vars)} {
		if strings.EqualFold(src["ACTIONS_STEP_DEBUG"], "true") {
			return true
		}
	}
	return false
}

func objectStrings(o value.Object) map[string]string {
	m := map[string]string{}
	for _, f := range o {
		m[f.Key] = expr.ToString(f.Value)
	}
	return m
}

func runnerArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "X64"
	case "arm64":
		return "ARM64"
	case "arm":
		return "ARM"
	case "386":
		return "X86"
	}
	return strings.ToUpper(runtime.GOARCH)
}

// checkCancelled notices cancellation (or the job timeout) between steps.
// From then on success() is false, and the steps that still run (always(),
// cancelled(), post steps) get a short grace period of their own.
func (r *jobRun) checkCancelled() {
	ctx := r.jobCtx
	if ctx.Err() == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancelled {
		return
	}
	r.cancelled = true
	r.timedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	r.graceCtx, r.graceCancel = context.WithTimeout(context.WithoutCancel(ctx), cancelGrace)
}

// stepContext is the context steps run under: the job's, or the grace period
// once the job is cancelled.
func (r *jobRun) stepContext(ctx context.Context) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.graceCtx != nil {
		return r.graceCtx
	}
	return ctx
}

func (r *jobRun) jobOutputs(top *scope) value.Object {
	var out value.Object
	ctx := r.exprCtx(top, r.currentEnv(), nil)
	for _, f := range r.job.Job.Outputs {
		v, err := expr.EvalValue(f.Value, ctx)
		if err != nil {
			r.emit(Event{Kind: Warning, Text: fmt.Sprintf("job output %q: %v", f.Key, err)})
			continue
		}
		s := expr.ToString(v)
		if r.masker.contains(s) {
			r.emit(Event{Kind: Warning, Text: fmt.Sprintf("Skip output '%s' since it may contain secret.", f.Key)})
			continue
		}
		out = append(out, value.Field{Key: f.Key, Value: s})
	}
	return out
}

func (r *jobRun) emit(e Event) {
	if r.cfg.OnEvent == nil {
		return
	}
	e.Time = time.Now()
	e.Job = r.job.Name
	e.Text = r.masker.mask(e.Text)
	for k, v := range e.Props {
		e.Props[k] = r.masker.mask(v)
	}
	r.cfg.OnEvent(e)
}

func (r *jobRun) currentEnv() value.Object {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.env)
}

// killLeftovers kills what steps left running in the background, as GitHub
// does at the end of a job.
func (r *jobRun) killLeftovers() {
	r.mu.Lock()
	pgids := slices.Clone(r.pgids)
	r.mu.Unlock()
	for _, pgid := range pgids {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

// evalNumber evaluates a numeric setting such as timeout-minutes.
func (r *jobRun) evalNumber(raw any, ctx *expr.Context) (float64, error) {
	if raw == nil {
		return 0, nil
	}
	v, err := expr.EvalValue(raw, ctx)
	if err != nil {
		return 0, err
	}
	return expr.ToNumber(v), nil
}

// evalEnv evaluates an env: mapping (or an expression producing one) into
// string values.
func (r *jobRun) evalEnv(raw any, ctx *expr.Context) (value.Object, error) {
	if raw == nil {
		return nil, nil
	}
	v, err := expr.EvalValue(raw, ctx)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(value.Object)
	if !ok {
		return nil, fmt.Errorf("expected a mapping, got %s", expr.ToString(v))
	}
	out := make(value.Object, 0, len(obj))
	for _, f := range obj {
		out = append(out, value.Field{Key: f.Key, Value: expr.ToString(f.Value)})
	}
	return out, nil
}

func secretsObject(m map[string]string) value.Object {
	var o value.Object
	for _, k := range slices.Sorted(maps.Keys(m)) {
		o = append(o, value.Field{Key: k, Value: m[k]})
	}
	return o
}
