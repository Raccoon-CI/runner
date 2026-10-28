// Package actions reads action metadata (action.yml) and downloads the
// actions that `uses:` steps refer to.
package actions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"racoonci/internal/value"
	"racoonci/internal/workflow"
)

type Action struct {
	Name    string
	Inputs  []Input
	Outputs []Output
	Runs    Runs
}

type Input struct {
	Name        string
	Default     any // may contain ${{ }} expressions, e.g. ${{ github.token }}
	Required    bool
	Deprecation string
}

type Output struct {
	Name  string
	Value string // composite actions only: an expression over their steps
}

type Runs struct {
	// Using is node12/16/20/24, composite or docker.
	Using string

	// JavaScript actions.
	Main, Pre, Post string
	PreIf, PostIf   string // GitHub's default for both is always()

	// Composite actions.
	Steps []*workflow.Step

	// Docker actions.
	Image                         string
	Entrypoint                    string
	PreEntrypoint, PostEntrypoint string
	Args                          []any
	Env                           value.Object
}

// IsNode reports whether the action runs with Node.js.
func (r Runs) IsNode() bool {
	return strings.HasPrefix(r.Using, "node")
}

// Load reads the metadata of the action in dir.
func Load(dir string) (*Action, error) {
	for _, name := range []string{"action.yml", "action.yaml"} {
		src, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		a, err := Parse(src)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return a, nil
	}
	return nil, fmt.Errorf("can't find action.yml or action.yaml in %s", dir)
}

// Parse parses action metadata. Unlike workflow files, unknown keys are
// ignored: action authors add their own (author, branding, input types), and
// GitHub accepts them.
func Parse(src []byte) (*Action, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("action metadata is empty")
	}
	dec := value.NewDecoder(value.DefaultNodeBudget)
	a := &Action{}
	var runs *yaml.Node
	err := mapping(doc.Content[0], func(k string, v *yaml.Node) (err error) {
		switch k {
		case "name":
			a.Name = v.Value
		case "inputs":
			err = mapping(v, func(name string, v *yaml.Node) error {
				in := Input{Name: name}
				err := mapping(v, func(k string, v *yaml.Node) (err error) {
					switch k {
					case "default":
						in.Default, err = dec.Decode(v)
					case "required":
						in.Required = v.Value == "true"
					case "deprecationMessage":
						in.Deprecation = v.Value
					}
					return err
				})
				a.Inputs = append(a.Inputs, in)
				return err
			})
		case "outputs":
			err = mapping(v, func(name string, v *yaml.Node) error {
				out := Output{Name: name}
				a.Outputs = append(a.Outputs, out)
				return mapping(v, func(k string, v *yaml.Node) error {
					if k == "value" {
						a.Outputs[len(a.Outputs)-1].Value = v.Value
					}
					return nil
				})
			})
		case "runs":
			runs = v
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if runs == nil {
		return nil, errors.New(`action has no "runs"`)
	}
	if err := a.parseRuns(dec, runs); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Action) parseRuns(dec *value.Decoder, n *yaml.Node) error {
	r := &a.Runs
	var steps *yaml.Node
	err := mapping(n, func(k string, v *yaml.Node) (err error) {
		switch k {
		case "using":
			r.Using = strings.ToLower(v.Value)
		case "main":
			r.Main = v.Value
		case "pre":
			r.Pre = v.Value
		case "post":
			r.Post = v.Value
		case "pre-if":
			r.PreIf = v.Value
		case "post-if":
			r.PostIf = v.Value
		case "steps":
			steps = v
		case "image":
			r.Image = v.Value
		case "entrypoint":
			r.Entrypoint = v.Value
		case "pre-entrypoint":
			r.PreEntrypoint = v.Value
		case "post-entrypoint":
			r.PostEntrypoint = v.Value
		case "args":
			var args any
			args, err = dec.Decode(v)
			if err == nil {
				var ok bool
				if r.Args, ok = args.([]any); !ok && args != nil {
					err = fmt.Errorf("line %d: runs.args must be a list", v.Line)
				}
			}
		case "env":
			var env any
			env, err = dec.Decode(v)
			if err == nil {
				var ok bool
				if r.Env, ok = env.(value.Object); !ok && env != nil {
					err = fmt.Errorf("line %d: runs.env must be a mapping", v.Line)
				}
			}
		}
		return err
	})
	if err != nil {
		return err
	}
	switch {
	case r.IsNode():
		if r.Main == "" {
			return fmt.Errorf("%s action has no runs.main", r.Using)
		}
	case r.Using == "composite":
		if steps == nil {
			return errors.New("composite action has no runs.steps")
		}
		r.Steps, err = workflow.DecodeSteps(dec, steps, "runs")
		return err
	case r.Using == "docker":
		if r.Image == "" {
			return errors.New("docker action has no runs.image")
		}
	case r.Using == "":
		return errors.New(`action has no runs.using`)
	default:
		return fmt.Errorf("unsupported runs.using %q", r.Using)
	}
	return nil
}

func mapping(n *yaml.Node, fn func(k string, v *yaml.Node) error) error {
	for n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expected a mapping", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		v := n.Content[i+1]
		for v.Kind == yaml.AliasNode {
			v = v.Alias
		}
		if err := fn(n.Content[i].Value, v); err != nil {
			return err
		}
	}
	return nil
}
