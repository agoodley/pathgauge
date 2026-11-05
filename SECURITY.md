# Security policy

PathGauge runs privileged network operations and deliberately generates heavy traffic, so security reports are especially welcome.

## Reporting a vulnerability

Report it privately through GitHub: **Security → Report a vulnerability** on this repository. Please don't open a public issue. Include the version, the steps to reproduce and the impact you expect.

This is a volunteer project. We aim to acknowledge reports within 7 days and to agree a disclosure date with you.

## Especially in scope

- Generating or reflecting traffic without valid session authentication, or amplification (a reply larger than the request).
- Escaping the service sandbox, or gaining privileges beyond `CAP_NET_RAW` and `CAP_NET_ADMIN`.
- Shaping or other network changes that survive their lease, a service stop or a restart.
- Agent enrollment, certificate or control-channel weaknesses.
- Dashboard and API issues: authentication, authorisation, XSS, CSRF, injection.

## Supported versions

Before 1.0, only the latest release receives fixes.
