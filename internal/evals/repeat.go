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
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RepeatOptions controls repeated execution of one evaluation case. Each run
// receives its own work and artifact directory.
type RepeatOptions struct {
	Runs                  int
	WorkDirRoot           string
	ArtifactDirRoot       string
	ResultDir             string
	MetricsFileTemplate   string
	Variant               string
	DryRun                bool
	MaxFailureRatePercent float64
}

type RepeatResult struct {
	SchemaVersion         string            `json:"schemaVersion"`
	Case                  ResultCase        `json:"case"`
	SkillVariant          string            `json:"skillVariant,omitempty"`
	Status                string            `json:"status"`
	Passed                bool              `json:"passed"`
	Runs                  int               `json:"runs"`
	PassedRuns            int               `json:"passedRuns"`
	FailedRuns            int               `json:"failedRuns"`
	FailureRatePercent    float64           `json:"failureRatePercent"`
	MaxFailureRatePercent float64           `json:"maxFailureRatePercent"`
	Metrics               RepeatMetrics     `json:"metrics"`
	Results               []RepeatRunResult `json:"results"`
}

type RepeatRunResult struct {
	Run         int    `json:"run"`
	WorkDir     string `json:"workDir"`
	ArtifactDir string `json:"artifactDir"`
	ResultPath  string `json:"resultPath,omitempty"`
	Result      Result `json:"result"`
}

type RepeatMetrics struct {
	DurationMS *NumericStats    `json:"durationMs,omitempty"`
	Cost       *NumericStats    `json:"cost,omitempty"`
	Turns      *NumericStats    `json:"turnsUsed,omitempty"`
	Assertions []AssertionStats `json:"assertions,omitempty"`
}

type AssertionStats struct {
	ID                 string  `json:"id"`
	Runs               int     `json:"runs"`
	Passed             int     `json:"passed"`
	Failed             int     `json:"failed"`
	FailureRatePercent float64 `json:"failureRatePercent"`
}

// NumericStats uses population variance because the measured runs are the
// complete sample being reported, rather than a sample of a larger dataset.
type NumericStats struct {
	Count    int     `json:"count"`
	Min      float64 `json:"min"`
	Max      float64 `json:"max"`
	Mean     float64 `json:"mean"`
	Variance float64 `json:"variance"`
	StdDev   float64 `json:"stddev"`
}

func RunRepeated(ctx context.Context, c Case, options RepeatOptions) (RepeatResult, error) {
	if options.Runs <= 0 {
		return RepeatResult{}, fmt.Errorf("runs must be greater than zero")
	}
	if options.MaxFailureRatePercent < 0 || options.MaxFailureRatePercent > 100 {
		return RepeatResult{}, fmt.Errorf("max failure rate must be between 0 and 100 percent")
	}

	workRoot := options.WorkDirRoot
	if workRoot == "" {
		var err error
		workRoot, err = os.MkdirTemp("", "eval-repeat-work-")
		if err != nil {
			return RepeatResult{}, fmt.Errorf("create repeat work directory: %w", err)
		}
	}
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		return RepeatResult{}, fmt.Errorf("create repeat work root: %w", err)
	}

	artifactRoot := options.ArtifactDirRoot
	if artifactRoot == "" {
		artifactRoot = workRoot
	}
	if err := os.MkdirAll(artifactRoot, 0o755); err != nil {
		return RepeatResult{}, fmt.Errorf("create repeat artifact root: %w", err)
	}
	if options.ResultDir != "" {
		if err := os.MkdirAll(options.ResultDir, 0o755); err != nil {
			return RepeatResult{}, fmt.Errorf("create repeat result directory: %w", err)
		}
	}

	report := RepeatResult{
		SchemaVersion:         ResultVersion,
		Case:                  ResultCase{Name: c.Metadata.Name, Version: c.Metadata.Version},
		SkillVariant:          options.Variant,
		Runs:                  options.Runs,
		MaxFailureRatePercent: options.MaxFailureRatePercent,
	}
	for run := 1; run <= options.Runs; run++ {
		workDir := filepath.Join(workRoot, fmt.Sprintf("run-%03d", run))
		artifactDir := filepath.Join(artifactRoot, fmt.Sprintf("run-%03d", run))
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return RepeatResult{}, fmt.Errorf("create work directory for run %d: %w", run, err)
		}
		if err := os.MkdirAll(artifactDir, 0o755); err != nil {
			return RepeatResult{}, fmt.Errorf("create artifact directory for run %d: %w", run, err)
		}

		metricsFile := repeatPath(options.MetricsFileTemplate, run)
		if metricsFile != "" && !filepath.IsAbs(metricsFile) {
			metricsFile = filepath.Join(workDir, metricsFile)
		}
		result := Run(ctx, c, RunOptions{
			WorkDir: workDir, ArtifactDir: artifactDir, MetricsFile: metricsFile,
			RunIndex: run, Variant: options.Variant, DryRun: options.DryRun,
		})
		runResult := RepeatRunResult{Run: run, WorkDir: workDir, ArtifactDir: artifactDir, Result: result}
		if options.ResultDir != "" {
			runResult.ResultPath = filepath.Join(options.ResultDir, fmt.Sprintf("run-%03d.json", run))
			if err := WriteResult(runResult.ResultPath, result); err != nil {
				return RepeatResult{}, fmt.Errorf("write result for run %d: %w", run, err)
			}
		}
		report.Results = append(report.Results, runResult)
	}

	for _, run := range report.Results {
		if run.Result.Status == "passed" {
			report.PassedRuns++
		} else {
			report.FailedRuns++
		}
	}
	report.FailureRatePercent = float64(report.FailedRuns) / float64(report.Runs) * 100
	switch {
	case report.FailedRuns == 0:
		report.Status = "passed"
	case report.PassedRuns == 0:
		report.Status = "failed"
	default:
		report.Status = "flaky"
	}
	report.Passed = report.FailureRatePercent <= report.MaxFailureRatePercent
	report.Metrics = repeatMetrics(report.Results)
	return report, nil
}

func repeatPath(template string, run int) string {
	if template == "" {
		return ""
	}
	return strings.ReplaceAll(template, "{run}", fmt.Sprintf("%03d", run))
}

func repeatMetrics(results []RepeatRunResult) RepeatMetrics {
	durations := make([]float64, 0, len(results))
	costs := make([]float64, 0, len(results))
	turns := make([]float64, 0, len(results))
	assertions := map[string]AssertionStats{}
	for _, run := range results {
		durations = append(durations, float64(run.Result.DurationMS))
		for _, assertion := range run.Result.Assertions {
			stat := assertions[assertion.ID]
			stat.ID = assertion.ID
			stat.Runs++
			if assertion.Passed {
				stat.Passed++
			} else {
				stat.Failed++
			}
			assertions[assertion.ID] = stat
		}
		if run.Result.Metrics == nil {
			continue
		}
		if run.Result.Metrics.Cost != nil {
			costs = append(costs, *run.Result.Metrics.Cost)
		}
		if run.Result.Metrics.TurnsUsed != nil {
			turns = append(turns, float64(*run.Result.Metrics.TurnsUsed))
		}
	}
	metrics := RepeatMetrics{DurationMS: stats(durations)}
	if len(costs) > 0 {
		metrics.Cost = stats(costs)
	}
	if len(turns) > 0 {
		metrics.Turns = stats(turns)
	}
	ids := make([]string, 0, len(assertions))
	for id := range assertions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		stat := assertions[id]
		stat.FailureRatePercent = float64(stat.Failed) / float64(stat.Runs) * 100
		metrics.Assertions = append(metrics.Assertions, stat)
	}
	return metrics
}

func stats(values []float64) *NumericStats {
	if len(values) == 0 {
		return nil
	}
	minimum, maximum := values[0], values[0]
	var total float64
	for _, value := range values {
		minimum = math.Min(minimum, value)
		maximum = math.Max(maximum, value)
		total += value
	}
	mean := total / float64(len(values))
	var varianceTotal float64
	for _, value := range values {
		delta := value - mean
		varianceTotal += delta * delta
	}
	variance := varianceTotal / float64(len(values))
	return &NumericStats{Count: len(values), Min: minimum, Max: maximum, Mean: mean, Variance: variance, StdDev: math.Sqrt(variance)}
}

func WriteRepeatReport(path string, report RepeatResult) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal repeat report: %w", err)
	}
	data = append(data, '\n')
	if path == "-" || path == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create repeat report directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write repeat report %q: %w", path, err)
	}
	return nil
}
