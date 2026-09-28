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

// Package evals contains the versioned, diagnostic evaluation contract.
//
// Evaluation cases describe a workflow scenario and its evidence. They do not
// contain controller logic and they never replace deterministic unit, envtest,
// or Kubernetes E2E tests.
package evals

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	CaseAPIVersion = "evals.konveyor.io/v1alpha1"
	CaseKind       = "WorkflowEvalCase"
	ResultVersion  = "v1"
)

// Case is a reproducible workflow evaluation scenario.
type Case struct {
	APIVersion  string       `yaml:"apiVersion" json:"apiVersion"`
	Kind        string       `yaml:"kind" json:"kind"`
	Metadata    Metadata     `yaml:"metadata" json:"metadata"`
	Description string       `yaml:"description" json:"description"`
	Workflow    WorkflowSpec `yaml:"workflow" json:"workflow"`
	Skills      SkillSpec    `yaml:"skills" json:"skills"`
	Scope       *ScopeSpec   `yaml:"scope,omitempty" json:"scope,omitempty"`
	Artifacts   []Artifact   `yaml:"artifacts" json:"artifacts"`
	Assertions  []Assertion  `yaml:"assertions" json:"assertions"`
	Rubric      Rubric       `yaml:"rubric" json:"rubric"`
}

type Metadata struct {
	Name    string `yaml:"name" json:"name"`
	Version string `yaml:"version" json:"version"`
}

type WorkflowSpec struct {
	// Command is intentionally a shell command: workflow setup commonly needs
	// kubectl, kind, or a repository-specific script. Case files are trusted
	// repository inputs, just like existing hack/*.sh test drivers.
	Command  string `yaml:"command" json:"command"`
	Teardown string `yaml:"teardown,omitempty" json:"teardown,omitempty"`
	Timeout  string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

type SkillSpec struct {
	Required []string       `yaml:"required,omitempty" json:"required,omitempty"`
	Variants []SkillVariant `yaml:"variants,omitempty" json:"variants,omitempty"`
}

type SkillVariant struct {
	Name   string   `yaml:"name" json:"name"`
	Remove []string `yaml:"remove,omitempty" json:"remove,omitempty"`
	Add    []string `yaml:"add,omitempty" json:"add,omitempty"`
}

// ScopeSpec limits the files a workflow may change. Allowed entries are
// exact paths, directory prefixes, or path.Match-style globs relative to the
// repository root.
type ScopeSpec struct {
	Allowed []string `yaml:"allowed,omitempty" json:"allowed,omitempty"`
}

type Artifact struct {
	Path        string `yaml:"path" json:"path"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Required    bool   `yaml:"required,omitempty" json:"required,omitempty"`
}

type Assertion struct {
	ID          string `yaml:"id" json:"id"`
	Description string `yaml:"description" json:"description"`
	Command     string `yaml:"command" json:"command"`
	Required    bool   `yaml:"required,omitempty" json:"required,omitempty"`
	Timeout     string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Interval    string `yaml:"interval,omitempty" json:"interval,omitempty"`
}

type Rubric struct {
	Version    string      `yaml:"version" json:"version"`
	Dimensions []Dimension `yaml:"dimensions" json:"dimensions"`
}

type Dimension struct {
	ID          string  `yaml:"id" json:"id"`
	Description string  `yaml:"description" json:"description"`
	Weight      float64 `yaml:"weight" json:"weight"`
	Grader      string  `yaml:"grader" json:"grader"`
	Evidence    string  `yaml:"evidence,omitempty" json:"evidence,omitempty"`
}

func LoadCase(path string) (Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Case{}, fmt.Errorf("read eval case %q: %w", path, err)
	}

	var c Case
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil {
		return Case{}, fmt.Errorf("parse eval case %q: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return Case{}, fmt.Errorf("invalid eval case %q: %w", path, err)
	}
	return c, nil
}

func (c Case) Validate() error {
	var problems []string
	if c.APIVersion != CaseAPIVersion {
		problems = append(problems, fmt.Sprintf("apiVersion must be %q", CaseAPIVersion))
	}
	if c.Kind != CaseKind {
		problems = append(problems, fmt.Sprintf("kind must be %q", CaseKind))
	}
	if strings.TrimSpace(c.Metadata.Name) == "" {
		problems = append(problems, "metadata.name is required")
	}
	if strings.TrimSpace(c.Metadata.Version) == "" {
		problems = append(problems, "metadata.version is required")
	}
	if strings.TrimSpace(c.Workflow.Command) == "" {
		problems = append(problems, "workflow.command is required")
	}
	if c.Scope != nil {
		if len(c.Scope.Allowed) == 0 {
			problems = append(problems, "scope.allowed must not be empty")
		}
		for i, allowed := range c.Scope.Allowed {
			allowed = strings.TrimSpace(allowed)
			if allowed == "" {
				problems = append(problems, fmt.Sprintf("scope.allowed[%d] must not be empty", i))
				continue
			}
			if strings.HasPrefix(allowed, "/") || strings.HasPrefix(allowed, "../") || allowed == ".." {
				problems = append(problems, fmt.Sprintf("scope.allowed[%d] must be repository-relative", i))
			}
		}
	}

	seen := map[string]bool{}
	for i, assertion := range c.Assertions {
		if strings.TrimSpace(assertion.ID) == "" {
			problems = append(problems, fmt.Sprintf("assertions[%d].id is required", i))
		} else if seen[assertion.ID] {
			problems = append(problems, fmt.Sprintf("assertion id %q is duplicated", assertion.ID))
		}
		seen[assertion.ID] = true
		if strings.TrimSpace(assertion.Command) == "" {
			problems = append(problems, fmt.Sprintf("assertions[%d].command is required", i))
		}
		if assertion.Timeout != "" {
			if duration, err := time.ParseDuration(assertion.Timeout); err != nil || duration <= 0 {
				if err == nil {
					err = fmt.Errorf("must be greater than zero")
				}
				problems = append(problems, fmt.Sprintf("assertion %q has invalid timeout: %v", assertion.ID, err))
			}
		}
		if assertion.Interval != "" {
			if duration, err := time.ParseDuration(assertion.Interval); err != nil || duration <= 0 {
				if err == nil {
					err = fmt.Errorf("must be greater than zero")
				}
				problems = append(problems, fmt.Sprintf("assertion %q has invalid interval: %v", assertion.ID, err))
			}
		}
	}

	seen = map[string]bool{}
	for i, artifact := range c.Artifacts {
		if strings.TrimSpace(artifact.Path) == "" {
			problems = append(problems, fmt.Sprintf("artifacts[%d].path is required", i))
		}
	}
	for i, variant := range c.Skills.Variants {
		if strings.TrimSpace(variant.Name) == "" {
			problems = append(problems, fmt.Sprintf("skills.variants[%d].name is required", i))
		} else if seen[variant.Name] {
			problems = append(problems, fmt.Sprintf("skill variant %q is duplicated", variant.Name))
		}
		seen[variant.Name] = true
	}

	if c.Rubric.Version == "" {
		problems = append(problems, "rubric.version is required")
	}
	if c.Workflow.Timeout != "" {
		if duration, err := time.ParseDuration(c.Workflow.Timeout); err != nil || duration <= 0 {
			if err == nil {
				err = fmt.Errorf("must be greater than zero")
			}
			problems = append(problems, fmt.Sprintf("workflow.timeout is invalid: %v", err))
		}
	}
	if len(c.Rubric.Dimensions) == 0 {
		problems = append(problems, "rubric.dimensions must not be empty")
	}
	seen = map[string]bool{}
	var total float64
	for i, dimension := range c.Rubric.Dimensions {
		if strings.TrimSpace(dimension.ID) == "" {
			problems = append(problems, fmt.Sprintf("rubric.dimensions[%d].id is required", i))
		} else if seen[dimension.ID] {
			problems = append(problems, fmt.Sprintf("rubric dimension %q is duplicated", dimension.ID))
		}
		seen[dimension.ID] = true
		if dimension.Weight <= 0 || math.IsNaN(dimension.Weight) || math.IsInf(dimension.Weight, 0) {
			problems = append(problems, fmt.Sprintf("rubric dimension %q must have a positive weight", dimension.ID))
		}
		if dimension.Grader != "deterministic" && dimension.Grader != "human" && dimension.Grader != "llm" {
			problems = append(problems, fmt.Sprintf("rubric dimension %q grader must be deterministic, human, or llm", dimension.ID))
		}
		total += dimension.Weight
	}
	if len(c.Rubric.Dimensions) > 0 && math.Abs(total-1) > 0.000001 {
		problems = append(problems, fmt.Sprintf("rubric weights must sum to 1.0 (got %.6f)", total))
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}
