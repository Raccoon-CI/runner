package runner

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRealDockerActions(t *testing.T) {
	dockerTest(t)
	before := leftovers(t)
	res, rec := run(t, testJob{
		files: map[string]string{
			"dk/action.yml": `
name: dk
inputs:
  who:
    default: world
runs:
  using: docker
  image: Dockerfile
  args:
    - ${{ inputs.who }}
    - second
  env:
    FROM_RUNS_ENV: ${{ inputs.who }}-env
  post-entrypoint: /post.sh
`,
			"dk/Dockerfile": "FROM busybox:1.36\nCOPY entry.sh post.sh /\nRUN chmod +x /entry.sh /post.sh\nENTRYPOINT [\"/entry.sh\"]\n",
			"dk/entry.sh": `#!/bin/sh
echo "args: $* input=$INPUT_WHO runs_env=$FROM_RUNS_ENV"
echo "ws=$GITHUB_WORKSPACE pwd=$(pwd) home=$HOME"
echo "host-var=[$SOME_HOST_VAR] path-set=$([ -n "$PATH" ] && echo yes)"
echo "result=docker-$INPUT_WHO" >> "$GITHUB_OUTPUT"
echo "FROM_DOCKER=yes" >> "$GITHUB_ENV"
echo "answer=$(cat /github/workspace/in.txt)"
echo made-in-container > out.txt
`,
			"dk/post.sh": "#!/bin/sh\necho post-ran\n",
			"in.txt":     "from-the-host",
		},
		yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    env:
      JOB_ENV: job-value
    steps:
      - uses: actions/checkout@v4
      - id: dk
        uses: ./dk
        with:
          who: raccoon
      - run: echo "out=${{ steps.dk.outputs.result }} env=$FROM_DOCKER file=$(cat out.txt)"
      - uses: docker://busybox:1.36
        with:
          entrypoint: /bin/sh
          args: -c "echo image-step $GITHUB_WORKSPACE $JOB_ENV"
      - uses: docker://busybox:1.36
        with:
          args: sh -c "exit 4"
        continue-on-error: true
`,
		configFn: func(c *Config) { c.BaseEnv = append(os.Environ(), "SOME_HOST_VAR=leaked") },
	})
	out := rec.output()
	if res.Conclusion != Success {
		t.Fatalf("conclusion %s\n%s", res.Conclusion, out)
	}
	for _, want := range []string{
		"args: raccoon second input=raccoon runs_env=raccoon-env",
		"ws=/github/workspace pwd=/github/workspace home=/github/home",
		"host-var=[] ", // this machine's environment is not passed
		"answer=from-the-host",
		"out=docker-raccoon env=yes file=made-in-container",
		"image-step /github/workspace job-value",
		"post-ran",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !rec.has(Error, "Process completed with exit code 4.") {
		t.Errorf("the failing image step did not report its exit code:\n%s", rec.dump())
	}
	if left := leftovers(t); left != before {
		t.Errorf("containers left behind: %q (before: %q)", left, before)
	}
}

// Cancelling a job stops the container too, not just the docker client.
func TestRealDockerActionIsStoppedOnCancel(t *testing.T) {
	dockerTest(t)
	before := leftovers(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(4*time.Second, cancel) // a deadline would read as a job timeout
	res, rec := run(t, testJob{
		ctx: ctx,
		yaml: `
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - uses: docker://busybox:1.36
        with:
          entrypoint: sleep
          args: "120"
`,
	})
	if res.Conclusion != Cancelled {
		t.Fatalf("conclusion %s\n%s", res.Conclusion, rec.dump())
	}
	if left := leftovers(t); left != before {
		t.Errorf("a cancelled container is still there: %q", left)
	}
}
