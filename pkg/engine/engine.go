// Package engine runs a whole workflow: it orders jobs by needs, evaluates
// job conditions, expands matrices (including ones built from earlier jobs'
// outputs), and runs each job instance through package runner.
package engine

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"raccoonci/internal/catalog"
	"raccoonci/internal/expr"
	"raccoonci/internal/platform"
	"raccoonci/internal/provision"
	"raccoonci/internal/runner"
	"raccoonci/internal/runnerimages"
	"raccoonci/internal/scan"
	"raccoonci/internal/value"
	"raccoonci/internal/workflow"
)

type Options struct {
	// Github is the run's github context, without the per-job keys.
	Github  value.Object
	Secrets map[string]string
	Vars    value.Object
	Inputs  value.Object
	Images  *runnerimages.Snapshot
	// Catalog defaults to catalog.Default().
	Catalog *catalog.Catalog
	// Provision, if set, installs what each job instance needs (as found by
	// scanning it) before it runs. Repo is the repository at the
	// triggering commit, for the scan to read version files from; use an
	// os.OpenRoot FS for untrusted repos.
	Provision *provision.Provisioner
	Repo      fs.FS

	// WorkDir gets one directory per job instance.
	WorkDir string
	// Runner is the configuration each job instance starts from.
	Runner runner.Config
	// Jobs limits the run to these jobs and the jobs they need.
	Jobs []string
	// MaxParallel caps how many job instances run at once (default 1).
	MaxParallel int
	// Remote, if set, runs each job instance somewhere else (on another
	// machine) instead of here, and returns how it ended. It is called once
	// the instance holds a MaxParallel slot. The events of the instance reach
	// Runner.OnEvent through the caller's own plumbing; an error fails the
	// instance. The machine runs it with RunInstance.
	Remote func(ctx context.Context, inst RemoteInstance) (runner.Result, error)
}

// RemoteInstance is one job instance as the engine resolved it: the inputs a
// machine needs, besides the workflow and the run's contexts, to run it.
type RemoteInstance struct {
	Job      *workflow.Job
	Name     string
	Matrix   value.Object
	Strategy value.Object
	Needs    value.Object
	// Toolchains are the names of the toolchains the instance uses, as the
	// scan found them (sorted by first use).
	Toolchains []string
}

// InstanceResult is how one instance ended.
type InstanceResult struct {
	Conclusion runner.Conclusion
	Outputs    value.Object
}

// RunInstance runs one job instance (as the engine does for each matrix
// combination, after it has resolved the matrix and the needs), for a machine
// that was sent just that. opts.Remote is ignored.
func RunInstance(ctx context.Context, w *workflow.Workflow, opts Options, job *workflow.Job, matrix, strategy, needs value.Object) InstanceResult {
	opts.Remote = nil
	if opts.MaxParallel < 1 {
		opts.MaxParallel = 1
	}
	if opts.Catalog == nil {
		opts.Catalog = catalog.Default()
	}
	e := &engine{w: w, opts: opts, sem: make(chan struct{}, opts.MaxParallel),
		done: map[string]chan struct{}{}, res: map[string]*JobResult{}}
	r := e.runInstance(ctx, job, matrix, strategy, needs)
	return InstanceResult{Conclusion: r.conclusion, Outputs: r.outputs}
}

type JobResult struct {
	ID         string
	Conclusion runner.Conclusion
	Outputs    value.Object
}

type Result struct {
	Conclusion runner.Conclusion
	Jobs       []*JobResult // in workflow order
}

type engine struct {
	w    *workflow.Workflow
	opts Options
	sem  chan struct{}
	seq  int
	mu   sync.Mutex
	done map[string]chan struct{}
	res  map[string]*JobResult
}

func Run(ctx context.Context, w *workflow.Workflow, opts Options) (*Result, error) {
	selected, err := selectJobs(w, opts.Jobs)
	if err != nil {
		return nil, err
	}
	if opts.MaxParallel < 1 {
		opts.MaxParallel = 1
	}
	if opts.Catalog == nil {
		opts.Catalog = catalog.Default()
	}
	e := &engine{
		w: w, opts: opts,
		sem:  make(chan struct{}, opts.MaxParallel),
		done: map[string]chan struct{}{},
		res:  map[string]*JobResult{},
	}
	for _, j := range selected {
		e.done[j.ID] = make(chan struct{})
	}
	var wg sync.WaitGroup
	for _, j := range selected {
		wg.Go(func() {
			defer close(e.done[j.ID])
			r := e.runJob(ctx, j)
			e.mu.Lock()
			e.res[j.ID] = r
			e.mu.Unlock()
		})
	}
	wg.Wait()

	out := &Result{Conclusion: runner.Success}
	for _, j := range selected {
		r := e.res[j.ID]
		out.Jobs = append(out.Jobs, r)
		switch r.Conclusion {
		case runner.Failure:
			out.Conclusion = runner.Failure
		case runner.Cancelled:
			if out.Conclusion == runner.Success {
				out.Conclusion = runner.Cancelled
			}
		}
	}
	return out, nil
}

// selectJobs returns the named jobs plus everything they need, in workflow
// order; all jobs when none are named.
func selectJobs(w *workflow.Workflow, ids []string) ([]*workflow.Job, error) {
	if len(ids) == 0 {
		return w.Jobs, nil
	}
	want := map[string]bool{}
	var add func(id string) error
	add = func(id string) error {
		j := w.Job(id)
		if j == nil {
			return fmt.Errorf("no job %q in the workflow", id)
		}
		if want[id] {
			return nil
		}
		want[id] = true
		for _, n := range j.Needs {
			if err := add(n); err != nil {
				return err
			}
		}
		return nil
	}
	for _, id := range ids {
		if err := add(id); err != nil {
			return nil, err
		}
	}
	var out []*workflow.Job
	for _, j := range w.Jobs {
		if want[j.ID] {
			out = append(out, j)
		}
	}
	return out, nil
}

func (e *engine) emit(ev runner.Event) {
	if e.opts.Runner.OnEvent != nil {
		e.opts.Runner.OnEvent(ev)
	}
}

func (e *engine) result(id string) *JobResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.res[id]
}

// ancestorFailed reports whether any job j depends on, directly or not,
// failed: failure() is true for the whole chain below a failed job.
func (e *engine) ancestorFailed(j *workflow.Job) bool {
	for _, n := range j.Needs {
		if e.result(n).Conclusion == runner.Failure || e.ancestorFailed(e.w.Job(n)) {
			return true
		}
	}
	return false
}

func (e *engine) runJob(ctx context.Context, j *workflow.Job) *JobResult {
	res := &JobResult{ID: j.ID, Conclusion: runner.Skipped}
	needs := value.Object{}
	skippedNeed := false
	for _, n := range j.Needs {
		select {
		case <-e.done[n]:
		case <-ctx.Done():
			<-e.done[n]
		}
		r := e.result(n)
		if r.Conclusion != runner.Success {
			skippedNeed = true
		}
		outputs := r.Outputs
		if outputs == nil {
			outputs = value.Object{}
		}
		needs = append(needs, value.Field{Key: n, Value: value.Object{
			{Key: "result", Value: string(r.Conclusion)},
			{Key: "outputs", Value: outputs},
		}})
	}

	ectx := &expr.Context{Contexts: map[string]any{
		"github": e.opts.Github,
		"needs":  needs,
		"vars":   orEmpty(e.opts.Vars),
		"inputs": orEmpty(e.opts.Inputs),
	}, Status: &expr.Status{
		Failed:    e.ancestorFailed(j),
		Cancelled: ctx.Err() != nil,
		Skipped:   skippedNeed,
	}}
	fail := func(format string, args ...any) *JobResult {
		e.emit(runner.Event{Kind: runner.Error, Job: j.ID, Text: fmt.Sprintf(format, args...)})
		res.Conclusion = runner.Failure
		return res
	}
	ok, err := expr.EvalCondition(j.If, ectx)
	if err != nil {
		return fail("if: %v", err)
	}
	if !ok {
		e.emit(runner.Event{Kind: runner.JobFinished, Job: j.ID, Conclusion: runner.Skipped})
		return res
	}
	if j.Uses != "" {
		return fail("calling reusable workflows (%s) isn't supported yet", j.Uses)
	}
	ectx.Status = nil

	var rawMatrix any
	failFast, maxParallel := any(true), any(nil)
	if s := j.Strategy; s != nil {
		rawMatrix = s.Matrix
		if s.FailFast != nil {
			failFast = s.FailFast
		}
		maxParallel = s.MaxParallel
	}
	m, err := expr.EvalValue(rawMatrix, ectx)
	if err != nil {
		return fail("strategy.matrix: %v", err)
	}
	combos, err := workflow.ExpandMatrix(m)
	if err != nil {
		return fail("strategy.matrix: %v", err)
	}
	if ff, err := expr.EvalValue(failFast, ectx); err == nil {
		failFast = expr.Truthy(ff)
	}
	limit := len(combos)
	if maxParallel != nil {
		mp, err := expr.EvalValue(maxParallel, ectx)
		if err != nil {
			return fail("strategy.max-parallel: %v", err)
		}
		if n := int(expr.ToNumber(mp)); n > 0 && n < limit {
			limit = n
		}
	}

	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	jobSem := make(chan struct{}, limit)
	results := make([]instanceResult, len(combos))
	var wg sync.WaitGroup
	for i, combo := range combos {
		strategy := value.Object{
			{Key: "fail-fast", Value: failFast},
			{Key: "job-index", Value: float64(i)},
			{Key: "job-total", Value: float64(len(combos))},
			{Key: "max-parallel", Value: float64(limit)},
		}
		wg.Go(func() {
			jobSem <- struct{}{}
			defer func() { <-jobSem }()
			r := e.runInstance(jobCtx, j, combo, strategy, needs)
			results[i] = r
			if r.conclusion == runner.Failure && !r.continueOnError && failFast == true {
				cancelJob()
			}
		})
	}
	wg.Wait()

	res.Conclusion = runner.Success
	for _, r := range results {
		switch {
		case r.conclusion == runner.Failure && !r.continueOnError:
			res.Conclusion = runner.Failure
		case r.conclusion == runner.Cancelled && res.Conclusion == runner.Success:
			res.Conclusion = runner.Cancelled
		}
		// For matrix jobs, the last instance to set an output wins, as on
		// GitHub, where the order isn't defined either.
		for _, f := range r.outputs {
			if f.Value != "" {
				res.Outputs = res.Outputs.Set(f.Key, f.Value)
			}
		}
	}
	return res
}

func orEmpty(o value.Object) value.Object {
	if o == nil {
		return value.Object{}
	}
	return o
}

type instanceResult struct {
	conclusion      runner.Conclusion
	continueOnError bool
	outputs         value.Object
}

func (e *engine) runInstance(ctx context.Context, j *workflow.Job, combo, strategy, needs value.Object) instanceResult {
	ectx := &expr.Context{Contexts: map[string]any{
		"github":   e.opts.Github,
		"needs":    needs,
		"vars":     orEmpty(e.opts.Vars),
		"inputs":   orEmpty(e.opts.Inputs),
		"matrix":   combo,
		"strategy": strategy,
	}}
	name := instanceName(j, combo, ectx)
	res := instanceResult{conclusion: runner.Failure}
	fail := func(format string, args ...any) instanceResult {
		e.emit(runner.Event{Kind: runner.Error, Job: name, Text: fmt.Sprintf(format, args...)})
		e.emit(runner.Event{Kind: runner.JobFinished, Job: name, Conclusion: runner.Failure})
		return res
	}
	if v, err := expr.EvalValue(j.ContinueOnError, ectx); err == nil {
		res.continueOnError = expr.Truthy(v)
	}
	// A run that cannot happen here fails at once, without waiting for a slot
	// behind runs that can (unless fail-fast has already cancelled it).
	runsOn, err := expr.EvalValue(j.RunsOn, ectx)
	if err != nil {
		return fail("runs-on: %v", err)
	}
	cat := e.opts.Catalog
	if os := platform.FromRunsOn(cat, runsOn); os != "" && !cat.OSAvailable(os) && ctx.Err() == nil {
		return fail("%s", cat.OSUnavailable(os))
	}

	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		e.emit(runner.Event{Kind: runner.JobFinished, Job: name, Conclusion: runner.Cancelled})
		res.conclusion = runner.Cancelled
		return res
	}
	if ctx.Err() != nil { // fail-fast cancelled it while it waited
		e.emit(runner.Event{Kind: runner.JobFinished, Job: name, Conclusion: runner.Cancelled})
		res.conclusion = runner.Cancelled
		return res
	}

	var image *runnerimages.Defaults
	if e.opts.Images != nil {
		for _, l := range platform.Labels(runsOn) {
			if image = e.opts.Images.ForLabel(cat, l); image != nil {
				break
			}
		}
	}

	if e.opts.Remote != nil {
		// The machine repeats everything below (provisioning, the runner) for
		// this one instance; its events come back through Remote. The scan
		// is repeated there; here it only names the toolchains, for the
		// caller to decide whether an environment snapshot fits.
		inst := RemoteInstance{Job: j, Name: name, Matrix: combo, Strategy: strategy, Needs: needs}
		scanned := scan.Job(scan.Input{Workflow: e.w, Job: j, Context: ectx, Image: image, Repo: e.opts.Repo, Catalog: cat})
		for _, d := range scanned.Demands {
			if d.Kind == scan.Toolchain && !slices.Contains(inst.Toolchains, d.Name) {
				inst.Toolchains = append(inst.Toolchains, d.Name)
			}
		}
		r, err := e.opts.Remote(ctx, inst)
		if err != nil {
			return fail("%v", err)
		}
		res.conclusion, res.outputs = r.Conclusion, r.Outputs
		return res
	}

	e.mu.Lock()
	e.seq++
	dir := filepath.Join(e.opts.WorkDir, fmt.Sprintf("%d-%s", e.seq, j.ID))
	e.mu.Unlock()
	cfg := e.opts.Runner
	cfg.WorkDir = dir
	if cfg.Catalog == nil {
		cfg.Catalog = cat
	}
	if e.opts.Provision != nil {
		cfg.PrependPath = append(slices.Clone(cfg.PrependPath), e.provision(ctx, name, j, ectx, image)...)
	}
	r, err := runner.Run(ctx, cfg, &runner.Job{
		Workflow: e.w,
		Job:      j,
		Name:     name,
		Github:   e.opts.Github,
		Matrix:   combo,
		Strategy: strategy,
		Needs:    needs,
		Inputs:   e.opts.Inputs,
		Vars:     e.opts.Vars,
		Secrets:  e.opts.Secrets,
		Image:    image,
	})
	if err != nil {
		return fail("%v", err)
	}
	res.conclusion, res.outputs = r.Conclusion, r.Outputs
	return res
}

// instanceName is the job's name: its name: (which may use matrix values),
// or GitHub's default of the id followed by the matrix values.
func instanceName(j *workflow.Job, combo value.Object, ctx *expr.Context) string {
	if j.Name != "" {
		if v, err := expr.Interpolate(j.Name, ctx); err == nil {
			return expr.ToString(v)
		}
		return j.Name
	}
	if len(combo) == 0 {
		return j.ID
	}
	var vals []string
	var leaves func(v any)
	leaves = func(v any) {
		switch v := v.(type) {
		case value.Object:
			for _, f := range v {
				leaves(f.Value)
			}
		case []any:
			for _, e := range v {
				leaves(e)
			}
		default:
			vals = append(vals, expr.ToString(v))
		}
	}
	leaves(combo)
	return j.ID + " (" + strings.Join(vals, ", ") + ")"
}
