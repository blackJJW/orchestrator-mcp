# Orchestrator local controller

A native Go CLI that compares an explicit project profile with a request in dry-run mode. The bootstrap, Notion admission and Codex adapters retain their independently reviewed behavior. The S2 candidate adds a read-only Git/source/brief/receipt verifier behind the existing admission interface; the CLI remains dry-run only.

## Build and validation

Use an installed Go 1.27 or later and `make`:

```sh
make check
```

The canonical check runs formatting checks, tests, vet and a native build. It uses only the standard library. Automatic toolchain installation and module downloads are disabled. Build output, caches, temporary files and validation telemetry stay in `.local/`; global telemetry settings are unchanged.

## CLI

```sh
# Missing profile: NO_PROFILE; execution_started=false
.local/bin/orchestrator-mcp

# Explicit matching fixture scope: DRY_RUN_ONLY; execution_started=false
.local/bin/orchestrator-mcp -profile testdata/projects/alpha.json \
  -task bootstrap-check -workspace testdata/workspaces/alpha -permission source.write

# The other fixture lacks source.write: PERMISSION_NOT_GRANTED
.local/bin/orchestrator-mcp -profile testdata/projects/beta.json \
  -task bootstrap-check -workspace testdata/workspaces/beta -permission source.write
```

The CLI writes a versioned JSON decision. `scope_allowed` means only that the supplied request matches the supplied profile. It does not grant execution authority. `mode` is `dry-run` and `execution_started` is always `false`.

A denied scope still returns exit 0 with its reason. Invalid profiles, missing workspaces and output failures return exit 1; invalid arguments return exit 2. Callers must inspect the decision fields.

## Profile contract

```json
{
  "schema_version": 1,
  "project_id": "example",
  "workspace_root": "../workspace",
  "task_allowlist": ["explicit-task"],
  "permissions": ["source.read"]
}
```

- A supported schema, exact project ID and existing workspace directory are required. Relative profile roots are resolved from the profile file's directory; request paths are resolved from the caller's directory.
- Normalized real workspace paths must match exactly. Parent, child, sibling and filesystem roots do not gain access.
- Tasks and permissions use exact, case-sensitive values. Wildcards, duplicates, blank values and control characters are rejected. Missing, null or empty allowlists grant nothing.
- The loader accepts one UTF-8 JSON object up to 64 KiB. Unknown or case-alias fields, duplicate keys, wrong types and trailing data are rejected. Profiles must not contain credentials.
- POSIX profile loading opens with `O_NONBLOCK`, verifies the opened descriptor is a regular file and reads that same descriptor. A writerless FIFO or replacement with a non-regular file cannot block profile loading.

The two fixture profiles point only to inert workspaces owned by this repository.

## Boundaries and source control

`internal/config` owns filesystem validation; `internal/core` returns synchronous pure decisions; `cmd/orchestrator-mcp` handles CLI and JSON I/O. The Notion adapter makes bounded, fixed-host GET requests only when a caller supplies a token; it never reads credential files and its claim method always refuses writes. The Codex adapter requires a separate host-issued read-only launch grant; fixtures execute only the test binary. The CLI does not invoke these adapters. There is no daemon, journal, Docker package or MCP server. Workspace comparison is a dry-run check, not a runtime sandbox.

The initial `main` used the independently accepted bootstrap source, followed by the reviewed Notion and Codex integrations. The S2 candidate is isolated on its review branch. Later implementation candidates require separate branches and independent review before integration. Existing public history is preserved; this candidate does not repair earlier merge metadata.

Before every publication, use an explicit reviewed file allowlist and inspect every staged file and the prospective commit history. Do not bulk-stage a private working directory. Keep local evidence, reports, binaries, caches, credentials, real environment files and personal settings outside Git. Git publication does not grant provider, runtime or acceptance authority.

See [the bootstrap contract](docs/MTT-571.md).

## S2 source identity verification

The [S2 contract](docs/S2-IDENTITY.md) describes `intake.NewGitSource` and its trusted host inputs. It reads a complete local Git checkout, exact source bytes, immutable brief and pinned canonical/review evidence. Commit, tree and source-set identities stay distinct. Compatible main advancement is limited to unchanged approved behavior inputs; unsupported or missing proof fails closed. The existing admission path repeats verification around its claim; no CLI command, claim writer or provider grant is added.

Canonical/review receipt projections and raw artifacts must be pinned by an independently trusted caller. Fixture receipts prove verifier behavior, not historical or live validation provenance. S1 still requires separately authorized official access and authoritative claim/fencing; S3 still requires actual native/sandbox/process qualification. Source verification supplies applicability evidence, never authority.

## Notion admission and test ownership

The [Notion admission contract](docs/MTT-572.md) describes the reader and host-supplied authority checks. Missing, null, duplicate or case-alias schema fields fail closed. Empty dependency relations, permitted null lease dates and optional extensions retain their supported meanings. Admission checks the selected task, project, permission, owner, nonce, lease, dependencies, brief and source before and after a fixture claim. An admitted result still has `execution_started=false` and grants no provider execution or lifecycle acceptance.

Notion tests use the actual reader and `intake.Admit` with a test-local fake consumer. They compile without a Codex adapter. Malformed snapshots produce zero claims, source verifications and fake dispatches. A positive control observes two reader passes, one in-memory claim, two source verifications and one fake consumer delivery; authority and post-claim nonce failures deliver nothing. These fixtures do not establish live authentication, atomic claims, a real source verifier or provider/MCP operation.

## Codex corrective candidate

The [Codex adapter contract](docs/MTT-573.md) preserves fixed argv, stdin-only briefs, native binary pinning, bounded output and deny-first grant checks. Process-wide reservations prevent repeated task/run or task/claim dispatch across Executor instances. Reservations survive completion and failure; context deadlines cannot exceed the grant lease, and expired leases never produce normal completion.

JSONL duplicate keys and typed field aliases fail closed. The versioned CLI stream exposes agent message text without a final-phase discriminator, so the controller requires exactly one response-contract candidate in the last completed agent message. Ordinary progress remains valid; multiple or conflicting candidates are unsupported. A successful result is only `COMPLETED_UNVERIFIED`, never acceptance or DONE.

Historical corrections2/2 remain recorded separately from the newly authorized C1/C2 slice. Fake/race validation and source review do not qualify live authentication, cross-process ownership, atomic Notion claims, real source verification, sandbox or descendant cleanup. No provider job, journal, MCP server or merge is part of this candidate.

## S1 access and fixture claims

The scoped Notion read adapter binds a caller-approved finite access scope. Production
claim stays denied. Conditional claim/fencing is proved only by a test-local authority
with actual Git source rechecks and fake consumer delivery. The immutable result/readback
contract performs no write or retry. See [S1 contract](docs/S1-ACCESS.md). Actual access,
Notion atomicity and native execution remain unqualified; the CLI stays dry-run only.
