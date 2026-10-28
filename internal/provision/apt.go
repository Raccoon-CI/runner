package provision

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Apt installs Debian packages. It needs root, or passwordless sudo.
type Apt struct {
	// Sudo runs apt-get through `sudo -n`.
	Sudo bool
	// Run executes a command and returns its combined output; tests replace
	// it.
	Run func(ctx context.Context, argv ...string) ([]byte, error)
	// IsInstalled asks dpkg whether a package is installed; tests replace it.
	IsInstalled func(pkg string) bool

	mu      sync.Mutex
	updated bool
}

// NewApt returns an Apt for this machine, or ErrNoPrivileges if the current
// user can neither be root nor use sudo without a password.
func NewApt() (*Apt, error) {
	a := &Apt{}
	if os.Geteuid() != 0 {
		if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
			return nil, ErrNoPrivileges
		}
		a.Sudo = true
	}
	return a, nil
}

func (a *Apt) run(ctx context.Context, argv ...string) ([]byte, error) {
	if a.Run != nil {
		return a.Run(ctx, argv...)
	}
	return exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
}

// Installed reports whether dpkg considers pkg installed.
func (a *Apt) Installed(pkg string) bool {
	if a.IsInstalled != nil {
		return a.IsInstalled(pkg)
	}
	out, err := exec.Command("dpkg-query", "-W", "-f=${Status}", pkg).Output()
	return err == nil && strings.Contains(string(out), "install ok installed")
}

// command builds an apt-get invocation that never asks questions. Package
// names reach here already checked by the catalog (no leading dash).
func (a *Apt) command(args ...string) []string {
	var argv []string
	if a.Sudo {
		argv = append(argv, "sudo", "-n")
	}
	argv = append(argv, "env", "DEBIAN_FRONTEND=noninteractive", "apt-get")
	return append(argv, args...)
}

// Install installs pkgs and returns the error for each package that could
// not be installed. It first tries them all at once; if that fails, it
// tries them one at a time, so one package that Debian doesn't have doesn't
// keep the others from being installed.
func (a *Apt) Install(ctx context.Context, pkgs []string) map[string]error {
	failures := map[string]error{}
	if err := a.update(ctx); err != nil {
		for _, p := range pkgs {
			failures[p] = err
		}
		return failures
	}
	install := func(names ...string) error {
		out, err := a.run(ctx, a.command(append([]string{"install", "-y", "--no-install-recommends"}, names...)...)...)
		if err != nil {
			return fmt.Errorf("%w: %s", err, lastLines(string(out), 3))
		}
		return nil
	}
	if err := install(pkgs...); err == nil {
		return failures
	} else if len(pkgs) == 1 {
		failures[pkgs[0]] = err
		return failures
	}
	for _, p := range pkgs {
		if err := install(p); err != nil {
			failures[p] = err
		}
	}
	return failures
}

// update refreshes the package lists, once.
func (a *Apt) update(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.updated {
		return nil
	}
	if out, err := a.run(ctx, a.command("update", "-qq")...); err != nil {
		return fmt.Errorf("apt-get update: %w: %s", err, lastLines(string(out), 3))
	}
	a.updated = true
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
