package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeRunnerFixture(t *testing.T, root, path, content string) {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerExecutesPrivatePackageTestsAndForwardsFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		args []string
		pass bool
	}{
		{name: "default packages", want: "3", pass: true},
		{name: "assertion failure", want: "4", args: []string{"-count=1", "./..."}},
		{name: "test filter", want: "4", args: []string{"-run=DoesNotExist", "./..."}, pass: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeRunnerFixture(t, root, "go.mod", "module runner-fixture\n\ngo 1.25.0\n")
			writeRunnerFixture(t, root, "tests/go.mod", "module runner-fixture-tests\n\ngo 1.25.0\n")
			writeRunnerFixture(t, root, "sum.go", "package fixture\nfunc sum(a, b int) int { return a+b }\n")
			writeRunnerFixture(t, root, "tests/sum_test.go", `package fixture
import "testing"
func TestPrivateSum(t *testing.T) {
    if got := sum(1, 2); got != `+tc.want+` { t.Fatalf("unexpected sum: %d", got) }
}
`)
			t.Chdir(root)
			if code := run(tc.args); (code == 0) != tc.pass {
				t.Fatalf("exit code %d; want pass=%v", code, tc.pass)
			}
			if _, err := os.Stat(filepath.Join(root, "sum_test.go")); !os.IsNotExist(err) {
				t.Fatal("runner wrote a test file into the source directory")
			}
		})
	}
}

func TestOverlayRejectsMissingPackagesAndSourceFileCollisions(t *testing.T) {
	for _, tc := range []struct {
		name, source string
	}{
		{name: "missing source directory"},
		{name: "source file collision", source: "pkg/value_test.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeRunnerFixture(t, root, "tests/pkg/value_test.go", "package pkg\n")
			if tc.source != "" {
				writeRunnerFixture(t, root, tc.source, "package pkg\n")
			}
			if _, err := testOverlay(root); err == nil {
				t.Fatal("invalid test layout was accepted")
			}
		})
	}
}
