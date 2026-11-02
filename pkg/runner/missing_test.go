package runner

import (
	"slices"
	"sync"
	"testing"
)

func TestCommandNotFoundIsReported(t *testing.T) {
	var mu sync.Mutex
	var got []string
	res, rec := run(t, testJob{
		yaml: `
on: push
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: raccoon-no-such-tool --version
`,
		configFn: func(c *Config) {
			c.OnMissing = func(cmd string) { mu.Lock(); got = append(got, cmd); mu.Unlock() }
		},
	})
	if res.Conclusion != Failure {
		t.Fatalf("conclusion %v", res.Conclusion)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(got, []string{"raccoon-no-such-tool"}) {
		t.Fatalf("OnMissing got %v\n%s", got, rec.dump())
	}
	if !rec.has(Warning, "command not found: raccoon-no-such-tool") {
		t.Errorf("no warning:\n%s", rec.dump())
	}
}

func TestOtherFailuresAreNotReported(t *testing.T) {
	var got []string
	run(t, testJob{
		yaml: `
on: push
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: |
          echo "jq: command not found"; exit 1
      - if: always()
        run: exit 127
`,
		configFn: func(c *Config) { c.OnMissing = func(cmd string) { got = append(got, cmd) } },
	})
	// The first step exits 1, the second has no "not found" line: neither is a
	// missing command.
	if len(got) != 0 {
		t.Fatalf("reported %v", got)
	}
}
