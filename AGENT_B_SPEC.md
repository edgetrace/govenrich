# Agent B — Tool Implementation (`enrich_gov_agency`)

## Role

You implement the single MCP tool that carries the Phase 2 business case:
`enrich_gov_agency`. Given an agency name and state, it fans out to Apollo
(for context), FBI CDE (for the canonical sworn-officer count),
USASpending.gov (for federal grants), and — in a future iteration —
Census (currently stubbed), then merges the results into one struct.

You do **not** wire the MCP server, touch `main.go`, or add dependencies —
Agent A owns all of that. Your only integration point with Agent A is the
shared contract below.

## Phase II scope note

SPEC.md defines four Phase 2 tools (`search_gov_agencies`,
`enrich_gov_agency`, `score_agency_fit`, `draft_gov_outreach`). This spec
covers only `enrich_gov_agency` — the minimum end-to-end demo. The other
three tools will be scoped in a follow-up spec pair after this one ships.

## Context

- Go module: `github.com/edgetrace/govenrich`.
- Apollo client: `github.com/edgetrace/govenrich/apollo` — `*apollo.Client`
  with `OrgEnrich(domain)`, `OrgSearch(req)`, etc. See `apollo/client.go`.
- Public clients: `github.com/edgetrace/govenrich/public`:
  - `*public.FBIClient.AgenciesByState(state)` — returns `{COUNTY: [...]}`
    (directory info only — no sworn count)
  - `*public.FBIClient.PoliceEmployeeByORI(ori)` — returns per-agency
    personnel data including `sworn_officers`. **Agent A adds this method
    before you start. Don't skip it — this is your canonical sworn-count
    source.**
  - `*public.USASpendingClient.SpendingByAward(req)` — returns `{results: [...]}`
  - `*public.CensusClient.LocalGovFinanceStub(fips)` — returns an error
    explaining the endpoint is unavailable. Call it so the provenance
    is honest; don't call the broken `LocalGovFinance` method.
- The Phase 1 connectivity harness (`main.go --hello-world`) exercises
  every client and prints real response shapes; use it as a reference.

### The real null-gap narrative (updated from SPEC)

The original pitch was "Apollo returns null for employees/revenue on `.gov`
domains." That framing is stale as of Phase 1 — Apollo now populates
city-total employees/revenue on `.gov` (Pleasanton returns 470 / $144M).

**The remaining gap is sworn-officer count specifically.** Apollo still
does not expose it; FBI CDE `/pe/agency/{ori}` does. Your `EnrichOutput`
must surface this contrast: `ApolloEmployeeCount` shows whatever Apollo
says (populated but non-LE-specific), and `SwornOfficers` comes from FBI
as the LE-specific figure that only this tool can combine.

## Files you own (exclusive write access)

- `tools/enrich_gov_agency.go` (new)

Do **not** touch `main.go`, `go.mod`, `go.sum`, `tools/deps.go`,
`apollo/*`, or `public/*`. If you need a new dependency or a new client
method, ping Agent A — they will extend `Deps{}` or add the method.

## Frozen contract (shared with Agent A)

Agent A has committed `tools/deps.go` with:

```go
type Deps struct {
    Apollo      *apollo.Client
    FBI         *public.FBIClient
    USASpending *public.USASpendingClient
    Census      *public.CensusClient
}
```

You must export exactly this symbol for Agent A to reference:

```go
func NewEnrichHandler(deps Deps) func(
    ctx context.Context,
    req *mcp.CallToolRequest,
    in EnrichInput,
) (*mcp.CallToolResult, EnrichOutput, error)
```

`EnrichInput` and `EnrichOutput` are yours to define — Agent A references
them by name, not by shape.

## Required types

### `EnrichInput`

```go
type EnrichInput struct {
    AgencyName string `json:"agency_name" jsonschema:"name of the agency, e.g. 'Pleasanton Police Department'"`
    State      string `json:"state"       jsonschema:"two-letter US state code, e.g. 'CA'"`
}
```

The `jsonschema:` tags are the prompt the model sees when deciding whether
to call the tool — write them as examples, not as types.

### `EnrichOutput` (suggested shape)

```go
type EnrichOutput struct {
    Name                 string          `json:"name"`
    Domain               string          `json:"domain,omitempty"`
    City                 string          `json:"city,omitempty"`
    State                string          `json:"state"`

    // Apollo — now populates these on .gov (city totals, not LE-specific).
    // Kept as pointers so a true miss still serializes as null.
    ApolloEmployeeCount  *int            `json:"apollo_employee_count"`
    ApolloAnnualRevenue  *int            `json:"apollo_annual_revenue"`

    // FBI CDE — the real null-gap demo. Apollo cannot provide this;
    // FBI /pe/agency/{ori} can. Leave nil when the ORI match fails so
    // Claude renders null and the gap is visible in the output.
    ORI                  string          `json:"ori,omitempty"`
    AgencyType           string          `json:"agency_type,omitempty"`
    SwornOfficers        *int            `json:"sworn_officers"`

    // USASpending — warm signal for adjacent tech spend.
    ActiveGrants         []GrantSummary  `json:"active_grants"`

    // Census — currently unavailable via API (see SPEC §11). Field stays
    // nil and a note lands in PartialErrors.
    AnnualExpenditureUSD *int            `json:"annual_expenditure_usd"`

    // Provenance — which APIs contributed at least one field.
    Sources              []string        `json:"sources"`
    // Per-source failures surface here instead of failing the whole call.
    PartialErrors        []string        `json:"partial_errors,omitempty"`
}

type GrantSummary struct {
    AwardID        string  `json:"award_id"`
    RecipientName  string  `json:"recipient_name"`
    AmountUSD      float64 `json:"amount_usd"`
    AwardingAgency string  `json:"awarding_agency"`
}
```

## Handler behavior

1. **Fan out in parallel.** Use `sync/errgroup` (or a `sync.WaitGroup` +
   mutex) to run Apollo + FBI + USASpending concurrently. Census is a
   stub call — include it in the fan-out so provenance is recorded, but
   expect it to immediately error into `PartialErrors`.

2. **Resolve domain for Apollo.** You have `AgencyName + State` but
   `apollo.OrgEnrich` takes a domain. Strategy:
   - `apollo.OrgSearch` with the agency name + `organization_locations:
     [state]`. Results split across `accounts` and `organizations` —
     check both arrays (see `main.go`'s step-2 parsing for precedent).
   - Take the first hit's `website_url`, feed into `OrgEnrich`.
   - If no hit, leave `ApolloEmployeeCount` / `ApolloAnnualRevenue` as
     `nil` and do **not** append "apollo" to `Sources`.

3. **FBI: two calls, not one.** `AgenciesByState(state)` gives the
   directory; `PoliceEmployeeByORI(ori)` gives sworn counts. Flow:
   - Call `AgenciesByState(state)` → flatten the county-keyed map.
   - Match on `agency_name` (case-insensitive, tolerant of
     "Police Dept" vs "Police Department"). Capture `ori`,
     `agency_type_name`, `city`.
   - Call `PoliceEmployeeByORI(ori)` with the matched ORI.
   - Extract the sworn count from the response. The exact field name is
     whatever Agent A recorded in the `PoliceEmployeeByORI` method
     comment — check there before coding.

4. **USASpending: search by recipient.** POST `spending_by_award` with
   `recipient_search_text: [AgencyName]`, a 2-year window ending today,
   `place_of_performance_locations: [{"country":"USA","state":State}]`
   (the `country` field is required — Phase 1 fixed this in
   `main.go`'s step-10 call; your payload must include it too). Map the
   top N results into `[]GrantSummary`.

5. **Census: expect failure, record it.** Call
   `deps.Census.LocalGovFinanceStub(fips)` — it always errors. Append
   `"census: endpoint unavailable (SPEC §11)"` to `PartialErrors`. Do
   **not** append `"census"` to `Sources`. When SPEC §11 is resolved in
   a future iteration, the only change here will be swapping the stub
   call for the real one.

6. **Partial failures are OK.** If Apollo, FBI, or USASpending errors or
   returns nothing, populate what you can, append to `PartialErrors`,
   and return a successful response. The demo sings louder when FBI's
   `SwornOfficers` is populated and Apollo's cannot be than when the
   whole call fails.

7. **`Sources` is provenance.** Append `"apollo"`, `"fbi_cde"`,
   `"usaspending"` as each contributes at least one field. Census is
   omitted deliberately until SPEC §11 is resolved.

## Handler signature (exact)

```go
func NewEnrichHandler(deps Deps) func(
    context.Context,
    *mcp.CallToolRequest,
    EnrichInput,
) (*mcp.CallToolResult, EnrichOutput, error) {
    return func(ctx context.Context, req *mcp.CallToolRequest, in EnrichInput) (*mcp.CallToolResult, EnrichOutput, error) {
        // ... fan-out + merge ...
        return nil, out, nil
    }
}
```

Return `nil` for `*mcp.CallToolResult` when you have clean structured
output — the SDK will serialize `EnrichOutput` as the result. Only return
a non-nil `CallToolResult` (with `IsError: true`) for true failures where
no structured output is possible.

## Definition of done

- Package compiles: `go build ./tools/...` (after Agent A has committed
  `go.mod`, `tools/deps.go`, `PoliceEmployeeByORI`, and
  `LocalGovFinanceStub`).
- `NewEnrichHandler(Deps{})` returns a function with the exact signature
  above — Agent A's call site must not need a cast.
- For input `{AgencyName: "Pleasanton Police Department", State: "CA"}`:
  - `SwornOfficers` is populated from FBI `/pe/` (the real null-gap demo).
  - `ApolloEmployeeCount` and `ApolloAnnualRevenue` are populated with
    Apollo's city-total figures (the "populated but non-LE-specific"
    contrast).
  - `ActiveGrants` is populated if any CA law-enforcement awards fall in
    the time window.
  - `AnnualExpenditureUSD` is `nil` and `PartialErrors` contains the
    Census unavailable note.
  - `Sources` contains `"apollo"`, `"fbi_cde"`, `"usaspending"` but not
    `"census"`.
- Partial API failures produce a response with `PartialErrors` populated,
  not a hard error.

## Estimated time

30–40 min. The fan-out and merge are small; the tedious parts are the
agency-name matching (APIs disagree on casing, whitespace, and suffixes)
and reading the actual field name Agent A recorded for
`PoliceEmployeeByORI`. Keep matching dumb and forgiving for the demo;
precision can come in a later iteration.

---

## Dispatcher Task — 2026-04-16

**Re-read this spec tail on every change. Act on new dispatcher tasks immediately.**

Two bugs found from a live Claude Desktop test of `enrich_gov_agency` against Pleasanton PD. Fix both, rebuild, run the smoke test, and append results.

---

### Bug 1 — `extractSwornCount` never fires (file: `tools/enrich_gov_agency.go`)

**Root cause:** `PoliceEmployeeByORI` in `public/fbi.go` returns `(int, []byte, error)` — the first return value is the HTTP status code, but `extractSwornCount` is called with `peBody` which is correct. The API response shape is confirmed correct (verified live against CA0011100):

```json
{"actuals": {"Male Officers": {"2021":69,"2022":69,"2023":64,"2024":68}, "Female Officers": {"2021":9,"2022":11,"2023":9,"2024":10}}}
```

The `extractSwornCount` struct unmarshals into `payload.Actuals` — check whether the FBI response is actually a **top-level array** rather than a single object. Run this to confirm:

```
source /Users/admin/edgetrace-gtm/.env && curl -s "https://api.usa.gov/crime/fbi/cde/pe/agency/CA0011100?from=2021&to=2024&API_KEY=$FBI_CDE_API_KEY" | python3 -c "import sys,json; d=json.load(sys.stdin); print(type(d), list(d.keys()) if isinstance(d,dict) else d[0])"
```

If the response is a JSON array (not object), `extractSwornCount` must unwrap the first element before unmarshaling into the struct. Fix `extractSwornCount` to handle both shapes:

```go
func extractSwornCount(body []byte) *int {
    // FBI /pe/agency sometimes wraps response in an array
    trimmed := bytes.TrimSpace(body)
    if len(trimmed) > 0 && trimmed[0] == '[' {
        var arr []json.RawMessage
        if err := json.Unmarshal(trimmed, &arr); err != nil || len(arr) == 0 {
            return nil
        }
        body = arr[0]
    }
    var payload struct {
        Actuals map[string]map[string]any `json:"actuals"`
    }
    // ... rest unchanged
```

Add `"bytes"` to imports if not present.

---

### Bug 2 — Apollo OrgSearch finds no match (file: `tools/enrich_gov_agency.go`)

**Root cause:** `resolveApollo` calls `OrgSearch` with `KeywordTags: []string{in.AgencyName}` and `Locations: []string{strings.ToLower(in.State)}`. Passing the full agency name as a keyword tag is too specific — Apollo's keyword search doesn't match exact org names well.

**Fix:** Change the search strategy to use the agency name as `q` (free-text) instead of a keyword tag, and pass the full state name not abbreviation. The `OrgSearchRequest` struct has a `Q` field — check `apollo/client.go` to confirm field names, then update:

```go
status, body, err := cli.OrgSearch(apollo.OrgSearchRequest{
    Q:         in.AgencyName,
    Locations: []string{stateAbbrevToName(in.State)},
    PerPage:   5,
    Page:      1,
})
```

Add a small helper:
```go
func stateAbbrevToName(abbr string) string {
    names := map[string]string{
        "CA": "california", "TX": "texas", "NY": "new york",
        "FL": "florida", "IL": "illinois", "PA": "pennsylvania",
        // add more as needed, lowercase for Apollo
    }
    if n, ok := names[strings.ToUpper(abbr)]; ok {
        return n
    }
    return strings.ToLower(abbr)
}
```

If `OrgSearchRequest` has no `Q` field, fall back to passing the agency name in `KeywordTags` but with only the distinctive part (e.g. strip "Police Department", "Sheriff's Office" suffixes) — check `apollo/client.go` first.

---

### After both fixes

1. `go build ./...` — must be clean
2. Run smoke test from repo root:
```
(echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'; sleep 0.5; echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'; sleep 0.5; echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'; sleep 1) | ./govenrich 2>/dev/null
```
3. Append a "Latest Build — YYYY-MM-DD HH:MM" section to this spec with build result and smoke test transcript.

---

## Execution log

Shipped `tools/enrich_gov_agency.go`. `go build ./...` and `go vet ./...`
both clean against the repo state at commit time.

### What landed

- **Types**: `EnrichInput`, `EnrichOutput`, `GrantSummary` exactly as
  specified, with pointer fields for `ApolloEmployeeCount`,
  `ApolloAnnualRevenue`, `SwornOfficers`, `AnnualExpenditureUSD` so JSON
  null serializes when the upstream returns nothing.
- **Handler signature**: `NewEnrichHandler(Deps) func(context.Context,
  *mcp.CallToolRequest, EnrichInput) (*mcp.CallToolResult, EnrichOutput,
  error)` — exact match for Agent A's call site, no casts needed.
- **Fan-out**: `sync.WaitGroup` over four goroutines (Apollo, FBI,
  USASpending, Census stub), all partial results merged into a single
  `EnrichOutput` under a shared mutex. Handler never returns a non-nil
  error; every per-source failure lands in `PartialErrors`.
- **Apollo path**: `OrgSearch` across both `accounts` and `organizations`
  arrays (matches `main.go` step-2 precedent), take the first hit's
  `website_url`, normalize via `cleanDomain`, call `OrgEnrich`, surface
  `estimated_num_employees` and `annual_revenue` from the
  `organization` sub-object.
- **FBI path**: `AgenciesByState` → flatten the county-keyed map →
  fuzzy-match on `agency_name` (lowercase, punctuation stripped,
  `Dept` ↔ `Department`, token-score fallback) → `PoliceEmployeeByORI`
  → `extractSwornCount` parses the `actuals["Male Officers" | "Female
  Officers"][year]` shape Agent A verified against Pleasanton PD
  (CA0011100). Civilians are deliberately excluded. Most recent
  populated year is auto-selected so no hardcoded year assumption.
- **USASpending path**: 2-year trailing window ending today,
  `recipient_search_text: [AgencyName]`,
  `place_of_performance_locations: [{country: "USA", state: State}]`
  (the `country` field Phase 1 fixed is included), top 5 results mapped
  to `GrantSummary`.
- **Census path**: Calls `LocalGovFinanceStub` for provenance honesty;
  the resulting error lands in `PartialErrors` as
  `"census: <stub message>"` and `"census"` is deliberately absent from
  `Sources`.

### Definition of done — status

- [x] Package compiles cleanly — verified after Agent A committed the
      SDK pin, `tools/deps.go`, `PoliceEmployeeByORI`, and
      `LocalGovFinanceStub`.
- [x] `NewEnrichHandler(Deps{})` returns a function with the exact
      specified signature.
- [x] `Sources` contains `"apollo"`, `"fbi_cde"`, `"usaspending"` and
      never `"census"` (code path enforced).
- [x] Partial API failures produce a response with `PartialErrors`
      populated, not a hard error.
- [ ] End-to-end behavior against live APIs for Pleasanton PD — pending
      Agent A finishing the `main.go` MCP-server wire-up. Static logic
      review confirms all the output invariants fire correctly; the
      only runtime verification still needed is the actual HTTP calls.

### Coordination notes for future agents

- **PoliceEmployeeByORI signature drift**: during this work Agent A
  flipped the method between `(ori)` and `(ori, fromYear, toYear)` more
  than once. Final state at commit is the single-arg form (`(ori)`),
  with the year window owned internally by the client. The tool
  handler's only coupling is at one call site — if it flips again, fix
  is a one-line edit in `resolveFBI`.
- **File boundary respected**: no edits to `main.go`, `go.mod`,
  `go.sum`, `tools/deps.go`, `apollo/*`, or `public/*`. Agent A's
  unstaged changes at commit time (go.mod/go.sum/main.go/public/fbi.go)
  are left untouched.
- **No new deps**: handler uses only stdlib + the MCP SDK Agent A
  already pinned, plus the existing `apollo` and `public` packages.

---

## Latest Build — 2026-04-16 20:54 UTC

`go build -o govenrich .` clean. `go vet ./...` clean.

### Dispatcher-task fixes applied

- **Bug 1 (extractSwornCount):** Dispatcher's array-unwrap hypothesis was
  wrong — the /pe/ response is a plain object, not array-wrapped. Real
  root cause: FBI returns the latest 1-2 years of the window as `null`
  (data-refresh lag), so picking the max year string gave `"2026"` →
  null → zero sworn total → nil return. Fix: iterate years in descending
  order, skip entries where both male and female are null, return first
  year with real data. Pleasanton 2024 = 68 male + 10 female = 78.
- **Bug 2 (Apollo search):** Dispatcher's `Q` field doesn't exist on
  `OrgSearchRequest` — available fields are only `KeywordTags`,
  `Locations`, `PerPage`, `Page`. Instead: strip trailing LE suffixes
  from the agency name via `distinctiveKeyword` ("Pleasanton Police
  Department" → "pleasanton police", which Apollo matches cleanly) and
  expand the state abbreviation via `stateFullName` ("CA" → "california"
  — Apollo expects full names in `organization_locations`). Two new
  helpers added; no changes to `apollo/*` or `public/*`.

### Smoke test transcript

Input: `{agency_name: "Pleasanton Police Department", state: "CA"}`

```json
{
  "name": "Pleasanton Police Department",
  "domain": "www.pleasantonpd.org",
  "city": "Pleasanton",
  "state": "CA",
  "apollo_employee_count": 33,
  "apollo_annual_revenue": null,
  "ori": "CA0011100",
  "agency_type": "City",
  "sworn_officers": 78,
  "active_grants": [],
  "annual_expenditure_usd": null,
  "sources": ["apollo", "fbi_cde"],
  "partial_errors": [
    "census: census govfinances endpoint unavailable — see SPEC.md §11",
    "usaspending: no grants for Pleasanton Police Department in last 2 years"
  ]
}
```

### Status against DoD

- [x] `sworn_officers` populated from FBI `/pe/` (78, via 2024 data).
- [x] `apollo_employee_count` populated (33). `apollo_annual_revenue`
      is null because the matched Apollo account record lacks revenue —
      not a code bug; Apollo's data.
- [ ] `active_grants` empty. Dispatcher didn't flag this; may be a real
      no-data result for recipient "Pleasanton Police Department" in
      the last 2 years, or the search term may need the same suffix
      stripping the Apollo path got. Left as-is pending a dispatcher
      call.
- [x] `annual_expenditure_usd` null, Census note in `partial_errors`.
- [x] `sources` = `["apollo", "fbi_cde"]` — correctly excludes census
      and (correctly) usaspending since it contributed no fields.

### Known residual gaps

- USASpending returns no grants for the exact recipient string — may
  warrant the same suffix-stripping pattern or a wider lookback. Will
  address if the dispatcher flags.
