package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/actions"
	"github.com/Raccoon-CI/raccoonci/internal/docker"
	"github.com/Raccoon-CI/raccoonci/pkg/expr"
	"github.com/Raccoon-CI/raccoonci/pkg/value"
)

// Docker actions (`uses: docker://image` and `runs.using: docker`) run in a
// container of their own, started with `docker run` the way GitHub's runner
// does: the workspace, the temp directory, the tool cache, the file commands
// and the runner's "home" are mounted under /github and /opt, the variables
// that name paths are rewritten to match, the host's environment is not
// passed, and the container joins the job's network if the job has one.

// dockerPart is which entrypoint of a Docker action runs.
type dockerPart int

const (
	dockerMain dockerPart = iota
	dockerPre
	dockerPost
)

// dockerClient is the docker client of this job, found on first use.
func (r *jobRun) dockerClient(ctx context.Context) (*docker.Client, error) {
	r.mu.Lock()
	c := r.dkr
	r.mu.Unlock()
	if c != nil {
		return c, nil
	}
	c = r.cfg.Docker
	if c == nil {
		var err error
		if c, err = docker.New(); err != nil {
			return nil, err
		}
	}
	if err := c.Ping(ctx); err != nil {
		return nil, fmt.Errorf("the docker daemon is not reachable: %w", err)
	}
	rootless := c.Rootless(ctx)
	r.mu.Lock()
	r.dkr, r.rootless = c, rootless
	r.mu.Unlock()
	return c, nil
}

// dockerActionImage returns the image a Docker action runs: pulled for
// docker://, built from the action's Dockerfile otherwise. Built once per job.
func (r *jobRun) dockerActionImage(ctx context.Context, st *stepState, c *docker.Client, a *actions.Action, dir string) (string, error) {
	image := a.Runs.Image
	if img, ok := strings.CutPrefix(image, "docker://"); ok {
		return r.pullForAction(ctx, st, c, img)
	}
	file := filepath.Join(dir, filepath.FromSlash(image))
	sum := sha256.Sum256([]byte(file))
	tag := "raccoonci-action-" + hex.EncodeToString(sum[:6]) + ":latest"
	r.mu.Lock()
	built := r.builtImages[tag]
	r.mu.Unlock()
	if built {
		return tag, nil
	}
	r.emitStep(st, Output, "Building docker image from "+image, nil)
	if err := c.Build(ctx, tag, file, dir); err != nil {
		return "", err
	}
	r.mu.Lock()
	if r.builtImages == nil {
		r.builtImages = map[string]bool{}
	}
	r.builtImages[tag] = true
	r.mu.Unlock()
	return tag, nil
}

func (r *jobRun) pullForAction(ctx context.Context, st *stepState, c *docker.Client, image string) (string, error) {
	if err := c.Pull(ctx, image, ""); err != nil {
		if _, ierr := c.ImageEnv(ctx, image); ierr != nil {
			return "", err
		}
		r.emitStep(st, Warning, fmt.Sprintf("could not pull %s (%v); using the copy already here", image, err), nil)
	}
	return image, nil
}

// dockerRun is one container run of a Docker action.
type dockerRun struct {
	image      string
	entrypoint string
	args       []string
	extraEnv   value.Object // runs.env, evaluated
}

// runDockerRun runs the container and returns the step's outcome.
func (r *jobRun) runDockerRun(ctx context.Context, st *stepState, c *docker.Client, d dockerRun, inputs, env value.Object) Conclusion {
	fail := func(format string, args ...any) Conclusion {
		r.emitStep(st, Error, fmt.Sprintf(format, args...), nil)
		return Failure
	}
	for _, p := range []string{filepath.Join(r.temp, "_github_home")} {
		if err := os.MkdirAll(p, 0o777); err != nil {
			return fail("%v", err)
		}
	}
	// host directory -> where the container sees it
	mounts := [][2]string{
		{r.workspace, "/github/workspace"},
		{r.temp, "/github/runner_temp"},
		{filepath.Join(r.temp, "_github_home"), "/github/home"},
		{filepath.Join(r.temp, "_github_workflow"), "/github/workflow"},
		{filepath.Join(r.temp, "_runner_file_commands"), "/github/file_commands"},
		{r.toolCache, "/opt/hostedtoolcache"},
	}
	toContainer := func(v string) string {
		best := -1
		for i, m := range mounts {
			if (v == m[0] || strings.HasPrefix(v, m[0]+"/")) && (best < 0 || len(m[0]) > len(mounts[best][0])) {
				best = i
			}
		}
		if best < 0 {
			return v
		}
		return mounts[best][1] + strings.TrimPrefix(v, mounts[best][0])
	}

	var extra [][2]string
	for _, f := range inputs {
		extra = append(extra, [2]string{"INPUT_" + strings.ToUpper(strings.ReplaceAll(f.Key, " ", "_")), expr.ToString(f.Value)})
	}
	for _, f := range d.extraEnv {
		extra = append(extra, [2]string{f.Key, expr.ToString(f.Value)})
	}
	r.mu.Lock()
	for k, v := range st.state {
		extra = append(extra, [2]string{"STATE_" + k, v})
	}
	r.mu.Unlock()
	var cenv []string
	for _, kv := range r.envFor(env, st, extra, false) {
		k, v, _ := strings.Cut(kv, "=")
		// Only the variables that name this machine's paths are rewritten.
		if strings.HasPrefix(k, "GITHUB_") || strings.HasPrefix(k, "RUNNER_") || k == "AGENT_TOOLSDIRECTORY" {
			v = toContainer(v)
		}
		cenv = append(cenv, k+"="+v)
	}
	cenv = append(cenv, "HOME=/github/home")

	name := docker.RandomName("raccoonci-action")
	args := []string{"--rm", "--name", name, "--label", "raccoonci=1", "--workdir", "/github/workspace"}
	r.mu.Lock()
	network := r.network
	r.mu.Unlock()
	if network != "" {
		args = append(args, "--network", network)
	}
	if d.entrypoint != "" {
		args = append(args, "--entrypoint", d.entrypoint)
	}
	if _, err := os.Stat("/var/run/docker.sock"); err == nil {
		args = append(args, "-v", "/var/run/docker.sock:/var/run/docker.sock")
	}
	for _, m := range mounts {
		args = append(args, "-v", m[0]+":"+m[1])
	}
	args = append(args, d.image)
	args = append(args, d.args...)

	cmd, cleanup, err := c.RunCmd(context.Background(), cenv, args...)
	if err != nil {
		return fail("%v", err)
	}
	defer cleanup()
	defer r.fixOwnership(c, d.image, mounts)
	return r.runCmd(ctx, st, cmd, "", "", func() {
		kctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		c.Kill(kctx, name)
	})
}

// fixOwnership gives the files the container created (as root) back to the
// user running Raccoon CI, so later steps and the cleanup of the work
// directory can touch them. Best effort: the image may have no chown.
func (r *jobRun) fixOwnership(c *docker.Client, image string, mounts [][2]string) {
	if os.Getuid() == 0 || r.rootlessDocker() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The job's work directory holds the workspace, the temp directory, the
	// home and the file commands; the tool cache is separate.
	args := []string{"--rm", "--user", "0", "--entrypoint", "chown"}
	var dirs []string
	for _, m := range mounts {
		if m[1] == "/github/workspace" || m[1] == "/github/runner_temp" {
			args = append(args, "-v", m[0]+":"+m[1])
			dirs = append(dirs, m[1])
		}
	}
	args = append(args, image, "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
	args = append(args, dirs...)
	cmd, cleanup, err := c.RunCmd(ctx, nil, args...)
	if err != nil {
		return
	}
	defer cleanup()
	_ = cmd.Run()
}

func (r *jobRun) rootlessDocker() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rootless
}

// runDockerAction runs one entrypoint of a `runs.using: docker` action.
func (r *jobRun) runDockerAction(ctx context.Context, sc *scope, st *stepState, a *actions.Action, info *actionInfo, inputs, env value.Object, part dockerPart) Conclusion {
	fail := func(format string, args ...any) Conclusion {
		r.emitStep(st, Error, fmt.Sprintf(format, args...), nil)
		return Failure
	}
	c, err := r.dockerClient(ctx)
	if err != nil {
		return fail("%v", err)
	}
	image, err := r.dockerActionImage(ctx, st, c, a, info.path)
	if err != nil {
		return fail("%v", err)
	}
	child := &scope{parent: sc, inputs: inputs, action: info, depth: sc.depth + 1}
	ectx := r.exprCtx(child, env, nil, st.github)
	d := dockerRun{image: image}
	switch part {
	case dockerMain:
		d.entrypoint = a.Runs.Entrypoint
	case dockerPre:
		d.entrypoint = a.Runs.PreEntrypoint
	case dockerPost:
		d.entrypoint = a.Runs.PostEntrypoint
	}
	for _, raw := range a.Runs.Args {
		v, err := expr.EvalValue(raw, ectx)
		if err != nil {
			return fail("runs.args: %v", err)
		}
		d.args = append(d.args, expr.ToString(v))
	}
	for _, f := range a.Runs.Env {
		v, err := expr.EvalValue(f.Value, ectx)
		if err != nil {
			return fail("runs.env %s: %v", f.Key, err)
		}
		d.extraEnv = d.extraEnv.Set(f.Key, v)
	}
	return r.runDockerRun(ctx, st, c, d, inputs, env)
}

// runDockerImageStep runs `uses: docker://image`, with the step's `entrypoint`
// and `args` inputs (args is split like a command line, nothing is expanded).
func (r *jobRun) runDockerImageStep(ctx context.Context, st *stepState, image string, with, env value.Object) Conclusion {
	fail := func(format string, args ...any) Conclusion {
		r.emitStep(st, Error, fmt.Sprintf(format, args...), nil)
		return Failure
	}
	c, err := r.dockerClient(ctx)
	if err != nil {
		return fail("%v", err)
	}
	if image, err = r.pullForAction(ctx, st, c, image); err != nil {
		return fail("%v", err)
	}
	d := dockerRun{image: image}
	if v, ok := with.Lookup("entrypoint"); ok {
		d.entrypoint = expr.ToString(v)
	}
	if v, ok := with.Lookup("args"); ok {
		if d.args, err = docker.SplitOptions(expr.ToString(v)); err != nil {
			return fail("args: %v", err)
		}
	}
	return r.runDockerRun(ctx, st, c, d, with, env)
}
