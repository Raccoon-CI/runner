package docker

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSplitOptions(t *testing.T) {
	for in, want := range map[string][]string{
		"":                                    nil,
		"   ":                                 nil,
		"--cpus 1":                            {"--cpus", "1"},
		"--health-cmd pg_isready --retries 5": {"--health-cmd", "pg_isready", "--retries", "5"},
		`--health-cmd "redis-cli ping" --x`:   {"--health-cmd", "redis-cli ping", "--x"},
		`--health-cmd 'a "b" c'`:              {"--health-cmd", `a "b" c`},
		`--label a\ b`:                        {"--label", "a b"},
		`--e="x y"`:                           {"--e=x y"},
		"--a\n--b\t--c":                       {"--a", "--b", "--c"},
		`--empty ""`:                          {"--empty", ""},
		`--no-expand $HOME`:                   {"--no-expand", "$HOME"},
	} {
		got, err := SplitOptions(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{`--a "b`, `--a 'b`, `--a b\`} {
		if _, err := SplitOptions(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCheckOptions(t *testing.T) {
	forbidden := []string{"--network", "--net", "--name", "--entrypoint"}
	if err := CheckOptions([]string{"--cpus", "1", "--health-cmd", "x"}, forbidden...); err != nil {
		t.Errorf("harmless options refused: %v", err)
	}
	for _, o := range [][]string{{"--network", "host"}, {"--network=host"}, {"--name=x"}, {"--entrypoint", "sh"}, {"--net=none"}} {
		if err := CheckOptions(o, forbidden...); err == nil {
			t.Errorf("%q accepted", o)
		}
	}
}

func TestExecCmdKeepsValuesOffTheCommandLine(t *testing.T) {
	c := &Client{Bin: "/usr/bin/docker"}
	cmd, cleanup, err := c.ExecCmd(context.Background(), "cid", ExecOpts{Dir: "/work", User: "0", Env: []string{
		"SECRET=hunter2",
		"MULTI=a\nb",
		"HOME=/root",
		"DOCKER_HOST=tcp://x:1",
		"=nameless",
		"noequals",
	}}, []string{"sh", "-c", "true"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	line := strings.Join(cmd.Args, " ")
	for _, leak := range []string{"hunter2", "a\nb", "/root", "tcp://x:1"} {
		if strings.Contains(line, leak) {
			t.Errorf("value %q is on the command line: %s", leak, line)
		}
	}
	if !strings.Contains(line, "-e SECRET") || !strings.Contains(line, "-e MULTI") || !strings.Contains(line, "-w /work") || !strings.Contains(line, "-u 0") {
		t.Errorf("command line = %s", line)
	}
	if !strings.HasSuffix(line, "cid sh -c true") {
		t.Errorf("container and command not last: %s", line)
	}
	env := strings.Join(cmd.Env, "\x00")
	if !strings.Contains(env, "SECRET=hunter2") || !strings.Contains(env, "MULTI=a\nb") {
		t.Error("values missing from the docker process's environment")
	}
	// Names the CLI reads itself go in a private file, not into its own environment.
	var file string
	for i, a := range cmd.Args {
		if a == "--env-file" {
			file = cmd.Args[i+1]
		}
	}
	if file == "" {
		t.Fatal("no env file for HOME and DOCKER_HOST")
	}
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("env file: %v %v", fi, err)
	}
	b, _ := os.ReadFile(file)
	if string(b) != "HOME=/root\nDOCKER_HOST=tcp://x:1\n" {
		t.Errorf("env file = %q", b)
	}
	cleanup()
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("env file not removed")
	}

	if _, _, err := c.ExecCmd(context.Background(), "cid", ExecOpts{Env: []string{"HOME=a\nb"}}, []string{"true"}); err == nil {
		t.Error("a multi-line HOME was accepted")
	}
}

func TestRandomNameIsUnique(t *testing.T) {
	a, b := RandomName("rc"), RandomName("rc")
	if a == b || !strings.HasPrefix(a, "rc-") {
		t.Errorf("%q %q", a, b)
	}
}

// TestRealDocker needs a docker daemon and network access to Docker Hub.
// Set RACCOONCI_TEST_DOCKER=1 to run it.
func TestRealDocker(t *testing.T) {
	if os.Getenv("RACCOONCI_TEST_DOCKER") == "" {
		t.Skip("RACCOONCI_TEST_DOCKER not set")
	}
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	const image = "busybox:1.36"
	if err := c.Pull(ctx, image, ""); err != nil {
		t.Fatal(err)
	}
	env, err := c.ImageEnv(ctx, image)
	if err != nil || len(env) == 0 || !strings.HasPrefix(env[0], "PATH=") {
		t.Fatalf("ImageEnv = %v, %v", env, err)
	}

	net := RandomName("rc-test")
	if err := c.NetworkCreate(ctx, net, "raccoonci.test=1"); err != nil {
		t.Fatal(err)
	}
	defer c.NetworkRemove(context.Background(), net)

	name := RandomName("rc-test")
	id, err := c.Create(ctx, nil, "--name", name, "--network", net, "--network-alias", "svc", "--entrypoint", "tail", image, "-f", "/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Remove(context.Background(), id)
	if err := c.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	in, err := c.Inspect(ctx, id)
	if err != nil || !in.State.Running {
		t.Fatalf("Inspect = %+v, %v", in, err)
	}

	// A value reaches the process, multi-line and unexpanded, and a name the
	// CLI reads itself reaches it too.
	out, err := c.Output(ctx, id, ExecOpts{Dir: "/tmp", Env: []string{"SECRET=line1\nline2 $HOME \"q\"", "HOME=/tmp"}}, "sh", "-c", `printf '[%s][%s]' "$SECRET" "$HOME"`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "[line1\nline2 $HOME \"q\"][/tmp]" {
		t.Errorf("output = %q", out)
	}
	if _, err := c.Output(ctx, id, ExecOpts{}, "sh", "-c", "exit 3"); err == nil {
		t.Error("exit status 3 not reported")
	}

	if err := c.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, id); err != nil {
		t.Errorf("removing twice: %v", err)
	}
}
