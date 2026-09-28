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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type RunOptions struct {
	WorkDir     string
	ArtifactDir string
	MetricsFile string
	RunIndex    int
	Variant     string
	DryRun      bool
}

// RunMetrics is the normalized execution data emitted by the harness. The
// pointers distinguish an omitted metric from a reported zero value.
type RunMetrics struct {
	Outcome      string   `json:"outcome,omitempty"`
	LimitReached string   `json:"limitReached,omitempty"`
	TurnsUsed    *int     `json:"turnsUsed,omitempty"`
	ContextUsed  *int     `json:"contextUsed,omitempty"`
	ContextSize  *int     `json:"contextSize,omitempty"`
	Cost         *float64 `json:"cost,omitempty"`
}

type Result struct {
	SchemaVersion string           `json:"schemaVersion"`
	Case          ResultCase       `json:"case"`
	SkillVariant  string           `json:"skillVariant,omitempty"`
	Status        string           `json:"status"`
	StartedAt     time.Time        `json:"startedAt"`
	FinishedAt    time.Time        `json:"finishedAt"`
	DurationMS    int64            `json:"durationMs"`
	Workflow      CommandResult    `json:"workflow"`
	Teardown      *CommandResult   `json:"teardown,omitempty"`
	Assertions    []CheckResult    `json:"assertions"`
	Artifacts     []ArtifactResult `json:"artifacts"`
	Rubric        RubricResult     `json:"rubric"`
	Skills        SkillSpec        `json:"skills"`
	Scope         *ScopeResult     `json:"scope,omitempty"`
	Metrics       *RunMetrics      `json:"metrics,omitempty"`
	MetricsError  string           `json:"metricsError,omitempty"`
}

type ResultCase struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type CommandResult struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exitCode"`
	Output   string `json:"output,omitempty"`
	Skipped  bool   `json:"skipped,omitempty"`
}

type CheckResult struct {
	ID          string `json:"id"`
	Passed      bool   `json:"passed"`
	Required    bool   `json:"required"`
	ExitCode    int    `json:"exitCode"`
	Output      string `json:"output,omitempty"`
	Description string `json:"description"`
}

type ArtifactResult struct {
	Path     string `json:"path"`
	Present  bool   `json:"present"`
	Required bool   `json:"required"`
}

type RubricResult struct {
	Version    string           `json:"version"`
	Dimensions []DimensionScore `json:"dimensions"`
}

type DimensionScore struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Weight      float64  `json:"weight"`
	Grader      string   `json:"grader"`
	Evidence    string   `json:"evidence,omitempty"`
	Score       *float64 `json:"score"`
}

func Run(ctx context.Context, c Case, options RunOptions) Result {
	started := time.Now().UTC()
	if options.WorkDir == "" {
		options.WorkDir = "."
	}
	if options.ArtifactDir == "" {
		options.ArtifactDir = options.WorkDir
	}

	result := Result{
		SchemaVersion: ResultVersion,
		Case:          ResultCase{Name: c.Metadata.Name, Version: c.Metadata.Version},
		SkillVariant:  options.Variant,
		Status:        "failed",
		StartedAt:     started,
		Skills:        c.Skills,
		Rubric:        RubricResult{Version: c.Rubric.Version},
	}
	for _, dimension := range c.Rubric.Dimensions {
		result.Rubric.Dimensions = append(result.Rubric.Dimensions, DimensionScore{
			ID: dimension.ID, Description: dimension.Description, Weight: dimension.Weight,
			Grader: dimension.Grader, Evidence: dimension.Evidence,
		})
	}
	var scopeBefore fileSnapshot
	var scopeBeforeErr error
	if c.Scope != nil && !options.DryRun {
		scopeBefore, scopeBeforeErr = snapshotRepository(options.WorkDir)
	}
	if options.Variant != "" {
		found := false
		for _, variant := range c.Skills.Variants {
			if variant.Name == options.Variant {
				found = true
				break
			}
		}
		if !found {
			result.Workflow = CommandResult{
				Command: c.Workflow.Command, ExitCode: -1,
				Output: fmt.Sprintf("unknown skill variant %q", options.Variant),
			}
			finishResult(&result)
			return result
		}
	}

	env := []string{"EVAL_CASE_NAME=" + c.Metadata.Name, fmt.Sprintf("EVAL_RUN_INDEX=%d", options.RunIndex)}
	if options.Variant != "" {
		env = append(env, "EVAL_SKILL_VARIANT="+options.Variant)
	}
	if c.Workflow.Timeout != "" {
		if timeout, err := time.ParseDuration(c.Workflow.Timeout); err == nil {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
	}

	if options.DryRun {
		result.Status = "not_run"
		result.Workflow = CommandResult{Command: c.Workflow.Command, ExitCode: -1, Skipped: true}
		for _, assertion := range c.Assertions {
			result.Assertions = append(result.Assertions, CheckResult{
				ID: assertion.ID, Required: assertion.Required, ExitCode: -1, Description: assertion.Description,
			})
		}
	} else {
		result.Workflow = runCommand(ctx, options.WorkDir, c.Workflow.Command, env)
		allRequiredPassed := result.Workflow.ExitCode == 0
		for _, assertion := range c.Assertions {
			check := runAssertion(ctx, options.WorkDir, assertion, env)
			passed := check.ExitCode == 0
			result.Assertions = append(result.Assertions, CheckResult{
				ID: assertion.ID, Passed: passed, Required: assertion.Required,
				ExitCode: check.ExitCode, Output: check.Output, Description: assertion.Description,
			})
			if assertion.Required && !passed {
				allRequiredPassed = false
			}
		}
		for _, artifact := range c.Artifacts {
			path := artifact.Path
			if !filepath.IsAbs(path) {
				path = filepath.Join(options.ArtifactDir, path)
			}
			_, err := os.Stat(path)
			present := err == nil
			result.Artifacts = append(result.Artifacts, ArtifactResult{
				Path: artifact.Path, Present: present, Required: artifact.Required,
			})
			if artifact.Required && !present {
				allRequiredPassed = false
			}
		}
		if allRequiredPassed {
			result.Status = "passed"
		}
		if c.Workflow.Teardown != "" {
			teardown := runCommand(ctx, options.WorkDir, c.Workflow.Teardown, env)
			result.Teardown = &teardown
			if teardown.ExitCode != 0 {
				result.Status = "failed"
			}
		}
	}
	if c.Scope != nil && !options.DryRun {
		if scopeBeforeErr != nil {
			result.Scope = &ScopeResult{Allowed: append([]string(nil), c.Scope.Allowed...), Error: scopeBeforeErr.Error()}
		} else {
			scopeAfter, err := snapshotRepository(options.WorkDir)
			if err != nil {
				result.Scope = &ScopeResult{Allowed: append([]string(nil), c.Scope.Allowed...), Error: err.Error()}
			} else {
				scope := evaluateScope(scopeBefore, scopeAfter, c.Scope.Allowed)
				result.Scope = &scope
			}
		}
		if result.Scope.Error != "" || !result.Scope.Passed {
			result.Status = "failed"
		}
	}
	if options.MetricsFile != "" && !options.DryRun {
		metricsPath := options.MetricsFile
		if !filepath.IsAbs(metricsPath) {
			metricsPath = filepath.Join(options.WorkDir, metricsPath)
		}
		metrics, err := LoadMetrics(metricsPath)
		if err != nil {
			result.MetricsError = err.Error()
		} else {
			result.Metrics = &metrics
		}
	}
	if result.MetricsError != "" {
		result.Status = "failed"
	}

	finishResult(&result)
	return result
}

// LoadMetrics reads either a normalized RunMetrics document or the harness's
// termination-log shape, whose usage values are nested under "usage".
func LoadMetrics(path string) (RunMetrics, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RunMetrics{}, fmt.Errorf("read metrics file %q: %w", path, err)
	}

	var document struct {
		Outcome      string      `json:"outcome"`
		LimitReached string      `json:"limitReached"`
		TurnsUsed    *int        `json:"turnsUsed"`
		ContextUsed  *int        `json:"contextUsed"`
		ContextSize  *int        `json:"contextSize"`
		Cost         *float64    `json:"cost"`
		Usage        *RunMetrics `json:"usage"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return RunMetrics{}, fmt.Errorf("parse metrics file %q: %w", path, err)
	}

	metrics := RunMetrics{
		Outcome:      document.Outcome,
		LimitReached: document.LimitReached,
		TurnsUsed:    document.TurnsUsed,
		ContextUsed:  document.ContextUsed,
		ContextSize:  document.ContextSize,
		Cost:         document.Cost,
	}
	if document.Usage != nil {
		metrics.TurnsUsed = document.Usage.TurnsUsed
		metrics.ContextUsed = document.Usage.ContextUsed
		metrics.ContextSize = document.Usage.ContextSize
		metrics.Cost = document.Usage.Cost
	}
	if metrics.Outcome == "" && metrics.LimitReached == "" && metrics.TurnsUsed == nil && metrics.ContextUsed == nil && metrics.ContextSize == nil && metrics.Cost == nil {
		return RunMetrics{}, fmt.Errorf("metrics file %q contains no recognized metrics", path)
	}
	return metrics, nil
}

func finishResult(result *Result) {
	finished := time.Now().UTC()
	result.FinishedAt = finished
	result.DurationMS = finished.Sub(result.StartedAt).Milliseconds()
}

func runCommand(ctx context.Context, workDir, command string, env []string) CommandResult {
	result := CommandResult{Command: command, ExitCode: -1}
	if strings.TrimSpace(command) == "" {
		result.Output = "command is empty"
		return result
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), env...)
	output, err := cmd.CombinedOutput()
	result.Output = string(output)
	if err == nil {
		result.ExitCode = 0
		return result
	}
	if exitError, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exitError.ExitCode()
		return result
	}
	result.Output = fmt.Sprintf("%s\n%s", result.Output, err)
	return result
}

func runAssertion(ctx context.Context, workDir string, assertion Assertion, env []string) CommandResult {
	timeout := time.Duration(0)
	interval := 5 * time.Second
	if assertion.Timeout != "" {
		timeout, _ = time.ParseDuration(assertion.Timeout)
	}
	if assertion.Interval != "" {
		interval, _ = time.ParseDuration(assertion.Interval)
	}

	deadline := time.Now().Add(timeout)
	for {
		result := runCommand(ctx, workDir, assertion.Command, env)
		if result.ExitCode == 0 || timeout == 0 || time.Now().After(deadline) {
			return result
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result
		case <-timer.C:
		}
	}
}

func WriteResult(path string, result Result) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal eval result: %w", err)
	}
	data = append(data, '\n')
	if path == "-" || path == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create result directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write eval result %q: %w", path, err)
	}
	return nil
}
