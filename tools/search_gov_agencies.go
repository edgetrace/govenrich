// search_gov_agencies.go — ranked list of LE agencies for a state, merged
// from FBI CDE directory + Apollo domain lookup. Does NOT fan out per-ORI
// PoliceEmployeeByORI calls — that's per-agency-expensive and belongs to
// enrich_gov_agency for single-agency work. See AGENT_B_SPEC.md Dispatcher
// Task 3 for the full contract.
package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/edgetrace/govenrich/apollo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------------------
// Tool I/O types
// ---------------------------------------------------------------------------

type SearchInput struct {
	State       string `json:"state"                  jsonschema:"two-letter US state code, e.g. 'CA'"`
	MinOfficers int    `json:"min_officers,omitempty" jsonschema:"minimum sworn officer count, e.g. 50. 0 or omitted = no filter. Requires enrich pass — noted but not applied at list mode."`
	Limit       int    `json:"limit,omitempty"        jsonschema:"max results to return, default 10, max 50"`
}

type SearchOutput struct {
	Agencies      []AgencyResult `json:"agencies"`
	TotalFound    int            `json:"total_found"`
	PartialErrors []string       `json:"partial_errors,omitempty"`
}

type AgencyResult struct {
	Name          string `json:"name"`
	City          string `json:"city,omitempty"`
	State         string `json:"state"`
	ORI           string `json:"ori,omitempty"`
	AgencyType    string `json:"agency_type,omitempty"`
	SwornOfficers *int   `json:"sworn_officers"`
	Domain        string `json:"domain,omitempty"`
	FitScore      int    `json:"fit_score"`
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

const (
	defaultSearchLimit = 10
	maxSearchLimit     = 50
	// Apollo domain lookup fan-out: capped to stay polite to Apollo and
	// to keep a state-wide search from blowing through many credits.
	apolloLookupCap      = 20
	apolloLookupParallel = 5
)

func NewSearchHandler(deps Deps) func(
	context.Context,
	*mcp.CallToolRequest,
	SearchInput,
) (*mcp.CallToolResult, SearchOutput, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
		out := SearchOutput{
			Agencies:      []AgencyResult{},
			PartialErrors: []string{},
		}

		state := strings.ToUpper(strings.TrimSpace(in.State))
		limit := in.Limit
		if limit <= 0 {
			limit = defaultSearchLimit
		}
		if limit > maxSearchLimit {
			limit = maxSearchLimit
		}

		// Step 1: FBI directory for the whole state.
		status, body, err := deps.FBI.AgenciesByState(state)
		if err != nil {
			out.PartialErrors = append(out.PartialErrors, "fbi agencies_by_state: "+err.Error())
			return nil, out, nil
		}
		if status != 200 {
			out.PartialErrors = append(out.PartialErrors, fmt.Sprintf("fbi agencies_by_state: HTTP %d", status))
			return nil, out, nil
		}

		raw := flattenAgencies(body)
		if len(raw) == 0 {
			out.PartialErrors = append(out.PartialErrors, "fbi: no agencies returned for state "+state)
			return nil, out, nil
		}

		// Step 2: filter to likely LE directory records (City or County
		// types). byStateAbbr also surfaces federal/state agencies we don't
		// target; keep the output focused.
		results := make([]AgencyResult, 0, len(raw))
		for _, a := range raw {
			at := strField(a, "agency_type_name")
			if at != "" && !isLETargetType(at) {
				continue
			}
			results = append(results, AgencyResult{
				Name:       strField(a, "agency_name"),
				City:       strField(a, "city_name"),
				State:      state,
				ORI:        strField(a, "ori"),
				AgencyType: at,
			})
		}
		if len(results) == 0 {
			out.PartialErrors = append(out.PartialErrors, "fbi: no city/county agencies after type filter")
			return nil, out, nil
		}
		out.TotalFound = len(results)

		if in.MinOfficers > 0 {
			out.PartialErrors = append(out.PartialErrors,
				fmt.Sprintf("min_officers=%d noted but not applied: sworn counts require per-ORI /pe/ calls — use enrich_gov_agency for per-agency sworn data", in.MinOfficers))
		}

		// Step 3: Apollo domain lookup for the first N candidates.
		// Bounded fan-out — capped by both count and parallelism.
		lookupCount := apolloLookupCap
		if len(results) < lookupCount {
			lookupCount = len(results)
		}
		resolveDomainsParallel(deps.Apollo, results[:lookupCount], state)

		// Step 4: score every result. ScoreAgency lives in
		// score_agency_fit.go and handles nil sworn counts gracefully.
		for i := range results {
			results[i].FitScore = ScoreAgency(results[i])
		}

		// Step 5: sort descending by FitScore; ties broken by name.
		sort.SliceStable(results, func(i, j int) bool {
			if results[i].FitScore != results[j].FitScore {
				return results[i].FitScore > results[j].FitScore
			}
			return results[i].Name < results[j].Name
		})

		if len(results) > limit {
			results = results[:limit]
		}
		out.Agencies = results
		return nil, out, nil
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// isLETargetType returns true for agency_type_name values that match the
// GovEnrich ICP — municipal PDs and county sheriffs. Federal/state types
// are excluded because we don't sell to them today.
func isLETargetType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "city", "county":
		return true
	}
	return false
}

// resolveDomainsParallel mutates the Domain field on each AgencyResult by
// hitting Apollo OrgSearch. Capped to apolloLookupParallel goroutines to
// stay polite to Apollo's API.
func resolveDomainsParallel(cli *apollo.Client, results []AgencyResult, state string) {
	sem := make(chan struct{}, apolloLookupParallel)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d := lookupDomain(cli, results[idx].Name, state)
			if d != "" {
				results[idx].Domain = d
			}
		}(i)
	}
	wg.Wait()
}

// lookupDomain issues a single OrgSearch call with the distinctive keyword
// and returns the first non-empty website_url found, or "" if no hit.
func lookupDomain(cli *apollo.Client, name, state string) string {
	kw := distinctiveKeyword(name)
	if kw == "" {
		return ""
	}
	_, body, err := cli.OrgSearch(apollo.OrgSearchRequest{
		KeywordTags: []string{kw},
		Locations:   []string{stateFullName(state)},
		PerPage:     1,
		Page:        1,
	})
	if err != nil {
		return ""
	}
	first := firstMatch(body, "accounts", "organizations")
	if first == nil {
		return ""
	}
	d := strField(first, "website_url")
	if d == "" {
		d = strField(first, "primary_domain")
	}
	return cleanDomain(d)
}
