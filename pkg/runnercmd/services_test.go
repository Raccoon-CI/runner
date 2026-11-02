package runnercmd

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// Each instance sent to a machine gets a token of its own: it may create
// artifacts as its own job only, and it is not the token of the local run.
func TestEachInstanceGetsATokenForItsOwnJob(t *testing.T) {
	dir := t.TempDir()
	svc, err := LocalServices(filepath.Join(dir, "art"), filepath.Join(dir, "cache"), "run1", false, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Stop()

	envA, err := svc.JobEnv("build-1")
	if err != nil {
		t.Fatal(err)
	}
	envB, _ := svc.JobEnv("build-2")
	tokA, tokB := envA["ACTIONS_RUNTIME_TOKEN"], envB["ACTIONS_RUNTIME_TOKEN"]
	if tokA == "" || tokA == tokB || tokA == svc.Env["ACTIONS_RUNTIME_TOKEN"] {
		t.Fatalf("tokens are not distinct: %q %q %q", tokA, tokB, svc.Env["ACTIONS_RUNTIME_TOKEN"])
	}
	if envA["ACTIONS_RESULTS_URL"] != svc.Env["ACTIONS_RESULTS_URL"] || envA["ACTIONS_CACHE_URL"] != svc.Env["ACTIONS_CACHE_URL"] {
		t.Error("the service addresses changed")
	}

	create := func(tok, job string) int {
		body := `{"workflow_run_backend_id":"run1","workflow_job_run_backend_id":"` + job + `","name":"a","version":4}`
		req, _ := http.NewRequest("POST", svc.Base+"/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := create(tokA, "build-1"); code != 200 {
		t.Errorf("a job creating its own artifact: %d", code)
	}
	if code := create(tokA, "build-2"); code == 200 {
		t.Error("a token created an artifact for another job")
	}
}
