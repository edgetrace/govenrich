// Package public wraps the free government data APIs used to fill the gaps
// Apollo leaves on .gov domains: FBI CDE for sworn officer counts,
// USASpending.gov for federal grants, and Census for local govt finance.
package public

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const FBIBaseURL = "https://api.usa.gov/crime/fbi/cde"

type FBIClient struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
}

func NewFBIClient(apiKey string) *FBIClient {
	return &FBIClient{
		APIKey:  apiKey,
		BaseURL: FBIBaseURL,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// AgenciesByState returns the CDE agency list for a US state abbreviation
// (e.g. "CA"). Response is keyed by county name; each agency record carries
// ORI, agency_name, city_name, agency_type_name, lat/long, and NIBRS status.
// Sworn officer count is NOT in this payload — use PoliceEmployeeByORI for
// that. See SPEC.md §9 (which originally overstated what this endpoint
// returns).
func (c *FBIClient) AgenciesByState(state string) (int, []byte, error) {
	u := fmt.Sprintf("%s/agency/byStateAbbr/%s?API_KEY=%s",
		c.BaseURL, url.PathEscape(state), url.QueryEscape(c.APIKey))
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("fbi get: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, data, nil
}

// PoliceEmployeeByORI hits the FBI CDE police-employee endpoint for a single
// agency. The endpoint requires from/to year query params — without them the
// API returns "Bad request, 'from' and 'to' year is required." (HTTP 400).
//
// Response shape (verified against CA0011100 = Pleasanton PD):
//
//	{
//	  "actuals": {
//	    "Male Officers":    {"2020": 69, "2021": 69, "2022": 69, "2023": 64},
//	    "Female Officers":  {"2020": 9,  "2021": 9,  "2022": 11, "2023": 9},
//	    "Male Civilians":   {...},
//	    "Female Civilians": {...}
//	  },
//	  "rates":          { "Law Enforcement Employees per 1,000 People": {...} },
//	  "populations":    { "Participated Population": {"2023": 75238, ...} },
//	  "cde_properties": { "max_data_date": {"UCR": "03/2026"}, ... }
//	}
//
// Sworn officer total for a given year =
//
//	actuals["Male Officers"][year] + actuals["Female Officers"][year]
//
// "Male Civilians" and "Female Civilians" are NOT sworn and must not be
// added to that total. Prefer the most recent year available under
// cde_properties.max_data_date rather than hardcoding a year — FBI lag
// varies by agency.
func (c *FBIClient) PoliceEmployeeByORI(ori string, fromYear, toYear int) (int, []byte, error) {
	u := fmt.Sprintf("%s/pe/agency/%s?from=%d&to=%d&API_KEY=%s",
		c.BaseURL, url.PathEscape(ori), fromYear, toYear, url.QueryEscape(c.APIKey))
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("fbi pe get: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, data, nil
}
