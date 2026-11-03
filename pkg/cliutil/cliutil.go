// Package cliutil holds the small helpers the raccoonci commands share: where
// a workflow's repository is, what event to match it against, which jobs the
// changed files leave out.
package cliutil

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Raccoon-CI/lib/pkg/affected"
	"github.com/Raccoon-CI/lib/pkg/catalog"
	"github.com/Raccoon-CI/lib/pkg/trigger"
	"github.com/Raccoon-CI/lib/pkg/value"
	"github.com/Raccoon-CI/lib/pkg/workflow"
	"github.com/Raccoon-CI/runner/pkg/cliflag"
)

func AbsPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func KeyValues(flagName string, list []string) (value.Object, error) {
	var o value.Object
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%s %q: want NAME=VALUE", flagName, kv)
		}
		o = o.Set(k, v)
	}
	return o, nil
}

func GitTopLevel(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) || errors.Is(err, exec.ErrNotFound) {
			abs, _ := filepath.Abs(dir)
			return abs
		}
	}
	return strings.TrimSpace(string(out))
}

// RepoDirName turns "owner/name" into a directory name that cannot collide
// with another repository's, whatever characters the names hold.
func RepoDirName(repository string) string {
	sum := sha256.Sum256([]byte(repository))
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, repository)
	return hex.EncodeToString(sum[:4]) + "-" + safe
}

// RepoRoot finds the repository a workflow file belongs to, by its
// .github/workflows location.
func RepoRoot(workflowPath string) (string, bool) {
	abs, err := filepath.Abs(workflowPath)
	if err != nil {
		return "", false
	}
	dir := filepath.Dir(abs)
	if filepath.Base(dir) != "workflows" || filepath.Base(filepath.Dir(dir)) != ".github" {
		return "", false
	}
	return filepath.Dir(filepath.Dir(dir)), true
}

// Unaffected builds engine.Options.Unaffected from the changed files: a job is
// left out when none of them concerns it and leaving it out is safe. The
// directories package discovery skips are those of the catalog in use, so a
// catalog laid over the built-in one (-catalog, a feed) applies here too.
func Unaffected(w *workflow.Workflow, repo fs.FS, wfPath string, files []string, cat *catalog.Catalog) (func(*workflow.Job) (string, bool), error) {
	cfg, err := affected.LoadConfig(repo)
	if err != nil {
		return nil, err
	}
	decisions := affected.Decide(affected.Input{Workflow: w, WorkflowPath: wfPath, Files: files, Repo: repo, Config: cfg, SkipDirs: affected.SkipDirs(cat)})
	return func(j *workflow.Job) (string, bool) {
		d, ok := decisions[j.ID]
		if !ok || d.Affected {
			return "", false
		}
		if safe, _ := affected.Skippable(j); !safe {
			return "", false
		}
		return "none of the " + fmt.Sprint(len(files)) + " changed files concerns it (" + d.Reason + ")", true
	}, nil
}

// EventFlags describe the event to match a workflow against, and default to
// what the local checkout says: its branch, and the files changed by its
// last commit.
type EventFlags struct {
	Name, action, ref, baseRef, before, cron string
	Files                                    cliflag.List
}

func (f *EventFlags) Register(fs *flag.FlagSet, withEventName bool) {
	if withEventName {
		fs.StringVar(&f.Name, "event", "push", "the event: push, pull_request, workflow_dispatch, schedule, ...")
	}
	fs.StringVar(&f.action, "action", "", "the activity type (opened, labeled, ...); empty means the event's default")
	fs.StringVar(&f.ref, "ref", "", "the ref a push is about, e.g. refs/heads/main or refs/tags/v1 (default: this checkout's branch)")
	fs.StringVar(&f.baseRef, "base-ref", "", "the branch a pull request targets (default: main)")
	fs.StringVar(&f.before, "before", "", "the git revision the changed files are counted from (default: HEAD~1 for a push, the base branch for a pull request)")
	fs.Var(&f.Files, "file", "a changed file, instead of asking git (repeatable)")
	fs.StringVar(&f.cron, "cron", "", "for the schedule event: the cron expression that fired")
}

// event builds the trigger.Event, asking git in repo for what was not given.
func (f *EventFlags) Event(repo, name string) trigger.Event {
	ev := trigger.Event{Name: name, Action: f.action, Ref: f.ref, BaseRef: f.baseRef, Cron: f.cron}
	if ev.Ref == "" {
		if b := gitOut(repo, "symbolic-ref", "-q", "HEAD"); b != "" {
			ev.Ref = b
		}
	}
	if name == "pull_request" || name == "pull_request_target" {
		if ev.BaseRef == "" {
			ev.BaseRef = "main"
		}
	}
	switch {
	case len(f.Files) > 0:
		ev.Files = append([]string{}, f.Files...)
	case name == "push":
		before := f.before
		if before == "" {
			before = "HEAD~1"
		}
		ev.Files = gitFiles(repo, before+"..HEAD")
	case name == "pull_request" || name == "pull_request_target":
		base := f.before
		if base == "" {
			base = ev.BaseRef
		}
		ev.Files = gitFiles(repo, base+"...HEAD")
	}
	return ev
}

func gitOut(repo string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitFiles lists the files changed in a revision range, or nil if git cannot
// say (no such revision, not a checkout): the files are then "not known".
func gitFiles(repo, rng string) []string {
	out, err := exec.Command("git", "-C", repo, "diff", "--name-only", "-z", rng).Output()
	if err != nil {
		return nil
	}
	files := []string{}
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}
