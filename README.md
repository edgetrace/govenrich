# GovEnrich

MCP server that combines Apollo.io lead data with free government data sources
(FBI Crime Data Explorer, USASpending.gov, Census) to fill the gaps Apollo
leaves on `.gov` law-enforcement agencies. The headline gap as of this build
is **sworn officer count** — Apollo now populates city-total employee and
revenue figures on `.gov` domains, but does not provide LE-specific head
counts, which FBI CDE does.

One Go binary, stdio transport, no Node sidecar.

## Status

**Phase 1** — connectivity check. Four HTTP clients covering 11 external
endpoints exercised behind a `--hello-world` flag; pass/fail table on exit.

**Phase 2a (in flight)** — MCP transport wired. The no-flag path now serves
a single tool, `enrich_gov_agency`, over stdio. `search_gov_agencies`,
`score_agency_fit`, and `draft_gov_outreach` are planned for a follow-up
iteration (see [`SPEC.md`](./SPEC.md) §"MCP Tools").

## Layout

```
main.go                    # entry: MCP server (default) or --hello-world
apollo/client.go           # Apollo REST client (8 endpoints + helper)
public/
  fbi.go                   # FBI CDE agencies-by-state + police employee
  usaspending.go           # USASpending grants search
  census.go                # Census local government finance (stubbed)
tools/
  deps.go                  # frozen Deps contract (4 clients)
  enrich_gov_agency.go     # the Phase 2a MCP tool handler
```

## Setup

1. `cp .env.example .env` and fill in:
   - `APOLLO_API_KEY` — Apollo.io key. A **master** key is required for
     steps 6, 7, and 8 (`/emailer_campaigns/search`, `/contacts`,
     `/emailer_campaigns/{id}/add_contact_ids`); a standard key is fine for
     the rest.
   - `FBI_CDE_API_KEY` — free, sign up at <https://api.data.gov/signup>
   - `ANTHROPIC_API_KEY` — Phase 2 only, for `draft_gov_outreach`
2. `go build -o govenrich .`

## Usage

```
./govenrich               # serves MCP over stdio (default) — see "Running as an MCP server" below
./govenrich --hello-world # runs all 10 external calls against live APIs and exits
```

Example output on a master-key run against California LE data:

```
[✓] Apollo  org search (CA LE keywords)    200  3 matches (3 accounts, 0 orgs)
[✓] Apollo  org enrichment (.gov domain)   200  employees=470, revenue=$144473000 (city total — sworn-officer gap still needs FBI)
[✓] Apollo  people search (LE titles)      200  3 contacts, no email (expected)
[✗] Apollo  people enrichment              200  no email revealed
[✓] Apollo  sequence search                200  10 sequences found
[✓] Apollo  create contact                 200  contact_id returned
[✓] Apollo  add to sequence                200  queued
[✓] FBI CDE agency list (CA)               200  865 agencies (directory only; sworn_officers needs /pe/ endpoint)
[✓] USASpending grants (CA LE)             200  5 awards returned
[✗] Census  govt finance (CA)              404  HTTP 404
```

Remaining `[✗]` lines reflect real state, not code bugs: Apollo had no email
on file for the particular candidate in step 5, and the Census URL in SPEC.md
§11 returns 404 and needs to be rebuilt against the `timeseries` collection.

## Endpoint reference

| # | Call | Method | Cost | Mutates server state? |
|---|------|--------|------|-----------------------|
| 2 | Apollo `/mixed_companies/search` | POST | ⚠ credits | no |
| 3 | Apollo `/organizations/enrich` | GET | ⚠ credits | no |
| 4 | Apollo `/mixed_people/api_search` | POST | free | no |
| 5 | Apollo `/people/match` | POST | ⚠ credits | no |
| 6 | Apollo `/emailer_campaigns/search` | POST | free (master) | no |
| – | Apollo `/email_accounts` | GET | free (master) | no (helper for step 8) |
| 7 | Apollo `/contacts` | POST | free (master) | **yes — creates a Contact** |
| 8 | Apollo `/emailer_campaigns/{id}/add_contact_ids` | POST | free (master) | **yes — enrolls Contact in first sequence** |
| 9 | FBI CDE `byStateAbbr/CA` | GET | free | no |
| 10 | USASpending `spending_by_award/` | POST | free | no |
| 11 | Census `govfinances` (currently 404) | GET | free | no |

## Write-side effects — read before running with a master key

Steps 7 and 8 are the only calls that mutate remote state. On a master-key
run:

- **Step 7 creates one new Apollo Contact per run**, named after whoever
  Apollo's people search ranks first. Runs accumulate — there is no dedupe
  and no delete. Expect orphan test contacts in the workspace over time.
- **Step 8 enrolls that Contact in `sequences[0]`** — whatever sequence
  happens to be first in the master-key `/emailer_campaigns/search`
  response. That is *your first live sequence*.
- Step 7's payload sends only `first_name`, `last_name`, `title`,
  `organization_name` — no `email`. Apollo has not backfilled an email on
  these contacts in testing, so a successful step 8 has not actually sent
  mail. That guarantee is empirical, not contractual.

Steps 7 and 8 are not gated behind a flag today. If you do not want them
to run, use a standard (non-master) key; they will report
`skipped — requires master API key` and no state will change.

## Known issues

- **Census §11**: `https://api.census.gov/data/2022/govfinances` returns 404.
  The endpoint does not exist — government finance is published under the
  `timeseries` collection. Flagged in both `public/census.go` and `SPEC.md`
  pending a dataset rebuild.
- **FBI sworn officers**: `byStateAbbr` returns agency directory info only
  (ori, name, city, NIBRS status). Sworn counts come from the separate
  `PoliceEmployeeByORI(ori)` call against `/pe/agency/{ori}`, which the
  `enrich_gov_agency` tool wires up. SPEC §9 overstates what `byStateAbbr`
  provides on its own.
- **People enrichment (step 5)**: Apollo commonly has no email for an
  arbitrary LE search candidate. The `[✗]` is a real no-data signal, not a
  code error.

## Running as an MCP server

The default (no-flag) path serves MCP over stdio and registers
`enrich_gov_agency`. Apply one of the configs below to whichever client
you use. **This repo does not write to your client config files** —
copy/paste the block yourself and restart the client.

### Claude Desktop

File: `~/Library/Application Support/Claude/claude_desktop_config.json`
(macOS). Fully quit Claude Desktop (`⌘Q`, not window close) and relaunch
after editing — MCP subprocesses are only spawned on startup.

### Claude Code

File: `~/.claude.json` — add the same `mcpServers` block under whichever
project scope you want the server available in, then restart the Code
session.

### Shared config shape

```json
{
  "mcpServers": {
    "govenrich": {
      "command": "/absolute/path/to/govenrich",
      "env": {
        "APOLLO_API_KEY": "...",
        "FBI_CDE_API_KEY": "..."
      }
    }
  }
}
```

`ANTHROPIC_API_KEY` is only needed once `draft_gov_outreach` ships in a
later iteration; omit it for Phase 2a.

### Verifying without a client

Before wiring a GUI client, you can smoke-test the transport directly:

```sh
{ echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}';
  echo '{"jsonrpc":"2.0","method":"notifications/initialized"}';
  echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}';
  sleep 1; } | ./govenrich
```

Expect two JSON-RPC responses on stdout — the second lists
`enrich_gov_agency` with its input/output schema. Any non-JSON line on
stdout indicates transport corruption (a stray `fmt.Println` somewhere)
and the client will fail silently; fix it before touching the client
config.
