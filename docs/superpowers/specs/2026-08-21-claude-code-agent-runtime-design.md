# Claude Code as an alternate harness agent runtime

## Status

Go-side plan (Tasks 1-6) shipped as designed, with one correction found
during manual testing: `initialize`'s `protocolVersion` must be numeric
(`1`), not the string `"0.1"` this doc originally showed — claude-agent-acp
validates `initialize` params against the ACP JSON schema, which types
`protocolVersion` as an integer, and rejects a string with -32602 Invalid
params. goose is lenient and accepted either, which is why this wasn't
caught by the goose-only test suite.

The "published `agent-claude` container image" non-goal below is no
longer accurate: `claude-agent-acp` is now installed directly into
`agent-base` (`images/agent-base/Containerfile`), so every language image
built from it supports both runtimes — no separate image was needed. A
Kind integration test mirroring `hack/harness-test/` lives at
`hack/harness-claude-test/` (`make harness-claude-test`).

## Background

The migration harness (`harness/`) currently drives exactly one agent
runtime: goose, via `goose serve` + a WebSocket ACP client
(`internal/goose`, `internal/acp/wsclient.go`). ADR 0002 already
anticipated other ACP-speaking runtimes ("OpenCode, Claude, Cursor,
etc.") arriving over stdio instead of HTTP/WebSocket, and scoped a
"harness bridge" for it at roughly 200-300 lines.

A standalone spike (outside this repo, using `@agentclientprotocol/
claude-agent-acp` — the official ACP adapter for the Claude Agent SDK)
proved the underlying hypothesis: Claude Code speaks real ACP over
stdio, authenticates against the same GCP Vertex AI project/ADC
credentials the existing `gcp-vertex-ai` Gateway already uses, supports
per-session model selection over the wire (`session/set_config_option`,
`configId: "model"`), and produced a correct, self-verified,
adversarially-reviewed migration (Java 8 → 17) end to end.

This design wires that proven capability into the harness itself, as a
second, selectable agent runtime — without touching goose's existing
behavior.

## Goals

- Add a `claude` agent runtime, selectable via `HARNESS_AGENT_RUNTIME`,
  that drives Claude Code (via `claude-agent-acp`) instead of goose.
- Runs fully unattended (no human viewer required) — matches the
  existing headless pod model.
- Zero behavior change to the existing goose path when the runtime is
  unset or `"goose"`.
- No new CRD fields, no controller changes, no new credential paths —
  reuse the config and secrets the controller already injects.

## Non-goals (this pass)

- ACP tee / multi-viewer live streaming for the Claude runtime. Forced
  off; a later design can address it (it needs its own redesign per
  ADR 0008's per-viewer-dial model, which doesn't translate to a
  single stdio pipe).
- A published `agent-claude` container image or a real
  `AgentWorkflow`/`AgentWorkflowRun` end-to-end cluster run. This pass
  proves the Go integration with unit tests; image/workflow wiring is
  a follow-up once the Go side is solid.
- Support for providers other than `anthropic` and `gcp_vertex_ai`
  (the only providers this repo currently configures).

## Current state (relevant pieces)

- `cmd/migration-harness/main.go`: `runStage` — clones the repo, starts
  the agent server (`goose.StartServe`), dials it
  (`acp.WaitReadyDial` → `*WSClient`), wraps it in a
  `SessionClient`, optionally starts the ACP tee, builds the prompt
  (`internal/prompt`, runtime-agnostic), sends one `session/prompt`
  call, pushes the result.
- `internal/acp/wsclient.go`: `WSClient` — JSON-RPC 2.0 over a
  WebSocket, one demux goroutine (`readLoop`), request/response
  routing by id, notification fan-out, agent-initiated request
  dispatch (`session/request_permission` etc).
- `internal/acp/session.go`: `SessionClient` — ACP session
  operations (`initialize`, `session/new`, `session/prompt`,
  permission-request answering). Depends on the concrete `*WSClient`
  type, including three unexported helper methods
  (`registerPending`, `addNotifSink`, `removeNotifSink`,
  `unregisterPending`).
- `internal/goose/lifecycle.go`: `StartServe` + `providerEnv` — spawns
  `goose serve`, maps `KONVEYOR_LLM_*`-derived config to
  goose's env vars, writes a temp ADC file from
  `GOOGLE_APPLICATION_CREDENTIALS_JSON` for the `gcp_vertex_ai`
  provider.
- `internal/config/config.go`: `Config` — no runtime-selection field
  today; everything assumes goose.

## Design

### 1. `internal/acp`: extract a transport interface

`SessionClient` calls five unexported `*WSClient` methods plus `Send`,
`SendResponse`, `SetAgentRequestHandler`. Extract an unexported
interface (`rpcConn`) with exactly this method set. Change
`SessionClient.ws`'s type and `NewSessionClient`'s parameter type from
`*WSClient` to `rpcConn`. `*WSClient` already implements every method
— this is a type change only, no behavior change, and no caller
outside `session.go` needs to change (Go's structural typing means
`acp.NewSessionClient(wsClient)` in `main.go` keeps compiling as-is).

### 2. `internal/acp/stdioclient.go`: `StdioClient`

Same shape as `WSClient` minus the WebSocket specifics: wraps a
subprocess's `stdin`/`stdout` pipes, one demux goroutine reads
newline-delimited JSON from stdout and does the same three-way
dispatch (`routeResponse` / `fanOutNotification` /
`dispatchAgentRequest`) `WSClient.readLoop` does, writes go straight to
the stdin pipe under the same `writeMu` discipline. Implements exactly
the `rpcConn` interface — `SubscribeRawNotifications`/`CallRPC` (tee-only
methods) are not needed and not implemented, since the tee stays
goose-only in this pass.

### 3. `internal/claude/lifecycle.go`: process lifecycle

Mirrors `internal/goose/lifecycle.go`'s shape:

- `StartACP(ctx, cfg ACPConfig) (*Process, error)` — looks up
  `claude-agent-acp` on `PATH`, starts it with `Stdin`/`Stdout` as
  pipes (`Stderr` inherited to the harness's own stderr for
  diagnostics, matching goose's `cmd.Stderr = os.Stderr`), returns a
  `*Process` wrapping the pipes plus the same `Alive()`/`Stop()`
  lifecycle contract `goose.ServeProcess` has (so `main.go`'s health
  check and shutdown logic don't need runtime-specific branches beyond
  construction).
- `providerEnv(provider, model, apiKey, endpoint) (env []string,
  tempDirs []string)` — same signature and ADC-temp-file pattern as
  goose's, targeting Claude Code's vars instead:
  - `anthropic` → `ANTHROPIC_API_KEY`
  - `gcp_vertex_ai` → `CLAUDE_CODE_USE_VERTEX=1`, `CLOUD_ML_REGION`
    (parsed from the existing `Endpoint`, e.g.
    `global-aiplatform.googleapis.com` → `global`; region-prefixed
    hosts like `us-east5-aiplatform.googleapis.com` → `us-east5`),
    `ANTHROPIC_VERTEX_PROJECT_ID` (parsed from the `project_id` field
    of the `GOOGLE_APPLICATION_CREDENTIALS_JSON` blob — no new config
    field), `GOOGLE_APPLICATION_CREDENTIALS` (temp file, same
    `writeADCFile` pattern, reused verbatim or duplicated minimally).
  - other providers: warn and skip, matching goose's
    `providerEnv`'s existing fallback behavior.

### 4. `internal/config`: `AgentRuntime` field

Add `AgentRuntime string`, populated from `HARNESS_AGENT_RUNTIME`
(default `"goose"` when unset or unrecognized — fail open to today's
behavior).

### 5. `cmd/migration-harness/main.go`: branch at construction only

Introduce a small runtime-selection seam right where `srv` and
`wsClient` are constructed today (steps 5-6 in `runStage`): a
`start(ctx, cfg) (proc, conn, error)`-shaped branch that returns either
the existing goose `*ServeProcess`/`*WSClient` pair or a new
claude `*claude.Process`/`*acp.StdioClient` pair, both satisfying the
same two small local interfaces (`alive()`, `stop() error` for the
process; `rpcConn`-compatible for the connection). Everything after
session creation — prompt building, watcher, push, exit status — is
already generic over `SessionClient` and does not change.

For `"claude"` specifically, immediately after `CreateSession`, call
`session/set_config_option` with `configId: "mode"`,
`value: "bypassPermissions"` — this is what makes the run proceed
unattended: Claude Code's default session mode prompts for tool use,
and there is no live viewer to answer. `SessionClient.answerAgentRequest`
(fail-closed deny) stays as the safety net for any permission ask that
slips through anyway, same as it already is for goose's `approve` mode
edge case.

`cfg.ACPTee` is forced `false` for the claude runtime regardless of
`HARNESS_ACP_TEE`, with a logged warning — the tee's per-viewer dial
model (ADR 0008) doesn't apply to a single stdio pipe.

## Testing plan

- `internal/acp/stdioclient_test.go`: unit tests against a fake
  subprocess (a small test helper script, or an in-process pipe pair)
  mirroring the structure of `wsclient_test.go` — response routing,
  notification fan-out, agent-request dispatch, concurrent `Call`s.
- `internal/claude/lifecycle_test.go`: unit tests for `providerEnv`
  mirroring `goose`'s existing `lifecycle_test.go` pattern — table
  tests per provider, including the region-parsing and
  project-ID-from-JSON logic.
- `internal/acp/session_test.go`: confirm `SessionClient` compiles and
  behaves identically against both `*WSClient` and a minimal fake
  `rpcConn` (no behavior change expected, but the interface extraction
  should not silently change semantics).
- No changes to `internal/acp/acptest` (goose-specific integration
  tests) — they continue to exercise the goose path unchanged.

## Security implications

- `bypassPermissions` removes Claude Code's own tool-use confirmation
  for the claude runtime. This is equivalent to goose's existing
  unattended default (no `GOOSE_MODE=approve`) — the pod's own
  isolation (Sandbox, no push credentials per `AGENTS.md`'s "push
  credentials stay in the harness" rule) is the actual security
  boundary, not the agent's own permission prompts. No new privilege
  is granted beyond what goose's stage pods already have.
- No new credential material or paths: the claude runtime reuses the
  exact `GOOGLE_APPLICATION_CREDENTIALS_JSON`/`ANTHROPIC_API_KEY`
  values the controller already injects for goose.
- `ANTHROPIC_VERTEX_PROJECT_ID` is parsed from credential JSON already
  present in the process environment — no new secret exposure surface.
