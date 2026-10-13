# S1 access, fixture claim and result handoff

S1 supplies a bounded read/access contract and a conditional writer model for fixtures.
The CLI remains dry-run only. No production claim writer, credential lookup, provider job,
journal, delivery writer or recovery loop is included.

## Official API boundary

The [retrieve-page reference](https://developers.notion.com/reference/retrieve-a-page)
requires caller authentication and the connection's content access.
The [authorization guide](https://developers.notion.com/guides/get-started/authorization)
describes connection capabilities and page grants.
The [update-page reference](https://developers.notion.com/reference/patch-page)
requires update capability, bearer authentication and Notion-Version; its documented
`is_locked` flag applies to UI editing and does not restrict API updates.

The inspected update request contract does not establish expected-version CAS or a
fencing primitive. The documented [409 conflict status](https://developers.notion.com/reference/status-codes)
does not supply those semantics. Therefore production claim support is **unqualified**
and `Reader.Claim` and `AccessReader.Claim` return `CLAIM_WRITE_UNAUTHORIZED`.
Read-write-read, page locks and agent labels are not atomic ownership proof.
This conclusion concerns the documented/qualified contract; no authenticated request
or unsupported operation probe was made.

## Scoped reads

`NewAccessReader` reuses the existing strict Reader with an independently authorized
caller scope: exact canonical workspace/project/data source, a finite task-to-page map,
finite pages including dependencies, a single read action and matching access reference.
References are host-supplied authority bindings, not authentication of an issuer.
No page property can construct or expand them.

Every GET is restricted to the fixed official HTTPS host and an allowlisted page or
its markdown endpoint. Dependency escape is rejected before the delegated transport.
Scope maps are copied into the private transport; caller mutation cannot expand access.
The existing Reader's token/version/timeouts/redirect/body/schema checks are preserved.
Tokens are supplied by the caller to the existing Reader, never extracted from files
or a plugin. A nonempty token or a valid reference alone is not live access permission.

No actual API access occurred. Existing Reader response-schema compatibility with the
current official service and actual authentication remain **UNVERIFIED**.

## Fixture conditional writer

`BindClaim` binds the approved profile/contract and a verified S2 `SourceProof` to
task/page/project/workspace/run/access reference/owner/commit/tree/source/brief/receipt pins.
Only proof obtained from `GitSource.Check` is admissible to the authorized host.
The value type cannot authenticate its own producer.

`ClaimTransition` validates expected nonce/fence, ownership and lease, source/brief/
receipt identity, lease bounds and a consumed run. The check and state transition
must occur in one authoritative conditional operation. **The helper is not an atomic
operation.** A mutex-backed authority exists only in `s1_test.go`, with two independent
controller clients. No production path accepts an injected claim writer.

The fixture admits exactly one claimant, increments its fence and consumes that run.
An active lease, unknown owned lease, stale nonce/fence, overflowing fence, changed
binding or replay is rejected. Context is checked at the write boundary.
Timeout after possible application, partial state or lost response returns an unknown
effect and does not dispatch. No automatic acquisition retry or ownership promotion
is implemented.

The existing `intake.Admit` performs source verification before/after claim and re-reads
the strict task snapshot. The fixture writer additionally checks actual S2 proof at
the atomic boundary and confirms its fence on readback. Source/brief/receipt/fence drift
is denied. The positive control invokes an actual fake consumer with the delivered
source/brief identities. This test-local admission is not a native launch or authority
grant. Fixture PASS does not prove Notion atomicity or cross-host fencing.

## Result contract for MTT-574

`ResultHandoff` describes a future qualified conditional-replace operation, immutable
task/run/owner/claim nonce/fence/source/brief/receipt delivery key and payload digest.
The same producer identity retains its key; different payloads conflict.
`ConfirmResultReadback` requires the exact identity/key/operation/payload digest.
Neither function sends a result, persists it or decides lifecycle acceptance.

A future authorized writer must establish conditional semantics and actual readback.
It must persist result before Notion delivery, reconcile only the same durable result,
stop on unknown effects and refuse blind append/provider retries or stale promotion.
This file does not implement that writer or MTT-574 recovery.

## Qualification and scope

S2 supports complete standalone Git checkouts; linked worktrees are not enabled here.
Existing core, Reader parser, intake, source verifier, executor/parser and CLI behavior
are preserved. S1 fixtures use only task-owned local Git objects and in-memory transport.

Official authentication, authoritative production CAS/fencing, trusted operational host
provenance, native Codex identity/auth/sandbox/process cleanup and durable recovery are
unqualified. MTT-570 stays operationally BLOCKED, MTT-572/573 and S1 stay READY_FOR_REVIEW
after static review, and MTT-574 stays BACKLOG. Static review or Git integration does
not create runtime authority or DONE.
