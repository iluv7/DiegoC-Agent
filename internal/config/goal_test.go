package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGoalConfigDefaultsAndValidation(t *testing.T) {
	for _, tc := range []struct {
		yaml   string
		model  string
		checks int
		valid  bool
	}{
		{"", "main-model", 10, true},
		{"goal:\n  model: small-model\n  max_checks: 3\n", "small-model", 3, true},
		{"goal:\n  max_checks: -1\n", "", 0, false},
		{"goal:\n  max_checks: 101\n", "", 0, false},
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("api_key: test-key\nmodel: main-model\n"+tc.yaml), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := FromYAML(path)
		if !tc.valid {
			if err == nil {
				t.Fatal("invalid goal.max_checks accepted")
			}
			continue
		}
		if err != nil || cfg.Goal.Model != tc.model || cfg.Goal.MaxChecks != tc.checks {
			t.Fatalf("goal config: %+v %v", cfg, err)
		}
	}
}
