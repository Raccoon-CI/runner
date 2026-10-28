package engine

import (
	"context"

	"raccoonci/internal/expr"
	"raccoonci/internal/provision"
	"raccoonci/internal/runner"
	"raccoonci/internal/runnerimages"
	"raccoonci/internal/scan"
	"raccoonci/internal/workflow"
)

const (
	setupStep       = "Set up environment"
	provisionFailed = provision.Failed
)

// provision scans a job instance for what it needs, provides it, and
// returns the directories to put in front of the job's PATH. Nothing here
// fails a job: the scan is a heuristic, so problems are reported as
// warnings, and the job still runs (it may not need what was missing).
func (e *engine) provision(ctx context.Context, name string, j *workflow.Job, ectx *expr.Context, image *runnerimages.Defaults) []string {
	needs := scan.Job(scan.Input{
		Workflow: e.w,
		Job:      j,
		Context:  ectx,
		Image:    image,
		Repo:     e.opts.Repo,
		Catalog:  e.opts.Catalog,
	})
	if len(needs.Demands) == 0 && len(needs.Warnings) == 0 {
		return nil
	}
	say := func(kind runner.EventKind, text string) {
		e.emit(runner.Event{Kind: kind, Job: name, Step: setupStep, Text: text})
	}
	e.emit(runner.Event{Kind: runner.StepStarted, Job: name, Step: setupStep})
	for _, w := range needs.Warnings {
		say(runner.Warning, w)
	}
	res := e.opts.Provision.Ensure(ctx, needs.Demands)
	for _, it := range res.Items {
		if it.Status == provisionFailed {
			say(runner.Warning, it.Describe())
		} else {
			say(runner.Output, it.Describe())
		}
	}
	e.emit(runner.Event{Kind: runner.StepFinished, Job: name, Step: setupStep, Conclusion: runner.Success})
	return res.PathDirs
}
