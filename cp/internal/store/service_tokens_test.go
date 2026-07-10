package store

import "testing"

func TestParseRole(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    Role
		wantErr bool
	}{
		{"Developer", RoleDeveloper, false},
		{"developer", RoleDeveloper, false},
		{"dev", RoleDeveloper, false},
		{"Project Admin", RoleProjectAdmin, false},
		{"project-admin", RoleProjectAdmin, false},
		{"project_admin", RoleProjectAdmin, false},
		{"Org Admin", RoleOrgAdmin, false},
		{"org-admin", RoleOrgAdmin, false},
		{"admin", RoleOrgAdmin, false},
		{"root", "", true},
		{"", "", true},
	} {
		got, err := ParseRole(tc.in)
		if tc.wantErr != (err != nil) {
			t.Fatalf("ParseRole(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if got != tc.want {
			t.Fatalf("ParseRole(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRoleAtLeast(t *testing.T) {
	for _, tc := range []struct {
		r, min Role
		want   bool
	}{
		{RoleOrgAdmin, RoleDeveloper, true},
		{RoleOrgAdmin, RoleOrgAdmin, true},
		{RoleProjectAdmin, RoleProjectAdmin, true},
		{RoleProjectAdmin, RoleOrgAdmin, false},
		{RoleDeveloper, RoleProjectAdmin, false},
		{Role("bogus"), RoleDeveloper, false}, // unknown roles never pass
	} {
		if got := tc.r.AtLeast(tc.min); got != tc.want {
			t.Fatalf("%q.AtLeast(%q) = %v, want %v", tc.r, tc.min, got, tc.want)
		}
	}
}
