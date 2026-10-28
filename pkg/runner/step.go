package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"raccoonci/internal/expr"
	"raccoonci/internal/value"
	"raccoonci/internal/workflow"
)

// scope is a list of steps being run: the job's, or a composite action's,
// which has its own steps and inputs contexts and its own success().
type scope struct {
	parent *scope
	steps  value.Object
	inputs value.Object
	// env is what this level adds to the env context: for a composite
	// action, the env: of the step that called it.
	env    value.Object
	action *actionInfo // the composite action, nil for the job
	failed bool
	depth  int
}

type actionInfo struct {
	path, repository, ref string
}

func rootScope(r *jobRun) *scope {
	return &scope{inputs: r.job.Inputs}
}

// stepState is one running step (or its pre/post part).
type stepState struct {
	name   string
	depth  int
	seq    int          // numbers the step's temp files
	github value.Object // github context, with this step's file command paths

	outputs value.Object
	state   map[string]string // save-state; shared by an action's pre, main and post
	files   map[string]string // file command name -> path

	// Guarded by jobRun.mu, like the job state handleLine also touches.
	echo      bool
	stopToken string
	failed    bool // a workflow or file command failed
}

// fileCommands maps the github context key of each file command to its
// file name prefix, as the GitHub runner names them.
var fileCommands = [][2]string{
	{"env", "set_env"}, {"path", "add_path"}, {"output", "set_output"},
	{"state", "save_state"}, {"step_summary", "step_summary"},
}

func (r *jobRun) newStep(sc *scope, s *workflow.Step, name string, state map[string]string) (*stepState, error) {
	if state == nil {
		state = map[string]string{}
	}
	r.mu.Lock()
	r.fileSeq++
	seq := r.fileSeq
	r.mu.Unlock()
	st := &stepState{name: name, depth: sc.depth, seq: seq, state: state, files: map[string]string{}}
	st.github = r.scopeGithub(sc)
	st.github = st.github.Set("action", r.actionID(s))
	for _, fc := range fileCommands {
		p := filepath.Join(r.temp, "_runner_file_commands", fmt.Sprintf("%s_%d", fc[1], seq))
		if err := os.WriteFile(p, nil, 0o666); err != nil {
			return nil, err
		}
		st.files[fc[0]] = p
		st.github = st.github.Set(fc[0], p)
	}
	return st, nil
}

// scopeGithub is the github context inside sc: in a composite action,
// github.action_path and friends describe that action.
func (r *jobRun) scopeGithub(sc *scope) value.Object {
	g := slices.Clone(r.github)
	for s := sc; s != nil; s = s.parent {
		if s.action != nil {
			return withAction(g, s.action)
		}
	}
	return g
}

func withAction(g value.Object, a *actionInfo) value.Object {
	g = slices.Clone(g)
	g = g.Set("action_path", a.path)
	g = g.Set("action_repository", a.repository)
	g = g.Set("action_ref", a.ref)
	return g
}

var nonIdent = regexp.MustCompile(`[^A-Za-z0-9]+`)

// actionID is github.action: the step's id, or one GitHub-style generated
// from what the step runs (__run, __run_2, __actions_checkout, ...).
func (r *jobRun) actionID(s *workflow.Step) string {
	if s.ID != "" {
		return s.ID
	}
	base := "__run"
	if s.Uses != "" {
		u, _, _ := strings.Cut(s.Uses, "@")
		base = "__" + strings.Trim(nonIdent.ReplaceAllString(u, "_"), "_")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.actionIDs[base]++
	if n := r.actionIDs[base]; n > 1 {
		return fmt.Sprintf("%s_%d", base, n)
	}
	return base
}

// exprCtx builds the contexts a step's expressions see. github defaults to
// the scope's github context.
func (r *jobRun) exprCtx(sc *scope, env value.Object, status *expr.Status, github ...value.Object) *expr.Context {
	g := r.github
	if len(github) > 0 {
		g = github[0]
	} else if sc != nil {
		g = r.scopeGithub(sc)
	}
	r.mu.Lock()
	jobStatus := Success
	switch {
	case r.cancelled:
		jobStatus = Cancelled
	case r.failed:
		jobStatus = Failure
	}
	ctxContainer, ctxServices := r.ctxContainer, r.ctxServices
	r.mu.Unlock()
	orEmpty := func(o value.Object) value.Object {
		if o == nil {
			return value.Object{}
		}
		return o
	}
	c := map[string]any{
		"github": g,
		"env":    orEmpty(env),
		"job": value.Object{
			{Key: "status", Value: string(jobStatus)},
			{Key: "container", Value: orEmpty(ctxContainer)},
			{Key: "services", Value: orEmpty(ctxServices)},
		},
		"runner":   orEmpty(r.runner),
		"matrix":   orEmpty(r.job.Matrix),
		"strategy": orEmpty(r.job.Strategy),
		"needs":    orEmpty(r.job.Needs),
		"vars":     orEmpty(r.job.Vars),
		"secrets":  secretsObject(r.job.Secrets),
		"steps":    value.Object{},
		"inputs":   value.Object{},
	}
	if sc != nil {
		c["steps"] = orEmpty(sc.steps)
		c["inputs"] = orEmpty(sc.inputs)
	}
	return &expr.Context{Contexts: c, Status: status, Funcs: map[string]expr.Func{"hashfiles": hashFiles(r.workspace)}}
}

// scopeEnv is the env context inside sc, before the step's own env:.
func (r *jobRun) scopeEnv(sc *scope) value.Object {
	env := r.currentEnv()
	var chain []*scope
	for s := sc; s != nil; s = s.parent {
		chain = append(chain, s)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		for _, f := range chain[i].env {
			env = env.Set(f.Key, f.Value)
		}
	}
	return env
}

func (r *jobRun) status(sc *scope) *expr.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	failed := sc.failed
	if sc.parent == nil {
		failed = r.failed
	}
	return &expr.Status{Failed: failed, Cancelled: r.cancelled}
}

func (r *jobRun) stepName(s *workflow.Step, ctx *expr.Context) string {
	if s.Name != "" {
		if v, err := expr.Interpolate(s.Name, ctx); err == nil {
			return expr.ToString(v)
		}
		return s.Name
	}
	if s.Uses != "" {
		return "Run " + s.Uses
	}
	for _, l := range strings.Split(s.Run, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return "Run " + l
		}
	}
	return "Run"
}

// runStep runs one step in sc and records its result in sc's steps context.
func (r *jobRun) runStep(ctx context.Context, sc *scope, s *workflow.Step) {
	env := r.scopeEnv(sc)
	ectx := r.exprCtx(sc, env, r.status(sc))
	name := r.stepName(s, ectx)
	ok, err := expr.EvalCondition(s.If, ectx)
	if err != nil {
		r.emit(Event{Kind: StepStarted, Step: name, Depth: sc.depth})
		r.emit(Event{Kind: Error, Step: name, Depth: sc.depth, Text: "if: " + err.Error()})
		r.finishStep(sc, s, name, Failure, Failure, nil)
		return
	}
	if !ok {
		r.finishStep(sc, s, name, Skipped, Skipped, nil)
		return
	}
	r.emit(Event{Kind: StepStarted, Step: name, Depth: sc.depth})
	st, err := r.newStep(sc, s, name, r.preState(s))
	if err != nil {
		r.emit(Event{Kind: Error, Step: name, Depth: sc.depth, Text: err.Error()})
		r.finishStep(sc, s, name, Failure, Failure, nil)
		return
	}
	outcome := r.execStep(ctx, sc, s, st, env)
	conclusion := outcome
	if outcome == Failure && s.ContinueOnError != nil {
		v, err := expr.EvalValue(s.ContinueOnError, r.exprCtx(sc, env, nil, st.github))
		if err != nil {
			r.emit(Event{Kind: Error, Step: name, Depth: sc.depth, Text: "continue-on-error: " + err.Error()})
		} else if expr.Truthy(v) {
			conclusion = Success
		}
	}
	r.finishStep(sc, s, name, outcome, conclusion, st.outputs)
}

func (r *jobRun) finishStep(sc *scope, s *workflow.Step, name string, outcome, conclusion Conclusion, outputs value.Object) {
	if outputs == nil {
		outputs = value.Object{}
	}
	if s.ID != "" {
		sc.steps = sc.steps.Set(s.ID, value.Object{
			{Key: "outputs", Value: outputs},
			{Key: "outcome", Value: string(outcome)},
			{Key: "conclusion", Value: string(conclusion)},
		})
	}
	if conclusion == Failure {
		r.mu.Lock()
		sc.failed = true
		if sc.parent == nil {
			r.failed = true
		}
		r.mu.Unlock()
	}
	r.emit(Event{Kind: StepFinished, Step: name, Depth: sc.depth, Conclusion: conclusion})
}

// execStep runs a step whose condition passed and returns its outcome.
func (r *jobRun) execStep(ctx context.Context, sc *scope, s *workflow.Step, st *stepState, env value.Object) Conclusion {
	fail := func(format string, args ...any) Conclusion {
		r.emitStep(st, Error, fmt.Sprintf(format, args...), nil)
		return Failure
	}
	stepEnv, err := r.evalEnv(s.Env, r.exprCtx(sc, env, nil, st.github))
	if err != nil {
		return fail("env: %v", err)
	}
	for _, f := range stepEnv {
		env = env.Set(f.Key, f.Value)
	}
	ectx := r.exprCtx(sc, env, nil, st.github)

	base := r.stepContext(ctx)
	sctx := base
	minutes, err := r.evalNumber(s.TimeoutMinutes, ectx)
	if err != nil {
		return fail("timeout-minutes: %v", err)
	}
	if minutes > 0 {
		var cancel context.CancelFunc
		sctx, cancel = context.WithTimeout(base, time.Duration(minutes*float64(time.Minute)))
		defer cancel()
	}

	var outcome Conclusion
	if s.Uses != "" {
		outcome = r.runUses(sctx, sc, s, st, env, stepEnv, ectx)
	} else {
		outcome = r.runScript(sctx, sc, s, st, env, ectx)
	}
	switch {
	case base.Err() != nil:
		return Cancelled
	case sctx.Err() != nil:
		return fail("The action has timed out after %g minutes.", minutes)
	}
	return outcome
}

func (r *jobRun) runScript(ctx context.Context, sc *scope, s *workflow.Step, st *stepState, env value.Object, ectx *expr.Context) Conclusion {
	fail := func(format string, args ...any) Conclusion {
		r.emitStep(st, Error, fmt.Sprintf(format, args...), nil)
		return Failure
	}
	v, err := expr.Interpolate(s.Run, ectx)
	if err != nil {
		return fail("run: %v", err)
	}
	script := expr.ToString(v)

	// Job and workflow defaults don't reach into composite actions.
	shell, wd := s.Shell, s.WorkingDirectory
	if sc.parent == nil {
		for _, d := range []workflow.Defaults{r.job.Job.Defaults, r.job.Workflow.Defaults} {
			if shell == "" {
				shell = d.Shell
			}
			if wd == "" {
				wd = d.WorkingDirectory
			}
		}
	}
	if v, err := expr.Interpolate(shell, ectx); err != nil {
		return fail("shell: %v", err)
	} else {
		shell = expr.ToString(v)
	}
	dir, err := r.workingDir(wd, ectx)
	if err != nil {
		return fail("%v", err)
	}

	procEnv := r.processEnv(env, st, nil)
	scriptPath := filepath.Join(r.temp, fmt.Sprintf("script_%d%s", st.seq, scriptExt(shell)))
	argv, body, err := shellCommand(r.lookup, shell, script, scriptPath, envValue(procEnv, "PATH"))
	if err != nil {
		return fail("%v", err)
	}
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		return fail("%v", err)
	}
	r.emitStep(st, Debug, "shell: "+strings.Join(argv, " "), nil)
	return r.exec(ctx, st, argv, dir, procEnv)
}

func (r *jobRun) workingDir(wd string, ctx *expr.Context) (string, error) {
	if wd == "" {
		return r.workspace, nil
	}
	v, err := expr.Interpolate(wd, ctx)
	if err != nil {
		return "", fmt.Errorf("working-directory: %w", err)
	}
	dir := expr.ToString(v)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(r.workspace, dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("working-directory %s does not exist", dir)
	}
	return dir, nil
}

func (r *jobRun) emitStep(st *stepState, kind EventKind, text string, props map[string]string) {
	r.emit(Event{Kind: kind, Step: st.name, Depth: st.depth, Text: text, Props: props})
}

// githubEnv lists the github context keys exported as GITHUB_* variables.
var githubEnv = []string{
	"action", "action_path", "action_ref", "action_repository", "actor", "actor_id",
	"api_url", "base_ref", "env", "event_name", "event_path", "graphql_url", "head_ref",
	"job", "output", "path", "ref", "ref_name", "ref_protected", "ref_type",
	"repository", "repository_id", "repository_owner", "repository_owner_id",
	"retention_days", "run_attempt", "run_id", "run_number", "server_url", "sha",
	"state", "step_summary", "triggering_actor", "workflow", "workflow_ref",
	"workflow_sha", "workspace",
}

// processEnv is a step's process environment. User env can't override the
// GITHUB_* and RUNNER_* variables, as on GitHub (CI it can).
func (r *jobRun) processEnv(env value.Object, st *stepState, extra [][2]string) []string {
	r.mu.Lock()
	base := slices.Clone(r.cBase) // a container job starts from its image's environment
	serviceEnv := r.cfg.ServiceEnv
	if r.cServiceEnv != nil {
		serviceEnv = r.cServiceEnv
	}
	r.mu.Unlock()
	if base == nil {
		base = r.cfg.BaseEnv
		if base == nil {
			base = os.Environ()
		}
	}
	var keys []string
	m := map[string]string{}
	set := func(k, v string) {
		if _, ok := m[k]; !ok {
			keys = append(keys, k)
		}
		m[k] = v
	}
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok {
			set(k, v)
		}
	}
	set("CI", "true")
	for _, f := range env {
		set(f.Key, expr.ToString(f.Value))
	}
	set("GITHUB_ACTIONS", "true")
	for _, k := range githubEnv {
		if v, ok := st.github.Get(k); ok {
			set("GITHUB_"+strings.ToUpper(k), expr.ToString(v))
		}
	}
	for _, f := range r.runner {
		if v := expr.ToString(f.Value); v != "" { // RUNNER_DEBUG only when debugging
			set("RUNNER_"+strings.ToUpper(f.Key), v)
		}
	}
	set("AGENT_TOOLSDIRECTORY", r.toolCache) // setup-python prefers it to RUNNER_TOOL_CACHE
	if img := r.job.Image; img != nil {
		set("ImageOS", img.Image)
		set("ImageVersion", img.Version)
	}
	for k, v := range serviceEnv {
		set(k, v)
	}
	for _, kv := range extra {
		set(kv[0], kv[1])
	}
	r.mu.Lock()
	if len(r.path) > 0 {
		p := strings.Join(r.path, string(os.PathListSeparator))
		if m["PATH"] != "" {
			p += string(os.PathListSeparator) + m["PATH"]
		}
		set("PATH", p)
	}
	r.mu.Unlock()
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

func envValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}
