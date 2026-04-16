// Package apollo is a thin HTTP client over the Apollo.io REST API.
// Methods return the raw response bytes so callers can decode into whatever
// shape they need; Phase-1 hello-world just inspects a handful of fields.
package apollo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const DefaultBaseURL = "https://api.apollo.io/api/v1"

type Client struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
}

func New(apiKey, baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		APIKey:  apiKey,
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) do(method, path string, body any) (int, []byte, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, reqBody)
	if err != nil {
		return 0, nil, fmt.Errorf("new request: %w", err)
	}
	// Apollo rejects Authorization: Bearer and requires the X-Api-Key header.
	req.Header.Set("X-Api-Key", c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("http do %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, data, nil
}

// SPEC.md step 1 (/auth/health) is omitted: the endpoint returns 404 in
// current Apollo deployments. A successful /mixed_companies/search is the
// real key-validity signal.

// POST /mixed_companies/search — ⚠ consumes credits
type OrgSearchRequest struct {
	KeywordTags []string `json:"q_organization_keyword_tags,omitempty"`
	Locations   []string `json:"organization_locations,omitempty"`
	PerPage     int      `json:"per_page,omitempty"`
	Page        int      `json:"page,omitempty"`
}

func (c *Client) OrgSearch(r OrgSearchRequest) (int, []byte, error) {
	return c.do(http.MethodPost, "/mixed_companies/search", r)
}

// 3. GET /organizations/enrich?domain=... — ⚠ consumes credits
func (c *Client) OrgEnrich(domain string) (int, []byte, error) {
	return c.do(http.MethodGet, "/organizations/enrich?domain="+url.QueryEscape(domain), nil)
}

// 4. POST /mixed_people/api_search — no credits
type PeopleSearchRequest struct {
	Titles              []string `json:"person_titles,omitempty"`
	Seniorities         []string `json:"person_seniorities,omitempty"`
	Locations           []string `json:"organization_locations,omitempty"`
	OrganizationDomains []string `json:"organization_domains,omitempty"`
	PerPage             int      `json:"per_page,omitempty"`
	Page                int      `json:"page,omitempty"`
}

func (c *Client) PeopleSearch(r PeopleSearchRequest) (int, []byte, error) {
	return c.do(http.MethodPost, "/mixed_people/api_search", r)
}

// 5. POST /people/match — ⚠ consumes credits
type PeopleMatchRequest struct {
	FirstName            string `json:"first_name"`
	LastName             string `json:"last_name"`
	OrganizationName     string `json:"organization_name,omitempty"`
	Domain               string `json:"domain,omitempty"`
	RevealPersonalEmails bool   `json:"reveal_personal_emails"`
}

func (c *Client) PeopleMatch(r PeopleMatchRequest) (int, []byte, error) {
	return c.do(http.MethodPost, "/people/match", r)
}

// EmailAccountsList: GET /email_accounts — returns the connected inboxes.
// Needed to supply send_email_from_email_account_id on step 8.
func (c *Client) EmailAccountsList() (int, []byte, error) {
	return c.do(http.MethodGet, "/email_accounts", nil)
}

// 6. POST /emailer_campaigns/search — no credits
type SequenceSearchRequest struct {
	PerPage int `json:"per_page,omitempty"`
	Page    int `json:"page,omitempty"`
}

func (c *Client) SequenceSearch(r SequenceSearchRequest) (int, []byte, error) {
	return c.do(http.MethodPost, "/emailer_campaigns/search", r)
}

// 7. POST /contacts — master API key required
type CreateContactRequest struct {
	FirstName        string `json:"first_name"`
	LastName         string `json:"last_name"`
	Title            string `json:"title,omitempty"`
	Email            string `json:"email,omitempty"`
	OrganizationName string `json:"organization_name,omitempty"`
	WebsiteURL       string `json:"website_url,omitempty"`
}

func (c *Client) CreateContact(r CreateContactRequest) (int, []byte, error) {
	return c.do(http.MethodPost, "/contacts", r)
}

// 8. POST /emailer_campaigns/{sequence_id}/add_contact_ids — master API key required
type AddContactToSequenceRequest struct {
	ContactIDs         []string `json:"contact_ids"`
	EmailerCampaignID  string   `json:"emailer_campaign_id"`
	SendEmailFromEmail string   `json:"send_email_from_email_account_id,omitempty"`
}

func (c *Client) AddContactToSequence(sequenceID string, r AddContactToSequenceRequest) (int, []byte, error) {
	r.EmailerCampaignID = sequenceID
	return c.do(http.MethodPost, "/emailer_campaigns/"+url.PathEscape(sequenceID)+"/add_contact_ids", r)
}
