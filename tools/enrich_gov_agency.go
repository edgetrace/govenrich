// Package tools implements the MCP tools registered by the govenrich server.
// This file exposes enrich_gov_agency — the Phase 2 tool that merges Apollo,
// FBI CDE, and USASpending responses into a single agency record. See
// AGENT_B_SPEC.md for the contract this file satisfies.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
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
	status, body, err := cli.OrgSearch(apollo.OrgSearchRequest{
		KeywordTags: []string{in.AgencyName},
		Locations:   []string{strings.ToLower(in.State)},
		PerPage:     3,
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

// resolveUSASpending queries recent federal grants whose recipient name
// contains the agency. An active award is a warm signal for adjacent tech
// spend, which matters to the ICP story.
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
	start := end.AddDate(-2, 0, 0)

	status, body, err := cli.SpendingByAward(public.USASpendingRequest{
		Filters: public.USASpendingFilters{
			AwardTypeCodes:      []string{"02", "03", "04", "05"},
			RecipientSearchText: []string{in.AgencyName},
			TimePeriod: []public.USASpendingTimePeriod{
				{StartDate: start.Format("2006-01-02"), EndDate: end.Format("2006-01-02")},
			},
			PlaceOfPerformanceLocations: []map[string]string{
				{"country": "USA", "state": strings.ToUpper(in.State)},
			},
		},
		Fields: []string{"Award ID", "Recipient Name", "Award Amount", "Awarding Agency"},
		Page:   1,
		Limit:  5,
		Sort:   "Award Amount",
		Order:  "desc",
	})
	if err != nil {
		addErr("usaspending: " + err.Error())
		return
	}
	if status != 200 {
		addErr(fmt.Sprintf("usaspending: HTTP %d", status))
		return
	}

	var payload map[string]any
	if jerr := json.Unmarshal(body, &payload); jerr != nil {
		addErr("usaspending parse: " + jerr.Error())
		return
	}
	rawResults, _ := payload["results"].([]any)
	if len(rawResults) == 0 {
		addErr("usaspending: no grants for " + in.AgencyName + " in last 2 years")
		return
	}

	grants := make([]GrantSummary, 0, len(rawResults))
	for _, r := range rawResults {
		rec, ok := r.(map[string]any)
		if !ok {
			continue
		}
		g := GrantSummary{
			AwardID:        strField(rec, "Award ID"),
			RecipientName:  strField(rec, "Recipient Name"),
			AwardingAgency: strField(rec, "Awarding Agency"),
		}
		if f, ok := rec["Award Amount"].(float64); ok {
			g.AmountUSD = f
		}
		grants = append(grants, g)
	}
	if len(grants) == 0 {
		addErr("usaspending: results present but none parseable")
		return
	}

	mu.Lock()
	out.ActiveGrants = grants
	mu.Unlock()
	addSource("usaspending")
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
// response. The authoritative shape (verified by Agent A against
// CA0011100 = Pleasanton PD) is:
//
//	{"actuals": {
//	   "Male Officers":   {"2020": 69, "2021": 69, "2022": 69, "2023": 64},
//	   "Female Officers": {"2020": 9,  "2021": 9,  "2022": 11, "2023": 9},
//	   "Male Civilians":  {...},  // NOT sworn — must be excluded
//	   "Female Civilians":{...},  // NOT sworn — must be excluded
//	 }, ...}
//
// Sworn total = Male Officers[year] + Female Officers[year] for the most
// recent year present in either map.
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

	latestYear := ""
	for y := range male {
		if y > latestYear {
			latestYear = y
		}
	}
	for y := range female {
		if y > latestYear {
			latestYear = y
		}
	}
	if latestYear == "" {
		return nil
	}

	total := 0
	if n := intPtr(male[latestYear]); n != nil {
		total += *n
	}
	if n := intPtr(female[latestYear]); n != nil {
		total += *n
	}
	if total == 0 {
		// A year key existed but both officer counts were zero or missing —
		// treat that as no-data rather than claim a zero sworn force.
		return nil
	}
	return &total
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
