#!/usr/bin/env bash
# Run migration-harness locally with HARNESS_AGENT_RUNTIME=claude — no
# Kubernetes/Kind cluster, no container images. Talks to a local tackle2-hub
# (see hub-local.sh) and real GCP Vertex AI credentials.
#
# WARNING: this performs a REAL git clone + push against whatever repo the
# Hub application points at (hub-local.sh's default is a fork of coolstore),
# and spends real Vertex AI tokens. Point APP_NAME/HUB_BASE_URL at a scratch
# app+repo you're comfortable pushing test branches to before running this
# against anything you care about.
#
# Prerequisites:
#   - A reachable Hub with HUB_BASE_URL/HUB_TOKEN/HUB_TOKEN_ID/APP_ID set —
#     either export them yourself, or run ../hub-local.sh up first (this
#     script auto-sources its generated env file if present).
#   - gcloud application-default credentials configured
#     (gcloud auth application-default login) for a GCP project with Vertex
#     AI Claude models enabled.
#   - Node.js/npm on PATH, to install claude-agent-acp if it's missing.
#
# Usage:
#   hack/harness-claude-local.sh
#
# Env overrides (all optional):
#   KONVEYOR_LLM_MODEL   Vertex model id (default: claude-sonnet-4-5)
#   GCP_REGION           Vertex region (default: global)
#   TARGET_BRANCH        branch to push results to (default: a timestamped
#                         konveyor/claude-runtime-local-test-<ts> branch)
#   STAGE_PROMPT         the task given to the agent (default: a harmless
#                         read-only "describe this repo" prompt)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
HARNESS_DIR="$REPO_ROOT/harness"

echo "=== Checking Hub ==="
HUB_ENV_FILE="$(cd "$REPO_ROOT/.." 2>/dev/null && pwd)/tackle2-hub/.run/hub.env"
if [ -z "${HUB_BASE_URL:-}" ] && [ -f "$HUB_ENV_FILE" ]; then
    echo "  sourcing $HUB_ENV_FILE"
    # shellcheck disable=SC1090
    source "$HUB_ENV_FILE"
fi
: "${HUB_BASE_URL:?HUB_BASE_URL not set. Run ../hub-local.sh up, or export HUB_BASE_URL/HUB_TOKEN/HUB_TOKEN_ID/APP_ID yourself.}"
: "${HUB_TOKEN:?HUB_TOKEN not set — see the HUB_BASE_URL error above.}"
: "${APP_ID:?APP_ID not set — see the HUB_BASE_URL error above.}"
if ! curl -s -o /dev/null "$HUB_BASE_URL/hub/applications"; then
    echo "ERROR: Hub not reachable at $HUB_BASE_URL" >&2
    exit 1
fi
echo "  Hub reachable at $HUB_BASE_URL (app id=$APP_ID)"

echo ""
echo "=== Checking claude-agent-acp ==="
if ! command -v claude-agent-acp >/dev/null 2>&1; then
    INSTALL_DIR="${SCRIPT_DIR}/.claude-agent-acp"
    echo "  not on PATH — installing locally to $INSTALL_DIR"
    mkdir -p "$INSTALL_DIR"
    # --prefix pins the install target explicitly: without it, npm walks up
    # the directory tree looking for the nearest package.json and installs
    # there instead (e.g. into $HOME, if a package.json happens to live
    # there) rather than into INSTALL_DIR.
    npm install --prefix "$INSTALL_DIR" @agentclientprotocol/claude-agent-acp
    export PATH="${INSTALL_DIR}/node_modules/.bin:${PATH}"
    hash -r
fi
command -v claude-agent-acp >/dev/null 2>&1 || { echo "ERROR: claude-agent-acp still not found on PATH" >&2; exit 1; }
echo "  using $(command -v claude-agent-acp)"

echo ""
echo "=== Checking GCP Vertex credentials ==="
ADC_PATH="${GOOGLE_APPLICATION_CREDENTIALS:-$HOME/.config/gcloud/application_default_credentials.json}"
[ -f "$ADC_PATH" ] || { echo "ERROR: no ADC file at $ADC_PATH — run: gcloud auth application-default login" >&2; exit 1; }
GCP_PROJECT="$(gcloud config get-value project 2>/dev/null || true)"
[ -n "$GCP_PROJECT" ] || { echo "ERROR: no gcloud project set — run: gcloud config set project <id>" >&2; exit 1; }
echo "  ADC at $ADC_PATH, project $GCP_PROJECT"

echo ""
echo "=== Building migration-harness ==="
(cd "$HARNESS_DIR" && go build -o /tmp/migration-harness ./cmd/migration-harness)
echo "  built /tmp/migration-harness"

TIMESTAMP=$(date +%s)
export HARNESS_AGENT_RUNTIME=claude
export KONVEYOR_LLM_MODEL="${KONVEYOR_LLM_MODEL:-claude-sonnet-4-5}"
export KONVEYOR_LLM_PROVIDER=gcp-vertex-ai
export KONVEYOR_LLM_ENDPOINT="${GCP_REGION:-global}-aiplatform.googleapis.com"
export GOOGLE_APPLICATION_CREDENTIALS_JSON
GOOGLE_APPLICATION_CREDENTIALS_JSON="$(cat "$ADC_PATH")"
# Covers user ADC (gcloud auth application-default login), which has no
# project_id field for the harness's vertexProjectID parser to find — a
# real deployment's service-account JSON does have one. Per Claude Code's
# own docs, GOOGLE_CLOUD_PROJECT takes precedence over
# ANTHROPIC_VERTEX_PROJECT_ID anyway, so this covers both cases.
export GOOGLE_CLOUD_PROJECT="$GCP_PROJECT"
export KONVEYOR_ACP_SECRET_KEY="${KONVEYOR_ACP_SECRET_KEY:-local-test-key}"
export TARGET_BRANCH="${TARGET_BRANCH:-konveyor/claude-runtime-local-test-$TIMESTAMP}"
export KONVEYOR_INSTRUCTIONS="${STAGE_PROMPT:-Read the README in this repository and list its top-level source directories in your final message. Make no changes.}"
export HARNESS_ACP_TEE=off   # the ACP tee doesn't support the claude runtime yet
export HARNESS_WORK_DIR="${HARNESS_WORK_DIR:-/tmp/harness-claude-local-repo}"
rm -rf "$HARNESS_WORK_DIR"

echo ""
echo "=== Running (HARNESS_AGENT_RUNTIME=claude, target branch $TARGET_BRANCH) ==="
/tmp/migration-harness run
