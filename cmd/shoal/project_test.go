package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveProjectPrecedence checks the order: --project, then
// SHOAL_PROJECT, then the default file written by `shoal project use`.
func TestResolveProjectPrecedence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "project")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, flag, env, file, want string
	}{
		{"flag beats env and file", "from-flag", "from-env", file, "from-flag"},
		{"env beats file", "", "from-env", file, "from-env"},
		{"file is the default", "", "", file, "from-file"},
		{"no file means none", "", "", filepath.Join(t.TempDir(), "absent"), ""},
		{"no file path means none", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveProject(tc.flag, tc.env, tc.file)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("resolveProject(%q, %q, file) = %q, want %q", tc.flag, tc.env, got, tc.want)
			}
		})
	}
}

// TestRootHasProjectFlag checks that -p and --project are global flags, so
// every command accepts them.
func TestRootHasProjectFlag(t *testing.T) {
	root := newRootCmd()
	for _, name := range []string{"project", "quota"} {
		if _, _, err := root.Find([]string{name}); err != nil {
			t.Fatalf("command %s: %v", name, err)
		}
	}
	if root.PersistentFlags().Lookup("project") == nil || root.PersistentFlags().ShorthandLookup("p") == nil {
		t.Fatal("--project/-p is not a root persistent flag")
	}
}
