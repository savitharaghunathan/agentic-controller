package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	gogit "github.com/go-git/go-git/v5"

	"github.com/konveyor/migration-harness/internal/acp"
	"github.com/konveyor/migration-harness/internal/config"
	"github.com/konveyor/migration-harness/internal/git"
	"github.com/konveyor/migration-harness/internal/goose"
	"github.com/konveyor/migration-harness/internal/logging"
	"github.com/konveyor/migration-harness/internal/watcher"
)

var rootCmd = &cobra.Command{
	Use:   "migration-harness",
	Short: "Thin git plumbing wrapper for goose-based migration stages",
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run a single migration stage (plan, execute, or verify)",
	RunE:  runStage,
}

func init() {
	rootCmd.AddCommand(runCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runStage(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// 1. Load config from env
	cfg := config.LoadFromEnv()
	if cfg == nil {
		return fmt.Errorf("KONVEYOR_MODEL_PRIMARY_MODEL and KONVEYOR_MODEL_PRIMARY_PROVIDER are required")
	}

	// 2. Read git creds
	creds, err := git.ReadFromEnv()
	if err != nil {
		return fmt.Errorf("git credentials: %w", err)
	}
	if creds == nil {
		return fmt.Errorf("GIT_REPO_URL is required")
	}

	// 3. Clone, strip creds, checkout branch
	logging.Header("Git Setup")
	logging.Info("cloning %s...", creds.RepoURL)

	cloneDir := os.Getenv("HARNESS_WORK_DIR")
	if cloneDir == "" {
		cloneDir = "/workspace/repo"
	}

	repo, err := git.Clone(ctx, creds, cloneDir)
	if err != nil {
		return fmt.Errorf("clone: %w", err)
	}

	if err := git.StripCredentials(repo); err != nil {
		return fmt.Errorf("strip credentials: %w", err)
	}
	git.ClearEnvCredentials()

	if err := git.CheckoutBranch(repo, creds.Branch); err != nil {
		return fmt.Errorf("checkout branch %s: %w", creds.Branch, err)
	}
	logging.Ok("cloned to %s, branch %s", cloneDir, creds.Branch)

	// 4. Start goose serve
	logging.Header("Goose Setup")
	srv, err := goose.StartServe(ctx, 0, cfg.Provider, cfg.Model, cfg.APIKey, cfg.Endpoint)
	if err != nil {
		return fmt.Errorf("start goose serve: %w", err)
	}
	defer srv.Stop()

	// 5. Connect ACP, create session
	wsClient, err := acp.WaitReadyDial(ctx, "127.0.0.1", srv.Port(), srv.SecretKey(), 30*time.Second)
	if err != nil {
		return fmt.Errorf("connect to goose: %w", err)
	}
	defer wsClient.Close()

	session := acp.NewSessionClient(wsClient)
	sessionID, err := session.CreateSession(ctx, cloneDir, nil)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	// 6. Discover skills
	skillContent, skillPaths, err := discoverSkills()
	if err != nil {
		return fmt.Errorf("discover skills: %w", err)
	}

	// 7. Build prompt from context layers
	prompt := buildPrompt(skillContent)

	// 8. Start filesystem watcher BEFORE blocking prompt
	commitPush := func() error {
		if _, err := git.CommitAll(repo, "konveyor: auto-commit progress"); err != nil {
			return err
		}
		return git.Push(ctx, creds, repo, creds.Branch)
	}
	w, err := watcher.New(cloneDir, commitPush)
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}
	if err := w.Start(ctx); err != nil {
		return fmt.Errorf("start watcher: %w", err)
	}
	defer w.Stop()

	// 9. Send single ACP prompt (blocks until goose finishes or MaxTurns is hit)
	logging.Header("Running Stage")
	logging.Info("max turns: %d", cfg.MaxTurns)
	_, err = session.SendPrompt(ctx, sessionID, []acp.ContentBlock{
		{Type: "text", Text: prompt},
	}, cfg.MaxTurns)

	if err != nil {
		logging.Err("prompt failed: %v", err)
	}

	if !srv.Alive() {
		logging.Err("goose serve crashed")
	}

	// 10. Stop watcher
	w.Stop()

	// 11. Read result.json for exit status
	result, exitCode := readResultStatus(cloneDir)

	// 11b. Write handoff.md for next stage
	if err := writeHandoff(cloneDir, skillPaths, result, repo); err != nil {
		logging.Warn("handoff: %v", err)
	}

	// 12. Final commit + push
	logging.Header("Final Push")
	if _, err := git.CommitAll(repo, "konveyor: stage complete"); err != nil {
		logging.Warn("final commit: %v", err)
	}
	if err := git.Push(ctx, creds, repo, creds.Branch); err != nil {
		logging.Warn("final push: %v", err)
	}

	// 13. Exit
	if exitCode != 0 {
		logging.Err("stage failed (result.json)")
		os.Exit(1)
	}
	logging.Ok("stage succeeded")
	return nil
}

const defaultSkillsDir = "/opt/skills"

func skillsDir() string {
	if v := os.Getenv("HARNESS_SKILLS_DIR"); v != "" {
		return v
	}
	return defaultSkillsDir
}

func discoverSkills() (string, []string, error) {
	pattern := filepath.Join(skillsDir(), "*/SKILL.md")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", nil, err
	}
	if len(matches) == 0 {
		return "", nil, fmt.Errorf("no skills found at %s", pattern)
	}

	var combined strings.Builder
	for i, m := range matches {
		content, err := os.ReadFile(m)
		if err != nil {
			return "", nil, fmt.Errorf("read skill %s: %w", m, err)
		}
		logging.Info("discovered skill: %s", m)
		if i > 0 {
			combined.WriteString("\n\n---\n\n")
		}
		combined.Write(content)
	}
	return combined.String(), matches, nil
}

func buildPrompt(skillContent string) string {
	var b strings.Builder

	if v := os.Getenv("KONVEYOR_PROMPT"); v != "" {
		b.WriteString(v)
		b.WriteString("\n\n")
	}

	if v := os.Getenv("KONVEYOR_PLAYBOOK_INSTRUCTIONS"); v != "" {
		b.WriteString("## Migration Context\n\n")
		b.WriteString(v)
		b.WriteString("\n\n")
	}

	b.WriteString("## Skill Instructions\n\n")
	b.WriteString(skillContent)
	b.WriteString("\n\n")

	if v := os.Getenv("KONVEYOR_INSTRUCTIONS"); v != "" {
		b.WriteString("## Stage Task\n\n")
		b.WriteString(v)
	}

	return b.String()
}

type stageResult struct {
	Stage  string `json:"stage"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

func readResultStatus(workDir string) (stageResult, int) {
	path := filepath.Join(workDir, ".konveyor", "result.json")
	data, err := os.ReadFile(path)
	if err != nil {
		logging.Warn("no result.json found — treating as failure")
		return stageResult{Stage: "unknown", Status: "failed", Reason: "no result.json"}, 1
	}

	var results []stageResult
	if err := json.Unmarshal(data, &results); err != nil {
		logging.Warn("invalid result.json: %v", err)
		return stageResult{Stage: "unknown", Status: "failed", Reason: "invalid result.json"}, 1
	}

	if len(results) == 0 {
		logging.Warn("result.json is empty — treating as failure")
		return stageResult{Stage: "unknown", Status: "failed", Reason: "empty result.json"}, 1
	}

	last := results[len(results)-1]
	if last.Status == "succeeded" {
		return last, 0
	}

	logging.Err("stage %s failed: %s", last.Stage, last.Reason)
	return last, 1
}

func writeHandoff(workDir string, skills []string, result stageResult, repo *gogit.Repository) error {
	handoffPath := filepath.Join(workDir, ".konveyor", "handoff.md")
	if err := os.MkdirAll(filepath.Dir(handoffPath), 0o755); err != nil {
		return fmt.Errorf("create .konveyor dir: %w", err)
	}

	// Read existing handoff content from prior stages
	existing, _ := os.ReadFile(handoffPath)

	var b strings.Builder

	if len(existing) > 0 {
		b.Write(existing)
		b.WriteString("\n---\n\n")
	}

	fmt.Fprintf(&b, "## Stage: %s\n\n", result.Stage)
	fmt.Fprintf(&b, "**Status:** %s\n", result.Status)
	fmt.Fprintf(&b, "**Completed:** %s\n", time.Now().UTC().Format(time.RFC3339))
	if result.Reason != "" {
		fmt.Fprintf(&b, "**Reason:** %s\n", result.Reason)
	}

	b.WriteString("\n### Skills Loaded\n\n")
	for _, s := range skills {
		fmt.Fprintf(&b, "- %s\n", s)
	}

	if changed := changedFiles(repo); len(changed) > 0 {
		b.WriteString("\n### Files Changed\n\n")
		for _, f := range changed {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}

	if v := os.Getenv("KONVEYOR_INSTRUCTIONS"); v != "" {
		b.WriteString("\n### Instructions\n\n")
		b.WriteString(v)
		b.WriteString("\n")
	}

	if err := os.WriteFile(handoffPath, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write handoff.md: %w", err)
	}

	logging.Ok("wrote %s", handoffPath)
	return nil
}

func changedFiles(repo *gogit.Repository) []string {
	wt, err := repo.Worktree()
	if err != nil {
		return nil
	}
	status, err := wt.Status()
	if err != nil {
		return nil
	}
	var files []string
	for path := range status {
		files = append(files, path)
	}
	return files
}
