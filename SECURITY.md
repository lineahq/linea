# Security policy

## Reporting a vulnerability

Report vulnerabilities privately through GitHub:
[open a private security advisory](https://github.com/lineahq/linea/security/advisories/new)
on this repository. Do not open a public issue, pull request or discussion for
a suspected vulnerability.

Include what you can of:

- the affected part (debugger, fleet server, a fleet plugin, the Collector
  components) and version or commit;
- the configuration or request that triggers it, and its impact;
- steps to reproduce, or a proof of concept.

We keep you informed while we investigate and credit you in the advisory
unless you ask us not to. Please give us a reasonable time to release a fix
before disclosing the issue.

## Supported versions

Security fixes go into the latest release. Before 1.0, older releases are not
patched.

## Scope

Of particular interest: anything that exposes telemetry, captured data,
environment values, credentials or raw configuration outside the access a
principal was granted; a team server request that reads a host file or host
environment variable;
agent or management credentials crossing their listeners; and a fleet
reporting path that sends commands to agents.
