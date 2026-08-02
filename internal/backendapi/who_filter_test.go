package backendapi

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/warden"
)

func TestFilterLocalBackend(t *testing.T) {
	sessions := []warden.SessionInfo{
		{ID: "a", Backend: "10.0.0.7"},
		{ID: "b", Backend: "10.0.0.8"},
		{ID: "c", Backend: "10.0.0.7"},
		{ID: "d", Backend: ""},
	}
	// PodIP set: only this backend's sessions.
	got := filterLocalBackend(sessions, "10.0.0.7")
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("local filter = %+v, want a,c", got)
	}
	// PodIP empty: cannot scope, returns all (== --all behaviour).
	if all := filterLocalBackend(sessions, ""); len(all) != 4 {
		t.Fatalf("empty podIP must return all, got %d", len(all))
	}
}
