// draft_gov_outreach.go — generate a personalized cold-outreach email for
// a law-enforcement agency using Claude Sonnet 4.6. No tools — pure
// generation. Grounds the output in whatever structured context the
// caller provides (enrich output, score, web-research summary).
//
// Like search_gov_web, this degrades to a partial_errors note when
// ANTHROPIC_API_KEY is unset rather than erroring.
package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------------------
// Tool I/O types
// ---------------------------------------------------------------------------

type DraftInput struct {
	Agency     EnrichOutput    `json:"agency"`
	Contact    ContactResult   `json:"contact,omitempty"    jsonschema:"optional: a specific person to address, usually chained from find_gov_contacts"`
	Score      ScoreOutput     `json:"score,omitempty"      jsonschema:"optional: fit score context, usually chained from score_agency_fit"`
	WebContext WebSearchOutput `json:"web_context,omitempty" jsonschema:"optional: web research, usually chained from search_gov_web"`
	SenderName string          `json:"sender_name" jsonschema:"your first name, e.g. 'Alex'"`
	Product    string          `json:"product"     jsonschema:"product being pitched, e.g. 'video analytics platform'"`
	Company    string          `json:"company"     jsonschema:"your company name, e.g. 'EdgeTrace'"`
}

type DraftOutput struct {
	Subject             string   `json:"subject"`
	Body                string   `json:"body"`
	PersonalizationUsed []string `json:"personalization_used"`
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

const draftSystem = "You are a B2G sales expert. Write a concise, specific first-touch cold email. Max 150 words for the body. Reference real data points from the provided context. Never generic. End with a soft CTA (15-minute call). After the email, list exactly which data points you personalized with, one per line, prefixed 'USED:'. Format your entire reply as:\nSUBJECT: <subject line>\n\n<email body>\n\nUSED:\n- <data point 1>\n- <data point 2>"

func NewDraftHandler(deps Deps) func(
	context.Context,
	*mcp.CallToolRequest,
	DraftInput,
) (*mcp.CallToolResult, DraftOutput, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in DraftInput) (*mcp.CallToolResult, DraftOutput, error) {
		out := DraftOutput{
			PersonalizationUsed: []string{},
		}
		if deps.AnthropicKey == "" {
			// Echo the fallback into the body so callers see what happened
			// without the handler having to return an error.
			out.Body = "draft_gov_outreach: ANTHROPIC_API_KEY not configured"
			return nil, out, nil
		}

		userPrompt := buildDraftPrompt(in)

		client := anthropic.NewClient(option.WithAPIKey(deps.AnthropicKey))
		resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.Model("claude-sonnet-4-6"),
			MaxTokens: 1024,
			System: []anthropic.TextBlockParam{{
				Text: draftSystem,
			}},
			Messages: []anthropic.MessageParam{
				anthropic.NewUserMessage(anthropic.NewTextBlock(userPrompt)),
			},
		})
		if err != nil {
			out.Body = "anthropic messages: " + err.Error()
			return nil, out, nil
		}

		var raw strings.Builder
		for _, block := range resp.Content {
			if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
				raw.WriteString(tb.Text)
				raw.WriteString("\n")
			}
		}
		subject, body, used := splitDraftResponse(raw.String())
		out.Subject = subject
		out.Body = body
		out.PersonalizationUsed = used
		return nil, out, nil
	}
}

// ---------------------------------------------------------------------------
// Prompt assembly
// ---------------------------------------------------------------------------

// buildDraftPrompt stitches available structured fields into a single
// context block. Empty sub-fields are omitted so the model doesn't waste
// tokens on empty bullets.
func buildDraftPrompt(in DraftInput) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Write a cold email from %s at %s pitching %s.\n\n", in.SenderName, in.Company, in.Product)
	fmt.Fprintln(&b, "AGENCY CONTEXT:")
	fmt.Fprintf(&b, "- Name: %s\n", in.Agency.Name)
	if in.Agency.City != "" {
		fmt.Fprintf(&b, "- City: %s, %s\n", in.Agency.City, in.Agency.State)
	} else {
		fmt.Fprintf(&b, "- State: %s\n", in.Agency.State)
	}
	if in.Agency.Domain != "" {
		fmt.Fprintf(&b, "- Domain: %s\n", in.Agency.Domain)
	}
	if in.Agency.SwornOfficers != nil {
		fmt.Fprintf(&b, "- Sworn officers: %d\n", *in.Agency.SwornOfficers)
	}
	if in.Agency.ApolloEmployeeCount != nil {
		fmt.Fprintf(&b, "- Total employees (city): %d\n", *in.Agency.ApolloEmployeeCount)
	}
	if len(in.Agency.ActiveGrants) > 0 {
		fmt.Fprintf(&b, "- Active federal grants (%d):\n", len(in.Agency.ActiveGrants))
		for _, g := range in.Agency.ActiveGrants {
			fmt.Fprintf(&b, "    • $%.0f %s (%s) tier %d\n",
				g.AmountUSD, g.RecipientName, g.AwardingAgency, g.SearchTier)
		}
	}

	if in.Contact.FirstName != "" || in.Contact.LastName != "" {
		fmt.Fprintln(&b, "\nCONTACT:")
		fmt.Fprintf(&b, "- %s %s", in.Contact.FirstName, in.Contact.LastName)
		if in.Contact.Title != "" {
			fmt.Fprintf(&b, ", %s", in.Contact.Title)
		}
		fmt.Fprintln(&b)
	}

	if in.Score.Score > 0 || len(in.Score.Reasoning) > 0 {
		fmt.Fprintln(&b, "\nICP FIT:")
		fmt.Fprintf(&b, "- Score: %d/100 (%s)\n", in.Score.Score, in.Score.Tier)
		if len(in.Score.Reasoning) > 0 {
			fmt.Fprintln(&b, "- Factors:")
			for _, r := range in.Score.Reasoning {
				fmt.Fprintf(&b, "    • %s\n", r)
			}
		}
	}

	if len(in.WebContext.Stakeholders) > 0 || len(in.WebContext.BudgetSignals) > 0 || len(in.WebContext.NewsItems) > 0 {
		fmt.Fprintln(&b, "\nWEB RESEARCH:")
		if len(in.WebContext.Stakeholders) > 0 {
			fmt.Fprintln(&b, "- Named stakeholders:")
			for _, s := range in.WebContext.Stakeholders {
				fmt.Fprintf(&b, "    • %s — %s\n", s.Name, s.Title)
			}
		}
		if len(in.WebContext.BudgetSignals) > 0 {
			fmt.Fprintln(&b, "- Budget signals:")
			for i, sig := range in.WebContext.BudgetSignals {
				if i >= 3 {
					break
				}
				fmt.Fprintf(&b, "    • %s\n", sig)
			}
		}
		if len(in.WebContext.NewsItems) > 0 {
			fmt.Fprintln(&b, "- Recent news:")
			for i, n := range in.WebContext.NewsItems {
				if i >= 3 {
					break
				}
				fmt.Fprintf(&b, "    • %s\n", n)
			}
		}
	}

	return b.String()
}

// ---------------------------------------------------------------------------
// Response parsing
// ---------------------------------------------------------------------------

// splitDraftResponse pulls SUBJECT / body / USED list out of Claude's
// formatted reply. Robust to minor formatting drift — trims whitespace on
// every value, strips bullet markers, ignores empty lines.
func splitDraftResponse(raw string) (subject, body string, used []string) {
	lines := strings.Split(raw, "\n")
	var bodyLines []string
	inUsed := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "SUBJECT:"):
			subject = strings.TrimSpace(line[len("SUBJECT:"):])
		case strings.HasPrefix(upper, "USED:"):
			inUsed = true
			// A remainder on the same line, e.g. "USED: one, two"
			rest := strings.TrimSpace(line[len("USED:"):])
			if rest != "" {
				for _, u := range strings.Split(rest, ",") {
					if u = strings.TrimSpace(u); u != "" {
						used = append(used, u)
					}
				}
			}
		default:
			if inUsed {
				// Lines after the USED: header are bullet points.
				u := strings.TrimLeft(trimmed, "-•* \t")
				if u != "" {
					used = append(used, u)
				}
			} else {
				bodyLines = append(bodyLines, line)
			}
		}
	}
	body = strings.TrimSpace(strings.Join(bodyLines, "\n"))
	return subject, body, used
}
