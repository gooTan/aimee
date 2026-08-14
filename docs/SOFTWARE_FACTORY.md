# Subscription software factory

Two shipped workflows, `quick-change` and `orchestrated-change`, turn aimee
into a software factory that runs on the flat-rate subscriptions of locally
authenticated coding CLIs. Aimee remains the only orchestrator: it owns
workflow state, worktrees, retries, gates, artifacts, Git operations, and
human approval. The external CLIs are dumb, single-shot delegates.

## Seats

| Seat | CLI | Model | Role | Access |
|------|-----|-------|------|--------|
| `fable` | Claude CLI (`claude`) | the CLI's default subscription model | premium planner: architecture, decomposition, contracts, invariants, acceptance criteria | read-only |
| `sol` | Codex CLI (`codex`) | `gpt-5.6-sol` | premium second opinion: plan review, difficult debugging, escalated decisions | read-only |
| `luna` | Codex CLI (`codex`) | `gpt-5.6-luna` | context preparation (ContextBrief), frozen-diff review, summarization | read-only |
| `deepseek` | OpenCode (`opencode acp`) | `opencode-go/deepseek-v4-flash` | primary implementation and routine repair | isolated writable worktree |
| `antigravity` | Antigravity CLI (`agy`) | pick one from `agy models` (a Gemini 3.x model) | Gemini reviewer in the pro ladder; optional escalation seat | read-only by role |
| `oracle` | Oracle CLI (`oracle`, ChatGPT web) | `gpt-5.6` with `--browser-thinking-time pro` (GPT-5.6 Pro) | frontier reviewer in the pro ladder; optional adjudicator | consultation only: no tools, no worktree |

Every seat uses the CLI's own login state. Aimee never extracts or proxies
OAuth tokens; the child process reads the CLI's normal credential store from
the server user's home directory. Forge and git credentials are stripped from
delegate children as usual, so a delegate's only route to Git is aimee's own
tooling.

## The two workflows

### quick-change

```
prep (luna) -> implement (deepseek) -> freeze -> review (luna) -> human gate -> deliver
```

For routine, well-understood changes. It makes zero premium calls by
construction, and the engine's premium ledger would refuse one anyway.
Deterministic verification (`aimee git verify`) runs inside the implement
block after every delegate pass; a failure feeds the diagnostic straight back
to DeepSeek for at most two bounded repair rounds. Luna's review findings take
the same path. Delivery stops at a human gate.

### orchestrated-change

```
prep: luna prepares a typed ContextBrief
plan: ONE premium call (fable) authors the implementation plan
implement (deepseek) -> freeze -> review (luna)
   routine findings        -> back to deepseek (bounded)
   escalation-class finding -> second_opinion (sol), at most once
pr: draft pull request -> human gate -> deliver
```

The premium planner receives exactly two things: the immutable original
request and a validated ContextBrief (relevant files and symbols, interfaces,
constraints, prior decisions, risks, open questions, acceptance requirements,
artifact references; hard cap 32 KiB). Full repository listings, raw logs,
complete diffs, and conversation history do not fit the schema and are
rejected before any premium dispatch.

Sol runs only when Luna's review classifies a blocking finding as a genuine
`architecture`, `security`, `migration`, `contract`, or `requirement`
decision (the `on_escalate` edge). Any other label degrades to the routine
repair path. Sol and Fable are never both called automatically on one path.

### orchestrated-change-pro

```
prep (luna brief) -> plan (fable, one capped call) -> implement (deepseek)
  -> freeze -> review ladder, cheapest first:
       luna -> oracle (ChatGPT web) -> gemini (Antigravity)
  -> draft PR -> human gate -> deliver
```

The fully autonomous iterate-until-clean variant. Findings from any rung of
the ladder return directly to the DeepSeek implementer; the repaired diff is
re-verified and climbs the whole ladder again. The draft pull request cannot
open until luna, oracle, and gemini have each approved the same frozen diff
with zero findings, so the human gate is only ever presented with a fully
converged change. Oracle runs before Gemini because it is the cheaper seat.

Oracle and Antigravity are flat-rate consultation seats: they are bounded by
each node's `max_rounds` (four) and the engine's no-progress convergence
detection rather than the sol/fable premium ledger, because iterate-until-
clean requires repeated review rounds. The planning ledger still caps fable
plus at most one sol escalation at two calls.

Oracle integration boundary: aimee writes the complete review prompt to a
temporary task file, runs `oracle -p <pointer> -f <task-file> --write-output
<answer-file>`, and reads back only the final assistant message. Oracle never
receives tools, a worktree, credentials, or conversation history; it is a
provider adapter, and aimee remains the workflow controller.

## Premium-call limits

The engine keeps a durable per-run-tree ledger (`wfe_premium_call` in DB1).
Before any dispatch of a premium delegate the runner records the call inside a
transaction that enforces the cap; exceeding it parks the run with pause
reason `premium_cap` for a human. A premium delegate dispatched with write
tools is refused outright (`premium_write_refused`). Deterministic checks,
not model claims, decide whether verification passed.

Configuration (environment of `aimee-server`):

| Variable | Default | Meaning |
|----------|---------|---------|
| `AIMEE_PREMIUM_DELEGATES` | `sol,fable` | comma-separated delegate names that count as premium; `none` disables enforcement |
| `AIMEE_PREMIUM_CALL_CAP` | `2` | hard dispatch ceiling per workflow run tree |

## Setup

### 1. Authenticate the CLIs (one time, interactive)

```bash
codex login          # ChatGPT subscription; verify with: codex doctor
claude               # sign in once; verify with: claude -p "hi"
opencode auth login  # verify with: opencode models
agy                  # launch with no arguments to sign in; verify with: agy models

# Oracle (ChatGPT web) keeps its own Chrome profile; sign in there once:
npm install -g @steipete/oracle
oracle --engine browser --browser-manual-login --browser-keep-browser -p "HI"
```

Sign in as the same OS user that runs `aimee-server`, because delegates
inherit that home directory.

### 2. Discover the exact model ids

```bash
codex exec -m gpt-5.6-luna "say ok"      # premium tier list comes from your plan
opencode models | grep deepseek           # for example opencode-go/deepseek-v4-flash
agy models                                # pick a reviewer model
```

Pinned means exact-or-fail: a delegate configured with a model the CLI cannot
serve fails the dispatch instead of silently substituting.

### 3. Configure the agents

Add the seats to `$AIMEE_HOME/agents.json` (or use `aimee agent add`). The
shipped workflows reference the delegate names below; keep them.

```jsonc
[
  {
    "name": "fable",
    "backend": "provider-cli",
    "cli_kind": "claude",
    "cli_cmd": "claude",
    "roles": ["draft", "review", "explain"],
    "primary_only": false,
    "tier_price_exempt": "flat-rate subscription seat",
    "enabled": true
  },
  {
    "name": "sol",
    "backend": "provider-cli",
    "cli_kind": "codex",
    "cli_cmd": "codex",
    "model": "gpt-5.6-sol",
    "roles": ["review", "draft", "diagnose"],
    "tier_price_exempt": "flat-rate subscription seat",
    "enabled": true
  },
  {
    "name": "luna",
    "backend": "provider-cli",
    "cli_kind": "codex",
    "cli_cmd": "codex",
    "model": "gpt-5.6-luna",
    "roles": ["draft", "review", "explain", "search"],
    "tier_price_exempt": "flat-rate subscription seat",
    "enabled": true
  },
  {
    "name": "deepseek",
    "backend": "provider-cli",
    "cli_kind": "acp",
    "cli_cmd": "opencode acp",
    "model": "opencode-go/deepseek-v4-flash",
    "roles": ["code", "execute"],
    "cli_idle_timeout_ms": 600000,
    "tier_price_exempt": "flat-rate subscription seat",
    "enabled": true
  },
  {
    "name": "antigravity",
    "backend": "provider-cli",
    "cli_kind": "agy",
    "cli_cmd": "agy",
    "model": "gemini-3.1-pro-high",
    "roles": ["review", "explain"],
    "cli_idle_timeout_ms": 1800000,
    "tier_price_exempt": "flat-rate subscription seat",
    "enabled": true
  },
  {
    "name": "oracle",
    "backend": "provider-cli",
    "cli_kind": "oracle",
    "cli_cmd": "oracle -e browser --browser-thinking-time pro",
    "model": "gpt-5.6",
    "roles": ["review", "explain"],
    "cli_idle_timeout_ms": 2700000,
    "tier_price_exempt": "flat-rate subscription seat",
    "enabled": true
  }
]
```

Notes:

- Write capability is derived per call from the role, never trusted from
  disk. Only `deepseek` carries write roles; the engine additionally refuses
  any write-capable dispatch of a premium delegate.
- The ACP adapter pins the model with `session/set_model` after `session/new`
  and fails the dispatch if the agent does not accept the pin. For a
  read-only role it also refuses `fs/write_text_file` and answers permission
  prompts with reject or cancel.
- The `agy` adapter never passes `--dangerously-skip-permissions`. In
  headless print mode Antigravity auto-denies every permission-gated tool,
  which seals its subagent tools (`define_subagent`, `invoke_subagent`,
  `browser_subagent`) and mutating tools. Slash-command expansion is disabled
  with `--disable-slash-commands`. Grant nothing under `permissions.allow` in
  agy's settings for this seat.
- Nested delegation is refused everywhere else too: `subagent_ban_enabled`
  guards aimee's own tool surface, the Claude CLI argv disallows
  `Task`/`Agent`, and `max_delegation_depth` bounds the chain.
- Claude terms: set `primary_only: false` deliberately; check the provider's
  terms for unattended use before running Fable as a delegate.

### 4. Pick a workflow

The definitions ship in `config/workflows/` and are seeded to
`$AIMEE_HOME/workflows` on container start. Admit work as usual (watch-dir
trigger, API submit, or browser) and select `quick-change` for routine fixes,
`orchestrated-change` for anything that deserves a plan, or
`orchestrated-change-pro` when the frozen diff must clear the full
luna/oracle/gemini review ladder before you ever see the draft pull request.
All three are `enforced` and end in a human gate. Approve or reject from the browser Workflow Actions page
or `POST /v1/workflow/items/{id}/gate` with `{"decision":"approve","gate":"human_gate"}`.

## Troubleshooting

| Symptom | Meaning | Action |
|---------|---------|--------|
| park `premium_cap` | the run tree spent its premium allowance | finish on standard seats, or raise `AIMEE_PREMIUM_CALL_CAP` and resume |
| park `premium_write_refused` | a workflow pinned a premium delegate on a write node | fix the workflow definition |
| park `retry_limit` at `prep` | Luna kept returning an invalid ContextBrief | inspect the node artifact; adjust the request or the seat's model |
| `premium planning input rejected` in the plan detail | the brief was malformed or over 32 KiB | the run loops back to prep without premium spend; usually resolves itself |
| `agent did not accept pinned model` | ACP peer refused `session/set_model` | re-check `opencode models` for the exact id |
| `agy` returns `Please sign in` | Antigravity has no cached login | run `agy` interactively once |
| oracle: `manual-login profile is not initialized` | Oracle's private Chrome profile has no ChatGPT session | run the one-time `--browser-manual-login` flow from setup step 1 |
| `oracle adapter: exit N with no answer output` | browser automation failed mid-run | check `oracle status`, re-login if needed; the run parks and resumes cleanly |
| oracle reviews park `runner_unavailable` when aimee runs in a container | the container has no browser | run `oracle serve` (or `oracle bridge` from Windows) next to the browser and point `cli_cmd` at the remote engine flags |
| oracle runs GPT-5.5 instead of the current Pro tier | oracle's `gpt-5-pro` alias mis-normalizes (steipete/oracle#373) | pin `"model": "gpt-5.6"` plus `--browser-thinking-time pro` in `cli_cmd`; switch to the `gpt-5-pro` alias once the upstream fix ships |
| oracle runs an unexpected effort tier (for example Pro when you wanted fast) | ChatGPT's Effort picker is sticky account state; oracle pins and verifies the model but leaves effort wherever the last run set it unless told | always pass an explicit `--browser-thinking-time` on every oracle seat: `pro` for the review ladder, `standard` for cheap consultations (valid: light, standard, extended, extra-high, pro, heavy) |
| codex dispatch runs the wrong model | the agent entry has no `model` pin | set the exact id from your plan's model list |
| everything parks `runner_unavailable` | delegate CLI missing from PATH of aimee-server | install the CLI for the service user |

## Manual smoke tests

Each takes under a minute and proves one seam end to end.

```bash
# 1. Codex seats respond on the subscription
codex exec -m gpt-5.6-luna "Reply with exactly: luna-ok"
codex exec -m gpt-5.6-sol  "Reply with exactly: sol-ok"

# 2. Claude seat responds
claude -p "Reply with exactly: fable-ok"

# 3. OpenCode serves the pinned DeepSeek model over ACP
opencode models | grep deepseek-v4-flash

# 4. Antigravity responds in headless stream-json (prompt travels in argv)
agy -p "Reply with exactly: agy-ok" --output-format stream-json

# 4b. Oracle reaches ChatGPT web through its signed-in profile
echo "smoke context" > /tmp/ctx.txt
oracle -p "Reply with exactly: oracle-ok" -f /tmp/ctx.txt -e browser \
  -m gpt-5.5 --no-notify --write-output /tmp/oracle-ok.txt --timeout 240
cat /tmp/oracle-ok.txt

# 5. Factory dry run: admit a small request on quick-change, watch it reach
#    the human gate, approve, and confirm zero premium calls in the ledger:
#    sqlite3 $AIMEE_HOME/db1.sqlite 'SELECT COUNT(*) FROM wfe_premium_call;'
```
