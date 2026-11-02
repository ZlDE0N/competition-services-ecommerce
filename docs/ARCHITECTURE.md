# Architecture

## The pipeline

`POST /products/:id/enhance` runs these steps. The nightly job runs the same code.

1. **Validate and look up the cache.** A result is cached for one hour (`?refresh=true` skips it). Web-search results keep their own cache, which saves several seconds on a repeat.
2. **Load the product** from the catalog by id, SKU, barcode, manufacturer part number or family.
3. **Merge identical requests** (`singleflight`). If a pre-warm and a click ask for the same product at once, they share one run.
4. **Collect evidence**, in this order of trust: technical datasheets (PDF/DOCX read by the model) > catalog data > web search > the name alone. Barcode lookups add part numbers that sharpen the search.
5. **Write**, in parallel where possible: an improved name and visual tags; then commercial content (highlight bullets, short and long description, specs, tags, SEO title and description), shaped by the prompt of the product's taxonomy.
6. **Check:** keep the brand only if the original name had it; enforce Spanish capitalization and unit rules; guarantee a description even for products with almost no data; sanitize the HTML.
7. **Return a proposal.** It is a *preview*. It reaches the approval queue only when someone applies it (`POST /enrichment-enqueue`), so every click on "generate" does not create a queue item.

## The approval queue

One **pending** item per product: enqueueing again updates it. An approver can edit it (`PATCH`), approve it (the proposal's name, short and long description are written to the product, in one transaction) or reject it with a reason. Rejecting does not re-enqueue.

## Nightly enrichment

`cron.go` wakes at 02:00 UTC-6. For every active row of `tblPIMEnrichmentTaxonomyConfig` it picks up to `Threshold` products that are active, were created within the last `DaysBack` days and have no pending or approved item (newest first), enriches them and queues them, then notifies the approvers (in-app and, if configured, e-mail).

## Images

`image_search.go` builds search terms (model-generated, with deterministic fallbacks), queries Brave Image Search and a barcode database, and scores each candidate: relevance from the title, then a visual analysis (`image_analysis.go`) for transparency and quality. Candidates carry a quality label. `image_processor.go` prepares the chosen images with metadata for the client; `azure_storage.go` stores them in Blob Storage.

## Moving parts

| Component | Role | If it is down |
|---|---|---|
| SQL Server | Catalog, queue, taxonomy config, notifications, roles | Product features are disabled; `/health` says so |
| Redis (TLS) | Result and prompt cache | The service runs without cache |
| AI model API | Copy, datasheets, image judgment | Fallback content where one exists |
| Brave Search | Web and image search | Evidence comes from the catalog only |
| Blob Storage | Chosen images | Image upload is disabled |

## Concurrency notes

- The prompt store and the image-type mapping are guarded by mutexes.
- Outbound HTTP always goes through `newSafeHTTPClient` (see [SECURITY.md](../SECURITY.md)).
- The demo has its own in-memory state, capped at 60 queue items.
