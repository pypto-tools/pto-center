package center

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestNodeGroupMembership(t *testing.T) {
	dir := t.TempDir()
	old, err := sql.Open("sqlite3", filepath.Join(dir, "center.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`CREATE TABLE nodes(name TEXT PRIMARY KEY, invitation TEXT NOT NULL, credential TEXT NOT NULL DEFAULT '',
		seen INTEGER NOT NULL DEFAULT 0,group_name TEXT NOT NULL DEFAULT '');
		INSERT INTO nodes(name,invitation,group_name) VALUES('old','','Ops, East'),('empty','','');`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check := func(store *Store, name string, want ...string) {
		t.Helper()
		state, err := store.State()
		if err != nil {
			t.Fatal(err)
		}
		first := ""
		if len(want) > 0 {
			first = want[0]
		}
		for _, n := range state.Nodes {
			if n.Name == name {
				if n.Groups == nil || !slices.Equal(n.Groups, want) || n.Group != first {
					t.Fatalf("%s groups=%q, legacy=%q; want %q", name, n.Groups, n.Group, want)
				}
				return
			}
		}
		t.Fatalf("missing node %s", name)
	}
	check(s, "old", "Ops, East")
	check(s, "empty")
	invite, err := s.Invite("shared", "CI", "User", "CI", "")
	if err != nil {
		t.Fatal(err)
	}
	key := Token()
	if err := s.Enroll("shared", invite, key); err != nil {
		t.Fatal(err)
	}
	check(s, "shared", "CI", "User")
	if err := s.SetGroups("old", []string{"Ops, East"}, []string{"Ops, East", "CI"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGroups("old", []string{"Ops, East"}, []string{"User"}); !errors.Is(err, errGroupsChanged) {
		t.Fatalf("stale edit returned %v", err)
	}
	if err := s.SetGroups("old", []string{"Ops, East", "CI"}, []string{"User", strings.Repeat("a", 81)}); err == nil {
		t.Fatal("invalid edit accepted")
	}
	if err := s.SetGroups("shared", []string{"CI", "User"}, []string{}); err != nil || !s.Auth("shared", key) {
		t.Fatalf("clear groups on enrolled node: %v", err)
	}
	check(s, "shared")
	reopened, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	check(reopened, "old", "Ops, East", "CI")
	check(reopened, "shared")
}
