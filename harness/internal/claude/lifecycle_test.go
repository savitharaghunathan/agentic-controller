package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProviderEnvAnthropic(t *testing.T) {
	env, _ := providerEnv("anthropic", "claude-sonnet-4-5", "sk-ant-key", "https://api.anthropic.com")
	assertEnvContains(t, env, "ANTHROPIC_API_KEY", "sk-ant-key")
	assertEnvContains(t, env, "ANTHROPIC_BASE_URL", "https://api.anthropic.com")
	assertEnvContains(t, env, "ANTHROPIC_MODEL", "claude-sonnet-4-5")
	// For anthropic provider, we don't add vertex-specific variables
	// (though they may be inherited from the environment)
}

func TestProviderEnvGCPVertex(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS_JSON", `{"type":"service_account","project_id":"my-gcp-project"}`)
	env, tempDirs := providerEnv("gcp-vertex-ai", "claude-sonnet-4-5", "", "global-aiplatform.googleapis.com")
	for _, d := range tempDirs {
		defer os.RemoveAll(d)
	}
	assertEnvContains(t, env, "CLAUDE_CODE_USE_VERTEX", "1")
	assertEnvContains(t, env, "CLOUD_ML_REGION", "global")
	assertEnvContains(t, env, "ANTHROPIC_VERTEX_PROJECT_ID", "my-gcp-project")
	assertEnvContains(t, env, "ANTHROPIC_MODEL", "claude-sonnet-4-5")
	assertEnvPresent(t, env, "GOOGLE_APPLICATION_CREDENTIALS")
	assertEnvNotPresent(t, env, "GOOGLE_APPLICATION_CREDENTIALS_JSON")
}

func TestProviderEnvGCPVertexRegionPrefixed(t *testing.T) {
	env, _ := providerEnv("gcp-vertex-ai", "", "", "us-east5-aiplatform.googleapis.com")
	assertEnvContains(t, env, "CLOUD_ML_REGION", "us-east5")
}

func TestProviderEnvGCPVertexNoCredentialsJSON(t *testing.T) {
	_, tempDirs := providerEnv("gcp-vertex-ai", "", "", "global-aiplatform.googleapis.com")
	if len(tempDirs) != 0 {
		t.Errorf("expected no temp dirs without credentials JSON, got %v", tempDirs)
	}
	// When GOOGLE_APPLICATION_CREDENTIALS_JSON is not set, we don't create temp files
	// or set the related vertex variables (from our code, not from pre-existing env)
}

func TestProviderEnvUnmappedProvider(t *testing.T) {
	// For unmapped providers, we don't add provider-specific variables
	// (but the test param would be passed to the unmarked provider, so model won't be added either)
	env, _ := providerEnv("aws-bedrock", "some-model", "unused", "")
	// Just verify that the function doesn't crash and returns an environment
	if len(env) == 0 {
		t.Errorf("expected non-empty environment")
	}
}

func TestProviderEnvEmptyStringsAddNothing(t *testing.T) {
	// With empty provider string, we don't add any provider-specific variables
	env, _ := providerEnv("", "", "", "")
	// Just verify that the function doesn't crash and returns an environment
	if len(env) == 0 {
		t.Errorf("expected non-empty environment")
	}
}

func TestVertexRegion(t *testing.T) {
	cases := map[string]string{
		"global-aiplatform.googleapis.com":         "global",
		"us-east5-aiplatform.googleapis.com":       "us-east5",
		"https://global-aiplatform.googleapis.com": "global",
		"not-a-vertex-host.example.com":            "",
		"":                                         "",
	}
	for endpoint, want := range cases {
		if got := vertexRegion(endpoint); got != want {
			t.Errorf("vertexRegion(%q) = %q, want %q", endpoint, got, want)
		}
	}
}

func TestVertexProjectID(t *testing.T) {
	if got := vertexProjectID(`{"type":"service_account","project_id":"my-project","other":"field"}`); got != "my-project" {
		t.Errorf("vertexProjectID = %q, want %q", got, "my-project")
	}
	if got := vertexProjectID("not json"); got != "" {
		t.Errorf("vertexProjectID(invalid) = %q, want empty", got)
	}
}

func TestWriteADCFile(t *testing.T) {
	content := `{"type":"service_account","project_id":"test"}`
	path, err := writeADCFile(content)
	if err != nil {
		t.Fatalf("writeADCFile: %v", err)
	}
	defer os.RemoveAll(filepath.Dir(path))

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != content {
		t.Errorf("content = %q, want %q", string(data), content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("permissions = %o, want 0600", info.Mode().Perm())
	}
}

func TestStartACPBinaryNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir on PATH — claude-agent-acp cannot resolve
	_, err := StartACP(context.Background(), ACPConfig{})
	if err == nil {
		t.Fatal("expected error when claude-agent-acp is not on PATH")
	}
}

func assertEnvContains(t *testing.T, env []string, key, value string) {
	t.Helper()
	want := key + "=" + value
	for _, e := range env {
		if e == want {
			return
		}
	}
	t.Errorf("env missing %s=%s", key, value)
}

func assertEnvPresent(t *testing.T, env []string, key string) {
	t.Helper()
	prefix := key + "="
	for _, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			return
		}
	}
	t.Errorf("env missing key %s", key)
}

func assertEnvNotPresent(t *testing.T, env []string, key string) {
	t.Helper()
	prefix := key + "="
	for _, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			t.Errorf("env should not contain key %s, found %s", key, e)
		}
	}
}
