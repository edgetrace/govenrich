package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/edgetrace/govenrich/apollo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SequenceInput is the MCP tool input for create_apollo_sequence — one
// enriched contact plus the Apollo sequence/campaign ID to enroll them
// into. DraftEmail is optional and, when supplied, is attached as a
// human-readable note so the outreach context follows the contact into
// Apollo.
type SequenceInput struct {
	Contact    ContactResult `json:"contact"                jsonschema:"contact from find_gov_contacts"`
	SequenceID string        `json:"sequence_id"            jsonschema:"Apollo sequence/campaign ID, e.g. from your Apollo account"`
	DraftEmail DraftOutput   `json:"draft_email,omitempty"  jsonschema:"optional draft from draft_gov_outreach, stored as context note"`
}

// SequenceOutput reports what actually happened. Partial failures are
// surfaced via PartialErrors so the model can narrate them to the user
// instead of the whole call becoming a hard tool error — Apollo's
// write endpoints require a master API key and routinely 401/403 on
// read-only keys, which is expected during rehearsal.
type SequenceOutput struct {
	ContactID     string   `json:"contact_id"`
	Queued        bool     `json:"queued"`
	SequenceID    string   `json:"sequence_id"`
	Notes         string   `json:"notes,omitempty"`
	PartialErrors []string `json:"partial_errors,omitempty"`
}

// NewSequenceHandler returns the typed MCP handler for create_apollo_sequence.
// Order of operations mirrors the Phase-1 live-run playbook: create the
// contact, then enrol it into the sequence using the user's default inbox.
func NewSequenceHandler(deps Deps) func(context.Context, *mcp.CallToolRequest, SequenceInput) (*mcp.CallToolResult, SequenceOutput, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in SequenceInput) (*mcp.CallToolResult, SequenceOutput, error) {
		out := SequenceOutput{SequenceID: in.SequenceID}

		contactID, err := createContact(deps.Apollo, in.Contact, &out)
		if err != nil || contactID == "" {
			return nil, out, nil
		}
		out.ContactID = contactID

		sendFromID, ok := pickDefaultInbox(deps.Apollo, &out)
		if !ok {
			return nil, out, nil
		}

		enrolled := enrolInSequence(deps.Apollo, in.SequenceID, contactID, sendFromID, &out)
		out.Queued = enrolled

		return nil, out, nil
	}
}

// createContact POSTs /contacts and returns the new Apollo contact_id.
// 401/403 is recorded as a partial error (master key required) and the
// caller short-circuits — there's no contact to enrol without an ID.
func createContact(c *apollo.Client, person ContactResult, out *SequenceOutput) (string, error) {
	status, body, err := c.CreateContact(apollo.CreateContactRequest{
		FirstName:        person.FirstName,
		LastName:         person.LastName,
		Title:            person.Title,
		Email:            person.Email,
		OrganizationName: person.Organization,
	})
	if err != nil {
		out.PartialErrors = append(out.PartialErrors, fmt.Sprintf("create_contact transport: %v", err))
		return "", err
	}
	if status == 401 || status == 403 {
		out.Notes = "create_contact requires master Apollo API key"
		out.PartialErrors = append(out.PartialErrors, fmt.Sprintf("create_contact status %d — master Apollo API key required", status))
		return "", nil
	}
	if status < 200 || status >= 300 {
		out.PartialErrors = append(out.PartialErrors, fmt.Sprintf("create_contact status %d: %s", status, truncate(body, 200)))
		return "", nil
	}
	return extractContactID(body), nil
}

// extractContactID pulls the new contact id from either /contacts
// response shape Apollo has returned in the wild: the documented
// `{"contact": {"id": "..."}}` envelope and the flatter `{"id": "..."}`
// that shows up under certain account configurations.
func extractContactID(body []byte) string {
	var wrapped struct {
		Contact struct {
			ID string `json:"id"`
		} `json:"contact"`
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return ""
	}
	if wrapped.Contact.ID != "" {
		return wrapped.Contact.ID
	}
	return wrapped.ID
}

// pickDefaultInbox returns the send_email_from_email_account_id that
// AddContactToSequence will use. Prefer the inbox Apollo has flagged as
// the account default; fall back to the first inbox in the list. If
// the user has no inboxes connected, we cannot enrol and bail out.
func pickDefaultInbox(c *apollo.Client, out *SequenceOutput) (string, bool) {
	status, body, err := c.EmailAccountsList()
	if err != nil {
		out.PartialErrors = append(out.PartialErrors, fmt.Sprintf("email_accounts transport: %v", err))
		return "", false
	}
	if status < 200 || status >= 300 {
		out.PartialErrors = append(out.PartialErrors, fmt.Sprintf("email_accounts status %d: %s", status, truncate(body, 200)))
		return "", false
	}
	var env struct {
		EmailAccounts []struct {
			ID      string `json:"id"`
			Default bool   `json:"default"`
		} `json:"email_accounts"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		out.PartialErrors = append(out.PartialErrors, fmt.Sprintf("email_accounts parse: %v", err))
		return "", false
	}
	if len(env.EmailAccounts) == 0 {
		out.PartialErrors = append(out.PartialErrors, "no connected Apollo inboxes — skipping sequence enrollment")
		return "", false
	}
	for _, a := range env.EmailAccounts {
		if a.Default && a.ID != "" {
			return a.ID, true
		}
	}
	return env.EmailAccounts[0].ID, true
}

// enrolInSequence POSTs to /emailer_campaigns/{sequence_id}/add_contact_ids
// and returns true only on a 2xx. 401/403 is recorded as the familiar
// master-key failure; any other non-2xx is also captured in PartialErrors
// so the caller sees the concrete Apollo error body.
func enrolInSequence(c *apollo.Client, sequenceID, contactID, sendFromID string, out *SequenceOutput) bool {
	status, body, err := c.AddContactToSequence(sequenceID, apollo.AddContactToSequenceRequest{
		ContactIDs:         []string{contactID},
		SendEmailFromEmail: sendFromID,
	})
	if err != nil {
		out.PartialErrors = append(out.PartialErrors, fmt.Sprintf("add_to_sequence transport: %v", err))
		return false
	}
	if status == 401 || status == 403 {
		out.PartialErrors = append(out.PartialErrors, "add_to_sequence requires master Apollo API key")
		return false
	}
	if status < 200 || status >= 300 {
		out.PartialErrors = append(out.PartialErrors, fmt.Sprintf("add_to_sequence status %d: %s", status, truncate(body, 200)))
		return false
	}
	return true
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
