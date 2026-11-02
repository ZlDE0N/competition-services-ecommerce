package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// TaxonomyLevel define en qué nivel de la taxonomía aplica el prompt
type TaxonomyLevel string

const (
	TaxonomyLevelDepartment  TaxonomyLevel = "department"
	TaxonomyLevelCategory    TaxonomyLevel = "category"
	TaxonomyLevelSubCategory TaxonomyLevel = "subcategory"
)

// TaxonomyPrompt representa un prompt personalizado para una combinación de taxonomía
type TaxonomyPrompt struct {
	ID          string        `json:"id"`
	Level       TaxonomyLevel `json:"level"`       // "department", "category", "subcategory"
	Department  string        `json:"department"`  // Nombre del departamento
	Category    string        `json:"category"`    // Nombre de la categoría (vacío si solo aplica a dept)
	SubCategory string        `json:"subCategory"` // Nombre de la subcategoría (vacío si solo aplica a cat)
	Prompt      string        `json:"prompt"`      // Instrucciones libres adicionales para esta taxonomía

	// Campos estructurados de la Matriz de Taxonomía
	Tone           string   `json:"tone"`           // "aspiracional" | "tecnico" | "informativo"
	Format         string   `json:"format"`         // "bullets" | "paragraph" | "mixed"
	KeyAttributes  []string `json:"keyAttributes"`  // Atributos clave que siempre deben mencionarse (ej: ["Número de pines", "Incluye llave"])
	ShortDescGuide string   `json:"shortDescGuide"` // Guía específica para la descripción corta
	LongDescGuide  string   `json:"longDescGuide"`  // Guía específica para la descripción larga

	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// PromptConfig almacena el prompt general y todos los prompts por taxonomía
type PromptConfig struct {
	GeneralPrompt   string           `json:"generalPrompt"`   // Prompt base para todos los productos
	TaxonomyPrompts []TaxonomyPrompt `json:"taxonomyPrompts"` // Prompts específicos por taxonomía
	UpdatedAt       time.Time        `json:"updatedAt"`
}

// promptStore almacena los prompts en memoria con mutex para concurrencia
var (
	promptStore    PromptConfig
	promptStoreMu  sync.RWMutex
	promptCacheKey = "PIM:AI:prompt_config:v1" // prefixed at runtime via rk()
)

func init() {
	// Prompt general por defecto
	promptStore = PromptConfig{
		GeneralPrompt: `Eres un experto en información de productos para el mercado de Panamá y Centroamérica.
Al generar contenido para fichas de productos, debes:
- Usar español neutro y profesional
- Respetar el tono comercial de las marcas
- Ser preciso con especificaciones técnicas
- Destacar los beneficios clave para el consumidor
- Mantener coherencia con la categoría del producto`,
		TaxonomyPrompts: []TaxonomyPrompt{},
		UpdatedAt:       time.Now(),
	}

	// Intentar cargar desde Redis si está disponible
	loadPromptsFromRedis()
}

// loadPromptsFromRedis carga la configuración de prompts desde Redis
func loadPromptsFromRedis() {
	if rdb == nil {
		return
	}
	val, err := rdb.Get(ctx, rk(promptCacheKey)).Result()
	if err != nil {
		return
	}
	var config PromptConfig
	if err := json.Unmarshal([]byte(val), &config); err != nil {
		return
	}
	promptStoreMu.Lock()
	promptStore = config
	promptStoreMu.Unlock()
	fmt.Printf("✅ Prompt config loaded from Redis\n")
}

// savePromptsToRedis persiste la configuración en Redis
func savePromptsToRedis() {
	if rdb == nil {
		return
	}
	promptStoreMu.RLock()
	data, err := json.Marshal(promptStore)
	promptStoreMu.RUnlock()
	if err != nil {
		return
	}
	rdb.Set(ctx, rk(promptCacheKey), data, 0) // Sin TTL — persiste indefinidamente
}

// GetTaxonomyContextForProduct devuelve el contexto de taxonomía más específico para un producto.
// Aplica precedencia: subcategoría > categoría > departamento.
// Retorna nil si no hay match activo.
func GetTaxonomyContextForProduct(product *Product) *TaxonomyPrompt {
	promptStoreMu.RLock()
	defer promptStoreMu.RUnlock()

	dept := strings.ToLower(strings.TrimSpace(product.DepartmentName))
	cat := strings.ToLower(strings.TrimSpace(product.CategoryName))
	sub := strings.ToLower(strings.TrimSpace(product.SubCategoryName))

	var bySubcat, byCat, byDept *TaxonomyPrompt

	for i := range promptStore.TaxonomyPrompts {
		tp := &promptStore.TaxonomyPrompts[i]
		if !tp.Active {
			continue
		}
		tpDept := strings.ToLower(strings.TrimSpace(tp.Department))
		tpCat := strings.ToLower(strings.TrimSpace(tp.Category))
		tpSub := strings.ToLower(strings.TrimSpace(tp.SubCategory))

		switch tp.Level {
		case TaxonomyLevelSubCategory:
			if dept == tpDept && cat == tpCat && sub == tpSub && bySubcat == nil {
				bySubcat = tp
			}
		case TaxonomyLevelCategory:
			if dept == tpDept && cat == tpCat && byCat == nil {
				byCat = tp
			}
		case TaxonomyLevelDepartment:
			if dept == tpDept && byDept == nil {
				byDept = tp
			}
		}
	}

	// Devolver el más específico disponible
	if bySubcat != nil {
		return bySubcat
	}
	if byCat != nil {
		return byCat
	}
	return byDept
}

// GetPromptForProduct devuelve el prompt más específico para un producto dado
// Busca de lo más específico (dept+cat+subcat) a lo más general (general)
func GetPromptForProduct(product *Product) string {
	promptStoreMu.RLock()
	defer promptStoreMu.RUnlock()

	dept := strings.ToLower(strings.TrimSpace(product.DepartmentName))
	cat := strings.ToLower(strings.TrimSpace(product.CategoryName))
	sub := strings.ToLower(strings.TrimSpace(product.SubCategoryName))

	var matched []string

	for _, tp := range promptStore.TaxonomyPrompts {
		if !tp.Active || tp.Prompt == "" {
			continue
		}
		tpDept := strings.ToLower(strings.TrimSpace(tp.Department))
		tpCat := strings.ToLower(strings.TrimSpace(tp.Category))
		tpSub := strings.ToLower(strings.TrimSpace(tp.SubCategory))

		switch tp.Level {
		case TaxonomyLevelSubCategory:
			if dept == tpDept && cat == tpCat && sub == tpSub {
				matched = append([]string{tp.Prompt}, matched...) // Mayor peso: al frente
			}
		case TaxonomyLevelCategory:
			if dept == tpDept && cat == tpCat {
				matched = append(matched, tp.Prompt)
			}
		case TaxonomyLevelDepartment:
			if dept == tpDept {
				matched = append(matched, tp.Prompt)
			}
		}
	}

	// Construir prompt final: taxonomía específica (mayor peso) + general
	parts := []string{}
	if promptStore.GeneralPrompt != "" {
		parts = append(parts, promptStore.GeneralPrompt)
	}
	parts = append(parts, matched...)

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// buildSystemPromptForTaxonomy construye el system prompt de Claude ajustado al contexto
// de taxonomía del producto. Si no hay contexto, devuelve el prompt genérico estático.
func buildSystemPromptForTaxonomy(ctx *TaxonomyPrompt) string {
	// Tono
	tone := "informativo"
	if ctx != nil && ctx.Tone != "" {
		tone = ctx.Tone
	}

	// Formato
	format := "bullets"
	if ctx != nil && ctx.Format != "" {
		format = ctx.Format
	}

	// Instrucciones de tono
	var toneInstruction string
	switch tone {
	case "aspiracional":
		toneInstruction = `TONE: Aspirational and emotional. Use language that evokes lifestyle, comfort, and desire.
- SHORT: Emotionally engaging, highlights how the product improves the customer's life.
- LONG: Tell a story. Connect the product to the customer's aspirations, home, or well-being.
- Avoid cold technical specs in SHORT and LONG. Emphasize experience and feeling.`
	case "tecnico":
		toneInstruction = `TONE: Technical and precise. Customers want specs, not emotions.
- SHORT: Lead with the most important technical specs. Be direct and factual.
- LONG: Expand on technical details, compatibility, materials, and measurements.
- Avoid aspirational or emotional language. Facts only.`
	default:
		toneInstruction = `TONE: Balanced and informative. Mix relevant specs with customer benefits.
- SHORT: Clear and concise, covering the most relevant product aspects.
- LONG: Informative narrative combining benefits and key specifications.`
	}

	// Instrucciones de formato para bullets
	var bulletsInstruction string
	switch format {
	case "paragraph":
		bulletsInstruction = `BULLETS: Write 3-5 short benefit phrases (not "Label: Value"). Example: "Protección reforzada contra intrusos" | "Instalación sencilla sin herramientas especiales"`
	case "mixed":
		bulletsInstruction = `BULLETS: Mix of "Label: Value" specs and short benefit phrases. Example: "Material: Acero inoxidable" | "Acabado duradero y resistente a la corrosión"`
	default: // "bullets"
		bulletsInstruction = `BULLETS RULES (CRITICAL):
- BULLETS must ALWAYS be in "Label: Value" format. Examples: "Marca: Master Lock", "Material: Aluminio", "Medida: 30mm"
- NEVER write narrative sentences in BULLETS. WRONG: "Ideal para uso exterior". CORRECT: "Uso: Exterior"
- Min 4 bullets, max 8 bullets. ALWAYS include at least 4.
- If brand exists → first bullet MUST be "Marca: <brand>"`
	}

	// Atributos clave obligatorios
	var keyAttrsInstruction string
	if ctx != nil && len(ctx.KeyAttributes) > 0 {
		keyAttrsInstruction = fmt.Sprintf(`KEY ATTRIBUTES (MANDATORY): The following attributes MUST appear in BULLETS and/or SHORT description:
%s
If the product data does not provide these values, mark them as "A consultar". Do NOT omit them.`, strings.Join(ctx.KeyAttributes, "\n- "))
		keyAttrsInstruction = "- " + keyAttrsInstruction
	}

	// Guías por campo
	var fieldGuides string
	if ctx != nil {
		if ctx.ShortDescGuide != "" {
			fieldGuides += fmt.Sprintf("\nSHORT DESCRIPTION GUIDE: %s", ctx.ShortDescGuide)
		}
		if ctx.LongDescGuide != "" {
			fieldGuides += fmt.Sprintf("\nLONG DESCRIPTION GUIDE: %s", ctx.LongDescGuide)
		}
	}

	base := fmt.Sprintf(`You are an expert e-commerce content creator. ALL OUTPUT IN SPANISH (Latin American).
DATA PRIORITY: 1.TECHNICAL SHEET > 2.PIM ATTRIBUTES > 3.WEB SEARCH > 4.Product name.
NEVER INVENT specs. Never use SKU/codes as model or brand.

%s

%s
%s%s

Respond in EXACTLY this plain-text format (no JSON, no markdown, no extra text):
SHORT: <short customer-focused description in Spanish>
LONG: <long description in Spanish>
TITLE: <selling title max 48 chars in Spanish>
BULLETS: <bullet1> | <bullet2> | <bullet3> | <bullet4>
BENEFITS: <benefit1> | <benefit2> | <benefit3>
ATTRS: <attr1> | <attr2> | <attr3>
SEO_TITLE: <SEO title max 60 chars in Spanish>
SEO_DESC: <SEO description max 160 chars in Spanish>`,
		toneInstruction,
		bulletsInstruction,
		keyAttrsInstruction,
		fieldGuides,
	)

	return base
}

// ============================================================
// HTTP Handlers
// ============================================================

// getPromptConfig devuelve la configuración completa de prompts
func getPromptConfig(c *gin.Context) {
	promptStoreMu.RLock()
	config := promptStore
	promptStoreMu.RUnlock()

	c.JSON(http.StatusOK, config)
}

// updateGeneralPrompt actualiza el prompt general
func updateGeneralPrompt(c *gin.Context) {
	var body struct {
		Prompt string `json:"prompt" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt es requerido"})
		return
	}

	promptStoreMu.Lock()
	promptStore.GeneralPrompt = strings.TrimSpace(body.Prompt)
	promptStore.UpdatedAt = time.Now()
	promptStoreMu.Unlock()

	// Invalidar caché de productos enriquecidos (prompt cambió)
	if rdb != nil {
		rdb.Del(ctx, rk("PIM:AI:enhanced_product:*"))
	}

	savePromptsToRedis()
	c.JSON(http.StatusOK, gin.H{"message": "Prompt general actualizado correctamente"})
}

// createTaxonomyPrompt crea un nuevo prompt para una taxonomía
func createTaxonomyPrompt(c *gin.Context) {
	var tp TaxonomyPrompt
	if err := c.ShouldBindJSON(&tp); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if tp.Department == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "department es requerido"})
		return
	}
	// Determinar nivel automáticamente
	if tp.SubCategory != "" {
		tp.Level = TaxonomyLevelSubCategory
	} else if tp.Category != "" {
		tp.Level = TaxonomyLevelCategory
	} else {
		tp.Level = TaxonomyLevelDepartment
	}

	tp.ID = fmt.Sprintf("%d", time.Now().UnixNano())
	tp.Active = true
	tp.CreatedAt = time.Now()
	tp.UpdatedAt = time.Now()

	promptStoreMu.Lock()
	promptStore.TaxonomyPrompts = append(promptStore.TaxonomyPrompts, tp)
	promptStore.UpdatedAt = time.Now()
	promptStoreMu.Unlock()

	savePromptsToRedis()
	c.JSON(http.StatusCreated, tp)
}

// updateTaxonomyPrompt actualiza un prompt de taxonomía existente
func updateTaxonomyPrompt(c *gin.Context) {
	id := c.Param("id")
	var update TaxonomyPrompt
	if err := c.ShouldBindJSON(&update); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	found := false
	promptStoreMu.Lock()
	for i, tp := range promptStore.TaxonomyPrompts {
		if tp.ID == id {
			update.ID = id
			update.CreatedAt = tp.CreatedAt
			update.UpdatedAt = time.Now()
			if update.SubCategory != "" {
				update.Level = TaxonomyLevelSubCategory
			} else if update.Category != "" {
				update.Level = TaxonomyLevelCategory
			} else {
				update.Level = TaxonomyLevelDepartment
			}
			promptStore.TaxonomyPrompts[i] = update
			promptStore.UpdatedAt = time.Now()
			found = true
			break
		}
	}
	promptStoreMu.Unlock()

	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "Prompt no encontrado"})
		return
	}
	savePromptsToRedis()
	c.JSON(http.StatusOK, update)
}

// deleteTaxonomyPrompt elimina un prompt de taxonomía
func deleteTaxonomyPrompt(c *gin.Context) {
	id := c.Param("id")

	found := false
	promptStoreMu.Lock()
	for i, tp := range promptStore.TaxonomyPrompts {
		if tp.ID == id {
			promptStore.TaxonomyPrompts = append(
				promptStore.TaxonomyPrompts[:i],
				promptStore.TaxonomyPrompts[i+1:]...,
			)
			promptStore.UpdatedAt = time.Now()
			found = true
			break
		}
	}
	promptStoreMu.Unlock()

	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "Prompt no encontrado"})
		return
	}
	savePromptsToRedis()
	c.JSON(http.StatusOK, gin.H{"message": "Prompt eliminado correctamente"})
}

// previewPromptForProduct muestra qué prompt se aplicaría a un producto específico
func previewPromptForProduct(c *gin.Context) {
	productId := c.Param("id")

	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Base de datos no disponible"})
		return
	}

	product, err := findProductByID(productId)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Producto no encontrado"})
		return
	}

	taxCtx := GetTaxonomyContextForProduct(product)
	systemPrompt := buildSystemPromptForTaxonomy(taxCtx)

	c.JSON(http.StatusOK, gin.H{
		"productId":       productId,
		"productName":     product.Name,
		"department":      product.DepartmentName,
		"category":        product.CategoryName,
		"subCategory":     product.SubCategoryName,
		"taxonomyContext": taxCtx,
		"systemPrompt":    systemPrompt,
	})
}

// matchPromptForTaxonomy matches taxonomy query params against promptStore rules and returns
// the best-matching rule, match level, general prompt, and all dept-level matching rules.
func matchPromptForTaxonomy(c *gin.Context) {
	department := strings.TrimSpace(c.Query("department"))
	category := strings.TrimSpace(c.Query("category"))
	subCategory := strings.TrimSpace(c.Query("subCategory"))

	if department == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "department es requerido"})
		return
	}

	dept := strings.ToLower(department)
	cat := strings.ToLower(category)
	sub := strings.ToLower(subCategory)

	promptStoreMu.RLock()
	generalPrompt := promptStore.GeneralPrompt
	rules := promptStore.TaxonomyPrompts
	promptStoreMu.RUnlock()

	var bySubcat, byCat, byDept *TaxonomyPrompt
	var allMatchingRules []TaxonomyPrompt

	for i := range rules {
		tp := &rules[i]
		if !tp.Active {
			continue
		}
		tpDept := strings.ToLower(strings.TrimSpace(tp.Department))
		tpCat := strings.ToLower(strings.TrimSpace(tp.Category))
		tpSub := strings.ToLower(strings.TrimSpace(tp.SubCategory))

		if tpDept != dept {
			continue
		}

		// Collect all active rules where department matches
		allMatchingRules = append(allMatchingRules, *tp)

		switch tp.Level {
		case TaxonomyLevelSubCategory:
			if cat != "" && sub != "" && tpCat == cat && tpSub == sub && bySubcat == nil {
				bySubcat = tp
			}
		case TaxonomyLevelCategory:
			if cat != "" && tpCat == cat && byCat == nil {
				byCat = tp
			}
		case TaxonomyLevelDepartment:
			if byDept == nil {
				byDept = tp
			}
		}
	}

	var matchedRule *TaxonomyPrompt
	matchLevel := "none"

	if bySubcat != nil {
		matchedRule = bySubcat
		matchLevel = "subcategory"
	} else if byCat != nil {
		matchedRule = byCat
		matchLevel = "category"
	} else if byDept != nil {
		matchedRule = byDept
		matchLevel = "department"
	}

	if allMatchingRules == nil {
		allMatchingRules = []TaxonomyPrompt{}
	}

	c.JSON(http.StatusOK, gin.H{
		"matchedRule":      matchedRule,
		"matchLevel":       matchLevel,
		"generalPrompt":    generalPrompt,
		"allMatchingRules": allMatchingRules,
	})
}

// getTaxonomyOptions devuelve los departamentos, categorías y subcategorías disponibles en la DB
// para que el frontend pueda ofrecer dropdowns al configurar la matriz.
func getTaxonomyOptions(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Base de datos no disponible"})
		return
	}

	type Option struct {
		Name string `json:"name"`
	}

	// Departamentos
	deptRows, err := db.Query(`SELECT DISTINCT d.Name FROM catalog.pltblDepartment d WHERE d.Name IS NOT NULL AND d.Name <> '' ORDER BY d.Name`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Error al consultar departamentos"})
		return
	}
	defer deptRows.Close()
	var departments []Option
	for deptRows.Next() {
		var o Option
		if err := deptRows.Scan(&o.Name); err == nil && o.Name != "" {
			departments = append(departments, o)
		}
	}

	// Categorías
	catRows, err := db.Query(`SELECT DISTINCT c.Name FROM catalog.pltblCategory c WHERE c.Name IS NOT NULL AND c.Name <> '' ORDER BY c.Name`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Error al consultar categorías"})
		return
	}
	defer catRows.Close()
	var categories []Option
	for catRows.Next() {
		var o Option
		if err := catRows.Scan(&o.Name); err == nil && o.Name != "" {
			categories = append(categories, o)
		}
	}

	// Subcategorías
	subRows, err := db.Query(`SELECT DISTINCT s.Name FROM catalog.pltblSubCategory s WHERE s.Name IS NOT NULL AND s.Name <> '' ORDER BY s.Name`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Error al consultar subcategorías"})
		return
	}
	defer subRows.Close()
	var subcategories []Option
	for subRows.Next() {
		var o Option
		if err := subRows.Scan(&o.Name); err == nil && o.Name != "" {
			subcategories = append(subcategories, o)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"departments":   departments,
		"categories":    categories,
		"subcategories": subcategories,
	})
}
