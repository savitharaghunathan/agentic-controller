package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/konveyor/migration-harness/internal/logging"
)

// Process manages a claude-agent-acp subprocess speaking ACP over stdio.
type Process struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	done     chan struct{}
	tempDirs []string
}

// ACPConfig configures StartACP. Provider, Model, APIKey and Endpoint are
// translated to the env vars Claude Code expects — mirrors
// goose.ServeConfig's shape for the fields the two runtimes share.
type ACPConfig struct {
	Provider string
	Model    string
	APIKey   string
	Endpoint string
}

// StartACP launches claude-agent-acp, wired to stdio for ACP JSON-RPC.
func StartACP(ctx context.Context, cfg ACPConfig) (*Process, error) {
	binPath, err := exec.LookPath("claude-agent-acp")
	if err != nil {
		return nil, fmt.Errorf("claude-agent-acp not found: %w", err)
	}

	cmd := exec.CommandContext(ctx, binPath)
	env, tempDirs := providerEnv(cfg.Provider, cfg.Model, cfg.APIKey, cfg.Endpoint)
	cmd.Env = env
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		for _, d := range tempDirs {
			os.RemoveAll(d)
		}
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		for _, d := range tempDirs {
			os.RemoveAll(d)
		}
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		for _, d := range tempDirs {
			os.RemoveAll(d)
		}
		return nil, fmt.Errorf("start claude-agent-acp: %w", err)
	}

	p := &Process{
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		done:     make(chan struct{}),
		tempDirs: tempDirs,
	}

	go func() {
		cmd.Wait()
		close(p.done)
	}()

	logging.Info("claude-agent-acp started (pid %d)", cmd.Process.Pid)
	return p, nil
}

// Stdin returns the subprocess's stdin, for writing ACP requests.
func (p *Process) Stdin() io.WriteCloser { return p.stdin }

// Stdout returns the subprocess's stdout, for reading ACP responses.
func (p *Process) Stdout() io.ReadCloser { return p.stdout }

// Alive returns true if the subprocess is still running.
func (p *Process) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Stop sends SIGTERM and waits up to 5 seconds, then SIGKILL. Cleans up
// any temporary credential files created during startup.
func (p *Process) Stop() error {
	defer p.cleanup()

	if !p.Alive() {
		return nil
	}

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("sigterm: %w", err)
	}

	select {
	case <-p.done:
		logging.Ok("claude-agent-acp stopped cleanly")
		return nil
	case <-time.After(5 * time.Second):
		logging.Warn("claude-agent-acp did not stop in 5s, sending SIGKILL")
		if err := p.cmd.Process.Kill(); err != nil {
			return fmt.Errorf("sigkill: %w", err)
		}
		<-p.done
		return nil
	}
}

func (p *Process) cleanup() {
	for _, d := range p.tempDirs {
		os.RemoveAll(d)
	}
	p.tempDirs = nil
}

// providerEnv returns the current process environment with LLM provider
// credentials translated to the env vars claude-agent-acp (via the Claude
// Agent SDK) expects. Only anthropic and gcp_vertex_ai are mapped — the
// only two providers this repo currently configures; anything else is
// left unmapped with a warning, same fallback goose.providerEnv uses.
func providerEnv(provider, model, apiKey, endpoint string) (env []string, tempDirs []string) {
	env = os.Environ()
	p := strings.ReplaceAll(strings.ToLower(provider), "-", "_")

	switch p {
	case "anthropic":
		if apiKey != "" {
			env = append(env, "ANTHROPIC_API_KEY="+apiKey)
		}
		if endpoint != "" {
			env = append(env, "ANTHROPIC_BASE_URL="+endpoint)
		}
		if model != "" {
			env = append(env, "ANTHROPIC_MODEL="+model)
		}

	case "gcp_vertex_ai":
		env = append(env, "CLAUDE_CODE_USE_VERTEX=1")
		if region := vertexRegion(endpoint); region != "" {
			env = append(env, "CLOUD_ML_REGION="+region)
		}
		if content := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS_JSON"); content != "" {
			if projectID := vertexProjectID(content); projectID != "" {
				env = append(env, "ANTHROPIC_VERTEX_PROJECT_ID="+projectID)
			}
			path, err := writeADCFile(content)
			if err != nil {
				logging.Warn("write ADC file: %v", err)
			} else {
				env = append(env, "GOOGLE_APPLICATION_CREDENTIALS="+path)
				tempDirs = append(tempDirs, filepath.Dir(path))
			}
			env = filterEnvKey(env, "GOOGLE_APPLICATION_CREDENTIALS_JSON")
		}
		if model != "" {
			env = append(env, "ANTHROPIC_MODEL="+model)
		}

	default:
		if p != "" {
			logging.Warn("unmapped provider %q — credentials not forwarded to claude-agent-acp", p)
		}
	}

	return env, tempDirs
}

// vertexRegion extracts the Vertex AI region from an aiplatform endpoint
// host, e.g. "global-aiplatform.googleapis.com" -> "global",
// "us-east5-aiplatform.googleapis.com" -> "us-east5". Returns "" if the
// host doesn't match the expected "<region>-aiplatform.googleapis.com"
// shape.
func vertexRegion(endpoint string) string {
	host := endpoint
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	const suffix = "-aiplatform.googleapis.com"
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	return strings.TrimSuffix(host, suffix)
}

// vertexProjectID extracts the "project_id" field from a GCP service
// account JSON blob, so ANTHROPIC_VERTEX_PROJECT_ID doesn't need a new
// config field — the value already arrives in
// GOOGLE_APPLICATION_CREDENTIALS_JSON for the gcp_vertex_ai provider.
func vertexProjectID(credentialJSON string) string {
	var parsed struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal([]byte(credentialJSON), &parsed); err != nil {
		return ""
	}
	return parsed.ProjectID
}

func filterEnvKey(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

// writeADCFile writes service account JSON to a temp file for Google ADC.
// Uses a temp directory outside the repo to prevent accidental commit/push.
func writeADCFile(content string) (string, error) {
	dir, err := os.MkdirTemp("", "migration-harness-claude-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir for ADC: %w", err)
	}
	path := filepath.Join(dir, "gcp-adc.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return "", fmt.Errorf("write ADC file: %w", err)
	}
	return path, nil
}
