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

package controller

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"

	konveyoriov1alpha1 "github.com/konveyor/agentic-controller/api/v1alpha1"
)

// ParamsFilePath is the in-Sandbox path where the controller-written
// parameter file is mounted (ADR 0009). Part of the harness contract.
// It is the single source of truth for the mount: createParamsConfigMap
// derives the mount directory and the ConfigMap key from it, so the
// harness contract cannot drift from what the controller mounts.
const ParamsFilePath = "/run/konveyor/params.json"

// Substitution scope names, used as $(scope.name) prefixes and as the
// params.json section keys.
const (
	scopeAgent    = "agent"
	scopeWorkflow = "workflow"
)

// substitutionRef matches $(agent.<name>) / $(workflow.<name>)
// references. Only these two scopes are recognized; any other $(...)
// text is left untouched (it is not our syntax).
var substitutionRef = regexp.MustCompile(`\$\((agent|workflow)\.([a-zA-Z_][a-zA-Z0-9_]*)\)`)

// coerceParams resolves declared params against supplied values and
// returns a map of name -> type-coerced value (JSON number, bool, or
// string) suitable for a params.json section, plus a name -> string
// map for use in substitution. A value that cannot be coerced to its
// declared type is a reconcile error (ADR 0009/0018) — the caller must
// not create a Sandbox with a malformed params file.
//
// Only params with an effective value (supplied or defaulted) are
// included; a param left empty is omitted from both maps.
func coerceParams(decls []konveyoriov1alpha1.Param, supplied map[string]string) (map[string]any, map[string]string, error) {
	coerced := make(map[string]any, len(decls))
	strs := make(map[string]string, len(decls))

	for _, p := range decls {
		value, ok := supplied[p.Name]
		if !ok {
			value = p.Default
		}
		if value == "" {
			// Declared but unset (optional, no default): resolve
			// $(scope.name) to empty rather than an unresolved-reference
			// error, and omit it from params.json. Only undeclared
			// references (never added to strs) fail. Required params are
			// already caught in validateParams.
			strs[p.Name] = ""
			continue
		}

		var cv any
		switch p.Type {
		case konveyoriov1alpha1.ParamTypeNumber:
			// Prefer integer, fall back to float, so "25" serializes as
			// 25 rather than 25.0.
			if i, err := strconv.ParseInt(value, 10, 64); err == nil {
				cv = i
			} else if f, err := strconv.ParseFloat(value, 64); err == nil {
				cv = f
			} else {
				return nil, nil, fmt.Errorf("parameter %q declared number but value %q is not numeric", p.Name, value)
			}
		case konveyoriov1alpha1.ParamTypeBoolean:
			b, err := strconv.ParseBool(value)
			if err != nil {
				return nil, nil, fmt.Errorf("parameter %q declared boolean but value %q is not a boolean", p.Name, value)
			}
			cv = b
		default: // string (also the CRD default)
			cv = value
		}
		coerced[p.Name] = cv
		// Substitution renders the canonical coerced value via the single
		// stringifyJSON rule — the same one the workflow scope uses — so a
		// value renders identically regardless of which scope references
		// it (e.g. a boolean is always "true", never the raw "T"/"yes"/"1").
		strs[p.Name] = stringifyJSON(cv)
	}
	return coerced, strs, nil
}

// suppliedValues turns a []ParamValue into a name -> value map.
func suppliedValues(values []konveyoriov1alpha1.ParamValue) map[string]string {
	m := make(map[string]string, len(values))
	for _, v := range values {
		m[v.Name] = v.Value
	}
	return m
}

// substitute replaces $(agent.<name>) / $(workflow.<name>) references in
// text using the given per-scope string maps. A reference to a name the
// scope does not provide is a reconcile error (ADR 0009) — an
// undeclared reference must not reach a Sandbox as a literal. Text that
// is not a recognized reference passes through unchanged. Returns the
// rendered text, or an aggregated error naming every unresolved
// reference.
func substitute(text string, scopes map[string]map[string]string) (string, error) {
	if text == "" || !strings.Contains(text, "$(") {
		return text, nil
	}

	var unresolved []string
	rendered := substitutionRef.ReplaceAllStringFunc(text, func(match string) string {
		parts := substitutionRef.FindStringSubmatch(match)
		scope, name := parts[1], parts[2]
		if vals, ok := scopes[scope]; ok {
			if v, ok := vals[name]; ok {
				return v
			}
		}
		unresolved = append(unresolved, match)
		return match
	})

	if len(unresolved) > 0 {
		slices.Sort(unresolved)
		unresolved = slices.Compact(unresolved)
		return "", fmt.Errorf("unresolved parameter references: %s", strings.Join(unresolved, ", "))
	}
	return rendered, nil
}

// paramsFile is the three-section structure written to params.json.
type paramsFile struct {
	Workflow  map[string]any `json:"workflow,omitempty"`
	Agent     map[string]any `json:"agent,omitempty"`
	Execution map[string]any `json:"execution,omitempty"`
}

// buildParams resolves an AgentRun's parameters into the params.json
// structure and the per-scope string maps used for $(scope.name)
// substitution. The agent scope is coerced here from the Agent's
// declarations and the run's supplied values. The workflow scope is
// read verbatim from the run's stamped WorkflowParams (already coerced
// by the AgentWorkflowRun controller, which owns the workflow
// declarations — ADR 0018). A coercion failure is returned as an error
// so the caller aborts before creating a Sandbox.
func buildParams(run *konveyoriov1alpha1.AgentRun, agent *konveyoriov1alpha1.Agent) (paramsFile, map[string]map[string]string, error) {
	agentCoerced, agentStrs, err := coerceParams(agent.Spec.Params, suppliedValues(run.Spec.Params))
	if err != nil {
		return paramsFile{}, nil, fmt.Errorf("agent params: %w", err)
	}

	var workflowCoerced map[string]any
	workflowStrs := map[string]string{}
	if run.Spec.WorkflowParams != nil && len(run.Spec.WorkflowParams.Raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(run.Spec.WorkflowParams.Raw)))
		dec.UseNumber() // keep 25 as "25", not "25" via float64 -> "25.000000"
		if err := dec.Decode(&workflowCoerced); err != nil {
			return paramsFile{}, nil, fmt.Errorf("workflow params: %w", err)
		}
		for k, v := range workflowCoerced {
			workflowStrs[k] = stringifyJSON(v)
		}
	}

	exec := resolveExecution(run.Spec.Execution, agent.Spec.Execution)
	f := paramsFile{
		Workflow:  workflowCoerced,
		Agent:     agentCoerced,
		Execution: executionSection(exec),
	}
	scopes := map[string]map[string]string{
		scopeAgent:    agentStrs,
		scopeWorkflow: workflowStrs,
	}
	return f, scopes, nil
}

// coerceWorkflowParams coerces the workflow's declared params against
// the supplied values and marshals them into a RawExtension for
// stamping onto a stage AgentRun's WorkflowParams (ADR 0018). Returns
// nil when no workflow param has an effective value. Coercion is done
// here, in the controller that owns the AgentWorkflow declarations, so
// the AgentRun controller can treat the result as opaque coerced JSON.
func coerceWorkflowParams(decls []konveyoriov1alpha1.Param, supplied map[string]string) (*runtime.RawExtension, map[string]string, error) {
	coerced, strs, err := coerceParams(decls, supplied)
	if err != nil {
		return nil, nil, err
	}
	if len(coerced) == 0 {
		return nil, strs, nil
	}
	raw, err := json.Marshal(coerced)
	if err != nil {
		return nil, nil, err
	}
	return &runtime.RawExtension{Raw: raw}, strs, nil
}

// stringifyJSON is the single rule for rendering a coerced parameter
// value as the string used in $(scope.name) substitution, shared by both
// the agent and workflow scopes so a value renders identically whichever
// scope references it. It handles the types the agent scope produces
// (int64, float64, bool, string) and the types the workflow scope
// produces via UseNumber decoding (json.Number, bool, string).
func stringifyJSON(v any) string {
	switch t := v.(type) {
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// executionSection renders the resolved ExecutionSpec into the map
// written to the execution section of params.json. Mode is always
// present (defaulted to auto); askUser and limits appear only when set.
func executionSection(exec *konveyoriov1alpha1.ExecutionSpec) map[string]any {
	section := map[string]any{
		"mode": string(effectiveMode(exec)),
	}
	if exec != nil {
		if exec.AskUser {
			section["askUser"] = true
		}
		if exec.MaxTurns != nil {
			section["maxTurns"] = *exec.MaxTurns
		}
		if exec.MaxCost != "" {
			section["maxCost"] = exec.MaxCost
		}
	}
	return section
}

// renderParamsFile marshals a paramsFile to indented JSON.
func renderParamsFile(f paramsFile) ([]byte, error) {
	return json.MarshalIndent(f, "", "  ")
}
