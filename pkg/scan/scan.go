// Package scan works out what a job needs installed before it runs: the
// toolchains setup-* actions ask for, plus tools the job uses without
// declaring them because GitHub's Ubuntu images happen to preinstall them.
// Raccoon CI's machines run a slim Debian base, so those implicit demands
// must be installed (at the image's default versions) or the job breaks.
//
// Scanning is static and best-effort: it never fails a job, and anything it
// can't see (scripts in the repo, actions that can't be downloaded) is caught
// at run time.
//
// What commands, actions and files mean comes from package catalog; this
// package only knows how to find them in a workflow.
package scan

import (
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/Raccoon-CI/raccoonci/internal/actions"
	"github.com/Raccoon-CI/raccoonci/pkg/catalog"
	"github.com/Raccoon-CI/raccoonci/pkg/expr"
	"github.com/Raccoon-CI/raccoonci/pkg/runnerimages"
	"github.com/Raccoon-CI/raccoonci/pkg/value"
	"github.com/Raccoon-CI/raccoonci/pkg/workflow"
)

type Kind = catalog.Kind

const (
	Toolchain = catalog.Toolchain
	Package   = catalog.Package
	Tool      = catalog.Tool
)

type Demand struct {
	Kind Kind
	Name string
	// Version is a version spec for toolchains. It is "" when it is only
	// known at run time (or for packages and tools).
	Version string
	// Implicit means the workflow never declared it: on GitHub it only
	// worked because the image preinstalls it.
	Implicit bool
	// Declared is set on an implicit demand when the workflow also asks for
	// exactly this version by name (a later setup-* step). The job then needs
	// that version, not just something like it.
	Declared bool
	Reason   string
}

type Result struct {
	Demands  []Demand
	Warnings []string
}

type Input struct {
	Workflow *workflow.Workflow
	Job      *workflow.Job
	// Context holds the contexts known so far (matrix, strategy at plan
	// time; everything at run time).
	Context *expr.Context
	// Image is the GitHub image the job's runs-on names. When nil (e.g.
	// self-hosted labels) only explicit demands are reported.
	Image *runnerimages.Defaults
	// Repo is the repository at the triggering commit, or nil. Use an
	// os.OpenRoot FS, so symlinks in untrusted repos can't escape it.
	Repo fs.FS
	// Catalog defaults to catalog.Default().
	Catalog *catalog.Catalog
	// Action, if set, returns the metadata of the remote action a `uses:`
	// reference names (owner/repo[/path]@ref), or false if it can't. It is
	// what lets the scan see inside composite and docker actions, which means
	// downloading them; the scan never fails when it returns false.
	Action func(uses string) (*actions.Action, bool)
	// Missing lists commands an earlier run of this job could not find
	// (exit 127). Those the catalog knows and the image preinstalls are
	// reported as implicit demands, as if a step had run them.
	Missing []string
}

// placeholder replaces expressions that can't be evaluated yet, so scripts
// still parse.
const placeholder = "__raccoon_expr__"

type scanner struct {
	in       Input
	cat      *catalog.Catalog
	res      Result
	provided map[string]bool // set up or installed by an earlier step
	has      map[string]bool // kind/name already demanded
	seen     map[string]int  // kind/name/version already demanded -> index in res.Demands
	warned   map[string]bool
	stack    []string // composite actions being scanned, outermost first
	checkout bool     // the job checks out the repo
	opaque   string   // first step that runs an opaque build command
}

func Job(in Input) *Result {
	s := &scanner{
		in:       in,
		provided: map[string]bool{},
		has:      map[string]bool{},
		seen:     map[string]int{},
		warned:   map[string]bool{},
	}
	if s.in.Context == nil {
		s.in.Context = &expr.Context{}
	}
	s.cat = in.Catalog
	if s.cat == nil {
		s.cat = catalog.Default()
	}
	j := in.Job
	if j.Container != nil || j.Services != nil {
		s.add(Demand{Kind: Tool, Name: "docker", Reason: "the job uses container or services"})
	}
	if j.Container != nil {
		// Steps run inside the job's container, whose image brings its own
		// tools; nothing of the host's preinstalled software applies.
		return &s.res
	}
	for i, st := range j.Steps {
		where := fmt.Sprintf("step %d", i+1)
		if st.Name != "" {
			where += fmt.Sprintf(" (%s)", expr.InterpolateLoose(st.Name, s.in.Context, "…"))
		}
		if st.Uses != "" {
			s.uses(where, st)
		} else {
			s.run(where, st)
		}
	}
	s.manifests()
	for _, cmd := range in.Missing {
		if r, ok := s.cat.Command(cmd); ok {
			s.need("an earlier run of this job", cmd, r)
		}
	}
	return &s.res
}

func (s *scanner) add(d Demand) {
	key := d.Kind.String() + "/" + d.Name
	if i, ok := s.seen[key+"/"+d.Version]; ok {
		if !d.Implicit && s.res.Demands[i].Implicit {
			s.res.Demands[i].Declared = true
		}
		return
	}
	s.seen[key+"/"+d.Version] = len(s.res.Demands)
	s.has[key] = true
	s.res.Demands = append(s.res.Demands, d)
}

func (s *scanner) warn(msg string) {
	if !s.warned[msg] {
		s.warned[msg] = true
		s.res.Warnings = append(s.res.Warnings, msg)
	}
}

func (s *scanner) uses(where string, st *workflow.Step) {
	ref := expr.InterpolateLoose(st.Uses, s.in.Context, placeholder)
	if strings.HasPrefix(ref, "docker://") {
		s.add(Demand{Kind: Tool, Name: "docker", Reason: where + " runs a container action"})
		return
	}
	if strings.HasPrefix(ref, "./") {
		s.inside(where, ref, st)
		return
	}
	name, at, _ := strings.Cut(ref, "@")
	name = strings.ToLower(name)
	a := s.cat.Action(name)
	switch {
	case a != nil && a.Checkout:
		s.checkout = true
	case a != nil && a.NeedsDocker:
		s.add(Demand{Kind: Tool, Name: "docker", Reason: where + " uses " + name})
	case a != nil && a.Provides != "":
		s.provided[a.Provides] = true
	case a != nil && a.Setup != "":
		s.setup(where, name, at, a, st)
	default:
		s.inside(where, ref, st)
	}
}

// maxActionDepth bounds composite actions that call composite actions.
const maxActionDepth = 4

// inside looks into an action the catalog has no special rule for: a docker
// action needs docker, and the steps of a composite action need what any
// other steps need (a script calling make, a nested setup-python). The
// metadata of a remote action comes from Input.Action (it has to be
// downloaded), that of a local one from the repository.
func (s *scanner) inside(where, ref string, st *workflow.Step) {
	if len(s.stack) >= maxActionDepth || slices.Contains(s.stack, ref) {
		return
	}
	var act *actions.Action
	if strings.HasPrefix(ref, "./") {
		act = s.localAction(ref)
	} else if s.in.Action != nil {
		act, _ = s.in.Action(ref)
	}
	if act == nil {
		return
	}
	switch act.Runs.Using {
	case "docker":
		s.add(Demand{Kind: Tool, Name: "docker", Reason: where + " runs the docker action " + ref})
	case "composite":
		saved := s.in.Context
		s.in.Context = s.compositeContext(act, st)
		s.stack = append(s.stack, ref)
		defer func() { s.in.Context, s.stack = saved, s.stack[:len(s.stack)-1] }()
		for i, inner := range act.Runs.Steps {
			w := fmt.Sprintf("%s: %s step %d", where, ref, i+1)
			// A step whose condition is false here (another OS, an input
			// switched off) never runs; one we can't evaluate yet might.
			if ok, err := expr.EvalCondition(inner.If, s.in.Context); err == nil && !ok {
				continue
			}
			if inner.Uses != "" {
				s.uses(w, inner)
			} else {
				s.run(w, inner)
			}
		}
	}
}

// localAction reads ./path/action.yml from the repository.
func (s *scanner) localAction(ref string) *actions.Action {
	if s.in.Repo == nil {
		return nil
	}
	dir := path.Clean(strings.TrimPrefix(ref, "./"))
	if !fs.ValidPath(dir) {
		return nil
	}
	for _, name := range []string{"action.yml", "action.yaml"} {
		if b, err := readLimited(s.in.Repo, path.Join(dir, name)); err == nil {
			if a, err := actions.Parse(b); err == nil {
				return a
			}
			return nil
		}
	}
	return nil
}

// compositeContext is the context the steps of a composite action see: the
// caller's, with inputs set from the calling step's `with` (or the action's
// defaults). An input that can't be evaluated yet is left out, so it reads as
// null instead of leaking a placeholder into a version.
func (s *scanner) compositeContext(act *actions.Action, st *workflow.Step) *expr.Context {
	outer := s.in.Context
	ctxs := maps.Clone(outer.Contexts)
	if ctxs == nil {
		ctxs = map[string]any{}
	}
	inputs := value.Object{}
	for _, in := range act.Inputs {
		raw, ok := st.With.Lookup(in.Name)
		if !ok {
			raw = in.Default
		}
		if raw == nil {
			continue
		}
		if v, err := expr.EvalValue(raw, outer); err == nil {
			inputs = inputs.Set(in.Name, v)
		}
	}
	ctxs["inputs"] = inputs
	// Raccoon CI's machines are Linux, whatever the workflow's runs-on says,
	// and steps guarded by runner.os == 'Windows' or 'macOS' will not run.
	if _, ok := ctxs["runner"]; !ok {
		ctxs["runner"] = value.Object{{Key: "os", Value: "Linux"}, {Key: "arch", Value: "X64"}}
	}
	return &expr.Context{Contexts: ctxs, Status: &expr.Status{}}
}

func (s *scanner) setup(where, action, ref string, sa *catalog.ActionDef, st *workflow.Step) {
	tc := sa.Setup
	defer func() { s.provided[tc] = true }()
	demand := func(version, reason string, implicit bool) {
		s.add(Demand{Kind: Toolchain, Name: tc, Version: version, Implicit: implicit, Reason: reason})
	}

	if raw, ok := st.With.Lookup(sa.VersionInput); ok && raw != nil {
		v, err := expr.EvalValue(raw, s.in.Context)
		if err != nil {
			demand("", fmt.Sprintf("%s: %s %s (known at run time)", where, action, sa.VersionInput), false)
			return
		}
		s.checkFloatVersion(where, sa.VersionInput, tc, v)
		for _, line := range strings.Split(expr.ToString(v), "\n") { // setup-python accepts one per line
			if line = strings.TrimSpace(line); line != "" {
				demand(line, fmt.Sprintf("%s: %s %s", where, action, sa.VersionInput), false)
			}
		}
		return
	}
	if raw, ok := st.With.Lookup(sa.FileInput); ok && sa.FileInput != "" && raw != nil {
		file := expr.InterpolateLoose(expr.ToString(raw), s.in.Context, placeholder)
		reason := fmt.Sprintf("%s: %s %s %s", where, action, sa.FileInput, file)
		if s.in.Repo == nil || strings.Contains(file, placeholder) {
			demand("", reason+" (read at run time)", false)
			return
		}
		if v, ok := s.readVersionFile(file, tc); ok {
			demand(v, reason, false)
		} else {
			s.warn(fmt.Sprintf("%s: %s names %s, which doesn't exist or pins no %s version", where, sa.FileInput, file, s.cat.Title(tc)))
		}
		return
	}
	if sa.IsVersionRef(ref) {
		demand(ref, fmt.Sprintf("%s: %s@%s", where, action, ref), false)
		return
	}
	if s.in.Repo != nil {
		for _, f := range sa.DefaultFiles {
			if v, ok := s.readVersionFile(f, tc); ok {
				demand(v, fmt.Sprintf("%s: %s reads %s", where, action, f), false)
				return
			}
		}
	}
	// No version anywhere: the action falls back to the preinstalled one.
	if s.in.Image != nil && s.in.Image.Tools[tc] != "" {
		demand(s.in.Image.Tools[tc], fmt.Sprintf("%s: %s without a version uses the preinstalled %s", where, action, s.cat.Title(tc)), true)
	}
}

// checkFloatVersion catches versions YAML turned into numbers: 3.10 is read
// as 3.1 (on GitHub too). The catalog lists, per toolchain, versions so old
// that a number like that almost certainly lost a trailing zero.
func (s *scanner) checkFloatVersion(where, input, tc string, v any) {
	f, ok := v.(float64)
	def := s.cat.Toolchains[tc]
	if !ok || def == nil {
		return
	}
	if str := expr.ToString(f); slices.Contains(def.LossyFloats, str) {
		s.warn(fmt.Sprintf("%s: %s is the YAML number %s; YAML reads 3.10 as 3.1 (on GitHub too), so quote the version, e.g. '%s0', if that's what you meant",
			where, input, str, str))
	}
}

func (s *scanner) run(where string, st *workflow.Step) {
	shell := st.Shell
	for _, d := range []string{s.in.Job.Defaults.Shell, s.in.Workflow.Defaults.Shell, "bash"} {
		if shell == "" {
			shell = d
		}
	}
	shell = expr.InterpolateLoose(shell, s.in.Context, placeholder)
	prog := path.Base(strings.Fields(shell + " x")[0])

	var lang syntax.LangVariant
	switch prog {
	case "bash":
		lang = syntax.LangBash
	case "sh":
		lang = syntax.LangPOSIX
	default: // python, pwsh, perl {0}, ...
		if r, ok := s.cat.Command(prog); ok {
			s.need(where, prog, r)
		}
		return
	}

	script := expr.InterpolateLoose(st.Run, s.in.Context, placeholder)
	f, err := syntax.NewParser(syntax.Variant(lang)).Parse(strings.NewReader(script), "")
	if err != nil {
		s.warn(fmt.Sprintf("%s: couldn't parse the script (%v); its commands aren't checked for missing tools", where, err))
		return
	}
	funcs := map[string]bool{}
	syntax.Walk(f, func(n syntax.Node) bool {
		if fd, ok := n.(*syntax.FuncDecl); ok && fd.Name != nil {
			funcs[fd.Name.Value] = true
		}
		return true
	})
	syntax.Walk(f, func(n syntax.Node) bool {
		if ce, ok := n.(*syntax.CallExpr); ok {
			s.call(where, ce, funcs)
		}
		return true
	})
}

// call handles one simple command of a script.
func (s *scanner) call(where string, ce *syntax.CallExpr, funcs map[string]bool) {
	args := make([]string, len(ce.Args))
	for i, w := range ce.Args {
		args[i] = w.Lit() // "" unless the word is a plain literal
	}
	args = s.skipWrappers(args)
	if len(args) == 0 || args[0] == "" {
		return
	}
	cmd, rest := args[0], args[1:]
	switch {
	case funcs[cmd]:
		return
	case strings.Contains(cmd, "/") || s.cat.IsOpaque(cmd):
		if s.opaque == "" {
			s.opaque = fmt.Sprintf("%s runs %s", where, cmd)
		}
		if strings.Contains(cmd, "/") {
			return
		}
	}
	s.checkCompat(where, cmd, rest)
	for _, pkg := range s.cat.Installs(cmd, rest) {
		s.provided[pkg] = true // the job installs it itself
	}
	if r, ok := s.cat.Command(cmd); ok {
		s.need(where, cmd, r)
	}
}

// skipWrappers drops sudo, env VAR=x, timeout 5m and the like, as the
// catalog describes them.
func (s *scanner) skipWrappers(args []string) []string {
	for len(args) > 0 {
		w := s.cat.Wrapper(args[0])
		if w == nil {
			break
		}
		args = args[1:]
		for len(args) > 0 && (strings.HasPrefix(args[0], "-") || w.SkipAssignments && strings.Contains(args[0], "=")) {
			args = args[1:]
		}
		args = args[min(w.SkipArgs, len(args)):]
	}
	return args
}

// checkCompat applies the catalog's warnings about commands that behave
// differently on Raccoon CI's machines than on GitHub's Ubuntu images.
func (s *scanner) checkCompat(where, cmd string, args []string) {
	image := ""
	if s.in.Image != nil {
		image = s.in.Image.Image
	}
	for _, w := range s.cat.CompatWarnings {
		if !slices.Contains(w.Commands, cmd) {
			continue
		}
		if w.ArgPrefix == "" {
			s.warn(where + ": " + s.cat.CompatMessage(w, image, ""))
			continue
		}
		for _, a := range args {
			if strings.HasPrefix(a, w.ArgPrefix) {
				s.warn(where + ": " + s.cat.CompatMessage(w, image, a))
			}
		}
	}
}

// need records an implicit demand for what cmd requires, if the GitHub
// image preinstalls it and no earlier step provided it.
func (s *scanner) need(where, cmd string, r catalog.Requirement) {
	if s.in.Image == nil || s.provided[r.Name] || s.has[r.Kind.String()+"/"+r.Name] {
		return
	}
	img := s.in.Image
	d := Demand{Kind: r.Kind, Name: r.Name, Implicit: true}
	switch r.Kind {
	case Toolchain:
		if d.Version = img.Tools[r.Name]; d.Version == "" {
			return
		}
		d.Reason = fmt.Sprintf("%s runs %s without setting up %s", where, cmd, s.cat.Title(r.Name))
	case Package:
		if !slices.Contains(img.Packages, r.OnImage) {
			return
		}
		d.Reason = fmt.Sprintf("%s runs %s", where, cmd)
	case Tool:
		if !slices.Contains(img.Software, r.OnImage) {
			return
		}
		d.Reason = fmt.Sprintf("%s runs %s", where, cmd)
	}
	if tc := s.cat.Requires(r.Name); tc != "" {
		s.need(where, cmd, catalog.Requirement{Kind: Toolchain, Name: tc})
	}
	s.add(d)
}

// manifests covers opaque build commands (make, ./build.sh): if the repo has
// go.mod and the job runs make, it most likely needs Go. Installing a
// toolchain that turns out unused costs a download; a missing one fails the job.
func (s *scanner) manifests() {
	if s.opaque == "" || !s.checkout || s.in.Repo == nil || s.in.Image == nil {
		return
	}
	for _, tc := range s.cat.ToolchainNames() {
		if s.provided[tc] || s.has["toolchain/"+tc] {
			continue
		}
		for _, file := range s.cat.Toolchains[tc].Manifests {
			if _, err := fs.Stat(s.in.Repo, file); err != nil {
				continue
			}
			if v := s.in.Image.Tools[tc]; v != "" {
				s.add(Demand{Kind: Toolchain, Name: tc, Version: v, Implicit: true,
					Reason: fmt.Sprintf("the repo has %s and %s", file, s.opaque)})
			}
			break
		}
	}
}
