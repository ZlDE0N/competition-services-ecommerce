# Changelog

## Unreleased

### Added
- Market radar: interactive competition network and a sourced Argentina 2026 snapshot (`GET /demo/market`), with tests that require a source, period and quality rating for every figure. See `docs/MARKET.md`.

## 0.1.0 — 2026-11-02

First release of this repository.

### Added
- Web UI: animated explainer and an interactive demo (sample catalog, pipeline
  timeline, before/after view, approval queue, connector payload).
- Self-contained demo API (`/demo/*`): no database, keys or model calls; same
  response shapes as the real endpoints.
- `docs/`: architecture, connector guide and data model.

### Security
- API-key authentication (`API_KEYS`); production refuses to start without it.
- CORS allow-list instead of `*` with credentials.
- SSRF-safe HTTP client for every download whose URL comes from outside, with
  size limits and an image pixel limit.
- Allow-list HTML sanitizer for model output and reviewer edits.
- Reviewer identity taken from a trusted header only; admin routes require an
  approval role; prompt and request size limits.
- LIKE wildcards escaped; no plaintext Redis fallback unless opted in.

### Changed
- Engine imported with company-specific names, domains and ticket ids removed;
  schema renamed to `catalog`; module renamed.
