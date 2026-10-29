package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"raccoonci/internal/docker"
	"raccoonci/internal/expr"
)

const (
	// pipeGrace is how long a finished step waits for background processes
	// that still hold its output open. A step like `server &` finishes
	// when its shell exits, as on GitHub; the server is killed at job end.
	pipeGrace = time.Second
	// killGrace is how long a cancelled step gets between SIGINT and
	// SIGKILL, as on GitHub.
	killGrace = 7500 * time.Millisecond
	// maxLine splits pathological output without newlines.
	maxLine = 1 << 20
)

// exec runs a step's process in its own process group and returns the
// step's outcome. Cancelling ctx interrupts the whole group.
func (r *jobRun) exec(ctx context.Context, st *stepState, argv []string, dir string, env []string) Conclusion {
	var cmd *exec.Cmd
	cid := r.containerID()
	pidFile := ""
	if cid != "" {
		// Inside the job container. `docker exec` forwards no signals, so the
		// process is started through a shell that records its pid (which is
		// also its process group, docker makes every exec a group leader) in
		// a file both sides see; cancelling signals it with another exec.
		if f, err := os.CreateTemp(r.temp, "pid-*"); err == nil {
			f.Close()
			if os.Chmod(f.Name(), 0o666) == nil { // the step may run as another user
				pidFile = f.Name()
				defer os.Remove(pidFile)
			}
		}
		if pidFile != "" && r.hasShell(cid) {
			argv = append([]string{"/bin/sh", "-c", `echo $$ >"$0" && exec "$@"`, pidFile}, argv...)
		} else {
			pidFile = ""
		}
		var cleanup func()
		var err error
		if cmd, cleanup, err = r.dkr.ExecCmd(context.Background(), cid, docker.ExecOpts{Dir: dir, Env: env}, argv); err != nil {
			r.emitStep(st, Error, err.Error(), nil)
			return Failure
		}
		defer cleanup()
	} else {
		cmd = exec.Command(argv[0], argv[1:]...)
		cmd.Dir, cmd.Env = dir, env
	}
	return r.runCmd(ctx, st, cmd, cid, pidFile, nil)
}

// runCmd runs a prepared command as a step's process: output lines become
// events (and workflow commands), cancelling ctx interrupts its process
// group and kills it after the grace period, and the step's file commands are
// applied at the end. For a step in the job container cid and pidFile say how
// to signal it there. onKill, if set, is called when the grace period runs
// out, for processes that outlive their client (a docker container).
func (r *jobRun) runCmd(ctx context.Context, st *stepState, cmd *exec.Cmd, cid, pidFile string, onKill func()) Conclusion {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var mu sync.Mutex // one line at a time across stdout and stderr
	handle := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		r.handleLine(st, line)
	}
	stdout, stderr := &lineWriter{fn: handle}, &lineWriter{fn: handle}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = pipeGrace
	if err := cmd.Start(); err != nil {
		r.emitStep(st, Error, err.Error(), nil)
		return Failure
	}
	pgid := cmd.Process.Pid
	r.mu.Lock()
	r.pgids = append(r.pgids, pgid)
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		// For a container step the signal goes to the process in the
		// container, and the docker client leaves when that process does.
		// Only if that cannot be done is the client itself signalled.
		signal := func(sig syscall.Signal) {
			if pidFile == "" || !r.signalInContainer(cid, pidFile, sig) {
				_ = syscall.Kill(-pgid, sig)
			}
		}
		select {
		case <-ctx.Done():
			signal(syscall.SIGINT)
			select {
			case <-time.After(killGrace):
				signal(syscall.SIGKILL)
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
				if onKill != nil {
					onKill()
				}
			case <-done:
			}
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	stdout.flush()
	stderr.flush()
	// Like GitHub, apply file commands even when the step failed, so outputs
	// it set before failing reach later steps.
	filesOK := r.processFileCommands(st)

	if ctx.Err() != nil {
		return Cancelled // the caller tells a timeout from a cancellation
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState.Success():
	case errors.As(err, &exitErr):
		r.emitStep(st, Error, fmt.Sprintf("Process completed with exit code %d.", exitErr.ExitCode()), nil)
		return Failure
	default:
		r.emitStep(st, Error, err.Error(), nil)
		return Failure
	}
	if !filesOK {
		return Failure
	}
	r.mu.Lock()
	failed := st.failed
	r.mu.Unlock()
	if failed {
		return Failure
	}
	return Success
}

// lineWriter calls fn for each line written to it.
type lineWriter struct {
	buf bytes.Buffer
	fn  func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		i := bytes.IndexByte(w.buf.Bytes(), '\n')
		if i < 0 {
			if w.buf.Len() > maxLine {
				w.fn(w.buf.String())
				w.buf.Reset()
			}
			return len(p), nil
		}
		line := string(w.buf.Next(i + 1))
		w.fn(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"))
	}
}

func (w *lineWriter) flush() {
	if w.buf.Len() > 0 {
		w.fn(strings.TrimSuffix(w.buf.String(), "\r"))
		w.buf.Reset()
	}
}

// handleLine processes one line of step output: a workflow command, or
// output to log with secrets masked.
func (r *jobRun) handleLine(st *stepState, line string) {
	r.mu.Lock()
	stop, echo := st.stopToken, st.echo
	r.mu.Unlock()
	if stop != "" {
		if strings.TrimSpace(line) == "::"+stop+"::" {
			r.mu.Lock()
			st.stopToken = ""
			r.mu.Unlock()
			return
		}
		r.emitStep(st, Output, line, nil)
		return
	}
	c, ok := parseCommand(line)
	if !ok {
		r.emitStep(st, Output, line, nil)
		return
	}
	if echo {
		r.emitStep(st, Output, line, nil)
	}
	fail := func(format string, args ...any) {
		r.emitStep(st, Error, fmt.Sprintf("Unable to process command '%s' successfully. ", line)+fmt.Sprintf(format, args...), nil)
		r.mu.Lock()
		st.failed = true
		r.mu.Unlock()
	}
	switch c.name {
	case "add-mask":
		r.masker.add(c.data)
	case "set-output":
		r.emitStep(st, Warning, "The `set-output` command is deprecated and will be disabled soon. Please upgrade to using Environment Files.", nil)
		if c.props["name"] == "" {
			fail("Required property 'name' is missing.")
			return
		}
		r.mu.Lock()
		st.outputs = st.outputs.Set(c.props["name"], c.data)
		r.mu.Unlock()
	case "save-state":
		if c.props["name"] == "" {
			fail("Required property 'name' is missing.")
			return
		}
		r.mu.Lock()
		st.state[c.props["name"]] = c.data
		r.mu.Unlock()
	case "set-env", "add-path":
		if !r.unsecureCommandsAllowed() {
			fail("The `%s` command is disabled. Please upgrade to using Environment Files or opt into unsecure command execution by setting the `ACTIONS_ALLOW_UNSECURE_COMMANDS` environment variable to `true`.", c.name)
			return
		}
		r.mu.Lock()
		if c.name == "set-env" {
			r.env = r.env.Set(c.props["name"], c.data)
		} else {
			r.path = append([]string{c.data}, r.path...)
		}
		r.mu.Unlock()
	case "debug":
		r.emitStep(st, Debug, c.data, nil)
	case "notice":
		r.emitStep(st, Notice, c.data, c.props)
	case "warning":
		r.emitStep(st, Warning, c.data, c.props)
	case "error":
		r.emitStep(st, Error, c.data, c.props)
	case "group":
		r.emitStep(st, GroupStart, c.data, nil)
	case "endgroup":
		r.emitStep(st, GroupEnd, "", nil)
	case "echo":
		switch strings.ToLower(strings.TrimSpace(c.data)) {
		case "on":
			r.mu.Lock()
			st.echo = true
			r.mu.Unlock()
		case "off":
			r.mu.Lock()
			st.echo = false
			r.mu.Unlock()
		default:
			fail("Invalid echo command value. Possible values can be: 'on', 'off'.")
		}
	case "stop-commands":
		if c.data == "" {
			fail("Invalid stop-commands token.")
			return
		}
		r.mu.Lock()
		st.stopToken = c.data
		r.mu.Unlock()
	case "add-matcher", "remove-matcher":
		// Problem matchers turn tool output into annotations. Not yet:
		// the output is still logged as is.
	}
}

func (r *jobRun) unsecureCommandsAllowed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, _ := r.env.Get("ACTIONS_ALLOW_UNSECURE_COMMANDS")
	return strings.EqualFold(expr.ToString(v), "true")
}

// processFileCommands applies what the step wrote to $GITHUB_ENV,
// $GITHUB_PATH, $GITHUB_OUTPUT, $GITHUB_STATE and $GITHUB_STEP_SUMMARY.
func (r *jobRun) processFileCommands(st *stepState) bool {
	ok := true
	fail := func(file string, err error) {
		r.emitStep(st, Error, fmt.Sprintf("$GITHUB_%s: %v", strings.ToUpper(file), err), nil)
		ok = false
	}
	read := func(name string) []byte {
		data, err := os.ReadFile(st.files[name])
		if err != nil {
			fail(name, err)
		}
		return data
	}

	for _, name := range []string{"env", "output", "state"} {
		pairs, err := parseKeyValueFile(read(name))
		if err != nil {
			fail(name, err)
			continue
		}
		r.mu.Lock()
		for _, kv := range pairs {
			switch name {
			case "env":
				if strings.EqualFold(kv[0], "NODE_OPTIONS") {
					r.mu.Unlock()
					fail(name, errors.New("can't set NODE_OPTIONS through $GITHUB_ENV"))
					r.mu.Lock()
					continue
				}
				r.env = r.env.Set(kv[0], kv[1])
			case "output":
				st.outputs = st.outputs.Set(kv[0], kv[1])
			case "state":
				st.state[kv[0]] = kv[1]
			}
		}
		r.mu.Unlock()
	}

	paths := parsePathFile(read("path"))
	r.mu.Lock()
	for _, p := range paths {
		r.path = append([]string{p}, deleteString(r.path, p)...)
	}
	r.mu.Unlock()

	if summary := read("step_summary"); len(summary) > maxSummary {
		r.emitStep(st, Error, fmt.Sprintf("$GITHUB_STEP_SUMMARY upload aborted, supports content up to a size of %dk, got %dk.", maxSummary>>10, len(summary)>>10), nil)
	} else if len(summary) > 0 {
		r.mu.Lock()
		r.summary.WriteString(r.masker.mask(string(summary)))
		r.mu.Unlock()
	}
	return ok
}

func deleteString(list []string, s string) []string {
	out := list[:0:0]
	for _, e := range list {
		if e != s {
			out = append(out, e)
		}
	}
	return out
}
