# Read-only MCP scope server

MTT-690 implements the first MCP-first v6 surface. The subsequent `verify_source`
slice exposes the existing S2 verifier without changing MTT-690's completed scope.
The existing dry-run CLI and
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

Without `-source-spec`, discovery exposes exactly one tool, `check_scope`, with read-only/closed-world
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

## Optional `verify_source`

```sh
.local/bin/orchestrator-mcp-server -profile /approved/project.json -source-spec /approved/source.json -port 8080
```

The operator supplies one startup-only binding file. It is a regular, non-symlink
UTF-8 JSON file of at most 64 KiB, opened nonblocking. Every field is required;
unknown, duplicate, case-alias, null, wrong-type and empty required values fail
startup. Its outer fields are `contract`, `source_spec`, `task_base`. The two nested
objects use the existing Go field names **exactly**, including capitalization:

- `contract` (`core.Contract`): `TaskID`, `PageID`, `ProjectID`, `Workspace`,
  `Permission`, `Executor`, `Owner`, `AuthorityRef`, `BriefDigest`, `FrozenSource`.
- `source_spec` (`config.SourceSpec`): `Workspace`, `ProjectID`, `CandidateRef`,
  `BaseRef`, `Head`, `Tree`, `SourceSet`, `ReviewBase`, `Brief`, `BriefDigest`,
  `Inputs`, `CompatibilityInputs`, `Receipts`.
- `task_base`: the existing task's full lowercase SHA-1 base context.

All scalar fields are nonempty strings. `Brief` preserves its exact approved text;
other strings cannot have surrounding whitespace or control separators. `Inputs`
and `CompatibilityInputs` are nonempty arrays of strings. `Receipts` contains
exactly two objects with lowercase `path` and `sha256`, one canonical and one review
receipt as required by [the unchanged S2 contract](S2-IDENTITY.md).

The contract's TaskID must be allowlisted, Permission must be `source.read`, and
the profile must grant `source.read`. Contract/SourceSpec project, canonical
workspace, brief and frozen source must agree. Startup invokes the existing S2
check, bounded to 15 seconds, before opening a listener. Invalid bindings or
unverifiable startup source/evidence return only `SOURCE_UNPROVEN` and prevent
startup. This reuses S2; it adds no approval gate or evidence producer.

Successful startup adds `verify_source` as a second read-only/closed-world tool.
Its only MCP argument is:

```json
{"task_id":"the-operator-bound-task"}
```

The client cannot supply workspace, profile, paths, refs, digests, receipts,
permission or task base. Each call applies `core.Plan` with the fixed canonical
workspace and `source.read`, checks the contract TaskID, constructs the existing
`intake.NewGitSource`, and calls `GitSource.Check` with the frozen task base. The
whole call has a maximum 15-second deadline, including the existing per-Git-command
limits. Startup input files are never reloaded; actual source and evidence bytes
are verified afresh on every call.

The response always contains exactly `source_verified` (boolean), `reason`
(string), `execution_started` (false), and `proof` (object or null). Success reason
is `SOURCE_VERIFIED`; proof contains only `commit`, `tree`, `source_set`,
`review_base`, `current_base`, `brief_digest`. Failure has `source_verified=false`
and `proof=null`. Scope denials preserve `core.Plan` reasons; a different
allowlisted task or an unproven/canceled source returns `SOURCE_UNPROVEN`.
Incompatible base advancement returns `SOURCE_INCOMPATIBLE`. SDK input-schema
errors remain MCP errors. No raw Brief, source text, receipt paths/pins or internal
error detail is returned.

Only standalone, complete SHA-1 checkouts supported by S2 can pass. Linked
worktrees, missing objects, unsafe Git configuration, source/brief/receipt drift
and unsupported compatibility remain rejected. The operator must provide an
authentic approved binding and independently trusted receipt/artifact pins; do
not derive expectations from a receipt, relabel MTT-690 logs as S2 acceptance, or
create operational evidence to make this tool pass. Fixture projections in tests
remain explicitly inert. A real approved target without these inputs is
**UNVERIFIED**, even if the tool's fixture tests pass.

This tool does not call `intake.Admit`, Claim, NativeBridge, Journal or a Notion
writer. Source verification observes current applicability; it grants no execution,
Claim, lifecycle acceptance, atomic filesystem snapshot or future immutability.

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

The previously accepted Codex CLI `check_scope` call proof is preserved. This
slice's `verify_source` proof uses an actual SDK Client and inert S2 fixtures;
direct Codex `verify_source` invocation and an operational approved target remain
unverified. Client settings and Provider-backed prompts are not changed or run.
`make check` is offline after the explicit `make deps`.
