// Command raccoonci-runner is the program that runs on a machine a controller
// sends jobs to: the agent that fetches an assignment, run-instance that runs
// one job instance, the builder of the snapshot such machines start from, and
// `run`, the local engine (without -on). It is the machine side of `raccoonci`,
// which still offers the same commands; the two share their code
// (internal/runnercmd), and the runner repository will own this one (TODO D, S5).
//
// Only the built-in catalog is used here: a machine gets its knowledge of
// toolchains from the binary, and the runner-images data from the controller.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Raccoon-CI/runner/pkg/runnercmd"
	"github.com/Raccoon-CI/lib/pkg/catalog"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: raccoonci-runner <command>

  run [flags] <workflow.yml>  run a workflow on this machine (run -h for flags; no -on here)
  agent [flags]            wait for ~/.raccoonci-agent.json (or -config), run the job it names
  run-instance [flags]     run one job instance read from standard input (the agent starts this)
  runner-snapshot [flags]  build the InstaBox snapshot machines start from
  -version                 print the version
`

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	var err error
	switch args[0] {
	case "run":
		var ok bool
		if ok, err = runnercmd.Run(args[1:], catalog.Default(), nil); err == nil && !ok {
			os.Exit(1)
		}
	case "agent":
		err = runnercmd.Agent(args[1:])
	case "run-instance":
		err = runnercmd.RunInstance(args[1:], catalog.Default())
	case "runner-snapshot":
		err = runnercmd.Snapshot(args[1:])
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "raccoonci-runner:", err)
		os.Exit(1)
	}
}
