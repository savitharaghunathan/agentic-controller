#!/bin/bash
# Setup harness integration test in a Kind cluster.
#
# Prerequisites:
#   - Kind cluster running (make e2e-setup)
#
# Usage:
#   hack/harness-test/setup.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

CONTAINER_TOOL="${CONTAINER_TOOL:-podman}"
KIND_CLUSTER="${KIND_CLUSTER:-agentic-controller-e2e}"
NAMESPACE="${NAMESPACE:-default}"
SKIP_AGENT_BUILD="${SKIP_AGENT_BUILD:-false}"

echo "=== Creating secrets ==="

# Vertex AI credentials from local ADC
ADC_PATH="${HOME}/.config/gcloud/application_default_credentials.json"
if [ ! -f "$ADC_PATH" ]; then
    echo "ERROR: ADC file not found at $ADC_PATH"
    echo "Run: gcloud auth application-default login"
    exit 1
fi
kubectl create secret generic vertex-credentials \
    --namespace "$NAMESPACE" \
    --from-file=GOOGLE_APPLICATION_CREDENTIALS_JSON="$ADC_PATH" \
    --dry-run=client -o yaml | kubectl apply -f -
echo "  vertex-credentials created"

# Hub token — must be set in environment.
if [ -z "${HUB_TOKEN:-}" ]; then
    echo "ERROR: HUB_TOKEN must be set. Export HUB_TOKEN from your Hub instance."
    exit 1
fi
echo "  hub token set (HUB_TOKEN_ID=${HUB_TOKEN_ID:-<unset>})"

echo ""
echo "=== Building agent images ==="
if [ "$SKIP_AGENT_BUILD" = "true" ]; then
    echo "  skipped (using prebuilt agent images)"
else
    make -C "$REPO_ROOT" agent-java-build CONTAINER_TOOL="$CONTAINER_TOOL"
fi

echo ""
echo "=== Building skill images ==="

SKILL_IMAGE="quay.io/konveyor/skills"
SKILL_DIRS=(plan execute verify)

for SKILL in "${SKILL_DIRS[@]}"; do
    SKILL_PATH="$REPO_ROOT/catalog/skills/$SKILL"
    if [ ! -d "$SKILL_PATH" ]; then
        echo "  WARN: skill dir $SKILL_PATH not found, skipping"
        continue
    fi
    # Build from a clean context. Passing the Containerfile on stdin to a
    # remote Podman machine can make its macOS temporary path appear in the
    # context, which then gets copied into the scratch image.
    SKILL_CONTEXT=$(mktemp -d)
    cp -R "$SKILL_PATH"/. "$SKILL_CONTEXT"/
    cat > "$SKILL_CONTEXT/Containerfile" <<'SKILLEOF'
FROM scratch
COPY . /
SKILLEOF
    $CONTAINER_TOOL build -t "${SKILL_IMAGE}:${SKILL}" "$SKILL_CONTEXT"
    rm -rf "$SKILL_CONTEXT"
    echo "  built ${SKILL_IMAGE}:${SKILL}"
done

echo ""
echo "=== Loading images into Kind ==="

IMAGES=(
    "quay.io/konveyor/agent-base"
    "quay.io/konveyor/agent-java"
)

for IMG in "${IMAGES[@]}"; do
    $CONTAINER_TOOL tag "${IMG}:latest" "${IMG}:dev"
    if [ "$CONTAINER_TOOL" = "podman" ]; then
        $CONTAINER_TOOL save "${IMG}:dev" -o /tmp/agent-image.tar
        KIND_EXPERIMENTAL_PROVIDER=podman kind load image-archive /tmp/agent-image.tar --name "$KIND_CLUSTER"
        rm -f /tmp/agent-image.tar
    else
        kind load docker-image "${IMG}:dev" --name "$KIND_CLUSTER"
    fi
    echo "  loaded ${IMG}:dev"
done

for SKILL in "${SKILL_DIRS[@]}"; do
    if $CONTAINER_TOOL image inspect "${SKILL_IMAGE}:${SKILL}" >/dev/null 2>&1; then
        if [ "$CONTAINER_TOOL" = "podman" ]; then
            $CONTAINER_TOOL save "${SKILL_IMAGE}:${SKILL}" -o /tmp/skill-image.tar
            KIND_EXPERIMENTAL_PROVIDER=podman kind load image-archive /tmp/skill-image.tar --name "$KIND_CLUSTER"
            rm -f /tmp/skill-image.tar
        else
            kind load docker-image "${SKILL_IMAGE}:${SKILL}" --name "$KIND_CLUSTER"
        fi
        echo "  loaded ${SKILL_IMAGE}:${SKILL}"
    fi
done

echo ""
echo "=== Applying resources ==="
# Prefer the Vertex project that actually hosts the Claude models
# (ANTHROPIC_VERTEX_PROJECT_ID, same one Claude Code uses); the gcloud
# default project may lack access to the anthropic publisher models.
GCP_PROJECT_ID="${ANTHROPIC_VERTEX_PROJECT_ID:-$(gcloud config get-value project 2>/dev/null)}"
if [ -z "$GCP_PROJECT_ID" ]; then
    echo "ERROR: No GCP project set. Set ANTHROPIC_VERTEX_PROJECT_ID or run: gcloud config set project <project-id>"
    exit 1
fi
echo "  GCP project: (set)"
sed "s/__GCP_PROJECT_ID__/$GCP_PROJECT_ID/" "$SCRIPT_DIR/resources.yaml" | kubectl apply -n "$NAMESPACE" -f -
TIMESTAMP="${WORKFLOW_TIMESTAMP:-$(date +%s)}"
sed -e "s/__GCP_PROJECT_ID__/$GCP_PROJECT_ID/g" \
    -e "s/__TIMESTAMP__/$TIMESTAMP/g" \
    -e "s|__HUB_TOKEN__|$HUB_TOKEN|g" \
    -e "s|__HUB_TOKEN_ID__|${HUB_TOKEN_ID:-}|g" \
    "$SCRIPT_DIR/workflow-resources.yaml" | kubectl apply -n "$NAMESPACE" -f -
echo "  AgentWorkflowRun: coolstore-migration-$TIMESTAMP"

echo ""
echo "=== Done ==="
echo "Watch the run: kubectl get agentworkflowrun coolstore-migration-$TIMESTAMP -n $NAMESPACE -w"
echo "Check pods:    kubectl get pods -n $NAMESPACE"
echo "View logs:     kubectl logs -n $NAMESPACE -f coolstore-migration-${TIMESTAMP}-plan -c agent"
