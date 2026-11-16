package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceBackendDefaultsAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		yaml    string
		backend string
		valid   bool
	}{
		{"default", "", "local", true},
		{"explicit", "workspace:\n  backend: LOCAL\n", "local", true},
		{"invalid", "workspace:\n  backend: firecracker\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("api_key: test-key\n"+tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := FromYAML(path)
			if !tc.valid {
				if err == nil {
					t.Fatal("invalid backend accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.Workspace.Backend != tc.backend {
				t.Fatalf("workspace config: %+v", cfg.Workspace)
			}
		})
	}
}
