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
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

type ScopeResult struct {
	Passed     bool     `json:"passed"`
	Allowed    []string `json:"allowed,omitempty"`
	Changed    []string `json:"changed,omitempty"`
	Unexpected []string `json:"unexpected,omitempty"`
	Error      string   `json:"error,omitempty"`
}

type fileSnapshot map[string]string

func snapshotRepository(workDir string) (fileSnapshot, error) {
	rootOutput, err := gitOutput(workDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("find repository root: %w", err)
	}
	root := strings.TrimSpace(string(rootOutput))
	if root == "" {
		return nil, fmt.Errorf("git returned an empty repository root")
	}

	tracked, err := gitOutput(root, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("list tracked files: %w", err)
	}
	untracked, err := gitOutput(root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("list untracked files: %w", err)
	}

	paths := map[string]struct{}{}
	for _, data := range [][]byte{tracked, untracked} {
		for _, name := range strings.Split(string(data), "\x00") {
			if name != "" {
				paths[name] = struct{}{}
			}
		}
	}

	snapshot := fileSnapshot{}
	for name := range paths {
		fingerprint, exists, err := fingerprint(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return nil, fmt.Errorf("fingerprint %q: %w", name, err)
		}
		if exists {
			snapshot[name] = fingerprint
		}
	}
	return snapshot, nil
}

func gitOutput(workDir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = workDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func fingerprint(name string) (string, bool, error) {
	info, err := os.Lstat(name)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(name)
		if err != nil {
			return "", false, err
		}
		return "symlink:" + target, true, nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Sprintf("mode:%s", info.Mode()), true, nil
	}
	file, err := os.Open(name)
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", false, err
	}
	return fmt.Sprintf("mode:%s:%x", info.Mode(), hash.Sum(nil)), true, nil
}

func evaluateScope(before, after fileSnapshot, allowed []string) ScopeResult {
	result := ScopeResult{Allowed: append([]string(nil), allowed...), Passed: true}
	paths := map[string]struct{}{}
	for name := range before {
		paths[name] = struct{}{}
	}
	for name := range after {
		paths[name] = struct{}{}
	}
	changed := make([]string, 0)
	for name := range paths {
		if before[name] != after[name] {
			changed = append(changed, name)
		}
	}
	sort.Strings(changed)
	result.Changed = changed
	for _, name := range changed {
		if !scopeAllows(name, allowed) {
			result.Unexpected = append(result.Unexpected, name)
		}
	}
	result.Passed = len(result.Unexpected) == 0
	return result
}

func scopeAllows(name string, allowed []string) bool {
	for _, pattern := range allowed {
		pattern = strings.TrimSpace(filepath.ToSlash(pattern))
		if strings.HasSuffix(pattern, "/") {
			if strings.HasPrefix(name, pattern) {
				return true
			}
			continue
		}
		if strings.ContainsAny(pattern, "*[?[") {
			matched, err := path.Match(pattern, name)
			if err == nil && matched {
				return true
			}
			continue
		}
		if name == pattern || strings.HasPrefix(name, pattern+"/") {
			return true
		}
	}
	return false
}
