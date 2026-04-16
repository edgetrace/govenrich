// find_gov_contacts.go — look up LE-leadership contacts for an agency via
// Apollo PeopleSearch, with optional email reveal via PeopleMatch. When a
// known domain is passed in, Apollo filters server-side via
// OrganizationDomains; otherwise the title + state filter is used
// unchanged.
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

		// Apollo's `organization_domains` filter is silently ignored on
		// .gov domains (live-probed), so we filter by
		// `q_organization_name` instead — verified working against
		// Vallejo PD. The client-side substring narrowing below stays
		// as defense-in-depth: `q_organization_name` is also permissive
		// (a bare token can pull in same-name hotels, weeklies, etc.).
		req := apollo.PeopleSearchRequest{
			Titles:    titles,
			Locations: []string{stateFullName(in.State)},
			OrgName:   in.AgencyName,
			PerPage:   limit,
			Page:      1,
		}

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

		// Client-side narrowing: match the requested agency name against
		// the Apollo organization.name substring. Apollo's LE-title search
		// across a whole state returns contacts from many agencies; the
		// dumb substring match on a distinctive token recovers a precision
		// pass that organization_domains should have given us server-side.
		if token := strings.ToLower(distinctiveKeyword(in.AgencyName)); token != "" {
			narrowed := make([]ContactResult, 0, len(contacts))
			for _, c := range contacts {
				if strings.Contains(strings.ToLower(c.Organization), token) {
					narrowed = append(narrowed, c)
				}
			}
			if len(narrowed) > 0 {
				contacts = narrowed
			} else {
				out.PartialErrors = append(out.PartialErrors,
					fmt.Sprintf("apollo: no contacts with %q in organization.name — returning the broader state+title matches", token))
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
			// Apollo frequently returns only first_name for gov
			// contacts (last_name = null). PeopleMatch accepts partial
			// names when scoped by organization, so skip only if both
			// names are missing.
			if c.FirstName == "" && c.LastName == "" {
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
