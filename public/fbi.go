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
// (e.g. "CA"). Each record carries ORI, agency_name, city_name, agency_type,
// and sworn_officers — the last being Apollo's biggest LE blind spot.
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
