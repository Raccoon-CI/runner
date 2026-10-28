package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// shellCommand returns the command line that runs a `run:` script written to
// scriptPath, following GitHub's table for Linux runners, and the script
// body to write (pwsh gets GitHub's error-handling preamble).
func shellCommand(shell, script, scriptPath string, pathEnv string) (argv []string, body string, err error) {
	var format []string
	switch shell {
	case "":
		// Unspecified runs a different command line than `shell: bash`.
		if _, err := lookPath("bash", pathEnv); err == nil {
			format = []string{"bash", "-e", "{0}"}
		} else {
			format = []string{"sh", "-e", "{0}"}
		}
	case "bash":
		format = []string{"bash", "--noprofile", "--norc", "-eo", "pipefail", "{0}"}
	case "sh":
		format = []string{"sh", "-e", "{0}"}
	case "python":
		format = []string{"python", "{0}"}
	case "pwsh":
		format = []string{"pwsh", "-command", ". '{0}'"}
	case "powershell", "cmd":
		return nil, "", fmt.Errorf("shell %q is only available on Windows runners", shell)
	default:
		if !strings.Contains(shell, "{0}") {
			return nil, "", fmt.Errorf("invalid shell %q: use bash, sh, python or pwsh, or a command line containing {0}", shell)
		}
		format = splitArgs(shell)
	}
	if filepath.Base(format[0]) == "pwsh" {
		script = "$ErrorActionPreference = 'stop'\n" + script +
			"\nif ((Test-Path -LiteralPath variable:\\LASTEXITCODE)) { exit $LASTEXITCODE }"
	}
	for _, a := range format {
		argv = append(argv, strings.ReplaceAll(a, "{0}", scriptPath))
	}
	exe, err := lookPath(argv[0], pathEnv)
	if err != nil && argv[0] == "python" {
		// Debian has no `python` unless python-is-python3 is installed.
		exe, err = lookPath("python3", pathEnv)
	}
	if err != nil {
		return nil, "", err
	}
	argv[0] = exe
	return argv, script, nil
}

// scriptExt is the extension GitHub gives the script file, which some
// interpreters (pwsh) insist on.
func scriptExt(shell string) string {
	name := shell
	if f := splitArgs(shell); len(f) > 0 {
		name = filepath.Base(f[0])
	}
	switch name {
	case "", "bash", "sh":
		return ".sh"
	case "pwsh", "powershell":
		return ".ps1"
	case "python":
		return ".py"
	case "cmd":
		return ".cmd"
	}
	return ""
}

// splitArgs splits a custom shell command line on spaces, honouring single
// and double quotes.
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	inArg := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inArg = r, true
		case r == ' ' || r == '\t':
			if inArg {
				out = append(out, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if inArg {
		out = append(out, cur.String())
	}
	return out
}

// lookPath finds an executable on the step's PATH, which differs from the
// runner's own once steps add to $GITHUB_PATH.
func lookPath(name, pathEnv string) (string, error) {
	if strings.Contains(name, "/") {
		if isExecutable(name) {
			return name, nil
		}
		return "", fmt.Errorf("%s: not found or not executable", name)
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if isExecutable(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: command not found in PATH", name)
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}
