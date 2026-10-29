// Package docker drives the docker command line for container jobs and
// service containers. It shells out instead of using the engine API client, to
// stay a thin layer on what users already have installed and configured
// (contexts, credential helpers, rootless setups).
//
// Secrets never go on a command line, where `ps` shows them: values reach
// containers through the environment of the docker process (-e NAME), or for
// the few names the docker CLI itself reads, through a private env file.
package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
)

// Client runs docker commands.
type Client struct {
	// Bin is the docker executable.
	Bin string
}

// New finds docker on PATH.
func New() (*Client, error) {
	bin, err := exec.LookPath("docker")
	if err != nil {
		return nil, errors.New("docker is not installed or not on PATH")
	}
	return &Client{Bin: bin}, nil
}

// run executes `docker args...` and returns its stdout. env is added to the
// docker process's environment; stdin, if not empty, is piped in.
func (c *Client) run(ctx context.Context, env []string, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Env = append(os.Environ(), env...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return out.String(), nil
}

// Ping reports whether the docker daemon answers.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.run(ctx, nil, "", "version", "--format", "{{.Server.Version}}")
	return err
}

// Rootless reports whether container root is not host root (rootless docker or
// a remapped user namespace), in which case files need no ownership fix-up.
func (c *Client) Rootless(ctx context.Context) bool {
	out, err := c.run(ctx, nil, "", "info", "--format", "{{json .SecurityOptions}}")
	if err != nil {
		return false
	}
	return strings.Contains(out, "rootless") || strings.Contains(out, "userns")
}

// Login stores credentials for server in the docker config at configDir.
func (c *Client) Login(ctx context.Context, configDir, server, user, password string) error {
	args := []string{"login", "--username", user, "--password-stdin"}
	if server != "" {
		args = append(args, server)
	}
	_, err := c.run(ctx, []string{"DOCKER_CONFIG=" + configDir}, password, args...)
	return err
}

// Pull fetches image. configDir, if set, is the docker config to use.
func (c *Client) Pull(ctx context.Context, image, configDir string) error {
	var env []string
	if configDir != "" {
		env = []string{"DOCKER_CONFIG=" + configDir}
	}
	_, err := c.run(ctx, env, "", "pull", "--quiet", image)
	return err
}

// Build builds the Dockerfile at dockerfile (a path) with contextDir as the
// build context and tags the image.
func (c *Client) Build(ctx context.Context, tag, dockerfile, contextDir string) error {
	_, err := c.run(ctx, nil, "", "build", "--quiet", "-t", tag, "-f", dockerfile, contextDir)
	return err
}

// RunCmd builds `docker run args...` with env ("NAME=value" entries) passed
// the way Create passes it, off the command line. The returned func removes
// any temporary file and must be called once the command is done. The caller
// starts it and decides where its output goes; give the container a name with
// --name so Kill can stop it, since killing the docker client does not.
func (c *Client) RunCmd(ctx context.Context, env []string, args ...string) (*exec.Cmd, func(), error) {
	flags, extra, cleanup, err := envArgs(env)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, c.Bin, append(append([]string{"run"}, flags...), args...)...)
	cmd.Env = append(os.Environ(), extra...)
	return cmd, cleanup, nil
}

// Kill stops a running container immediately. Best effort.
func (c *Client) Kill(ctx context.Context, name string) {
	_, _ = c.run(ctx, nil, "", "kill", name)
}

// ImageEnv is the environment an image is configured with.
func (c *Client) ImageEnv(ctx context.Context, image string) ([]string, error) {
	out, err := c.run(ctx, nil, "", "image", "inspect", "--format", "{{json .Config.Env}}", image)
	if err != nil {
		return nil, err
	}
	var env []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); err != nil {
		return nil, fmt.Errorf("docker image inspect: %w", err)
	}
	return env, nil
}

// NetworkCreate makes a user-defined network, on which containers find each
// other by name.
func (c *Client) NetworkCreate(ctx context.Context, name string, labels ...string) error {
	args := []string{"network", "create"}
	for _, l := range labels {
		args = append(args, "--label", l)
	}
	_, err := c.run(ctx, nil, "", append(args, name)...)
	return err
}

// NetworkGateway returns a bridge network's IPv4 gateway, which is the host's
// own address on that network, and the network's subnet.
func (c *Client) NetworkGateway(ctx context.Context, name string) (netip.Addr, netip.Prefix, error) {
	out, err := c.run(ctx, nil, "", "network", "inspect", "--format", "{{json .IPAM.Config}}", name)
	if err != nil {
		return netip.Addr{}, netip.Prefix{}, err
	}
	return parseGateway(out)
}

func parseGateway(out string) (netip.Addr, netip.Prefix, error) {
	var cfgs []struct{ Gateway, Subnet string }
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &cfgs); err != nil {
		return netip.Addr{}, netip.Prefix{}, fmt.Errorf("docker network inspect: %w", err)
	}
	for _, cf := range cfgs {
		gw, gerr := netip.ParseAddr(cf.Gateway)
		subnet, serr := netip.ParsePrefix(cf.Subnet)
		if gerr == nil && serr == nil && gw.Is4() && subnet.Contains(gw) {
			return gw, subnet.Masked(), nil
		}
	}
	return netip.Addr{}, netip.Prefix{}, errors.New("the network has no IPv4 gateway")
}

func (c *Client) NetworkRemove(ctx context.Context, name string) error {
	_, err := c.run(ctx, nil, "", "network", "rm", name)
	return err
}

// Create makes a container from `docker create` arguments and returns its id.
// env ("NAME=value" entries) becomes the container's environment, passed the
// way ExecCmd passes it, so values stay off the command line.
func (c *Client) Create(ctx context.Context, env []string, args ...string) (string, error) {
	flags, extra, cleanup, err := envArgs(env)
	if err != nil {
		return "", err
	}
	defer cleanup()
	out, err := c.run(ctx, extra, "", append(append([]string{"create"}, flags...), args...)...)
	return strings.TrimSpace(out), err
}

// envArgs turns "NAME=value" entries into docker flags. Most values travel in
// the docker process's environment (extra), named by -e NAME: that handles
// multi-line values and keeps them out of `ps`. Names the docker CLI reads
// itself would change how it behaves, so those go through a private env file,
// which cleanup removes.
func envArgs(env []string) (flags, extra []string, cleanup func(), err error) {
	cleanup = func() {}
	var fileLines []string
	for _, kv := range env {
		name, val, ok := strings.Cut(kv, "=")
		if !ok || name == "" || strings.ContainsAny(name, "\x00\n") {
			continue
		}
		switch {
		case !cliVar(name):
			flags = append(flags, "-e", name)
			extra = append(extra, kv)
		case strings.Contains(val, "\n"):
			return nil, nil, cleanup, fmt.Errorf("environment variable %s has a multi-line value, which docker cannot pass for this name", name)
		default:
			fileLines = append(fileLines, kv)
		}
	}
	if len(fileLines) == 0 {
		return flags, extra, cleanup, nil
	}
	f, err := os.CreateTemp("", "raccoonci-env-*")
	if err != nil {
		return nil, nil, cleanup, err
	}
	name := f.Name()
	cleanup = func() { os.Remove(name) }
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return nil, nil, func() {}, err
	}
	_, werr := f.WriteString(strings.Join(fileLines, "\n") + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		cleanup()
		return nil, nil, func() {}, werr
	}
	return append(flags, "--env-file", name), extra, cleanup, nil
}

func (c *Client) Start(ctx context.Context, id string) error {
	_, err := c.run(ctx, nil, "", "start", id)
	return err
}

// Remove force-removes containers and their anonymous volumes. Missing
// containers are not an error.
func (c *Client) Remove(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := c.run(ctx, nil, "", append([]string{"rm", "--force", "--volumes"}, ids...)...)
	if err != nil && strings.Contains(err.Error(), "No such container") {
		return nil
	}
	return err
}

// Logs returns the last lines a container wrote.
func (c *Client) Logs(ctx context.Context, id string, tail int) string {
	out, err := c.run(ctx, nil, "", "logs", "--tail", fmt.Sprint(tail), id)
	if err != nil {
		return ""
	}
	return out
}

// Inspect is the part of `docker inspect` we read.
type Inspect struct {
	State struct {
		Status   string
		Running  bool
		ExitCode int
		Health   *struct{ Status string }
	}
	NetworkSettings struct {
		Ports map[string][]struct{ HostIp, HostPort string }
	}
}

func (c *Client) Inspect(ctx context.Context, id string) (*Inspect, error) {
	out, err := c.run(ctx, nil, "", "inspect", "--format", "{{json .}}", id)
	if err != nil {
		return nil, err
	}
	var in Inspect
	if err := json.Unmarshal([]byte(out), &in); err != nil {
		return nil, fmt.Errorf("docker inspect: %w", err)
	}
	return &in, nil
}

// cliVar says whether the docker CLI reads a variable itself (to find its
// config, contexts and daemon), so a step's value must not replace the host's.
func cliVar(name string) bool {
	switch name {
	case "PATH", "HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR":
		return true
	}
	return strings.HasPrefix(name, "DOCKER_")
}

// ExecOpts says how to run a command in a container.
type ExecOpts struct {
	Dir  string   // working directory
	User string   // run as this user; the container's own if empty
	Env  []string // "NAME=value" entries
}

// ExecCmd builds the command that runs argv in a running container. The
// returned func removes any temporary file and must be called once the
// command is done. ctx kills the docker client, not the process inside the
// container: docker exec does not forward signals, so that one lives until
// the container goes.
func (c *Client) ExecCmd(ctx context.Context, container string, o ExecOpts, argv []string) (*exec.Cmd, func(), error) {
	args := []string{"exec"}
	if o.Dir != "" {
		args = append(args, "-w", o.Dir)
	}
	if o.User != "" {
		args = append(args, "-u", o.User)
	}
	flags, extra, cleanup, err := envArgs(o.Env)
	if err != nil {
		return nil, nil, err
	}
	args = append(args, flags...)
	args = append(args, container)
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Env = append(os.Environ(), extra...)
	return cmd, cleanup, nil
}

// Output runs argv in a running container and returns its stdout.
func (c *Client) Output(ctx context.Context, container string, o ExecOpts, argv ...string) (string, error) {
	cmd, cleanup, err := c.ExecCmd(ctx, container, o, argv)
	if err != nil {
		return "", err
	}
	defer cleanup()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// RandomName makes a container or network name that will not collide.
func RandomName(prefix string) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "-" + hex.EncodeToString(b)
}

// SplitOptions splits the options: string of a container the way a shell
// would split words: whitespace separates, single and double quotes group,
// and a backslash escapes the next character outside single quotes. Nothing is
// expanded.
func SplitOptions(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped, inWord = true, true
		case quote == '"':
			if r == '"' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("options: unterminated %c quote", quote)
	}
	if escaped {
		return nil, errors.New("options: trailing backslash")
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}

// CheckOptions rejects options that would break how containers are wired
// together (the network, the name, the entrypoint).
func CheckOptions(opts []string, forbidden ...string) error {
	for _, o := range opts {
		name, _, _ := strings.Cut(o, "=")
		for _, f := range forbidden {
			if name == f {
				return fmt.Errorf("the option %s is not supported: Raccoon CI sets it itself", name)
			}
		}
	}
	return nil
}
