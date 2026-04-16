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

// LocalGovFinanceStub is what Phase 2 tool handlers should call until the
// Census endpoint situation is resolved. SPEC.md §11 asks for per-place
// government finance, which Census does not expose via its JSON API
// (the three govs* timeseries datasets are statewide only; per-place LGF
// data now ships as downloadable files from the 2022 Census of Governments).
//
// This stub preserves the provenance pathway — callers record a
// PartialError without crashing the enrichment flow. When the spec
// decision lands (either adopt timeseries/govsstatefin, ingest the 2022
// COG files, or drop Census entirely), swap this stub for the real impl.
func (c *CensusClient) LocalGovFinanceStub(_ string) (int, []byte, error) {
	return 0, nil, fmt.Errorf("census govfinances endpoint unavailable — see SPEC.md §11")
}

// LocalGovFinance pulls annual expenditure rows by state FIPS (e.g. "06" = CA).
// Caller filters by FUNCTION=05 (police protection) or 61 (capital outlay).
//
// NOTE: the URL in CensusGovFinanceURL is wrong — SPEC.md §11 asks for a
// dataset Census does not publish via API. This method returns HTTP 404
// against any call. Kept in place so --hello-world still exercises the
// endpoint as a ✗ marker. Phase 2 handlers should call LocalGovFinanceStub
// instead.
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
