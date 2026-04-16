# GovEnrich

MCP server that combines Apollo.io lead data with free government data sources
(FBI Crime Data Explorer, USASpending.gov, Census) to fill the gaps Apollo
leaves on `.gov` domains — most notably sworn officer counts and revenue on
law-enforcement agencies.

One Go binary, stdio transport, no Node sidecar.

## Status

**Phase 1** — connectivity check only. Four HTTP clients covering 11 external
endpoints are wired up behind a `--hello-world` flag that exercises each call
in sequence and prints a pass/fail table. MCP tool registration is not yet
wired.

See [`SPEC.md`](./SPEC.md) for the full Phase 1/2 spec, ICP scoring profile,
and the four MCP tools planned for Phase 2.

## Layout

```
main.go              # entry point, --hello-world driver
apollo/client.go     # Apollo REST client (8 endpoints)
public/
  fbi.go             # FBI CDE agencies-by-state
  usaspending.go     # USASpending grants search
  census.go          # Census local government finance
```

## Setup

1. `cp .env.example .env` and fill in:
   - `APOLLO_API_KEY` — Apollo.io key (master key required for contact/sequence
     endpoints; standard key is fine for the rest)
   - `FBI_CDE_API_KEY` — free, sign up at <https://api.data.gov/signup>
   - `ANTHROPIC_API_KEY` — Phase 2 only, for `draft_gov_outreach`
2. `go build -o govenrich`

## Usage

```
./govenrich --hello-world
```

Runs every external call against California law-enforcement data and prints:

```
[✓] Apollo  /auth/health                   200  key valid
[✓] Apollo  org search (CA LE keywords)    200  3 orgs returned
[✗] Apollo  org enrichment (.gov domain)   200  WARNING: employee_count=null, revenue=null
...
[✓] FBI CDE agency list (CA)               200  N agencies, sworn_officers populated on M
[✓] USASpending grants (CA LE)             200  5 awards returned
[✓] Census  govt finance (CA)              200  N rows of expenditure by function
```

The `[✗]` on Apollo's `.gov` enrichment is intentional — it's the business
case. FBI CDE and Census calls immediately after demonstrate the gap GovEnrich
fills.

Steps 2, 3, and 5 consume Apollo credits. Steps 7 and 8 require a master
Apollo key and will report `skipped — requires master API key` on a standard
key rather than failing the run.

## Claude Desktop config (Phase 2)

```json
{
  "mcpServers": {
    "govenrich": {
      "command": "/absolute/path/to/govenrich",
      "env": {
        "APOLLO_API_KEY": "...",
        "FBI_CDE_API_KEY": "...",
        "ANTHROPIC_API_KEY": "..."
      }
    }
  }
}
```
