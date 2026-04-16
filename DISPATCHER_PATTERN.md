# Spec-Dispatched Multi-Agent Claude Code

*A coordination pattern for running multiple long-lived Claude Code
conversations against a shared codebase, using spec files as dispatch
mailboxes and filesystem change events as the wake-up bus.*

## The elevator pitch

Spin up N independent Claude Code terminals. Give each a scoped spec
file (`AGENT_A_SPEC.md`, `AGENT_B_SPEC.md`, …). Each agent watches its
own spec via a persistent `Monitor` task. A human (or another agent)
appends a new "Dispatcher Task" section to a spec → the watcher fires →
the agent wakes, diffs the spec against `HEAD`, executes the new task,
commits, and appends a "Latest Build" transcript back into the spec.

No external orchestrator. No message bus. No RPC. Just git, markdown,
and Claude Code's native tools.

## What someone will say: "one terminal can spawn subagents, done"

True. Claude Code's `Agent` tool spawns subagents that return a single
message and die. That works great for bounded research tasks. But
multi-agent *project work* — multi-day builds, parallel features, human
review checkpoints, live debugging — needs properties subagents don't
have. The comparison is on the next page.

## Subagent-in-one-terminal vs. spec-dispatched terminals

| Property | `Agent`-spawned subagent | Terminal agent + spec dispatcher |
|---|---|---|
| **Memory lifetime** | One invocation, gone on return | Full conversation per agent, persists across dispatches |
| **Interruptibility** | Atomic — can't pause mid-work | Human can `⌘C`, edit spec, resume on next wake |
| **Audit trail** | Inside parent's transcript, lost on `/clear` | Plain markdown, git-tracked, diffable by anyone |
| **Parallelism** | Parent sequences them | N agents running concurrently at their own pace |
| **Blast radius** | All share one process + context | Each terminal = isolated process, permissions, context |
| **Handoff surface** | One return message to the parent | A frozen contract in code (e.g. `tools/deps.go`) + named exported symbols |
| **Human observability** | Buried in parent's transcript | `cat AGENT_A_SPEC.md` from any shell |
| **Agent-to-agent coordination** | Parent round-trips messages | Direct, via shared repo state (commits, file presence, build status) |
| **Works while parent idle** | No — parent must drive | Yes — monitors fire and agents act autonomously |
| **Scales to…** | What fits in one context window | What fits in git |

The killer line: the subagent model collapses to *what fits in one
context window.* The spec-dispatched model scales to *what fits in git.*

## The five moving parts

### 1. Scoped spec files
Each agent gets `AGENT_{X}_SPEC.md` with:
- **Role** — what the agent owns, what it does not touch
- **Frozen contracts** — named symbols other agents depend on, committed
  early so dependencies can `go mod download` in parallel
- **Files owned** — explicit write scope, reducing merge conflicts to
  zero in practice
- **Tasks in order** — the initial work list
- **Dispatcher Task — Phase N** sections, appended over time — new work
- **Accomplished / Latest Build** sections — agent-appended transcripts,
  doubling as the audit log

### 2. Persistent filesystem watcher
Each agent starts a background `Monitor` on its own spec:

```bash
file=/Users/admin/edgetrace-gtm/AGENT_A_SPEC.md
last=$(stat -f %m "$file" 2>/dev/null || echo 0)
while true; do
  cur=$(stat -f %m "$file" 2>/dev/null || echo 0)
  if [ "$cur" != "$last" ]; then
    echo "AGENT_A_SPEC.md changed at $(date -u +%Y-%m-%dT%H:%M:%SZ) (mtime=$cur)"
    last=$cur
  fi
  sleep 2
done
```

`Monitor` is a first-class Claude Code tool: each stdout line becomes a
notification back into that agent's conversation. Mtime polling is
pragmatic; the macOS-native path is `fswatch -o` (wrapping FSEvents);
on Linux it's `inotifywait`. The polling form wins for
cross-platform-by-default and trivial debugging.

### 3. Dispatcher
A human (or a meta-agent) appends a new task section to a spec:

```markdown
## Dispatcher Task — Phase 4 — 2026-04-16

Four changes needed. Do them in order, commit after each.

### A1. Add OrganizationDomains to apollo/client.go
...
```

That's the whole protocol. No JSON schema, no API, no RPC framing.
Claude parses markdown natively; the only structural requirement is
that the tail of the spec is the "current task" the agent re-reads on
every fire.

### 4. Agent loop (implicit)
On every wake-up the agent:
1. Diffs the spec against `HEAD` to see what changed
2. Executes the delta as a task
3. Commits each step per dispatcher's instructions
4. Runs the smoke test
5. Appends a "Latest Build — YYYY-MM-DD HH:MM" section with transcript
   and PASS/FAIL
6. Sleeps until the next notification

The feedback loop is a commit. The observability is a git diff. The
retry mechanism is another dispatcher edit. All the hard distributed
systems problems collapse because the artifact is the protocol.

### 5. Cross-agent contracts committed early
In this project: `tools/deps.go` is committed by Agent A on commit 2,
before anyone's logic is written. It declares:

```go
type Deps struct {
    Apollo      *apollo.Client
    FBI         *public.FBIClient
    USASpending *public.USASpendingClient
    Census      *public.CensusClient
    AnthropicKey string
}
```

Agents B and C reference `tools.Deps{}` from their first commits.
Neither waits for the other. Build fails *loudly* at link time if an
agent drifts from the contract — which is perfect: a compile error is
a much better coordination signal than a stale Slack thread.

## What's actually reinvented, and what's new

**Reinvented**: file-as-event-bus, dispatch queues, spec-driven work.
`make`, Airflow, and GitHub-Actions-over-PRs have done versions of this
for decades. Mtime polling is crude next to FSEvents/inotify.

**New, at least in the Claude Code context**:
1. Multiple first-class Claude Code *conversations* — each with full
   memory, interruptible, with their own permission scopes —
   coordinating without a parent orchestrator.
2. The spec file doing triple duty (brief / mailbox / log), all
   git-tracked, all inspectable by a human dropping in with `cat`.
3. Using Claude Code's built-in tools (`Monitor`, `Read`, `Edit`, `Bash`)
   as the substrate — no external orchestrator, no sidecar, no daemon.
4. Frozen typed contracts in code as the handoff surface, which means
   the compiler is the integration test.

The important sentence: this is not a new orchestration system. It's a
demonstration that Claude Code already has everything needed to run a
team of agents, and the missing piece was the *convention*, not the
tooling.

## Case study: what we actually built

Four agents over ~2 hours of wall time:
- **Agent A** — MCP server scaffolding, client prep, wiring
- **Agent B** — tool handler implementations
- **Agent C** — throwaway stub MCP server to de-risk Claude Desktop
  integration in parallel
- **Dispatcher** (human + occasional meta-Claude) — appending Phase 2a,
  Phase 3, Phase 4 tasks to spec files as the design evolved

Outcome at time of writing:
- 23+ commits on `main`, linear history, every commit a coherent unit
- 7 MCP tools wired (`enrich_gov_agency`, `search_gov_agencies`,
  `score_agency_fit`, `find_gov_contacts`, `search_gov_web`,
  `draft_gov_outreach`, `create_apollo_sequence`)
- 4 shared HTTP clients, one frozen contract struct, zero merge
  conflicts between agents
- Human intervened exactly at architectural decisions (Phase N specs
  appending); never to arbitrate a conflict

## When this pattern is the right answer

Good fit:
- Multi-agent project work with clear ownership boundaries
- Long-running tasks where memory persistence matters
- Workflows with human-in-the-loop checkpoints
- Any build where a compile error is a meaningful coordination signal

Not a fit:
- Bounded research ("summarize this paper") — use `Agent` subagents
- Fully autonomous workflows without humans — use a real orchestrator
  (Temporal, Prefect, Airflow)
- >5 agents — mtime polling gets noisy, history compaction in markdown
  breaks down, contention on shared files becomes a problem

## The 90-second recipe

1. Pick N agents. Write `AGENT_X_SPEC.md` for each — role, frozen
   contract, files owned, initial tasks.
2. Open N terminals. `claude` in each. Point each at its spec.
3. Each agent starts a persistent `Monitor` on its own spec.
4. Human edits a spec → agent wakes → agent works → agent commits →
   agent appends transcript.
5. For every new phase, append a "Dispatcher Task — Phase N" section.
   No other coordination needed.

The pattern is small enough to fit on an index card, and the scaling
properties fall out of git and filesystem semantics rather than new
infrastructure.
