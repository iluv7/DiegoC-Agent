package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryInjectionConfigDefaultAndOptOut(t *testing.T) {
	for _, tc := range []struct {
		option  string
		enabled bool
	}{{"", true}, {"  injection_enabled: true\n", true}, {"  injection_enabled: false\n", false}} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("api_key: test\nmemory:\n  enabled: true\n"+tc.option), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := FromYAML(path)
		if err != nil || !cfg.Memory.Enabled || cfg.Memory.InjectionEnabled != tc.enabled {
			t.Fatalf("config: %+v %v", cfg, err)
		}
	}
}
