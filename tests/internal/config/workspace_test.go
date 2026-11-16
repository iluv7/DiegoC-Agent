package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceBackendDefaultsAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		yaml      string
		backend   string
		image     string
		namespace string
		valid     bool
	}{
		{"default", "", "local", "", "", true},
		{"docker", "workspace:\n  backend: docker\n  docker:\n    container: box\n    image: alpine:3.20\n", "docker", "alpine:3.20", "", true},
		{"k8s defaults", "workspace:\n  backend: k8s\n", "k8s", "debian:bookworm-slim", "default", true},
		{"k8s custom", "workspace:\n  backend: k8s\n  k8s:\n    namespace: agent-ns\n    image: alpine:3.20\n    start_timeout_seconds: 30\n", "k8s", "alpine:3.20", "agent-ns", true},
		{"invalid", "workspace:\n  backend: firecracker\n", "", "", "", false},
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
			switch tc.backend {
			case "docker":
				if cfg.Workspace.Docker.Image != tc.image {
					t.Fatalf("docker config: %+v", cfg.Workspace.Docker)
				}
			case "k8s":
				if cfg.Workspace.K8s.Image != tc.image || cfg.Workspace.K8s.Namespace != tc.namespace {
					t.Fatalf("k8s config: %+v", cfg.Workspace.K8s)
				}
				if cfg.Workspace.K8s.StartTimeoutSeconds <= 0 {
					t.Fatalf("k8s start timeout default missing: %+v", cfg.Workspace.K8s)
				}
			}
		})
	}
}
