#!/bin/bash
# Simulate an AgentPlaybookRun by creating sequential AgentRuns.
#
# The controller does not yet have an AgentPlaybookRun reconciler, so this
# script manually orchestrates plan → execute → verify on a shared branch.
#
# Prerequisites:
#   - Kind cluster with controller running
#   - hack/harness-test/setup.sh already run (secrets, images, Agent CRs)
#
# Usage:
#   hack/harness-test/run-playbook.sh [options]
#
# Options:
#   --repo URL        Git repo to migrate (default: coolstore)
#   --branch NAME     Target branch (default: konveyor/playbook-<timestamp>)
#   --stages STAGES   Comma-separated stages to run (default: plan,execute,verify)
#   --timeout SECS    Per-stage timeout in seconds (default: 1800)
#   --skip-cleanup    Don't delete AgentRuns on failure

set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/savitharaghunathan/coolstore.git}"
TARGET_BRANCH=""
STAGES="plan,execute,verify"
TIMEOUT=1800
SKIP_CLEANUP=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --repo)      REPO_URL="$2"; shift 2 ;;
        --branch)    TARGET_BRANCH="$2"; shift 2 ;;
        --stages)    STAGES="$2"; shift 2 ;;
        --timeout)   TIMEOUT="$2"; shift 2 ;;
        --skip-cleanup) SKIP_CLEANUP=true; shift ;;
        *)           echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "$TARGET_BRANCH" ]; then
    TARGET_BRANCH="konveyor/playbook-$(date +%s)"
fi

GCP_PROJECT_ID=$(gcloud config get-value project 2>/dev/null)
if [ -z "$GCP_PROJECT_ID" ]; then
    echo "ERROR: No GCP project set. Run: gcloud config set project <project-id>"
    exit 1
fi

STAGE_AGENTS=(
    "plan:migration-plan-agent"
    "execute:migration-execute-java"
    "verify:migration-verify-java"
)

STAGE_INSTRUCTIONS=(
    "plan:Analyze the project structure using graphify and produce PLAN.md with migration steps"
    "execute:Execute each step in PLAN.md to migrate the code from Java EE to Quarkus"
    "verify:Run mvn clean compile, fix any compilation errors, then run tests"
)

get_agent_for_stage() {
    local stage="$1"
    for mapping in "${STAGE_AGENTS[@]}"; do
        if [[ "$mapping" == "${stage}:"* ]]; then
            echo "${mapping#*:}"
            return
        fi
    done
    echo "ERROR: No agent mapping for stage: $stage" >&2
    return 1
}

get_instructions_for_stage() {
    local stage="$1"
    for mapping in "${STAGE_INSTRUCTIONS[@]}"; do
        if [[ "$mapping" == "${stage}:"* ]]; then
            echo "${mapping#*:}"
            return
        fi
    done
    echo ""
}

create_agentrun() {
    local stage="$1"
    local run_name="playbook-${stage}-$(date +%s)"
    local agent_ref
    agent_ref=$(get_agent_for_stage "$stage")
    local instructions
    instructions=$(get_instructions_for_stage "$stage")

    cat <<EOF | kubectl apply -f - >&2
apiVersion: konveyor.io/v1alpha1
kind: AgentRun
metadata:
  name: ${run_name}
spec:
  agentRef: ${agent_ref}
  models:
    - role: primary
      provider: gcp-vertex-ai
      model: claude-sonnet-4-5
  instructions: "${instructions}"
  env:
    - name: GIT_REPO_URL
      value: "${REPO_URL}"
    - name: GIT_TOKEN
      valueFrom:
        secretKeyRef:
          name: git-credentials
          key: token
    - name: GIT_TARGET_BRANCH
      value: "${TARGET_BRANCH}"
    - name: GCP_PROJECT_ID
      value: "${GCP_PROJECT_ID}"
    - name: GCP_LOCATION
      value: "global"
EOF

    echo "$run_name"
}

wait_for_agentrun() {
    local run_name="$1"
    local timeout="$2"
    local elapsed=0
    local poll_interval=10

    echo "  Waiting for AgentRun ${run_name} (timeout: ${timeout}s)..."

    while [ $elapsed -lt "$timeout" ]; do
        local phase
        phase=$(kubectl get agentrun "$run_name" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")

        case "$phase" in
            Succeeded)
                local duration
                duration=$(kubectl get agentrun "$run_name" -o jsonpath='{.status.duration}' 2>/dev/null || echo "?")
                echo "  AgentRun ${run_name}: Succeeded (${duration}s)"
                return 0
                ;;
            Failed)
                echo "  AgentRun ${run_name}: Failed"
                echo "  Conditions:"
                kubectl get agentrun "$run_name" -o jsonpath='{range .status.conditions[*]}    {.type}: {.reason} - {.message}{"\n"}{end}' 2>/dev/null
                return 1
                ;;
            *)
                if [ $((elapsed % 30)) -eq 0 ]; then
                    echo "  [${elapsed}s] phase=${phase}"
                fi
                ;;
        esac

        sleep "$poll_interval"
        elapsed=$((elapsed + poll_interval))
    done

    echo "  AgentRun ${run_name}: Timed out after ${timeout}s"
    return 1
}

echo "============================================"
echo "  AgentPlaybookRun Simulator"
echo "============================================"
echo ""
echo "  Repo:    ${REPO_URL}"
echo "  Branch:  ${TARGET_BRANCH}"
echo "  Stages:  ${STAGES}"
echo "  Timeout: ${TIMEOUT}s per stage"
echo ""

IFS=',' read -ra STAGE_LIST <<< "$STAGES"
COMPLETED_RUNS=()
FAILED=false

for stage in "${STAGE_LIST[@]}"; do
    echo "--- Stage: ${stage} ---"

    run_name=$(create_agentrun "$stage")
    COMPLETED_RUNS+=("$run_name")
    echo "  Created AgentRun: ${run_name}"

    # Stream logs in background once the pod starts.
    (
        sleep 15
        pod=$(kubectl get pods -l "konveyor.io/agentrun=${run_name}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
        if [ -n "$pod" ]; then
            kubectl logs -f "$pod" -c agent 2>/dev/null | sed "s/^/  [${stage}] /" &
        fi
    ) &
    LOG_PID=$!

    if ! wait_for_agentrun "$run_name" "$TIMEOUT"; then
        FAILED=true
        kill "$LOG_PID" 2>/dev/null || true
        wait "$LOG_PID" 2>/dev/null || true
        echo ""
        echo "Stage ${stage} failed. Stopping playbook."
        break
    fi

    kill "$LOG_PID" 2>/dev/null || true
    wait "$LOG_PID" 2>/dev/null || true
    echo ""
done

echo "============================================"
echo "  Results"
echo "============================================"
echo ""

for run_name in "${COMPLETED_RUNS[@]}"; do
    phase=$(kubectl get agentrun "$run_name" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
    duration=$(kubectl get agentrun "$run_name" -o jsonpath='{.status.duration}' 2>/dev/null || echo "?")
    agent=$(kubectl get agentrun "$run_name" -o jsonpath='{.spec.agentRef}' 2>/dev/null || echo "?")
    printf "  %-40s  %-12s  %ss  (%s)\n" "$run_name" "$phase" "$duration" "$agent"
done

echo ""
echo "  Branch: ${TARGET_BRANCH}"

if [ "$FAILED" = true ]; then
    echo ""
    echo "  Playbook FAILED."
    if [ "$SKIP_CLEANUP" = false ]; then
        echo "  (Use --skip-cleanup to keep AgentRuns for debugging)"
    fi
    exit 1
else
    echo ""
    echo "  Playbook SUCCEEDED."
    exit 0
fi
