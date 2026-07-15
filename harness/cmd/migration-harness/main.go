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

	// 6. Discover skill
	skillContent, err := discoverSkill()
	if err != nil {
		return fmt.Errorf("discover skill: %w", err)
	}

	// 7. Build prompt from 4 context layers
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
	exitCode := readResultStatus(cloneDir)

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

func discoverSkill() (string, error) {
	matches, err := filepath.Glob("/opt/skills/*/SKILL.md")
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no skill found at /opt/skills/*/SKILL.md")
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("expected exactly one skill, found %d: %v", len(matches), matches)
	}
	content, err := os.ReadFile(matches[0])
	if err != nil {
		return "", fmt.Errorf("read skill %s: %w", matches[0], err)
	}
	logging.Info("discovered skill: %s", matches[0])
	return string(content), nil
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

func readResultStatus(workDir string) int {
	path := filepath.Join(workDir, ".konveyor", "result.json")
	data, err := os.ReadFile(path)
	if err != nil {
		logging.Warn("no result.json found — treating as failure")
		return 1
	}

	var results []stageResult
	if err := json.Unmarshal(data, &results); err != nil {
		logging.Warn("invalid result.json: %v", err)
		return 1
	}

	if len(results) == 0 {
		logging.Warn("result.json is empty — treating as failure")
		return 1
	}

	last := results[len(results)-1]
	if last.Status == "succeeded" {
		return 0
	}

	logging.Err("stage %s failed: %s", last.Stage, last.Reason)
	return 1
}
