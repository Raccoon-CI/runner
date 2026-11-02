package local

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Raccoon-CI/raccoonci/internal/value"
)

func TestCheckoutCopiesWorkingTree(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, "src"), 0o755)
	os.WriteFile(filepath.Join(repo, "src", "main.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(repo, "run.sh"), []byte("#!/bin/sh"), 0o755)
	os.Symlink("src/main.go", filepath.Join(repo, "link"))

	checkout := Checkout(repo, "octo/hello")
	ws := t.TempDir()
	ok, err := checkout(context.Background(), ws, value.Object{{Key: "path", Value: "sub"}})
	if err != nil || !ok {
		t.Fatalf("checkout: %v %v", ok, err)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "sub", "link")); err != nil || string(b) != "package main" {
		t.Errorf("symlink: %q %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(ws, "sub", "run.sh")); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("mode lost: %v %v", fi, err)
	}

	// Another repository goes to the real action.
	ok, err = checkout(context.Background(), ws, value.Object{{Key: "repository", Value: "octo/other"}})
	if ok || err != nil {
		t.Errorf("other repo: %v %v", ok, err)
	}
	// A path can't climb out of the workspace.
	if ok, err := checkout(context.Background(), ws, value.Object{{Key: "path", Value: "../../escape"}}); !ok || err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "escape", "run.sh")); err != nil {
		t.Errorf("../ path should land inside the workspace: %v", err)
	}
}

func TestCheckoutSkipsWorkdirInsideRepo(t *testing.T) {
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "a"), []byte("a"), 0o644)
	ws := filepath.Join(repo, ".work", "ws")
	if ok, err := Checkout(repo, "o/r")(context.Background(), ws, nil); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(ws, "a")); err != nil {
		t.Error(err)
	}
}
