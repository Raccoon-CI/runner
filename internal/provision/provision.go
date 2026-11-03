// Package provision installs what the scanner says a job needs, before the
// job runs: toolchains go into the tool cache in the layout the setup-*
// actions look in (so they find them and download nothing), tools and apt
// packages are checked for, and apt packages can be installed.
//
// Provisioning never fails a job. The scan it works from is a heuristic, so
// everything that goes wrong is reported in the Result for the caller to
// show as a warning.
package provision

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/Raccoon-CI/lib/pkg/catalog"
	"github.com/Raccoon-CI/runner/internal/semver"
	"github.com/Raccoon-CI/runner/pkg/scan"
)

// Installer fetches one toolchain from where it is published. Which
// installer a toolchain uses is the catalog's choice (toolchains.<name>.installer);
// what the installers download from is not, and stays in code.
type Installer interface {
	// Resolve turns a version spec (20, 20.x, >=18, lts/*, ...) into the
	// exact version to install, without a leading v.
	Resolve(ctx context.Context, spec string) (string, error)
	// Install downloads version into dest, which must only appear once the
	// download is complete and verified.
	Install(ctx context.Context, version, dest string) error
	// BinDirs are the directories (relative to the install) holding the
	// toolchain's commands.
	BinDirs() []string
}

type Status string

const (
	Installed Status = "installed" // downloaded or installed just now
	Cached    Status = "cached"    // already there
	Skipped   Status = "skipped"   // nothing for us to do; Detail says why
	Failed    Status = "failed"    // needed, and could not be provided
)

type Item struct {
	Demand scan.Demand
	Status Status
	Detail string
	Err    error
	// BinDirs are where the installed toolchain's commands are.
	BinDirs []string
}

type Result struct {
	Items []Item
	// PathDirs must be put in front of PATH: the commands of toolchains the
	// job uses without setting them up itself (implicit demands).
	PathDirs []string
}

// Failed lists the items that could not be provided.
func (r *Result) Failed() []Item {
	var out []Item
	for _, it := range r.Items {
		if it.Status == Failed {
			out = append(out, it)
		}
	}
	return out
}

type Provisioner struct {
	// ToolCache is $RUNNER_TOOL_CACHE: <tool>/<version>/<arch>[.complete].
	ToolCache string
	Catalog   *catalog.Catalog
	// Installers by name, as toolchains.<name>.installer refers to them.
	Installers map[string]Installer
	// Apt installs packages. With nil, missing packages are only reported.
	Apt *Apt
	// Parallel bounds concurrent downloads (default 4).
	Parallel int
	// LookPath finds a command on PATH; exec.LookPath if nil.
	LookPath func(string) (string, error)
	// Probe runs a command and returns its output, to read the version of a
	// toolchain already on the machine; run directly if nil.
	Probe func(ctx context.Context, command string, args []string) (string, error)

	locks sync.Map // "<tool>@<version>" -> *sync.Mutex
}

// Arch is the architecture directory name toolkit's tool cache uses.
func Arch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	}
	return runtime.GOARCH
}

func (p *Provisioner) cat() *catalog.Catalog {
	if p.Catalog == nil {
		return catalog.Default()
	}
	return p.Catalog
}

func (p *Provisioner) lookPath(cmd string) (string, error) {
	if p.LookPath != nil {
		return p.LookPath(cmd)
	}
	return exec.LookPath(cmd)
}

// Ensure provides what demands ask for. Downloads run in parallel; the items
// come back in the order of the demands.
func (p *Provisioner) Ensure(ctx context.Context, demands []scan.Demand) *Result {
	res := &Result{Items: make([]Item, len(demands))}
	for i, d := range demands {
		res.Items[i].Demand = d
	}
	parallel := p.Parallel
	if parallel < 1 {
		parallel = 4
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	run := func(f func()) {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			f()
		})
	}

	var pkgs []int // indexes of package demands, installed in one go
	for i, d := range demands {
		switch d.Kind {
		case catalog.Toolchain:
			run(func() { p.ensureToolchain(ctx, &res.Items[i]) })
		case catalog.Tool:
			p.ensureTool(&res.Items[i])
		case catalog.Package:
			pkgs = append(pkgs, i)
		}
	}
	if len(pkgs) > 0 {
		run(func() { p.ensurePackages(ctx, res.Items, pkgs) })
	}
	wg.Wait()

	for _, it := range res.Items {
		if it.Demand.Implicit && it.Demand.Kind == catalog.Toolchain && (it.Status == Installed || it.Status == Cached) {
			for _, d := range it.BinDirs {
				if !slices.Contains(res.PathDirs, d) {
					res.PathDirs = append(res.PathDirs, d)
				}
			}
		}
	}
	return res
}

func (p *Provisioner) lock(key string) *sync.Mutex {
	m, _ := p.locks.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (p *Provisioner) ensureToolchain(ctx context.Context, it *Item) {
	d := it.Demand
	fail := func(format string, args ...any) {
		it.Status, it.Err = Failed, fmt.Errorf(format, args...)
	}
	if d.Version == "" {
		it.Status, it.Detail = Skipped, "its version is only known at run time; the setup action installs it"
		return
	}
	def := p.cat().Toolchains[d.Name]
	if def != nil && def.ImplicitOnly && !d.Implicit {
		it.Status, it.Detail = Skipped, "its setup action installs "+p.cat().Title(d.Name)+" itself"
		return
	}
	var inst Installer
	if def != nil {
		inst = p.Installers[def.Installer]
	}
	if inst == nil {
		if d.Implicit {
			fail("%s %s is needed but there is no installer for %s yet", d.Name, d.Version, p.cat().Title(d.Name))
		} else {
			it.Status, it.Detail = Skipped, "no installer for "+p.cat().Title(d.Name)+" yet; the setup action downloads it"
		}
		return
	}
	name := p.cat().ToolcacheName(d.Name)
	binDirs := func(version string) []string {
		var out []string
		for _, b := range inst.BinDirs() {
			out = append(out, filepath.Join(p.versionDir(name, version), Arch(), b))
		}
		return out
	}

	// A job that uses a tool without asking for a version only needs the
	// image's version in spirit: if that exact patch isn't published any
	// more (python-build-standalone keeps only the newest patch of each
	// minor), the newest patch of the same minor will do. A version the
	// workflow asked for by name is never swapped for another.
	specs := []string{d.Version}
	if want, ok := semver.Parse(d.Version); ok && d.Implicit && !d.Declared {
		specs = append(specs, fmt.Sprintf("%d.%d", want.Major, want.Minor))
	}

	// Like the setup-* actions, prefer what is already cached to asking the
	// network: a spec that a cached version satisfies is done.
	for _, spec := range specs {
		if v, err := semver.Pick(spec, p.cachedVersions(name)); err == nil {
			p.touch(name, v)
			it.Status, it.Detail, it.BinDirs = Cached, fmt.Sprintf("%s %s", d.Name, v), binDirs(v)
			return
		}
	}
	// A copy that came with the machine (a provider's template ships Go,
	// Node.js...) is used as it is rather than downloaded again.
	if v, ok := p.adoptSystem(ctx, name, def, specs); ok {
		p.touch(name, v)
		it.Status, it.Detail, it.BinDirs = Cached, fmt.Sprintf("%s %s (already on this machine)", d.Name, v), binDirs(v)
		return
	}
	var version string
	var firstErr error
	for i, spec := range specs {
		v, err := inst.Resolve(ctx, spec)
		if err == nil {
			version, firstErr = v, nil
			break
		}
		if i == 0 {
			firstErr = err // about the version that was asked for
		}
	}
	if firstErr != nil {
		fail("resolving %s %s: %w", d.Name, d.Version, firstErr)
		return
	}
	mu := p.lock(name + "@" + version)
	mu.Lock()
	defer mu.Unlock()
	if p.complete(name, version) { // another job installed it while we waited
		p.touch(name, version)
		it.Status, it.Detail, it.BinDirs = Cached, fmt.Sprintf("%s %s", d.Name, version), binDirs(version)
		return
	}
	if err := p.install(ctx, inst, name, version); err != nil {
		fail("installing %s %s: %w", d.Name, version, err)
		return
	}
	it.Status, it.Detail, it.BinDirs = Installed, fmt.Sprintf("%s %s", d.Name, version), binDirs(version)
}

// touch records that a cached version was just chosen, for Prune: the
// .complete marker's modification time is the version's last use.
func (p *Provisioner) touch(name, version string) {
	now := time.Now()
	os.Chtimes(filepath.Join(p.versionDir(name, version), Arch()+".complete"), now, now)
}

func (p *Provisioner) versionDir(name, version string) string {
	return filepath.Join(p.ToolCache, name, version)
}

// complete says whether version is fully in the tool cache. The install
// directory is stat'ed too, so an adopted copy whose original has been removed
// (a symlink to nowhere) does not count.
func (p *Provisioner) complete(name, version string) bool {
	dir := p.versionDir(name, version)
	if _, err := os.Stat(filepath.Join(dir, Arch()+".complete")); err != nil {
		return false
	}
	arch := filepath.Join(dir, Arch())
	if _, err := os.Stat(arch); err != nil {
		return false
	}
	// A slim adopted install is a directory of links; one that points nowhere
	// means the machine lost the tool.
	entries, _ := os.ReadDir(filepath.Join(arch, "bin"))
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 {
			if _, err := os.Stat(filepath.Join(arch, "bin", e.Name())); err != nil {
				return false
			}
		}
	}
	return true
}

func (p *Provisioner) runProbe(ctx context.Context, command string, args []string) (string, error) {
	if p.Probe != nil {
		return p.Probe(ctx, command, args)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, command, args...).Output()
	return string(out), err
}

// adoptSystem looks for a copy of the toolchain that is already installed on
// this machine and satisfies one of specs. If there is one it is linked into
// the tool cache, where the setup-* actions look, and its version is returned.
// What to run and how to read the version comes from the catalog's probe; a
// toolchain without one is never adopted. Anything that does not look right
// (the command is not in a bin directory, the version does not parse) is
// "no copy", never an error.
func (p *Provisioner) adoptSystem(ctx context.Context, name string, def *catalog.ToolchainDef, specs []string) (string, bool) {
	if def == nil || def.Probe == nil {
		return "", false
	}
	pr := def.Probe
	path, err := p.lookPath(pr.Command)
	if err != nil {
		return "", false
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil || filepath.Base(filepath.Dir(real)) != "bin" {
		return "", false
	}
	out, err := p.runProbe(ctx, real, pr.Args)
	if err != nil {
		return "", false
	}
	version, ok := pr.Version(out)
	if !ok {
		return "", false
	}
	if _, ok := semver.Parse(version); !ok {
		return "", false
	}
	matched := false
	for _, spec := range specs {
		if _, err := semver.Pick(spec, []string{version}); err == nil {
			matched = true
			break
		}
	}
	if !matched {
		return "", false
	}

	root := filepath.Dir(filepath.Dir(real)) // <root>/bin/<command>
	mu := p.lock(name + "@" + version)
	mu.Lock()
	defer mu.Unlock()
	if p.complete(name, version) {
		return version, true
	}
	dir := p.versionDir(name, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false
	}
	link := filepath.Join(dir, Arch())
	os.RemoveAll(link) // what an interrupted adoption left; the marker is not there yet
	if p.sharesPrefix(name, root) {
		// The root is an install prefix several tools share (/usr/local holds Go,
		// Node.js...). Linking all of it would put the others on PATH under this
		// toolchain's name, in front of the version another step just selected.
		if err := slimInstall(link, root, commandsOf(name, def)); err != nil {
			return "", false
		}
	} else if err := os.Symlink(root, link); err != nil {
		return "", false
	}
	if err := os.WriteFile(filepath.Join(dir, Arch()+".complete"), nil, 0o644); err != nil {
		return "", false
	}
	return version, true
}

func commandsOf(name string, def *catalog.ToolchainDef) []string {
	if def != nil && len(def.Commands) > 0 {
		return def.Commands
	}
	return []string{name}
}

// sharesPrefix says whether root/bin also holds commands of another toolchain.
func (p *Provisioner) sharesPrefix(name, root string) bool {
	for other, def := range p.cat().Toolchains {
		if other == name {
			continue
		}
		for _, c := range commandsOf(other, def) {
			if _, err := os.Lstat(filepath.Join(root, "bin", c)); err == nil {
				return true
			}
		}
	}
	return false
}

// slimInstall makes dest/bin holding links to just the given commands of root.
func slimInstall(dest, root string, commands []string) error {
	bin := filepath.Join(dest, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	n := 0
	for _, c := range commands {
		src := filepath.Join(root, "bin", c)
		if _, err := os.Lstat(src); err != nil {
			continue
		}
		if err := os.Symlink(src, filepath.Join(bin, c)); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return errors.New("none of the commands is there")
	}
	return nil
}

// cachedVersions lists the complete installs of a tool in the tool cache.
func (p *Provisioner) cachedVersions(name string) []string {
	entries, _ := os.ReadDir(filepath.Join(p.ToolCache, name))
	var out []string
	for _, e := range entries {
		if e.IsDir() && p.complete(name, e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// install downloads version into the tool cache: into a private directory
// first, then renamed into place, then marked complete the way
// @actions/tool-cache does (so setup-* actions trust it).
func (p *Provisioner) install(ctx context.Context, inst Installer, name, version string) error {
	vdir := p.versionDir(name, version)
	if err := os.MkdirAll(vdir, 0o755); err != nil {
		return err
	}
	part, err := os.MkdirTemp(vdir, ".part-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(part)
	if err := inst.Install(ctx, version, part); err != nil {
		return err
	}
	if p.complete(name, version) {
		return nil // another raccoonci process finished first; don't replace what it may be using
	}
	final := filepath.Join(vdir, Arch())
	if err := os.RemoveAll(final); err != nil { // debris of an interrupted install
		return err
	}
	if err := os.Rename(part, final); err != nil {
		return err
	}
	return os.WriteFile(final+".complete", nil, 0o644)
}

// ensureTool checks for a tool that has no installer (yet).
func (p *Provisioner) ensureTool(it *Item) {
	d := it.Demand
	for _, cmd := range p.cat().CommandsOf(d.Kind, d.Name) {
		if path, err := p.lookPath(cmd); err == nil {
			it.Status, it.Detail = Cached, path
			return
		}
	}
	it.Status, it.Err = Failed, fmt.Errorf("%s is needed but isn't installed, and there is no installer for it yet", d.Name)
}

// ensurePackages checks the apt packages items[idx] ask for and installs the
// missing ones in one go if p.Apt allows.
func (p *Provisioner) ensurePackages(ctx context.Context, items []Item, idx []int) {
	var missing []int
	for _, i := range idx {
		d := items[i].Demand
		deb := p.cat().DebianName(d.Name)
		found := ""
		for _, cmd := range p.cat().CommandsOf(catalog.Package, d.Name) {
			if path, err := p.lookPath(cmd); err == nil {
				found = path
				break
			}
		}
		switch {
		case found != "":
			items[i].Status, items[i].Detail = Cached, found
		case p.Apt != nil && p.Apt.Installed(deb):
			items[i].Status, items[i].Detail = Cached, deb
		default:
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return
	}
	if p.Apt == nil {
		for _, i := range missing {
			items[i].Status = Failed
			items[i].Err = fmt.Errorf("package %s is needed but not installed (installing packages is not enabled)", p.cat().DebianName(items[i].Demand.Name))
		}
		return
	}
	var names []string
	for _, i := range missing {
		names = append(names, p.cat().DebianName(items[i].Demand.Name))
	}
	failures := p.Apt.Install(ctx, names)
	for _, i := range missing {
		deb := p.cat().DebianName(items[i].Demand.Name)
		if err := failures[deb]; err != nil {
			items[i].Status, items[i].Err = Failed, fmt.Errorf("installing package %s: %w", deb, err)
		} else {
			items[i].Status, items[i].Detail = Installed, deb
		}
	}
}

// ErrNoPrivileges means packages can't be installed on this machine.
var ErrNoPrivileges = errors.New("installing packages needs root or passwordless sudo")

// Describe is a one-line summary of an item for logs.
func (it Item) Describe() string {
	d := it.Demand
	asked := d.Name
	if d.Version != "" {
		asked += " " + d.Version
	}
	switch it.Status {
	case Failed:
		return fmt.Sprintf("%s: %v", asked, it.Err)
	case Skipped:
		return fmt.Sprintf("%s: %s", asked, it.Detail)
	}
	label := d.Name
	switch {
	case d.Kind == catalog.Toolchain:
		label = it.Detail // the exact version, e.g. node 20.20.2
	case d.Kind == catalog.Package && it.Status == Installed && it.Detail != d.Name:
		label += " (" + it.Detail + ")" // Debian's name for it
	}
	if it.Status == Installed {
		return "installed " + label
	}
	return label + " is already available"
}
