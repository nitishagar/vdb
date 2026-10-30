// Package repolint executes scripts/repo-lint.sh as an executable
// invariant: Apache-2.0 LICENSE, unofficial disclaimer, thoughts/ never
// tracked, no bundled binaries (invariants 1, 18, 19).
package repolint

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runScript(t *testing.T, dir string) (string, bool) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "repo-lint.sh"))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	return filepath.Dir(strings.TrimSpace(string(out)))
}

// The real repo must always pass its own lint.
func TestRepoLintPassesOnRealRepo(t *testing.T) {
	out, ok := runScript(t, repoRoot(t))
	if !ok {
		t.Fatalf("repo-lint failed on the real repo:\n%s", out)
	}
	if !strings.Contains(out, "repo-lint: OK") {
		t.Errorf("healthy output missing OK marker: %s", out)
	}
}

// Violations must fail loudly: a git repo missing LICENSE + disclaimer and
// tracking thoughts/ fails every clause it violates.
func TestRepoLintFailsOnViolations(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	if err := exec.Command("bash", "-c", "cd "+dir+" && mkdir -p thoughts && echo x > thoughts/ledger.md && echo ok > README.md && printf 'dist/\\n' > .gitignore && git add -A && git commit -qm x").Run(); err != nil {
		t.Fatalf("fixture repo: %v", err)
	}
	out, ok := runScript(t, dir)
	if ok {
		t.Fatalf("repo-lint must fail on violations, passed with:\n%s", out)
	}
	for _, want := range []string{"LICENSE", "disclaimer", "thoughts/"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should mention %q:\n%s", want, out)
		}
	}
}
