# Market radar (Argentina, 2026)

`GET /demo/market?country=ar` returns a static, researched snapshot of the competitive landscape around an online catalog. The web UI draws it as a network: your catalog in the center, each competitor or channel around it, line weight by threat level.

## What it is, and what it is not

- **It is reference data**, assembled in November 2026 from public reports, with the source, the period each number describes and a quality rating (`high`, `medium`, `low`).
- **It is not live.** Nothing is scraped at run time, and it contains **no prices**.
- **It is not one market share.** The public numbers measure different things and disagree, so they are shown side by side, never merged.

## Sources used

| Figure | Source | Quality | Caveat |
|---|---|---|---|
| Online sales 2025: ARS 34.03 trillion (+55%), 645 M units, 25.1 M buyers | [CACE annual study, via Mercado](https://mercado.com.ar/tendencias/comercio-electronico-el-estudio-anual-de-cace-midio-34-033-238-millones-en-2025) | medium | Survey-based. The February preliminary figure was ARS 35.33 trillion (+60%): [source](https://mercado.com.ar/tendencias/cace-el-ecommerce-facturo-35-326-983-millones-en-2025-y-subio-60) |
| Share among international platforms (Mercado Libre 33%, AliExpress 15%, Amazon 13%, Temu 10%, Shein 8%) | [Forbes Argentina, citing CACE / Kantar](https://www.forbesargentina.com/negocios/el-ecommerce-local-crece-79-contexto-mayor-competencia-internacional-temu-shein-n77047) | medium | 2025; measures a survey of platforms, not revenue |
| Purchase mentions (Mercado Libre 41%, Temu 13%, Shein 12%) | [El Cronista, DatosClaros survey](https://www.cronista.com/economia-politica/ni-temu-ni-shein-cual-es-el-canal-que-mas-eligen-los-argentinos-para-comprar-online-y-comparar-precios/) | medium | Survey of people, not sales |
| Traffic ranking (Mercado Libre #1, Temu #2, Frávega #3, Amazon #4) | [Similarweb](https://www.similarweb.com/top-websites/argentina/e-commerce-and-shopping/marketplace/) | medium | Visits, not purchases |
| Marketplace share (Mercado Libre 75.9%, Amazon 13.7%) | [WMTips](https://www.wmtips.com/technologies/marketplaces/country/ar/) | **low** | Third-party estimate, method unclear |
| Tiendanube ~18% of sellers | [DevelopArgentina](https://developargentina.com/blog/ecommerce-argentina-estadisticas-mercado-2026) | **low** | Counts sellers, not sales; a blog |
| Top online stores (Frávega, Musimundo, Carrefour) | [Ecommerce DB via Statista](https://www.statista.com/statistics/1316655/argentina-top-online-stores-argentina-ecommercedb) | **low** | 2023 ranking, partly behind a paywall |
| Platform positioning of Tiendanube | [ResearchAndMarkets, Jan 2026](https://www.businesswire.com/news/home/20260129610825/en/Argentina-B2C-Ecommerce-Business-Report-2025-2029-Platform-Ecosystems-Payments-and-Logistics-Drive-Competitive-Intensification---ResearchAndMarkets.com) | medium | Press release of a paid report |

Not found: an official 2026 sales-share ranking for Temu and Shein, and 2026 traffic figures for Naldo and Cetrogar. They are left out rather than guessed.

## Keeping it honest

`market_test.go` fails the build if any figure lacks an `https` source, a period or a quality rating, or if a player rated a *high* threat rests only on low-quality data. When you replace a number, replace its source and period too.

## Making it live

The snapshot is the default. A connector can replace it with your own feed (your price-monitoring tool, a marketplace API you are allowed to use): serve the same JSON shape from your own service and point the UI at it. Respect each site's terms of use; this repository does not scrape competitors.
