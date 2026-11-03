# Competition Services · E-commerce

**A catalog enrichment engine built on the Claude API.** Give it a SKU; it gathers evidence (datasheets, catalog data, web results), has Claude write the copy for that product category, and hands a human a reviewed proposal. Once approved, an *external module* of your own copies the result into your CRM, e-commerce platform or ERP.

![Hero](docs/img/hero.png)

> **Try it in ten seconds, no accounts and no keys:**
> ```bash
> go run .
> # open http://localhost:8080
> ```
> The built-in demo runs on hand-written sample data. No database, Claude call or search service is contacted.

## Built on Claude

Claude does the writing and the reading that a catalog team does by hand. Everything around it (evidence ranking, validation, sanitizing, approval) exists so that model output is **proposed, checked and then approved by a person**, never published directly.

| Task | Model (default) | How it is used |
|---|---|---|
| Product name, short and long description, specs, tags, SEO text | `claude-haiku-4-5-20251001` | A static system prompt per taxonomy marked with `cache_control` (prompt caching) plus a small per-product message. |
| Search-term generation (web and image search) | `claude-haiku-4-5-20251001` | Short JSON-shaped completions with deterministic fallbacks if the call fails. |
| Reading scanned or image-only PDF datasheets | `claude-sonnet-4-20250514` (`PDF_READER_MODEL` overrides it) | The PDF is sent as a base64 `document` block. Text-based PDFs and DOCX files are read locally first, so the model is only the fallback. |
| Warranty text standardization | `claude-haiku-4-5-20251001` | The user text is delimited in `<warranty_text>` and declared to be data, not instructions. |

Model ids are fixed in the code. Check them against the current model list and set the ones you want before relying on them.

**What Claude is not used for.** Images are not sent to the model: relevance is scored from titles and the transparency and quality analysis is done on pixels, locally.

### Keeping cost and latency in check

- **Prompt caching** on the two heaviest steps (description plus keywords, commercial content): the static instructions are cached, so repeat calls skip most of the input cost and time.
- **One-hour result cache** and **request merging**: a pre-warm and a click for the same product share one run.
- **Smallest model that works** for text; the larger model only for documents.
- **PDF limits:** size cap (`PDF_MAX_MB`), documents per product (`PDF_MAX_DOCS_PER_PRODUCT`), output tokens (`PDF_MAX_TOKENS`), and a 30-day cache of extracted text so the same datasheet is never re-sent.
- **One Claude client** (`claude_client.go`): reuses connections, retries only network errors, 429, 5xx and 529 with backoff, honours `Retry-After`, and always keeps the API's own error message.

### Treating model output as untrusted

- Generated HTML passes an allow-list sanitizer (simple formatting tags, no attributes); names are reduced to plain text.
- Reviewer edits are validated as JSON and every string is sanitized before it is stored.
- Nothing reaches the catalog until an approver accepts it, and approval needs a role checked in the database.
- Web pages, PDFs and images the model is given are fetched through an SSRF-safe client with size limits.

### Using it

```bash
export ANTHROPIC_API_KEY=...        # from the Claude Console
export BRAVE_SEARCH_API_KEY=...     # web and image search
```

Without a key the engine still runs: the UI and the demo need none, and the real pipeline answers with a clear error instead of failing silently.

## What it does

| | |
|---|---|
| **Product copy** | Name, short and long description, specs, tags, SEO title and description. Spanish-first style rules; the prompts are yours to change. |
| **Datasheet reading** | Pulls specifications out of PDF and DOCX files, with limits on size, pages and cost per product. |
| **Source ranking** | Datasheet beats catalog data, which beats web results. The brand is kept only if the original name had it. |
| **Image discovery** | Finds candidates (Brave Search, barcode lookup), scores relevance and transparency, labels quality, prepares and stores the chosen ones. |
| **Taxonomy-aware prompts** | One general prompt plus overrides by department, category or subcategory: tone, format, attributes that must always appear. |
| **Warranty text** | Standardizes free-form warranty descriptions into one consistent format. |
| **Nightly enrichment** | Choose the taxonomies to cover; each night (02:00 UTC-6) new matching products are enriched and queued. |
| **Approval queue** | Pending / approved / rejected, reviewer edits, one pending item per product, who decided what, e-mail and in-app notifications. |
| **Market radar** | A network map of the competitors and channels around a catalog, with a sourced snapshot for Argentina 2026 (Mercado Libre, Temu, Amazon, Frávega…). Every figure carries its source, period and a trust rating. See [docs/MARKET.md](docs/MARKET.md). |
| **Cache and dedupe** | Results are cached for one hour; identical simultaneous requests share a single run. |

![Live demo](docs/img/demo.png)

## How it connects

![Architecture](docs/img/connect.png)

The engine exposes a small REST API. A **connector module** is a thin piece of code you own: it asks for enrichment, waits for approval and copies the result into your system. See **[docs/CONNECTORS.md](docs/CONNECTORS.md)** for the three patterns (pull, poll and sync, batch) with working examples.

```bash
# ask for a proposal
curl -X POST "$ENGINE_URL/products/$PRODUCT_ID/enhance" -H "X-API-Key: $ENGINE_API_KEY"

# read what was approved
curl "$ENGINE_URL/enrichment-queue?status=approved&pageSize=50" -H "X-API-Key: $ENGINE_API_KEY"
```

## Running the real engine

The demo needs nothing. The full engine needs the services below; copy `.env.example` to `.env` and fill it in.

| Needs | For |
|---|---|
| A Claude API key (`ANTHROPIC_API_KEY`) | Writing the copy, search terms and warranty text; reading scanned datasheets. |
| `BRAVE_SEARCH_API_KEY` | Web and image search. |
| SQL Server with the `catalog` schema | The product catalog, the queue and the notification tables. See [docs/DATA-MODEL.md](docs/DATA-MODEL.md). |
| Redis (TLS) | Result cache and prompt store. |
| Azure Blob Storage | Storing the chosen images. |
| SendGrid (optional) | E-mail to approvers. |

```bash
for f in migrations/*.sql; do sqlcmd -S "$DB_SERVER" -d "$DB_DATABASE" -U "$DB_USER" -P "$DB_PASSWORD" -i "$f"; done
ENVIRONMENT=production API_KEYS=a-long-random-key go run .
```

Or with Docker: `docker build -t catalog-enrichment . && docker run --env-file .env -p 8080:8080 catalog-enrichment`.

## API

All routes except `/`, `/health`, `/assets/*` and `/demo/*` need `X-API-Key`. Routes that change configuration or content also check, in the database, that the person named in `X-User-Email` has an approval role.

| Method and path | Purpose |
|---|---|
| `POST /products/:id/enhance` | Generate an enrichment proposal (`?refresh=true` skips the cache). |
| `GET /products/:id/images/search` | Find and score candidate images. |
| `POST /products/:id/images/process` | Prepare the selected images. |
| `POST /warranty/standardize-description` | Standardize a warranty text. |
| `POST /enrichment-enqueue` | Put a proposal in the approval queue. |
| `GET /enrichment-queue` | List items (`status=pending\|approved\|rejected\|all`, `page`, `pageSize`). |
| `GET /enrichment-queue/:id` | One item. |
| `POST /enrichment-queue/:id/approve` · `…/reject` | Decide (approval role required). |
| `PATCH /enrichment-queue/:id` | Edit a pending item (sanitized, approval role). |
| `GET/POST/DELETE /enrichment-taxonomy-config` | Taxonomies covered by the nightly job. |
| `GET /prompts/config` · `PUT/POST/DELETE /prompts/...` | Read and manage prompts (writes need an approval role). |
| `GET /prompts/preview/:id` · `GET /prompts/match` · `GET /taxonomy/options` | Preview the prompt a product would use. |
| `GET /media-resources` · `GET /image-types/mappings` · `POST /image-types/configure` | Map image types to media resources. |
| `GET /notifications` · `PATCH /notifications/:id/read` | In-app notifications. |
| `GET /proxy-image?url=` | Fetch an external image for the UI (SSRF-safe, 5 MB limit). |
| `GET /health` · `GET /cache/stats` | Health and cache statistics. |

A proposal looks like this:

```json
{
  "productId": "…",
  "originalData": { "name": "TALADRO VOLTIX 18V", "shortDescription": "taladro inalambrico con bateria", "tags": ["taladro"] },
  "enhancedData": { "name": "Taladro Percutor Inalámbrico Voltix 18 V …", "shortDescription": "<ul><li>…</li></ul>",
                    "fullDescription": "<p>…</p>", "tags": ["…"], "tituloSEO": "…", "descripcionSEO": "…" },
  "confidence": 0.94,
  "ruleApplied": { "…": "…" }
}
```

## Safe by default

- **API key on every private route.** In production the service refuses to start without `API_KEYS`.
- **Outbound fetches are fenced.** Images, PDFs and pages are fetched by a client that checks the real destination IP on every connection and redirect, so it cannot be aimed at your internal network. Downloads and decoded images have size limits.
- **Generated HTML is cleaned.** Model output and reviewer edits keep only simple formatting tags.
- **Roles decide, not request bodies.** Reviewer identity comes from a trusted header and is checked against the database.
- **Explicit CORS allow-list**, strict CSP on the UI (no CDN, no inline code), size caps on prompts and requests.
- `govulncheck` reports **0 reachable vulnerabilities**; CI runs it on every change.

Details and the trust model: **[SECURITY.md](SECURITY.md)**.

## Project layout

```
main.go                 router, product lookup, enrichment pipeline
search_terms_ai.go      search-term generation (AI and fallbacks)
image_search.go         image discovery, relevance scoring
image_analysis.go       transparency and quality analysis
image_processor.go      image preparation for the client
pdf_reader.go, utils.go datasheet reading (PDF, DOCX)
prompts.go              general and per-taxonomy prompts
queue.go                approval queue
taxonomy_config.go      taxonomies for the nightly job
cron.go                 nightly job
notifications.go        e-mail and in-app notifications
security.go, safehttp.go, sanitize.go   authentication, SSRF-safe client, HTML sanitizer
demo.go, ui.go, web/    demo API and the embedded web UI
migrations/             SQL for the queue, taxonomy config and schema
```

## Status

Early. The engine is production-shaped but its data layer is written against a specific SQL Server catalog schema (see [docs/DATA-MODEL.md](docs/DATA-MODEL.md)); a storage adapter so it can read any catalog is the next step. Copy rules are Spanish-first. There is no license file yet: until one is added, all rights are reserved.

```bash
go test -race ./...    # unit tests for security, sanitizer, demo and UI
```

---

*Independent project. Not affiliated with, endorsed by or sponsored by Anthropic. It uses the Claude API for text generation and document reading; Claude is a trademark of Anthropic, PBC. Sample data is fictional.*
