package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"raccoonci/internal/actions"
	"raccoonci/internal/expr"
	"raccoonci/internal/value"
	"raccoonci/internal/workflow"
)

func isRemote(uses string) bool {
	return uses != "" && uses != "." && !strings.HasPrefix(uses, "./") && !strings.HasPrefix(uses, "docker://")
}

// isCheckout reports whether uses is the action that checks out the
// repository, as the catalog says.
func (r *jobRun) isCheckout(uses string) bool {
	name, _, _ := strings.Cut(uses, "@")
	a := r.cfg.Catalog.Action(name)
	return a != nil && a.Checkout
}

// fetch downloads a remote action's repository, once per job.
func (r *jobRun) fetch(ctx context.Context, uses string) (string, error) {
	r.mu.Lock()
	dir, ok := r.remoteActs[uses]
	r.mu.Unlock()
	if ok {
		return dir, nil
	}
	ref, err := actions.ParseRef(uses)
	if err != nil {
		return "", err
	}
	if r.cfg.Actions == nil {
		return "", fmt.Errorf("%s: no action store configured", uses)
	}
	dir, err = r.cfg.Actions.Fetch(ctx, ref)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	r.remoteActs[uses] = dir
	r.mu.Unlock()
	return dir, nil
}

// post is the post: part of an action, run after all main steps.
type post struct {
	name   string
	sc     *scope
	s      *workflow.Step
	a      *actions.Action
	info   *actionInfo
	inputs value.Object
	env    value.Object
	state  map[string]string
}

func (r *jobRun) preState(s *workflow.Step) map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.preStates[s]
}

// runPres runs the pre: part of the job's JavaScript actions before any
// main step, in step order. Only pre-if (default always()) decides; the
// step's own if: is evaluated later, when its main part is due.
func (r *jobRun) runPres(ctx context.Context, top *scope) {
	for _, s := range r.job.Job.Steps {
		if !isRemote(s.Uses) || (r.cfg.Checkout != nil && r.isCheckout(s.Uses)) {
			continue
		}
		dir, ref, err := r.actionDir(ctx, s.Uses)
		if err != nil {
			continue // reported when the main step runs
		}
		a, err := actions.Load(dir)
		if err != nil || !a.Runs.IsNode() || a.Runs.Pre == "" {
			continue
		}
		r.checkCancelled()
		env := r.scopeEnv(top)
		ectx := r.exprCtx(top, env, r.status(top))
		name := "Pre " + r.stepName(s, ectx)
		cond := a.Runs.PreIf
		if cond == "" {
			cond = "always()"
		}
		if ok, err := expr.EvalCondition(cond, ectx); err != nil || !ok {
			continue
		}
		state := map[string]string{}
		r.mu.Lock()
		if r.preStates == nil {
			r.preStates = map[*workflow.Step]map[string]string{}
		}
		r.preStates[s] = state
		r.mu.Unlock()

		r.emit(Event{Kind: StepStarted, Step: name})
		st, err := r.newStep(top, s, name, state)
		outcome := Failure
		if err == nil {
			info := &actionInfo{path: dir, repository: ref.Repository(), ref: ref.Version}
			st.github = withAction(st.github, info)
			stepEnv, err := r.evalEnv(s.Env, r.exprCtx(top, env, nil, st.github))
			if err == nil {
				for _, f := range stepEnv {
					env = env.Set(f.Key, f.Value)
				}
				outcome = r.runNodePart(r.stepContext(ctx), top, s, st, a, info, env, a.Runs.Pre)
			} else {
				r.emitStep(st, Error, "env: "+err.Error(), nil)
			}
		}
		r.finishPart(name, outcome)
	}
}

// runNodePart runs an action's pre: or post: script.
func (r *jobRun) runNodePart(ctx context.Context, sc *scope, s *workflow.Step, st *stepState, a *actions.Action, info *actionInfo, env value.Object, entry string) Conclusion {
	ectx := r.exprCtx(sc, env, nil, st.github)
	with, err := expr.EvalValue(s.With, ectx)
	if err != nil {
		r.emitStep(st, Error, "with: "+err.Error(), nil)
		return Failure
	}
	withObj, _ := with.(value.Object)
	inputs := r.actionInputs(st, a, withObj, ectx, false)
	return r.runNode(ctx, st, a, info, entry, inputs, env)
}

func (r *jobRun) finishPart(name string, outcome Conclusion) {
	if outcome == Failure {
		r.mu.Lock()
		r.failed = true
		r.mu.Unlock()
	}
	r.emit(Event{Kind: StepFinished, Step: name, Conclusion: outcome})
}

// runPosts runs the registered post: parts in reverse order. Their default
// condition is always(), so they clean up after failures too.
func (r *jobRun) runPosts(ctx context.Context) {
	r.mu.Lock()
	posts := slices.Clone(r.posts)
	r.mu.Unlock()
	for i := len(posts) - 1; i >= 0; i-- {
		p := posts[i]
		r.checkCancelled()
		cond := p.a.Runs.PostIf
		if cond == "" {
			cond = "always()"
		}
		ectx := r.exprCtx(p.sc, p.env, r.status(rootScope(r)))
		if ok, err := expr.EvalCondition(cond, ectx); err != nil || !ok {
			continue
		}
		r.emit(Event{Kind: StepStarted, Step: p.name})
		outcome := Failure
		if st, err := r.newStep(p.sc, p.s, p.name, p.state); err == nil {
			st.github = withAction(st.github, p.info)
			outcome = r.runNode(r.stepContext(ctx), st, p.a, p.info, p.a.Runs.Post, p.inputs, p.env)
		}
		r.finishPart(p.name, outcome)
	}
}

// actionDir returns the directory of a remote action.
func (r *jobRun) actionDir(ctx context.Context, uses string) (string, actions.Ref, error) {
	ref, err := actions.ParseRef(uses)
	if err != nil {
		return "", ref, err
	}
	repoDir, err := r.fetch(ctx, uses)
	if err != nil {
		return "", ref, err
	}
	return filepath.Join(repoDir, filepath.FromSlash(ref.Path)), ref, nil
}

func (r *jobRun) runUses(ctx context.Context, sc *scope, s *workflow.Step, st *stepState, env, stepEnv value.Object, ectx *expr.Context) Conclusion {
	fail := func(format string, args ...any) Conclusion {
		r.emitStep(st, Error, fmt.Sprintf(format, args...), nil)
		return Failure
	}
	with, err := expr.EvalValue(s.With, ectx)
	if err != nil {
		return fail("with: %v", err)
	}
	withObj, _ := with.(value.Object)

	var info *actionInfo
	switch uses := s.Uses; {
	case strings.HasPrefix(uses, "docker://"):
		return fail("%s: docker:// actions aren't supported yet", uses)
	case !isRemote(uses):
		info = &actionInfo{path: filepath.Join(r.workspace, filepath.FromSlash(uses))}
	default:
		if r.cfg.Checkout != nil && r.isCheckout(uses) {
			handled, err := r.cfg.Checkout(ctx, r.workspace, withObj)
			if err != nil {
				return fail("checkout: %v", err)
			}
			if handled {
				return Success
			}
		}
		dir, ref, err := r.actionDir(ctx, uses)
		if err != nil {
			return fail("%v", err)
		}
		info = &actionInfo{path: dir, repository: ref.Repository(), ref: ref.Version}
	}
	a, err := actions.Load(info.path)
	if err != nil {
		return fail("%s: %v", s.Uses, err)
	}
	st.github = withAction(st.github, info)
	ectx = r.exprCtx(sc, env, nil, st.github)
	inputs := r.actionInputs(st, a, withObj, ectx, true)

	switch {
	case a.Runs.IsNode():
		outcome := r.runNode(ctx, st, a, info, a.Runs.Main, inputs, env)
		if a.Runs.Post != "" {
			r.mu.Lock()
			r.posts = append(r.posts, &post{
				name: "Post " + st.name, sc: sc, s: s, a: a, info: info,
				inputs: inputs, env: env, state: st.state,
			})
			r.mu.Unlock()
		}
		return outcome
	case a.Runs.Using == "composite":
		return r.runComposite(ctx, sc, st, a, info, inputs, stepEnv)
	}
	return fail("%s: %s actions aren't supported yet", s.Uses, a.Runs.Using)
}

// actionInputs resolves an action's inputs from with: and the declared
// defaults, which may be expressions such as ${{ github.token }}.
func (r *jobRun) actionInputs(st *stepState, a *actions.Action, with value.Object, ctx *expr.Context, warn bool) value.Object {
	var in value.Object
	var unexpected []string
	for _, f := range with {
		declared := slices.ContainsFunc(a.Inputs, func(i actions.Input) bool { return strings.EqualFold(i.Name, f.Key) })
		if !declared {
			unexpected = append(unexpected, "'"+f.Key+"'")
		}
		in = in.Set(f.Key, expr.ToString(f.Value))
	}
	if warn && len(unexpected) > 0 {
		var valid []string
		for _, i := range a.Inputs {
			valid = append(valid, "'"+i.Name+"'")
		}
		r.emitStep(st, Warning, fmt.Sprintf("Unexpected input(s) %s, valid inputs are [%s]", strings.Join(unexpected, ", "), strings.Join(valid, ", ")), nil)
	}
	for _, d := range a.Inputs {
		if _, ok := in.Lookup(d.Name); ok {
			if warn && d.Deprecation != "" {
				r.emitStep(st, Warning, fmt.Sprintf("Input '%s' has been deprecated with message: %s", d.Name, d.Deprecation), nil)
			}
			continue
		}
		v, err := expr.EvalValue(d.Default, ctx)
		if err != nil {
			r.emitStep(st, Warning, fmt.Sprintf("input %q default: %v", d.Name, err), nil)
			v = ""
		}
		in = append(in, value.Field{Key: d.Name, Value: expr.ToString(v)})
	}
	return in
}

func (r *jobRun) runNode(ctx context.Context, st *stepState, a *actions.Action, info *actionInfo, entry string, inputs, env value.Object) Conclusion {
	if r.cfg.Node == nil {
		r.emitStep(st, Error, "no Node.js runtime configured for JavaScript actions", nil)
		return Failure
	}
	nodeFor := r.cfg.Node
	if r.cfg.NodeMusl != nil && r.containerIsMusl(ctx) {
		nodeFor = r.cfg.NodeMusl
	}
	node, err := nodeFor(ctx, a.Runs.Using)
	if err != nil {
		r.emitStep(st, Error, fmt.Sprintf("%s runtime: %v", a.Runs.Using, err), nil)
		return Failure
	}
	if err := r.checkNodeRuns(ctx, st, node); err != nil {
		r.emitStep(st, Error, err.Error(), nil)
		return Failure
	}
	var extra [][2]string
	for _, f := range inputs {
		extra = append(extra, [2]string{"INPUT_" + strings.ToUpper(strings.ReplaceAll(f.Key, " ", "_")), expr.ToString(f.Value)})
	}
	r.mu.Lock()
	for k, v := range st.state {
		extra = append(extra, [2]string{"STATE_" + k, v})
	}
	r.mu.Unlock()
	return r.exec(ctx, st, []string{node, filepath.Join(info.path, filepath.FromSlash(entry))}, r.workspace, r.processEnv(env, st, extra))
}

// runComposite runs a composite action's steps in a scope of their own.
func (r *jobRun) runComposite(ctx context.Context, parent *scope, st *stepState, a *actions.Action, info *actionInfo, inputs, stepEnv value.Object) Conclusion {
	child := &scope{parent: parent, inputs: inputs, env: stepEnv, action: info, depth: parent.depth + 1}
	for _, s := range a.Runs.Steps {
		r.checkCancelled()
		r.runStep(ctx, child, s)
	}
	octx := r.exprCtx(child, r.scopeEnv(child), nil)
	for _, o := range a.Outputs {
		v, err := expr.Interpolate(o.Value, octx)
		if err != nil {
			r.emitStep(st, Warning, fmt.Sprintf("output %q: %v", o.Name, err), nil)
			continue
		}
		st.outputs = st.outputs.Set(o.Name, expr.ToString(v))
	}
	if child.failed {
		return Failure
	}
	return Success
}
