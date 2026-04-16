// Package tools implements the MCP tools registered by the govenrich server.
// This file exposes enrich_gov_agency — the Phase 2 tool that merges Apollo,
// FBI CDE, and USASpending responses into a single agency record. See
// AGENT_B_SPEC.md for the contract this file satisfies.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edgetrace/govenrich/apollo"
	"github.com/edgetrace/govenrich/public"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------------------
// Input / Output types (referenced by Agent A via name, shape owned here)
// ---------------------------------------------------------------------------

type EnrichInput struct {
	AgencyName string `json:"agency_name" jsonschema:"name of the agency, e.g. 'Pleasanton Police Department'"`
	State      string `json:"state"       jsonschema:"two-letter US state code, e.g. 'CA'"`
}

type EnrichOutput struct {
	Name   string `json:"name"`
	Domain string `json:"domain,omitempty"`
	City   string `json:"city,omitempty"`
	State  string `json:"state"`

	// Apollo populates these on .gov now (city totals, not LE-specific).
	// Pointers so a true miss serializes as null.
	ApolloEmployeeCount *int `json:"apollo_employee_count"`
	ApolloAnnualRevenue *int `json:"apollo_annual_revenue"`

	// FBI CDE — the real null-gap demo. Apollo cannot provide this;
	// FBI /pe/agency/{ori} can.
	ORI           string `json:"ori,omitempty"`
	AgencyType    string `json:"agency_type,omitempty"`
	SwornOfficers *int   `json:"sworn_officers"`

	// USASpending — warm signal for adjacent tech spend.
	ActiveGrants []GrantSummary `json:"active_grants"`

	// Census — unavailable via API (SPEC §11). Field stays nil and the
	// unavailable note lands in PartialErrors.
	AnnualExpenditureUSD *int `json:"annual_expenditure_usd"`

	// Provenance — which APIs contributed at least one field.
	Sources []string `json:"sources"`
	// Per-source failures surface here instead of failing the whole call.
	PartialErrors []string `json:"partial_errors,omitempty"`
}

type GrantSummary struct {
	AwardID        string  `json:"award_id"`
	RecipientName  string  `json:"recipient_name"`
	AmountUSD      float64 `json:"amount_usd"`
	AwardingAgency string  `json:"awarding_agency"`
	// SearchTier records how we found the grant: 1 = exact department
	// name, 2 = city/county token with state geo filter, 3 = city token
	// without geo filter. Tiers 2 and 3 indicate "city-level" awards
	// where the agency inherits budget through its parent jurisdiction.
	SearchTier int `json:"search_tier"`
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// NewEnrichHandler returns the typed MCP handler for enrich_gov_agency.
// Agent A calls this from main.go's MCP server setup.
func NewEnrichHandler(deps Deps) func(
	context.Context,
	*mcp.CallToolRequest,
	EnrichInput,
) (*mcp.CallToolResult, EnrichOutput, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in EnrichInput) (*mcp.CallToolResult, EnrichOutput, error) {
		out := EnrichOutput{
			Name:          in.AgencyName,
			State:         strings.ToUpper(strings.TrimSpace(in.State)),
			ActiveGrants:  []GrantSummary{},
			Sources:       []string{},
			PartialErrors: []string{},
		}

		var mu sync.Mutex
		var wg sync.WaitGroup

		addSource := func(s string) {
			mu.Lock()
			defer mu.Unlock()
			for _, existing := range out.Sources {
				if existing == s {
					return
				}
			}
			out.Sources = append(out.Sources, s)
		}
		addErr := func(s string) {
			mu.Lock()
			out.PartialErrors = append(out.PartialErrors, s)
			mu.Unlock()
		}

		// Fan out. Each resolver populates its own slice of EnrichOutput
		// fields under mu. Errors fall through as PartialErrors; we never
		// return a hard error from this handler short of a panic.
		wg.Add(4)
		go func() {
			defer wg.Done()
			resolveApollo(ctx, deps.Apollo, in, &out, &mu, addSource, addErr)
		}()
		go func() {
			defer wg.Done()
			resolveFBI(ctx, deps.FBI, in, &out, &mu, addSource, addErr)
		}()
		go func() {
			defer wg.Done()
			resolveUSASpending(ctx, deps.USASpending, in, &out, addSource, addErr, &mu)
		}()
		go func() {
			defer wg.Done()
			// Census is stubbed — call it for provenance honesty. The stub
			// always errors; do not append "census" to Sources.
			if _, _, cerr := deps.Census.LocalGovFinanceStub(stateFIPS(in.State)); cerr != nil {
				addErr("census: " + cerr.Error())
			}
		}()

		wg.Wait()
		return nil, out, nil
	}
}

// ---------------------------------------------------------------------------
// Per-source resolvers
// ---------------------------------------------------------------------------

// resolveApollo runs OrgSearch to find the agency's domain, then OrgEnrich to
// pull employee count and revenue. These fields are now populated for .gov
// domains — they are city-wide totals, not LE-specific, and the demo uses
// them as a contrast against FBI's sworn-officer count.
func resolveApollo(
	_ context.Context,
	cli *apollo.Client,
	in EnrichInput,
	out *EnrichOutput,
	mu *sync.Mutex,
	addSource func(string),
	addErr func(string),
) {
	// Apollo's keyword-tag search matches poorly on the full agency name
	// ("Pleasanton Police Department" → 0 hits) but cleanly on a distinctive
	// prefix ("pleasanton police" → 3 hits). Strip trailing LE suffixes and
	// expand the state abbreviation — Apollo expects full state names in
	// organization_locations.
	status, body, err := cli.OrgSearch(apollo.OrgSearchRequest{
		KeywordTags: []string{distinctiveKeyword(in.AgencyName)},
		Locations:   []string{stateFullName(in.State)},
		PerPage:     5,
		Page:        1,
	})
	if err != nil {
		addErr("apollo org_search: " + err.Error())
		return
	}
	if status != 200 {
		addErr(fmt.Sprintf("apollo org_search: HTTP %d", status))
		return
	}

	// mixed_companies/search splits matches across `accounts` (in-workspace)
	// and `organizations` (Apollo dataset). Check both.
	first := firstMatch(body, "accounts", "organizations")
	if first == nil {
		addErr("apollo: no org match for " + in.AgencyName)
		return
	}

	domain := strField(first, "website_url")
	if domain == "" {
		domain = strField(first, "primary_domain")
	}
	city := strField(first, "city")

	// If we matched but have no domain, we still learned something from
	// OrgSearch — record the city but stop short of OrgEnrich.
	if domain == "" {
		mu.Lock()
		if out.City == "" {
			out.City = city
		}
		mu.Unlock()
		addSource("apollo")
		addErr("apollo: match found but no website_url — skipping enrich")
		return
	}

	estatus, ebody, eerr := cli.OrgEnrich(cleanDomain(domain))
	if eerr != nil {
		addErr("apollo org_enrich: " + eerr.Error())
		return
	}
	if estatus != 200 {
		addErr(fmt.Sprintf("apollo org_enrich: HTTP %d", estatus))
		return
	}

	var payload map[string]any
	if jerr := json.Unmarshal(ebody, &payload); jerr != nil {
		addErr("apollo org_enrich parse: " + jerr.Error())
		return
	}
	org, _ := payload["organization"].(map[string]any)
	if org == nil {
		addErr("apollo org_enrich: no organization in payload")
		return
	}

	empCount := intPtr(org["estimated_num_employees"])
	rev := intPtr(org["annual_revenue"])
	enrichCity := strField(org, "city")
	enrichDomain := strField(org, "website_url")
	if enrichDomain == "" {
		enrichDomain = strField(org, "primary_domain")
	}

	mu.Lock()
	out.ApolloEmployeeCount = empCount
	out.ApolloAnnualRevenue = rev
	if out.City == "" {
		if enrichCity != "" {
			out.City = enrichCity
		} else {
			out.City = city
		}
	}
	if out.Domain == "" {
		if enrichDomain != "" {
			out.Domain = cleanDomain(enrichDomain)
		} else {
			out.Domain = cleanDomain(domain)
		}
	}
	mu.Unlock()
	addSource("apollo")
}

// resolveFBI matches the agency in the state-wide CDE directory, captures the
// ORI, then hits the per-ORI /pe/ endpoint for the sworn-officer count. This
// is the demo's core reveal — the field Apollo cannot populate.
func resolveFBI(
	_ context.Context,
	cli *public.FBIClient,
	in EnrichInput,
	out *EnrichOutput,
	mu *sync.Mutex,
	addSource func(string),
	addErr func(string),
) {
	status, body, err := cli.AgenciesByState(strings.ToUpper(in.State))
	if err != nil {
		addErr("fbi agencies_by_state: " + err.Error())
		return
	}
	if status != 200 {
		addErr(fmt.Sprintf("fbi agencies_by_state: HTTP %d", status))
		return
	}

	agencies := flattenAgencies(body)
	if len(agencies) == 0 {
		addErr("fbi: no agencies returned for state " + in.State)
		return
	}

	match := matchAgency(agencies, in.AgencyName)
	if match == nil {
		addErr("fbi: no directory match for " + in.AgencyName)
		return
	}

	ori := strField(match, "ori")
	agencyType := strField(match, "agency_type_name")
	city := strField(match, "city_name")
	fullName := strField(match, "agency_name")

	mu.Lock()
	out.ORI = ori
	out.AgencyType = agencyType
	if out.City == "" {
		out.City = city
	}
	if fullName != "" {
		out.Name = fullName
	}
	mu.Unlock()
	addSource("fbi_cde")

	if ori == "" {
		addErr("fbi: directory match has no ORI — cannot fetch sworn count")
		return
	}

	// PoliceEmployeeByORI owns its from/to year window internally. We pick
	// the most recent populated year from the actuals map in the response.
	peStatus, peBody, peErr := cli.PoliceEmployeeByORI(ori)
	if peErr != nil {
		addErr("fbi police_employee: " + peErr.Error())
		return
	}
	if peStatus != 200 {
		addErr(fmt.Sprintf("fbi police_employee: HTTP %d", peStatus))
		return
	}

	sworn := extractSwornCount(peBody)
	if sworn == nil {
		addErr("fbi police_employee: sworn count not present in payload")
		return
	}

	mu.Lock()
	out.SwornOfficers = sworn
	mu.Unlock()
}

// resolveUSASpending queries federal grants in a three-tier waterfall
// over a 5-year trailing window. Most LE agencies receive zero
// department-direct awards — the money flows to the parent city/county —
// so a single pass on the exact agency name misses the signal. Each tier
// runs only if the previous returned nothing.
//
//	Tier 1: recipient = AgencyName, state geo filter              (e.g. "Pleasanton Police Department")
//	Tier 2: recipient = city/county token, state geo filter        (e.g. "Pleasanton")
//	Tier 3: recipient = city/county token, no geo filter (broadest)
//
// Hits at tier 2+ are noted in PartialErrors so the caller can tell a
// "department-direct" signal from a "city-level inherited budget"
// signal — they have different meanings for outreach.
func resolveUSASpending(
	_ context.Context,
	cli *public.USASpendingClient,
	in EnrichInput,
	out *EnrichOutput,
	addSource func(string),
	addErr func(string),
	mu *sync.Mutex,
) {
	end := time.Now()
	start := end.AddDate(-5, 0, 0)
	startStr, endStr := start.Format("2006-01-02"), end.Format("2006-01-02")
	stateUpper := strings.ToUpper(strings.TrimSpace(in.State))

	baseReq := public.USASpendingRequest{
		Filters: public.USASpendingFilters{
			AwardTypeCodes: []string{"02", "03", "04", "05"},
			TimePeriod: []public.USASpendingTimePeriod{
				{StartDate: startStr, EndDate: endStr},
			},
			PlaceOfPerformanceLocations: []map[string]string{
				{"country": "USA", "state": stateUpper},
			},
		},
		Fields: []string{"Award ID", "Recipient Name", "Award Amount", "Awarding Agency"},
		Page:   1,
		Limit:  5,
		Sort:   "Award Amount",
		Order:  "desc",
	}

	cityToken := cityTokenFromAgencyName(in.AgencyName)
	cityLevelNote := "usaspending: grants are city-level (no department-direct awards found)"

	// Tier 1 — exact department name.
	t1 := baseReq
	t1.Filters.RecipientSearchText = []string{in.AgencyName}
	grants, err := usaSpendingSearch(cli, t1, 1)
	if err != nil {
		addErr("usaspending tier1: " + err.Error())
		return
	}

	// Tier 2 — city token with geo filter. Skip if the city token equals
	// the original (stripping produced nothing) — the request would just
	// duplicate tier 1.
	if len(grants) == 0 && cityToken != "" && !strings.EqualFold(cityToken, in.AgencyName) {
		t2 := baseReq
		t2.Filters.RecipientSearchText = []string{cityToken}
		g2, err := usaSpendingSearch(cli, t2, 2)
		if err != nil {
			addErr("usaspending tier2: " + err.Error())
		} else if len(g2) > 0 {
			grants = g2
			addErr(cityLevelNote)
		}
	}

	// Tier 3 — city token, no geo filter. Broadest net.
	if len(grants) == 0 && cityToken != "" {
		t3 := baseReq
		t3.Filters.RecipientSearchText = []string{cityToken}
		t3.Filters.PlaceOfPerformanceLocations = nil
		g3, err := usaSpendingSearch(cli, t3, 3)
		if err != nil {
			addErr("usaspending tier3: " + err.Error())
		} else if len(g3) > 0 {
			grants = g3
			addErr(cityLevelNote)
		}
	}

	if len(grants) == 0 {
		addErr("usaspending: no grants found after 3-tier search")
		return
	}

	mu.Lock()
	out.ActiveGrants = grants
	mu.Unlock()
	addSource("usaspending")
}

// usaSpendingSearch runs one tier of the waterfall — issues the request,
// parses results, and stamps SearchTier on every GrantSummary.
func usaSpendingSearch(
	cli *public.USASpendingClient,
	req public.USASpendingRequest,
	tier int,
) ([]GrantSummary, error) {
	status, body, err := cli.SpendingByAward(req)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("HTTP %d", status)
	}
	var payload map[string]any
	if jerr := json.Unmarshal(body, &payload); jerr != nil {
		return nil, fmt.Errorf("parse: %w", jerr)
	}
	rawResults, _ := payload["results"].([]any)
	out := make([]GrantSummary, 0, len(rawResults))
	for _, r := range rawResults {
		rec, ok := r.(map[string]any)
		if !ok {
			continue
		}
		g := GrantSummary{
			AwardID:        strField(rec, "Award ID"),
			RecipientName:  strField(rec, "Recipient Name"),
			AwardingAgency: strField(rec, "Awarding Agency"),
			SearchTier:     tier,
		}
		if f, ok := rec["Award Amount"].(float64); ok {
			g.AmountUSD = f
		}
		out = append(out, g)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// flattenAgencies collapses the county-keyed CDE response into a flat slice.
// Response shape: { "INYO": [{ori, agency_name, ...}], "LOS ANGELES": [...] }.
func flattenAgencies(body []byte) []map[string]any {
	var byCounty map[string]any
	if err := json.Unmarshal(body, &byCounty); err != nil {
		return nil
	}
	var out []map[string]any
	for _, v := range byCounty {
		list, ok := v.([]any)
		if !ok {
			continue
		}
		for _, x := range list {
			if o, ok := x.(map[string]any); ok {
				out = append(out, o)
			}
		}
	}
	return out
}

// matchAgency scores every agency record against the input name using a
// dumb, forgiving normalization. Good enough for the demo; precision later.
func matchAgency(agencies []map[string]any, target string) map[string]any {
	want := normalizeAgencyName(target)
	if want == "" {
		return nil
	}
	wantTokens := strings.Fields(want)

	var best map[string]any
	bestScore := 0
	for _, a := range agencies {
		got := normalizeAgencyName(strField(a, "agency_name"))
		if got == "" {
			continue
		}
		if got == want {
			return a
		}
		score := 0
		for _, t := range wantTokens {
			if len(t) < 3 {
				continue
			}
			if strings.Contains(got, t) {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			best = a
		}
	}
	// Require at least one non-trivial token to match (e.g. "Pleasanton").
	if bestScore == 0 {
		return nil
	}
	return best
}

// normalizeAgencyName lowercases, strips punctuation, and expands common
// abbreviations so "Pleasanton Police Dept." matches "Pleasanton Police
// Department".
func normalizeAgencyName(s string) string {
	s = strings.ToLower(s)
	// Replace punctuation with space rather than delete, so "police/sheriff"
	// doesn't become "policesheriff".
	replacer := strings.NewReplacer(
		".", " ", ",", " ", "'", "", "-", " ", "/", " ", "(", " ", ")", " ",
	)
	s = replacer.Replace(s)
	// Abbrev expansion.
	s = strings.ReplaceAll(s, " dept ", " department ")
	if strings.HasSuffix(s, " dept") {
		s = s[:len(s)-5] + " department"
	}
	return strings.Join(strings.Fields(s), " ")
}

// extractSwornCount reads the sworn-officer count from a /pe/agency/{ori}
// response. The authoritative shape (verified against CA0011100 =
// Pleasanton PD):
//
//	{"actuals": {
//	   "Male Officers":   {"2022": 69, "2023": 64, "2024": 68, "2025": null, "2026": null},
//	   "Female Officers": {"2022": 11, "2023": 9,  "2024": 10, "2025": null, "2026": null},
//	   "Male Civilians":  {...},  // NOT sworn — must be excluded
//	   "Female Civilians":{...},  // NOT sworn — must be excluded
//	 }, ...}
//
// FBI's refresh lag means the most recent years in the window are often
// `null`. We scan years in descending order and return the first year
// where either officer count has real data.
func extractSwornCount(body []byte) *int {
	var payload struct {
		Actuals map[string]map[string]any `json:"actuals"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	male := payload.Actuals["Male Officers"]
	female := payload.Actuals["Female Officers"]
	if len(male) == 0 && len(female) == 0 {
		return nil
	}

	// Collect the union of year keys across both maps so one-sided data
	// still surfaces.
	yearSet := make(map[string]struct{}, len(male)+len(female))
	for y := range male {
		yearSet[y] = struct{}{}
	}
	for y := range female {
		yearSet[y] = struct{}{}
	}
	years := make([]string, 0, len(yearSet))
	for y := range yearSet {
		years = append(years, y)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(years))) // "2026" > "2025" ... lexical works for 4-digit years

	for _, y := range years {
		m := intPtr(male[y])
		f := intPtr(female[y])
		if m == nil && f == nil {
			continue // both null — FBI hasn't refreshed this year yet
		}
		total := 0
		if m != nil {
			total += *m
		}
		if f != nil {
			total += *f
		}
		if total > 0 {
			return &total
		}
	}
	return nil
}

// firstMatch returns the first record found across the named top-level arrays
// in a JSON-decoded body.
func firstMatch(body []byte, keys ...string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	for _, k := range keys {
		list, ok := m[k].([]any)
		if !ok {
			continue
		}
		for _, x := range list {
			if o, ok := x.(map[string]any); ok {
				return o
			}
		}
	}
	return nil
}

func strField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// intPtr extracts a numeric field into *int. Returns nil for missing or
// non-numeric values so the output field serializes as JSON null.
func intPtr(v any) *int {
	switch t := v.(type) {
	case float64:
		i := int(t)
		return &i
	case int:
		return &t
	case json.Number:
		if i, err := t.Int64(); err == nil {
			n := int(i)
			return &n
		}
	}
	return nil
}

// cleanDomain strips scheme and path so the value is a bare host ready for
// apollo.OrgEnrich.
func cleanDomain(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	if idx := strings.Index(s, "/"); idx >= 0 {
		s = s[:idx]
	}
	return s
}

// stateFIPS maps a two-letter USPS abbr to a Census FIPS code. Used only by
// the Census stub call; exact values matter for future iteration when the
// real endpoint is wired.
func stateFIPS(abbr string) string {
	m := map[string]string{
		"AL": "01", "AK": "02", "AZ": "04", "AR": "05", "CA": "06",
		"CO": "08", "CT": "09", "DE": "10", "FL": "12", "GA": "13",
		"HI": "15", "ID": "16", "IL": "17", "IN": "18", "IA": "19",
		"KS": "20", "KY": "21", "LA": "22", "ME": "23", "MD": "24",
		"MA": "25", "MI": "26", "MN": "27", "MS": "28", "MO": "29",
		"MT": "30", "NE": "31", "NV": "32", "NH": "33", "NJ": "34",
		"NM": "35", "NY": "36", "NC": "37", "ND": "38", "OH": "39",
		"OK": "40", "OR": "41", "PA": "42", "RI": "44", "SC": "45",
		"SD": "46", "TN": "47", "TX": "48", "UT": "49", "VT": "50",
		"VA": "51", "WA": "53", "WV": "54", "WI": "55", "WY": "56",
		"DC": "11",
	}
	return m[strings.ToUpper(strings.TrimSpace(abbr))]
}

// stateFullName converts a USPS two-letter abbr to Apollo's expected
// organization_locations value (full lowercase state name). Falls back to
// the lowercased input so unknown codes still search something.
func stateFullName(abbr string) string {
	m := map[string]string{
		"AL": "alabama", "AK": "alaska", "AZ": "arizona", "AR": "arkansas",
		"CA": "california", "CO": "colorado", "CT": "connecticut",
		"DE": "delaware", "FL": "florida", "GA": "georgia", "HI": "hawaii",
		"ID": "idaho", "IL": "illinois", "IN": "indiana", "IA": "iowa",
		"KS": "kansas", "KY": "kentucky", "LA": "louisiana", "ME": "maine",
		"MD": "maryland", "MA": "massachusetts", "MI": "michigan",
		"MN": "minnesota", "MS": "mississippi", "MO": "missouri",
		"MT": "montana", "NE": "nebraska", "NV": "nevada",
		"NH": "new hampshire", "NJ": "new jersey", "NM": "new mexico",
		"NY": "new york", "NC": "north carolina", "ND": "north dakota",
		"OH": "ohio", "OK": "oklahoma", "OR": "oregon", "PA": "pennsylvania",
		"RI": "rhode island", "SC": "south carolina", "SD": "south dakota",
		"TN": "tennessee", "TX": "texas", "UT": "utah", "VT": "vermont",
		"VA": "virginia", "WA": "washington", "WV": "west virginia",
		"WI": "wisconsin", "WY": "wyoming", "DC": "district of columbia",
	}
	if n, ok := m[strings.ToUpper(strings.TrimSpace(abbr))]; ok {
		return n
	}
	return strings.ToLower(strings.TrimSpace(abbr))
}

// distinctiveKeyword strips trailing organizational suffixes from an agency
// name so Apollo's keyword-tag search matches. Apollo returns 0 hits for
// "Pleasanton Police Department" but 3 hits for "pleasanton police" — the
// "Department"/"Office"/etc. suffix poisons the phrase match.
func distinctiveKeyword(name string) string {
	tokens := strings.Fields(strings.ToLower(strings.TrimSpace(name)))
	stop := map[string]bool{
		"department": true, "dept": true, "dept.": true,
		"office": true, "services": true, "service": true,
		"bureau": true, "division": true, "agency": true,
	}
	for len(tokens) > 2 && stop[tokens[len(tokens)-1]] {
		tokens = tokens[:len(tokens)-1]
	}
	return strings.Join(tokens, " ")
}

// cityTokenFromAgencyName strips known LE/public-safety suffixes from an
// agency name, leaving the bare city/county token. Used by the USASpending
// tier-2/3 fallbacks — federal grants typically land with the parent
// jurisdiction, not the named department. Longest suffixes first so
// "Department of Public Safety" is stripped before "Public Safety" can
// partial-match something that already matched.
func cityTokenFromAgencyName(name string) string {
	s := strings.TrimSpace(name)
	suffixes := []string{
		"Department of Public Safety",
		"Sheriff's Department",
		"Sheriff's Office",
		"Police Department",
		"Sheriff Office",
		"Sheriff Department",
		"Public Safety",
		"Police Dept",
		"Sheriff Dept",
	}
	lower := strings.ToLower(s)
	for _, suf := range suffixes {
		ls := strings.ToLower(suf)
		if strings.HasSuffix(lower, " "+ls) {
			return strings.TrimSpace(s[:len(s)-len(suf)-1])
		}
		if lower == ls {
			return ""
		}
	}
	return s
}
