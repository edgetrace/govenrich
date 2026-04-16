# GovEnrich — Phase 1 Spec (Revised)
**Language:** Go  
**Architecture decision:** Single Go MCP server exposing all tools — Apollo REST calls
and gov enrichment layer combined. No thevgergroup dependency. No Node.js sidecar.
One binary, full response control.

---

## Architecture

```
Claude Code / Claude Desktop
        │
        └── GovEnrich MCP Server (Go binary, stdio)
                │
                ├── Apollo REST API (direct HTTP calls)
                │       POST /mixed_companies/search
                │       GET  /organizations/enrich
                │       POST /mixed_people/api_search
                │       POST /people/match
                │       POST /emailer_campaigns/search
                │       POST /contacts
                │       POST /emailer_campaigns/{id}/add_contact_ids
                │
                ├── USASpending.gov (no key)
                │       POST /api/v2/search/spending_by_award/
                │
                ├── FBI Crime Data Explorer (free key)
                │       GET  /crime/fbi/cde/agency/byStateAbbr/{state}
                │
                └── Census API (no key)
                        GET  /data/2022/govfinances
```

---

## Project Structure

```
govenrich/
├── main.go                    # MCP server entry point, tool registry
├── go.mod
├── go.sum
├── .env.example
│
├── apollo/
│   └── client.go              # Apollo HTTP client, all 8 endpoint calls
│
├── public/
│   ├── usaspending.go         # USASpending.gov grant search
│   ├── fbi.go                 # FBI CDE agency + sworn officer lookup
│   └── census.go              # Census local govt finance
│
├── enrichment/
│   └── scorer.go              # ICP scoring logic against Pleasanton profile
│
└── tools/
    ├── search_gov_agencies.go  # MCP tool 1
    ├── enrich_gov_agency.go    # MCP tool 2
    ├── score_agency_fit.go     # MCP tool 3
    └── draft_gov_outreach.go   # MCP tool 4
```

---

## Go Dependencies

```go
// go.mod
module github.com/edgetrace/govenrich

go 1.22

require (
    github.com/modelcontextprotocol/go-sdk v0.2.0  // MCP protocol
    github.com/joho/godotenv v1.5.1                // .env loading
)
```

---

## Environment Variables

```bash
# Apollo
APOLLO_API_KEY=your-key-here
APOLLO_BASE_URL=https://api.apollo.io/api/v1

# FBI Crime Data Explorer (free: https://api.data.gov/signup)
FBI_CDE_API_KEY=your-key-here

# Anthropic (for draft_gov_outreach tool)
ANTHROPIC_API_KEY=your-key-here

# USASpending — no key required
# Census     — no key required
```

---

## MCP Server Entry Point

```go
// main.go skeleton
func main() {
    mcp.NewServer("govenrich", "0.1.0").
        AddTool(tools.SearchGovAgencies).
        AddTool(tools.EnrichGovAgency).
        AddTool(tools.ScoreAgencyFit).
        AddTool(tools.DraftGovOutreach).
        ServeStdio()
}
```

**Transport:** stdio — standard for local Claude Code / Claude Desktop.  
**Claude Desktop config (`claude_desktop_config.json`):**
```json
{
  "mcpServers": {
    "govenrich": {
      "command": "/path/to/govenrich",
      "env": {
        "APOLLO_API_KEY": "your-key",
        "FBI_CDE_API_KEY": "your-key",
        "ANTHROPIC_API_KEY": "your-key"
      }
    }
  }
}
```

---

## Phase 1 Goal: Hello World to Every Endpoint

Before wiring MCP tools, validate every external HTTP call succeeds and
capture full response shapes. Implement all clients in `apollo/client.go`
and `public/*.go` with a `--hello-world` flag on `main.go` that runs
each call in sequence and exits.

**Expected output:**
```
[✓] Apollo  /auth/health                   200  key valid
[✓] Apollo  org search (CA LE keywords)    200  3 orgs returned
[✗] Apollo  org enrichment (.gov domain)   200  WARNING: employee_count=null, revenue=null
[✓] Apollo  people search (LE titles)      200  3 contacts, no email (expected)
[✓] Apollo  people enrichment             200  email revealed
[✓] Apollo  sequence search               200  N sequences found
[✓] Apollo  create contact                201  contact_id returned
[✓] Apollo  add to sequence               200  queued
[✓] FBI CDE agency list (CA)              200  sworn_officers populated
[✓] USASpending grants (CA LE)            200  grant amounts returned
[✓] Census  govt finance (CA)             200  expenditure by function returned
```

The `[✗]` on Apollo org enrichment is intentional and is the business case.
Apollo returns null for sworn officer count and revenue on `.gov` domains.
FBI CDE and Census calls immediately after demonstrate what GovEnrich fills.

---

## Apollo API Endpoints

**Base URL:** `https://api.apollo.io/api/v1`  
**Auth:** `Authorization: Bearer {APOLLO_API_KEY}` on all requests  
**Content-Type:** `application/json`  
**Note:** Steps 7 and 8 require a master API key, not a standard key.

---

### 1. Health Check
```
POST /auth/health
Body: {}
```
Fail fast here before burning credits on a bad key.  
Expected: `200 OK`

---

### 2. Organization Search
**⚠ Consumes credits.**
```
POST /mixed_companies/search
Body:
{
  "q_organization_keyword_tags": ["law enforcement", "police department", "sheriff"],
  "organization_locations":      ["california"],
  "per_page": 3,
  "page":     1
}
```
Capture: `organizations[].id`, `.name`, `.website_url`,
`.estimated_num_employees`, `.city`, `.state`, `.primary_phone`

---

### 3. Organization Enrichment
**⚠ Consumes credits.**
```
GET /organizations/enrich?domain=cityofpleasantonca.gov
```
Capture: `organization.estimated_num_employees`, `.annual_revenue`, `.industry`  
Expect nulls. Log explicitly — this is the gap GovEnrich fills.

---

### 4. People Search
No credits consumed. Returns name/title/org only — no contact info.
```
POST /mixed_people/api_search
Body:
{
  "person_titles":          ["chief of police", "police chief", "sheriff",
                             "IT director", "technology director", "records manager"],
  "person_seniorities":     ["c_suite", "vp", "director", "manager"],
  "organization_locations": ["california"],
  "per_page": 3,
  "page":     1
}
```
Capture: `people[].id`, `.name`, `.title`, `.organization.name`, `.organization.id`

---

### 5. People Enrichment
**⚠ Consumes credits.**
```
POST /people/match
Body:
{
  "first_name":             "Jane",
  "last_name":              "Smith",
  "organization_name":      "Pleasanton Police Department",
  "reveal_personal_emails": false
}
```
Capture: `person.email`, `.phone_numbers[].raw_number`, `.linkedin_url`

---

### 6. Search Sequences
No credits. Retrieves sequence IDs needed for step 8.
```
POST /emailer_campaigns/search
Body: { "per_page": 10, "page": 1 }
```
Capture: `emailer_campaigns[].id`, `.name`, `.status`

---

### 7. Create Contact
**Requires master API key.**  
Person must be a Contact in Apollo before they can join a sequence.
```
POST /contacts
Body:
{
  "first_name":        "Jane",
  "last_name":         "Smith",
  "title":             "Chief of Police",
  "email":             "jsmith@cityofpleasantonca.gov",
  "organization_name": "Pleasanton Police Department",
  "website_url":       "cityofpleasantonca.gov"
}
```
Capture: `contact.id` — passed directly to step 8.

---

### 8. Add Contact to Sequence
**Requires master API key.**
```
POST /emailer_campaigns/{sequence_id}/add_contact_ids
Body:
{
  "contact_ids":         ["contact_id_here"],
  "emailer_campaign_id": "sequence_id_here"
}
```
Expected: `200 OK`

---

## Public Data API Endpoints

---

### 9. FBI Crime Data Explorer
Canonical sworn officer count per agency. Apollo's biggest LE blind spot.  
**Free key required: https://api.data.gov/signup**

```
GET https://api.usa.gov/crime/fbi/cde/agency/byStateAbbr/CA
    ?API_KEY={FBI_CDE_API_KEY}
```

Capture per record: `ori` (canonical agency ID — use as primary join key),
`agency_name`, `city_name`, `agency_type`, `sworn_officers`

---

### 10. USASpending.gov
Federal grants to LE agencies. Active grant = warm signal for adjacent tech spend.  
**No key required.**

```
POST https://api.usaspending.gov/api/v2/search/spending_by_award/
Body:
{
  "filters": {
    "award_type_codes":      ["02", "03", "04", "05"],
    "recipient_search_text": ["police department"],
    "time_period": [{ "start_date": "2023-01-01", "end_date": "2024-12-31" }],
    "place_of_performance_locations": [{ "state": "CA" }]
  },
  "fields":  ["Award ID", "Recipient Name", "Award Amount", "Awarding Agency"],
  "page":    1,
  "limit":   5,
  "sort":    "Award Amount",
  "order":   "desc"
}
```
Capture: `results[].Recipient Name`, `.Award Amount`, `.Awarding Agency`

---

### 11. Census Local Government Finance
Annual expenditure by govt entity. Proxy for tech budget capacity.  
**No key required.**

```
GET https://api.census.gov/data/2022/govfinances
    ?get=NAME,GOVTYPE,EXPENDITURE,FUNCTION
    &for=place:*
    &in=state:06
```
(`state:06` = California FIPS)

Capture rows where `FUNCTION=05` (Police Protection) or `FUNCTION=61`
(Capital Outlay). Join to FBI data on city name.

---

## ICP Scoring Profile — Pleasanton PD (Ground Truth)

```go
// enrichment/scorer.go

type ICPProfile struct {
    AgencyType          string // "municipal_police"
    SwornOfficers       int    // 70
    CityPopulation      int    // 80000
    CamerasDeployed     int    // 20
    State               string // "CA"
    AnnualTechBudgetEst int    // 500000
    FederalGrantsActive bool   // true
    ARR                 int    // 5000
}

// Score returns 0-100 fit score and reasoning strings for outreach context
func Score(agency EnrichedAgency, icp ICPProfile) (int, []string)
```

Scoring dimensions:
- Sworn officer count within ±30% of ICP → high fit
- Active federal grant in past 24 months → binary boost
- State match (CA = highest, Western US = medium)
- Agency type match (municipal PD > county sheriff > state agency)
- Capital outlay budget above threshold

---

## MCP Tools (Phase 2 — after hello world passes)

### Tool 1: `search_gov_agencies`
```
Input:  state string, agency_type string, min_officers int
Output: []AgencyRecord ranked by ICP fit score
```
Apollo org search + FBI CDE for the state, joined on name/city.
Returns merged records with sworn officer count Apollo is missing.

---

### Tool 2: `enrich_gov_agency`
```
Input:  agency_name string, state string
Output: EnrichedAgency
        (Apollo fields + FBI sworn count + USASpending grants + Census budget)
```
Apollo org enrichment + FBI lookup by name + USASpending recipient search
+ Census expenditure lookup. Returns single unified struct.

---

### Tool 3: `score_agency_fit`
```
Input:  EnrichedAgency
Output: score int (0–100), reasoning []string
```
Pure Go logic, no external calls. Compares enriched record to Pleasanton ICP.

---

### Tool 4: `draft_gov_outreach`
```
Input:  EnrichedAgency, score int, reasoning []string
Output: subject string, body string
```
Calls Anthropic API with enriched context to generate a personalized
first-touch email. Prompt includes sworn officer count, active grants,
and ICP fit reasoning so the output is specific — not a generic cold email.
