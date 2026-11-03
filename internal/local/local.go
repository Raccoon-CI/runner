// Package local supports `raccoonci run`: running a workflow from a local
// checkout on this machine. It builds the github context from git, replaces
// actions/checkout of the repository with a copy of the working tree (so
// uncommitted changes are tested), and prints run events to a terminal.
package local

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Raccoon-CI/lib/pkg/expr"
	"github.com/Raccoon-CI/lib/pkg/value"
	"github.com/Raccoon-CI/runner/pkg/runner"
)

func git(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var githubRemote = regexp.MustCompile(`github\.com[:/]([^/]+)/([^/]+?)(?:\.git)?/?$`)

// Github builds the github context for running workflowPath (inside repo)
// as if event had happened at the repository's current HEAD.
func Github(repo, workflowPath, workflowName, event string) value.Object {
	sha := git(repo, "rev-parse", "HEAD")
	if sha == "" {
		sha = strings.Repeat("0", 40)
	}
	ref := git(repo, "symbolic-ref", "-q", "HEAD")
	refType, refName := "branch", strings.TrimPrefix(ref, "refs/heads/")
	if ref == "" { // detached HEAD
		ref, refName, refType = sha, sha, ""
	}
	owner, name := "local", filepath.Base(repo)
	if m := githubRemote.FindStringSubmatch(git(repo, "remote", "get-url", "origin")); m != nil {
		owner, name = m[1], m[2]
	}
	actor := git(repo, "config", "user.name")
	if u, err := user.Current(); actor == "" && err == nil {
		actor = u.Username
	}
	repository := owner + "/" + name
	rel, err := filepath.Rel(repo, workflowPath)
	if err != nil {
		rel = filepath.Base(workflowPath)
	}
	if workflowName == "" {
		workflowName = filepath.ToSlash(rel)
	}

	repoObj := value.Object{
		{Key: "full_name", Value: repository},
		{Key: "name", Value: name},
		{Key: "owner", Value: value.Object{{Key: "login", Value: owner}}},
		{Key: "default_branch", Value: refName},
	}
	payload := value.Object{{Key: "ref", Value: ref}, {Key: "repository", Value: repoObj}}
	switch event {
	case "push":
		payload = append(payload,
			value.Field{Key: "before", Value: strings.Repeat("0", 40)},
			value.Field{Key: "after", Value: sha},
			value.Field{Key: "head_commit", Value: value.Object{
				{Key: "id", Value: sha},
				{Key: "message", Value: git(repo, "log", "-1", "--format=%B")},
			}},
			value.Field{Key: "pusher", Value: value.Object{{Key: "name", Value: actor}}},
		)
	case "workflow_dispatch":
		payload = append(payload, value.Field{Key: "inputs", Value: value.Object{}})
	}

	runID := strconv.FormatInt(time.Now().Unix(), 10)
	return value.Object{
		{Key: "event_name", Value: event},
		{Key: "event", Value: payload},
		{Key: "sha", Value: sha},
		{Key: "ref", Value: ref},
		{Key: "ref_name", Value: refName},
		{Key: "ref_type", Value: refType},
		{Key: "ref_protected", Value: "false"},
		{Key: "head_ref", Value: ""},
		{Key: "base_ref", Value: ""},
		{Key: "repository", Value: repository},
		{Key: "repository_owner", Value: owner},
		{Key: "actor", Value: actor},
		{Key: "triggering_actor", Value: actor},
		{Key: "workflow", Value: workflowName},
		{Key: "workflow_ref", Value: repository + "/" + filepath.ToSlash(rel) + "@" + ref},
		{Key: "workflow_sha", Value: sha},
		{Key: "run_id", Value: runID},
		{Key: "run_number", Value: "1"},
		{Key: "run_attempt", Value: "1"},
		{Key: "retention_days", Value: "90"},
		{Key: "server_url", Value: "https://github.com"},
		{Key: "api_url", Value: "https://api.github.com"},
		{Key: "graphql_url", Value: "https://api.github.com/graphql"},
	}
}

// Checkout returns a runner.Config.Checkout hook that copies the working
// tree of repo (named repository on GitHub) instead of cloning it. Checkouts
// of other repositories run the real action.
func Checkout(repo, repository string) func(context.Context, string, value.Object) (bool, error) {
	return func(_ context.Context, workspace string, with value.Object) (bool, error) {
		if r, ok := with.Lookup("repository"); ok && r != "" && !strings.EqualFold(expr.ToString(r), repository) {
			return false, nil
		}
		dest := workspace
		if p, ok := with.Lookup("path"); ok && expr.ToString(p) != "" {
			dest = filepath.Join(workspace, filepath.Clean("/"+expr.ToString(p)))
		}
		return true, copyTree(repo, dest)
	}
}

// copyTree copies src into dest, keeping modes and symlinks. It skips dest
// itself in case it lies inside src.
func copyTree(src, dest string) error {
	absDest, _ := filepath.Abs(dest)
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if abs, _ := filepath.Abs(p); abs == absDest {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dest, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			os.Remove(target)
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			return copyFile(p, target, info.Mode().Perm())
		}
		return nil // sockets, devices
	})
}

func copyFile(src, dest string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Printer writes run events as terminal lines, prefixed with the job name.
type Printer struct {
	Out   io.Writer
	Debug bool

	mu     sync.Mutex
	starts map[string]time.Time
	groups map[string]int
}

var marks = map[runner.Conclusion]string{
	runner.Success: "✓", runner.Failure: "✗", runner.Cancelled: "⊘", runner.Skipped: "-",
}

func (p *Printer) Event(e runner.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.starts == nil {
		p.starts, p.groups = map[string]time.Time{}, map[string]int{}
	}
	key := e.Job + "\x00" + e.Step
	pad := strings.Repeat("  ", e.Depth)
	line := func(format string, args ...any) {
		fmt.Fprintf(p.Out, "[%s] %s%s\n", e.Job, pad, fmt.Sprintf(format, args...))
	}
	switch e.Kind {
	case runner.JobStarted:
		p.starts[e.Job] = e.Time
		line("job started")
	case runner.StepStarted:
		p.starts[key] = e.Time
		line("▶ %s", e.Step)
	case runner.Output:
		line("  | %s%s", strings.Repeat("  ", p.groups[key]), e.Text)
	case runner.GroupStart:
		line("  | %s▸ %s", strings.Repeat("  ", p.groups[key]), e.Text)
		p.groups[key]++
	case runner.GroupEnd:
		if p.groups[key] > 0 {
			p.groups[key]--
		}
	case runner.Debug:
		if p.Debug {
			line("  debug: %s", e.Text)
		}
	case runner.Notice, runner.Warning, runner.Error:
		kind := map[runner.EventKind]string{runner.Notice: "notice", runner.Warning: "warning", runner.Error: "error"}[e.Kind]
		where := ""
		if f := e.Props["file"]; f != "" {
			where = " (" + f
			if l := e.Props["line"]; l != "" {
				where += ":" + l
			}
			where += ")"
		}
		title := ""
		if t := e.Props["title"]; t != "" {
			title = t + ": "
		}
		indent := "  "
		if e.Step == "" {
			indent = ""
		}
		line("%s%s: %s%s%s", indent, kind, title, e.Text, where)
	case runner.StepFinished:
		took := ""
		if start, ok := p.starts[key]; ok {
			took = fmt.Sprintf(" (%s)", e.Time.Sub(start).Round(100*time.Millisecond))
		}
		line("%s %s%s", marks[e.Conclusion], e.Step, took)
	case runner.JobFinished:
		took := ""
		if start, ok := p.starts[e.Job]; ok {
			took = fmt.Sprintf(" in %s", e.Time.Sub(start).Round(100*time.Millisecond))
		}
		line("%s job %s%s", marks[e.Conclusion], e.Conclusion, took)
	}
}
