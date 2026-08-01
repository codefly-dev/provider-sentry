# Future mutation pressure test (design only)

This is a **pure design analysis**, not a work item. v0.1 of `provider-sentry`
is observe/project only and implements **no** remote mutation. The purpose here
is to surface protocol assumptions that an observe-only provider hides, so the
`codefly.provider/v0` protocol is pressure-tested against a plausible future
mutating Sentry provider before any of it is built. Nothing below is in scope
for v0.1, and none of it should be implemented until it is separately approved.

## Candidate mutations

| Mutation | Sentry endpoint (shape) | Scope required |
|---|---|---|
| Create project | `POST /api/0/teams/{org}/{team}/projects/` | `project:write` (and a team) |
| Create client key | `POST /api/0/projects/{org}/{project}/keys/` | `project:write` |
| Rotate / update key | `PUT /api/0/projects/{org}/{project}/keys/{id}/` | `project:write` |
| Revoke / delete key | `DELETE /api/0/projects/{org}/{project}/keys/{id}/` | `project:admin` |

## Protocol assumptions the observe-only shape hides

1. **Personal-team side effect.** Sentry project creation is scoped under a
   *team*, and creating an organization can auto-create a personal team. A
   `CREATE sentry.project` action therefore has a side effect (a team) that is
   not the named resource. The protocol's ownership/attribution model assumes
   one action mutates one identified resource; a project-create that also mints
   a team violates that. The pressure test: a `CREATE` must be able to declare
   *dependent* created resources, or project creation stays out of scope. v0.1
   keeps it out (documented non-goal).

2. **Conditional organization scope.** `project:write` suffices for keys, but
   creating a project can require org-level context. The manifest's
   `credential_purposes` model binds one credential purpose per request; a
   mutation whose required scope is *conditional* (project vs. org depending on
   whether the team exists) cannot be expressed as a single static purpose. The
   pressure test: either the host resolves scope escalation as a separate
   governed admission, or the manifest must express conditional scope. v0.1
   avoids the question by never mutating.

3. **Capture/output timing on key rotation.** A client-key rotation mints a new
   public DSN. Unlike Stripe's webhook secret (a `CAPTURE_TO_SINK` value), the
   Sentry DSN is *public*, so it is a `FORWARD_SAFE` value the provider may hold
   — but the *old* DSN is still live until the consumer redeploys. A
   rotate-then-project sequence must therefore project the new DSN only after
   the new key is confirmed active, and must not revoke the old key before the
   projection commits. The pressure test: `PROJECT_OUTPUT` must be orderable
   *after* a mutation in the same plan, and revocation must be a *later* plan
   than the projection — the protocol already supports ordered actions, so this
   is expressible, but it demands a two-plan rotation, not a single atomic one.

4. **Replacement vs. rotation.** Revoking and re-creating a key is a `REPLACE`
   (new remote id, new DSN); rotating in place is an `UPDATE`. Sentry exposes
   both, so a mutating provider must classify drift into replace vs. update the
   way the Stripe provider classifies an api-version change as replace. The
   observe-only provider never has to make this call; a mutating one does, and
   the state model must retain the prior key until the replacement's projection
   commits (mirroring the default-retain deletion policy).

## Conclusion

The observe-only provider hides four assumptions: dependent-resource side
effects, conditional credential scope, post-mutation projection ordering, and
replace-vs-update classification. Three are already expressible in
`codefly.provider/v0` (ordered actions, retain-by-default, replace semantics);
the first (a single `CREATE` with a dependent auto-created team) is **not**
cleanly expressible and is the strongest argument for keeping project creation
out of v0.1. That is why v0.1's non-goals include project/team/organization
creation and client-key creation/rotation automation.
