# Software factory

The shipped software factory has one parent workflow, `build`, and one child
workflow, `slice`. Aimee owns workflow state, worktrees, gates, artifacts, Git
operations, and retries. Provider CLIs are delegates; they do not operate the
forge directly.

## Build flow

The live `build` graph is:

1. `draft` normalizes the request and `feature` opens the feature branch.
2. `plan` authors the implementation plan. `plan_gate` reviews it with the
   `plan` roundtable and returns requested changes to `plan`.
3. `split` creates implementation packets. `slices` runs the `slice` child for
   every packet.
4. `accept_freeze` freezes the assembled implementation. `gemini_review` runs
   Antigravity and `sol_review` runs the independent Codex review. Either may
   return the work to `split`.
5. `document`, `doc_freeze`, and `doc_gate` update and review documentation.
6. `archive` retires the source request and `final_pr` opens the final PR against
   the repository default branch.

Each `slice` runs `scope`, `impl`, `freeze`, the `implementation` roundtable,
opens a PR against the parent feature branch, waits for green CI, and merges the
slice. Failed review or CI returns to implementation.

Node retry limits remain finite. Model calls, ACP sessions, roundtables, and the
workflow as a whole have no implicit completion deadline. Explicit operator
deadlines remain supported. Runtime heartbeat and last-progress timestamps are
reported separately; a running delegate is classified as `working`,
`suspected_stall`, or `runtime_unresponsive` without being cancelled merely for
being old.

## Seats

The deployed `$AIMEE_HOME/models.json` is authoritative for model IDs and CLI
commands. The workflow uses these named seats:

| Seat | Transport | Workflow use | Access |
|------|-----------|--------------|--------|
| `fable` | Claude CLI | premium planning and configured fallback paths | read-only |
| `luna` | Codex CLI | plan roundtable and implementation fallback | role-scoped |
| `muse` | OpenCode ACP | general implementation | isolated writable worktree |
| `opus-ui` | Claude CLI | UI implementation | isolated writable worktree |
| `antigravity` | Antigravity CLI | implementation/documentation panels and first acceptance review | read-only for review |
| `sol-review` | Codex CLI | second acceptance review | read-only |

ACP model and effort pinning are exact-or-fail. A peer that refuses either pin
cannot silently substitute another model or effort. Write capability is derived
from the dispatched role, and premium write attempts are rejected before model
execution.

Do not put `timeout_ms` or `cli_idle_timeout_ms` in deployed model entries unless
an operator deliberately wants a completion deadline. Absence means unbounded.
`autonomy.max_wall_secs` and roundtable `deadline_ms` follow the same rule.

## Roundtables

The repository presets match the proven live allocation:

- `plan`: Luna reviewer and Luna QA; Luna chairman and fallback.
- `implementation`: Antigravity reviewer, QA, and optional architect;
  Antigravity chairman and fallback.
- `documentation`: the same allocation as `implementation`.
- `default`: Sol reviewer, Antigravity QA, optional Fable architect;
  Antigravity chairman with Sol fallback.

All presets require two successful seats and disable discussion. Workflow gate
`max_rounds` values bound correction loops without imposing a wall-clock limit
on a model call.

## Shared memory

Every routed model must be able to call Aimee memory. Claude and ACP dispatches
receive `aimee mcp-serve`; Codex uses the installed Aimee plugin; Antigravity
must register the same MCP server. Deployment validation must store a unique
fact, retrieve it through each applicable path without including the value in
the prompt, and confirm the returned memory ID or retrieval event.

## Source definitions

- `config/workflows/build.yaml`
- `config/workflows/slice.yaml`
- `config/roundtables/default.json`
- `config/roundtables/plan.json`
- `config/roundtables/implementation.json`
- `config/roundtables/documentation.json`

Installed copies under `$AIMEE_HOME/workflows` and `$AIMEE_HOME/roundtables`
must match these files. Deploy only these valid definitions; obsolete workflow
variants must not remain installed.

## Validation

```bash
make -C src docs-check
python3 scripts/check-docs.py
aimee workflow validate
diff -u config/workflows/build.yaml "$AIMEE_HOME/workflows/build.yaml"
diff -u config/workflows/slice.yaml "$AIMEE_HOME/workflows/slice.yaml"
diff -u config/roundtables/plan.json "$AIMEE_HOME/roundtables/plan.json"
diff -u config/roundtables/implementation.json "$AIMEE_HOME/roundtables/implementation.json"
diff -u config/roundtables/documentation.json "$AIMEE_HOME/roundtables/documentation.json"
```

Run a fresh `build` request after deployment and require the full path through
the slice PR, CI merge, acceptance reviews, documentation gate, and final PR.
Test the configured Fable fallback by forcing only its eligible pre-response
failure condition; a post-response error must not duplicate the turn.
