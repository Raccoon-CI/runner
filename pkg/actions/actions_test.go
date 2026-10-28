package actions

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseNode(t *testing.T) {
	a, err := Parse([]byte(`
name: Setup Thing
author: someone
branding: {icon: box}
inputs:
  token:
    default: ${{ github.token }}
    type: string
  version:
    required: true
    deprecationMessage: use version-file
outputs:
  path: {description: where}
runs:
  using: Node24
  main: dist/index.js
  post: dist/post.js
  post-if: success()
`))
	if err != nil {
		t.Fatal(err)
	}
	if a.Runs.Using != "node24" || !a.Runs.IsNode() || a.Runs.Main != "dist/index.js" || a.Runs.PostIf != "success()" {
		t.Errorf("runs = %+v", a.Runs)
	}
	if len(a.Inputs) != 2 || a.Inputs[0].Default != "${{ github.token }}" || !a.Inputs[1].Required || a.Inputs[1].Deprecation == "" {
		t.Errorf("inputs = %+v", a.Inputs)
	}
	if len(a.Outputs) != 1 || a.Outputs[0].Name != "path" {
		t.Errorf("outputs = %+v", a.Outputs)
	}
}

func TestParseCompositeAndDocker(t *testing.T) {
	a, err := Parse([]byte(`
runs:
  using: composite
  steps:
    - id: x
      run: echo hi
      shell: bash
    - uses: actions/cache@v4
      with: {path: x}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Runs.Steps) != 2 || a.Runs.Steps[0].ID != "x" || a.Runs.Steps[1].Uses != "actions/cache@v4" {
		t.Errorf("steps = %+v", a.Runs.Steps)
	}
	a, err = Parse([]byte("runs:\n  using: docker\n  image: Dockerfile\n  args: [a, '${{ inputs.b }}']\n  env: {K: v}\n"))
	if err != nil || a.Runs.Image != "Dockerfile" || len(a.Runs.Args) != 2 || len(a.Runs.Env) != 1 {
		t.Errorf("docker: %+v %v", a, err)
	}
	for _, bad := range []string{
		"name: x",
		"runs: {using: node20}",
		"runs: {using: composite}",
		"runs: {using: python, main: x}",
		"runs:\n  using: composite\n  steps:\n    - name: neither run nor uses\n",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestParseRef(t *testing.T) {
	r, err := ParseRef("github/codeql-action/init@v3")
	if err != nil || r != (Ref{Owner: "github", Repo: "codeql-action", Path: "init", Version: "v3"}) {
		t.Errorf("got %+v %v", r, err)
	}
	if r.String() != "github/codeql-action/init@v3" {
		t.Errorf("String = %s", r)
	}
	r, err = ParseRef("actions/checkout@releases/v1")
	if err != nil || r.Version != "releases/v1" {
		t.Errorf("got %+v %v", r, err)
	}
	for _, bad := range []string{"actions/checkout", "checkout@v4", "a/../b@v1", "a/b@", "a/b@../x", "a b/c@v1"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

type entry struct {
	name, body, link string
	typ              byte
}

func tarball(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "abc"}}); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body)), Linkname: e.link}
		if e.typ == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestStoreFetch(t *testing.T) {
	good := tarball(t, []entry{
		{name: "octo-act-abc/", typ: tar.TypeDir},
		{name: "octo-act-abc/action.yml", body: "runs: {using: node24, main: index.js}\n"},
		{name: "octo-act-abc/sub/action.yml", body: "runs: {using: composite, steps: []}\n"},
		{name: "octo-act-abc/link", typ: tar.TypeSymlink, link: "action.yml"},
	})
	var hits, authed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" {
			authed.Add(1)
		}
		switch r.URL.Path {
		case "/octo/act/tar.gz/v1", "/repos/octo/act/tarball/v1":
			w.Write(good)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	s := NewStore(t.TempDir(), "")
	s.CodeloadBase, s.APIBase = srv.URL, srv.URL
	ctx := context.Background()
	ref := Ref{Owner: "octo", Repo: "act", Version: "v1"}
	dirs := make(chan string, 3)
	for range 3 {
		go func() {
			d, err := s.Fetch(ctx, ref)
			if err != nil {
				t.Error(err)
			}
			dirs <- d
		}()
	}
	dir := <-dirs
	<-dirs
	<-dirs
	if hits.Load() != 1 {
		t.Errorf("%d downloads, want 1", hits.Load())
	}
	if a, err := Load(dir); err != nil || a.Runs.Main != "index.js" {
		t.Errorf("Load: %+v %v", a, err)
	}
	if _, err := Load(filepath.Join(dir, "sub")); err != nil {
		t.Error(err)
	}
	if authed.Load() != 0 {
		t.Error("sent a token without one configured")
	}

	if _, err := s.Fetch(ctx, Ref{Owner: "octo", Repo: "missing", Version: "v1"}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing repo: %v", err)
	}

	s = NewStore(t.TempDir(), "tok")
	s.CodeloadBase, s.APIBase = srv.URL, srv.URL
	if _, err := s.Fetch(ctx, ref); err != nil || authed.Load() != 1 {
		t.Errorf("token download: %v (authed %d)", err, authed.Load())
	}
}
