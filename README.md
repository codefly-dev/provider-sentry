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
- The **setup credential** (`project:read`) is an opaque handle used only to
  observe the project and its keys. It is **never projected** into any output.
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

## Client-key selection

Selection is deterministic and never picks `array[0]`:

1. an explicitly configured `client_key_id` wins if it is active and
   project-owned;
2. otherwise, exactly one active key is selected;
3. zero active keys → `MANUAL_ACTION`;
4. multiple active keys → `BLOCKED` with the safe key ids;
5. an explicit key that is missing or revoked → `BLOCKED`.

Repeated observation of the same keys yields the same plan digest (no diff).

## Plan / actions (v0.1)

Observe/project only: `VALIDATE`, `OBSERVE`, `NO_OP`, `PROJECT_OUTPUT`,
`BLOCKED`, `MANUAL_ACTION`. No `CREATE`/`UPDATE`/`REPLACE`/`DELETE`/`IMPORT`
action exists on any resource type, so an admitted request cannot express a
remote mutation. `PROJECT_OUTPUT` is still an explicit, approved host action —
this proves the same Plan/Apply/commit path works when there is no remote write.

## Scope diagnostics (honest)

Project and client-key endpoints require project read scope; `org:ci` is
appropriate for CI/release/source-map work. Sentry does not expose arbitrary
token-scope introspection, so build-token purpose fit is reported as
**unverified** rather than asserted. Whether an `sntrys_` organization token can
reach project-read endpoints is qualified in live acceptance, not assumed.

## Protocol status (v0.1)

The provider foundation (contracts, broker, coordinator, conformance) is green.
This repository ships the complete **offline** surface; the broker-driven
methods and the live/dogfood tiers land with the running host and a dedicated
Sentry test project.

| Surface | State |
|---|---|
| `GetProviderInformation`, `Validate`, `Plan`, `UpgradeState` (offline) | **Implemented + tested** |
| Manifest, runtime catalog, artifact packaging | **Implemented + validated against core** |
| `error-tracking@1` + `error-tracking-build@1` projection planning | **Implemented + tested** |
| Response-policy filtering of poison client-key secrets | **Tested against `core/provider/responsepolicy`** |
| Error / rate-limit → diagnostic mapping | **Implemented + tested** |
| `Observe`, `ApplyAction`, `Doctor` (broker-driven) | **Pending** — need the running host; unimplemented from `sdk.Base` |
| Tier 1 cassettes, Tier 3 live Sentry, Tier 4 starter dogfood | **Pending** — need the host and a dedicated Sentry test project |
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
