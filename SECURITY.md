# Security Policy

Shoal is in **alpha**. Interfaces, the on-disk state format and the wire
protocol can change between minor versions, and the project has not yet had an
independent security audit. Please read the limitations in the
[README](README.md) before you run it anywhere that matters.

## Supported versions

Only the **latest minor release** receives security fixes while Shoal is in
alpha. Older alpha releases do not get backported fixes; upgrade to the latest
minor release instead.

| Version        | Supported          |
| -------------- | ------------------ |
| latest minor   | Yes                |
| older releases | No                 |

## Reporting a vulnerability

Please report suspected vulnerabilities privately. Do **not** open a public
issue, pull request or discussion for a security problem.

- **Email:** developer@shoal-technology.solutions
- **GitHub:** private vulnerability reporting on
  `Shoal-Technology/shoal` (Security tab, "Report a vulnerability"). This option is
  enabled when the repository becomes public; until then, use email.

Include, where you can:

- the Shoal version (`shoal --version`) and the commit or release archive;
- the affected component (CLI, server, agent, store, ingress, portal, ...);
- steps to reproduce, or a proof of concept;
- the impact you expect, and whether you have shared the report elsewhere.

Encrypted mail is not offered yet. If you need to send sensitive material,
say so in a first short email and we will agree on a channel.

## Response timeline

These are targets, not guarantees, while the project is small:

| Step                                   | Target                  |
| -------------------------------------- | ----------------------- |
| Acknowledge your report                | within 3 business days  |
| Confirm or reject the issue, with a severity | within 7 days     |
| Fix or mitigation for a confirmed issue | within 90 days of the report, sooner for critical issues |
| Public advisory and credit             | with the fixed release, after you have had a chance to review it |

We coordinate disclosure with you. If a fix takes longer than 90 days, we will
tell you why and agree on a new date. Reporters are credited in the advisory
unless they ask not to be.

## Scope

In scope:

- code in this repository: the `shoal` CLI, the server, the agent, the store,
  the ingress and TLS handling, the web portal, and the install script
  `hack/install.sh`;
- the release artifacts and their checksums and signatures as produced by the
  release workflow.

Out of scope:

- vulnerabilities in third-party dependencies that do not affect how Shoal
  uses them (report those upstream; we will still update to a fixed version
  when one exists);
- findings that need an attacker who already has root on a node or the
  operator's credentials, unless Shoal makes that exposure worse than the
  documented model;
- issues in example applications under `examples/`, denial of service through
  unrealistic resource use on a single-node test setup, and missing hardening
  that the documentation already lists as a limitation.

Automated scanner output without a working proof of concept is welcome as a
hint but is not treated as a vulnerability report by itself.
