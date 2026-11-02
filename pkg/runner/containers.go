package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/docker"
	"github.com/Raccoon-CI/raccoonci/internal/expr"
	"github.com/Raccoon-CI/raccoonci/internal/value"
)

// Container jobs and service containers.
//
// A job with container: runs its steps inside that image, with `docker exec`
// into a long-lived container. A job with services: gets one container per
// service, all on a network made for the job. Services are reached by their id
// from a container job and through published ports on 127.0.0.1 from a host job.
//
// The job's directories, the tool cache and the downloaded actions are bind
// mounted at the same paths inside the container as on the host, so file
// commands, working directories and action paths need no translation.

const (
	// initStep is the name of the pseudo step that brings the containers up.
	initStep = "Initialize containers"
	// defaultPath is used when an image does not set PATH.
	defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	dockerSock  = "/var/run/docker.sock"
)

// How long a service may take to become healthy, and how often to look. Vars
// so tests can speed them up.
var (
	healthTimeout = 5 * time.Minute
	healthPoll    = 2 * time.Second
	// A service without a health check is taken as ready once it has stayed
	// up this long, which catches one that dies on start (a bad command, a
	// missing variable) before steps run against it.
	settleTime = time.Second
)

var serviceID = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,39}$`)

type containerSpec struct {
	image    string
	username string
	password string
	env      []string // NAME=value
	ports    []string
	volumes  []string
	options  []string
}

type namedSpec struct {
	id string
	*containerSpec
}

// parseSpec reads an evaluated container: or service entry: an image name or
// a mapping. A nil result means there is none.
func parseSpec(v any) (*containerSpec, error) {
	switch v := v.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		s := &containerSpec{image: strings.TrimSpace(v)}
		return s, s.check()
	case value.Object:
		s := &containerSpec{}
		for _, f := range v {
			var err error
			switch f.Key {
			case "image":
				s.image = strings.TrimSpace(expr.ToString(f.Value))
			case "credentials":
				o, ok := f.Value.(value.Object)
				if !ok {
					return nil, errors.New("credentials: expected a mapping")
				}
				for _, c := range o {
					switch c.Key {
					case "username":
						s.username = expr.ToString(c.Value)
					case "password":
						s.password = expr.ToString(c.Value)
					default:
						return nil, fmt.Errorf("credentials: unknown key %q", c.Key)
					}
				}
			case "env":
				o, ok := f.Value.(value.Object)
				if f.Value != nil && !ok {
					return nil, errors.New("env: expected a mapping")
				}
				for _, e := range o {
					if e.Key == "" || strings.ContainsAny(e.Key, "=\x00\n") {
						return nil, fmt.Errorf("env: bad variable name %q", e.Key)
					}
					s.env = append(s.env, e.Key+"="+expr.ToString(e.Value))
				}
			case "ports":
				s.ports, err = stringList(f.Value)
			case "volumes":
				s.volumes, err = stringList(f.Value)
			case "options":
				s.options, err = docker.SplitOptions(expr.ToString(f.Value))
			default:
				return nil, fmt.Errorf("unknown key %q", f.Key)
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.Key, err)
			}
		}
		return s, s.check()
	}
	return nil, fmt.Errorf("expected an image name or a mapping, got %s", expr.ToString(v))
}

func (s *containerSpec) check() error {
	switch {
	case s.image == "":
		return errors.New("image is required")
	case strings.HasPrefix(s.image, "-") || strings.ContainsAny(s.image, " \t\r\n\x00"):
		return fmt.Errorf("bad image name %q", s.image)
	}
	return nil
}

func stringList(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	l, ok := v.([]any)
	if !ok {
		return nil, errors.New("expected a list")
	}
	out := make([]string, 0, len(l))
	for _, e := range l {
		s := expr.ToString(e)
		if s == "" {
			return nil, errors.New("empty entry")
		}
		out = append(out, s)
	}
	return out, nil
}

// normalizePort binds a published port to the loopback interface unless the
// workflow names an address. GitHub's runners publish on every interface; a
// machine that is somebody's laptop or a shared box should not.
func normalizePort(p string) string {
	switch len(strings.Split(p, ":")) {
	case 1:
		return "127.0.0.1::" + p
	case 2:
		return "127.0.0.1:" + p
	}
	return p
}

// registryOf is the registry an image is pulled from, "" for Docker Hub.
func registryOf(image string) string {
	first, _, ok := strings.Cut(image, "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return first
	}
	return ""
}

// mergeEnv returns base with the NAME=value entries of over replacing or
// adding to it.
func mergeEnv(base, over []string) []string {
	out := append([]string(nil), base...)
	for _, kv := range over {
		name, _, _ := strings.Cut(kv, "=")
		replaced := false
		for i, e := range out {
			if n, _, _ := strings.Cut(e, "="); n == name {
				out[i], replaced = kv, true
				break
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return out
}

// bindOK says whether a host path can go into a -v argument unharmed.
func bindOK(p string) bool {
	return filepath.IsAbs(p) && !strings.ContainsAny(p, ":,\x00\n")
}

func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// evalSpecs evaluates container: and services: and parses them.
func (r *jobRun) evalSpecs(ctx *expr.Context) (job *containerSpec, services []namedSpec, err error) {
	j := r.job.Job
	if j.Container != nil {
		v, err := expr.EvalValue(j.Container, ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("container: %w", err)
		}
		if job, err = parseSpec(v); err != nil {
			return nil, nil, fmt.Errorf("container: %w", err)
		}
	}
	if j.Services != nil {
		v, err := expr.EvalValue(j.Services, ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("services: %w", err)
		}
		obj, ok := v.(value.Object)
		if !ok && v != nil {
			return nil, nil, errors.New("services: expected a mapping")
		}
		for _, f := range obj {
			if !serviceID.MatchString(f.Key) {
				return nil, nil, fmt.Errorf("services: bad service id %q: use letters, digits, '-' and '_'", f.Key)
			}
			s, err := parseSpec(f.Value)
			if err != nil {
				return nil, nil, fmt.Errorf("services.%s: %w", f.Key, err)
			}
			if s == nil {
				return nil, nil, fmt.Errorf("services.%s: no image", f.Key)
			}
			services = append(services, namedSpec{f.Key, s})
		}
	}
	return job, services, nil
}

func (r *jobRun) initOut(format string, a ...any) {
	r.emit(Event{Kind: Output, Step: initStep, Text: fmt.Sprintf(format, a...)})
}

func (r *jobRun) containerID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobCID
}

// startContainers brings up the job's container and services, as the
// "Initialize containers" step. It does nothing for jobs without either.
func (r *jobRun) startContainers(ctx context.Context) error {
	j := r.job.Job
	if j.Container == nil && j.Services == nil {
		return nil
	}
	job, services, err := r.evalSpecs(r.exprCtx(rootScope(r), r.currentEnv(), nil))
	if err != nil {
		return err
	}
	if job == nil && len(services) == 0 {
		return nil
	}
	r.emit(Event{Kind: StepStarted, Step: initStep})
	err = r.bringUp(ctx, job, services)
	conclusion := Success
	if err != nil {
		conclusion = Failure
	}
	r.emit(Event{Kind: StepFinished, Step: initStep, Conclusion: conclusion})
	return err
}

func (r *jobRun) bringUp(ctx context.Context, job *containerSpec, services []namedSpec) error {
	c := r.cfg.Docker
	if c == nil {
		var err error
		if c, err = docker.New(); err != nil {
			return err
		}
	}
	if err := c.Ping(ctx); err != nil {
		return fmt.Errorf("the docker daemon is not reachable: %w", err)
	}
	r.dkr = c
	r.rootless = c.Rootless(ctx)

	prefix := docker.RandomName("raccoonci")
	r.initOut("Creating network %s", prefix)
	if err := c.NetworkCreate(ctx, prefix, "raccoonci=1"); err != nil {
		return err
	}
	r.mu.Lock()
	r.network, r.netMade = prefix, true
	r.mu.Unlock()

	serviceIDs := map[string]string{}
	for _, s := range services {
		r.initOut("Starting service %s (%s)", s.id, s.image)
		id, err := r.startService(ctx, prefix, s)
		if err != nil {
			return fmt.Errorf("service %s: %w", s.id, err)
		}
		serviceIDs[s.id] = id
	}

	if job != nil {
		r.initOut("Starting job container (%s)", job.image)
		if err := r.startJobContainer(ctx, prefix, job); err != nil {
			return fmt.Errorf("container: %w", err)
		}
		r.reachServices(ctx, prefix)
	}

	svcCtx := value.Object{}
	for _, s := range services {
		if err := r.waitReady(ctx, serviceIDs[s.id], s.id); err != nil {
			return err
		}
		ports, err := r.publishedPorts(ctx, serviceIDs[s.id])
		if err != nil {
			return fmt.Errorf("service %s: %w", s.id, err)
		}
		svcCtx = svcCtx.Set(s.id, value.Object{
			{Key: "id", Value: serviceIDs[s.id]},
			{Key: "network", Value: prefix},
			{Key: "ports", Value: ports},
		})
	}
	r.mu.Lock()
	r.ctxServices = svcCtx
	if r.jobCID != "" {
		r.ctxContainer = value.Object{{Key: "id", Value: r.jobCID}, {Key: "network", Value: prefix}}
	}
	r.mu.Unlock()
	return nil
}

// pull fetches a spec's image, logging in first if it has credentials. An
// image that is already present is used (with a warning) if the pull fails.
func (r *jobRun) pull(ctx context.Context, s *containerSpec) error {
	r.initOut("Pulling image %s", s.image)
	cfgDir := ""
	if s.password != "" {
		r.masker.add(s.password)
	}
	if s.username != "" || s.password != "" {
		dir, err := os.MkdirTemp("", "raccoonci-docker-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		if err := r.dkr.Login(ctx, dir, registryOf(s.image), s.username, s.password); err != nil {
			return err
		}
		cfgDir = dir
	}
	if err := r.dkr.Pull(ctx, s.image, cfgDir); err != nil {
		if _, ierr := r.dkr.ImageEnv(ctx, s.image); ierr == nil {
			r.emit(Event{Kind: Warning, Step: initStep, Text: fmt.Sprintf("could not pull %s, using the copy already here: %v", s.image, err)})
			return nil
		}
		return err
	}
	return nil
}

// common is the `docker create` arguments both kinds of container get from
// their spec. The environment is passed to Create separately.
func (r *jobRun) common(prefix, name string, s *containerSpec, forbidden ...string) ([]string, error) {
	args := []string{"--name", name, "--network", prefix, "--label", "raccoonci=1"}
	for _, p := range s.ports {
		args = append(args, "-p", normalizePort(p))
	}
	for _, v := range s.volumes {
		args = append(args, "-v", v)
	}
	if err := docker.CheckOptions(s.options, forbidden...); err != nil {
		return nil, err
	}
	return append(args, s.options...), nil
}

func (r *jobRun) track(id string) {
	r.mu.Lock()
	r.created = append(r.created, id)
	r.mu.Unlock()
}

func (r *jobRun) startService(ctx context.Context, prefix string, s namedSpec) (string, error) {
	if err := r.pull(ctx, s.containerSpec); err != nil {
		return "", err
	}
	args, err := r.common(prefix, prefix+"-"+s.id, s.containerSpec, "--network", "--net", "--name")
	if err != nil {
		return "", err
	}
	args = append(args, "--network-alias", s.id, s.image)
	id, err := r.dkr.Create(ctx, s.env, args...)
	if err != nil {
		return "", err
	}
	r.track(id)
	return id, r.dkr.Start(ctx, id)
}

func (r *jobRun) startJobContainer(ctx context.Context, prefix string, s *containerSpec) error {
	if err := r.pull(ctx, s); err != nil {
		return err
	}
	imageEnv, err := r.dkr.ImageEnv(ctx, s.image)
	if err != nil {
		return err
	}

	mounts := []string{r.workDir}
	if !within(r.workDir, r.toolCache) {
		mounts = append(mounts, r.toolCache)
	}
	var binds []string
	for _, m := range mounts {
		binds = append(binds, m+":"+m)
	}
	for _, m := range r.cfg.Mounts {
		m, err := filepath.Abs(m)
		if err != nil || !bindOK(m) {
			return fmt.Errorf("cannot mount %q", m)
		}
		// Create it here, or docker makes it, owned by root.
		if err := os.MkdirAll(m, 0o755); err != nil {
			return err
		}
		binds = append(binds, m+":"+m+":ro")
	}
	for _, m := range mounts {
		if !bindOK(m) {
			return fmt.Errorf("cannot mount %q: the path has a ':' or ','", m)
		}
	}
	if _, err := os.Stat(dockerSock); err == nil {
		// As on GitHub's runners, so steps can use docker. It gives them the
		// host's docker daemon, which is root on this machine.
		r.initOut("Mounting %s into the job container", dockerSock)
		binds = append(binds, dockerSock+":"+dockerSock)
	}

	args, err := r.common(prefix, prefix+"-job", s, "--network", "--net", "--name", "--entrypoint")
	if err != nil {
		return err
	}
	for _, b := range binds {
		args = append(args, "-v", b)
	}
	// The container only has to stay alive: steps run in it with docker exec.
	args = append(args, "--entrypoint", "tail", s.image, "-f", "/dev/null")
	id, err := r.dkr.Create(ctx, s.env, args...)
	if err != nil {
		return err
	}
	r.track(id)
	if err := r.dkr.Start(ctx, id); err != nil {
		return err
	}

	base := mergeEnv(imageEnv, s.env)
	if envValue(base, "PATH") == "" {
		base = append(base, "PATH="+defaultPath)
	}
	r.mu.Lock()
	r.jobCID, r.cBase = id, base
	r.path = nil // toolchains the provisioner put on the host's PATH are no use in an image
	r.mu.Unlock()
	return nil
}

// reachServices serves the runtime services (artifacts, cache) on the host's
// address on the job's network, for steps in the job container. Failing is a
// warning, not an error: the job may never call them.
func (r *jobRun) reachServices(ctx context.Context, network string) {
	if r.cfg.ContainerServices == nil || len(r.cfg.ServiceEnv) == 0 {
		return
	}
	warn := func(err error) {
		r.emit(Event{Kind: Warning, Step: initStep, Text: fmt.Sprintf("artifacts and the cache will not work in the job container: %v", err)})
	}
	if r.rootless {
		warn(errors.New("rootless docker keeps its networks off this host's interfaces"))
		return
	}
	gw, subnet, err := r.dkr.NetworkGateway(ctx, network)
	if err != nil {
		warn(err)
		return
	}
	env, stop, err := r.cfg.ContainerServices(gw, subnet)
	if err != nil {
		warn(err)
		return
	}
	r.mu.Lock()
	r.cServiceEnv, r.cServiceStop = env, stop
	r.mu.Unlock()
	r.initOut("Serving artifacts and the cache to the job container at %s", gw)
}

// waitReady waits until a service container is healthy, or just running if its
// image has no health check.
func (r *jobRun) waitReady(ctx context.Context, id, name string) error {
	began := time.Now()
	deadline := began.Add(healthTimeout)
	for {
		in, err := r.dkr.Inspect(ctx, id)
		if err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
		fail := func(format string, a ...any) error {
			msg := fmt.Sprintf("service %s "+format, append([]any{name}, a...)...)
			if logs := strings.TrimSpace(r.dkr.Logs(context.WithoutCancel(ctx), id, 20)); logs != "" {
				msg += "; its last output:\n" + logs
			}
			return errors.New(msg)
		}
		switch {
		case !in.State.Running:
			return fail("exited with code %d", in.State.ExitCode)
		case in.State.Health == nil:
			if time.Since(began) >= settleTime {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(settleTime / 5):
			}
			continue
		case in.State.Health.Status == "healthy":
			r.initOut("Service %s is healthy", name)
			return nil
		case in.State.Health.Status == "unhealthy":
			return fail("is unhealthy")
		}
		if time.Now().After(deadline) {
			return fail("was not healthy after %s", healthTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(healthPoll):
		}
	}
}

// publishedPorts maps each published container port to the host port.
func (r *jobRun) publishedPorts(ctx context.Context, id string) (value.Object, error) {
	in, err := r.dkr.Inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(in.NetworkSettings.Ports))
	for k := range in.NetworkSettings.Ports {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 80/tcp before 80/udp
	out := value.Object{}
	for _, k := range keys {
		bindings := in.NetworkSettings.Ports[k]
		port, _, _ := strings.Cut(k, "/")
		if len(bindings) == 0 {
			continue
		}
		if _, dup := out.Lookup(port); !dup {
			out = out.Set(port, bindings[0].HostPort)
		}
	}
	return out, nil
}

// stopContainers removes the containers and the network. It is safe to call
// more than once.
func (r *jobRun) stopContainers() {
	r.mu.Lock()
	c, created, network, made, jobID := r.dkr, r.created, r.network, r.netMade, r.jobCID
	r.created, r.netMade, r.jobCID = nil, false, ""
	rootless := r.rootless
	serviceStop := r.cServiceStop
	r.cServiceEnv, r.cServiceStop = nil, nil
	r.mu.Unlock()
	if serviceStop != nil {
		defer serviceStop() // after the containers that used it are gone
	}
	if c == nil || (len(created) == 0 && !made) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if jobID != "" && !rootless && os.Getuid() != 0 {
		// Steps that ran as root left root-owned files in the bind mounts.
		cctx, ccancel := context.WithTimeout(ctx, 30*time.Second)
		dirs := []string{r.workDir}
		if !within(r.workDir, r.toolCache) {
			dirs = append(dirs, r.toolCache)
		}
		argv := append([]string{"chown", "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())}, dirs...)
		_, _ = c.Output(cctx, jobID, docker.ExecOpts{User: "0"}, argv...) // best effort: the image may have no chown
		ccancel()
	}
	if err := c.Remove(ctx, created...); err != nil {
		r.emit(Event{Kind: Warning, Text: fmt.Sprintf("removing containers: %v", err)})
	}
	if made {
		if err := c.NetworkRemove(ctx, network); err != nil {
			r.emit(Event{Kind: Warning, Text: fmt.Sprintf("removing network %s: %v", network, err)})
		}
	}
}

// hasShell says whether the job container has /bin/sh, which steps are
// started through (see exec).
func (r *jobRun) hasShell(cid string) bool {
	_, err := r.lookup("/bin/sh", defaultPath)
	return err == nil && cid != ""
}

// signalInContainer sends sig to the process group of a step running in the
// job container, whose pid is in pidFile. It reports whether it could.
func (r *jobRun) signalInContainer(cid, pidFile string, sig syscall.Signal) bool {
	b, err := os.ReadFile(pidFile)
	pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || perr != nil || pid <= 1 {
		return false // the step has not started yet
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// As root, whatever user the step runs as.
	_, err = r.dkr.Output(ctx, cid, docker.ExecOpts{User: "0"}, "/bin/sh", "-c",
		`kill "-$1" -- "-$2"`, "sh", strconv.Itoa(int(sig)), strconv.Itoa(pid))
	return err == nil
}

// containerIsMusl says whether the job container's image is built on musl
// (Alpine), whose dynamic loader is /lib/ld-musl-*. Host jobs and images
// without either loader (distroless, scratch) are not.
func (r *jobRun) containerIsMusl(ctx context.Context) bool {
	cid := r.containerID()
	if cid == "" {
		return false
	}
	r.mu.Lock()
	checked, musl := r.muslChecked, r.musl
	r.mu.Unlock()
	if !checked {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_, err := r.dkr.Output(cctx, cid, docker.ExecOpts{}, "/bin/sh", "-c",
			`for f in /lib/ld-musl-*.so.1 /usr/lib/ld-musl-*.so.1; do [ -e "$f" ] && exit 0; done; exit 1`)
		musl = err == nil
		r.mu.Lock()
		r.muslChecked, r.musl = true, musl
		r.mu.Unlock()
	}
	return musl
}

// checkNodeRuns makes sure the Node.js a JavaScript action needs can run in
// the job container. The runtimes are bind mounted from the host: the glibc
// builds, or for a musl image (Alpine) the musl builds. When even those cannot
// run (another architecture, a minimal image), the kernel's own "no such file
// or directory" says nothing about why, so this says it.
//
// The musl build links libstdc++ dynamically, which Alpine does not ship, so
// when it will not start in a musl container, libstdc++ is installed with apk
// (as root, in this job's container only) and the step log says so.
func (r *jobRun) checkNodeRuns(ctx context.Context, st *stepState, node string) error {
	cid := r.containerID()
	if cid == "" {
		return nil
	}
	r.mu.Lock()
	err, done := r.nodeChecks[node]
	r.mu.Unlock()
	if !done {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		_, xerr := r.dkr.Output(cctx, cid, docker.ExecOpts{}, node, "--version")
		var hint string
		if xerr != nil && r.containerIsMusl(ctx) {
			r.emitStep(st, Notice, "Node.js needs libstdc++, which this Alpine image lacks; installing it with apk", nil)
			if _, ierr := r.dkr.Output(cctx, cid, docker.ExecOpts{User: "0"}, "apk", "add", "--no-cache", "libstdc++"); ierr != nil {
				hint = fmt.Sprintf(" (installing libstdc++ with apk failed: %s; add it to the image)", firstLine(ierr.Error(), 300))
			} else {
				_, xerr = r.dkr.Output(cctx, cid, docker.ExecOpts{}, node, "--version")
			}
		}
		if xerr != nil {
			err = fmt.Errorf("the job container cannot run Node.js (%s), which JavaScript actions need: %s%s. "+
				"Raccoon CI mounts a glibc or musl build matching the image's libc, so the image is something else "+
				"(distroless, another architecture): use a Debian-, Ubuntu- or Alpine-based image", node, firstLine(xerr.Error(), 300), hint)
		}
		r.mu.Lock()
		if r.nodeChecks == nil {
			r.nodeChecks = map[string]error{}
		}
		r.nodeChecks[node] = err
		r.mu.Unlock()
	}
	return err
}

// firstLine is s up to its first line break, cut to max bytes.
func firstLine(s string, max int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// lookup finds an executable on pathEnv: on the host, or inside the job
// container for a container job.
func (r *jobRun) lookup(name, pathEnv string) (string, error) {
	cid := r.containerID()
	if cid == "" {
		return lookPath(name, pathEnv)
	}
	key := pathEnv + "\x00" + name
	r.mu.Lock()
	cached, ok := r.lookups[key]
	r.mu.Unlock()
	if !ok {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := r.dkr.Output(ctx, cid, docker.ExecOpts{Env: []string{"PATH=" + pathEnv}}, "/bin/sh", "-c",
			`case "$1" in */*) [ -x "$1" ] && printf '%s\n' "$1";; *) command -v "$1";; esac`, "sh", name)
		if err == nil {
			cached = strings.TrimSpace(out)
		}
		if !strings.HasPrefix(cached, "/") { // a builtin or nothing
			cached = ""
		}
		r.mu.Lock()
		if r.lookups == nil {
			r.lookups = map[string]string{}
		}
		r.lookups[key] = cached
		r.mu.Unlock()
	}
	if cached == "" {
		return "", fmt.Errorf("%s: command not found in the job container", name)
	}
	return cached, nil
}
