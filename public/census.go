package public

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// TODO(spec): this URL returns 404. Census government finance is published
// under the timeseries collection (e.g. /data/timeseries/govs/*), not as a
// 2022 annual endpoint. Exact dataset TBD — see SPEC.md §11 for the gap.
const CensusGovFinanceURL = "https://api.census.gov/data/2022/govfinances"

type CensusClient struct {
	HTTP *http.Client
}

func NewCensusClient() *CensusClient {
	return &CensusClient{HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// LocalGovFinance pulls annual expenditure rows by state FIPS (e.g. "06" = CA).
// Caller filters by FUNCTION=05 (police protection) or 61 (capital outlay).
func (c *CensusClient) LocalGovFinance(stateFIPS string) (int, []byte, error) {
	q := url.Values{}
	q.Set("get", "NAME,GOVTYPE,EXPENDITURE,FUNCTION")
	q.Set("for", "place:*")
	q.Set("in", "state:"+stateFIPS)
	u := CensusGovFinanceURL + "?" + q.Encode()

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("census get: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, data, nil
}
