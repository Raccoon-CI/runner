package runner

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// command is a workflow command a step prints on stdout or stderr, such as
// `::error file=app.js,line=1::Missing semicolon`.
type command struct {
	name  string
	props map[string]string
	data  string
}

var knownCommands = map[string]bool{
	"add-mask": true, "set-output": true, "save-state": true,
	"set-env": true, "add-path": true,
	"debug": true, "notice": true, "warning": true, "error": true,
	"group": true, "endgroup": true, "echo": true, "stop-commands": true,
	"add-matcher": true, "remove-matcher": true,
}

var (
	unescapeData     = strings.NewReplacer("%0D", "\r", "%0d", "\r", "%0A", "\n", "%0a", "\n", "%25", "%")
	unescapeProperty = strings.NewReplacer("%0D", "\r", "%0d", "\r", "%0A", "\n", "%0a", "\n",
		"%3A", ":", "%3a", ":", "%2C", ",", "%2c", ",", "%25", "%")
)

// unescapeV1 undoes the escaping of the older `##[name k=v;k=v]data` format.
var unescapeV1 = strings.NewReplacer("%3B", ";", "%3b", ";", "%0D", "\r", "%0d", "\r",
	"%0A", "\n", "%0a", "\n", "%5D", "]", "%5d", "]", "%25", "%")

// parseCommand parses a line the way the GitHub runner does: leading
// whitespace is allowed, unknown command names are plain output, and
// malformed properties are dropped. Both the current `::name k=v::data` and
// the older `##[name k=v;]data` format are commands; actions still print
// the older one (setup-node's ##[add-matcher]).
func parseCommand(line string) (command, bool) {
	line = strings.TrimLeft(line, " \t")
	if strings.HasPrefix(line, "##[") {
		return parseCommandV1(line)
	}
	if !strings.HasPrefix(line, "::") {
		return command{}, false
	}
	end := strings.Index(line[2:], "::")
	if end < 0 {
		return command{}, false
	}
	info := line[2 : 2+end]
	name, propStr, hasProps := strings.Cut(info, " ")
	if !knownCommands[name] {
		return command{}, false
	}
	c := command{name: name, props: map[string]string{}, data: unescapeData.Replace(line[2+end+2:])}
	if hasProps {
		for _, p := range strings.Split(strings.TrimSpace(propStr), ",") {
			k, v, ok := strings.Cut(p, "=")
			if ok && k != "" && v != "" {
				c.props[k] = unescapeProperty.Replace(v)
			}
		}
	}
	return c, true
}

func parseCommandV1(line string) (command, bool) {
	end := strings.Index(line, "]")
	if end < 0 {
		return command{}, false
	}
	name, propStr, hasProps := strings.Cut(line[3:end], " ")
	if !knownCommands[name] {
		return command{}, false
	}
	c := command{name: name, props: map[string]string{}, data: unescapeV1.Replace(line[end+1:])}
	if hasProps {
		for _, p := range strings.Split(propStr, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && k != "" && v != "" {
				c.props[k] = unescapeV1.Replace(v)
			}
		}
	}
	return c, true
}

// parseKeyValueFile parses the format of $GITHUB_ENV, $GITHUB_OUTPUT and
// $GITHUB_STATE: `name=value` lines, or multi-line values written as
//
//	name<<DELIMITER
//	line 1
//	line 2
//	DELIMITER
func parseKeyValueFile(data []byte) ([][2]string, error) {
	var out [][2]string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, len(data)+1)
	lineNo := 0
	next := func() (string, bool) {
		if !sc.Scan() {
			return "", false
		}
		lineNo++
		return strings.TrimSuffix(sc.Text(), "\r"), true
	}
	for {
		line, ok := next()
		if !ok {
			break
		}
		if line == "" {
			continue
		}
		eq, heredoc := strings.Index(line, "="), strings.Index(line, "<<")
		switch {
		case eq >= 0 && (heredoc < 0 || eq < heredoc):
			key := line[:eq]
			if key == "" {
				return nil, fmt.Errorf("line %d: empty name in %q", lineNo, line)
			}
			out = append(out, [2]string{key, line[eq+1:]})
		case heredoc >= 0:
			key, delim := line[:heredoc], line[heredoc+2:]
			if key == "" || delim == "" {
				return nil, fmt.Errorf("line %d: invalid format %q", lineNo, line)
			}
			start := lineNo
			var lines []string
			found := false
			for {
				l, ok := next()
				if !ok {
					break
				}
				if l == delim {
					found = true
					break
				}
				lines = append(lines, l)
			}
			if !found {
				return nil, fmt.Errorf("line %d: matching delimiter %q not found", start, delim)
			}
			out = append(out, [2]string{key, strings.Join(lines, "\n")})
		default:
			return nil, fmt.Errorf("line %d: invalid format %q", lineNo, line)
		}
	}
	return out, sc.Err()
}

// parsePathFile parses $GITHUB_PATH: one directory per line.
func parsePathFile(data []byte) []string {
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
