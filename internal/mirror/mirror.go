// Package mirror lets the user point Raccoon CI's downloads at a mirror, for
// networks where GitHub, nodejs.org or go.dev are slow or blocked.
//
// It reads environment variables on purpose, and never the catalog: a catalog
// may come from the network and must not be able to redirect downloads. The
// variable is RACCOONCI_MIRROR_<NAME>, holding the replacement for the
// default base URL of that source (same layout below it, no trailing slash
// needed). Names: NODE, NODE_MUSL, GO, PYTHON_LATEST, PYTHON_RELEASE, RUST,
// JAVA, ACTIONS_API, ACTIONS_CODELOAD.
//
// Checksums are still checked, but they are fetched from the mirror too, so a
// mirror is trusted as much as the origin would be.
package mirror

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Prefix of the environment variables.
const Prefix = "RACCOONCI_MIRROR_"

// Base returns the base URL for name: the environment's override when it is
// set and valid, else def. An invalid value (not an http or https URL with a
// host) is ignored with a note on stderr, so a typo never breaks a run.
func Base(name, def string) string {
	return base(os.Getenv, name, def)
}

func base(getenv func(string) string, name, def string) string {
	v := strings.TrimSpace(getenv(Prefix + name))
	if v == "" {
		return def
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		fmt.Fprintf(os.Stderr, "warning: ignoring %s%s: not an http(s) URL without credentials\n", Prefix, name)
		return def
	}
	return strings.TrimRight(v, "/")
}
