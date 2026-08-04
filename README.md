# provider-sentry

The Codefly **Sentry reference provider** — a `codefly:provider` leaf agent that
observes a Sentry project and its client keys and projects the generic
`error-tracking@1` (runtime/browser) and optional `error-tracking-build@1`
(build-only) configuration contracts. It performs **no remote mutation**.

It is the observe/project counterexample in the reference set (Stripe, Sentry,
Resend): it proves a provider can be useful with no remote write and no
callback, that secret-bearing read responses are filtered before provider code,
and that browser-public, build-secret, and setup-secret values stay correctly
separated.

## Security model

The provider is treated as **untrusted code**. It never holds a Sentry token and
never sees a client-key secret:

- Sentry API calls go through the **host broker** (the `ProviderHost`
  `ExecuteRequest` callback), never a direct socket — the sandbox declares
  `network: deny`.
- The **setup credential** (`org:read`) is an opaque handle used only to observe
  the organization's projects and their client keys. It is **never projected**
  into any output. The reads are organization-scoped because the broker binds
  every request path to one remote-id segment (see [Broker path
  model](#broker-path-model)), so the observable endpoints are the
  organization's project list and client-key list rather than the two-segment
  project-scoped endpoints.
- The **build credential** (`org:ci`) is a *separate* opaque handle projected as
  an opaque reference into the **build-only** `error-tracking-build@1` contract
  (`SENTRY_AUTH_TOKEN`). It never reaches frontend or backend runtime.
- **Runtime/browser** receive only the **public DSN** (`SENTRY_DSN`), which
  Sentry documents as safe to expose. There is no runtime management token.
- Sentry's client-key response places a legacy `secret` and a `dsn.secret`
  beside `dsn.public`. The manifest marks both legacy locations
  `SUPPRESS_REPORT_PRESENCE` across the whole array, so the broker forwards only
  the public DSN and safe identifiers; the raw secret bytes never enter the
  provider process, logs, state, plan, receipt, diagnostic, cassette, or Git.

## Origin admission

The manifest declares one bounded `api` origin rule covering the Sentry SaaS
default and the US/DE regional silos (`sentry.io`, `us.sentry.io`,
`de.sentry.io`). A binding selects the exact origin within that ceiling and the
host admits the exact scheme/host/port. A **self-hosted** origin is outside the
SaaS ceiling: it requires explicit, per-deployment governed host admission and
is never blanket-admitted by a wildcard pattern. Wrong-region and credentialed-
redirect behavior is a host/broker concern and is qualified live rather than
assumed here.

## Broker path model

The host broker binds **every** request path parameter to a single planned
remote id (`bindPath`), so an admitted request can address exactly one remote
resource. Sentry's project-scoped endpoints
(`/api/0/projects/{organization}/{project}/…`) carry two distinct path segments
and are therefore not broker-executable. The provider observes through the
**organization-scoped** endpoints instead — one remote-id segment (the
organization slug) each:

- `GET /api/0/organizations/{organization}/projects/` — the configured project
  is selected by slug;
- `GET /api/0/organizations/{organization}/project-keys/` — the project's client
  keys are kept by `projectId`.

This is why the setup credential is `org:read` rather than `project:read`.
Sentry paginates these lists with a `Link`-header cursor, which the broker (a
body-only response filter) does not surface to provider code; multi-page
continuation is therefore host-driven via `ObserveRequest.cursor` /
`MaterialObservation.next_cursor`. An observation the host cannot prove complete
is a blocked/uncertain safety failure at plan time — never a silent `array[0]`.

## Client-key selection

Selection is deterministic and never picks `array[0]`:

1. an explicitly configured `client_key_id` wins if it is active and
   project-owned;
2. otherwise, exactly one active key is selected;
3. zero active keys → `MANUAL_ACTION`;
4. multiple active keys → `BLOCKED` with the safe key ids;
5. an explicit key that is missing or revoked → `BLOCKED`;
6. an incomplete client-key observation → `BLOCKED` (a truncated page can hide a
   second active key).

Repeated observation of the same keys yields the same plan digest (no diff).

## Plan / actions (v0.1)

Observe/project only: `VALIDATE`, `OBSERVE`, `NO_OP`, `PROJECT_OUTPUT`,
`BLOCKED`, `MANUAL_ACTION`. No `CREATE`/`UPDATE`/`REPLACE`/`DELETE`/`IMPORT`
action exists on any resource type, so an admitted request cannot express a
remote mutation. `PROJECT_OUTPUT` is still an explicit, approved host action —
this proves the same Plan/Apply/commit path works when there is no remote write.

## Scope diagnostics (honest)

The organization projects and client-key endpoints require `org:read`; `org:ci`
is appropriate for CI/release/source-map work. Sentry does not expose arbitrary
token-scope introspection, so build-token purpose fit is reported as
**unverified** rather than asserted. Whether an `sntrys_` organization token can
reach these endpoints is qualified in live acceptance, not assumed.

## Protocol status (v0.1)

The provider foundation (contracts, broker, coordinator, conformance) is green.
This repository ships the complete **offline** surface and the broker-driven
surface, cassette-tested in-process against `core/provider/broker`; the
production host connection and the live/dogfood tiers land with the running
coordinator and a dedicated Sentry test project.

| Surface | State |
|---|---|
| `GetProviderInformation`, `Validate`, `Plan`, `UpgradeState` (offline) | **Implemented + tested** |
| Manifest, runtime catalog, artifact packaging | **Implemented + validated against core** |
| `error-tracking@1` + `error-tracking-build@1` projection planning | **Implemented + tested** |
| Response-policy filtering of poison client-key secrets | **Tested against `core/provider/responsepolicy`** |
| Error / rate-limit → diagnostic mapping | **Implemented + tested** |
| `Observe`, `ApplyAction` (projection), `Doctor` (broker-driven) | **Implemented + Tier-1 cassette tested** against `core/provider/broker` |
| Production host connection wiring | **Pending** — the standalone binary attaches no host; the broker-driven methods fail closed until the coordinator transport lands |
| Tier 3 live Sentry, Tier 4 starter dogfood | **Pending** — need the host and a dedicated Sentry test project |
| Starter `scripts/setup/sentry.sh` → non-writing shim | **Pending** — gated on plugin parity (tracked in `codefly-dev/module-saas-starter`) |

See [`docs/mutation-pressure-test.md`](docs/mutation-pressure-test.md) for the
pure design analysis of the future mutation surface (project/key creation,
rotation, revocation) — documented to surface protocol assumptions hidden by an
observe-only provider, **not** to expand v0.1 scope.

## Layout

```
provider.codefly.yaml     capability manifest (the security-load-bearing ceiling)
agent.codefly.yaml        agent identity (kind: codefly:provider)
main.go                   agents.Serve entrypoint; discovers verified identity
internal/provider/        the Provider service implementation
cmd/package/              produces the verified artifact layout (binary + manifest + descriptor)
docs/                     the future-mutation design pressure test
```

## Develop

```bash
make test      # go test ./...
make vet
make build     # -> ./provider-sentry
make package   # -> ./dist/{provider-sentry, provider.codefly.yaml, provider.artifact.json}
```

`core` is a public module, so `go` fetches it through the module proxy with no
extra configuration.
