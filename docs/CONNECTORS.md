# Connecting a CRM or e-commerce platform

The engine never writes to your CRM, and your CRM never reads the engine's database. They meet through a **connector module**: a small program you own and run on a schedule or on demand. This keeps the engine generic and your credentials on your side.

```
Your CRM / store  <──>  connector module  <──REST──>  enrichment engine
```

## Authentication

Every private route needs an API key in `X-API-Key`. Routes that decide something (approve, reject, edit, change prompts) also need `X-User-Email`: the person on whose behalf your connector acts. The engine checks in its database that this person has an approval role (`Super Admin` or `Products Admin`). Identity in a request **body** is ignored.

> Keep the API key on the server side of your connector. Anyone who has it can act as any approver.

## Pattern 1 — Pull (on demand)

Your system asks for a proposal when a user opens a product.

```bash
curl -X POST "$ENGINE_URL/products/$PRODUCT_ID/enhance" -H "X-API-Key: $ENGINE_API_KEY"
```

Returns the proposal immediately (`originalData`, `enhancedData`, `confidence`, `ruleApplied`). Nothing is stored until it is enqueued.

## Pattern 2 — Poll and sync (recommended)

A scheduled connector copies **approved** items into your system.

```js
// connector.mjs (Node 18+). Run it every few minutes with cron.
const base = process.env.ENGINE_URL;
const headers = { "X-API-Key": process.env.ENGINE_API_KEY };
const seen = await loadProcessedIds();           // your storage

for (let page = 1; ; page++) {
  const res = await fetch(`${base}/enrichment-queue?status=approved&pageSize=100&page=${page}`, { headers });
  if (!res.ok) throw new Error(`engine answered ${res.status}`);
  const { items } = await res.json();
  if (items.length === 0) break;
  for (const item of items) {
    if (seen.has(item.id)) continue;
    const data = (item.editedData ?? item.enrichedData).enhancedData;  // reviewer edits win
    await updateProductInYourSystem(item.productId, {
      name: data.name,
      shortDescription: data.shortDescription,   // HTML: <ul><li>…</li></ul>
      description: data.fullDescription,         // HTML: <p>…</p><ul>…</ul>
      tags: data.tags,
      seoTitle: data.tituloSEO,
      seoDescription: data.descripcionSEO,
    });
    await markProcessed(item.id);
  }
}
```

Notes:

- The list has no "changed since" filter, so the connector remembers the ids it has handled. `reviewedAt` is available for your own audit.
- Descriptions are HTML restricted to `p`, `ul`, `ol`, `li`, `strong`, `b`, `em`, `i`, `br`, `h3`, `h4`, with no attributes. Your system can render them as they are.
- The JSON field names `tituloSEO` and `descripcionSEO` are Spanish for historical reasons; they are the SEO title and description.
- `productId` is the catalog's id. Keep a mapping to your own ids, or use the `sku`.

## Pattern 3 — Batch

Loop a list of SKUs through the pull endpoint, enqueue each proposal with `POST /enrichment-enqueue` (body `{"productId": "<id>"}`, header `X-User-Email`), and let approvers work the queue.

## Pattern 4 — Let the engine pick the products

Instead of calling it per product, register taxonomies with `POST /enrichment-taxonomy-config`. Each night the engine enriches recent matching products on its own and queues them. Your connector only needs Pattern 2.

## Testing a connector without the engine's dependencies

The demo API mirrors the real shapes and needs no database or keys:

```bash
go run .                              # starts the demo
curl -X POST localhost:8080/demo/enrich -H 'Content-Type: application/json' -d '{"productId":"demo-001"}'
curl 'localhost:8080/demo/queue?status=pending'
curl -X POST localhost:8080/demo/queue/q-001/approve
curl 'localhost:8080/demo/queue?status=approved'
```

Point `ENGINE_URL` at `…/demo` equivalents while you develop, then switch to the real routes. Demo routes are unauthenticated and capped, and are off in production unless `DEMO_MODE=true`.

## Errors

| Status | Meaning |
|---|---|
| 401 | Missing or wrong `X-API-Key` |
| 403 | `X-User-Email` has no approval role |
| 404 | Product or queue item not found |
| 409 | The item was already processed |
| 413 | A prompt or body was too large |
| 503 | The database is unavailable |
