package public

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const USASpendingURL = "https://api.usaspending.gov/api/v2/search/spending_by_award/"

type USASpendingClient struct {
	HTTP *http.Client
}

func NewUSASpendingClient() *USASpendingClient {
	return &USASpendingClient{HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type USASpendingFilters struct {
	AwardTypeCodes               []string                 `json:"award_type_codes,omitempty"`
	RecipientSearchText          []string                 `json:"recipient_search_text,omitempty"`
	TimePeriod                   []USASpendingTimePeriod  `json:"time_period,omitempty"`
	PlaceOfPerformanceLocations  []map[string]string      `json:"place_of_performance_locations,omitempty"`
}

type USASpendingTimePeriod struct {
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
}

type USASpendingRequest struct {
	Filters USASpendingFilters `json:"filters"`
	Fields  []string           `json:"fields"`
	Page    int                `json:"page,omitempty"`
	Limit   int                `json:"limit,omitempty"`
	Sort    string             `json:"sort,omitempty"`
	Order   string             `json:"order,omitempty"`
}

// SpendingByAward issues a POST to the USASpending awards search endpoint.
// Active award within the filter window = warm signal for adjacent tech spend.
func (c *USASpendingClient) SpendingByAward(r USASpendingRequest) (int, []byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, USASpendingURL, bytes.NewReader(b))
	if err != nil {
		return 0, nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("usaspending post: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, data, nil
}
