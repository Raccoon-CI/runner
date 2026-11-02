package runnercmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Raccoon-CI/raccoonci/internal/catalog"
	"github.com/Raccoon-CI/raccoonci/internal/nodejs"
	"github.com/Raccoon-CI/raccoonci/internal/provision"
)

// RunEnv is what a machine needs to run job instances besides the workflow:
// the Node.js runtimes for actions, the tool cache and the provisioner. `run`
// and `run-instance` build it the same way.
type RunEnv struct {
	CacheDir  string // <user cache>/raccoonci
	Node      *nodejs.Runtime
	NodeMusl  *nodejs.Runtime // for job containers on musl images (Alpine)
	ToolCache string
	Prov      *provision.Provisioner // nil with -install none
}

// ValidInstall checks the value of -install.
func ValidInstall(install string) error {
	switch install {
	case "none", "toolchains", "all":
		return nil
	}
	return fmt.Errorf("-install %q: want none, toolchains or all", install)
}

// NewRunEnv makes the run environment under the user's cache directory.
func NewRunEnv(cat *catalog.Catalog, install string) (*RunEnv, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	rt := &RunEnv{CacheDir: filepath.Join(cache, "raccoonci")}
	rt.Node = nodejs.New(filepath.Join(rt.CacheDir, "node"))
	rt.Node.Catalog = cat
	// Inside node.Dir, so the mount that gives containers the Node.js
	// runtimes covers it.
	rt.NodeMusl = nodejs.NewMusl(filepath.Join(rt.Node.Dir, "musl"))
	rt.NodeMusl.Catalog = cat
	rt.ToolCache = filepath.Join(rt.CacheDir, "toolcache")
	switch install {
	case "none":
	case "toolchains", "all":
		rt.Prov = &provision.Provisioner{ToolCache: rt.ToolCache, Catalog: cat, Installers: provision.DefaultInstallers(nil)}
		if install == "all" {
			if rt.Prov.Apt, err = provision.NewApt(); err != nil {
				fmt.Fprintf(os.Stderr, "raccoonci: -install=all: %v; missing packages will only be reported\n", err)
			}
		}
	default:
		return nil, fmt.Errorf("-install %q: want none, toolchains or all", install)
	}
	return rt, nil
}
