package main

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Market radar: a map of the competitive landscape around a catalog, shipped
// with a researched snapshot for Argentina (2026).
//
// Every figure carries its source, the period it describes and a quality note,
// because the public numbers disagree with each other (surveys vs. traffic vs.
// self-reported sales). Nothing here is scraped at run time and nothing is a
// price: the snapshot is static reference data that a connector can replace
// with a live feed (see docs/CONNECTORS.md).

type marketMetric struct {
	Label   string `json:"label"`
	Value   string `json:"value"`
	Source  string `json:"source"`  // https URL of the publication
	Period  string `json:"period"`  // what the number describes
	Quality string `json:"quality"` // high | medium | low
	Note    string `json:"note,omitempty"`
}

type marketPlayer struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`   // marketplace | cross-border | retailer | platform
	Threat   string         `json:"threat"` // high | medium | low, relative to a catalog seller
	Summary  string         `json:"summary"`
	Playbook string         `json:"playbook"` // what to do about it
	Metrics  []marketMetric `json:"metrics"`
}

type marketSnapshot struct {
	Country string         `json:"country"`
	AsOf    string         `json:"asOf"`
	Caveat  string         `json:"caveat"`
	Totals  []marketMetric `json:"totals"`
	Players []marketPlayer `json:"players"`
}

const (
	srcCACE    = "https://mercado.com.ar/tendencias/comercio-electronico-el-estudio-anual-de-cace-midio-34-033-238-millones-en-2025"
	srcCACEPre = "https://mercado.com.ar/tendencias/cace-el-ecommerce-facturo-35-326-983-millones-en-2025-y-subio-60"
	srcSimilar = "https://www.similarweb.com/top-websites/argentina/e-commerce-and-shopping/marketplace/"
	srcForbes  = "https://www.forbesargentina.com/negocios/el-ecommerce-local-crece-79-contexto-mayor-competencia-internacional-temu-shein-n77047"
	srcCronist = "https://www.cronista.com/economia-politica/ni-temu-ni-shein-cual-es-el-canal-que-mas-eligen-los-argentinos-para-comprar-online-y-comparar-precios/"
	srcWMTips  = "https://www.wmtips.com/technologies/marketplaces/country/ar/"
	srcDevelop = "https://developargentina.com/blog/ecommerce-argentina-estadisticas-mercado-2026"
	srcRAM     = "https://www.businesswire.com/news/home/20260129610825/en/Argentina-B2C-Ecommerce-Business-Report-2025-2029-Platform-Ecosystems-Payments-and-Logistics-Drive-Competitive-Intensification---ResearchAndMarkets.com"
	srcECDB    = "https://www.statista.com/statistics/1316655/argentina-top-online-stores-argentina-ecommercedb"
)

// marketArgentina2026 is the researched snapshot. Keep it honest: when a number
// is replaced, replace its source and period too (the test enforces both).
var marketArgentina2026 = marketSnapshot{
	Country: "Argentina",
	AsOf:    "2026-11",
	Caveat: "Public figures come from different methods (surveys, web traffic, seller counts) and are not directly comparable. " +
		"Treat them as orders of magnitude and verify before quoting.",
	Totals: []marketMetric{
		{Label: "Online sales 2025", Value: "ARS 34.03 trillion (+55% nominal)", Source: srcCACE, Period: "2025", Quality: "medium",
			Note: "CACE annual study; the February preliminary figure was ARS 35.33 trillion (+60%): " + srcCACEPre},
		{Label: "Units sold 2025", Value: "645 million (+28%)", Source: srcCACE, Period: "2025", Quality: "medium"},
		{Label: "Online buyers 2025", Value: "25.1 million", Source: srcCACE, Period: "2025", Quality: "medium",
			Note: "Survey-based, not transaction-based"},
	},
	Players: []marketPlayer{
		{ID: "mercadolibre", Name: "Mercado Libre", Kind: "marketplace", Threat: "high",
			Summary:  "The default destination for buying and comparing prices online, with its own payments and logistics.",
			Playbook: "Do not fight it on reach: use it as a channel, keep your own catalog copy richer than listings there, and protect margin with bundles and service.",
			Metrics: []marketMetric{
				{Label: "Share among international platforms", Value: "33%", Source: srcForbes, Period: "2025 (CACE / Kantar)", Quality: "medium"},
				{Label: "Share of purchase mentions", Value: "41%", Source: srcCronist, Period: "2026 survey (DatosClaros)", Quality: "medium"},
				{Label: "Marketplace share", Value: "75.9%", Source: srcWMTips, Period: "2026", Quality: "low", Note: "Third-party estimate; method unclear"},
				{Label: "Web traffic rank (marketplaces)", Value: "#1", Source: srcSimilar, Period: "2026", Quality: "medium"},
			}},
		{ID: "amazon", Name: "Amazon", Kind: "marketplace", Threat: "medium",
			Summary:  "Growing cross-border and local presence; competes on selection and delivery promises.",
			Playbook: "Differentiate on local service, warranty and installation that a cross-border listing cannot match.",
			Metrics: []marketMetric{
				{Label: "Share among international platforms", Value: "13%", Source: srcForbes, Period: "2025 (CACE / Kantar)", Quality: "medium"},
				{Label: "Marketplace share", Value: "13.7%", Source: srcWMTips, Period: "2026", Quality: "low"},
				{Label: "Web traffic rank (marketplaces)", Value: "#4", Source: srcSimilar, Period: "2026", Quality: "medium"},
			}},
		{ID: "temu", Name: "Temu", Kind: "cross-border", Threat: "high",
			Summary:  "Low-price cross-border platform; strongest in fashion, home and small accessories.",
			Playbook: "Avoid pure price wars in commodity items; compete with availability, returns, warranty and verified specifications.",
			Metrics: []marketMetric{
				{Label: "Share among international platforms", Value: "10%", Source: srcForbes, Period: "2025 (CACE / Kantar)", Quality: "medium"},
				{Label: "Most used by people who buy abroad", Value: "41%", Source: srcForbes, Period: "2025 (CACE)", Quality: "medium"},
				{Label: "Share of purchase mentions", Value: "13%", Source: srcCronist, Period: "2026 survey (DatosClaros)", Quality: "medium"},
				{Label: "Web traffic rank (marketplaces)", Value: "#2", Source: srcSimilar, Period: "2026", Quality: "medium"},
			}},
		{ID: "shein", Name: "Shein", Kind: "cross-border", Threat: "medium",
			Summary:  "Fast-fashion cross-border platform; pressure concentrated in apparel.",
			Playbook: "Relevant if you sell apparel; otherwise watch it, do not react to it.",
			Metrics: []marketMetric{
				{Label: "Share among international platforms", Value: "8%", Source: srcForbes, Period: "2025 (CACE / Kantar)", Quality: "medium"},
				{Label: "Share of purchase mentions", Value: "12%", Source: srcCronist, Period: "2026 survey (DatosClaros)", Quality: "medium"},
			}},
		{ID: "aliexpress", Name: "AliExpress / Alibaba", Kind: "cross-border", Threat: "medium",
			Summary:  "Long-standing cross-border option, strong in electronics and tools.",
			Playbook: "Match on specification clarity; shoppers compare datasheets as much as price.",
			Metrics: []marketMetric{
				{Label: "Share among international platforms", Value: "15%", Source: srcForbes, Period: "2025 (CACE / Kantar)", Quality: "medium"},
			}},
		{ID: "fravega", Name: "Frávega", Kind: "retailer", Threat: "high",
			Summary:  "Omnichannel appliances and electronics retailer with a very large online audience.",
			Playbook: "Its listings set the reference price in appliances; keep names, specs and images at least as complete as theirs.",
			Metrics: []marketMetric{
				{Label: "Web traffic rank (marketplaces)", Value: "#3", Source: srcSimilar, Period: "2026", Quality: "medium"},
				{Label: "Top online stores", Value: "Top 2 by online sales", Source: srcECDB, Period: "2023 (Ecommerce DB)", Quality: "low", Note: "Older and partly hidden behind a paywall"},
			}},
		{ID: "musimundo", Name: "Musimundo", Kind: "retailer", Threat: "medium",
			Summary:  "Electronics and appliances chain with an established online store.",
			Playbook: "Track it for electronics pricing and promotions.",
			Metrics: []marketMetric{
				{Label: "Top online stores", Value: "Top 5 by online sales", Source: srcECDB, Period: "2023 (Ecommerce DB)", Quality: "low"},
			}},
		{ID: "carrefour", Name: "Carrefour Argentina", Kind: "retailer", Threat: "medium",
			Summary:  "Large grocery and general merchandise retailer selling online.",
			Playbook: "Relevant for household and small appliances.",
			Metrics: []marketMetric{
				{Label: "Top online stores", Value: "Top 5 by online sales", Source: srcECDB, Period: "2023 (Ecommerce DB)", Quality: "low"},
			}},
		{ID: "tiendanube", Name: "Tiendanube (Nuvemshop)", Kind: "platform", Threat: "low",
			Summary:  "Not a competitor: the storefront platform many local SMEs and brands use. It is where a connector often has to write.",
			Playbook: "Build a connector for it first if your customers are SMEs.",
			Metrics: []marketMetric{
				{Label: "Share of sellers", Value: "~18%", Source: srcDevelop, Period: "2026", Quality: "low", Note: "Counts sellers, not sales; source is a blog"},
				{Label: "Positioning", Value: "Preferred platform of local SMEs and brands", Source: srcRAM, Period: "2026 (ResearchAndMarkets)", Quality: "medium"},
			}},
	},
}

var validKinds = map[string]bool{"marketplace": true, "cross-border": true, "retailer": true, "platform": true}

func marketRegister(router *gin.Engine) {
	router.GET("/demo/market", func(c *gin.Context) {
		country := strings.ToLower(strings.TrimSpace(c.DefaultQuery("country", "ar")))
		if country != "ar" {
			c.JSON(http.StatusNotFound, gin.H{"error": "only country=ar is available in this snapshot"})
			return
		}
		c.Header("Cache-Control", "public, max-age=300")
		c.JSON(http.StatusOK, marketArgentina2026)
	})
}
