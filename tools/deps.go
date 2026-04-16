// Package tools hosts the MCP tool handlers the govenrich binary exposes.
// Deps is the frozen contract between the server wiring (owned by Agent A)
// and the tool implementations (owned by Agent B) — it carries the four
// HTTP clients built in main at startup.
package tools

import (
	"github.com/edgetrace/govenrich/apollo"
	"github.com/edgetrace/govenrich/public"
)

type Deps struct {
	Apollo      *apollo.Client
	FBI         *public.FBIClient
	USASpending *public.USASpendingClient
	Census      *public.CensusClient
	// AnthropicKey is optional — only search_gov_web and draft_gov_outreach
	// need it. Handlers that require it must check for "" and return a
	// structured error rather than panicking.
	AnthropicKey string
}
