# Security

## Reporting a problem

Please do not open a public issue for a vulnerability. Contact the maintainer privately through the e-mail on their GitHub profile.

## Trust model

The engine is meant to sit **behind your own system** (a CRM, a store back end, a gateway) and be called by it, not by end users' browsers.

- **Callers are authenticated by API key** (`X-API-Key`, `API_KEYS`). A key identifies a trusted application.
- **People are identified by a header** (`X-User-Email`) that the trusted application sets after authenticating its own user. The engine does not verify that header cryptographically; it checks in its database that this email has an approval role. Consequently: **whoever holds an API key can act as any approver.** Protect keys like passwords, give each integration its own key, and rotate them.
- Request **bodies never carry identity**. Reviewer names in a body are ignored.
- The web UI and `/demo/*` are public and touch no real data.

If you need per-user verification, put an authenticating gateway in front and have it set `X-User-Email` from a verified token.

## What is enforced in code

| Risk | Control |
|---|---|
| Unauthenticated access | API key on every private route; production refuses to start without keys unless `ALLOW_UNAUTHENTICATED=true` |
| SSRF through image, PDF or page URLs | `safehttp.go`: scheme and host validation, the resolved IP is checked on every connection (private, loopback, link-local, metadata and carrier-grade ranges blocked), at most 3 redirects, no proxy |
| Memory and decompression bombs | Size limits while reading (images 10 MB, documents 30 MB, proxy 5 MB), image pixel limit (40 MP) |
| Stored XSS from model output or edits | `sanitize.go`: allow-list of formatting tags without attributes; names are reduced to plain text; edited JSON is validated and every string sanitized |
| Privilege escalation | Admin routes (`/prompts/*` writes, `/image-types/configure`, approve, reject, edit, taxonomy config) require an approval role checked in the database |
| CORS abuse | Explicit allow-list (`CORS_ALLOWED_ORIGINS`); never `*` with credentials |
| Browser hardening | CSP without inline code, `nosniff`, frame denial, no referrer, restrictive permissions policy |
| SQL injection | Parameterized queries; `LIKE` wildcards escaped in the product lookup |
| Plaintext cache traffic | No unencrypted Redis fallback unless `REDIS_ALLOW_INSECURE=true` |
| Abuse of the public demo | Capped queue, small request bodies, `no-store`, disabled in production by default |
| Known CVEs | `govulncheck` in CI; 0 reachable findings at release |

## Known limitations

- The `X-User-Email` model above.
- `GET /cache/stats` and `/health` reveal operational information; they sit behind the API key (except `/health`).
- Rate limiting is not built in; apply it at your gateway.
- Prompts are sent to a third-party model: do not put secrets or personal data in them.

## Secrets

Never commit `.env`. Configure keys through environment variables or a secrets manager. Keys that were ever pasted into a chat, ticket or commit should be considered exposed and rotated.
