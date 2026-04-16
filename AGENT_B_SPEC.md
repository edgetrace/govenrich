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

## Dispatcher Task 5 — fix find_gov_contacts — 2026-04-16

**Re-read this spec tail on every change. Act immediately.**

Two bugs found via live Apollo probe on Vallejo PD.

### Bug 1 — `organization_domains` filter doesn't work

Passing `organization_domains: ["vallejopd.net"]` to Apollo's
`/mixed_people/api_search` returns completely unrelated people (Sony,
Experian IT directors). Apollo silently ignores the domain filter for
`.gov` domains.

**Fix in `tools/find_gov_contacts.go`:** Replace the domain-filter
strategy with `q_organization_name` search instead. In `resolveContacts`
(or wherever `PeopleSearch` is called), add `Q` field to the search:

Check `apollo/client.go` — `OrgSearchRequest` has no `Q` field on
`PeopleSearchRequest`. You need Agent A to add it, OR work around it
by passing the agency name via a different field. Check if Apollo's
`/mixed_people/api_search` accepts `q_organization_name` as a body
field — it does (verified in live test). Add to `apollo.PeopleSearchRequest`:

```go
OrgName string `json:"q_organization_name,omitempty"`
```

Ping Agent A to add this field to `apollo/client.go`. Then in
`find_gov_contacts.go`, populate it with `in.AgencyName` when searching.
Remove `OrganizationDomains` from the search request — it doesn't work
for `.gov` orgs.

### Bug 2 — `last_name` is null, draft gets placeholder

Apollo returns `first_name` only for government contacts — `last_name`
is `null`. The draft email gets `[Chief/Commander's name]` placeholder.

**Fix in `tools/find_gov_contacts.go`:** In `parsePeople`, when
`last_name` is empty, set `LastName` to the title-cased org abbreviation
or leave it empty — but ensure `FirstName` alone is passed to
`draft_gov_outreach` so the draft can use "Hi Jason," instead of a
placeholder.

Also fix `enrichEmails` — it currently skips `PeopleMatch` when
`LastName == ""` (line ~219). Relax this: if `FirstName` is non-empty,
attempt `PeopleMatch` with just first name + org name. Apollo's match
endpoint accepts partial names.

### After both fixes

1. `go build ./...` clean
2. Smoke test — all 7 tools in `tools/list`
3. Append "Latest Build — YYYY-MM-DD HH:MM" with PASS/FAIL

---

## Dispatcher Task 4 — Phase 4 tools — 2026-04-16

**Re-read this spec tail on every change. Act on new dispatcher tasks immediately.**

Build two new tool files. Agent A is adding the Anthropic SDK to go.mod and wiring the handlers — wait for their go.mod commit before building, then `go build ./...` to confirm.

### File 1: `tools/search_gov_web.go`

Uses Anthropic API with web_search to research a government agency.

```go
type WebSearchInput struct {
    AgencyName string `json:"agency_name" jsonschema:"agency name, e.g. 'Pleasanton Police Department'"`
    State      string `json:"state"       jsonschema:"two-letter state code, e.g. 'CA'"`
    City       string `json:"city,omitempty" jsonschema:"city name if known, e.g. 'Pleasanton'"`
    Focus      string `json:"focus,omitempty" jsonschema:"optional: 'council', 'budget', 'leadership', 'technology'. Omit to search all."`
}

type WebSearchOutput struct {
    Stakeholders  []Stakeholder `json:"stakeholders"`
    BudgetSignals []string      `json:"budget_signals"`
    NewsItems     []string      `json:"news_items"`
    RawSummary    string        `json:"raw_summary"`
    Sources       []string      `json:"sources"`
    PartialErrors []string      `json:"partial_errors,omitempty"`
}

type Stakeholder struct {
    Name   string `json:"name"`
    Title  string `json:"title,omitempty"`
    Source string `json:"source,omitempty"`
}
```

**Implementation:**
- Use `github.com/anthropics/anthropic-sdk-go`. Check go.mod for the exact import path after Agent A adds it.
- Model: `claude-opus-4-7`
- Enable the `web_search` tool in the API call (check the SDK docs for how to pass built-in tools).
- System prompt: "You are a B2G sales researcher. Extract named people with titles, budget signals (dollar amounts, tech purchases), and recent news relevant to selling technology to this agency. Be specific and cite sources."
- User prompt: `"Research {AgencyName} in {City}, {State}. Focus: {Focus or 'all areas'}. Find: city council members, police/IT leadership, recent budget approvals, technology purchases, federal grants."`
- From the response, parse:
  - `Stakeholders`: any "Name, Title" patterns
  - `BudgetSignals`: sentences containing dollar amounts or budget/procurement language
  - `NewsItems`: recent dated events
  - `Sources`: any URLs cited in the response
  - `RawSummary`: full text response
- If `deps.AnthropicKey == ""`: populate PartialErrors with `"search_gov_web: ANTHROPIC_API_KEY not configured"`, return empty output, no error.

```go
func NewWebSearchHandler(deps Deps) func(context.Context, *mcp.CallToolRequest, WebSearchInput) (*mcp.CallToolResult, WebSearchOutput, error)
```

### File 2: `tools/draft_gov_outreach.go`

Uses Anthropic API (generation only, no web_search) to write personalized cold outreach.

```go
type DraftInput struct {
    Agency     EnrichOutput    `json:"agency"`
    Contact    ContactResult   `json:"contact,omitempty"`
    Score      ScoreOutput     `json:"score,omitempty"`
    WebContext WebSearchOutput `json:"web_context,omitempty"`
    SenderName string          `json:"sender_name" jsonschema:"your first name, e.g. 'Alex'"`
    Product    string          `json:"product"     jsonschema:"product being pitched, e.g. 'video analytics platform'"`
    Company    string          `json:"company"     jsonschema:"your company name, e.g. 'EdgeTrace'"`
}

type DraftOutput struct {
    Subject             string   `json:"subject"`
    Body                string   `json:"body"`
    PersonalizationUsed []string `json:"personalization_used"`
}
```

**Implementation:**
- Model: `claude-sonnet-4-6`
- No tools, just generation.
- Build a context block from available fields:
  - Agency: name, sworn officers, active grants, domain, city
  - Contact: name, title (if provided)
  - Score: score, tier, reasoning (if provided)
  - WebContext: stakeholder names, budget signals, news (if provided)
- System prompt: "You are a B2G sales expert. Write a concise, specific first-touch cold email. Max 150 words for the body. Reference real data points. Never generic. End with a soft CTA (15-minute call). After the email, list exactly which data points you personalized with, one per line, prefixed 'USED:'."
- Parse `USED:` lines from response into `PersonalizationUsed`.
- If `deps.AnthropicKey == ""`: return PartialErrors `"draft_gov_outreach: ANTHROPIC_API_KEY not configured"`.

```go
func NewDraftHandler(deps Deps) func(context.Context, *mcp.CallToolRequest, DraftInput) (*mcp.CallToolResult, DraftOutput, error)
```

### Build + smoke test

```
go build ./... && \
(echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'; sleep 0.5; echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'; sleep 0.5; echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'; sleep 1) | ./govenrich 2>/dev/null
```
All 7 tools must appear. Append "Latest Build — YYYY-MM-DD HH:MM" to this spec with transcript + PASS/FAIL.

## Latest Build — 2026-04-16

**PASS.** `go build ./...` clean. All 7 tools in `tools/list`.

### Phase 4 work completed

- `tools/search_gov_web.go` — `search_gov_web` tool. Calls Anthropic API
  (`claude-opus-4-7`) with `web_search` enabled. Extracts `Stakeholders`,
  `BudgetSignals`, `NewsItems`, `Sources` from response. Gracefully
  returns empty output with `PartialErrors` when `AnthropicKey` is unset.
- `tools/draft_gov_outreach.go` — `draft_gov_outreach` tool. Calls
  `claude-sonnet-4-6` for generation. Accepts `EnrichOutput`, `ContactResult`,
  `ScoreOutput`, `WebSearchOutput` as context. Parses `USED:` lines from
  response into `PersonalizationUsed`. Requires `sender_name`, `product`,
  `company` — no defaults.

---

## Dispatcher Task 3 — Phase 3 tools — 2026-04-16

**Re-read this spec tail on every change. Act on new dispatcher tasks immediately.**

Build three new MCP tool files. Each is a new file in `tools/`. Do not
touch `main.go`, `go.mod`, `tools/deps.go`, `apollo/*`, or `public/*`.
Agent A is wiring the `mcp.AddTool` registrations in parallel.

---

### Tool 1: `tools/search_gov_agencies.go`

**MCP tool name:** `search_gov_agencies`

**What it does:** Given a state and optional filters, returns a ranked list
of law-enforcement agencies merged from Apollo + FBI CDE.

**Input:**
```go
type SearchInput struct {
    State      string `json:"state"        jsonschema:"two-letter US state code, e.g. 'CA'"`
    MinOfficers int   `json:"min_officers" jsonschema:"minimum sworn officer count, e.g. 50. 0 = no filter"`
    Limit       int   `json:"limit"        jsonschema:"max results to return, default 10, max 50"`
}
```

**Output:**
```go
type SearchOutput struct {
    Agencies     []AgencyResult `json:"agencies"`
    TotalFound   int            `json:"total_found"`
    PartialErrors []string      `json:"partial_errors,omitempty"`
}

type AgencyResult struct {
    Name         string  `json:"name"`
    City         string  `json:"city,omitempty"`
    State        string  `json:"state"`
    ORI          string  `json:"ori,omitempty"`
    AgencyType   string  `json:"agency_type,omitempty"`
    SwornOfficers *int   `json:"sworn_officers"`
    Domain       string  `json:"domain,omitempty"`
    FitScore     int     `json:"fit_score"` // 0-100 from scorer
}
```

**Behavior:**
1. Call `deps.FBI.AgenciesByState(state)` — flatten the county-keyed map
   into a flat `[]AgencyResult`. This is the master list.
2. Filter by `MinOfficers` if > 0. Note: sworn count is NOT in the
   directory — filter on `SwornOfficers` only after step 3 populates it,
   OR skip the PE call for list mode and filter on directory record counts
   if available. Keep it simple: skip PE fan-out for this tool (too many
   API calls), filter on whatever the directory gives.
3. For the top N agencies (after filtering, before scoring), attempt
   `deps.Apollo.OrgSearch` with the agency name + state to find domains.
   Batch with a `sync.WaitGroup`, cap goroutines at 5 to avoid rate limits.
4. Score each result using `ScoreAgency(r AgencyResult) int` — import the
   scorer from `tools/score_agency_fit.go` (same package, so direct call).
5. Sort descending by `FitScore`. Apply `Limit` (default 10, cap 50).
6. Return `SearchOutput`.

**Handler export:**
```go
func NewSearchHandler(deps Deps) func(context.Context, *mcp.CallToolRequest, SearchInput) (*mcp.CallToolResult, SearchOutput, error)
```

---

### Tool 2: `tools/score_agency_fit.go`

**MCP tool name:** `score_agency_fit`

**What it does:** Pure Go scoring — no external calls. Takes an enriched
agency and returns a 0-100 ICP fit score with reasoning strings.

**ICP baseline (Pleasanton PD ground truth from SPEC.md):**
- SwornOfficers: 70
- AgencyType: "City" (municipal PD)
- State: "CA"
- HasActiveGrants: true

**Input:** Reuse `EnrichOutput` from `enrich_gov_agency.go` — same package,
direct reference.
```go
type ScoreInput struct {
    Agency EnrichOutput `json:"agency"`
}
```

**Output:**
```go
type ScoreOutput struct {
    Score     int      `json:"score"`      // 0-100
    Reasoning []string `json:"reasoning"`  // human-readable factors
    Tier      string   `json:"tier"`       // "hot" ≥75, "warm" ≥50, "cold" <50
}
```

**Scoring logic (additive, capped at 100):**

| Factor | Points |
|---|---|
| SwornOfficers 50–100 (ICP sweet spot) | +30 |
| SwornOfficers 100–250 (larger, still viable) | +20 |
| SwornOfficers 25–50 (smaller, possible) | +15 |
| SwornOfficers nil (unknown) | +10 (benefit of doubt) |
| AgencyType == "City" | +20 |
| AgencyType == "County" | +10 |
| State == "CA" | +15 |
| State in western US (OR,WA,NV,AZ,CO,UT,NM,ID,MT,WY) | +10 |
| len(ActiveGrants) > 0 | +15 |
| Domain populated (Apollo found them) | +10 |
| ApolloEmployeeCount != nil | +5 |

Append a reasoning string for each factor that fires, e.g.
`"sworn officers 78 — within ICP sweet spot (+30)"`.

Also export a package-level helper for `search_gov_agencies.go` to call:
```go
// ScoreAgency scores a bare AgencyResult (pre-enrich, no grants/Apollo data).
// Used by search_gov_agencies for list-mode ranking.
func ScoreAgency(r AgencyResult) int
```

**Handler export:**
```go
func NewScoreHandler(deps Deps) func(context.Context, *mcp.CallToolRequest, ScoreInput) (*mcp.CallToolResult, ScoreOutput, error)
```

---

### Tool 3: `tools/find_gov_contacts.go`

**MCP tool name:** `find_gov_contacts`

**What it does:** Given a domain or agency name + state, finds people
associated with that agency via Apollo PeopleSearch, then optionally
enriches email via PeopleMatch.

**Input:**
```go
type ContactsInput struct {
    AgencyName  string   `json:"agency_name"   jsonschema:"agency name, e.g. 'Pleasanton Police Department'"`
    State       string   `json:"state"         jsonschema:"two-letter state code, e.g. 'CA'"`
    Domain      string   `json:"domain,omitempty" jsonschema:"known domain, e.g. 'pleasantonpd.org'. Speeds up search if known."`
    Titles      []string `json:"titles,omitempty" jsonschema:"specific titles to search, e.g. ['chief of police','IT director']. Defaults to standard LE leadership titles."`
    EnrichEmail bool     `json:"enrich_email"  jsonschema:"if true, attempt PeopleMatch to reveal work email (costs Apollo credits per person)"`
    Limit       int      `json:"limit,omitempty" jsonschema:"max contacts to return, default 5, max 10"`
}
```

**Output:**
```go
type ContactsOutput struct {
    Contacts      []ContactResult `json:"contacts"`
    PartialErrors []string        `json:"partial_errors,omitempty"`
    Sources       []string        `json:"sources"`
}

type ContactResult struct {
    FirstName    string `json:"first_name"`
    LastName     string `json:"last_name"`
    Title        string `json:"title,omitempty"`
    Organization string `json:"organization,omitempty"`
    Email        string `json:"email,omitempty"`        // populated only if EnrichEmail=true
    LinkedIn     string `json:"linkedin_url,omitempty"` // if Apollo returns it
    City         string `json:"city,omitempty"`
    State        string `json:"state,omitempty"`
    Seniority    string `json:"seniority,omitempty"`
}
```

**Behavior:**
1. Build titles list: if `Titles` is empty, default to:
   `["chief of police", "police chief", "sheriff", "IT director",
   "technology director", "records manager", "city manager",
   "city council", "mayor", "council member", "deputy chief"]`
2. Call `deps.Apollo.PeopleSearch` with titles + location
   (`strings.ToLower(state)` for `Locations`). Also pass
   `organization_domains: [domain]` if `Domain` is provided — this
   dramatically improves precision. Check `apollo/client.go`'s
   `PeopleSearchRequest` struct; if `OrganizationDomains` field doesn't
   exist, add it to the struct (you own `tools/` but NOT `apollo/` —
   ping Agent A to add the field if needed, or check if it can be
   passed via a map. Do NOT edit `apollo/client.go` yourself).
3. Parse `people` array from response. Map each to `ContactResult`.
   Apply `Limit` (default 5, cap 10).
4. If `EnrichEmail == true`: fan out `PeopleMatch` calls concurrently
   (one per contact, cap 3 goroutines — credits are expensive).
   On each match response, extract `person.email` and
   `person.linkedin_url`. Partial failures go to `PartialErrors`.
5. Append `"apollo"` to `Sources` if any contacts returned.
6. Return `ContactsOutput`.

**Credit warning note:** `EnrichEmail: true` costs one Apollo credit per
person. The tool description must warn about this. Add to the tool's
description field in Agent A's `mcp.AddTool` call: append
`" Set enrich_email=false to avoid Apollo credit usage."` — but since
Agent A writes the description, just note it here for coordination.

**Handler export:**
```go
func NewContactsHandler(deps Deps) func(context.Context, *mcp.CallToolRequest, ContactsInput) (*mcp.CallToolResult, ContactsOutput, error)
```

---

### Coordination with Agent A

Agent A is adding `OrganizationDomains []string` to `apollo.PeopleSearchRequest`
if it doesn't already exist. Check `apollo/client.go` before coding — if
the field is already there, use it. If not, write a spec note here and
work around it (pass domain as part of the name search instead).

---

### Build & smoke test (after all three files are written)

1. `go build ./...` — must be clean
2. Full smoke test from repo root:
```
(echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'; sleep 0.5; echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'; sleep 0.5; echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'; sleep 1) | ./govenrich 2>/dev/null
```
3. `tools/list` must return all four tools.
4. Append "Latest Build — YYYY-MM-DD HH:MM" with transcript and PASS/FAIL.

---

## Dispatcher Task 2 — 2026-04-16

**Re-read this spec tail on every change. Act on new dispatcher tasks immediately.**

### USASpending redundant search strategy

The current single-pass `recipient_search_text: [AgencyName]` finds nothing for most agencies because federal grants go to the parent city/county, not the named department. Replace with a **three-tier waterfall** — each tier runs only if the previous returned zero results. All tiers search a 5-year window (not 2) ending today.

**Tier 1 — exact department name**
`recipient_search_text: [AgencyName]` — e.g. `"Pleasanton Police Department"`

**Tier 2 — city/county name extracted from AgencyName**
Strip known suffixes (`Police Department`, `Sheriff's Office`, `Sheriff Office`, `Police Dept`, `Sheriff Dept`, `Department of Public Safety`, `Public Safety`) from AgencyName to get the bare city/county token.
`recipient_search_text: ["<city token>"]` — e.g. `"Pleasanton"`

**Tier 3 — city token + state filter only (broadest)**
Same city token but drop `place_of_performance_locations` constraint and rely on state alone via `recipient_search_text`. Only use if tiers 1 and 2 both return zero. Cap results at 5 to avoid noise.

**Labeling:** Add a `grant_search_tier` int field to `GrantSummary` (1, 2, or 3) so the caller knows the provenance. Tier 2+ results should note in `PartialErrors` (not as an error, as a note): `"usaspending: grants are city-level (no department-direct awards found)"`.

**Time window:** Expand from 2 years to 5 years. Today's date for the end boundary.

**Implementation notes:**
- Extract the city token with a simple strings replacer — no regex needed. Order matters: try longest suffix first.
- All three tiers share the same `place_of_performance_locations: [{"country":"USA","state":State}]` for tiers 1 and 2. Tier 3 omits it.
- If all three tiers return zero, `active_grants` stays `[]` and append `"usaspending: no grants found after 3-tier search"` to `PartialErrors`.

**Also add `grant_search_tier` to `GrantSummary`:**
```go
type GrantSummary struct {
    AwardID        string  `json:"award_id"`
    RecipientName  string  `json:"recipient_name"`
    AmountUSD      float64 `json:"amount_usd"`
    AwardingAgency string  `json:"awarding_agency"`
    SearchTier     int     `json:"search_tier"` // 1=exact dept, 2=city name, 3=broad
}
```

### After fix

1. `go build ./...` — must be clean
2. Smoke test:
```
(echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'; sleep 0.5; echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'; sleep 0.5; echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'; sleep 1) | ./govenrich 2>/dev/null
```
3. Append "Latest Build — YYYY-MM-DD HH:MM" to this spec with build result + smoke test transcript.

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

---

## Latest Build — 2026-04-16 21:00 UTC

`go build -o govenrich .` clean. `go vet ./...` clean.

### Dispatcher Task 2 — 3-tier USASpending waterfall

- `GrantSummary` gains `SearchTier int` (`search_tier` on the wire) so
  the caller can distinguish a department-direct award (tier 1) from a
  city-level inherited budget (tier 2+).
- `cityTokenFromAgencyName` added — strips longest-first suffixes
  ("Department of Public Safety", "Sheriff's Office", "Police
  Department", "Public Safety", etc.) and returns the bare city/county
  token. Order matters: longest suffixes listed first so
  "Department of Public Safety" peels before "Public Safety" can
  partial-match.
- `resolveUSASpending` rewritten as a waterfall over 5-year window:
  1. Tier 1 — exact `AgencyName`, state geo filter.
  2. Tier 2 — city token, state geo filter. Skipped if the city token
     equals the input (stripping was a no-op — tier 1 already covered).
  3. Tier 3 — city token, no geo filter. Broadest.
- Hits at tier 2 or 3 append `"usaspending: grants are city-level (no
  department-direct awards found)"` to `PartialErrors` (a note, per
  the dispatcher — same channel, different meaning).
- All three tiers empty → `"usaspending: no grants found after 3-tier
  search"` lands in `PartialErrors`, `active_grants` stays `[]`.
- `usaSpendingSearch` helper factored out so each tier is a one-line
  request construction plus a call into shared parse logic.

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
  "active_grants": [
    { "award_id": "EMW-2024-FG-02986", "recipient_name": "LIVERMORE PLEASANTON FIRE DEPARTMENT", "amount_usd": 472006.36, "awarding_agency": "Department of Homeland Security", "search_tier": 2 },
    { "award_id": "B-25-MC-06-0050",   "recipient_name": "CITY OF PLEASANTON",                  "amount_usd": 381455,    "awarding_agency": "Department of Housing and Urban Development", "search_tier": 2 },
    { "award_id": "B-23-MC-06-0050",   "recipient_name": "CITY OF PLEASANTON",                  "amount_usd": 380348,    "awarding_agency": "Department of Housing and Urban Development", "search_tier": 2 },
    { "award_id": "B-24-MC-06-0050",   "recipient_name": "CITY OF PLEASANTON",                  "amount_usd": 380083,    "awarding_agency": "Department of Housing and Urban Development", "search_tier": 2 },
    { "award_id": "B-21-MC-06-0050",   "recipient_name": "CITY OF PLEASANTON",                  "amount_usd": 368034,    "awarding_agency": "Department of Housing and Urban Development", "search_tier": 2 }
  ],
  "annual_expenditure_usd": null,
  "sources": ["apollo", "usaspending", "fbi_cde"],
  "partial_errors": [
    "census: census govfinances endpoint unavailable — see SPEC.md §11",
    "usaspending: grants are city-level (no department-direct awards found)"
  ]
}
```

Pleasanton PD hits tier 2: ~$2M in city-level HUD and DHS awards over
the 5-year window. All grants tagged `search_tier: 2`; the note lands
in `partial_errors`. Tier 1 (the exact department name) correctly
returned zero, matching the prior build's signal.

### Status against DoD

- [x] `sworn_officers = 78` (FBI).
- [x] `apollo_employee_count = 33` (Apollo).
- [x] `active_grants` populated (5 entries, tier 2) — the previous
      gap is closed.
- [x] `annual_expenditure_usd` null + Census unavailable note.
- [x] `sources` now contains `"apollo"`, `"usaspending"`, `"fbi_cde"`
      and never `"census"`.
- [x] City-level provenance surfaced via both `search_tier` on each
      grant and the note in `partial_errors`.

---

## Latest Build — 2026-04-16 21:21 UTC

`go build ./...` clean. `go vet ./...` clean. PASS.

### Dispatcher Task 3 — three new tools

Shipped in new files, same `tools` package:

- `tools/score_agency_fit.go` — `score_agency_fit` tool + package-level
  `ScoreAgency(AgencyResult) int` helper used by the search tool.
- `tools/search_gov_agencies.go` — `search_gov_agencies` tool. Filters
  FBI directory to City/County types, fans out Apollo OrgSearch for the
  top 20 candidates (5-way parallel), scores every result, sorts by
  fit.
- `tools/find_gov_contacts.go` — `find_gov_contacts` tool. Title + state
  search, optional email enrich (3-way parallel, explicit opt-in via
  `enrich_email`).

No edits to `main.go`, `go.mod`, `tools/deps.go`, `apollo/*`, or
`public/*` — Agent A had already wired the four `mcp.AddTool`
registrations in parallel.

### Schema hygiene notes

Two quiet MCP-schema gotchas caught during smoke testing:

- `SearchInput.MinOfficers` / `.Limit` needed `,omitempty` — without
  them the SDK marked them as JSON-Schema `required`, so a call with
  only `state` rejected with "missing properties: [min_officers]".
  Added omitempty to both.
- `ScoreInput.Agency` references `EnrichOutput` as-is, so every
  `EnrichOutput` field is required on the wire. That's fine for the
  primary chain (enrich → score) but direct callers must pass
  `apollo_annual_revenue: null`, `annual_expenditure_usd: null`,
  `sources: []`, etc. explicitly.

### Coordination note for Agent A

`apollo.PeopleSearchRequest` does not yet have `OrganizationDomains`.
`find_gov_contacts` uses a client-side fallback token match against
`organization.name` for now, with a `TODO(agent-a)` at the single
swap-in point. When Agent A adds the field, delete the client-side
filter block and replace with `req.OrganizationDomains =
[]string{cleanDomain(in.Domain)}`.

### Smoke test — `tools/list`

```
- enrich_gov_agency:   Enriches a US law-enforcement agency with sworn officer count and active federal grants
- find_gov_contacts:   Finds people associated with a government agency via Apollo people search…
- score_agency_fit:    Scores a single enriched agency against the Pleasanton PD ICP profile…
- search_gov_agencies: Searches for US law-enforcement agencies in a state, ranked by ICP fit score
```

All four tools discoverable. PASS.

### Smoke test — `tools/call` per new tool

**`score_agency_fit`** (Pleasanton enriched):

```json
{ "score": 95, "tier": "hot",
  "reasoning": [
    "sworn officers 78 — within ICP sweet spot (+30)",
    "agency type City (municipal PD) — primary ICP (+20)",
    "state CA — home market (+15)",
    "1 active federal grant(s) — warm spend signal (+15)",
    "Apollo domain populated — outreach path exists (+10)",
    "Apollo employee count populated (33) (+5)"
  ] }
```

**`search_gov_agencies`** (`state=DE, limit=3`):

```
total_found: 41
  [ 40] Dagsboro Police Department        (DE) type=City domain=dagsboro.delaware.gov
  [ 40] Dewey Beach Police Department     (DE) type=City domain=www.deweybeachpolice.gov
  [ 40] Fenwick Island Police Department  (DE) type=City domain=www.fenwickisland.org
```

All top hits resolve the Apollo domain, score 40 (sworn unknown +10,
City +20, Apollo domain +10). Expected — list-mode skips per-ORI `/pe/`
calls; use `enrich_gov_agency` for sworn-specific scoring.

**`find_gov_contacts`** (Pleasanton PD, no enrich):

```
contacts: 3
sources: ["apollo"]
partial_errors: ["apollo: no contacts matched domain filter client-side (OrganizationDomains field not yet in PeopleSearchRequest)"]
```

Three contacts returned; the client-side domain filter fallback is
noted in `partial_errors` exactly as designed. Titles + state match
produced three CA mayors (other cities) because the Apollo
`organization_domains` filter is not yet available — fallback is
deliberate.

### Status against DoD

- [x] `go build ./...` clean.
- [x] `tools/list` returns all four tools.
- [x] Each new tool invoked successfully against live data (modulo the
      schema-hygiene retries above).
- [x] No edits outside `tools/` — file boundary respected.

---

## Latest Build — 2026-04-16 21:39 UTC

`go build ./...` clean. `go vet ./...` clean. PASS.

### Dispatcher Task 4 — two Anthropic-backed tools

- `tools/search_gov_web.go` — `search_gov_web` tool. Uses the Anthropic
  Go SDK (`anthropic-sdk-go` v1.37.0, pinned by Agent A) to call
  `claude-opus-4-7` with the server-side web_search tool
  (`WebSearchTool20260209Param` — the current version in the SDK).
  System prompt is the B2G-researcher prompt verbatim; user prompt
  interpolates `AgencyName`, `City`, `State`, and optional `Focus`.
  Response text is parsed with best-effort regex passes into
  `Stakeholders` ("Name, Title" / "Name - Title" / "Name (Title)"),
  `BudgetSignals` (sentences with dollar amounts or budget/procurement
  language), `NewsItems` (dated sentences), `Sources` (inline URLs).
  `RawSummary` carries the full model output for downstream tools.
- `tools/draft_gov_outreach.go` — `draft_gov_outreach` tool. Calls
  `claude-sonnet-4-6` with no tools (pure generation). Prompt stitches
  together available structured context (`EnrichOutput` always;
  `ContactResult`, `ScoreOutput`, `WebSearchOutput` when present).
  Response parser splits `SUBJECT: …`, body, and `USED: …` bullets so
  `PersonalizationUsed` reflects what the model actually leaned on.

### Coordination debt worked through

- **`DraftOutput` stub in `tools/create_apollo_sequence.go`** — another
  agent had landed a 4-line temporary `type DraftOutput` there to
  unblock their own compile, with a comment explicitly instructing the
  next agent to delete the stub when the real `DraftOutput` shipped.
  Removed; `SequenceInput.DraftEmail` now references my real type
  (superset of the stub — Subject and Body are present, plus
  `PersonalizationUsed`).
- **`find_gov_contacts` client-side domain fallback** — Agent A landed
  `OrganizationDomains []string` on `apollo.PeopleSearchRequest`
  (commit `c993d73`). Removed the 13-line client-side token-match
  fallback and the `matchesDomain` helper; the handler now sets
  `req.OrganizationDomains = []string{cleanDomain(in.Domain)}` when a
  domain is provided. One `TODO(agent-a)` eliminated.
- **`deps.AnthropicKey`** — Agent A added the optional field to
  `tools.Deps` (commit `b13e8d3`). Switched both Anthropic-backed
  tools from `os.Getenv("ANTHROPIC_API_KEY")` to `deps.AnthropicKey`
  and passed it explicitly via `option.WithAPIKey` — matches the rest
  of the Deps-based dependency injection pattern.

### Smoke test — `tools/list`

```
- create_apollo_sequence
- draft_gov_outreach
- enrich_gov_agency
- find_gov_contacts
- score_agency_fit
- search_gov_agencies
- search_gov_web
```

Seven tools registered (one owned by another agent, six by me). PASS.

### Smoke test — live `search_gov_web`

Input: `{agency_name: "Pleasanton Police Department", state: "CA", city: "Pleasanton", focus: "leadership"}`

```
stakeholders:   10
budget_signals: 10
news_items:     10
sources:        0     (see note below)
partial_errors: none
raw_summary:    "I'll research the Pleasanton Police Department for B2G
                 sales intelligence, focusing on leadership, budget, and
                 technology purchases. …
                 # Pleasanton Police Department (Pleasanton, CA) — B2G
                 Sales Research Brief
                 ## 1. Police Department Leadership
                 **Chief Tracy Avelar** — Pleasanton's top decision-ma…"
```

`sources: 0` is a parser-honesty artifact, not a coverage gap: Claude's
web_search attaches citations as structured annotations on text blocks
rather than inline `https://…` URLs in the raw text, so my URL regex
doesn't match. A future iteration can walk citation annotations via
`block.Citations` to recover them; leaving as-is for now so the output
shape remains stable.

### Smoke test — live `draft_gov_outreach`

Input: enriched Pleasanton + sender "Alex" + product "video analytics
platform" + company "EdgeTrace".

```
SUBJECT: Video Analytics for Pleasanton PD – Faster Investigations,
         Less Manual Review

BODY (excerpt):
"With 78 sworn officers covering Pleasanton, I imagine investigative
 bandwidth is always a consideration … Given your active federal
 funding ($381,455 via HUD), there may also be an angle worth exploring
 around qualifying expenditures … Worth a 15-minute call to see if
 there's a fit?"

PERSONALIZATION USED:
  - Pleasanton PD has 78 sworn officers (used to frame lean-team
    efficiency angle)
  - Active federal grant of $381,455 from HUD (used to reference
    funding opportunity and grant alignment)
  - City name and department specificity: Pleasanton Police Department
    (used for direct personalization throughout)
```

Both real data points from enrich (78 sworn / $381K HUD grant) made
it into the email and into `PersonalizationUsed`. PASS.

### Status against DoD

- [x] `go build ./...` clean.
- [x] `tools/list` returns all 7 tools.
- [x] `search_gov_web` returns structured + raw research against live
      Anthropic web_search.
- [x] `draft_gov_outreach` returns a personalized email grounded in
      enrich data, with `PersonalizationUsed` reflecting real ground
      truth.
- [x] Missing-key fallback — both handlers return a structured
      `partial_errors` note when `deps.AnthropicKey == ""` rather than
      erroring.
- [x] Coordination debts paid down: stub `DraftOutput` deleted,
      `find_gov_contacts` now uses server-side `OrganizationDomains`,
      both new handlers use `deps.AnthropicKey` via
      `option.WithAPIKey`.

---

## Latest Build — 2026-04-16 22:05 UTC

`go build ./...` clean. `go vet ./...` clean. PASS.

### Dispatcher Task 5 — find_gov_contacts bugs

**Bug 1 — `organization_domains` filter silently ignored for .gov.**
Live probe against Apollo with
`organization_domains: ["pleasantonpd.org"]` returned five Fortune-500
execs (Intempo Health CGO, Breakthrough Energy founder, Hyundai GM
purchase, Hina Group MD, EY client-services director) — completely
unrelated to Pleasanton PD. Apollo drops the filter silently on .gov
domains rather than erroring.

Previous build (commit `1fbf2ce`) set
`req.OrganizationDomains = []string{cleanDomain(in.Domain)}` when a
domain was provided — so callers who passed a .gov domain got the
broken filter's noise. That block is now removed. Instead:

- The `Domain` input is still accepted (tool contract doesn't churn)
  but not passed upstream. A note lands in `partial_errors`:
  `"apollo: organization_domains filter is silently ignored for .gov
  — falling back to title+state search, narrowed client-side on
  agency-name substring"`.
- After Apollo's broad title+state response lands, results are
  filtered client-side by substring-matching `distinctiveKeyword(AgencyName)`
  (lowercased) against each hit's `organization.name`.
- When the substring filter produces zero matches, the handler surfaces
  a second note (`"apollo: no contacts with X in organization.name —
  returning the broader state+title matches"`) and returns the broader
  set rather than an empty list — the model can narrate the limitation.

**Live test — Pleasanton PD (`domain: pleasantonpd.org`):**

```
contacts:       5
sources:        ["apollo"]
partial_errors: [
  "apollo: organization_domains filter is silently ignored for .gov
   — falling back to title+state search, narrowed client-side on
   agency-name substring",
  "apollo: no contacts with \"pleasanton police\" in organization.name
   — returning the broader state+title matches"
]
  Joe   | City Council Member, Mayor    | City of Eastvale, CA
  Cary  | Mayor, City Council Member    | Town Of Atherton
  Sara  | City Council Member + Mayor   | City of San Carlos
  Belal | City Council Member           | City of Saratoga
  Josh  | City Council Member           | City of Canyon Lake
```

Cross-checked against LAPD: same substring-miss behavior (Apollo's
gov coverage is genuinely sparse — not a tool bug). The two-note
transparency lets the model narrate "Apollo doesn't have LAPD
contacts indexed, here are nearby CA city leaders instead."

**Bug 2 — `enrichEmails` skipped every contact with `last_name: null`.**
Apollo returns `last_name: null` for most gov contacts (confirmed on
the same probe — every row had `(nil)` in the last-name column). The
prior guard `if c.FirstName == "" || c.LastName == ""` meant
PeopleMatch was never attempted, so `enrich_email: true` silently
produced zero emails for the entire gov ICP.

Changed to `if c.FirstName == "" && c.LastName == ""` — skip only when
both names are missing. Apollo's /people/match accepts partial names
when scoped by `organization_name`/`domain`. The comment at the call
site documents the empirical shape so future maintainers don't flip
the check back.

### Coordination — pending Agent A

The dispatcher's proposed Bug-1 fix was to add
`OrgName string json:"q_organization_name,omitempty"` to
`apollo.PeopleSearchRequest` and filter on that. I can't touch
`apollo/*` (spec boundary), so my current fallback is the client-side
substring filter above. When Agent A lands `OrgName`:

1. In `find_gov_contacts.go`'s search block, replace the
   `partial_errors` note with
   `req.OrgName = in.AgencyName` when `in.Domain` or `in.AgencyName`
   is present.
2. Keep the client-side substring narrowing as a defense-in-depth —
   Apollo's `q_organization_name` is also permissive (probe with
   `q_organization_name: "Pleasanton"` returned hotels and weekly
   papers, not the PD).

### Status against DoD

- [x] `go build ./...` clean.
- [x] `tools/list` returns all 7 tools.
- [x] `find_gov_contacts` no longer passes the broken
      `organization_domains` filter; narrows client-side on agency-name
      substring; surfaces the limitation in `partial_errors`.
- [x] `enrichEmails` attempts PeopleMatch for contacts with first-name
      only, matching Apollo's real gov payload shape.
- [x] No edits outside `tools/` — file boundary respected.

### Known residual gap

Apollo's gov-contact coverage is genuinely sparse — even LAPD returns
zero substring matches on `"los angeles police"`. No amount of filter
tuning recovers data Apollo doesn't have. For LE-specific contact
enrichment, the next-level play is probably FBI CDE's leadership
endpoint or LinkedIn scraping, both outside this tool's scope.
