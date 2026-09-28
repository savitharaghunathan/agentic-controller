/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package evals

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func validCase() Case {
	return Case{
		APIVersion: CaseAPIVersion,
		Kind:       CaseKind,
		Metadata:   Metadata{Name: "example", Version: "1"},
		Workflow:   WorkflowSpec{Command: "true"},
		Assertions: []Assertion{{ID: "workflow-ready", Description: "ready", Command: "true", Required: true}},
		Rubric:     Rubric{Version: "1", Dimensions: []Dimension{{ID: "quality", Description: "quality", Weight: 1, Grader: "human"}}},
	}
}

func TestCaseValidate(t *testing.T) {
	if err := validCase().Validate(); err != nil {
		t.Fatalf("valid case rejected: %v", err)
	}

	c := validCase()
	c.Rubric.Dimensions[0].Weight = 0.5
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "sum to 1.0") {
		t.Fatalf("expected weight validation error, got %v", err)
	}
}

func TestLoadCase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "case.yaml")
	contents := `apiVersion: evals.konveyor.io/v1alpha1
kind: WorkflowEvalCase
metadata:
  name: loaded
  version: "1"
workflow:
  command: true
assertions:
  - id: ok
    description: command succeeds
    command: true
rubric:
  version: "1"
  dimensions:
    - id: quality
      description: quality
      weight: 1
      grader: human
`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCase(path)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if c.Metadata.Name != "loaded" || c.Workflow.Command != "true" {
		t.Fatalf("unexpected case: %+v", c)
	}
}

func TestLoadCaseRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "case.yaml")
	contents := `apiVersion: evals.konveyor.io/v1alpha1
kind: WorkflowEvalCase
metadata:
  name: loaded
  version: "1"
workflow:
  command: true
unknown: typo
rubric:
  version: "1"
  dimensions:
    - id: quality
      description: quality
      weight: 1
      grader: human
`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCase(path); err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestRunRecordsRequiredChecksAndArtifacts(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "PLAN.md"), []byte("plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := validCase()
	c.Artifacts = []Artifact{{Path: "PLAN.md", Required: true}, {Path: "missing", Required: true}}
	c.Assertions = append(c.Assertions, Assertion{ID: "missing", Description: "fails", Command: "false", Required: true})

	result := Run(context.Background(), c, RunOptions{WorkDir: workDir})
	if result.Status != "failed" {
		t.Fatalf("expected failed result, got %q", result.Status)
	}
	if len(result.Assertions) != 2 || !result.Assertions[0].Passed || result.Assertions[1].Passed {
		t.Fatalf("unexpected assertion results: %+v", result.Assertions)
	}
	if !result.Artifacts[0].Present || result.Artifacts[1].Present {
		t.Fatalf("unexpected artifact results: %+v", result.Artifacts)
	}
	if result.Rubric.Dimensions[0].Score != nil {
		t.Fatal("qualitative rubric score should be unset")
	}
}

func TestRunDryRunDoesNotExecuteCommands(t *testing.T) {
	c := validCase()
	c.Workflow.Command = "exit 42"
	result := Run(context.Background(), c, RunOptions{DryRun: true})
	if result.Status != "not_run" || !result.Workflow.Skipped || result.Workflow.ExitCode != -1 || result.Assertions[0].ExitCode != -1 {
		t.Fatalf("unexpected dry-run result: %+v", result)
	}
}

func TestRunRetriesAssertionUntilItPasses(t *testing.T) {
	workDir := t.TempDir()
	c := validCase()
	c.Assertions = []Assertion{{
		ID: "eventually-ready", Description: "becomes ready", Required: true,
		Command: "test -f ready || { touch ready; exit 1; }",
		Timeout: "1s", Interval: "1ms",
	}}

	result := Run(context.Background(), c, RunOptions{WorkDir: workDir})
	if result.Status != "passed" || len(result.Assertions) != 1 || !result.Assertions[0].Passed {
		t.Fatalf("expected retrying assertion to pass: %+v", result)
	}
}

func TestRunRejectsUnknownVariant(t *testing.T) {
	result := Run(context.Background(), validCase(), RunOptions{Variant: "missing"})
	if result.Status != "failed" || !strings.Contains(result.Workflow.Output, "unknown skill variant") {
		t.Fatalf("unexpected unknown variant result: %+v", result)
	}
}

func TestRunLoadsMetricsAfterWorkflow(t *testing.T) {
	workDir := t.TempDir()
	c := validCase()
	c.Workflow.Command = `printf '%s\n' '{"outcome":"succeeded","usage":{"turnsUsed":4,"cost":1.5}}' > termination.json`

	result := Run(context.Background(), c, RunOptions{WorkDir: workDir, MetricsFile: "termination.json"})
	if result.Status != "passed" || result.MetricsError != "" || result.Metrics == nil {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Metrics.TurnsUsed == nil || *result.Metrics.TurnsUsed != 4 || result.Metrics.Cost == nil || *result.Metrics.Cost != 1.5 {
		t.Fatalf("unexpected metrics: %+v", result.Metrics)
	}
}

func TestRunScopeAllowsDeclaredChanges(t *testing.T) {
	workDir := initGitRepo(t)
	c := validCase()
	c.Scope = &ScopeSpec{Allowed: []string{".eval-artifacts/"}}
	c.Workflow.Command = `mkdir -p .eval-artifacts && printf '%s\n' output > .eval-artifacts/result.txt`

	result := Run(context.Background(), c, RunOptions{WorkDir: workDir})
	if result.Status != "passed" || result.Scope == nil || !result.Scope.Passed {
		t.Fatalf("scope check failed: %+v", result)
	}
	if len(result.Scope.Changed) != 1 || result.Scope.Changed[0] != ".eval-artifacts/result.txt" {
		t.Fatalf("unexpected changed files: %+v", result.Scope.Changed)
	}
}

func TestRunScopeRejectsUnexpectedChanges(t *testing.T) {
	workDir := initGitRepo(t)
	c := validCase()
	c.Scope = &ScopeSpec{Allowed: []string{".eval-artifacts/"}}
	c.Workflow.Command = `mkdir -p .eval-artifacts && printf '%s\n' output > .eval-artifacts/result.txt && printf '%s\n' unexpected > notes.txt`

	result := Run(context.Background(), c, RunOptions{WorkDir: workDir})
	if result.Status != "failed" || result.Scope == nil || result.Scope.Passed {
		t.Fatalf("unexpected scope result: %+v", result)
	}
	if len(result.Scope.Unexpected) != 1 || result.Scope.Unexpected[0] != "notes.txt" {
		t.Fatalf("unexpected files: %+v", result.Scope.Unexpected)
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	workDir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", workDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	return workDir
}
