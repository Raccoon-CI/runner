package actions

import "testing"

func TestNewStoreDropsTokenForMirror(t *testing.T) {
	t.Setenv("RACCOONCI_MIRROR_ACTIONS_API", "https://mirror.example/api")
	if s := NewStore(t.TempDir(), "secret"); s.Token != "" || s.APIBase != "https://mirror.example/api" {
		t.Fatalf("token %q api %q", s.Token, s.APIBase)
	}
	t.Setenv("RACCOONCI_MIRROR_ACTIONS_API", "")
	if s := NewStore(t.TempDir(), "secret"); s.Token != "secret" {
		t.Fatalf("token lost without a mirror")
	}
}
