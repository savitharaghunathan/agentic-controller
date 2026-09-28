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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resultForComparison() Result {
	turns := 10
	cost := 2.5
	score := 0.8
	return Result{
		SchemaVersion: ResultVersion,
		Case:          ResultCase{Name: "migration", Version: "1"},
		Status:        "passed",
		DurationMS:    1000,
		Assertions:    []CheckResult{{ID: "build", Passed: true, Required: true}},
		Artifacts:     []ArtifactResult{{Path: "REPORT.md", Present: true, Required: true}},
		Rubric:        RubricResult{Dimensions: []DimensionScore{{ID: "correctness", Grader: "deterministic", Score: &score}}},
		Metrics:       &RunMetrics{TurnsUsed: &turns, Cost: &cost},
	}
}

func TestCompareReportsRegressionsAndMetrics(t *testing.T) {
	baseline := resultForComparison()
	current := resultForComparison()
	current.Status = "failed"
	current.Assertions[0].Passed = false
	current.Artifacts[0].Present = false
	current.DurationMS = 1500
	*current.Metrics.TurnsUsed = 12
	*current.Metrics.Cost = 3.25
	*current.Rubric.Dimensions[0].Score = 0.5

	comparison, err := Compare(baseline, current)
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Passed || len(comparison.Regressions) != 4 {
		t.Fatalf("unexpected comparison: %+v", comparison)
	}
	if len(comparison.Metrics) != 3 {
		t.Fatalf("metrics = %+v, want duration, turns, and cost", comparison.Metrics)
	}
	if comparison.Metrics[2].Name != "cost" || *comparison.Metrics[2].Delta != 0.75 {
		t.Fatalf("cost metric = %+v", comparison.Metrics[2])
	}
}

func TestCompareIgnoresQualitativeScores(t *testing.T) {
	baseline := resultForComparison()
	current := resultForComparison()
	qualitative := 0.2
	baseline.Rubric.Dimensions = append(baseline.Rubric.Dimensions, DimensionScore{ID: "quality", Grader: "human", Score: &qualitative})
	qualitative = 0.9
	current.Rubric.Dimensions = append(current.Rubric.Dimensions, DimensionScore{ID: "quality", Grader: "human", Score: &qualitative})

	comparison, err := Compare(baseline, current)
	if err != nil {
		t.Fatal(err)
	}
	if !comparison.Passed || len(comparison.Regressions) != 0 {
		t.Fatalf("qualitative score changed comparison: %+v", comparison)
	}
}

func TestCompareWithThresholdsDetectsDurationAndCostRegression(t *testing.T) {
	baseline := resultForComparison()
	current := resultForComparison()
	current.DurationMS = 1200
	*current.Metrics.Cost = 3.0
	durationLimit := 10.0
	costLimit := 15.0

	comparison, err := CompareWithThresholds(baseline, current, Thresholds{
		MaxDurationIncreasePercent: &durationLimit,
		MaxCostIncreasePercent:     &costLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Passed || comparison.Thresholds == nil || len(comparison.Thresholds.Violations) != 2 {
		t.Fatalf("unexpected threshold result: %+v", comparison)
	}
	if len(comparison.Regressions) != 2 {
		t.Fatalf("regressions = %+v, want duration and cost thresholds", comparison.Regressions)
	}
}

func TestCompareWithThresholdsAllowsChangesWithinLimits(t *testing.T) {
	baseline := resultForComparison()
	current := resultForComparison()
	current.DurationMS = 1050
	*current.Metrics.Cost = 2.6
	durationLimit := 10.0
	costLimit := 10.0

	comparison, err := CompareWithThresholds(baseline, current, Thresholds{
		MaxDurationIncreasePercent: &durationLimit,
		MaxCostIncreasePercent:     &costLimit,
	})
	if err != nil || !comparison.Passed {
		t.Fatalf("within-limit comparison = %+v, err=%v", comparison, err)
	}
}

func TestCompareWithThresholdsReportsMissingCurrentCost(t *testing.T) {
	baseline := resultForComparison()
	current := resultForComparison()
	current.Metrics = nil
	costLimit := 10.0

	comparison, err := CompareWithThresholds(baseline, current, Thresholds{MaxCostIncreasePercent: &costLimit})
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Passed || len(comparison.Thresholds.Violations) != 1 || comparison.Regressions[0].Kind != "metric-missing" {
		t.Fatalf("unexpected missing metric result: %+v", comparison)
	}
}

func TestLoadMetricsAcceptsHarnessTerminationData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "termination.json")
	contents := `{"outcome":"limitReached","limitReached":"maxCost","usage":{"turnsUsed":12,"cost":3.25}}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	metrics, err := LoadMetrics(path)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Outcome != "limitReached" || metrics.LimitReached != "maxCost" || metrics.TurnsUsed == nil || *metrics.TurnsUsed != 12 || metrics.Cost == nil || *metrics.Cost != 3.25 {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}
}

func TestLoadResultAndWriteComparison(t *testing.T) {
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result.json")
	if err := WriteResult(resultPath, resultForComparison()); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadResult(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	comparison, err := Compare(loaded, loaded)
	if err != nil || !comparison.Passed {
		t.Fatalf("self comparison = %+v, err=%v", comparison, err)
	}
	comparisonPath := filepath.Join(dir, "comparison.json")
	if err := WriteComparison(comparisonPath, comparison); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(comparisonPath)
	if err != nil || !strings.Contains(string(data), `"schemaVersion": "v1"`) {
		t.Fatalf("comparison output = %s, err=%v", data, err)
	}
	var decoded Comparison
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("comparison is not JSON: %v", err)
	}
}
