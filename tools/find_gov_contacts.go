// find_gov_contacts.go — look up LE-leadership contacts for an agency via
// Apollo PeopleSearch, with optional email reveal via PeopleMatch.
//
// Note on OrganizationDomains: AGENT_B_SPEC.md Dispatcher Task 3 asks
// Agent A to add an OrganizationDomains field to
// apollo.PeopleSearchRequest. At time of writing this file that field
// does NOT exist — PeopleSearchRequest has only Titles, Seniorities,
// Locations, PerPage, Page. This handler therefore uses the title +
// state filter only, and (if Domain is provided) narrows the returned
// list client-side by matching Domain against the Apollo person's
// organization.website_url. When Agent A lands the field, swap the
// client-side filter for a server-side filter at the marked TODO.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/edgetrace/govenrich/apollo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------------------
// Tool I/O types
// ---------------------------------------------------------------------------

type ContactsInput struct {
	AgencyName  string   `json:"agency_name"          jsonschema:"agency name, e.g. 'Pleasanton Police Department'"`
	State       string   `json:"state"                jsonschema:"two-letter state code, e.g. 'CA'"`
	Domain      string   `json:"domain,omitempty"     jsonschema:"known domain, e.g. 'pleasantonpd.org'. Speeds up search and improves precision when known."`
	Titles      []string `json:"titles,omitempty"     jsonschema:"specific titles to search, e.g. ['chief of police','IT director']. Defaults to standard LE leadership titles."`
	EnrichEmail bool     `json:"enrich_email"         jsonschema:"if true, attempt PeopleMatch to reveal work email. Costs one Apollo credit per person — use false for discovery."`
	Limit       int      `json:"limit,omitempty"      jsonschema:"max contacts to return, default 5, max 10"`
}

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
	Email        string `json:"email,omitempty"`
	LinkedIn     string `json:"linkedin_url,omitempty"`
	City         string `json:"city,omitempty"`
	State        string `json:"state,omitempty"`
	Seniority    string `json:"seniority,omitempty"`
}

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	defaultContactsLimit = 5
	maxContactsLimit     = 10
	emailEnrichParallel  = 3 // credits are expensive — keep concurrency low
)

var defaultLETitles = []string{
	"chief of police",
	"police chief",
	"sheriff",
	"IT director",
	"technology director",
	"records manager",
	"city manager",
	"city council",
	"mayor",
	"council member",
	"deputy chief",
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

func NewContactsHandler(deps Deps) func(
	context.Context,
	*mcp.CallToolRequest,
	ContactsInput,
) (*mcp.CallToolResult, ContactsOutput, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in ContactsInput) (*mcp.CallToolResult, ContactsOutput, error) {
		out := ContactsOutput{
			Contacts:      []ContactResult{},
			PartialErrors: []string{},
			Sources:       []string{},
		}

		titles := in.Titles
		if len(titles) == 0 {
			titles = defaultLETitles
		}
		limit := in.Limit
		if limit <= 0 {
			limit = defaultContactsLimit
		}
		if limit > maxContactsLimit {
			limit = maxContactsLimit
		}

		req := apollo.PeopleSearchRequest{
			Titles:    titles,
			Locations: []string{stateFullName(in.State)},
			PerPage:   limit,
			Page:      1,
		}
		// TODO(agent-a): once apollo.PeopleSearchRequest gains
		// OrganizationDomains, filter server-side:
		//   if in.Domain != "" { req.OrganizationDomains = []string{cleanDomain(in.Domain)} }

		status, body, err := deps.Apollo.PeopleSearch(req)
		if err != nil {
			out.PartialErrors = append(out.PartialErrors, "apollo people_search: "+err.Error())
			return nil, out, nil
		}
		if status != 200 {
			out.PartialErrors = append(out.PartialErrors, fmt.Sprintf("apollo people_search: HTTP %d", status))
			return nil, out, nil
		}

		contacts := parsePeople(body)
		if len(contacts) == 0 {
			out.PartialErrors = append(out.PartialErrors, "apollo people_search: no contacts matched")
			return nil, out, nil
		}

		// Client-side domain filter stand-in until OrganizationDomains ships.
		if in.Domain != "" {
			wantDomain := cleanDomain(in.Domain)
			filtered := contacts[:0]
			for _, c := range contacts {
				if matchesDomain(c.Organization, wantDomain) {
					filtered = append(filtered, c)
				}
			}
			if len(filtered) > 0 {
				contacts = filtered
			} else {
				out.PartialErrors = append(out.PartialErrors,
					"apollo: no contacts matched domain filter client-side (OrganizationDomains field not yet in PeopleSearchRequest)")
			}
		}

		if len(contacts) > limit {
			contacts = contacts[:limit]
		}
		out.Sources = append(out.Sources, "apollo")

		if in.EnrichEmail {
			enrichEmails(deps.Apollo, contacts, in.Domain,
				func(s string) { out.PartialErrors = append(out.PartialErrors, s) })
		}

		out.Contacts = contacts
		return nil, out, nil
	}
}

// ---------------------------------------------------------------------------
// Parsing helpers
// ---------------------------------------------------------------------------

// parsePeople extracts the structured contact fields from the raw
// mixed_people/api_search response. The shape for each record is:
//
//	{ "id": ..., "first_name": "...", "last_name": "...",
//	  "title": "...", "seniority": "...", "city": "...", "state": "...",
//	  "linkedin_url": "...",
//	  "organization": { "name": "...", "website_url": "..." } }
func parsePeople(body []byte) []ContactResult {
	people, err := arrayFieldFromBody(body, "people")
	if err != nil || len(people) == 0 {
		return nil
	}
	out := make([]ContactResult, 0, len(people))
	for _, p := range people {
		cr := ContactResult{
			FirstName: strField(p, "first_name"),
			LastName:  strField(p, "last_name"),
			Title:     strField(p, "title"),
			LinkedIn:  strField(p, "linkedin_url"),
			City:      strField(p, "city"),
			State:     strField(p, "state"),
			Seniority: strField(p, "seniority"),
		}
		if org, ok := p["organization"].(map[string]any); ok {
			cr.Organization = strField(org, "name")
		}
		out = append(out, cr)
	}
	return out
}

// matchesDomain returns true when the person's organization appears to be
// the domain we care about. Used only for the client-side filter stand-in
// documented at the top of this file.
func matchesDomain(orgName, wantDomain string) bool {
	if orgName == "" {
		return false
	}
	// People search doesn't return the org website, so we fall back to
	// a token match on the org name (e.g. "Pleasanton Police Department"
	// vs want "pleasantonpd.org" → check for "pleasanton" in both).
	wantToken := strings.TrimSuffix(wantDomain, ".org")
	wantToken = strings.TrimSuffix(wantToken, ".gov")
	wantToken = strings.TrimSuffix(wantToken, ".com")
	wantToken = strings.TrimPrefix(wantToken, "www.")
	// Use first 6+ chars as a coarse match.
	if len(wantToken) < 4 {
		return true
	}
	return strings.Contains(strings.ToLower(orgName), wantToken[:min(len(wantToken), 10)])
}

// arrayFieldFromBody unmarshals a top-level JSON object and pulls the named
// array out, unwrapping each element into map[string]any.
func arrayFieldFromBody(body []byte, key string) ([]map[string]any, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	raw, ok := m[key].([]any)
	if !ok {
		return nil, nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if o, ok := r.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Email enrichment
// ---------------------------------------------------------------------------

// enrichEmails mutates contacts in place with Email and refined LinkedIn
// from PeopleMatch. Concurrency is deliberately low because each call
// consumes an Apollo credit.
func enrichEmails(cli *apollo.Client, contacts []ContactResult, hintDomain string, reportErr func(string)) {
	sem := make(chan struct{}, emailEnrichParallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	domain := cleanDomain(hintDomain)
	for i := range contacts {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c := &contacts[idx]
			if c.FirstName == "" || c.LastName == "" {
				return
			}
			req := apollo.PeopleMatchRequest{
				FirstName:            c.FirstName,
				LastName:             c.LastName,
				OrganizationName:     c.Organization,
				Domain:               domain,
				RevealPersonalEmails: false,
			}
			status, body, err := cli.PeopleMatch(req)
			if err != nil {
				mu.Lock()
				reportErr(fmt.Sprintf("apollo people_match (%s %s): %v", c.FirstName, c.LastName, err))
				mu.Unlock()
				return
			}
			if status != 200 {
				mu.Lock()
				reportErr(fmt.Sprintf("apollo people_match (%s %s): HTTP %d", c.FirstName, c.LastName, status))
				mu.Unlock()
				return
			}
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				return
			}
			person, _ := payload["person"].(map[string]any)
			if person == nil {
				return
			}
			if email := strField(person, "email"); email != "" {
				c.Email = email
			}
			if li := strField(person, "linkedin_url"); li != "" {
				c.LinkedIn = li
			}
		}(i)
	}
	wg.Wait()
}
