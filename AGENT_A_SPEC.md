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

## Dispatcher Task — Phase 4 — 2026-04-16

**Re-read this spec tail on every change. Act on new dispatcher tasks immediately.**

Four changes needed. Do them in order, commit after each.

### A1. Add `OrganizationDomains` to `apollo/client.go`

In `apollo/client.go`, add one field to `PeopleSearchRequest`:
```go
OrganizationDomains []string `json:"organization_domains,omitempty"`
```

### A2. Add Anthropic SDK to `go.mod`

```
go get github.com/anthropics/anthropic-sdk-go
```
Use whatever latest non-prerelease tag exists. Commit go.mod + go.sum with the pinned version in the commit message.

### A3. Add `AnthropicKey` to `tools/deps.go`

```go
type Deps struct {
    Apollo       *apollo.Client
    FBI          *public.FBIClient
    USASpending  *public.USASpendingClient
    Census       *public.CensusClient
    AnthropicKey string  // optional — only search_gov_web and draft_gov_outreach need it
}
```

### A4. Wire in `main.go`

In `runMCPServer()`:
1. Add to `deps` struct: `AnthropicKey: os.Getenv("ANTHROPIC_API_KEY"),` — do NOT fatal if missing.
2. Add three new `mcp.AddTool` calls after the existing four:

```go
mcp.AddTool(srv, &mcp.Tool{
    Name:        "search_gov_web",
    Description: "Searches the web for city council meeting minutes, agendas, and news to identify key stakeholders and influencers at a government agency.",
}, tools.NewWebSearchHandler(deps))

mcp.AddTool(srv, &mcp.Tool{
    Name:        "draft_gov_outreach",
    Description: "Drafts a personalized first-touch outreach email for a government agency contact using enriched agency data, fit score, and web research context. Requires sender_name, product, and company.",
}, tools.NewDraftHandler(deps))

mcp.AddTool(srv, &mcp.Tool{
    Name:        "create_apollo_sequence",
    Description: "Creates an Apollo contact and enrolls them in a sequence. Requires master Apollo API key for sequence enrollment.",
}, tools.NewSequenceHandler(deps))
```

### A5. Build + smoke test

After Agent B and C commit their files, run:
```
go build -o govenrich && \
(echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'; sleep 0.5; echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'; sleep 0.5; echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'; sleep 1) | ./govenrich 2>/dev/null
```
`tools/list` must return all 7 tools. Append "Latest Build — YYYY-MM-DD HH:MM" to this spec with transcript + PASS/FAIL.

## Latest Build — 2026-04-16

**PASS.** `go build -o govenrich` clean. `tools/list` returns 7 tools:
`create_apollo_sequence`, `draft_gov_outreach`, `enrich_gov_agency`,
`find_gov_contacts`, `score_agency_fit`, `search_gov_agencies`,
`search_gov_web`. Binary at `/Users/admin/edgetrace-gtm/govenrich`.

### Phase 4 work completed

- `apollo/client.go` — added `OrganizationDomains []string` to `PeopleSearchRequest`
- `go.mod` — pinned `github.com/anthropics/anthropic-sdk-go v1.37.0`
- `tools/deps.go` — added `AnthropicKey string` (optional, documented)
- `main.go` — wired `AnthropicKey` from env (non-fatal if missing) +
  registered `search_gov_web`, `draft_gov_outreach`, `create_apollo_sequence`

---

## Dispatcher Task — Phase 3 — 2026-04-16

**Re-read this spec tail on every change. Act on new dispatcher tasks immediately.**

Three new tools are being added to the MCP server: `search_gov_agencies`,
`score_agency_fit`, and `find_gov_contacts`. Agent B owns the tool files.
Your job is to wire them into `main.go` alongside `enrich_gov_agency`.

### Task: Register three new tools in `runMCPServer()`

In `main.go`, in `runMCPServer()`, add three `mcp.AddTool` calls after the
existing `enrich_gov_agency` one:

```go
mcp.AddTool(srv, &mcp.Tool{
    Name:        "search_gov_agencies",
    Description: "Searches for US law-enforcement agencies in a state, ranked by ICP fit score. Returns merged Apollo + FBI records with sworn officer counts Apollo is missing.",
}, tools.NewSearchHandler(deps))

mcp.AddTool(srv, &mcp.Tool{
    Name:        "score_agency_fit",
    Description: "Scores a single enriched agency against the Pleasanton PD ICP profile. Returns 0-100 fit score and reasoning strings. No external API calls.",
}, tools.NewScoreHandler(deps))

mcp.AddTool(srv, &mcp.Tool{
    Name:        "find_gov_contacts",
    Description: "Finds people associated with a government agency via Apollo people search. Returns names, titles, and optionally enriched email addresses.",
}, tools.NewContactsHandler(deps))
```

Agent B will define `NewSearchHandler`, `NewScoreHandler`, and
`NewContactsHandler` in their respective files. Your only job here is the
`mcp.AddTool` registration.

### After wiring

1. `go build ./...` — will fail until Agent B commits their files. That's
   expected. Rebuild after they commit.
2. Once it builds, run the smoke test:
```
(echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'; sleep 0.5; echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'; sleep 0.5; echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'; sleep 1) | ./govenrich 2>/dev/null
```
3. `tools/list` must return all four tools: `enrich_gov_agency`,
   `search_gov_agencies`, `score_agency_fit`, `find_gov_contacts`.
4. Append "Latest Build — YYYY-MM-DD HH:MM" to this spec with the
   smoke test transcript and PASS/FAIL.

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
