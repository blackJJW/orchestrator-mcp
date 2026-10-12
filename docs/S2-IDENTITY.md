# S2 Git / Source / Brief / Receipt identity contract

S2 is a read-only implementation of the existing `intake.Source` seam. `internal/config.VerifySource` owns local filesystem and Git object inspection. `intake.NewGitSource` binds its input to the existing profile/contract and returns `core.SourceSnapshot`; `GitSource.Check` additionally exposes commit/tree/source-set/base/brief and receipt pins. Core policy, Notion reader, Codex executor and CLI behavior are unchanged. A proof is neither a claim nor launch or lifecycle authority.

## Trusted inputs

`config.SourceSpec` is supplied by an authorized host, not reconstructed from Notion status or receipt fields:

- Exact canonical workspace and project ID; finite complete source paths and approved behavior-input closure.
- Full lowercase SHA-1 candidate commit, tree and review-base IDs, explicit local candidate/base branch refs, and independently pinned source-set SHA-256.
- The immutable approved brief text and existing `core.Digest` SHA-256. Mutable owner/lease/status/checkpoint are not part of this text. Admission independently compares the task brief before and after its fixture claim.
- Exactly one canonical and one independent-review receipt path with externally trusted SHA-256 pins. An attacker-supplied receipt plus its own digest is not a trusted input.

The constructor copies all path/pin slices and binds project/workspace/brief/frozen source to the existing contract. `Verify` receives the task's base context. Caller/profile authority remains checked by existing admission; verifier success cannot supply missing authority.

## Git and filesystem proof

Version 1 supports standalone, complete SHA-1 Git checkouts using the installed `/usr/bin/git`; linked-worktree `.git` files, shallow/promisor/alternate/replace/graft sources and unsupported repository configuration fail closed. It issues only fixed local read commands with a sealed environment, global/system configuration disabled, no lazy fetch, optional locks or prompt, finite output and timeout. No network fetch, shell, Git filter/helper or user credential configuration is requested. Host-installed Git trust and concurrent writer exclusion remain host responsibilities.

HEAD and the approved candidate branch must resolve to the exact candidate. Its actual tree contains exactly the approved paths and only regular blob modes. Local bytes and executable mode must match the Git blobs. Extra tracked or untracked inputs outside `.local/`, missing/modified files, symlinks, non-regular files and escapes are rejected. Source reads use bounded nonblocking descriptors and an `os.Root` confinement boundary. Git metadata must be local, regular and without symlinks; unsupported configuration is refused before source inspection.

The source-set algorithm is SHA-256 of compact JSON mapping sorted relative paths to each file's SHA-256, without a newline. This matches the existing ASCII-path source-set evidence convention. Git commit and tree IDs are separately read and checked; they are never treated as source-set SHA-256. Source/refs/base/evidence are read again before returning. Existing intake performs fresh verification before and after claim. This detects observed drift; it is not a filesystem transaction, durable fence or atomic claim. Committed/current-byte applicability does not certify a future staged index/tree; publication separately checks every staged byte/mode and the final commit/tree mapping.

## Main advancement

Review base is an approved context and accumulated review range. It must be an ancestor of the candidate; it need not equal current main. The task base and actual base branch may advance from that context when ancestry and identical Git blob/mode identities are proved for every approved behavior input. If main has adopted the exact approved candidate or a descendant, candidate ancestry and unchanged candidate behavior inputs supply the corresponding proof. Context comparisons accept identical absence on both bases for candidate-only new inputs; presence on just one side fails. Unknown added paths outside the finite approved input set fail closed. At minimum all Go files, module files and Makefile must belong to the behavior closure. The host must include every additional behavior-affecting fixture/config/input; omission cannot be made safe by a hash or Notion label.

Unchanged behavior inputs with a documentation-only advancement can be accepted without requiring SHA equality. Changed/absent behavior inputs, unrelated ancestry or unknown compatibility are denied. There is no automatic semantic analyzer or permission to accept arbitrary changes. Candidate branch or checkout movement always invalidates the exact candidate binding.

## Evidence contract

`config.SourceReceipt` is a small strict projection of existing evidence, not a new runner or gate. Version 1 requires exact keys and JSON types, one object, no duplicate keys at any level, and no typed key aliases. Required strings cannot be null; source-bound head is explicitly an empty string. Coverage elements and nested path/hash values must be strings. It records kind, result, project, review base, binding, head, tree, source-set, brief digest, the complete input hash map, coverage and raw artifact path/hash pins.

- Canonical evidence requires `kind=canonical`, `result=PASS`, and coverage containing the repository's `make check`.
- Independent review requires `kind=review`, `result=ACCEPTED`, and `independent-review` coverage.
- Both require an exact matching base/tree/source-set/brief/input map, nonempty bounded artifact pins and matching raw bytes. All evidence paths stay under task-owned `.local/`; errors are redacted to `SOURCE_UNPROVEN`.
- `binding=commit` requires the receipt head to be an actual candidate ancestor with the identical tree. Thus an unchanged checkpoint commit can reuse a valid prior receipt. Mere ancestry with different source cannot.
- `binding=source` with an explicitly empty head allows trusted pre-commit source evidence to map to the identical final reviewed tree, complete inputs/config and brief. It does not assert that tests ran at the later Git commit.

The host must establish authentic canonical/review provenance and coverage before pinning a projection. No arbitrary legacy JSON/PASS text or filename is automatically imported, trusted or retagged. Hash checks establish binding/integrity, not reviewer identity or authorization. Nonmatching failed/tampered/missing/stale evidence is refused. Actual old evidence is preserved; receipt producers can supply this projection only after explicit provenance mapping.

## Validation and S1 handoff

Actual task-owned local Git fixtures cover invalid commit/tree/source/brief/receipt/artifact, moved refs, incompatible advancement, missing objects, unsafe configuration/path/symlink and strict receipt schema. Positives cover exact candidate reuse, unchanged checkpoint receipt reuse, source-bound mapping and approved compatible advancement through actual verifier → Admission → fake consumer. Invalid inputs have zero fixture claims/consumer calls; valid passes observe two reads/verifications, one fixture claim and one fake delivery. Post-claim drift observes the prior valid fixture claim but blocks delivery. Concurrent read-only verification is race checked. Inert receipt fixture logs are explicitly fixture projections, not genuine canonical validation of their fixture repositories.

Owning validation remains `make check`; targeted S2 regressions and narrow race coverage supplement it. Existing matching evidence may be reused for unchanged scope. Private logs/projections/reports and local Git fixtures remain outside public Git.

S1 consumes trusted source inputs and `GitSource.Check` evidence, plus the existing immutable contract/brief. It must supply independently authorized access and confirmed owner/nonce/lease/fencing; unknown writer semantics deny claims. Neither this verifier nor Notion metadata can create those rights. S1 should keep source verification around coordination changes and pass the same exact source/brief to later trusted launch qualification. Official authentication, atomic claim/fencing, native provider sandbox/process cleanup, durable journal and result readback remain unqualified and separately authorized. MTT-574 stays BACKLOG.
