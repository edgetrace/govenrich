# Agent C — MCP Stub & Claude Desktop Integration Harness

## Role

You build a **disposable** stub MCP server whose only job is to prove the
Claude Desktop integration path works on this machine before Agents A and
B finish. You also draft the user-facing Claude Desktop config and the
demo prompts so the user can rehearse against the stub while A/B code.

You are **not** implementing GovEnrich business logic. Your stub returns
hardcoded fake data that mimics `EnrichOutput`'s shape so rehearsal looks
realistic. When Agents A and B ship, the stub is deleted.

## Why this exists

MCP integration bugs surface at the seam with Claude Desktop, not in Go
code. The highest-probability failures on demo day are:

1. **Stdout corruption** — a stray `fmt.Println` to stdout kills the
   JSON-RPC channel and the tool panel silently shows nothing.
2. **Config file syntax** — `claude_desktop_config.json` is unforgiving
   about trailing commas, missing fields, relative paths.
3. **Protocol version mismatch** — the pinned SDK and Claude Desktop
   must agree on `protocolVersion`.
4. **Quit-not-close** — Claude Desktop only spawns MCP subprocesses on
   a full `⌘Q` relaunch, not a window close. First-time users miss this.
5. **Stale tool list** — Claude Desktop caches tool schemas; rebuilding
   the binary does not invalidate the cache.

A stub that does nothing but register one tool and return fake JSON
exercises all five. If it works, A's real server will work (modulo its
own bugs). If it does not, we debug the integration path on trivial
code, not on A's production logic.

## Phase II scope note

This is a throwaway verification harness — not a Phase II deliverable.
Agents A and B cover the real `enrich_gov_agency`. Your stub exists to
de-risk their integration and unblock demo rehearsal. Delete after A's
server is confirmed working in Claude Desktop.

## Files you own (exclusive write access)

All inside a new, isolated directory:

- `stub/` (new directory)
  - `stub/go.mod` (new module — not the main `govenrich` module)
  - `stub/go.sum`
  - `stub/main.go`
  - `stub/README.md` (two-paragraph explainer + cleanup instructions)
- `demo/` (new directory)
  - `demo/claude_desktop_config.example.json` — copy-paste target for the
    user, with both stub and (commented-out) real server entries
  - `demo/prompts.md` — verbatim demo prompts to rehearse

Do not touch the main module's `go.mod`, `main.go`, `apollo/`, `public/`,
`tools/`, or any file outside `stub/` and `demo/`. Agents A and B never
need to read or edit your files.

## Tasks (in order)

### 1. Initialize the stub module.

```
mkdir -p stub demo
cd stub
go mod init github.com/edgetrace/govenrich/stub
go list -m -versions github.com/modelcontextprotocol/go-sdk
```

Pick the latest non-prerelease tag (pick independently — you do not need
to match Agent A's pin since your module is separate). `go get` that tag.

### 2. Write the stub server.

`stub/main.go`:

- One imported struct pair, `HelloInput { Name string }` and
  `HelloOutput { Greeting, Agency string, SwornOfficers int, Note string }`.
  Use the same `SwornOfficers` field name Agent B will use so rehearsal
  muscle memory carries over.
- `jsonschema:"..."` tags on every input field. Include an example in the
  description — this is the prompt the model sees.
- One tool registered: `enrich_gov_agency_stub` (NOT `enrich_gov_agency`
  — name collision with Agent A's eventual server would confuse the
  model during rehearsal and at demo time if both happen to be registered).
- Handler returns fake data keyed off input: for `Name="Pleasanton"`,
  return `SwornOfficers: 70, Agency: "Pleasanton Police Department",
  Note: "STUB — replace with govenrich when shipping"`. For anything
  else, return a generic canned response. Do not call external APIs.
- `srv.Run(context.Background(), &mcp.StdioTransport{})`.

### 3. Stdout hygiene (mandatory).

- **Zero writes to stdout** in the stub. Not even a startup banner.
- If you must log, `fmt.Fprintln(os.Stderr, "stub: ...")`.
- Do not call `godotenv.Load()` — the stub needs no env vars.
- If you add any debug print, ask yourself: is this stdout? Delete it.

### 4. Build and smoke-test without Claude Desktop first.

From `stub/`:

```
go build -o govenrich-stub
(echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"<SDK default>","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}';
 echo '{"jsonrpc":"2.0","method":"notifications/initialized"}';
 echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}') | ./govenrich-stub
```

Expect two clean JSON-RPC responses on stdout. Any other output = stdout
contamination somewhere; find and fix before touching Claude Desktop.

For `<SDK default>`, grep the SDK for the protocol version constant
(`LatestProtocolVersion` or equivalent) and mirror its value. Record the
value you used in `stub/README.md` so Agent A can match.

### 5. Draft the Claude Desktop config.

`demo/claude_desktop_config.example.json`:

```json
{
  "mcpServers": {
    "govenrich-stub": {
      "command": "/ABSOLUTE/PATH/TO/edgetrace-gtm/stub/govenrich-stub"
    },
    "_govenrich_real_pending": {
      "_comment": "Uncomment and rename to govenrich when Agent A ships",
      "command": "/ABSOLUTE/PATH/TO/edgetrace-gtm/govenrich",
      "env": {
        "APOLLO_API_KEY": "REPLACE_ME",
        "FBI_CDE_API_KEY": "REPLACE_ME"
      }
    }
  }
}
```

Do not write to the user's real config file at
`~/Library/Application Support/Claude/claude_desktop_config.json`. Hand
the example to the user and document the copy/paste step in
`stub/README.md`.

### 6. Verify end-to-end in Claude Desktop.

Walk the user through:

1. Copy the stub entry from `demo/claude_desktop_config.example.json` into
   their real config. Replace the `/ABSOLUTE/PATH/...` placeholder.
2. Fully quit Claude Desktop (`⌘Q`, not just close the window).
3. Relaunch. Open the tool panel (hammer/slider icon, lower-right of the
   composer). Confirm `govenrich-stub` appears with `enrich_gov_agency_stub`
   listed.
4. Prompt: *"Use govenrich-stub to look up Pleasanton."* Claude should
   call the tool and render the fake JSON in an expandable panel.

If step 3 fails, check `~/Library/Logs/Claude/mcp*.log`. Record whatever
error you find in `stub/README.md` under a "Troubleshooting" section so
Agent A hits the same issues with a written playbook.

### 7. Draft demo prompts.

`demo/prompts.md` — three short prompts, verbatim, one per line, no
commentary:

1. The happy path: agency name + state that returns populated data.
2. The null-gap reveal: an agency where Apollo would return null — let
   the model narrate the contrast.
3. Scoring or follow-up (optional stretch): `draft_gov_outreach`-style
   ask that works against the stub's canned response.

Write these so the same prompts will work unchanged once Agent A/B ship
and the real `enrich_gov_agency` replaces `enrich_gov_agency_stub`. (The
user will just update the tool name in their mental script.)

### 8. Cleanup plan in `stub/README.md`.

Document the two-line delete procedure for when A's server is live:

```
rm -rf stub demo/claude_desktop_config.example.json
# then remove the govenrich-stub entry from ~/Library/Application Support/Claude/claude_desktop_config.json
```

## Definition of done

- `stub/go.mod` exists and pins a specific SDK version.
- `./stub/govenrich-stub` builds and produces a clean `tools/list`
  response from the JSON-RPC pipe smoke test.
- `govenrich-stub` appears in the Claude Desktop tool panel after a full
  relaunch.
- The user can invoke the stub via natural-language prompt and see the
  tool-call panel render fake `SwornOfficers` + `Note` fields.
- `demo/claude_desktop_config.example.json` has both the stub entry and
  a commented placeholder for the real binary.
- `demo/prompts.md` has three demo prompts that will work unchanged with
  Agent A's real server (modulo the tool name swap).
- `stub/README.md` documents the protocol version used, any
  troubleshooting findings from step 6, and the cleanup procedure.

## Non-goals

- Do not call Apollo, FBI, USASpending, or Census. This is a
  connectivity harness, not an enrichment demo.
- Do not share code with the main module. Separate `go.mod` = separate
  blast radius.
- Do not try to match Agent B's exact output schema. Close enough for
  rehearsal (`SwornOfficers` is the one shared field name); full
  fidelity is Agent B's job.
- Do not write to the user's real Claude Desktop config. Hand them an
  example; let them copy.

## Estimated time

15-20 minutes total. If the Claude Desktop integration takes longer
than 20 minutes, stop and report what's blocking — Agent A will hit the
same wall and needs to know.

## Status — 2026-04-16

Executed by automation. All code-side deliverables done; two DoD items
require the user to drive the Claude Desktop GUI.

**Done:**

- `stub/go.mod` created as a separate module
  (`github.com/edgetrace/govenrich/stub`); pins
  `github.com/modelcontextprotocol/go-sdk` at **v1.5.0** (latest
  non-prerelease at time of execution).
- `stub/main.go` registers one tool, `enrich_gov_agency_stub`, with
  `HelloInput{Name}` and `HelloOutput{Greeting, Agency, SwornOfficers,
  Note}`. `SwornOfficers` field name matches the shape Agent B will use.
  Pleasanton returns `SwornOfficers: 70, Agency: "Pleasanton Police
  Department"`; everything else returns a generic canned response.
- Stdout hygiene verified: zero writes to stdout in the code path; the
  single error branch writes to `os.Stderr` only. No `godotenv.Load()`.
- `stub/govenrich-stub` builds clean (arm64) at
  `/Users/admin/edgetrace-gtm/stub/govenrich-stub`.
- JSON-RPC pipe smoke test passes — `initialize` → `2025-11-25`,
  `tools/list` returns `enrich_gov_agency_stub` with full
  input+output schema, `tools/call name=Pleasanton` returns
  `sworn_officers=70`. No stderr noise.
- Protocol version used: `2025-11-25` (mirrors `latestProtocolVersion`
  in `mcp/shared.go` at v1.5.0). Recorded in `stub/README.md` for
  Agent A to match.
- `demo/claude_desktop_config.example.json` written with stub entry +
  commented-out real `govenrich` placeholder carrying `APOLLO_API_KEY`
  and `FBI_CDE_API_KEY` env vars.
- `demo/prompts.md` has three rehearsal prompts: (1) happy path
  Pleasanton, (2) null-gap reveal on a small-town department, (3)
  follow-up cold-outreach draft. Prompts are phrased so the same text
  works unchanged when Agent A's real `enrich_gov_agency` replaces the
  stub.
- `stub/README.md` documents SDK pin, protocol version, build + smoke
  test, Claude Desktop wire-up, troubleshooting playbook, and the
  two-line cleanup procedure.
- Zero writes outside `stub/` and `demo/`. The concurrent modifications
  seen to `go.mod`, `go.sum`, `main.go`, and `tools/enrich_gov_agency.go`
  during execution are Agent A's and Agent B's in-progress work, not
  mine.

**Smoke-test transcript (abbreviated):**

```
→ initialize          ← serverInfo govenrich-stub/0.0.1, protocolVersion 2025-11-25
→ notifications/initialized
→ tools/list          ← enrich_gov_agency_stub (input+output schema, sworn_officers present)
→ tools/call          ← structuredContent {agency: "Pleasanton Police Department",
                                           sworn_officers: 70,
                                           note: "STUB — replace with govenrich when shipping"}
```

**Pending user action (step 6 in the spec):**

1. Copy the `govenrich-stub` block from
   `demo/claude_desktop_config.example.json` into
   `~/Library/Application Support/Claude/claude_desktop_config.json`,
   replacing `/ABSOLUTE/PATH/TO/edgetrace-gtm/stub/govenrich-stub` with
   `/Users/admin/edgetrace-gtm/stub/govenrich-stub`.
2. Fully quit Claude Desktop (`⌘Q`, not window close) and relaunch.
3. Confirm `govenrich-stub` appears in the tool panel with
   `enrich_gov_agency_stub` listed, then run prompt 1 from
   `demo/prompts.md`.

If step 3 surfaces an error, tail
`~/Library/Logs/Claude/mcp*.log` and record the finding under
`stub/README.md` → Troubleshooting so Agent A inherits a written
playbook (as the spec instructs).
