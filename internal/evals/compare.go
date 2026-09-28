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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const ComparisonVersion = "v1"

// Comparison is a deterministic comparison of two evaluation results.
// Metrics are reported as observations; threshold policy belongs to the
// regression-checking layer that will consume this report.
type Comparison struct {
	SchemaVersion string             `json:"schemaVersion"`
	Case          ResultCase         `json:"case"`
	SkillVariant  string             `json:"skillVariant,omitempty"`
	Baseline      ResultSummary      `json:"baseline"`
	Current       ResultSummary      `json:"current"`
	Passed        bool               `json:"passed"`
	Regressions   []Difference       `json:"regressions,omitempty"`
	Improvements  []Difference       `json:"improvements,omitempty"`
	Metrics       []MetricComparison `json:"metrics,omitempty"`
	Thresholds    *ThresholdResult   `json:"thresholds,omitempty"`
}

type ResultSummary struct {
	Status     string      `json:"status"`
	DurationMS int64       `json:"durationMs"`
	Metrics    *RunMetrics `json:"metrics,omitempty"`
}

type Difference struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Baseline  any    `json:"baseline,omitempty"`
	Current   any    `json:"current,omitempty"`
	Threshold any    `json:"threshold,omitempty"`
}

type MetricComparison struct {
	Name     string   `json:"name"`
	Baseline *float64 `json:"baseline,omitempty"`
	Current  *float64 `json:"current,omitempty"`
	Delta    *float64 `json:"delta,omitempty"`
}

// Thresholds are optional percentage ceilings for increases from baseline.
// A nil value disables that check. Thresholds deliberately cover only
// deterministic metrics; qualitative rubric scores are never gated here.
type Thresholds struct {
	MaxDurationIncreasePercent *float64 `json:"maxDurationIncreasePercent,omitempty"`
	MaxCostIncreasePercent     *float64 `json:"maxCostIncreasePercent,omitempty"`
}

type ThresholdResult struct {
	Configured Thresholds `json:"configured"`
	Violations []string   `json:"violations,omitempty"`
}

func LoadResult(path string) (Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, fmt.Errorf("read eval result %q: %w", path, err)
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		return Result{}, fmt.Errorf("parse eval result %q: %w", path, err)
	}
	if result.SchemaVersion != ResultVersion {
		return Result{}, fmt.Errorf("eval result %q has schemaVersion %q, want %q", path, result.SchemaVersion, ResultVersion)
	}
	return result, nil
}

func Compare(baseline, current Result) (Comparison, error) {
	return CompareWithThresholds(baseline, current, Thresholds{})
}

func CompareWithThresholds(baseline, current Result, thresholds Thresholds) (Comparison, error) {
	if thresholds.MaxDurationIncreasePercent != nil && *thresholds.MaxDurationIncreasePercent < 0 {
		return Comparison{}, fmt.Errorf("max duration increase threshold must be non-negative")
	}
	if thresholds.MaxCostIncreasePercent != nil && *thresholds.MaxCostIncreasePercent < 0 {
		return Comparison{}, fmt.Errorf("max cost increase threshold must be non-negative")
	}
	if baseline.SchemaVersion != ResultVersion || current.SchemaVersion != ResultVersion {
		return Comparison{}, fmt.Errorf("results must use schemaVersion %q", ResultVersion)
	}
	if baseline.Case != current.Case {
		return Comparison{}, fmt.Errorf("results are for different cases: %s/%s vs %s/%s", baseline.Case.Name, baseline.Case.Version, current.Case.Name, current.Case.Version)
	}
	if baseline.SkillVariant != current.SkillVariant {
		return Comparison{}, fmt.Errorf("results use different skill variants: %q vs %q", baseline.SkillVariant, current.SkillVariant)
	}

	comparison := Comparison{
		SchemaVersion: ComparisonVersion,
		Case:          current.Case,
		SkillVariant:  current.SkillVariant,
		Baseline:      summary(baseline),
		Current:       summary(current),
		Passed:        true,
	}
	addStatusDifference(&comparison, baseline.Status, current.Status)
	compareAssertions(&comparison, baseline.Assertions, current.Assertions)
	compareArtifacts(&comparison, baseline.Artifacts, current.Artifacts)
	compareRubric(&comparison, baseline.Rubric.Dimensions, current.Rubric.Dimensions)
	compareMetrics(&comparison, baseline, current)
	applyThresholds(&comparison, thresholds)
	comparison.Passed = len(comparison.Regressions) == 0
	return comparison, nil
}

func summary(result Result) ResultSummary {
	return ResultSummary{Status: result.Status, DurationMS: result.DurationMS, Metrics: result.Metrics}
}

func addStatusDifference(comparison *Comparison, baseline, current string) {
	if baseline == current {
		return
	}
	difference := Difference{Kind: "status", ID: "status", Baseline: baseline, Current: current}
	if baseline == "passed" && current != "passed" {
		comparison.Regressions = append(comparison.Regressions, difference)
	} else if baseline != "passed" && current == "passed" {
		comparison.Improvements = append(comparison.Improvements, difference)
	}
}

func compareAssertions(comparison *Comparison, baseline, current []CheckResult) {
	base := make(map[string]CheckResult, len(baseline))
	cur := make(map[string]CheckResult, len(current))
	for _, check := range baseline {
		base[check.ID] = check
	}
	for _, check := range current {
		cur[check.ID] = check
	}
	for _, id := range sortedKeys(base, cur) {
		before, beforeOK := base[id]
		after, afterOK := cur[id]
		beforePassed := beforeOK && before.Passed
		afterPassed := afterOK && after.Passed
		if beforePassed == afterPassed {
			continue
		}
		difference := Difference{Kind: "assertion", ID: id, Baseline: beforePassed, Current: afterPassed}
		if beforePassed {
			comparison.Regressions = append(comparison.Regressions, difference)
		} else {
			comparison.Improvements = append(comparison.Improvements, difference)
		}
	}
}

func compareArtifacts(comparison *Comparison, baseline, current []ArtifactResult) {
	base := make(map[string]ArtifactResult, len(baseline))
	cur := make(map[string]ArtifactResult, len(current))
	for _, artifact := range baseline {
		base[artifact.Path] = artifact
	}
	for _, artifact := range current {
		cur[artifact.Path] = artifact
	}
	for _, path := range sortedKeys(base, cur) {
		before, beforeOK := base[path]
		after, afterOK := cur[path]
		beforePresent := beforeOK && before.Present
		afterPresent := afterOK && after.Present
		if beforePresent == afterPresent {
			continue
		}
		difference := Difference{Kind: "artifact", ID: path, Baseline: beforePresent, Current: afterPresent}
		if beforePresent {
			comparison.Regressions = append(comparison.Regressions, difference)
		} else {
			comparison.Improvements = append(comparison.Improvements, difference)
		}
	}
}

func compareRubric(comparison *Comparison, baseline, current []DimensionScore) {
	base := make(map[string]DimensionScore, len(baseline))
	cur := make(map[string]DimensionScore, len(current))
	for _, dimension := range baseline {
		base[dimension.ID] = dimension
	}
	for _, dimension := range current {
		cur[dimension.ID] = dimension
	}
	for _, id := range sortedKeys(base, cur) {
		before, beforeOK := base[id]
		after, afterOK := cur[id]
		if (!beforeOK || before.Grader != "deterministic") && (!afterOK || after.Grader != "deterministic") {
			continue
		}
		if before.Score == nil && after.Score == nil {
			continue
		}
		if before.Score == nil || after.Score == nil {
			difference := Difference{Kind: "deterministic-score", ID: id, Baseline: before.Score, Current: after.Score}
			if before.Score != nil {
				comparison.Regressions = append(comparison.Regressions, difference)
			} else {
				comparison.Improvements = append(comparison.Improvements, difference)
			}
			continue
		}
		if *before.Score == *after.Score {
			continue
		}
		difference := Difference{Kind: "deterministic-score", ID: id, Baseline: *before.Score, Current: *after.Score}
		if *after.Score < *before.Score {
			comparison.Regressions = append(comparison.Regressions, difference)
		} else {
			comparison.Improvements = append(comparison.Improvements, difference)
		}
	}
}

func compareMetrics(comparison *Comparison, baseline, current Result) {
	addMetric(comparison, "durationMs", float64(baseline.DurationMS), float64(current.DurationMS))
	if baseline.Metrics == nil || current.Metrics == nil {
		return
	}
	if baseline.Metrics.TurnsUsed != nil && current.Metrics.TurnsUsed != nil {
		addMetric(comparison, "turnsUsed", float64(*baseline.Metrics.TurnsUsed), float64(*current.Metrics.TurnsUsed))
	}
	if baseline.Metrics.Cost != nil && current.Metrics.Cost != nil {
		addMetric(comparison, "cost", *baseline.Metrics.Cost, *current.Metrics.Cost)
	}
}

func addMetric(comparison *Comparison, name string, baseline, current float64) {
	delta := current - baseline
	comparison.Metrics = append(comparison.Metrics, MetricComparison{
		Name: name, Baseline: &baseline, Current: &current, Delta: &delta,
	})
}

func applyThresholds(comparison *Comparison, thresholds Thresholds) {
	if thresholds.MaxDurationIncreasePercent == nil && thresholds.MaxCostIncreasePercent == nil {
		return
	}
	result := &ThresholdResult{Configured: thresholds}
	checkThreshold(comparison, result, "durationMs", thresholds.MaxDurationIncreasePercent)
	checkThreshold(comparison, result, "cost", thresholds.MaxCostIncreasePercent)
	comparison.Thresholds = result
}

func checkThreshold(comparison *Comparison, result *ThresholdResult, name string, maximum *float64) {
	if maximum == nil {
		return
	}
	metric := metricByName(comparison.Metrics, name)
	if metric == nil || metric.Baseline == nil {
		if name == "cost" && comparison.Baseline.Metrics != nil && comparison.Baseline.Metrics.Cost != nil && (comparison.Current.Metrics == nil || comparison.Current.Metrics.Cost == nil) {
			comparison.Regressions = append(comparison.Regressions, Difference{
				Kind: "metric-missing", ID: name, Baseline: *comparison.Baseline.Metrics.Cost,
				Threshold: *maximum,
			})
			result.Violations = append(result.Violations, name+" missing from current result")
		}
		// There is no baseline to compare against, so this threshold is not
		// applicable. A baseline that has a metric but a current result that
		// omits it is handled below as an unverifiable regression.
		return
	}
	if metric.Current == nil {
		comparison.Regressions = append(comparison.Regressions, Difference{
			Kind: "metric-missing", ID: name, Baseline: *metric.Baseline, Current: nil,
			Threshold: *maximum,
		})
		result.Violations = append(result.Violations, name+" missing from current result")
		return
	}
	increase := percentageIncrease(*metric.Baseline, *metric.Current)
	if increase <= *maximum {
		return
	}
	comparison.Regressions = append(comparison.Regressions, Difference{
		Kind: "threshold", ID: name, Baseline: *metric.Baseline, Current: *metric.Current,
		Threshold: *maximum,
	})
	result.Violations = append(result.Violations, fmt.Sprintf("%s increased by %.2f%% (limit %.2f%%)", name, increase, *maximum))
}

func metricByName(metrics []MetricComparison, name string) *MetricComparison {
	for i := range metrics {
		if metrics[i].Name == name {
			return &metrics[i]
		}
	}
	return nil
}

func percentageIncrease(baseline, current float64) float64 {
	if current <= baseline {
		return 0
	}
	if baseline == 0 {
		return 1e308
	}
	return (current - baseline) / baseline * 100
}

func sortedKeys[T any](maps ...map[string]T) []string {
	seen := map[string]struct{}{}
	for _, values := range maps {
		for key := range values {
			seen[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func WriteComparison(path string, comparison Comparison) error {
	data, err := json.MarshalIndent(comparison, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal eval comparison: %w", err)
	}
	data = append(data, '\n')
	if path == "-" || path == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create comparison directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write eval comparison %q: %w", path, err)
	}
	return nil
}
