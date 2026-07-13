# Security Policy

## Reporting a Vulnerability

If you believe you have found a security vulnerability in latch-relayer, please do not open a public GitHub issue.

Report it privately via GitHub's [Security Advisories](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing/privately-reporting-a-security-vulnerability) feature, or by emailing **security@3000labs.com**.

We will acknowledge receipt within 48 hours and aim to resolve confirmed vulnerabilities within 14 days.

## Scope

This relayer manages a Stellar pool account that holds user deposits. Vulnerabilities of highest concern:

- Private key exposure or exfiltration
- Unauthorised fund movement (incorrect forwarding, double-spend)
- SQL injection or authentication bypass in the HTTP API
- Denial of service that prevents deposits from being processed

## Supported Versions

Only the latest commit on `main` is actively maintained.
