// score_agency_fit.go — pure-Go ICP scoring for law-enforcement agencies.
// No external calls. Scoring is additive with a 100 cap; each factor that
// fires appends a human-readable reasoning string. See AGENT_B_SPEC.md
// Dispatcher Task 3 for the factor table.
package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------------------
// Tool I/O types
// ---------------------------------------------------------------------------

type ScoreInput struct {
	Agency EnrichOutput `json:"agency" jsonschema:"enriched agency record, typically the output of enrich_gov_agency"`
}

type ScoreOutput struct {
	Score     int      `json:"score"`
	Reasoning []string `json:"reasoning"`
	Tier      string   `json:"tier"` // "hot" ≥75, "warm" ≥50, "cold" <50
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

func NewScoreHandler(_ Deps) func(
	context.Context,
	*mcp.CallToolRequest,
	ScoreInput,
) (*mcp.CallToolResult, ScoreOutput, error) {
	return func(_ context.Context, _ *mcp.CallToolRequest, in ScoreInput) (*mcp.CallToolResult, ScoreOutput, error) {
		score, reasoning := scoreEnriched(in.Agency)
		return nil, ScoreOutput{
			Score:     score,
			Reasoning: reasoning,
			Tier:      tierFor(score),
		}, nil
	}
}

// ---------------------------------------------------------------------------
// Public scoring helpers used across tools
// ---------------------------------------------------------------------------

// ScoreAgency scores a bare AgencyResult (pre-enrich — no grants, no Apollo
// employee/revenue data). Used by search_gov_agencies for list-mode
// ranking; the full-enrichment score lives in scoreEnriched.
func ScoreAgency(r AgencyResult) int {
	score := 0
	score += swornPoints(r.SwornOfficers)
	score += agencyTypePoints(r.AgencyType)
	score += statePoints(r.State)
	if r.Domain != "" {
		score += 10
	}
	return capScore(score)
}

// ---------------------------------------------------------------------------
// Internal scoring (full, with reasoning)
// ---------------------------------------------------------------------------

func scoreEnriched(a EnrichOutput) (int, []string) {
	reasoning := []string{}
	score := 0

	// Sworn officers — Pleasanton ICP is ~70, so 50-100 is the sweet spot.
	if a.SwornOfficers != nil {
		n := *a.SwornOfficers
		switch {
		case n >= 50 && n <= 100:
			score += 30
			reasoning = append(reasoning, fmt.Sprintf("sworn officers %d — within ICP sweet spot (+30)", n))
		case n > 100 && n <= 250:
			score += 20
			reasoning = append(reasoning, fmt.Sprintf("sworn officers %d — larger than ICP but still viable (+20)", n))
		case n >= 25 && n < 50:
			score += 15
			reasoning = append(reasoning, fmt.Sprintf("sworn officers %d — smaller than ICP but possible (+15)", n))
		default:
			reasoning = append(reasoning, fmt.Sprintf("sworn officers %d — outside viable band (+0)", n))
		}
	} else {
		score += 10
		reasoning = append(reasoning, "sworn officers unknown — benefit of doubt (+10)")
	}

	// Agency type.
	switch strings.ToLower(a.AgencyType) {
	case "city":
		score += 20
		reasoning = append(reasoning, "agency type City (municipal PD) — primary ICP (+20)")
	case "county":
		score += 10
		reasoning = append(reasoning, "agency type County — secondary ICP (+10)")
	}

	// State.
	state := strings.ToUpper(strings.TrimSpace(a.State))
	if state == "CA" {
		score += 15
		reasoning = append(reasoning, "state CA — home market (+15)")
	} else if isWesternUS(state) {
		score += 10
		reasoning = append(reasoning, fmt.Sprintf("state %s — western US (+10)", state))
	}

	// Active federal grants — warm signal for adjacent tech spend.
	if len(a.ActiveGrants) > 0 {
		score += 15
		reasoning = append(reasoning, fmt.Sprintf("%d active federal grant(s) — warm spend signal (+15)", len(a.ActiveGrants)))
	}

	// Apollo footprint — populated domain means Apollo could reach them.
	if a.Domain != "" {
		score += 10
		reasoning = append(reasoning, "Apollo domain populated — outreach path exists (+10)")
	}
	if a.ApolloEmployeeCount != nil {
		score += 5
		reasoning = append(reasoning, fmt.Sprintf("Apollo employee count populated (%d) (+5)", *a.ApolloEmployeeCount))
	}

	return capScore(score), reasoning
}

// ---------------------------------------------------------------------------
// Factor helpers (also reusable by ScoreAgency)
// ---------------------------------------------------------------------------

func swornPoints(sworn *int) int {
	if sworn == nil {
		return 10
	}
	n := *sworn
	switch {
	case n >= 50 && n <= 100:
		return 30
	case n > 100 && n <= 250:
		return 20
	case n >= 25 && n < 50:
		return 15
	}
	return 0
}

func agencyTypePoints(t string) int {
	switch strings.ToLower(t) {
	case "city":
		return 20
	case "county":
		return 10
	}
	return 0
}

func statePoints(s string) int {
	state := strings.ToUpper(strings.TrimSpace(s))
	if state == "CA" {
		return 15
	}
	if isWesternUS(state) {
		return 10
	}
	return 0
}

func isWesternUS(state string) bool {
	switch state {
	case "OR", "WA", "NV", "AZ", "CO", "UT", "NM", "ID", "MT", "WY":
		return true
	}
	return false
}

func capScore(s int) int {
	if s > 100 {
		return 100
	}
	if s < 0 {
		return 0
	}
	return s
}

func tierFor(score int) string {
	switch {
	case score >= 75:
		return "hot"
	case score >= 50:
		return "warm"
	}
	return "cold"
}
