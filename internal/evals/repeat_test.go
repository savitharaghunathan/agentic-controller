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
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestRunRepeatedReportsFlakinessAndMetricVariance(t *testing.T) {
	c := validCase()
	c.Workflow.Command = `
set -eu
cost=1.0
turns=2
if [ "$EVAL_RUN_INDEX" = "2" ]; then
  cost=2.0
  turns=4
fi
printf '{"outcome":"succeeded","usage":{"turnsUsed":%s,"cost":%s}}\n' "$turns" "$cost" > metrics.json
if [ "$EVAL_RUN_INDEX" = "2" ]; then
  exit 1
fi
`
	resultDir := filepath.Join(t.TempDir(), "results")
	report, err := RunRepeated(context.Background(), c, RepeatOptions{
		Runs:                3,
		WorkDirRoot:         filepath.Join(t.TempDir(), "work"),
		ResultDir:           resultDir,
		MetricsFileTemplate: "metrics.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "flaky" || report.Passed || report.FailedRuns != 1 || report.PassedRuns != 2 {
		t.Fatalf("unexpected repeat status: %+v", report)
	}
	if math.Abs(report.FailureRatePercent-33.333333333333336) > 0.000001 {
		t.Fatalf("failure rate = %v", report.FailureRatePercent)
	}
	if report.Metrics.Cost == nil || report.Metrics.Cost.Count != 3 || report.Metrics.Cost.Max != 2 {
		t.Fatalf("unexpected cost stats: %+v", report.Metrics.Cost)
	}
	if report.Metrics.Turns == nil || report.Metrics.Turns.Count != 3 || report.Metrics.Turns.Max != 4 {
		t.Fatalf("unexpected turn stats: %+v", report.Metrics.Turns)
	}
	if len(report.Metrics.Assertions) != 1 || report.Metrics.Assertions[0].Runs != 3 || report.Metrics.Assertions[0].Failed != 0 {
		t.Fatalf("unexpected assertion stats: %+v", report.Metrics.Assertions)
	}
	for run := 1; run <= 3; run++ {
		path := filepath.Join(resultDir, fmt.Sprintf("run-%03d.json", run))
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("run result %d missing at %s: %v", run, path, err)
		}
	}
}

func TestRunRepeatedAllowsConfiguredFailureRate(t *testing.T) {
	c := validCase()
	c.Workflow.Command = `test "$EVAL_RUN_INDEX" != "2"`
	c.Assertions[0].Command = `test "$EVAL_RUN_INDEX" != "2"`
	limit := 50.0
	report, err := RunRepeated(context.Background(), c, RepeatOptions{
		Runs:                  2,
		WorkDirRoot:           filepath.Join(t.TempDir(), "work"),
		MaxFailureRatePercent: limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "flaky" || !report.Passed {
		t.Fatalf("configured failure rate was not honored: %+v", report)
	}
}
