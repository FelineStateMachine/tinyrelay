# Approved protocol and client expansion

Approved 2026-09-07. Full retained bindws behavior and UX parity remains required.
BUD-05 media optimization and BUD-07 payments are excluded. Tenants remain
permanent, without trials, fuel, billing, or product resource entitlements.

## Acceptance ledger

An implementation is complete only after relevant mounted routes, permissions,
restart behavior, interoperability, and user journeys have evidence. Compilation
or a feature flag alone does not establish protocol support.

| Work | Required outcome | Status |
| --- | --- | --- |
| GRASP-01 | Native streaming Git, signed refs, recovery and HTTP interoperability | In progress |
| GRASP-02/03 | Durable independent repository synchronization and complete collaboration filtering | In progress |
| GRASP-05/06 | Archive and alternative PR behavior with recoverable inventory | In progress |
| GRASP-08 | Private service boundary, authenticated metadata and Git, private peer replication | In progress |
| BUD-01/02/04/06/08/09/11/12 | Correct file retrieval, ownership, authorization, pagination, mirrors and reports | In progress |
| BUD-03/10 and NIP-B7/94/92 | Portable server lists, references, metadata and client fallback | In progress |
| NIP-34/22 | Repository activity, threads, statuses and GRASP server lists | In progress |
| NIP-65/77/67 | Inbox/outbox recovery and accurate scalable reconciliation | In progress |
| NIP-42/98/17/59/70 | Consistent authentication and private data access | In progress |
| NIP-43/56 | Membership requests and integrated moderation | In progress |
| NIP-11/66/86 | Truthful capabilities, optional peer monitoring, operational visibility | In progress |
| NIP-96 | Preserve upload/get/delete compatibility through shared file storage | In progress |

## Contained client experience

The same embedded site must provide repository discovery, branch/tag selection,
source trees, escaped source previews, downloads, history, commit diffs and
repository activity. File browsing includes pagination, metadata, previews,
downloads and existing signed management actions. Connect publishes owner-signed
service lists through the existing signer. Operational pages expose synchronization,
jobs, failures, storage, backups and capabilities with links to recovery actions.

All routes share the tenant's current access policy; private data must not become
public through a browsing or download route. The site retains its plain HTML
foundation and fixed desktop/mobile variants. The user explicitly approved richer
Git/source components: repository tabs, branch selectors, breadcrumbs, file trees,
syntax highlighting, compact line numbers, colored diffs and clone controls.
Basic forms, locally served scripts and optional Fixi enhancement remain the
foundation. Native Git supplies object reads and diffs. Large
files are streamed for download and previews disclose truncation.

Observability accompanies implementation: queue age, retries, last success,
subprocess time, memory and relay acknowledgement latency under mixed workloads.
Real repository performance fixtures on Slate remain regression evidence; tail
latency is still an investigation target.
