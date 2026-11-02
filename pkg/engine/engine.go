// Package engine runs a whole workflow on this machine's terms: the job graph
// (package jobgraph) with an executor that scans, provisions and runs each
// instance through package runner, or hands it to Options.Remote.
package engine

import (
	"context"
	"io/fs"
	"sync"

	"raccoonci/internal/catalog"
	"raccoonci/internal/jobgraph"
	"raccoonci/internal/missing"
	"raccoonci/internal/provision"
	"raccoonci/internal/runner"
	"raccoonci/internal/runnerimages"
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
	// Missing, if set, remembers commands a job could not find (exit 127), so
	// the next run of that job installs what provides them. WorkflowID tells
	// workflow files apart in it (their path in the repository).
	Missing    *missing.Store
	WorkflowID string

	// WorkDir gets one directory per job instance.
	WorkDir string
	// Runner is the configuration each job instance starts from.
	Runner runner.Config
	// Jobs limits the run to these jobs and the jobs they need.
	Jobs []string
	// MaxParallel caps how many job instances run at once (default 1).
	MaxParallel int
	// Unaffected, if set, is asked for each job whose own `if:` allows it to
	// run. Returning true leaves the job out because the changes do not
	// concern it, with the reason. Jobs that need it see it as succeeded.
	Unaffected func(j *workflow.Job) (reason string, skip bool)
	// Remote, if set, runs each job instance somewhere else (on another
	// machine) instead of here, and returns how it ended. It is called once
	// the instance holds a MaxParallel slot. The events of the instance reach
	// Runner.OnEvent through the caller's own plumbing; an error fails the
	// instance. The machine runs it with RunInstance.
	Remote func(ctx context.Context, inst RemoteInstance) (runner.Result, error)
}

type (
	// RemoteInstance is one job instance as the graph resolved it.
	RemoteInstance = jobgraph.RemoteInstance
	InstanceResult = jobgraph.InstanceResult
	JobResult      = jobgraph.JobResult
	Result         = jobgraph.Result
)

type engine struct {
	w    *workflow.Workflow
	opts Options
	mu   sync.Mutex
	seq  int
}

func newEngine(w *workflow.Workflow, opts Options) *engine {
	if opts.Catalog == nil {
		opts.Catalog = catalog.Default()
	}
	return &engine{w: w, opts: opts}
}

func (e *engine) graph() jobgraph.Options {
	o := e.opts
	g := jobgraph.Options{
		Github: o.Github, Vars: o.Vars, Inputs: o.Inputs, Catalog: o.Catalog,
		OnEvent: o.Runner.OnEvent, Jobs: o.Jobs, MaxParallel: o.MaxParallel,
		Unaffected: o.Unaffected, Remote: e.runHere,
	}
	if o.Remote != nil {
		g.Remote = e.runRemote
	}
	return g
}

// RunInstance runs one job instance (as Run does for each matrix combination,
// after it has resolved the matrix and the needs), for a machine that was sent
// just that. opts.Remote is ignored.
func RunInstance(ctx context.Context, w *workflow.Workflow, opts Options, job *workflow.Job, matrix, strategy, needs value.Object) InstanceResult {
	opts.Remote = nil
	e := newEngine(w, opts)
	return jobgraph.RunInstance(ctx, w, e.graph(), job, matrix, strategy, needs)
}

// Run runs the workflow's job graph.
func Run(ctx context.Context, w *workflow.Workflow, opts Options) (*Result, error) {
	return jobgraph.Run(ctx, w, newEngine(w, opts).graph())
}

func (e *engine) emit(ev runner.Event) {
	if e.opts.Runner.OnEvent != nil {
		e.opts.Runner.OnEvent(ev)
	}
}
