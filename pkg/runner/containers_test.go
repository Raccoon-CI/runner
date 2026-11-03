package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Raccoon-CI/runner/internal/docker"
	"github.com/Raccoon-CI/runner/internal/nodejs"
	"github.com/Raccoon-CI/lib/pkg/value"
)

func TestParseSpec(t *testing.T) {
	s, err := parseSpec("node:22")
	if err != nil || s.image != "node:22" {
		t.Fatalf("string form: %+v %v", s, err)
	}
	if s, err := parseSpec(""); s != nil || err != nil {
		t.Errorf("empty string: %+v %v", s, err)
	}
	if s, err := parseSpec(nil); s != nil || err != nil {
		t.Errorf("nil: %+v %v", s, err)
	}

	full := value.Object{
		{Key: "image", Value: "ghcr.io/me/img:1"},
		{Key: "credentials", Value: value.Object{{Key: "username", Value: "u"}, {Key: "password", Value: "p"}}},
		{Key: "env", Value: value.Object{{Key: "A", Value: "1"}, {Key: "B", Value: true}, {Key: "C", Value: float64(3)}}},
		{Key: "ports", Value: []any{float64(8080), "80:81"}},
		{Key: "volumes", Value: []any{"/a:/b"}},
		{Key: "options", Value: `--health-cmd "pg_isready -U x" --cpus 1`},
	}
	s, err = parseSpec(full)
	if err != nil {
		t.Fatal(err)
	}
	want := &containerSpec{
		image: "ghcr.io/me/img:1", username: "u", password: "p",
		env:     []string{"A=1", "B=true", "C=3"},
		ports:   []string{"8080", "80:81"},
		volumes: []string{"/a:/b"},
		options: []string{"--health-cmd", "pg_isready -U x", "--cpus", "1"},
	}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("got  %+v\nwant %+v", s, want)
	}

	for name, bad := range map[string]any{
		"no image":        value.Object{{Key: "env", Value: value.Object{}}},
		"unknown key":     value.Object{{Key: "image", Value: "x"}, {Key: "command", Value: "y"}},
		"option image":    "--privileged",
		"space in image":  "a b",
		"newline":         "a\nb",
		"bad credentials": value.Object{{Key: "image", Value: "x"}, {Key: "credentials", Value: "s"}},
		"credential key":  value.Object{{Key: "image", Value: "x"}, {Key: "credentials", Value: value.Object{{Key: "token", Value: "t"}}}},
		"ports not list":  value.Object{{Key: "image", Value: "x"}, {Key: "ports", Value: "80"}},
		"env not map":     value.Object{{Key: "image", Value: "x"}, {Key: "env", Value: "A=1"}},
		"env bad name":    value.Object{{Key: "image", Value: "x"}, {Key: "env", Value: value.Object{{Key: "A=B", Value: "1"}}}},
		"bad options":     value.Object{{Key: "image", Value: "x"}, {Key: "options", Value: `--a "b`}},
		"a number":        float64(3),
		"a list":          []any{"x"},
	} {
		if s, err := parseSpec(bad); err == nil {
			t.Errorf("%s accepted: %+v", name, s)
		}
	}
}

func TestNormalizePort(t *testing.T) {
	for in, want := range map[string]string{
		"5432":                "127.0.0.1::5432",
		"5432:5432":           "127.0.0.1:5432:5432",
		"8080:80/udp":         "127.0.0.1:8080:80/udp",
		"0.0.0.0:80:80":       "0.0.0.0:80:80",
		"127.0.0.1::80":       "127.0.0.1::80",
		"[::1]:8080:80":       "[::1]:8080:80",
		"8000-8010:8000-8010": "127.0.0.1:8000-8010:8000-8010",
	} {
		if got := normalizePort(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

func TestRegistryOf(t *testing.T) {
	for in, want := range map[string]string{
		"node:22":                      "",
		"library/node":                 "",
		"me/img:1":                     "",
		"ghcr.io/me/img:1":             "ghcr.io",
		"localhost/img":                "localhost",
		"localhost:5000/img":           "localhost:5000",
		"registry.example.com:443/a/b": "registry.example.com:443",
	} {
		if got := registryOf(in); got != want {
			t.Errorf("%s -> %q, want %q", in, got, want)
		}
	}
}

func TestMergeEnv(t *testing.T) {
	base := []string{"PATH=/bin", "A=1"}
	got := mergeEnv(base, []string{"A=2", "B=3"})
	if want := []string{"PATH=/bin", "A=2", "B=3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if base[1] != "A=1" {
		t.Error("base was modified")
	}
}

func TestBindOKAndWithin(t *testing.T) {
	for p, want := range map[string]bool{"/a/b": true, "/a b/c": true, "rel/path": false, "/a:b": false, "/a,b": false, "/a\nb": false} {
		if bindOK(p) != want {
			t.Errorf("bindOK(%q) = %v", p, !want)
		}
	}
	for _, c := range []struct {
		parent, child string
		want          bool
	}{
		{"/w", "/w/tool", true}, {"/w", "/w", true}, {"/w", "/wx", false}, {"/w", "/other", false}, {"/w/a", "/w", false},
	} {
		if got := within(c.parent, c.child); got != c.want {
			t.Errorf("within(%s, %s) = %v", c.parent, c.child, got)
		}
	}
}

func TestBadContainerConfigFailsBeforeDocker(t *testing.T) {
	bogus := func(c *Config) { c.Docker = &docker.Client{Bin: "/nonexistent/docker"} }
	for name, tc := range map[string]struct{ yaml, want string }{
		"service id":    {"services:\n      'bad id':\n        image: x", "bad service id"},
		"no image":      {"services:\n      db:\n        env: {A: b}", "image is required"},
		"unknown key":   {"container:\n      image: x\n      command: y", "unknown key"},
		"service entry": {"services:\n      db: [x]", "services.db"},
		"expression":    {"container: ${{ fromJSON('[1]') }}", "container"},
	} {
		res, rec := run(t, testJob{
			yaml:     "\non: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    " + tc.yaml + "\n    steps:\n      - run: echo ran\n",
			configFn: bogus,
		})
		if res.Conclusion != Failure || !rec.has(Error, tc.want) {
			t.Errorf("%s: %s, output %s", name, res.Conclusion, rec.output())
		}
	}
}

func TestContainerWithExpression(t *testing.T) {
	// The image comes from the matrix; with no docker it still fails, but
	// only after the expression was evaluated into a valid spec.
	res, rec := run(t, testJob{
		yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        image: [node:22]
    container: ${{ matrix.image }}
    steps:
      - run: echo ran
`,
		configFn: func(c *Config) { c.Docker = &docker.Client{Bin: "/nonexistent/docker"} },
	})
	_ = res
	if rec.has(Error, "bad image") || rec.has(Error, "container:") {
		t.Errorf("the matrix expression was not evaluated: %s", rec.output())
	}
}

// ---- real docker ----

// dockerTest skips unless RACCOONCI_TEST_DOCKER is set. It needs a docker
// daemon and network access to Docker Hub.
func dockerTest(t *testing.T) {
	t.Helper()
	if os.Getenv("RACCOONCI_TEST_DOCKER") == "" {
		t.Skip("RACCOONCI_TEST_DOCKER not set")
	}
	old := healthPoll
	healthPoll = 200 * time.Millisecond
	t.Cleanup(func() { healthPoll = old })
}

// leftovers lists the containers and networks Raccoon CI made and did not remove.
func leftovers(t *testing.T) string {
	t.Helper()
	var out strings.Builder
	for _, args := range [][]string{
		{"ps", "-a", "--filter", "label=raccoonci=1", "--format", "{{.Names}}"},
		{"network", "ls", "--filter", "label=raccoonci=1", "--format", "{{.Name}}"},
	} {
		b, err := exec.Command("docker", args...).Output()
		if err != nil {
			t.Fatal(err)
		}
		out.Write(b)
	}
	return strings.TrimSpace(out.String())
}

func TestRealContainerJob(t *testing.T) {
	dockerTest(t)
	before := leftovers(t)
	var workDir string
	res, rec := run(t, testJob{
		yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    container:
      image: busybox:1.36
      env:
        FROM_SPEC: spec-value
    env:
      FROM_JOB: job-value
    steps:
      - run: |
          echo "in-container: $(cat /etc/hostname | head -c 6)"
          echo "spec=$FROM_SPEC job=$FROM_JOB"
          echo "cid=${{ job.container.id != '' }}"
          echo "file from the container" > "$GITHUB_WORKSPACE/out.txt"
          echo "SET_BY_STEP=carried" >> "$GITHUB_ENV"
          echo "answer=42" >> "$GITHUB_OUTPUT"
        id: one
      - run: echo "env=$SET_BY_STEP output=${{ steps.one.outputs.answer }}"
      - run: |
          echo "multi" 
          printf 'secret-ish: %s\n' "$SECRET_VALUE"
        env:
          SECRET_VALUE: |
            line1
            line2
      - run: exit 3
        continue-on-error: true
        id: bad
      - run: echo "bad outcome ${{ steps.bad.outcome }}"
`,
		configFn: func(c *Config) { workDir = c.WorkDir },
	})
	out := rec.output()
	if res.Conclusion != Success {
		t.Fatalf("conclusion %s\n%s", res.Conclusion, out)
	}
	for _, want := range []string{
		"spec=spec-value job=job-value",
		"cid=true",
		"env=carried output=42",
		"line1",
		"line2",
		"Process completed with exit code 3.",
		"bad outcome failure",
		"Pulling image busybox:1.36",
	} {
		if !strings.Contains(rec.dump(), want) {
			t.Errorf("output lacks %q\n%s", want, rec.dump())
		}
	}
	// The container's file is on the host, in the workspace.
	files, _ := filepath.Glob(filepath.Join(workDir, "*", "*", "out.txt"))
	if len(files) != 1 {
		t.Fatalf("workspace file not found under %s: %v", workDir, files)
	}
	if b, _ := os.ReadFile(files[0]); string(b) != "file from the container\n" {
		t.Errorf("file = %q", b)
	}
	// The step ran as root in the container; the job's end gave the file back.
	if fi, err := os.Stat(files[0]); err != nil {
		t.Error(err)
	} else if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() && os.Getuid() != 0 {
		t.Errorf("out.txt is owned by uid %d, not %d", st.Uid, os.Getuid())
	}
	// The host's HOME did not leak into the container.
	if strings.Contains(out, "HOME="+filepath.Dir(workDir)) {
		t.Errorf("host environment leaked:\n%s", out)
	}
	if after := leftovers(t); after != before {
		t.Errorf("containers or networks left behind:\n%s", after)
	}
}

// Cancelling a step interrupts the process in the container (docker exec
// forwards no signals itself): the step's trap runs, its background child dies
// with the group, and the always() step after it finds nothing left over.
func TestRealContainerCancelSignalsTheStep(t *testing.T) {
	dockerTest(t)
	before := leftovers(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	res, rec := run(t, testJob{ctx: ctx, yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    container: busybox:1.36
    steps:
      - run: |
          trap 'echo got-interrupt; exit 0' INT
          sleep 300 &
          echo step-started
          wait
      - if: always()
        run: |
          echo "left: $(ps | grep -c '[s]leep 300')"
`, configFn: func(c *Config) {
		// Cancel once the step is running, whatever the image pull took.
		orig := c.OnEvent
		c.OnEvent = func(e Event) {
			orig(e)
			if e.Kind == Output && e.Text == "step-started" {
				time.AfterFunc(500*time.Millisecond, cancel)
			}
		}
	}})
	out := rec.output()
	if res.Conclusion != Cancelled {
		t.Fatalf("conclusion %s\n%s", res.Conclusion, rec.dump())
	}
	for _, want := range []string{"got-interrupt", "left: 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n%s", want, rec.dump())
		}
	}
	if d := time.Since(start); d > 90*time.Second {
		t.Errorf("took %s", d)
	}
	if after := leftovers(t); after != before {
		t.Errorf("left behind:\n%s", after)
	}
}

func TestRealContainerJobFailsAndCleansUp(t *testing.T) {
	dockerTest(t)
	before := leftovers(t)
	res, rec := run(t, testJob{yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    container: busybox:1.36
    steps:
      - run: echo before
      - run: exit 1
      - run: echo skipped
      - if: always()
        run: echo cleanup-ran
`})
	out := rec.output()
	if res.Conclusion != Failure || !strings.Contains(out, "cleanup-ran") || strings.Contains(out, "skipped\n") && !strings.Contains(out, "cleanup-ran") {
		t.Errorf("conclusion %s\n%s", res.Conclusion, out)
	}
	if after := leftovers(t); after != before {
		t.Errorf("left behind:\n%s", after)
	}

	// An image that does not exist fails the job, with docker's reason.
	res, rec = run(t, testJob{yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    container: raccoonci-test.invalid/nothing:1
    steps:
      - run: echo ran
`})
	if res.Conclusion != Failure || rec.has(Output, "ran") || !rec.has(Error, "pull") {
		t.Errorf("missing image: %s\n%s", res.Conclusion, rec.output())
	}
	if after := leftovers(t); after != before {
		t.Errorf("left behind after a failed start:\n%s", after)
	}
}

const redisSpec = `
        image: redis:7-alpine
        ports: ['6379']
        options: --health-cmd "redis-cli ping" --health-interval 1s --health-retries 30`

func TestRealServiceFromContainerJob(t *testing.T) {
	dockerTest(t)
	before := leftovers(t)
	res, rec := run(t, testJob{yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    container: busybox:1.36
    services:
      redis:` + redisSpec + `
    steps:
      - run: |
          printf 'PING\r\n' | nc -w 3 redis 6379
`})
	out := rec.output()
	if res.Conclusion != Success || !strings.Contains(out, "+PONG") {
		t.Fatalf("conclusion %s\n%s", res.Conclusion, rec.dump())
	}
	if !strings.Contains(out, "Service redis is healthy") {
		t.Errorf("no health message:\n%s", out)
	}
	if after := leftovers(t); after != before {
		t.Errorf("left behind:\n%s", after)
	}
}

func TestRealServiceFromHostJob(t *testing.T) {
	dockerTest(t)
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("needs bash for /dev/tcp")
	}
	before := leftovers(t)
	res, rec := run(t, testJob{yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    services:
      redis:` + redisSpec + `
    steps:
      - shell: bash
        run: |
          port=${{ job.services.redis.ports['6379'] }}
          echo "port=$port"
          exec 3<>/dev/tcp/127.0.0.1/$port
          printf 'PING\r\n' >&3
          head -c 7 <&3
`})
	out := rec.output()
	if res.Conclusion != Success || !strings.Contains(out, "+PONG") {
		t.Fatalf("conclusion %s\n%s", res.Conclusion, out)
	}
	if after := leftovers(t); after != before {
		t.Errorf("left behind:\n%s", after)
	}
}

// A JavaScript action in an Alpine job container runs on the musl Node.js, with
// libstdc++ added to the image by the runner. It needs the network for the
// Node.js download and for apk.
func TestRealAlpineNodeAction(t *testing.T) {
	dockerTest(t)
	nodeDir := t.TempDir()
	image := "alpine:3.20"
	if v := os.Getenv("RACCOONCI_TEST_ALPINE_IMAGE"); v != "" { // e.g. an image whose apk mirrors work behind a proxy
		image = v
	}
	res, rec := run(t, testJob{
		files: map[string]string{
			"js/action.yml": "name: js\nruns:\n  using: node20\n  main: main.js\n",
			"js/main.js":    "console.log('libc=' + (process.report.getReport().header.glibcVersionRuntime || 'musl'));\n",
		},
		yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    container: ` + image + `
    steps:
      - uses: actions/checkout@v4
      - uses: ./js
`,
		configFn: func(c *Config) {
			musl := nodejs.NewMusl(filepath.Join(nodeDir, "musl"))
			c.NodeMusl = musl.Binary
			c.Mounts = []string{nodeDir}
		},
	})
	if rec.has(Error, "installing libstdc++ with apk failed") {
		t.Skip("apk cannot reach the Alpine mirrors from here")
	}
	if res.Conclusion != Success || !rec.has(Output, "libc=musl") || !rec.has(Notice, "libstdc++") {
		t.Fatalf("%s\n%s", res.Conclusion, rec.dump())
	}
}

// TestRealPrivateRegistry pulls the job's image from a registry that wants a
// login, with the workflow's credentials: a local registry:2 with basic auth.
func TestRealPrivateRegistry(t *testing.T) {
	dockerTest(t)
	before := leftovers(t)
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	const user, pass = "raccoon", "t3st-pa55-w0rd"

	auth := t.TempDir()
	docker("pull", "--quiet", "httpd:2.4")
	docker("pull", "--quiet", "registry:2")
	docker("pull", "--quiet", "busybox:1.36")
	htpasswd := docker("run", "--rm", "httpd:2.4", "htpasswd", "-Bbn", user, pass)
	if err := os.WriteFile(filepath.Join(auth, "htpasswd"), []byte(htpasswd+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := docker("run", "-d", "--rm", "-p", "127.0.0.1::5000", "-v", auth+":/auth:ro",
		"-e", "REGISTRY_AUTH=htpasswd", "-e", "REGISTRY_AUTH_HTPASSWD_REALM=test",
		"-e", "REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd", "registry:2")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", reg).Run() })
	hostPort := docker("port", reg, "5000/tcp") // 127.0.0.1:49153
	addr := "localhost:" + hostPort[strings.LastIndex(hostPort, ":")+1:]
	image := addr + "/private/busybox:1.36"

	// Put an image there, then forget it locally so the job has to pull it.
	cfg := t.TempDir()
	login := exec.Command("docker", "--config", cfg, "login", "--username", user, "--password-stdin", addr)
	login.Stdin = strings.NewReader(pass)
	for i := 0; ; i++ { // the registry needs a moment to listen
		if out, err := login.CombinedOutput(); err == nil {
			break
		} else if i == 20 {
			t.Fatalf("login: %v\n%s", err, out)
		}
		time.Sleep(500 * time.Millisecond)
		login = exec.Command("docker", "--config", cfg, "login", "--username", user, "--password-stdin", addr)
		login.Stdin = strings.NewReader(pass)
	}
	docker("tag", "busybox:1.36", image)
	docker("--config", cfg, "push", "--quiet", image)
	docker("rmi", image)

	job := func(password string) string {
		return `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    container:
      image: ` + image + `
      credentials:
        username: ` + user + `
        password: ` + password + `
    steps:
      - run: echo from-private-image
`
	}
	res, rec := run(t, testJob{yaml: job(pass)})
	if res.Conclusion != Success || !rec.has(Output, "from-private-image") {
		t.Fatalf("with the right password: %s\n%s", res.Conclusion, rec.dump())
	}
	if strings.Contains(rec.dump(), pass) {
		t.Errorf("the password reached the log:\n%s", rec.dump())
	}

	docker("rmi", image)
	res, rec = run(t, testJob{yaml: job("wrong-password")})
	if res.Conclusion != Failure || rec.has(Output, "from-private-image") {
		t.Errorf("with a wrong password: %s\n%s", res.Conclusion, rec.dump())
	}
	if !rec.has(Error, "unauthorized") && !rec.has(Error, "denied") && !rec.has(Error, "login") {
		t.Errorf("the failure does not say why:\n%s", rec.dump())
	}
	if strings.Contains(rec.dump(), "wrong-password") {
		t.Errorf("the password reached the log:\n%s", rec.dump())
	}

	// Without credentials the pull is refused too, and no login state is left
	// in the user's own docker config.
	docker("rmi", "-f", image)
	if out, err := exec.Command("docker", "pull", image).CombinedOutput(); err == nil {
		t.Errorf("an anonymous pull worked; the job's login leaked into the host's config:\n%s", out)
	}
	if after := leftovers(t); after != before {
		t.Errorf("left behind:\n%s", after)
	}
}

func TestRealBadServices(t *testing.T) {
	dockerTest(t)
	before := leftovers(t)
	for name, tc := range map[string]struct{ spec, want string }{
		"exits at once":    {"image: busybox:1.36", "exited with code 0"},
		"unhealthy":        {"image: redis:7-alpine\n        options: --health-cmd false --health-interval 1s --health-retries 2", "is unhealthy"},
		"forbidden option": {"image: busybox:1.36\n        options: --network host", "--network"},
	} {
		res, rec := run(t, testJob{yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    services:
      svc:
        ` + tc.spec + `
    steps:
      - run: echo ran
`})
		if res.Conclusion != Failure || !rec.has(Error, tc.want) || rec.has(Output, "ran") {
			t.Errorf("%s: %s\n%s", name, res.Conclusion, rec.dump())
		}
	}
	if after := leftovers(t); after != before {
		t.Errorf("left behind:\n%s", after)
	}
}
