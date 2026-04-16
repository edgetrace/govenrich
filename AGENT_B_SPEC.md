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
