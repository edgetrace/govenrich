# govenrich-stub

Throwaway MCP server that registers a single tool, `enrich_gov_agency_stub`,
and returns canned data shaped like the real `enrich_gov_agency` output
(notably the `sworn_officers` field). Its only purpose is to de-risk the
Claude Desktop MCP integration path before Agent A's real server lands —
config syntax, stdout hygiene, protocol version, tool-cache invalidation,
and the quit-not-close relaunch gotcha.

This is a separate Go module (`github.com/edgetrace/govenrich/stub`) so the
main `govenrich` module's `go.mod` is untouched. Delete the directory once
Agent A's server is confirmed working in Claude Desktop — see
[Cleanup](#cleanup) below.

## SDK and protocol version

- `github.com/modelcontextprotocol/go-sdk` pinned at **v1.5.0**.
- Protocol version sent in the `initialize` handshake: **`2025-11-25`**
  (mirrors `latestProtocolVersion` from `mcp/shared.go` at v1.5.0).

Agent A should pin the same SDK tag so Claude Desktop negotiates against
one known version across both servers during the transition window.

## Build

From this directory:

```
go build -o govenrich-stub
```

## Smoke-test without Claude Desktop

Confirms the binary speaks MCP over stdio and that nothing is leaking to
stdout. Expect two JSON-RPC result envelopes on stdout and nothing on
stderr:

```
(printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'; \
 sleep 0.5) | ./govenrich-stub
```

The trailing `sleep` keeps stdin open long enough for the server to flush
its response before the stdio transport sees EOF; without it the server
exits before writing to stdout and you'll see an empty response.

## Wire into Claude Desktop

1. Open `~/Library/Application Support/Claude/claude_desktop_config.json`.
   If it does not exist, create it with `{ "mcpServers": {} }`.
2. Copy the `govenrich-stub` entry from
   `../demo/claude_desktop_config.example.json` into the `mcpServers`
   object. Replace `/ABSOLUTE/PATH/TO/edgetrace-gtm/stub/govenrich-stub`
   with the real absolute path on this machine (current build lives at
   `/Users/admin/edgetrace-gtm/stub/govenrich-stub`).
3. **Fully quit Claude Desktop** with `⌘Q`. Closing the window is not
   enough — Claude Desktop only respawns MCP subprocesses on a full
   relaunch.
4. Relaunch Claude Desktop. Open the tool panel (hammer/slider icon at
   the bottom-right of the composer). `govenrich-stub` should appear with
   `enrich_gov_agency_stub` listed underneath.
5. Try the demo prompts in `../demo/prompts.md`.

## Troubleshooting

If the server does not appear in the tool panel or tool calls fail, the
first place to look is:

```
tail -n 200 ~/Library/Logs/Claude/mcp.log
tail -n 200 ~/Library/Logs/Claude/mcp-server-govenrich-stub.log
```

Known gotchas that cost time during bring-up:

- **No stdout noise.** A single `fmt.Println` to stdout corrupts the
  JSON-RPC channel and the tool panel silently shows the server with zero
  tools. The stub uses only `os.Stderr` for its lone error path.
- **Quit, don't close.** `⌘W` or clicking the red dot leaves the existing
  MCP subprocess running against the old binary. Always `⌘Q` and relaunch
  after rebuilding.
- **Tool-list cache.** Claude Desktop sometimes keeps the previous tool
  schema even after relaunch. If `enrich_gov_agency_stub` does not appear,
  quit Claude Desktop, rebuild (`go build -o govenrich-stub`), then
  relaunch.
- **Absolute paths.** `command` must be an absolute path. `~` and relative
  paths silently fail — Claude Desktop's spawn `cwd` is not your shell's.
- **Config JSON is strict.** No trailing commas, no comments. If Claude
  Desktop starts without the tool panel showing any MCP entries at all,
  validate the JSON first.
- **Protocol version mismatch.** If you swap SDKs later and see
  `initialize` fail, grep the SDK for `latestProtocolVersion` and make
  sure the version string sent in the smoke test matches.

Record any new issues you hit here so Agent A inherits the playbook.

## Cleanup

Once Agent A's real `govenrich` server is confirmed working in Claude
Desktop, delete the stub:

```
rm -rf stub demo/claude_desktop_config.example.json
```

Then open `~/Library/Application Support/Claude/claude_desktop_config.json`
and remove the `govenrich-stub` entry from `mcpServers`. Fully quit
(`⌘Q`) and relaunch Claude Desktop to clear the stub from the tool panel.
