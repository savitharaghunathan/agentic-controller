#!/usr/bin/env bash

# Runs the real harness workflow E2E and checks the multi-stage handoff
# contract exercised by hack/harness-test/workflow-resources.yaml.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="${NAMESPACE:-default}"
TIMEOUT_SECONDS="${E2E_TIMEOUT_SECONDS:-900}"
WORKFLOW_TIMESTAMP="${WORKFLOW_TIMESTAMP:-$(date +%s)}"
RUN_NAME="coolstore-migration-${WORKFLOW_TIMESTAMP}"

if ! command -v kubectl >/dev/null 2>&1; then
  echo "kubectl is required" >&2
  exit 1
fi

if ! [[ "$TIMEOUT_SECONDS" =~ ^[0-9]+$ ]] || [ "$TIMEOUT_SECONDS" -eq 0 ]; then
  echo "E2E_TIMEOUT_SECONDS must be a positive integer" >&2
  exit 1
fi

deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))

get_workflow_phase() {
  kubectl get agentworkflowrun "$RUN_NAME" -n "$NAMESPACE" \
    -o jsonpath='{.status.phase}' 2>/dev/null || true
}

get_stage_run() {
  local stage="$1"
  kubectl get agentworkflowrun "$RUN_NAME" -n "$NAMESPACE" \
    -o "jsonpath={.status.stages[?(@.name=='${stage}')].agentRunName}" 2>/dev/null || true
}

get_run_phase() {
  local run_name="$1"
  kubectl get agentrun "$run_name" -n "$NAMESPACE" \
    -o jsonpath='{.status.phase}' 2>/dev/null || true
}

get_run_created_at() {
  local run_name="$1"
  kubectl get agentrun "$run_name" -n "$NAMESPACE" \
    -o jsonpath='{.metadata.creationTimestamp}' 2>/dev/null || true
}

get_run_completed_at() {
  local run_name="$1"
  kubectl get agentrun "$run_name" -n "$NAMESPACE" \
    -o jsonpath='{.status.completionTime}' 2>/dev/null || true
}

get_stage_instructions() {
  local stage="$1"
  kubectl get agentworkflowrun "$RUN_NAME" -n "$NAMESPACE" \
    -o "jsonpath={.status.stages[?(@.name=='${stage}')].instructions}" 2>/dev/null || true
}

wait_for_workflow() {
  while :; do
    local phase
    phase="$(get_workflow_phase)"
    case "$phase" in
      Succeeded)
        return 0
        ;;
      Failed|LimitReached)
        echo "workflow $RUN_NAME finished with phase $phase" >&2
        return 1
        ;;
    esac

    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "timed out waiting for workflow $RUN_NAME (last phase: ${phase:-unknown})" >&2
      return 1
    fi
    sleep 2
  done
}

assert_stage_order() {
  local previous_stage="$1"
  local next_stage="$2"
  local previous_run next_run previous_phase previous_completed next_created

  while :; do
    previous_run="$(get_stage_run "$previous_stage")"
    next_run="$(get_stage_run "$next_stage")"

    if [ -n "$next_run" ]; then
      previous_phase="$(get_run_phase "$previous_run")"
      if [ "$previous_phase" != "Succeeded" ]; then
        echo "$next_stage AgentRun $next_run appeared before $previous_stage AgentRun $previous_run succeeded (phase: ${previous_phase:-unknown})" >&2
        return 1
      fi
      previous_completed="$(get_run_completed_at "$previous_run")"
      next_created="$(get_run_created_at "$next_run")"
      if [ -z "$previous_completed" ] || [ -z "$next_created" ] || [[ "$next_created" < "$previous_completed" ]]; then
        echo "stage timestamps are out of order: $previous_stage completed at ${previous_completed:-unknown}, $next_stage created at ${next_created:-unknown}" >&2
        return 1
      fi
      echo "stage order verified: $previous_stage ($previous_run) -> $next_stage ($next_run)"
      return 0
    fi

    case "$(get_workflow_phase)" in
      Failed|LimitReached)
        echo "workflow $RUN_NAME failed before $next_stage AgentRun was created" >&2
        return 1
        ;;
    esac

    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "timed out waiting for $next_stage AgentRun" >&2
      return 1
    fi
    sleep 2
  done
}

echo "starting harness workflow E2E: $RUN_NAME"
WORKFLOW_TIMESTAMP="$WORKFLOW_TIMESTAMP" \
  NAMESPACE="$NAMESPACE" \
  "$SCRIPT_DIR/harness-test/setup.sh"

assert_stage_order plan execute
assert_stage_order execute verify
wait_for_workflow

for stage in plan execute verify; do
  stage_run="$(get_stage_run "$stage")"
  stage_phase="$(get_run_phase "$stage_run")"
  if [ "$stage_phase" != "Succeeded" ]; then
    echo "$stage AgentRun $stage_run did not succeed (phase: ${stage_phase:-unknown})" >&2
    exit 1
  fi
  echo "stage succeeded: $stage ($stage_run)"
done

# The fixture's execute and verify instructions carry the handoff contract.
# Check the frozen workflow snapshot rather than runtime logs, which vary by
# agent implementation and may not echo the full prompt.
execute_instructions="$(get_stage_instructions execute)"
verify_instructions="$(get_stage_instructions verify)"
if ! grep -Eiq 'handoff|\.konveyor/handoff\.md' <<<"$execute_instructions" ||
   ! grep -Eiq 'handoff|\.konveyor/handoff\.md' <<<"$verify_instructions"; then
  echo "workflow snapshot does not contain the execute/verify handoff contract" >&2
  exit 1
fi
echo "handoff contract verified in the frozen execute and verify instructions"

if [ -n "${E2E_HANDOFF_REPO_URL:-}" ]; then
  verify_dir="$(mktemp -d)"
  cleanup() {
    rm -rf "$verify_dir"
  }
  trap cleanup EXIT

  branch="${E2E_HANDOFF_BRANCH:-${TARGET_BRANCH:-konveyor/migration-${WORKFLOW_TIMESTAMP}}}"
  GIT_TERMINAL_PROMPT=0 git clone --quiet --branch "$branch" "$E2E_HANDOFF_REPO_URL" "$verify_dir"
  handoff_file="$verify_dir/.konveyor/handoff.md"
  if [ ! -f "$handoff_file" ] || ! grep -q '^## Execute' "$handoff_file" || ! grep -q '^## Verify' "$handoff_file"; then
    echo "committed handoff is missing expected Execute/Verify sections on branch $branch" >&2
    exit 1
  fi
  echo "committed handoff verified: $handoff_file"
else
  echo "committed branch verification skipped; set E2E_HANDOFF_REPO_URL to enable it"
fi

echo "workflow handoff E2E passed: $RUN_NAME"
