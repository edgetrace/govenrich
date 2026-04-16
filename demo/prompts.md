# Demo prompts

Run these verbatim in Claude Desktop against `govenrich-stub`. Once Agent A's
real server is wired, swap `govenrich-stub` → `govenrich` and
`enrich_gov_agency_stub` → `enrich_gov_agency` in your mental script; the
prompt wording keeps working.

1. Use govenrich-stub to enrich Pleasanton Police Department (California) — I want sworn officer count and a one-line takeaway.

2. Now enrich Dunsmuir Police Department (California). If any field comes back null or "unavailable," call that out explicitly instead of papering over it — I want to see where Apollo falls short on small .gov domains.

3. Given the two enrichments above, draft a two-sentence cold-outreach opener to the Pleasanton chief of police that name-checks one concrete fact from the tool output.
