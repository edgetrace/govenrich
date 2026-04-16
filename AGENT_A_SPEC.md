# Agent A — Server Plumbing, Wire-up & Client Prep

## Role

You own the MCP server scaffolding and a small amount of client prep that
Agent B depends on. Your job is to bolt the `modelcontextprotocol/go-sdk`
onto the existing binary, register one tool, serve it over stdio, and add
the two `public/*` methods Phase 1 left missing so Agent B can finish the
enrichment logic end-to-end.

You do **not** implement tool business logic — Agent B owns that in
`tools/enrich_gov_agency.go`. Your integration surface is a single exported
symbol you call: `tools.NewEnrichHandler(deps)`.

## Phase II scope note

SPEC.md defines four Phase 2 tools (`search_gov_agencies`,
`enrich_gov_agency`, `score_agency_fit`, `draft_gov_outreach`). This
spec pair (Agent A + B) covers only **`enrich_gov_agency`** — the
minimum end-to-end demo. The other three tools will be scoped in a
follow-up spec pair after this one ships.

## Context

- Go module: `github.com/edgetrace/govenrich` (see `go.mod`).
- The binary already has a `--hello-world` connectivity-check mode that
  must keep working untouched.
- The default (no-flag) branch at `main.go:35-38` currently prints a
  placeholder and exits. That branch is yours to replace with the MCP
  server.
- `SPEC.md` is the product spec; `README.md` is the user-facing doc.
- Phase 1 found two upstream issues that affect Agent B — you will fix
  them in client code before B starts:
  1. FBI CDE `byStateAbbr` does not include `sworn_officers`. Getting
     sworn counts requires a per-ORI call to `/pe/agency/{ori}` which
     is not yet wired.
  2. Census `/data/2022/govfinances` returns 404 — Census does not
     publish per-place government finance via API. The correct call
     is unresolved; see SPEC.md §11.

## Files you own (exclusive write access)

- `go.mod`
- `go.sum`
- `main.go`
- `tools/deps.go` (new)
- `public/fbi.go` (additions only — keep `AgenciesByState` intact)
- `public/census.go` (additions only — keep `LocalGovFinance` intact)

Do not touch `tools/enrich_gov_agency.go`, `apollo/*`, or other files in
`public/*`.

## Frozen contract (shared with Agent B)

Drop this in `tools/deps.go` as your first commit. It is the only thing
Agent B depends on from your side:

```go
package tools

import (
    "github.com/edgetrace/govenrich/apollo"
    "github.com/edgetrace/govenrich/public"
)

type Deps struct {
    Apollo      *apollo.Client
    FBI         *public.FBIClient
    USASpending *public.USASpendingClient
    Census      *public.CensusClient
}
```

Agent B will export, and you will call:

```go
tools.NewEnrichHandler(deps) // returns the typed MCP handler
```

`EnrichInput` / `EnrichOutput` types live in Agent B's file. If Agent B
needs an additional dependency (logger, Anthropic client, timeout config),
they will ping you to extend `Deps{}` — that is the only coordination
point.

## Tasks (in order)

### 1. Pin the MCP SDK.

Do not use `@latest`:

```
go list -m -versions github.com/modelcontextprotocol/go-sdk
```

Pick the latest non-prerelease tag, run
`go get github.com/modelcontextprotocol/go-sdk@vX.Y.Z` with that tag, then
commit `go.mod` + `go.sum` immediately so Agent B resolves the same
version. Include the pinned tag in the commit message so it is discoverable
via `git log`.

### 2. Create `tools/deps.go`.

Write exactly the struct above. Commit.

### 3. Add `PoliceEmployeeByORI` to `public/fbi.go`.

Agent B needs sworn officer counts and `byStateAbbr` does not return them.
Add (alongside the existing `AgenciesByState`):

```go
// PoliceEmployeeByORI hits the FBI CDE police-employee endpoint for a
// single agency. Response shape carries sworn_officers (plus civilian
// staff) — this is the real sworn-count source for Phase 2 enrichment.
func (c *FBIClient) PoliceEmployeeByORI(ori string) (int, []byte, error) {
    u := fmt.Sprintf("%s/pe/agency/%s?API_KEY=%s",
        c.BaseURL, url.PathEscape(ori), url.QueryEscape(c.APIKey))
    // ... standard http.Do + ReadAll, same pattern as AgenciesByState
}
```

Test the endpoint's real response shape with `curl` before finalizing the
method — if the field name isn't `sworn_officers`, record the actual key
in a comment so Agent B can match it.

### 4. Stub Census to a documented no-op.

`LocalGovFinance` currently 404s because the SPEC URL does not exist.
Rather than let Agent B write code against a broken endpoint, add a
documented no-op alongside the existing method:

```go
// LocalGovFinanceStub returns an empty result with an explicit reason.
// SPEC.md §11 asks for per-place government finance, which Census does
// not expose via API — Phase 2 will either ingest the 2022 Census of
// Governments downloadable files or drop this data source. Until then,
// this stub is what tool handlers should call.
func (c *CensusClient) LocalGovFinanceStub(_ string) (int, []byte, error) {
    return 0, nil, fmt.Errorf("census govfinances endpoint unavailable — see SPEC.md §11")
}
```

Leave `LocalGovFinance` in place for `--hello-world` visibility. Agent B
will call the stub, not the broken method.

### 5. Rewire `main.go`.

Replace the current block at `main.go:35-38`:

```go
if !*helloWorld {
    fmt.Println("govenrich — Phase 1 build. ...")
    return
}
```

with:

```go
if !*helloWorld {
    runMCPServer()
    return
}
```

Add `runMCPServer()` that:

- Loads env (`APOLLO_API_KEY`, `FBI_CDE_API_KEY`) — reuse `fatal()` for
  missing keys.
- Builds the four clients into a `tools.Deps{}`.
- `srv := mcp.NewServer(&mcp.Implementation{Name: "govenrich", Version: "0.1.0"}, nil)`
- `mcp.AddTool(srv, &mcp.Tool{Name: "enrich_gov_agency", Description: "Enriches a US law-enforcement agency with sworn officer count and active federal grants — fills the Apollo gap on .gov domains."}, tools.NewEnrichHandler(deps))`
- `if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil { fmt.Fprintln(os.Stderr, "mcp server:", err); os.Exit(1) }`

### 6. Critical hygiene — stdout is sacred.

In the MCP path, stdout is the JSON-RPC transport. A single stray
`fmt.Println` to stdout corrupts every subsequent message and the client
will fail to list the tool with no obvious error. Rules:

- Every log line in `runMCPServer` must go to `os.Stderr`.
- The existing banner `fmt.Println("govenrich hello-world — ...")` is
  fine because it only fires in the `--hello-world` branch.
- `godotenv.Load()` is silent on success and only prints to stderr on
  failure — safe.

### 7. Build and hand-test.

```
go build -o govenrich
```

If Agent B hasn't committed yet, this fails at link time on
`tools.NewEnrichHandler`. That's expected — rebuild after they commit.

Once it builds, verify the server speaks MCP without a client:

```
(echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"<SDK default>","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}';
 echo '{"jsonrpc":"2.0","method":"notifications/initialized"}';
 echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}') | ./govenrich
```

For `<SDK default>`, read whatever protocol version constant the pinned
SDK exposes (grep the SDK source for `LatestProtocolVersion` or equivalent
and mirror its value). If initialization fails, check the SDK version
against the protocol version you sent — they must match.

Expect two JSON-RPC responses on stdout. The second should list
`enrich_gov_agency` with its input schema.

### 8. Document the client config — do not write to user files.

Do not edit `~/Library/Application Support/Claude/claude_desktop_config.json`
or `~/.claude.json` directly. Instead, add a "Running as an MCP server"
section to `README.md` showing both config files and leave the actual
file changes to the user:

- **Claude Desktop**:
  `~/Library/Application Support/Claude/claude_desktop_config.json`
- **Claude Code**: `~/.claude.json` (`mcpServers` key, same shape)

Both targets use the same `{ command, env }` block in README.md §"Claude
Desktop config (Phase 2)".

## Definition of done

- `go.mod` pins a specific `github.com/modelcontextprotocol/go-sdk` tag
  (no `@latest`).
- `public/fbi.go` exposes `PoliceEmployeeByORI(ori)` returning a
  `sworn_officers`-bearing payload.
- `public/census.go` exposes `LocalGovFinanceStub` that returns a clear
  "unavailable" error.
- `go build -o govenrich` succeeds with no warnings.
- Hand-test pipe (step 7) returns a `tools/list` response naming
  `enrich_gov_agency`.
- `--hello-world` mode still behaves identically to before.
- No `fmt.Print*` call in the MCP code path writes to stdout.
- README has a client-config section; no user-scope files modified.

## Estimated time

40–50 min. Budget the tail on hand-test debugging — protocol-version
mismatches and stray stdout writes are the two bugs that will eat time.

---

## Dispatcher Task — 2026-04-16

**Re-read this section on every spec change. Act on any new dispatcher task immediately.**

**Priority: fix `.env` portability so Claude Desktop can spawn the binary from any working directory.**

Current problem: `godotenv.Load()` in `main.go` loads `.env` from CWD. When Claude Desktop spawns the binary it sets CWD to `/`, so `.env` is never found and `runMCPServer()` immediately calls `fatal()` on the missing API keys.

### Your task

1. In `runMCPServer()` in `main.go`, replace `godotenv.Load()` (which is already called in `main()` before the branch) with a load that resolves `.env` relative to the binary's own location:

```go
// Load .env from same directory as the binary, so Claude Desktop can
// spawn from any CWD.
if exe, err := os.Executable(); err == nil {
    _ = godotenv.Load(filepath.Join(filepath.Dir(exe), ".env"))
}
```

Add `"path/filepath"` to imports. The existing top-level `godotenv.Load()` in `main()` can stay — it handles the dev case where you run from the repo root. The new load in `runMCPServer()` handles the Claude Desktop case.

2. `go build -o govenrich` from repo root. Build must be clean.

3. Run the smoke test to confirm the server still speaks MCP:
```
(echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'; sleep 0.5; echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'; sleep 0.5; echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'; sleep 1) | ./govenrich 2>/dev/null
```
Expect two clean JSON-RPC responses. `tools/list` must return `enrich_gov_agency`.

4. Append a "Latest Build — YYYY-MM-DD HH:MM" section to this spec with the smoke test transcript and PASS/FAIL.

5. Copy the `.env` file to sit alongside the binary: `cp .env govenrich.env` — no wait, instead just confirm the `.env` already lives at `/Users/admin/edgetrace-gtm/.env` next to the binary at `/Users/admin/edgetrace-gtm/govenrich`. If yes, the `filepath.Dir(exe)` approach resolves correctly and no file copy is needed.

---

## Accomplished

Executed 2026-04-16. All DOD items met; 8 commits landed.

### DOD checks

| Item | Result |
|---|---|
| `go.mod` pins a non-prerelease SDK tag | `v1.5.0` pinned (commit `df7749d`) |
| `PoliceEmployeeByORI` returns sworn-bearing payload | Added + response shape documented inline (commits `255dedc`, `e3e10f5`) |
| `LocalGovFinanceStub` returns clear unavailability error | Added (commit `02ea325`) |
| `go build -o govenrich` succeeds | Clean |
| Hand-test pipe lists `enrich_gov_agency` | Verified — `initialize` returns `serverInfo: govenrich/0.1.0` at protocol `2025-11-25`; `tools/list` returns the tool with input + output schema |
| `--hello-world` mode unchanged | `runHelloWorld` body untouched; only the no-flag branch replaced |
| Zero `fmt.Print*` in MCP code path | 5 `fmt.Print*` in `main.go` — all gated inside `--hello-world`. `runMCPServer` writes only to `os.Stderr` (one `Fprintln` on fatal server error) |
| README documents both client configs; no user-scope files touched | New "Running as an MCP server" section covers Claude Desktop and Claude Code, with the verify-without-client smoke-test pipe inlined |

### Commits (in order)

1. `a00c559` — Rewrite Phase 2a agent specs with Phase 1 findings
2. `df7749d` — Pin `github.com/modelcontextprotocol/go-sdk v1.5.0`
3. `c5073fc` — Add `tools/deps.go` frozen contract
4. `255dedc` — Add `PoliceEmployeeByORI` and correct `AgenciesByState` comment
5. `02ea325` — Add `LocalGovFinanceStub` for Phase 2 tool handlers
6. `e3e10f5` — Simplify `PoliceEmployeeByORI` to match spec's one-arg signature
7. `de8b42b` — Rewire `main.go` default branch to serve MCP over stdio
8. `a73a277` — Document MCP server mode and both client configs in README

### Surprises / spec deviations worth recording

- **SDK protocol version** — `v1.5.0` advertises `"2025-11-25"` as
  `latestProtocolVersion`, not `"2024-11-05"` as the original hand-test
  block implied. Updated both the spec's pipe and the README verify block
  to use `2025-11-25`.
- **`/pe/agency/{ori}` requires `from`/`to` year params** — without them
  the endpoint returns HTTP 400 with `"Bad request, 'from' and 'to' year
  is required."` The method hides this internally (fixed 4-year trailing
  window) so callers keep the one-arg signature the spec advertises.
- **Sworn-officer extraction isn't a single field** — CDE returns
  `actuals.Male Officers[year]` and `actuals.Female Officers[year]`
  separately; Agent B sums them. Field names recorded in the method
  comment per spec §3.
- **Signature race with Agent B** — their `tools/enrich_gov_agency.go`
  call site oscillated between 1-arg and 3-arg `PoliceEmployeeByORI`
  during the run. Settled on the 1-arg form (matches written spec).
- **`go.mod` upgraded Go 1.22 → 1.25.0** as a side effect of
  `go get github.com/modelcontextprotocol/go-sdk@v1.5.0` — kept the
  upgrade rather than forcing a downgrade that would drop newer SDK
  features.
- **`tools/deps.go` untouched since commit 3** — Agent B has not yet
  requested any extra dependency, so the frozen contract held.

### Explicit non-actions

- Did not edit `~/Library/Application Support/Claude/claude_desktop_config.json`
  or `~/.claude.json`. Both documented in README with a copy/paste block;
  user applies.
- Did not re-run `--hello-world` (burns Apollo credits and creates a real
  contact — out of scope for Agent A's DOD).
- Did not modify `apollo/*` or `tools/enrich_gov_agency.go`. The
  `tools/enrich_gov_agency.go` file on disk is Agent B's responsibility.

---

## Latest Build — 2026-04-16 20:17 UTC

**Dispatcher task**: fix `.env` portability for Claude Desktop (spawns with
CWD=`/`). **Result: PASS.**

### Change

`main.go` imports gain `"path/filepath"`. `runMCPServer()` now loads
`.env` from the binary's own directory in addition to the top-level
`main()` load:

```go
if exe, err := os.Executable(); err == nil {
    _ = godotenv.Load(filepath.Join(filepath.Dir(exe), ".env"))
}
```

Top-level `godotenv.Load()` in `main()` is intact, so running from the
repo root still works for dev.

### Layout confirmation

`.env` lives at `/Users/admin/edgetrace-gtm/.env` next to the binary at
`/Users/admin/edgetrace-gtm/govenrich` — `filepath.Dir(exe)` resolves
to the same directory. No file copy needed.

### Smoke test transcript

**Test 1** — repo-root CWD (control):

```
$ (echo init; sleep 0.5; echo initialized; sleep 0.5; echo tools/list; sleep 1) | ./govenrich 2>/dev/null
[1] id=1 init OK, serverInfo={'name': 'govenrich', 'version': '0.1.0'}, protocol=2025-11-25
[2] id=2 tools/list OK, tools=['enrich_gov_agency']
```

**Test 2** — `/tmp` CWD (portability proof, simulates Claude Desktop):

```
$ cd /tmp && (echo init; sleep 0.5; echo initialized; sleep 0.5; echo tools/list; sleep 1) | /Users/admin/edgetrace-gtm/govenrich 2>/dev/null
[1] id=1 init OK, serverInfo={'name': 'govenrich', 'version': '0.1.0'}, protocol=2025-11-25
[2] id=2 tools/list OK, tools=['enrich_gov_agency']
```

Before the fix Test 2 would have died on `fatal("APOLLO_API_KEY missing")`
because the top-level `godotenv.Load()` found no `.env` in `/tmp`. After
the fix, `runMCPServer()` loads `.env` from `/Users/admin/edgetrace-gtm/`
regardless of CWD and both tests return a clean `tools/list`.


