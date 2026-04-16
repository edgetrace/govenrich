// search_gov_web.go — research a government agency using Claude + the
// server-side web_search tool. Returns stakeholders, budget signals, and
// news items for use as outreach context. See AGENT_B_SPEC.md Dispatcher
// Task 4 for the full contract.
//
// The Anthropic SDK reads ANTHROPIC_API_KEY from the environment by
// default; Claude Desktop injects env vars via claude_desktop_config.json.
// If the key is unset we return early with a partial_errors note rather
// than a hard error, so the tool degrades gracefully for users who only
// set APOLLO_API_KEY / FBI_CDE_API_KEY.
package tools

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------------------
// Tool I/O types
// ---------------------------------------------------------------------------

type WebSearchInput struct {
	AgencyName string `json:"agency_name"     jsonschema:"agency name, e.g. 'Pleasanton Police Department'"`
	State      string `json:"state"           jsonschema:"two-letter state code, e.g. 'CA'"`
	City       string `json:"city,omitempty"  jsonschema:"city name if known, e.g. 'Pleasanton'"`
	Focus      string `json:"focus,omitempty" jsonschema:"optional focus: 'council', 'budget', 'leadership', or 'technology'. Omit for an all-areas search."`
}

type WebSearchOutput struct {
	Stakeholders  []Stakeholder `json:"stakeholders"`
	BudgetSignals []string      `json:"budget_signals"`
	NewsItems     []string      `json:"news_items"`
	RawSummary    string        `json:"raw_summary"`
	Sources       []string      `json:"sources"`
	PartialErrors []string      `json:"partial_errors,omitempty"`
}

type Stakeholder struct {
	Name   string `json:"name"`
	Title  string `json:"title,omitempty"`
	Source string `json:"source,omitempty"`
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

const webSearchSystem = "You are a B2G sales researcher. Extract named people with titles, budget signals (dollar amounts, tech purchases), and recent news relevant to selling technology to this agency. Be specific and cite sources."

func NewWebSearchHandler(deps Deps) func(
	context.Context,
	*mcp.CallToolRequest,
	WebSearchInput,
) (*mcp.CallToolResult, WebSearchOutput, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in WebSearchInput) (*mcp.CallToolResult, WebSearchOutput, error) {
		out := WebSearchOutput{
			Stakeholders:  []Stakeholder{},
			BudgetSignals: []string{},
			NewsItems:     []string{},
			Sources:       []string{},
			PartialErrors: []string{},
		}
		if deps.AnthropicKey == "" {
			out.PartialErrors = append(out.PartialErrors, "search_gov_web: ANTHROPIC_API_KEY not configured")
			return nil, out, nil
		}

		focus := in.Focus
		if focus == "" {
			focus = "all areas"
		}
		city := in.City
		if city == "" {
			city = "the agency's city"
		}
		userPrompt := fmt.Sprintf(
			"Research %s in %s, %s. Focus: %s. Find: city council members, police/IT leadership, recent budget approvals, technology purchases, federal grants.",
			in.AgencyName, city, in.State, focus,
		)

		client := anthropic.NewClient(option.WithAPIKey(deps.AnthropicKey))
		resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.Model("claude-opus-4-7"),
			MaxTokens: 4096,
			System: []anthropic.TextBlockParam{{
				Text: webSearchSystem,
			}},
			Messages: []anthropic.MessageParam{
				anthropic.NewUserMessage(anthropic.NewTextBlock(userPrompt)),
			},
			Tools: []anthropic.ToolUnionParam{
				{OfWebSearchTool20260209: &anthropic.WebSearchTool20260209Param{}},
			},
		})
		if err != nil {
			out.PartialErrors = append(out.PartialErrors, "anthropic messages: "+err.Error())
			return nil, out, nil
		}

		// Walk the response; collect text from TextBlocks, ignore
		// server_tool_use / server_tool_result frames. The web_search
		// tool emits search results as citations embedded in TextBlocks.
		var fullText strings.Builder
		for _, block := range resp.Content {
			if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
				fullText.WriteString(tb.Text)
				fullText.WriteString("\n")
			}
		}
		raw := strings.TrimSpace(fullText.String())
		if raw == "" {
			out.PartialErrors = append(out.PartialErrors, "anthropic returned no text content — web search may have produced tool-only output")
			return nil, out, nil
		}

		out.RawSummary = raw
		out.Stakeholders = extractStakeholders(raw)
		out.BudgetSignals = extractBudgetSignals(raw)
		out.NewsItems = extractNewsItems(raw)
		out.Sources = extractURLs(raw)
		return nil, out, nil
	}
}

// ---------------------------------------------------------------------------
// Best-effort parsers
// ---------------------------------------------------------------------------

// extractStakeholders finds "Name, Title" / "Name - Title" / "Name (Title)"
// patterns. Heuristic; the real value is the RawSummary + Sources. The
// regex requires a capitalized first/last name so we don't pick up random
// phrases.
var stakeholderRe = regexp.MustCompile(`([A-Z][a-z]+(?:\s+[A-Z][a-z]+)+)\s*[-–—(,]\s*([A-Z][A-Za-z][^,\n(]{2,80})`)

func extractStakeholders(text string) []Stakeholder {
	seen := map[string]bool{}
	out := []Stakeholder{}
	for _, m := range stakeholderRe.FindAllStringSubmatch(text, -1) {
		if len(m) < 3 {
			continue
		}
		name := strings.TrimSpace(m[1])
		title := strings.TrimSpace(strings.TrimRight(m[2], ")."))
		key := name + "|" + title
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Stakeholder{Name: name, Title: title})
	}
	return out
}

// extractBudgetSignals returns sentences containing a USD amount or common
// budget/procurement language. Keeps duplicates out and trims whitespace.
var (
	dollarRe     = regexp.MustCompile(`\$[\d,]+(?:\.\d+)?(?:\s*(?:million|billion|M|B))?`)
	budgetWordRe = regexp.MustCompile(`(?i)\b(budget|appropriat|procure|contract award|purchased|RFP|bond measure)\b`)
	sentenceRe   = regexp.MustCompile(`[^.!?\n]+[.!?]`)
)

func extractBudgetSignals(text string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, sent := range sentenceRe.FindAllString(text, -1) {
		s := strings.TrimSpace(sent)
		if s == "" || seen[s] {
			continue
		}
		if dollarRe.MatchString(s) || budgetWordRe.MatchString(s) {
			seen[s] = true
			out = append(out, s)
			if len(out) >= 10 {
				break
			}
		}
	}
	return out
}

// extractNewsItems returns sentences that look dated — contain a year
// (2020-2099) or a month name. First 10 unique hits win.
var (
	yearRe  = regexp.MustCompile(`\b20[2-9]\d\b`)
	monthRe = regexp.MustCompile(`(?i)\b(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*\b`)
)

func extractNewsItems(text string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, sent := range sentenceRe.FindAllString(text, -1) {
		s := strings.TrimSpace(sent)
		if s == "" || seen[s] {
			continue
		}
		if yearRe.MatchString(s) || monthRe.MatchString(s) {
			seen[s] = true
			out = append(out, s)
			if len(out) >= 10 {
				break
			}
		}
	}
	return out
}

// extractURLs pulls every http(s) URL from the text. Preserves order,
// dedupes.
var urlRe = regexp.MustCompile(`https?://[^\s<>)"'\]]+`)

func extractURLs(text string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, u := range urlRe.FindAllString(text, -1) {
		u = strings.TrimRight(u, ".,;)")
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}
