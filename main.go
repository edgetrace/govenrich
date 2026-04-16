// Command govenrich is the MCP server that combines Apollo.io lead data with
// free government data sources (FBI CDE, USASpending, Census) to fill the
// gaps Apollo leaves on .gov domains.
//
// Phase 1: --hello-world flag runs every external HTTP call in sequence so
// we can confirm credentials, endpoint shapes, and the Apollo null-gap
// business case before wiring MCP tools.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edgetrace/govenrich/apollo"
	"github.com/edgetrace/govenrich/public"
	"github.com/joho/godotenv"
)

const (
	pleasantonDomain = "cityofpleasantonca.gov"
	stateAbbr        = "CA"
	stateFIPS        = "06"
)

func main() {
	helloWorld := flag.Bool("hello-world", false, "run Phase 1 connectivity check against every external API and exit")
	flag.Parse()

	_ = godotenv.Load()

	if !*helloWorld {
		fmt.Println("govenrich — Phase 1 build. Run with --hello-world to verify external API connectivity. MCP transport not yet wired.")
		return
	}

	apolloKey := os.Getenv("APOLLO_API_KEY")
	if apolloKey == "" {
		fatal("APOLLO_API_KEY missing — copy .env.example to .env and fill it in")
	}
	fbiKey := os.Getenv("FBI_CDE_API_KEY")
	if fbiKey == "" {
		fatal("FBI_CDE_API_KEY missing — free key at https://api.data.gov/signup")
	}

	fmt.Println("govenrich hello-world — steps 2, 3, 5, 7 consume Apollo credits.")
	fmt.Println()

	ap := apollo.New(apolloKey, os.Getenv("APOLLO_BASE_URL"))
	fbi := public.NewFBIClient(fbiKey)
	spend := public.NewUSASpendingClient()
	census := public.NewCensusClient()

	// 1. Apollo /auth/health — fail fast before burning credits.
	status, body, err := ap.Health()
	report(err == nil && status == 200, "Apollo  /auth/health", status, err, healthNote(body))

	// 2. Apollo org search.
	status, body, err = ap.OrgSearch(apollo.OrgSearchRequest{
		KeywordTags: []string{"law enforcement", "police department", "sheriff"},
		Locations:   []string{"california"},
		PerPage:     3,
		Page:        1,
	})
	orgs, _ := arrayField(body, "organizations")
	report(err == nil && status == 200 && len(orgs) > 0,
		"Apollo  org search (CA LE keywords)", status, err,
		fmt.Sprintf("%d orgs returned", len(orgs)))

	// 3. Apollo org enrichment — .gov domain, expected null-gap.
	status, body, err = ap.OrgEnrich(pleasantonDomain)
	ok, note := enrichmentGap(body, err, status)
	report(ok, "Apollo  org enrichment (.gov domain)", status, err, note)

	// 4. Apollo people search — free, no contact info.
	status, body, err = ap.PeopleSearch(apollo.PeopleSearchRequest{
		Titles:      []string{"chief of police", "police chief", "sheriff", "IT director", "technology director", "records manager"},
		Seniorities: []string{"c_suite", "vp", "director", "manager"},
		Locations:   []string{"california"},
		PerPage:     3,
		Page:        1,
	})
	people, _ := arrayField(body, "people")
	var firstPerson map[string]any
	if len(people) > 0 {
		firstPerson = people[0]
	}
	report(err == nil && status == 200 && len(people) > 0,
		"Apollo  people search (LE titles)", status, err,
		fmt.Sprintf("%d contacts, no email (expected)", len(people)))

	// 5. Apollo people enrichment — reveals email using person from step 4.
	matchStatus := 0
	matchOK := false
	matchNote := "skipped — step 4 returned no candidates"
	if firstPerson != nil {
		matchStatus, body, err = ap.PeopleMatch(apollo.PeopleMatchRequest{
			FirstName:            strField(firstPerson, "first_name"),
			LastName:             strField(firstPerson, "last_name"),
			OrganizationName:     orgName(firstPerson),
			RevealPersonalEmails: false,
		})
		matchOK, matchNote = emailRevealed(body, err, matchStatus)
	}
	report(matchOK, "Apollo  people enrichment", matchStatus, nil, matchNote)

	// 6. Apollo sequence search.
	status, body, err = ap.SequenceSearch(apollo.SequenceSearchRequest{PerPage: 10, Page: 1})
	seqs, _ := arrayField(body, "emailer_campaigns")
	var firstSeqID string
	if len(seqs) > 0 {
		firstSeqID = strField(seqs[0], "id")
	}
	report(err == nil && status == 200, "Apollo  sequence search", status, err,
		fmt.Sprintf("%d sequences found", len(seqs)))

	// 7. Apollo create contact — master API key required.
	contactID := ""
	createStatus := 0
	createOK := false
	createNote := "skipped — step 4 returned no candidates"
	if firstPerson != nil {
		createStatus, body, err = ap.CreateContact(apollo.CreateContactRequest{
			FirstName:        strField(firstPerson, "first_name"),
			LastName:         strField(firstPerson, "last_name"),
			Title:            strField(firstPerson, "title"),
			OrganizationName: orgName(firstPerson),
		})
		switch {
		case err != nil:
			createNote = err.Error()
		case createStatus == 401 || createStatus == 403:
			createNote = "skipped — requires master API key"
		case createStatus == 200 || createStatus == 201:
			contactID = extractContactID(body)
			if contactID != "" {
				createOK = true
				createNote = "contact_id returned"
			} else {
				createNote = "2xx but no contact.id in payload"
			}
		default:
			createNote = fmt.Sprintf("unexpected status %d", createStatus)
		}
	}
	report(createOK, "Apollo  create contact", createStatus, nil, createNote)

	// 8. Apollo add contact to sequence — master API key required.
	addStatus := 0
	addOK := false
	addNote := ""
	switch {
	case contactID == "":
		addNote = "skipped — no contact_id from step 7"
	case firstSeqID == "":
		addNote = "skipped — no sequence from step 6"
	default:
		var addErr error
		addStatus, _, addErr = ap.AddContactToSequence(firstSeqID, apollo.AddContactToSequenceRequest{
			ContactIDs: []string{contactID},
		})
		switch {
		case addErr != nil:
			addNote = addErr.Error()
		case addStatus == 200:
			addOK = true
			addNote = "queued"
		case addStatus == 401 || addStatus == 403:
			addNote = "skipped — requires master API key"
		default:
			addNote = fmt.Sprintf("unexpected status %d", addStatus)
		}
	}
	report(addOK, "Apollo  add to sequence", addStatus, nil, addNote)

	// 9. FBI CDE agency list.
	status, body, err = fbi.AgenciesByState(stateAbbr)
	fbiOK, fbiNote := fbiSworn(body, err, status)
	report(fbiOK, "FBI CDE agency list (CA)", status, err, fbiNote)

	// 10. USASpending grants.
	status, body, err = spend.SpendingByAward(public.USASpendingRequest{
		Filters: public.USASpendingFilters{
			AwardTypeCodes:      []string{"02", "03", "04", "05"},
			RecipientSearchText: []string{"police department"},
			TimePeriod: []public.USASpendingTimePeriod{
				{StartDate: "2023-01-01", EndDate: "2024-12-31"},
			},
			PlaceOfPerformanceLocations: []map[string]string{{"state": "CA"}},
		},
		Fields: []string{"Award ID", "Recipient Name", "Award Amount", "Awarding Agency"},
		Page:   1,
		Limit:  5,
		Sort:   "Award Amount",
		Order:  "desc",
	})
	spendOK, spendNote := spendingSummary(body, err, status)
	report(spendOK, "USASpending grants (CA LE)", status, err, spendNote)

	// 11. Census local govt finance.
	status, body, err = census.LocalGovFinance(stateFIPS)
	censusOK, censusNote := censusSummary(body, err, status)
	report(censusOK, "Census  govt finance (CA)", status, err, censusNote)

	fmt.Println()
	fmt.Printf("hello-world complete at %s\n", time.Now().Format(time.RFC3339))
}

// ---- reporting & parsing helpers ---------------------------------------

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "fatal:", msg)
	os.Exit(1)
}

func report(ok bool, label string, status int, err error, note string) {
	mark := "\u2713"
	if !ok {
		mark = "\u2717"
	}
	statusCol := "---"
	if status > 0 {
		statusCol = fmt.Sprintf("%d", status)
	}
	if err != nil && note == "" {
		note = err.Error()
	}
	fmt.Printf("[%s] %-38s %-4s %s\n", mark, label, statusCol, note)
}

func arrayField(body []byte, key string) ([]map[string]any, error) {
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

func strField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func orgName(person map[string]any) string {
	org, ok := person["organization"].(map[string]any)
	if !ok {
		return ""
	}
	return strField(org, "name")
}

func healthNote(body []byte) string {
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	if v, ok := m["is_logged_in"].(bool); ok && v {
		return "key valid"
	}
	return "response did not include is_logged_in=true"
}

// enrichmentGap returns ok=false when Apollo's .gov enrichment is null —
// that ✗ is the business case GovEnrich fills.
func enrichmentGap(body []byte, err error, status int) (bool, string) {
	if err != nil {
		return false, err.Error()
	}
	if status != 200 {
		return false, fmt.Sprintf("HTTP %d", status)
	}
	var m map[string]any
	if jerr := json.Unmarshal(body, &m); jerr != nil {
		return false, jerr.Error()
	}
	org, _ := m["organization"].(map[string]any)
	if org == nil {
		return false, "no organization in payload"
	}
	empVal, empPresent := org["estimated_num_employees"]
	revVal, revPresent := org["annual_revenue"]
	empNull := !empPresent || empVal == nil
	revNull := !revPresent || revVal == nil
	if empNull || revNull {
		return false, fmt.Sprintf("WARNING: employee_count=%s, revenue=%s",
			nullableStr(empVal, empNull), nullableStr(revVal, revNull))
	}
	return true, "populated (unexpected for .gov)"
}

func nullableStr(v any, isNull bool) string {
	if isNull {
		return "null"
	}
	return fmt.Sprintf("%v", v)
}

func emailRevealed(body []byte, err error, status int) (bool, string) {
	if err != nil {
		return false, err.Error()
	}
	if status != 200 {
		return false, fmt.Sprintf("HTTP %d", status)
	}
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	person, _ := m["person"].(map[string]any)
	if person == nil {
		return false, "no person in payload"
	}
	email := strField(person, "email")
	if email == "" {
		return false, "no email revealed"
	}
	return true, "email revealed (" + maskEmail(email) + ")"
}

func maskEmail(e string) string {
	at := strings.Index(e, "@")
	if at < 2 {
		return e
	}
	return e[:1] + strings.Repeat("*", at-1) + e[at:]
}

func extractContactID(body []byte) string {
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	if c, ok := m["contact"].(map[string]any); ok {
		if id := strField(c, "id"); id != "" {
			return id
		}
	}
	return strField(m, "id")
}

func fbiSworn(body []byte, err error, status int) (bool, string) {
	if err != nil {
		return false, err.Error()
	}
	if status != 200 {
		return false, fmt.Sprintf("HTTP %d", status)
	}
	// CDE returns either a flat array or {results:[...]} depending on version.
	var arr []map[string]any
	if jerr := json.Unmarshal(body, &arr); jerr != nil {
		var m map[string]any
		if jerr2 := json.Unmarshal(body, &m); jerr2 != nil {
			return false, "unparseable payload"
		}
		if r, ok := m["results"].([]any); ok {
			for _, x := range r {
				if o, ok := x.(map[string]any); ok {
					arr = append(arr, o)
				}
			}
		}
	}
	if len(arr) == 0 {
		return false, "no agencies returned"
	}
	withOfficers := 0
	for _, a := range arr {
		if v, ok := a["sworn_officers"]; ok && v != nil {
			if n, ok := v.(float64); ok && n > 0 {
				withOfficers++
			}
		}
	}
	return withOfficers > 0, fmt.Sprintf("%d agencies, sworn_officers populated on %d", len(arr), withOfficers)
}

func spendingSummary(body []byte, err error, status int) (bool, string) {
	if err != nil {
		return false, err.Error()
	}
	if status != 200 {
		return false, fmt.Sprintf("HTTP %d", status)
	}
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	results, _ := m["results"].([]any)
	if len(results) == 0 {
		return false, "no grants returned"
	}
	return true, fmt.Sprintf("%d awards returned", len(results))
}

func censusSummary(body []byte, err error, status int) (bool, string) {
	if err != nil {
		return false, err.Error()
	}
	if status != 200 {
		return false, fmt.Sprintf("HTTP %d", status)
	}
	var arr [][]any
	if jerr := json.Unmarshal(body, &arr); jerr != nil {
		return false, "response not a matrix"
	}
	if len(arr) < 2 {
		return false, "empty matrix"
	}
	return true, fmt.Sprintf("%d rows of expenditure by function", len(arr)-1)
}
