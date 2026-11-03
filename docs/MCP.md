# Read-only MCP scope server

MTT-690 implements the first MCP-first v6 surface. The subsequent `verify_source`
slice exposes the existing S2 verifier without changing MTT-690's completed scope.
The optional `plan_execution` slice adds project-owned routing suggestions only.
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
loopback port and prints its actual URL. Direct execution stays fixed to loopback;
only the explicit Docker mode below listens on `0.0.0.0` inside the container.
LAN addresses and remote deployment are not supported. Ctrl-C/SIGTERM
shuts the server down. Tests close their own listeners; no daemon is installed.

The operator must supply one explicit JSON profile. `config.Load` validates it
before the listener opens; the profile is held for that server's lifetime.
Missing/invalid profiles fail startup. Profile changes require an operator restart.

## Tool and result

With neither `-source-spec` nor `-routing-policy`, discovery exposes only `check_scope`, with read-only/closed-world
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

Successful startup adds `verify_source` as a read-only/closed-world tool.
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
remain explicitly inert. Current real-target readiness is
**APPROVED_TARGET_NOT_READY**; its S2 SourceProof eligibility is **UNVERIFIED**.
Do not invoke a real target until its exact approved Task/Contract/SourceSpec/task
base, applicable standalone complete checkout, and existing canonical/review
receipts with independently approved pins are available together. These are the
existing S2 applicability inputs for that target, not new development gates. Do
not select a target, create inputs or convert old receipts to satisfy this example.

This tool does not call `intake.Admit`, Claim, NativeBridge, Journal or a Notion
writer. Source verification observes current applicability; it grants no execution,
Claim, lifecycle acceptance, atomic filesystem snapshot or future immutability.

## Optional `plan_execution`

The operator may supply one additional JSON policy at startup. Existing
`config.Profile` continues to own scope; it has no executor catalog or routing
map, so its schema is unchanged. Omit `-routing-policy` to keep the existing tool
surface. An invalid supplied policy prevents listening.

```sh
.local/bin/orchestrator-mcp-server -profile testdata/projects/alpha.json -routing-policy testdata/routing/alpha.json -port 0
```

Use the printed free loopback URL for a separate development server; do not
restart or replace an already running Docker service to test this addition.
For an invocation-only Codex connection, use the example below with that URL
and `enabled_tools=["plan_execution"]`.

The [inert alpha policy](../testdata/routing/alpha.json) has this exact shape:

```json
{
  "schema_version": 1,
  "project_id": "alpha",
  "executors": [
    {"id":"implementation_primary","supported_profiles":["BALANCED"],"supported_reasoning":["HIGH"]}
  ],
  "routes": [
    {"task_type":"IMPLEMENTATION","executor_id":"implementation_primary","capability_profile":"BALANCED","reasoning":"HIGH"}
  ]
}
```

The file must be regular, non-symlink UTF-8 JSON of at most 64 KiB, opened
nonblocking and read once. All shown keys are required and case-sensitive;
unknown/duplicate keys, nulls, wrong types and trailing JSON are rejected,
including nested objects. Project ID must match the fixed profile. Project,
executor and task-type IDs are exact, nonempty strings of at most 128 bytes,
without whitespace edges, control characters or wildcard patterns. Executors
and routes must be nonempty, with unique executor IDs and task types.

Capability vocabulary is `FAST`, `BALANCED`, `DEEP`, `REVIEW_DEEP`; reasoning is
`LOW`, `MEDIUM`, `HIGH`, `EXTRA_HIGH`, from the existing generic design. Each
executor declares nonempty, unique supported values, and every route must name
a declared executor supporting its exact profile/reasoning. These are operator
policy declarations, not measured runtime qualification or provider models.

The sole client field is the optional string `task_type`. For example:

```json
{"task_type":"IMPLEMENTATION"}
```

```json
{"project_id":"alpha","routing_allowed":true,"reason":"ROUTE_PLANNED","executor_id":"implementation_primary","capability_profile":"BALANCED","reasoning":"HIGH","manual_dispatch_required":true,"authority_granted":false,"execution_started":false}
```

The [inert beta policy](../testdata/routing/beta.json), with the beta profile,
routes the same `IMPLEMENTATION` input to `builder_pool` / `FAST` / `LOW` using
the same pure Core function. No provider/executor name is built into the Core.
Policy files are not reloaded or selectable by clients. Extra fields such as
executor, project, workspace, policy path or `execute` are SDK input errors.

Missing/empty/`UNKNOWN` or unmapped task types return `routing_allowed=false`,
`reason=NEEDS_CONTEXT` and no executor/profile/reasoning. The type is a caller
supplied label, never inferred from objective text; supply a type mapped by the
operator policy. A missing policy exposes no tool (`NO_ROUTING_POLICY` at the
pure Core seam). An unsupported executor/profile/reasoning is rejected at
startup and also denied by the Core with `EXECUTOR_UNSUPPORTED`.

Every result requires manual dispatch and has `authority_granted=false` and
`execution_started=false`. A suggestion is not task scope approval, a Grant,
Claim, source verification, capacity/independence assessment or runtime launch.
There is no fallback, override, handoff, digest/receipt or provider call in this
slice. `check_scope`, optional `verify_source` and the dry-run CLI keep their
existing contracts. This source addition does not update an existing image or
running container; deploying it is a separate action.

## Codex CLI: invocation-only connection

Run the server manually in one terminal using the command above: omit
`-source-spec` for `check_scope` only; supply it only for an already approved
binding to add `verify_source`. Run Codex from the repository root in another
Bash or zsh terminal. Reuse the existing official Codex login and configuration; no new login,
API key, MCP OAuth login or separate `CODEX_HOME` is needed for this local endpoint.
The prompt uses Codex's normal model service; it does not launch the Native Controller.

The accepted calls used CLI 0.163.0 and `-c` overrides for that invocation, not
`codex mcp add` or edits to saved configuration. Choose an unused server name
(shown as `orchestrator_readonly`) and use the URL actually printed by the server:

```bash
orchestrator_mcp_args=(
  --disable plugins --disable apps --disable shell_tool --disable unified_exec
  --disable shell_snapshot
  -c 'hooks.enabled=false' -c 'notify=[]' -c 'web_search="disabled"'
  -c 'approval_policy="never"'
  -c 'mcp_servers.orchestrator_readonly.url="http://127.0.0.1:8080/mcp"'
  -c 'mcp_servers.orchestrator_readonly.enabled=true'
  -c 'mcp_servers.orchestrator_readonly.required=true'
  -c 'mcp_servers.orchestrator_readonly.enabled_tools=["check_scope"]'
)
codex "${orchestrator_mcp_args[@]}" mcp list --json
```

Overrides merge with existing MCP entries. For an isolated call, append an
invocation-only `-c 'mcp_servers.<exact-existing-name>.enabled=false'` to the array
for each other enabled server already listed, then list again. Replace the name
with an actual listed entry; do not add a nonexistent placeholder entry. The
recorded tests enabled only the target server and left saved entries unchanged.

With the example alpha profile and no source binding, call `check_scope` once:

```bash
orchestrator_workspace="$(cd testdata/workspaces/alpha && pwd -P)"
codex "${orchestrator_mcp_args[@]}" exec --ephemeral --sandbox read-only --json \
  "Use only orchestrator_readonly.check_scope. Call it once with {\"task_id\":\"bootstrap-check\",\"workspace\":\"${orchestrator_workspace}\",\"permission\":\"source.read\"}. Do not use other tools or change files. Stop if unavailable."
```

Only when the server was started with existing approved `-source-spec` inputs,
replace the placeholder below with that binding's exact Task ID and call:

```bash
codex "${orchestrator_mcp_args[@]}" \
  -c 'mcp_servers.orchestrator_readonly.enabled_tools=["verify_source"]' \
  exec --ephemeral --sandbox read-only --json \
  'Use only orchestrator_readonly.verify_source. Call it once with {"task_id":"ALREADY_APPROVED_TASK_ID"}. Do not use other tools or change files. Stop if unavailable.'
```

Do not run that real-target example while readiness is `APPROVED_TARGET_NOT_READY`.
An inert Fixture binding tests consistency of test inputs only; it cannot stand in
for approved real inputs. No operational binding or receipt is bundled here.

`mcp list` confirms configuration only. Inspect the JSON event stream for the
actual completed `mcp_tool_call`, its arguments and structured result; Codex's final
prose alone is not proof. `-c` does not persist the entry, and `--ephemeral` avoids
persisting the session rollout. Stop the foreground server with Ctrl-C/SIGTERM;
there is no saved MCP entry to remove. If a port is occupied, leave its process
alone, use `-port 0`, and substitute the printed URL in the invocation options.

See official [one-off configuration](https://learn.chatgpt.com/docs/config-file/config-advanced)
and [CLI options](https://learn.chatgpt.com/docs/cli/reference).

## Docker Compose (local only)

One multi-stage Dockerfile builds only the existing MCP server with Go 1.27.0
and the unchanged locked modules/SDK 1.8.0. The runtime contains that binary and
Debian's Git package/dependencies for S2's fixed `/usr/bin/git`; no Codex,
supervisor, Docker socket or host credentials are included. The build context is
allowlisted by `.dockerignore`, excluding `.git`, `.local`, tests and unrelated adapters.

The Compose service uses UID/GID 65532, a read-only root filesystem, no Linux
capabilities, no-new-privileges and only two explicit read-only bind mounts: the
public alpha profile and its workspace. Missing bind sources are not created.
The profile's relative workspace resolves to `/workspaces/alpha` inside the image.
It is a scope-check demo, not an approved real S2 target.

Docker Desktop must already be running. These commands use its existing local
`desktop-linux` context without changing the global context or Desktop settings:

```sh
docker --context desktop-linux compose up -d --build
docker --context desktop-linux compose ps
docker --context desktop-linux compose logs --tail 50 mcp
docker --context desktop-linux compose restart mcp
docker --context desktop-linux compose stop mcp
docker --context desktop-linux compose down
```

Use a separate `-p` project name consistently for an isolated trial. `restart:
unless-stopped` applies while Docker Desktop/Engine is running; it cannot start
Desktop itself. `stop` sends SIGTERM with a 10-second grace period for the
server's bounded shutdown. `down` removes this project's container and network;
neither command removes bind-mounted source. Test services must be stopped and
removed when the trial ends; packaging is not approval for ongoing deployment.

The service passes `-container-listen`, which requires Docker's regular
`/.dockerenv` marker and changes only the listener to IPv4 `0.0.0.0:8080`. This
marker is an accidental-use guard, not execution or host-trust authority. The
published port is **`127.0.0.1:8080:8080`**, never a LAN/wildcard host bind. Clients
use **`http://127.0.0.1:8080/mcp`**, not the internal `0.0.0.0` log address. Do not
enable host networking, privileged mode, a proxy or a tunnel. If host port 8080
is occupied, stop this trial rather than terminating another process or widening
the bind. No arbitrary listen-address flag is provided.

Reuse the Codex invocation-only example above with the same URL. For this Compose
demo set `orchestrator_workspace=/workspaces/alpha` instead of the host checkout
path; `task_id=bootstrap-check` and `permission=source.read` remain the same.
`unapproved` remains denied and all Decisions have `execution_started=false`.
After `restart`, clients can reconnect using the same URL; restarting the service
does not create a Task grant or dispatch.

The default service omits both `-source-spec` and `-routing-policy`, so it exposes only `check_scope`.
`verify_source` code and S2 trust rules remain unchanged. An operator may use it
only with an already approved profile/Contract/SourceSpec/task base and complete
standalone checkout, receipts and raw artifacts accessible read-only at their
**original canonical paths** inside the container. The runtime UID must already
have sufficient access and Git ownership eligibility. A host binding is not
automatically portable to a renamed container path. Do not rewrite paths,
SHA/pins/receipts, change host permissions, copy credentials or weaken Git safety
to make it pass. Git availability alone does not establish target applicability.
Real-target readiness stays **APPROVED_TARGET_NOT_READY** and SourceProof
eligibility **UNVERIFIED**; no real-target S2 check is required for this packaging slice.

## HTTP and authority boundary

- Direct execution listens on IPv4 `127.0.0.1`. Docker mode uses the container
  interface with host-loopback-only publication. The SDK's default localhost Host
  protection stays enabled, with the same loopback Host policy also enforced for
  container NAT destinations. Forwarded headers do not grant a remote bind.
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

## Tool-call logging

Completed SDK `tools/call` handling emits one JSON record to process stderr using
standard `log/slog`: UTC `time`, fixed `tool`, `status`, stable `reason`, and
`duration_ms`, plus the standard level/message fields. Status is `success`,
`denied`, `needs_context` or `error`. SDK/protocol errors use `TOOL_CALL_ERROR`;
unrecognized tool names are logged as `unknown`. Discovery and HTTP requests
rejected before SDK Tool handling are not call logs. Existing responses are unchanged.
`success` means a read-only decision completed, never Task execution or a Grant.

```json
{"time":"2026-11-04T02:30:00Z","level":"INFO","msg":"mcp_tool_call","tool":"plan_execution","status":"success","reason":"ROUTE_PLANNED","duration_ms":0.25}
```

Use the same Compose file combination as the running service with
`docker --context desktop-linux compose ... logs --tail 50 mcp`. A source-only
change does not update a running image; deployment requires separate approval.
No request/response bodies, task inputs, headers/tokens, paths, source proofs,
raw errors or inferred agent identities are logged. There is no file/DB sink,
rotation service, new flag or runtime configuration; Native Journal is independent.

## Evidence and limits

The new tests use the official SDK Client against real localhost HTTP listeners,
including the production entrypoint. They cover initialize/tools/list/tools/call,
exact allowed/denied Decisions, invalid arguments/tools, fixed startup profile,
Host/Origin/body limits, loopback binding, non-loopback connection rejection and
graceful shutdown. The existing CLI regressions remain in `make check`.

Recorded status on 2026-11-03, reusing existing accepted evidence:

| Surface | Result | Scope |
| --- | --- | --- |
| `check_scope` | Codex CLI direct call **PASS** | Actual completed MCP calls; allowed/denied scope, `execution_started=false` |
| `verify_source` | Codex CLI inert Fixture direct call **PASS** | Actual completed MCP calls; `SOURCE_VERIFIED`/true and `TASK_NOT_ALLOWLISTED`/false, failed `proof=null`, both `execution_started=false` |
| Approved real target | **APPROVED_TARGET_NOT_READY** | No real-target call; complete approved inputs were not established |
| Real-target S2 SourceProof eligibility | **UNVERIFIED** | Fixture PASS and implementation acceptance do not establish target applicability |

The Codex calls used existing login and invocation-only settings; settings hashes
and the saved MCP list were unchanged, and the servers exited normally. The
[`verify_source` integration record](https://github.com/blackJJW/orchestrator-mcp/pull/11)
separates actual Codex Fixture calls from real-target readiness. Documentation
maintenance reuses these results; it creates no new runtime acceptance or pins.
`make check` is offline after the explicit `make deps`.
