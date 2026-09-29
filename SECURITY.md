# Security policy

Switchboard verifies inbound webhooks and hands their payloads to AI agents, so a flaw in it can
leak one user's work to another, or let a stranger's text reach an agent that trusts it. Reports
are welcome, and we would much rather hear about a problem privately than find it in a public
issue.

## Supported versions

Security fixes land on `main` and ship in the next release. Only the **latest release** is
supported: <https://github.com/stump-wtf/switchboard/releases/latest>, also published as the
`ghcr.io/stump-wtf/switchboard` image. If you run an older release, upgrade first
([Upgrading](https://switchboard.stump.wtf/docs/guides/upgrading)) and check whether the problem
is still there.

## Reporting a vulnerability

Report it privately through GitHub's private vulnerability reporting:
<https://github.com/stump-wtf/switchboard/security/advisories/new>

Please include:

- the Switchboard version (`switchboard version`, or the image tag and digest);
- what an attacker needs first (a vended endpoint, a webhook URL, a user account on the instance,
  nothing at all);
- the smallest reproduction you have, and what it lets the attacker see or do.

**Don't open a public issue**, and don't post any of the following anywhere public, including in
the report's reproduction:

- credentials: `sbk_…` endpoint credentials, OAuth tokens, `whsec_…` signing secrets, or a
  `generic` webhook's ingest URL, which is itself a credential;
- webhook payloads or event history that contain anyone else's data;
- the private data you were able to reach. Describe what you could see, not the data itself.

If a credential of yours ended up in a report or a log, revoke the endpoint or rotate the webhook
right away. It stays live until you do.

## What's in scope

Anything that lets someone see or change what isn't theirs, forge or replay a delivery past
verification, or get past an endpoint's scope. The [security model](https://switchboard.stump.wtf/docs/guides/security-model)
describes what Switchboard is meant to protect, and lists known limitations that are already
documented and tracked. Prompt injection through a payload's *content* is outside Switchboard's
control by design (the payload is data from whoever sent it), but a way to make the doorbell or the
routing layer treat that content as trusted is in scope.
