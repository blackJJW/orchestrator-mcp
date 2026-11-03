# Read-only MCP scope server

MTT-690 implements the first MCP-first v6 surface. The existing dry-run CLI and
Native Controller are separate. PR #9 keyring work is not a prerequisite and is
not included here.

## Build and run

Use installed Go 1.27+ and make. The one direct dependency is the official
`github.com/modelcontextprotocol/go-sdk v1.8.0`; `go.mod`/`go.sum` pin its required
transitive dependencies. No package or toolchain is installed globally.

```sh
make deps
make check
.local/bin/orchestrator-mcp-server -profile testdata/projects/alpha.json -port 8080
```

The foreground server prints `MCP endpoint: http://127.0.0.1:8080/mcp` to stderr.
Connect a local MCP client using that Streamable HTTP URL. `-port 0` selects a free
loopback port and prints its actual URL. There is no bind-address option;
`0.0.0.0`, LAN addresses and remote deployment are not supported. Ctrl-C/SIGTERM
shuts the server down. Tests close their own listeners; no daemon is installed.

The operator must supply one explicit JSON profile. `config.Load` validates it
before the listener opens; the profile is held for that server's lifetime.
Missing/invalid profiles fail startup. Profile changes require an operator restart.

## Tool and result

Discovery exposes exactly one tool, `check_scope`, with read-only/closed-world
annotations. It accepts exactly these required string fields:

```json
{
  "task_id": "bootstrap-check",
  "workspace": "/canonical/operator-profile/workspace",
  "permission": "source.read"
}
```

For the example profile above, use the absolute canonical path to this checkout's
`testdata/workspaces/alpha`. Relative paths, aliases and different workspaces do
not match. Client paths are compared as strings, never resolved or opened.
The SDK derives and validates the input schema; unknown fields such as `profile`,
`project_id` and `execute`, wrong types, missing fields and unknown tools are rejected.

The tool calls the unchanged `core.Plan` and returns the unchanged `core.Decision`
as MCP structured content and JSON text. A valid allowed request has:

```json
{
  "schema_version": 1,
  "mode": "dry-run",
  "project_id": "alpha",
  "request": {
    "task_id": "bootstrap-check",
    "workspace": "/canonical/operator-profile/workspace",
    "permission": "source.read"
  },
  "scope_allowed": true,
  "execution_started": false,
  "reason": "DRY_RUN_ONLY"
}
```

Unknown tasks return `TASK_NOT_ALLOWLISTED`, other workspaces return
`WORKSPACE_SCOPE_MISMATCH`, permissions absent from the profile return
`PERMISSION_NOT_GRANTED`, and empty values retain `REQUEST_REQUIRED`. These policy
denials are successful MCP calls containing `scope_allowed=false`. Schema/protocol
errors are SDK errors instead. Every Decision has `execution_started=false`.
Even if an operator profile lists a write/execute permission, this tool only
compares its name; an allowed scope creates no Grant, Claim, write or execution.

## HTTP and authority boundary

- The listener is fixed to IPv4 `127.0.0.1`. The SDK's default localhost Host
  protection stays enabled. Forwarded headers do not grant a remote bind.
- Go's `http.CrossOriginProtection` checks Origin and fetch metadata. Explicit
  Origin is checked on all MCP methods, including GET. Same-origin requests and
  local native clients without Origin are permitted; cross-origin requests get 403.
- The SDK implements stateless Streamable HTTP with JSON responses and a 64 KiB
  body limit, including chunked requests. It handles MCP initialization, discovery,
  calls and protocol errors. There is no custom JSON-RPC implementation.
- Standard HTTP header/read/write/idle deadlines and header limits are applied.
  No authentication service, credentials, tunnel or external publication is added.
  Other local processes can query this intentionally local, read-only endpoint.
- Core/Config are reused without changes. The MCP server never calls Native,
  Provider, Notion, Journal, a scheduler or another account. `RESULT_DURABLE` and
  `SYNC_CONFIRMED`, and the existing Controller's states and authority, are unaffected.

Remote ChatGPT services cannot reach a host's loopback URL. Remote exposure and
authentication are outside this MVP; do not create a tunnel to make a test pass.

## Evidence and limits

The new tests use the official SDK Client against real localhost HTTP listeners,
including the production entrypoint. They cover initialize/tools/list/tools/call,
exact allowed/denied Decisions, invalid arguments/tools, fixed startup profile,
Host/Origin/body limits, loopback binding, non-loopback connection rejection and
graceful shutdown. The existing CLI regressions remain in `make check`.

Codex/Gemini client settings are not changed and their Provider-backed prompts are
not run. Their direct tool invocation is unverified; the SDK Client protocol path
is the connection proof. `make check` is offline after the explicit `make deps`.
