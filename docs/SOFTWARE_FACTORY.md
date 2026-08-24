# Software factory - single build workflow

One canonical workflow named `build` turns aimee into a subscription software factory. Aimee is the only orchestrator: it owns workflow state, worktrees, retries, gates, artifacts, Git operations, and human approval. External CLIs are single-shot delegates invoked through the bus. There is exactly one shipped workflow named `build`. Do not call it by any other name and do not use `-ui` workflow variants.

## Flow

The `build` workflow runs as a parent that forks per-packet children:

1. `draft` normalizes the proposal or bare request. Corrections return to `draft`.
2. `prep` delegates to Luna, which prepares a bounded `ContextBrief` (relevant files and symbols, interfaces, constraints, prior decisions, risks, open questions, acceptance requirements, artifact references, capped at 32 KiB). An invalid or oversized brief returns to `prep` without premium spend.
3. `plan` delegates to Fable (premium, role `draft`, persona `architect`) which authors the parent plan from the admitted proposal and the validated brief. `max_rounds` 6.
4. `plan_gate` reviews the plan with the `plan` preset: seats Sol as reviewer and Antigravity as QA, Sol is chairman, Antigravity is availability-only chairman fallback, no Fable self-review. `min_successful` 2, `quorum` 4, `max_rounds` 6. Changes return to `plan`.
5. `split` delegates to Fable which decomposes the approved plan into immutable packets with schema version 2. Each packet has `schema_version: 2` and `implementation_kind: general|ui` and carries no model, delegate, CLI, or workflow identifiers. Mixed work splits into independent UI and general packets when possible; a packet that must contain both uses `ui` when a user-visible UI outcome is part of the deliverable, otherwise `general`. Classification is by requested outcome, never by filenames or implementation path.
6. `slices` runs `foreach.workflow` with `workflow: slice` for each packet. The shared `slice` child implements with deterministic routing: `general` routes to `muse` as primary with Luna as availability-only fallback; `ui` routes to `opus-ui`. The router does not heuristically reinterpret `implementation_kind` on retry or replay. Each slice runs `scope` (understand without user input) then `impl` then `freeze` then `rt_gate` (implementation preset) then `pr.open` (base `feature`) then `gate.ci` then `merge` into the parent feature branch. Verification is deterministic and a failure returns to `impl` for a bounded repair.
7. `accept_freeze` and `accept_gate` freeze the assembled feature branch and review it with the `implementation` preset. `document` and `doc_freeze` and `doc_gate` do the same for documentation with the `documentation` preset. Both presets use seats Sol as reviewer and Antigravity as QA with optional Fable as architect, Fable as chairman with Sol as availability-only chairman fallback, `min_successful` 2. `on_fail` returns to `split` for acceptance and to `document` for documentation. Documentation updates user and developer docs and inline comments only when the accepted implementation needs them.
8. `archive` retires the trigger proposal and `final_pr` opens a draft PR from the feature branch into the admitted checkout branch (`base: trunk`). The final PR is draft for human review and merge.

Roundtables are discussion-free and bounded by `max_rounds` per gate. Reviewer commentary reaches the PR as inline review comments at file and line when possible. Forge credentials are stripped from delegate children; the engine posts review comments.

## Seats and routing

| Seat | CLI | Purpose | Access |
|------|-----|---------|--------|
| `fable` | Claude CLI (`claude`) | premium planner: architecture, decomposition, contracts, invariants | read-only |
| `sol` | Codex CLI (`codex`) | premium reviewer and chairman fallback | read-only |
| `luna` | Codex CLI (`codex`) | ContextBrief preparation, general availability fallback | code and execute only for fallback |
| `muse` | OpenCode ACP (`opencode acp`) | primary general implementer | isolated writable worktree |
| `opus-ui` | Claude CLI (`claude`) | UI implementer (`implementation_kind: ui`) | isolated writable worktree |
| `antigravity` | Antigravity CLI (`agy`) | initial PR reviewer, QA seat, chairman fallback | read-only |
| `sol-review` | Codex CLI (`codex`) | verifying reviewer | read-only |

### Muse seat - exact configuration

The general implementation seat is named exactly `muse`:

- `name`: `muse`
- `backend`: `provider-cli`
- `cli_kind`: `acp`
- `cli_cmd`: `/home/midnight/.local/bin/opencode acp`
- `model`: `opencode-go/muse-spark-1.2-contributor`
- `reasoning_effort`: `xhigh`
- `roles`: `draft`, `code`, `execute`
- `timeout_ms`: `1800000`
- `cli_idle_timeout_ms`: `1800000`
- `tier_price_exempt`: `flat-rate subscription seat`
- `enabled`: `true`, `tools_enabled`: `true`

The 30-minute idle deadline (`1800000` ms) intentionally permits implementation longer than five minutes. The `timeout_ms` value is the per-call bound and the idle timeout is the ACP session bound; both are set to `1800000`.

The ACP adapter pins the model with `session/set_model` after `session/new` and then pins the effort with `session/set_config_option` where `configId` is `effort` and `value` is `xhigh`. Either refusal is exact-or-fail: the dispatch fails instead of silently substituting a default model or effort.

### Luna availability fallback

Luna carries `code` and `execute` roles only so it can serve the explicit general availability fallback. It is never the normal general implementer. When `muse` fails before response start with one of the six typed pre-response availability classes, the engine retries the identical prompt on Luna. The six classes are `quota_rate_limit`, `capacity`, `capacity_deadline`, `authentication_session`, `provider_cli_unavailable`, and `start_deadline`. The retry does not run after response start, does not run on replay, and does not reinterpret the packet.

### UI routing

`opus-ui` is the normal route for `implementation_kind: ui`. `opus-ui-recovery` remains explicit recovery only when a workflow or operator explicitly mentions it. Do not route UI work to `muse` and do not route general work to `opus-ui`.

`opus-ui` and `opus-ui-recovery` use `timeout_ms: 1800000`,
`cli_idle_timeout_ms: 1800000`, and `max_turns: 0`. Do not add Claude's
`--max-turns` flag: UI implementation may legitimately need more than twelve
turns, while the 30-minute call and idle deadlines remain the bounded stop.

### Shared brain

Every routed model must be able to call Aimee memory so delegates can recall
the shared vector context instead of repeating it in prompts. Claude dispatches
receive the `aimee` MCP server explicitly, and ACP sessions receive the same
`aimee mcp-serve` server in `session/new`. Codex uses the installed Aimee plugin;
Antigravity must register `aimee mcp-serve` in its user MCP configuration.
Deployment validation must ask each enabled seat to retrieve a fact with
`memory recall` whose value is absent from the prompt, and confirm the returned
memory id or retrieval event as well as the answer.

## Premium policy

Premium write refusal applies to all premium seats. A premium delegate dispatched with write tools or role `code` is refused before dispatch with `premium_write_refused` and the run parks for operator fix of the workflow definition.

Only premium `draft` calls consume the durable planning ledger (`wfe_premium_call`). Repeated read-only reviews, including roundtable chairman and reviewer calls, do not consume ledger capacity. The default planning cap is `15`, which covers at most six Fable plan attempts plus three split generations each with at most three corrective attempts (`6 + 3*3 = 15`). Environment overrides remain:

- `AIMEE_PREMIUM_DELEGATES` (default `sol,fable`, comma-separated, `none` disables enforcement)
- `AIMEE_PREMIUM_CALL_CAP` (default `15`)

Aliases remap at dispatch time (`AIMEE_DELEGATE_ALIASES=fable=sol` or per-run `delegate_aliases`). The ledger charges the delegate that actually runs. A malformed or oversized `ContextBrief` is rejected before any premium dispatch and loops to `prep`.

## Source definitions

Repository source definitions are:

- `config/workflows/build.yaml`
- `config/workflows/slice.yaml`
- `config/roundtables/plan.json`
- `config/roundtables/implementation.json`
- `config/roundtables/documentation.json`

Installed copies under `$AIMEE_HOME/workflows` and `$AIMEE_HOME/roundtables` must match these files exactly. The container seeds them on start. Validate with `aimee workflow validate` and `diff` against the installed copies.

### Roundtable presets

`plan` preset (`config/roundtables/plan.json`):

- seats: `codex:gpt-5.6-sol` as `reviewer`, `antigravity` as `qa`
- chairman: `codex:gpt-5.6-sol`, chairman fallback: `antigravity`, enabled, `min_successful` 2

`implementation` and `documentation` presets (`config/roundtables/implementation.json`, `config/roundtables/documentation.json`):

- seats: `codex:gpt-5.6-sol` as `reviewer`, `antigravity` as `qa`, optional `claude:claude-fable-5` as `architect`
- chairman: `claude:claude-fable-5`, chairman fallback: `codex:gpt-5.6-sol`, enabled, `min_successful` 2

## Setup and auth

Authenticate as the OS user that runs `aimee-server`, because delegates inherit that home directory.

```bash
codex login          # ChatGPT subscription; verify: codex doctor
claude               # sign in once; verify: claude -p "hi"
opencode auth login  # verify: opencode models
agy                  # launch with no arguments to sign in; verify: agy models
```

Give the Antigravity review seat read-only repo access. In `~/.config/antigravity/settings.json` set `permissions.allow` to `read_file(*)`, `list_dir(*)`, `grep_search(*)`, `find_by_name(*)`. Writes and commands stay denied; the `agy` adapter never passes `--dangerously-skip-permissions` and slash commands are disabled.

Discover exact model ids from each CLI vendor before pinning them in the agent file.

### Agent file

The file is `$AIMEE_HOME/models.json` (fallback `$AIMEE_HOME/agents.json` for pre-rename installs). It is an object with a `models` array. Example fragment showing the Muse seat:

```json
{
  "models": [
    {
      "name": "muse",
      "backend": "provider-cli",
      "cli_kind": "acp",
      "cli_cmd": "/home/midnight/.local/bin/opencode acp",
      "model": "opencode-go/muse-spark-1.2-contributor",
      "reasoning_effort": "xhigh",
      "roles": ["draft", "code", "execute"],
      "timeout_ms": 1800000,
      "cli_idle_timeout_ms": 1800000,
      "tier_price_exempt": "flat-rate subscription seat",
      "enabled": true,
      "tools_enabled": true
    },
    {
      "name": "luna",
      "backend": "provider-cli",
      "cli_kind": "codex",
      "cli_cmd": "codex",
      "model": "gpt-5.6-luna",
      "reasoning_effort": "xhigh",
      "roles": ["code", "execute"],
      "tier_price_exempt": "flat-rate subscription seat",
      "enabled": true
    },
    {
      "name": "opus-ui",
      "backend": "provider-cli",
      "cli_kind": "claude",
      "cli_cmd": "claude",
      "model": "opus",
      "reasoning_effort": "high",
      "roles": ["code", "execute"],
      "max_turns": 0,
      "timeout_ms": 1800000,
      "cli_idle_timeout_ms": 1800000,
      "tier_price_exempt": "flat-rate subscription seat",
      "enabled": true
    }
  ]
}
```

Add `fable`, `sol`, `antigravity`, and `sol-review` entries similarly with their models and `review` roles. Write capability is derived per call from the role; premium seats must not carry `code` or `execute`.

Claude terms: set `primary_only: false` deliberately. Check the provider terms for unattended use before running a Claude seat as a delegate.

## Validation

```bash
# Docs and links
make -C src docs-check
python3 scripts/check-docs.py

# Workflow and roundtable shape
aimee workflow validate
diff -u config/workflows/build.yaml "$AIMEE_HOME/workflows/build.yaml"
diff -u config/workflows/slice.yaml "$AIMEE_HOME/workflows/slice.yaml"
diff -u config/roundtables/plan.json "$AIMEE_HOME/roundtables/plan.json"
diff -u config/roundtables/implementation.json "$AIMEE_HOME/roundtables/implementation.json"
diff -u config/roundtables/documentation.json "$AIMEE_HOME/roundtables/documentation.json"

# ACP and seat checks
opencode models | grep muse-spark-1.2-contributor
codex exec -m gpt-5.6-luna "Reply with exactly: luna-ok"
agy -p "Reply with exactly: agy-ok" --output-format stream-json
aimee memory recall --query "shared brain verification phrase" --limit-tokens 1000

# Canary assertions on this guide
grep -q muse-spark-1.2-contributor docs/SOFTWARE_FACTORY.md
grep -q xhigh docs/SOFTWARE_FACTORY.md
grep -q 1800000 docs/SOFTWARE_FACTORY.md
grep -q implementation_kind docs/SOFTWARE_FACTORY.md
grep -q '"name": "plan"' config/roundtables/plan.json
grep -q '"name": "implementation"' config/roundtables/implementation.json
grep -q '"name": "documentation"' config/roundtables/documentation.json
```

## Canary expectations

Validate slice routing with small proposals:

- Backend general work: a packet with `implementation_kind: general` is implemented by `muse` (primary) and produces one slice PR that passes CI and merges into `aimee/feat/<parent>`. On pre-response availability failure of `muse`, the same prompt retries on `luna` with identical persona and tools.
- UI work: a packet with `implementation_kind: ui` is implemented by `opus-ui` and follows the same `freeze` then `rt_gate` then `pr` then `ci` then `merge` path. General fallback does not apply to UI packets.
- Mixed dependent work: a split that emits one `general` and one `ui` packet where the UI packet depends on the general one. The general slice merges first, the UI slice integrates `aimee/feat/<parent>` at its next `impl` entry, and both slices converge before `accept_freeze`. Mixed work does not heuristically reclassify packets on retry or replay.

## Security notes

Every delegate runs with forge and git credentials stripped. The only Git route is aimee's own tooling. The ACP adapter refuses `fs/write_text_file` and rejects permission prompts for read-only roles. Antigravity is headless print mode with auto-denied permission-gated tools. Nested delegation is refused: `subagent_ban_enabled` guards aimee's tool surface, Claude argv disallows `Task` and `Agent`, and `max_delegation_depth` bounds the chain.

## Troubleshooting

| Symptom | Meaning | Action |
|---------|---------|--------|
| park `premium_cap` | run tree spent its planning allowance | finish on standard seats, or raise `AIMEE_PREMIUM_CALL_CAP` and resume |
| park `premium_write_refused` | workflow pinned a premium delegate on a write node | fix the workflow definition |
| park `retry_limit` at `prep` | Luna kept returning an invalid `ContextBrief` | inspect the node artifact; adjust the request or seat model |
| `premium planning input rejected` in plan detail | brief was malformed or over 32 KiB | run loops to `prep` without premium spend |
| `agent did not accept pinned model` | ACP peer refused `session/set_model` | re-check `opencode models` for the exact id |
| `agent did not accept reasoning effort` | ACP peer refused `session/set_config_option` with `configId` `effort` | verify the CLI supports effort `xhigh` |
| `agy` returns `Please sign in` | Antigravity has no cached login | run `agy` interactively once |
| everything parks `runner_unavailable` | delegate CLI missing from PATH of `aimee-server` | install the CLI for the service user |

## Manual smoke tests

```bash
# Codex seats respond on subscription
codex exec -m gpt-5.6-luna "Reply with exactly: luna-ok"
codex exec -m gpt-5.6-sol  "Reply with exactly: sol-ok"

# Claude seat responds
claude -p "Reply with exactly: fable-ok"

# OpenCode serves the pinned Muse model over ACP
opencode models | grep muse-spark-1.2-contributor

# Antigravity responds in headless stream-json
agy -p "Reply with exactly: agy-ok" --output-format stream-json

# Factory dry run: admit a small request on build
# Watch it reach the draft PR, then check ledger:
# sqlite3 $AIMEE_HOME/db1.sqlite 'SELECT COUNT(*) FROM wfe_premium_call;'
```
