# Orchestrator local controller

A native Go CLI that compares an explicit project profile with a request in dry-run mode. The published baseline contains the profile loader, pure scope decision, CLI and inert fixtures.

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

`internal/config` owns filesystem validation; `internal/core` returns synchronous pure decisions; `cmd/orchestrator-mcp` handles CLI and JSON I/O. This baseline has no provider execution, network request, credential reader, claim writer, daemon, journal, Docker package or MCP server. Workspace comparison is a dry-run check, not a runtime sandbox.

The initial `main` uses the independently accepted bootstrap source. Runtime source, tests, build recipe and fixtures retain their accepted bytes; publication changes are limited to this README, the public contract and ignore rules. Unaccepted adapters are excluded. Later implementation candidates require separate branches and independent review before integration.

Before every publication, use an explicit reviewed file allowlist and inspect every staged file and the prospective commit history. Do not bulk-stage a private working directory. Keep local evidence, reports, binaries, caches, credentials, real environment files and personal settings outside Git. Git publication does not grant provider, runtime or acceptance authority.

See [the bootstrap contract](docs/MTT-571.md).
