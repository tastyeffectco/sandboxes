package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// openTestStore opens an in-memory SQLite store with all migrations applied.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, "file::memory:?_fk=1", "../../migrations")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func minimalSandbox(id, idlePolicy string) *Sandbox {
	return &Sandbox{
		ID:             id,
		Status:         "running",
		Image:          "sandboxd-base:1.0.0",
		WorkspaceImg:   "/workspaces/" + id + ".img",
		WorkspaceMnt:   "/workspaces/" + id + ".mnt",
		MemoryHigh:     "4G",
		IdlePolicy:     idlePolicy,
		ExternalUserID: sql.NullString{String: "u1", Valid: true},
	}
}

func TestIdlePolicyRoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	cases := []struct{ id, policy string }{
		{"01AAAAAAAAAAAAAAAAAAAAAAAA", "sleep"},
		{"01BBBBBBBBBBBBBBBBBBBBBBBB", "always_on"},
		{"01CCCCCCCCCCCCCCCCCCCCCCCC", ""},
	}
	for _, tc := range cases {
		if err := st.Create(ctx, minimalSandbox(tc.id, tc.policy)); err != nil {
			t.Fatalf("Create %s: %v", tc.id, err)
		}
	}

	want := map[string]string{
		"01AAAAAAAAAAAAAAAAAAAAAAAA": "sleep",
		"01BBBBBBBBBBBBBBBBBBBBBBBB": "always_on",
		"01CCCCCCCCCCCCCCCCCCCCCCCC": "sleep", // empty defaults to sleep
	}
	for id, wantPolicy := range want {
		sb, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if sb.IdlePolicy != wantPolicy {
			t.Errorf("id=%s: IdlePolicy = %q, want %q", id, sb.IdlePolicy, wantPolicy)
		}
	}
}

func TestListIdleCandidatesExcludesAlwaysOn(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// Create a 'sleep' sandbox and an 'always_on' sandbox, both with an
	// old last_active_at so they'd be idle candidates if policy allowed.
	sleepID := "01DDDDDDDDDDDDDDDDDDDDDDDD"
	alwaysOnID := "01EEEEEEEEEEEEEEEEEEEEEEEE"

	if err := st.Create(ctx, minimalSandbox(sleepID, "sleep")); err != nil {
		t.Fatalf("Create sleep sandbox: %v", err)
	}
	if err := st.Create(ctx, minimalSandbox(alwaysOnID, "always_on")); err != nil {
		t.Fatalf("Create always_on sandbox: %v", err)
	}

	// Set last_active_at to 1 hour ago so both are past any reasonable threshold.
	old := time.Now().UTC().Add(-1 * time.Hour)
	if err := st.BumpLastActive(ctx, sleepID, old); err != nil {
		t.Fatalf("BumpLastActive sleep: %v", err)
	}
	if err := st.BumpLastActive(ctx, alwaysOnID, old); err != nil {
		t.Fatalf("BumpLastActive always_on: %v", err)
	}

	// cutoff = 5 minutes ago — both sandboxes are idle relative to this.
	cutoff := time.Now().UTC().Add(-5 * time.Minute)
	candidates, err := st.ListIdleCandidates(ctx, cutoff)
	if err != nil {
		t.Fatalf("ListIdleCandidates: %v", err)
	}

	for _, sb := range candidates {
		if sb.ID == alwaysOnID {
			t.Errorf("ListIdleCandidates returned always_on sandbox %s — it should be excluded", alwaysOnID)
		}
	}

	found := false
	for _, sb := range candidates {
		if sb.ID == sleepID {
			found = true
		}
	}
	if !found {
		t.Errorf("ListIdleCandidates did not return sleep sandbox %s — it should be a candidate", sleepID)
	}
}

func TestIdlePolicyInListAndListFiltered(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	id := "01FFFFFFFFFFFFFFFFFFFFFFFFFFFF"[0:26]
	sb := minimalSandbox(id, "always_on")
	if err := st.Create(ctx, sb); err != nil {
		t.Fatalf("Create: %v", err)
	}

	all, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, s := range all {
		if s.ID == id && s.IdlePolicy != "always_on" {
			t.Errorf("List: IdlePolicy = %q, want always_on", s.IdlePolicy)
		}
	}

	filtered, err := st.ListFiltered(ctx, "u1", "")
	if err != nil {
		t.Fatalf("ListFiltered: %v", err)
	}
	for _, s := range filtered {
		if s.ID == id && s.IdlePolicy != "always_on" {
			t.Errorf("ListFiltered: IdlePolicy = %q, want always_on", s.IdlePolicy)
		}
	}
}

func TestListIdleCandidatesExcludesAlwaysOnAtPressureCutoff(t *testing.T) {
	// The pressure reaper calls ListIdleCandidates(now); a cutoff of "now"
	// makes every running sandbox a candidate. always_on must STILL be
	// excluded so moderate memory pressure does not stop it. (The emergency
	// path is separate and intentionally ignores idle_policy.)
	st := openTestStore(t)
	ctx := context.Background()

	sleepID, alwaysOnID := "pressure-sleep", "pressure-always-on"
	if err := st.Create(ctx, minimalSandbox(sleepID, "sleep")); err != nil {
		t.Fatalf("Create sleep: %v", err)
	}
	if err := st.Create(ctx, minimalSandbox(alwaysOnID, "always_on")); err != nil {
		t.Fatalf("Create always_on: %v", err)
	}
	recent := time.Now().UTC().Add(-1 * time.Second)
	if err := st.BumpLastActive(ctx, sleepID, recent); err != nil {
		t.Fatalf("BumpLastActive sleep: %v", err)
	}
	if err := st.BumpLastActive(ctx, alwaysOnID, recent); err != nil {
		t.Fatalf("BumpLastActive always_on: %v", err)
	}

	candidates, err := st.ListIdleCandidates(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("ListIdleCandidates: %v", err)
	}
	var sawSleep, sawAlwaysOn bool
	for _, sb := range candidates {
		switch sb.ID {
		case sleepID:
			sawSleep = true
		case alwaysOnID:
			sawAlwaysOn = true
		}
	}
	if sawAlwaysOn {
		t.Errorf("always_on sandbox returned at pressure cutoff — must be excluded")
	}
	if !sawSleep {
		t.Errorf("sleep sandbox not returned at pressure cutoff — must be a candidate")
	}
}
