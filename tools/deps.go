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
}
