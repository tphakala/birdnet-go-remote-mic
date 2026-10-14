package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	workflowPath   = "../../.github/workflows/release.yml"
	stepPublish    = "Publish the signed release manifest"
	stepMarkLatest = "Mark the release latest"
	workflowRepo   = "owner/repo"
)

type workflowStep struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
}

// releaseSteps returns the steps of the release workflow's one job.
func releaseSteps(t *testing.T) []workflowStep {
	t.Helper()
	raw, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []workflowStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPath, err)
	}
	if len(wf.Jobs) != 1 {
		t.Fatalf("%d jobs in %s, want 1", len(wf.Jobs), workflowPath)
	}
	for _, j := range wf.Jobs {
		return j.Steps
	}
	return nil
}

func stepIndex(steps []workflowStep, name string) int {
	return slices.IndexFunc(steps, func(s workflowStep) bool { return s.Name == name })
}

// TestWorkflowMarksLatestAfterTheManifest pins the order that keeps the latest
// release's manifest verified: the flip is a later step than the one that
// publishes and verifies the manifest, and nothing passes the decision through
// the environment any more.
func TestWorkflowMarksLatestAfterTheManifest(t *testing.T) {
	t.Parallel()
	steps := releaseSteps(t)
	pub, mark := stepIndex(steps, stepPublish), stepIndex(steps, stepMarkLatest)
	if pub < 0 || mark < 0 {
		t.Fatalf("steps %q at %d and %q at %d, want both present", stepPublish, pub, stepMarkLatest, mark)
	}
	if mark <= pub {
		t.Errorf("%q (step %d) is not after %q (step %d)", stepMarkLatest, mark, stepPublish, pub)
	}
	raw, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "MAKE_LATEST") {
		t.Error("release.yml still mentions MAKE_LATEST")
	}
}

// stubScripts answer gh and go from the environment and log their argv.
const (
	ghStub = `#!/bin/bash
echo "gh $*" >> "$STUB_LOG"
case "$*" in
*isPrerelease*)
  [ "${STUB_PRERELEASE:-}" = fail ] && { echo "api down" >&2; exit 1; }
  echo "$STUB_PRERELEASE" ;;
*tagName*)
  case "$STUB_LATEST" in
  notfound) echo "release not found" >&2; exit 1 ;;
  error) echo "api down" >&2; exit 1 ;;
  *) echo "$STUB_LATEST" ;;
  esac ;;
esac
exit 0
`
	goStub = `#!/bin/bash
echo "go $*" >> "$STUB_LOG"
echo "$STUB_WANT"
`
)

// TestWorkflowMarkLatestStep runs the step's script against stub gh and go
// commands and checks which calls it makes.
func TestWorkflowMarkLatestStep(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not on PATH")
	}
	steps := releaseSteps(t)
	i := stepIndex(steps, stepMarkLatest)
	if i < 0 {
		t.Fatalf("no step %q in %s", stepMarkLatest, workflowPath)
	}
	script := steps[i].Run

	const (
		older      = "v1.2.0" // the latest release the stub reports
		yes        = "true"
		no         = "false"
		tag        = "v1.3.0"
		goCall     = "go run ./tools/releasemanifest make-latest -tag v1.3.0 -latest "
		viewPre    = "gh release view v1.3.0 --repo " + workflowRepo + " --json isPrerelease --jq .isPrerelease"
		viewLatest = "gh release view --repo " + workflowRepo + " --json tagName --jq .tagName"
		edit       = "gh release edit v1.3.0 --repo " + workflowRepo + " --latest"
	)
	for _, tc := range []struct {
		name       string
		prerelease string
		latest     string
		want       string
		wantCalls  []string
		wantFail   bool
	}{
		{"stable, newer than latest", no, older, yes,
			[]string{viewPre, viewLatest, goCall + older, edit}, false},
		{"stable, older than latest", no, "v2.0.0", no,
			[]string{viewPre, viewLatest, goCall + "v2.0.0"}, false},
		{"prerelease", yes, older, yes,
			[]string{viewPre}, false},
		{"no release yet", no, "notfound", yes,
			[]string{viewPre, viewLatest, goCall, edit}, false},
		{"latest lookup fails", no, "error", yes,
			[]string{viewPre, viewLatest}, true},
		{"prerelease lookup fails", "fail", older, yes,
			[]string{viewPre}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o750); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{"gh": ghStub, "go": goStub} {
				//nolint:gosec // an executable stub in a test temp dir
				if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			scriptPath := filepath.Join(dir, "step.sh")
			if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(dir, "calls.log")
			cmd := exec.CommandContext(t.Context(), bash, scriptPath)
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"TAG="+tag,
				"GITHUB_REPOSITORY="+workflowRepo,
				"RUNNER_TEMP="+dir,
				"STUB_LOG="+logPath,
				"STUB_PRERELEASE="+tc.prerelease,
				"STUB_LATEST="+tc.latest,
				"STUB_WANT="+tc.want,
			)
			out, runErr := cmd.CombinedOutput()
			if _, isExit := errors.AsType[*exec.ExitError](runErr); runErr != nil && !isExit {
				t.Fatal(runErr)
			}
			if failed := runErr != nil; failed != tc.wantFail {
				t.Errorf("failed = %v, want %v; output:\n%s", failed, tc.wantFail, out)
			}
			raw, _ := os.ReadFile(logPath) //nolint:gosec // a path in the test's temp dir
			got := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if !slices.Equal(got, tc.wantCalls) {
				t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.wantCalls, "\n"))
			}
		})
	}
}

// TestGoReleaserPublishesNonLatest pins that GoReleaser never makes a release
// latest itself: the workflow's last step does, once the manifest verifies.
func TestGoReleaserPublishesNonLatest(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Release struct {
			Prerelease string `yaml:"prerelease"`
			MakeLatest string `yaml:"make_latest"`
		} `yaml:"release"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse .goreleaser.yaml: %v", err)
	}
	if got := cfg.Release.MakeLatest; got != "false" {
		t.Errorf("release.make_latest = %q, want \"false\"", got)
	}
	if got := cfg.Release.Prerelease; got != "auto" {
		t.Errorf("release.prerelease = %q, want \"auto\"", got)
	}
}
