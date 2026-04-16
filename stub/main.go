// Throwaway MCP stub for Claude Desktop integration rehearsal.
// Registers one tool (enrich_gov_agency_stub) that returns canned data
// shaped like the real enrich_gov_agency output. Delete once Agent A's
// server is live in Claude Desktop. See stub/README.md.
package main

import (
	"context"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type HelloInput struct {
	Name string `json:"name" jsonschema:"US law-enforcement agency or city name to enrich (e.g. 'Pleasanton')"`
}

type HelloOutput struct {
	Greeting      string `json:"greeting" jsonschema:"short status line"`
	Agency        string `json:"agency" jsonschema:"resolved agency name"`
	SwornOfficers int    `json:"sworn_officers" jsonschema:"count of full-time sworn officers (canned)"`
	Note          string `json:"note" jsonschema:"stub marker — will disappear when real tool ships"`
}

func enrichStub(_ context.Context, _ *mcp.CallToolRequest, in HelloInput) (*mcp.CallToolResult, HelloOutput, error) {
	if strings.Contains(strings.ToLower(in.Name), "pleasanton") {
		return nil, HelloOutput{
			Greeting:      "stub ok",
			Agency:        "Pleasanton Police Department",
			SwornOfficers: 70,
			Note:          "STUB — replace with govenrich when shipping",
		}, nil
	}
	return nil, HelloOutput{
		Greeting:      "stub ok",
		Agency:        in.Name + " (canned)",
		SwornOfficers: 42,
		Note:          "STUB — replace with govenrich when shipping",
	}, nil
}

func main() {
	srv := mcp.NewServer(&mcp.Implementation{Name: "govenrich-stub", Version: "0.0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "enrich_gov_agency_stub",
		Description: "STUB harness for govenrich — returns canned sworn-officer data to de-risk Claude Desktop integration. Not a real enrichment tool.",
	}, enrichStub)

	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		// stderr-only; stdout is the JSON-RPC transport.
		_, _ = os.Stderr.WriteString("stub: " + err.Error() + "\n")
		os.Exit(1)
	}
}
