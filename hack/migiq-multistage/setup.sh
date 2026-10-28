#!/bin/bash
# Setup the migIQ multistage workflow test in a Kind cluster.
#
# Unlike hack/harness-test/setup.sh, this does NOT build or load any agent
# or skill images: it uses Dylan's already-published quay.io/dymurray/*
# images directly, so the cluster just needs outbound network access to
# pull them.
#
# Prerequisites:
#   - Kind cluster running with the agentic-controller installed and
#     reconciling (make e2e-setup && make install && make run, or
#     hack/setup-e2e.sh)
#   - A Hub reachable from the cluster (hack/install-konveyor.sh for a local
#     one, or point HUB_BASE_URL elsewhere -- see the hardcoded value below)
#   - HUB_TOKEN exported (mint via POST /auth/tokens against your Hub if the
#     run isn't created through the Hub UI/API -- see docs/getting-started.md)
#   - APP_ID exported: the Hub application ID for the target repo, with a
#     source Identity already attached in Hub for git push credentials
#
# Usage:
#   HUB_TOKEN=... APP_ID=... ENMAAS_KEY=... hack/migiq-multistage/setup.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "=== Creating secrets ==="

# EnMaaS credentials (Anthropic Messages API-compatible; see
# harness/internal/goose/lifecycle.go's providerEnv for how provider:
# anthropic + a custom endpoint map to ANTHROPIC_API_KEY/ANTHROPIC_HOST).
if [ -z "${ENMAAS_KEY:-}" ]; then
    echo "ERROR: ENMAAS_KEY must be set."
    exit 1
fi
kubectl create secret generic enmaas-credentials \
    --from-literal=api-key="$ENMAAS_KEY" \
    --dry-run=client -o yaml | kubectl apply -f -
echo "  enmaas-credentials created"

# Hub token -- must be set in environment; this script does not mint one.
if [ -z "${HUB_TOKEN:-}" ]; then
    echo "ERROR: HUB_TOKEN must be set. Mint one against your Hub (POST /auth/tokens)"
    echo "or export the per-run token if this run was created through the Hub UI/API."
    exit 1
fi
echo "  hub token set (HUB_TOKEN_ID=${HUB_TOKEN_ID:-<unset>})"

# Hub application ID -- must have a source Identity attached in Hub for git push.
if [ -z "${APP_ID:-}" ]; then
    echo "ERROR: APP_ID must be set. This is the Hub application ID for the target"
    echo "repo, and it must already have a source (git) Identity attached in Hub,"
    echo "or the final push will silently attempt an anonymous push and fail."
    exit 1
fi
echo "  app id: $APP_ID"

echo ""
echo "=== Applying Gateway + migIQ SkillCards ==="
kubectl apply -f "$SCRIPT_DIR/resources.yaml"

echo ""
echo "=== Waiting for Gateway to verify ==="
# The AgentRun controller does not watch Gateway status transitions, so an
# AgentRunn created while the Gateway's connectivity check is still running
# fails Sandbox creation with "gateway is not Ready" and never retries on its
# own -- it needs an unrelated reconcile trigger to notice the Gateway
# becoming Ready afterward. Waiting here avoids that race entirely.
if ! kubectl wait --for=condition=Ready=True gateway/enmaas-glm --timeout=60s; then
    echo "ERROR: Gateway enmaas-glm did not become Ready in time."
    kubectl get gateway enmaas-glm -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'
    echo ""
    exit 1
fi

echo ""
echo "=== Applying multi-stage workflow ==="
TIMESTAMP=$(date +%s)
sed -e "s/__TIMESTAMP__/$TIMESTAMP/g" \
    -e "s|__HUB_TOKEN__|$HUB_TOKEN|g" \
    -e "s|__HUB_TOKEN_ID__|${HUB_TOKEN_ID:-}|g" \
    -e "s|__APP_ID__|$APP_ID|g" \
    "$SCRIPT_DIR/workflow-resources.yaml" | kubectl apply -f -
echo "  AgentWorkflowRun: migiq-migration-multistage-$TIMESTAMP"

echo ""
echo "=== Done ==="
echo "Watch the run: kubectl get agentworkflowrun migiq-migration-multistage-$TIMESTAMP -w"
echo "Check pods:    kubectl get pods"
echo "View logs:     kubectl logs -f migiq-migration-multistage-${TIMESTAMP}-plan -c agent"
echo ""
echo "Expect plan/execute to have a real shot at working; containerize/deploy"
echo "are likely to fail regardless of this setup -- see the warnings at the"
echo "top of workflow-resources.yaml (sandbox build capability and target-"
echo "cluster credentials are both unresolved platform gaps, tracked by #261)."
