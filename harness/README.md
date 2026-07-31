# migration-harness

**Thin single-stage runner** that handles git plumbing and [goose](https://github.com/block/goose) lifecycle for AI-powered code migrations. The harness does not know what stage it is running — migration intelligence lives in [SkillCards](../CONTEXT.md).

---

## How It Works

```
┌──────────────────────────────────────────────────────┐
│                  migration-harness run                │
│                                                      │
│  1. Load config from env                             │
│  2. Resolve app + git creds from Hub API             │
│  3. Clone repo, strip creds, checkout target branch  │
│  4. Write analysis insights to .konveyor/            │
│  5. Start goose serve (ACP)                          │
│  6. Discover skills from /opt/skills/*/SKILL.md      │
│  7. Build prompt from context layers                 │
│  8. Start filesystem watcher (incremental push)      │
│  9. Send single ACP prompt (blocks until completion) │
│ 10. Final push                                       │
└──────────────────────────────────────────────────────┘
```

The harness sends **one prompt** per stage. The AgentPlaybookRun controller handles stage sequencing — the harness is identical in every stage image.

---

## Prerequisites

- **Go 1.21+** (to build)
- **[goose](https://github.com/block/goose)** (started by the harness via `goose serve`)
- **git**

---

## Build

```bash
cd harness
go build -o migration-harness ./cmd/migration-harness/
```

---

## Configuration

All configuration is via environment variables — there is no config file or `init` command.

### Required

| Variable | Description |
|----------|-------------|
| `KONVEYOR_MODEL_PRIMARY_MODEL` | LLM model name |
| `KONVEYOR_MODEL_PRIMARY_PROVIDER` | LLM provider (e.g. `anthropic`, `openai`) |
| `HUB_BASE_URL` | Konveyor Hub API base URL |
| `APP_ID` | Application ID in Hub |
| `KONVEYOR_ACP_SECRET_KEY` | Secret key for ACP WebSocket auth |
| `TARGET_BRANCH` | Git branch to push results to (must differ from source) |

### Optional

| Variable | Default | Description |
|----------|---------|-------------|
| `KONVEYOR_MODEL_PRIMARY_ENDPOINT` | — | Custom LLM endpoint URL |
| `KONVEYOR_MODEL_PRIMARY_API_KEY` | — | LLM API key |
| `HUB_TOKEN` | — | Hub authentication token |
| `KONVEYOR_PARAM_MAX_TURNS` | `200` | Max tool-call turns before terminating |
| `HARNESS_WORK_DIR` | `/workspace/repo` | Clone directory |
| `HARNESS_SKILLS_DIR` | `/opt/skills` | Skills mount directory |
| `KONVEYOR_PROMPT` | — | Agent-level standing instructions |
| `KONVEYOR_PLAYBOOK_INSTRUCTIONS` | — | Playbook guide context |
| `KONVEYOR_INSTRUCTIONS` | — | Stage-specific task instructions |

---

## Git Lifecycle

1. **Clone** — harness clones using Hub-provided credentials
2. **Strip credentials** — strips any embedded credentials from the remote URL (safety net — auth is passed via transport, not URL)
3. **Clear env** — Hub token is cleared from the process environment
4. **Checkout branch** — checks out `TARGET_BRANCH`
5. **Agent commits** — the agent commits locally with descriptive messages (per skill instructions)
6. **Watcher** — background fsnotify watcher pushes agent commits after a 30s quiet period
7. **Final push** — catches anything the watcher missed (runs even on failure)

The agent commits locally but never sees push credentials — only the harness binary pushes.

---

## Skill Discovery

The harness globs `/opt/skills/*/SKILL.md` at startup. Skills are mounted into agent pods by the controller via SkillCard init containers. The harness concatenates all discovered skills into the prompt alongside environment-provided context layers.

Two kinds of skills:

- **Stage skills** (plan, execute, verify) — define *process*: what to do
- **Domain skills** (e.g. javaee-to-quarkus) — define *knowledge*: how to do it

---

## Architecture

```
cmd/migration-harness/main.go    CLI entry point (cobra, single "run" command)
internal/
├── config/        Env-var configuration
├── acp/           ACP WebSocket client (session, prompt)
├── goose/         goose serve lifecycle (start, health, stop)
├── hub/           Konveyor Hub API client (app, creds, analysis)
├── git/           Credential-isolated git operations (go-git)
├── watcher/       Debounced filesystem watcher (fsnotify)
└── logging/       Colored terminal output
```

### Key design decisions

- **Single `run` command** — no subcommands, no interactive mode. One prompt, one stage.
- **go-git** — all git operations use `github.com/go-git/go-git/v5`. No shell-out to git CLI.
- **Credential isolation** — Hub and git push credentials are used by the harness only, cleared before goose starts. The agent commits locally; the harness pushes.
- **ACP WebSocket** — connects to goose via JSON-RPC over WebSocket (ACP protocol).
- **Exit status from ACP** — clean `SendPrompt` return = exit 0. Any error or goose crash = exit 1.

---

## License

Apache-2.0
