package main

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Demo mode: a self-contained, in-memory imitation of the enrichment API that
// the web UI uses. It needs no database, no API keys and never calls a model or
// the network: the "enriched" copy below is hand-written sample content. The
// response shapes mirror the real endpoints (originalData / enhancedData /
// confidence / ruleApplied, and the pending|approved|rejected queue) so a
// connector written against the demo works against the real service.

type demoProduct struct {
	ID, SKU, Brand, Category        string
	RawName, RawShort, RawFull      string
	RawTags                         []string
	Datasheet                       bool
	Name, Short, Full               string
	Tags, VisualTags                []string
	SEOTitle, SEODescription        string
	Rule                            string
	Confidence                      float64
	WebSources, DatasheetSourceName string
}

var demoCatalog = []demoProduct{
	{
		ID: "demo-001", SKU: "VX-DR18-KIT", Brand: "Voltix", Category: "Herramientas / Taladros",
		RawName: "TALADRO VOLTIX 18V", RawShort: "taladro inalambrico con bateria", RawFull: "",
		RawTags:   []string{"taladro"},
		Datasheet: true, WebSources: "3 web results", DatasheetSourceName: "ficha-tecnica-VX-DR18.pdf",
		Name:           "Taladro Percutor Inalámbrico Voltix 18 V con 2 Baterías y Maletín",
		Short:          "<ul><li>Motor sin escobillas de 18 V</li><li>Mandril metálico de 13 mm</li><li>2 baterías de litio de 2.0 Ah</li><li>Luz LED integrada</li></ul>",
		Full:           "<p>Taladro percutor inalámbrico pensado para obra y taller. Su motor sin escobillas entrega más torque con menos consumo, y el mandril metálico de 13 mm sujeta firme brocas de madera, metal y concreto ligero.</p><ul><li>Velocidad variable de 0 a 1 800 rpm</li><li>Embrague de 21 posiciones</li><li>Carga rápida en 60 minutos</li></ul>",
		Tags:           []string{"taladro", "percutor", "inalámbrico", "18V", "herramientas eléctricas", "baterías de litio"},
		VisualTags:     []string{"taladro negro y amarillo", "maletín incluido", "dos baterías"},
		SEOTitle:       "Taladro Percutor Inalámbrico 18 V Voltix | Kit con 2 Baterías",
		SEODescription: "Taladro percutor inalámbrico Voltix de 18 V con motor sin escobillas, mandril de 13 mm, 2 baterías de litio y maletín. Ideal para obra y taller.",
		Rule:           "Power tools · technical tone · bullets", Confidence: 0.94,
	},
	{
		ID: "demo-002", SKU: "AL-SPL12-INV", Brand: "Aerolux", Category: "Climatización / Aires acondicionados",
		RawName: "AIRE AEROLUX 12000", RawShort: "aire inverter", RawFull: "aire acondicionado",
		RawTags:   []string{"aire", "inverter"},
		Datasheet: true, WebSources: "5 web results", DatasheetSourceName: "datasheet-AL-SPL12.pdf",
		Name:           "Aire Acondicionado Split Inverter Aerolux 12 000 BTU 220 V",
		Short:          "<ul><li>Capacidad: 12 000 BTU (hasta 20 m²)</li><li>Tecnología Inverter, ahorro de energía</li><li>Gas refrigerante R32</li><li>Control remoto con modo sleep</li></ul>",
		Full:           "<p>Split inverter de 12 000 BTU para dormitorios y oficinas de hasta 20 m². El compresor inverter ajusta su velocidad para mantener la temperatura con menor consumo y menos ruido.</p><ul><li>Voltaje: 220 V / 60 Hz</li><li>Filtro antibacterial lavable</li><li>Garantía y servicio técnico incluidos</li></ul>",
		Tags:           []string{"aire acondicionado", "split", "inverter", "12000 BTU", "R32", "climatización"},
		VisualTags:     []string{"unidad interior blanca", "control remoto", "panel frontal liso"},
		SEOTitle:       "Aire Acondicionado Split Inverter 12 000 BTU Aerolux 220 V",
		SEODescription: "Aire acondicionado split inverter Aerolux de 12 000 BTU, gas R32 y bajo consumo. Para espacios de hasta 20 m².",
		Rule:           "HVAC · informative tone · bullets", Confidence: 0.91,
	},
	{
		ID: "demo-003", SKU: "NC-AF45", Brand: "NovaChef", Category: "Cocina / Freidoras de aire",
		RawName: "freidora novachef", RawShort: "freidora de aire 4.5l", RawFull: "",
		RawTags:   []string{"freidora"},
		Datasheet: false, WebSources: "4 web results",
		Name:           "Freidora de Aire NovaChef 4.5 L con Panel Digital",
		Short:          "<ul><li>Capacidad: 4.5 L (2 a 4 personas)</li><li>Panel digital con 8 programas</li><li>Cocina con hasta 80 % menos aceite</li><li>Cesta antiadherente apta para lavavajillas</li></ul>",
		Full:           "<p>Freidora de aire con circulación de aire caliente rápida para cocinar crujiente con muy poco aceite. El panel digital guarda 8 programas para papas, pollo, pescado y repostería.</p><ul><li>Potencia: 1 500 W</li><li>Temperatura ajustable de 80 a 200 °C</li><li>Apagado automático</li></ul>",
		Tags:           []string{"freidora de aire", "airfryer", "4.5 litros", "cocina", "panel digital"},
		VisualTags:     []string{"freidora negra", "pantalla digital", "cesta extraíble"},
		SEOTitle:       "Freidora de Aire 4.5 L NovaChef | Panel Digital y 8 Programas",
		SEODescription: "Freidora de aire NovaChef de 4.5 L con panel digital, 8 programas y cesta antiadherente. Cocina crujiente con menos aceite.",
		Rule:           "Kitchen · aspirational tone · bullets", Confidence: 0.88,
	},
	{
		ID: "demo-004", SKU: "PN-CAMA-M", Brand: "PetNest", Category: "Mascotas / Camas",
		RawName: "CAMA PERRO M", RawShort: "cama para perro mediana", RawFull: "",
		RawTags:   []string{},
		Datasheet: false, WebSources: "2 web results",
		Name:           "Cama Ortopédica para Perro PetNest Talla M 70 × 55 cm",
		Short:          "<ul><li>Talla M: 70 × 55 cm (perros de hasta 18 kg)</li><li>Espuma ortopédica de alta densidad</li><li>Funda removible y lavable</li><li>Base antideslizante</li></ul>",
		Full:           "<p>Cama ortopédica que reparte el peso y cuida las articulaciones de perros medianos. La funda es removible y se lava en lavadora; la base antideslizante evita que se mueva sobre pisos lisos.</p>",
		Tags:           []string{"cama para perro", "ortopédica", "talla M", "mascotas", "lavable"},
		VisualTags:     []string{"cama gris ovalada", "borde acolchado"},
		SEOTitle:       "Cama Ortopédica para Perro Talla M | PetNest 70 × 55 cm",
		SEODescription: "Cama ortopédica PetNest talla M para perros de hasta 18 kg, con funda lavable y base antideslizante.",
		Rule:           "Pets · informative tone · mixed", Confidence: 0.86,
	},
	{
		ID: "demo-005", SKU: "CR-PNT-4L-BL", Brand: "Coralis", Category: "Pinturas / Interior",
		RawName: "pintura blanca coralis", RawShort: "pintura latex 4 litros", RawFull: "",
		RawTags:   []string{"pintura", "blanca"},
		Datasheet: true, WebSources: "3 web results", DatasheetSourceName: "ficha-Coralis-latex.pdf",
		Name:           "Pintura Látex Interior Coralis Blanco Mate 4 L",
		Short:          "<ul><li>Acabado: mate lavable</li><li>Rendimiento: hasta 40 m² por galón</li><li>Secado al tacto en 1 hora</li><li>Bajo olor, base agua</li></ul>",
		Full:           "<p>Pintura látex de base agua para paredes y cielos interiores. Cubre bien en dos manos, deja un acabado mate uniforme y se puede limpiar con un paño húmedo.</p><ul><li>Repintado a las 4 horas</li><li>Dilución: hasta 10 % con agua</li><li>Presentación: 4 litros</li></ul>",
		Tags:           []string{"pintura látex", "interior", "blanco mate", "4 litros", "base agua"},
		VisualTags:     []string{"cubeta blanca", "etiqueta azul"},
		SEOTitle:       "Pintura Látex Interior Blanco Mate 4 L | Coralis",
		SEODescription: "Pintura látex Coralis blanco mate de 4 L: lavable, bajo olor y de secado rápido para paredes y cielos interiores.",
		Rule:           "Paints · technical tone · bullets", Confidence: 0.92,
	},
	{
		ID: "demo-006", SKU: "TF-MAT-YOGA", Brand: "TerraFit", Category: "Deportes / Yoga",
		RawName: "mat yoga terrafit", RawShort: "colchoneta", RawFull: "",
		RawTags:   []string{"yoga"},
		Datasheet: false, WebSources: "4 web results",
		Name:           "Colchoneta de Yoga TerraFit Antideslizante 6 mm",
		Short:          "<ul><li>Grosor: 6 mm para amortiguar articulaciones</li><li>Superficie antideslizante en ambos lados</li><li>Medidas: 183 × 61 cm</li><li>Incluye correa de transporte</li></ul>",
		Full:           "<p>Colchoneta de yoga y pilates con superficie texturizada que mantiene el agarre incluso con las manos sudadas. Ligera, fácil de enrollar y lista para llevar con su correa.</p>",
		Tags:           []string{"mat de yoga", "colchoneta", "pilates", "antideslizante", "6 mm"},
		VisualTags:     []string{"colchoneta verde enrollada", "correa negra"},
		SEOTitle:       "Colchoneta de Yoga Antideslizante 6 mm | TerraFit",
		SEODescription: "Colchoneta de yoga TerraFit de 6 mm, antideslizante por ambos lados y con correa de transporte.",
		Rule:           "Sports · aspirational tone · mixed", Confidence: 0.85,
	},
}

type demoStage struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Ms    int    `json:"ms"`
	Used  bool   `json:"used"`
	Note  string `json:"note,omitempty"`
}

type demoQueueItem struct {
	ID              string                 `json:"id"`
	ProductID       string                 `json:"productId"`
	Sku             string                 `json:"sku"`
	ProductName     string                 `json:"productName"`
	Status          string                 `json:"status"` // pending | approved | rejected
	EnrichmentModel string                 `json:"enrichmentModel"`
	CreatedAt       time.Time              `json:"createdAt"`
	ReviewedAt      *time.Time             `json:"reviewedAt,omitempty"`
	RejectionReason string                 `json:"rejectionReason,omitempty"`
	EnrichedData    map[string]interface{} `json:"enrichedData"`
}

type demoState struct {
	mu    sync.Mutex
	seq   int
	queue []*demoQueueItem
}

var demo = &demoState{}

func demoFind(id string) *demoProduct {
	for i := range demoCatalog {
		if demoCatalog[i].ID == id {
			return &demoCatalog[i]
		}
	}
	return nil
}

// completeness scores how filled in a record is (0-100).
func completeness(name, short, full string, tags, visual []string, seo bool) int {
	pts := 0
	if len(name) >= 12 {
		pts++
	}
	if len(short) >= 20 {
		pts++
	}
	if len(full) >= 220 {
		pts++
	}
	if len(tags) >= 5 {
		pts++
	}
	if len(visual) >= 2 {
		pts++
	}
	if seo {
		pts++
	}
	return pts * 100 / 6
}

func (p *demoProduct) before() int {
	return completeness(p.RawName, p.RawShort, p.RawFull, p.RawTags, nil, false)
}
func (p *demoProduct) after() int {
	return completeness(p.Name, p.Short, p.Full, p.Tags, p.VisualTags, p.SEOTitle != "")
}

func (s *demoState) statusOf(productID string) string {
	status := "raw"
	for _, q := range s.queue {
		if q.ProductID != productID {
			continue
		}
		switch q.Status {
		case "approved":
			return "approved"
		case "pending":
			status = "pending"
		}
	}
	return status
}

func (p *demoProduct) stages() []demoStage {
	return []demoStage{
		{ID: "read", Label: "Read the catalog record", Ms: 180, Used: true},
		{ID: "rank", Label: "Rank sources: datasheet > catalog > web", Ms: 240, Used: true},
		{ID: "web", Label: "Search the web for specifications", Ms: 900, Used: true, Note: p.WebSources},
		{ID: "pdf", Label: "Read the datasheet (PDF)", Ms: 1400, Used: p.Datasheet, Note: map[bool]string{true: p.DatasheetSourceName, false: "no datasheet found, skipped"}[p.Datasheet]},
		{ID: "write", Label: "Write the copy for this category", Ms: 1200, Used: true, Note: p.Rule},
		{ID: "check", Label: "Validate brand, units and consistency", Ms: 300, Used: true},
		{ID: "queue", Label: "Send to the approval queue", Ms: 150, Used: true},
	}
}

func (p *demoProduct) enriched() map[string]interface{} {
	return map[string]interface{}{
		"enhancedData": map[string]interface{}{
			"name": p.Name, "sku": p.SKU, "shortDescription": p.Short, "fullDescription": p.Full,
			"tags": p.Tags, "visualTags": p.VisualTags, "tituloSEO": p.SEOTitle, "descripcionSEO": p.SEODescription,
		},
	}
}

func demoRegister(router *gin.Engine) {
	g := router.Group("/demo")
	g.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
		c.Header("Cache-Control", "no-store")
		c.Next()
	})

	g.GET("/info", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"mode": "demo", "modelCalls": false, "database": false,
			"note": "Sample data. No model, search or storage service is contacted.",
		})
	})

	g.GET("/catalog", func(c *gin.Context) {
		demo.mu.Lock()
		defer demo.mu.Unlock()
		out := make([]gin.H, 0, len(demoCatalog))
		for i := range demoCatalog {
			p := &demoCatalog[i]
			st := demo.statusOf(p.ID)
			score := p.before()
			if st != "raw" {
				score = p.after()
			}
			out = append(out, gin.H{
				"id": p.ID, "sku": p.SKU, "brand": p.Brand, "category": p.Category,
				"name": p.RawName, "shortDescription": p.RawShort, "status": st, "completeness": score,
			})
		}
		c.JSON(http.StatusOK, gin.H{"items": out})
	})

	g.POST("/enrich", func(c *gin.Context) {
		var req struct {
			ProductID string `json:"productId"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.ProductID) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "productId is required"})
			return
		}
		p := demoFind(req.ProductID)
		if p == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "unknown demo product"})
			return
		}
		stages := p.stages()
		total := 0
		for _, s := range stages {
			if s.Used {
				total += s.Ms
			}
		}

		demo.mu.Lock()
		var item *demoQueueItem
		for _, q := range demo.queue { // same rule as the real queue: one pending item per product
			if q.ProductID == p.ID && q.Status == "pending" {
				item = q
				break
			}
		}
		if item == nil {
			demo.seq++
			item = &demoQueueItem{ID: demoID(demo.seq), ProductID: p.ID, Sku: p.SKU, ProductName: p.Name,
				Status: "pending", EnrichmentModel: "demo (no model call)", CreatedAt: time.Now().UTC()}
			demo.queue = append(demo.queue, item)
		}
		item.EnrichedData = p.enriched()
		queueID := item.ID
		demo.mu.Unlock()

		c.JSON(http.StatusOK, gin.H{
			"productId": p.ID,
			"originalData": gin.H{
				"name": p.RawName, "sku": p.SKU, "shortDescription": p.RawShort, "fullDescription": p.RawFull,
				"tags": p.RawTags, "visualTags": []string{},
			},
			"enhancedData": p.enriched()["enhancedData"],
			"confidence":   p.Confidence, "processingTime": total,
			"ruleApplied":  gin.H{"name": p.Rule, "category": p.Category},
			"stages":       stages,
			"completeness": gin.H{"before": p.before(), "after": p.after()},
			"queueId":      queueID,
		})
	})

	g.GET("/queue", func(c *gin.Context) {
		status := c.DefaultQuery("status", "pending")
		if status != "pending" && status != "approved" && status != "rejected" && status != "all" {
			status = "pending"
		}
		demo.mu.Lock()
		defer demo.mu.Unlock()
		items := make([]*demoQueueItem, 0, len(demo.queue))
		for _, q := range demo.queue {
			if status == "all" || q.Status == status {
				items = append(items, q)
			}
		}
		sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
		c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
	})

	review := func(newStatus string) gin.HandlerFunc {
		return func(c *gin.Context) {
			var req struct {
				Reason string `json:"reason"`
			}
			_ = c.ShouldBindJSON(&req)
			if len(req.Reason) > 300 {
				req.Reason = req.Reason[:300]
			}
			demo.mu.Lock()
			defer demo.mu.Unlock()
			for _, q := range demo.queue {
				if q.ID != c.Param("id") {
					continue
				}
				if q.Status != "pending" {
					c.JSON(http.StatusConflict, gin.H{"error": "item was already processed"})
					return
				}
				now := time.Now().UTC()
				q.Status, q.ReviewedAt = newStatus, &now
				if newStatus == "rejected" {
					q.RejectionReason = req.Reason
				}
				c.JSON(http.StatusOK, gin.H{"status": newStatus, "id": q.ID})
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": "queue item not found"})
		}
	}
	g.POST("/queue/:id/approve", review("approved"))
	g.POST("/queue/:id/reject", review("rejected"))

	g.POST("/reset", func(c *gin.Context) {
		demo.mu.Lock()
		demo.queue, demo.seq = nil, 0
		demo.mu.Unlock()
		c.JSON(http.StatusOK, gin.H{"status": "reset"})
	})
}

func demoID(n int) string {
	const digits = "0123456789"
	if n < 10 {
		return "q-00" + string(digits[n])
	}
	if n < 100 {
		return "q-0" + string(digits[n/10]) + string(digits[n%10])
	}
	return "q-" + string(digits[(n/100)%10]) + string(digits[(n/10)%10]) + string(digits[n%10])
}
