package account

import "testing"

func TestValidateUsername(t *testing.T) {
	for _, ok := range []string{"alice", "a", "user.name", "user_name", "user-name", "a@b.c", "A1"} {
		if err := ValidateUsername(ok); err != nil {
			t.Errorf("ValidateUsername(%q) = %v, want nil", ok, err)
		}
	}
	// A slash would let a name escape its URL path segment; the rest could
	// confuse a path or a listing.
	for _, bad := range []string{"", "a/b", "a\\b", "a b", "..", ".", "a:b", "a\x00b"} {
		if err := ValidateUsername(bad); err == nil {
			t.Errorf("ValidateUsername(%q) = nil, want an error", bad)
		}
	}
}

func TestValidateHome(t *testing.T) {
	got, err := ValidateHome("/volume1/homes/alice/")
	if err != nil {
		t.Fatalf("ValidateHome: %v", err)
	}
	if got != "/volume1/homes/alice" {
		t.Errorf("ValidateHome = %q, want it cleaned", got)
	}
	for _, bad := range []string{"", "homes/alice", "./alice"} {
		if _, err := ValidateHome(bad); err == nil {
			t.Errorf("ValidateHome(%q) = nil, want an error", bad)
		}
	}
}

// TestCheckConflicts covers the tenant-isolation invariant. The nested cases
// are the ones nothing else can catch: no path confinement is violated when one
// account's directory simply contains another's.
func TestCheckConflicts(t *testing.T) {
	existing := []Mapping{
		{Username: "alice", Home: "/homes/alice"},
		{Username: "bob", Home: "/homes/bob"},
	}
	tests := []struct {
		name      string
		candidate Mapping
		wantErr   bool
	}{
		{"a fresh account", Mapping{"carol", "/homes/carol"}, false},
		{"a sibling directory", Mapping{"carol", "/homes/alice2"}, false},
		{"duplicate username", Mapping{"alice", "/homes/carol"}, true},
		{"duplicate username, different case", Mapping{"ALICE", "/homes/carol"}, true},
		{"the same directory", Mapping{"carol", "/homes/alice"}, true},
		{"inside another account", Mapping{"carol", "/homes/alice/shared"}, true},
		{"deep inside another account", Mapping{"carol", "/homes/alice/a/b/c"}, true},
		{"containing another account", Mapping{"carol", "/homes"}, true},
		{"containing everything", Mapping{"carol", "/"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckConflicts(tc.candidate, existing)
			if tc.wantErr && err == nil {
				t.Errorf("CheckConflicts(%+v) = nil, want an error", tc.candidate)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("CheckConflicts(%+v) = %v, want nil", tc.candidate, err)
			}
		})
	}
}

// TestCheckConflictsIgnoresPrefixCollisions: "/homes/alice2" starts with
// "/homes/alice" as a string but is not inside it.
func TestCheckConflictsIgnoresPrefixCollisions(t *testing.T) {
	existing := []Mapping{{Username: "alice", Home: "/homes/alice"}}
	if err := CheckConflicts(Mapping{"carol", "/homes/alice2"}, existing); err != nil {
		t.Errorf("a sibling with a shared prefix was rejected: %v", err)
	}
}
