package main

import (
	"log"
	"time"
)

// startNightlyEnrichmentJob launches a goroutine that runs the nightly AI
// enrichment job every day at 02:00 CST (08:00 UTC).
// Call once from main() after DB is initialized.
func startNightlyEnrichmentJob() {
	go func() {
		for {
			next := nextRunTime()
			log.Printf("🌙 NightlyEnrichment: próxima ejecución en %s (%s)", next.Format("2006-01-02 15:04 MST"), time.Until(next).Round(time.Minute))
			time.Sleep(time.Until(next))
			runNightlyEnrichment()
		}
	}()
}

// nextRunTime returns the next 02:00 CST (UTC-6) wall-clock time.
func nextRunTime() time.Time {
	cst := time.FixedZone("CST", -6*3600)
	now := time.Now().In(cst)
	next := time.Date(now.Year(), now.Month(), now.Day(), 2, 0, 0, 0, cst)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

// runNightlyEnrichment is the actual nightly job.
// For each active taxonomy config it finds matching recent products without
// an approved enrichment and enqueues them for approver review.
func runNightlyEnrichment() {
	if db == nil {
		log.Printf("⚠️ NightlyEnrichment: DB no disponible — saltando")
		return
	}

	log.Printf("🌙 NightlyEnrichment: iniciando ejecución %s", time.Now().UTC().Format(time.RFC3339))

	configs, err := fetchActiveTaxonomyConfigs()
	if err != nil {
		log.Printf("⚠️ NightlyEnrichment: error obteniendo configs: %v", err)
		return
	}
	if len(configs) == 0 {
		log.Printf("🌙 NightlyEnrichment: sin taxonomías configuradas — nada que procesar")
		return
	}

	totalQueued := 0
	for _, cfg := range configs {
		products, err := fetchProductsForNightlyEnrichment(cfg)
		if err != nil {
			log.Printf("⚠️ NightlyEnrichment: error obteniendo productos para config %s: %v", cfg.Id, err)
			continue
		}
		if len(products) == 0 {
			log.Printf("🌙 NightlyEnrichment: config %s (%s) — sin productos nuevos", cfg.Id, cfg.DepartmentName)
			continue
		}

		log.Printf("🌙 NightlyEnrichment: config %s (%s) — %d productos a enriquecer", cfg.Id, cfg.DepartmentName, len(products))

		for _, stub := range products {
			// Fetch full product data for AI enrichment
			product, err := findProductByID(stub.ProductId)
			if err != nil || product == nil {
				log.Printf("⚠️ NightlyEnrichment: producto %s no encontrado: %v", stub.ProductId, err)
				continue
			}

			// Run AI enrichment
			enhanced, err := enhanceWithClaudeAndSearch(*product)
			if err != nil {
				log.Printf("⚠️ NightlyEnrichment: AI falló para %s (%s): %v", product.Sku, product.ProductId, err)
				continue
			}

			// Build the response and save to queue
			response := formatForClient(product.ProductId, *product, *enhanced, 0, nil)
			queueId, err := saveToEnrichmentQueue(product.ProductId, product, response, "system-nightly")
			if err != nil {
				log.Printf("⚠️ NightlyEnrichment: queue insert falló para %s: %v", product.Sku, err)
				continue
			}

			totalQueued++
			log.Printf("✅ NightlyEnrichment: encolado %s (%s) → queueId=%s", product.Sku, product.ProductId, queueId)

			// Small pause between products to avoid hammering Claude API
			time.Sleep(3 * time.Second)
		}
	}

	log.Printf("🌙 NightlyEnrichment: finalizado — %d productos encolados para aprobación", totalQueued)
}
