package runnercmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Raccoon-CI/lib/pkg/catalog"
	"github.com/Raccoon-CI/lib/pkg/runnerimages"
	"github.com/Raccoon-CI/lib/pkg/value"
	"github.com/Raccoon-CI/lib/pkg/wire"
	"github.com/Raccoon-CI/lib/pkg/workflow"
	"github.com/Raccoon-CI/runner/internal/local"
	"github.com/Raccoon-CI/runner/pkg/actions"
	"github.com/Raccoon-CI/runner/pkg/cliflag"
	"github.com/Raccoon-CI/runner/pkg/engine"
	"github.com/Raccoon-CI/runner/pkg/runner"
)

// RunInstance implements `raccoonci run-instance`, which the agent starts in
// the unpacked source tree: it runs the one job instance read from standard
// input and writes wire lines to standard output (what a controller and a
// machine say to each other lives in pkg/wire).
func RunInstance(args []string, cat *catalog.Catalog) error {
	fs := flag.NewFlagSet("run-instance", flag.ExitOnError)
	var secrets cliflag.List
	fs.Var(&secrets, "secret", "the environment variable `NAME` to pass as secrets.NAME (repeatable)")
	fs.Parse(args)

	var spec wire.InstanceSpec
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 64<<20)).Decode(&spec); err != nil {
		return fmt.Errorf("reading the instance: %w", err)
	}
	if spec.Protocol > wire.Version {
		return fmt.Errorf("the controller sent an assignment of version %d; this raccoonci understands up to %d: the machine's snapshot is older than the controller and must be rebuilt (raccoonci runner-snapshot)", spec.Protocol, wire.Version)
	}
	ctxv, err := value.FromJSON(spec.Contexts)
	if err != nil {
		return fmt.Errorf("contexts: %w", err)
	}
	contexts, ok := ctxv.(value.Object)
	if !ok {
		return errors.New("contexts: not an object")
	}
	obj := func(name string) value.Object {
		v, _ := contexts.Get(name)
		o, _ := v.(value.Object)
		return o
	}
	secretValues := map[string]string{}
	for _, name := range secrets {
		v, ok := os.LookupEnv(name)
		if !ok {
			return fmt.Errorf("secret %s is not in the environment", name)
		}
		secretValues[name] = v
	}

	src, err := os.Getwd()
	if err != nil {
		return err
	}
	wfSrc, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(spec.Workflow)))
	if err != nil {
		return err
	}
	w, err := workflow.Parse(wfSrc)
	if err != nil {
		return fmt.Errorf("%s: %w", spec.Workflow, err)
	}
	job := w.Job(spec.Job)
	if job == nil {
		return fmt.Errorf("%s has no job %q", spec.Workflow, spec.Job)
	}
	cat = withSentCatalog(cat, []byte(spec.Catalog), os.Stderr)
	rt, err := NewRunEnv(cat, spec.Install)
	if err != nil {
		return err
	}
	images, err := runnerimages.Embedded()
	if err != nil {
		return err
	}
	if len(spec.Images) > 0 {
		// The controller's data is fresher than the binary's. A bad one only
		// costs that: the built-in data is used and the log says so.
		if sent, err := runnerimages.ParseSnapshot(spec.Images); err != nil {
			fmt.Fprintf(os.Stderr, "raccoonci: the runner-images data sent with the job is unusable (%v); using the built-in data\n", err)
		} else {
			images = sent
		}
	}
	github := obj("github")
	repository, _ := github.Get("repository")
	repoName, _ := repository.(string)
	repoDir, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer repoDir.Close()

	workDir := filepath.Join(rt.CacheDir, "runs", fmt.Sprintf("%s-%d", time.Now().Format("20060102-150405"), os.Getpid()))
	enc := wire.NewEncoder(os.Stdout)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	began := time.Now()
	res := engine.RunInstance(ctx, w, engine.Options{
		Github:    github,
		Secrets:   secretValues,
		Vars:      obj("vars"),
		Inputs:    obj("inputs"),
		Images:    images,
		Catalog:   cat,
		Provision: rt.Prov,
		Repo:      repoDir.FS(),
		WorkDir:   workDir,
		Runner: runner.Config{
			ToolCache:  rt.ToolCache,
			Actions:    actions.NewStore(filepath.Join(workDir, "_actions"), secretValues["GITHUB_TOKEN"]),
			Node:       rt.Node.Binary,
			NodeMusl:   rt.NodeMusl.Binary,
			Mounts:     []string{rt.Node.Dir, filepath.Join(workDir, "_actions")},
			Checkout:   local.Checkout(src, repoName),
			ServiceEnv: spec.ServiceEnv,
			RunnerName: "raccoonci-instabox",
			OnEvent:    func(e runner.Event) { enc.Write(wire.Line{Event: &e}) },
		},
	}, job, obj("matrix"), obj("strategy"), obj("needs"))

	outputs, err := json.Marshal(res.Outputs)
	if err != nil {
		return err
	}
	enc.Write(wire.Line{Result: &wire.Result{Conclusion: res.Conclusion, Outputs: outputs, DurationMS: time.Since(began).Milliseconds(), Summary: res.Summary}})
	return nil
}

// withSentCatalog lays the catalog feed the controller sent over the built-in
// catalog. Overlay checks it; a bad one only costs that: the built-in catalog
// is used and the warning says so. Nothing sent is the built-in catalog.
func withSentCatalog(cat *catalog.Catalog, sent []byte, warn io.Writer) *catalog.Catalog {
	if len(sent) == 0 {
		return cat
	}
	merged, err := cat.Overlay(sent)
	if err != nil {
		fmt.Fprintf(warn, "raccoonci: the catalog sent with the job is unusable (%v); using the built-in catalog\n", err)
		return cat
	}
	return merged
}
