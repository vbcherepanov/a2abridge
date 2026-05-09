# Security Policy

## Supported versions

Only the latest minor of the current major receives security fixes.

| Version | Supported          |
| ------- | ------------------ |
| 2.x.x   | :white_check_mark: |
| 1.x.x   | :warning: critical fixes only until 2026-12-31 |
| 0.x.x   | :x:                |

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security findings.

Use one of:

1. **Private GitHub advisory** (preferred):
   <https://github.com/vbcherepanov/a2abridge/security/advisories/new>
2. Email **`vbcherepanov+security@gmail.com`** with subject prefix
   `[a2abridge security]`. PGP welcome but not required.

You should receive an acknowledgement within **48 hours**. We aim to
ship a patched release within **7 days** for high-severity issues, with
a CVE assigned where applicable.

## Scope

In scope:
- The `a2abridge` binary and every package under `internal/`.
- The `install.sh` / `install.ps1` installers.
- The embedded skill + UserPromptSubmit hook script.

Out of scope:
- Vulnerabilities in upstream dependencies (please report them upstream
  first; we'll bump after).
- Anything dependent on the user disabling default loopback bind, mTLS,
  or the PII screen.

## What counts as a vulnerability

Examples we treat as security-class:

- Path traversal or arbitrary write via `a2abridge install` or
  `uninstall`.
- Privilege escalation via the kardianos/service install path.
- Plaintext disclosure of secrets that the PII screen should have
  caught (false negative).
- mTLS bypass when `A2A_PEER_ALLOW` is set.
- Inbox / hook injection that can execute attacker-controlled code on
  the user's machine.

A secret in a peer's outbound message that *we* failed to redact (PII
screen miss) is a high-severity finding and a regression test will be
added to `pii_test.go`.
