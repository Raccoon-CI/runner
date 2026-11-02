package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"

	"raccoonci/internal/missing"
	"raccoonci/internal/platform"
	"raccoonci/internal/runner"
	"raccoonci/internal/runnerimages"
	"raccoonci/internal/scan"
)

// imageFor is the runner image the instance's runs-on picks, if any.
func (e *engine) imageFor(inst RemoteInstance) *runnerimages.Defaults {
	if e.opts.Images == nil {
		return nil
	}
	for _, l := range platform.Labels(inst.RunsOn) {
		if image := e.opts.Images.ForLabel(e.opts.Catalog, l); image != nil {
			return image
		}
	}
	return nil
}

// runRemote hands the instance to Options.Remote. The machine repeats
// everything runHere does (provisioning, the runner) for this one instance; its
// events come back through Remote. The scan is repeated there; here it only
// names the toolchains, for the caller to decide whether an environment
// snapshot fits.
func (e *engine) runRemote(ctx context.Context, inst RemoteInstance) (runner.Result, error) {
	scanned := scan.Job(scan.Input{Workflow: e.w, Job: inst.Job, Context: inst.Context, Image: e.imageFor(inst), Repo: e.opts.Repo, Catalog: e.opts.Catalog})
	for _, d := range scanned.Demands {
		if d.Kind == scan.Toolchain && !slices.Contains(inst.Toolchains, d.Name) {
			inst.Toolchains = append(inst.Toolchains, d.Name)
		}
	}
	return e.opts.Remote(ctx, inst)
}

// runHere provisions and runs the instance in this process.
func (e *engine) runHere(ctx context.Context, inst RemoteInstance) (runner.Result, error) {
	j, name := inst.Job, inst.Name
	image := e.imageFor(inst)
	e.mu.Lock()
	e.seq++
	dir := filepath.Join(e.opts.WorkDir, fmt.Sprintf("%d-%s", e.seq, j.ID))
	e.mu.Unlock()
	cfg := e.opts.Runner
	cfg.WorkDir = dir
	if cfg.Catalog == nil {
		cfg.Catalog = e.opts.Catalog
	}
	if e.opts.Missing != nil {
		key := missing.Key(e.opts.WorkflowID, j.ID)
		cfg.OnMissing = func(cmd string) {
			if _, err := e.opts.Missing.Add(key, cmd); err != nil {
				e.emit(runner.Event{Kind: runner.Warning, Job: name, Text: fmt.Sprintf("could not remember that %s was missing: %v", cmd, err)})
			}
		}
	}
	if e.opts.Provision != nil {
		cfg.PrependPath = append(slices.Clone(cfg.PrependPath), e.provision(ctx, name, j, inst.Context, image)...)
	}
	r, err := runner.Run(ctx, cfg, &runner.Job{
		Workflow: e.w,
		Job:      j,
		Name:     name,
		Github:   e.opts.Github,
		Matrix:   inst.Matrix,
		Strategy: inst.Strategy,
		Needs:    inst.Needs,
		Inputs:   e.opts.Inputs,
		Vars:     e.opts.Vars,
		Secrets:  e.opts.Secrets,
		Image:    image,
	})
	if err != nil {
		return runner.Result{}, err
	}
	return *r, nil
}
