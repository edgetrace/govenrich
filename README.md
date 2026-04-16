# GovEnrich

MCP server that fills Apollo's blind spots on `.gov` law-enforcement agencies — sworn officer counts, federal grants, stakeholder research, and personalized outreach drafts. One Go binary, stdio transport.

## Tools

| Tool | What it does |
|---|---|
| `enrich_gov_agency` | Merges Apollo + FBI CDE + USASpending into a single agency record |
| `search_gov_agencies` | Lists agencies in a state ranked by ICP fit score |
| `score_agency_fit` | Scores an enriched agency 0–100 against the Pleasanton PD ICP |
| `find_gov_contacts` | Apollo people search by title/domain, optional email reveal |
| `search_gov_web` | Anthropic web search for council members, budgets, tech news |
| `draft_gov_outreach` | Personalized cold email grounded in enriched data |
| `create_apollo_sequence` | Creates Apollo contact and enrolls in a sequence |

## Setup

```bash
cp .env.example .env   # fill in APOLLO_API_KEY, FBI_CDE_API_KEY, ANTHROPIC_API_KEY
go build -o govenrich .
```

Keys:
- `APOLLO_API_KEY` — standard key works for enrichment; master key required for `create_apollo_sequence`
- `FBI_CDE_API_KEY` — free at <https://api.data.gov/signup>
- `ANTHROPIC_API_KEY` — required for `search_gov_web` and `draft_gov_outreach`

## Claude Desktop

Add to `~/Library/Application Support/Claude/claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "govenrich": {
      "command": "/absolute/path/to/govenrich"
    }
  }
}
```

The binary reads `.env` from its own directory, so no `env` block needed if `.env` is alongside the binary. `⌘Q` quit and relaunch after editing.

## Demo prompt

> *"Research Vallejo Police Department in CA — enrich the agency, find IT and leadership contacts, score their fit, and draft an outreach email from Alex at EdgeTrace for our video analytics platform."*

## Smoke test

```bash
{ echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}';
  echo '{"jsonrpc":"2.0","method":"notifications/initialized"}';
  echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}';
  sleep 1; } | ./govenrich
```

Expect 7 tools in the second response.

## Hello-world mode

```bash
./govenrich --hello-world   # runs all external API calls and prints a pass/fail table
```

Note: steps 7–8 (create contact, add to sequence) require a master Apollo key and mutate live state.
