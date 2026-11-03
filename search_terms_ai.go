package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type ImageSearchTermsResponse struct {
	MainTerms      []string `json:"mainTerms"`
	Variations     []string `json:"variations"`
	WithBackground []string `json:"withBackground"`
}

func generateImageSearchTermsWithClaude(product *Product) ([]string, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not configured")
	}

	// Crear prompt especializado para búsqueda específica del producto con TODOS los campos relevantes
	prompt := fmt.Sprintf(`You are a product search specialist. Your task is to generate PRECISE search terms to find images of this EXACT product, not similar products or categories.

Product to find:
- Name: %s
- SKU: %s
- Manufacturer Part Number: %s
- Barcode/GTIN: %s
- Brand: %s
- Short Description: %s
- Full Description: %s
- External Codes: %s

CRITICAL REQUIREMENTS:
1. Be SPECIFIC to this exact product - prioritize manufacturer part numbers and brand names
2. Use manufacturer part numbers and model numbers as primary search terms
3. Include brand name in search terms for better accuracy
4. Create terms that match commercial listings (Amazon, Home Depot, etc.)
5. Keep specific measurements, materials, and model details
6. Focus on finding THIS product, not similar products
7. Use both Spanish and English terms for broader coverage
8. Include product category with brand + part number

SEARCH TERM PRIORITY (CRITICAL):
1. Manufacturer Part Number ALONE (highest priority - exact match)
2. Brand + Manufacturer Part Number (second highest priority - combined search)
3. Manufacturer Part Number + Product Category
4. Brand + Product Name + Key Specifications
5. Full Product Name + Model Details
6. Brand + Manufacturer Part Number + Product Category (triple combination)

Generate 3-5 search terms that are:
- PRIORITIZE manufacturer part number ONLY if it's alphanumeric (avoid UUIDs/GUIDs)
- Use actual product category words (curtain, cabinet, hardware, etc.)
- Include brand + product type + key features
- Include measurements, materials, colors from product name
- AVOID generic terms like "white background", "isolated" 
- NO UUIDs or internal system codes (D9F42A48-style codes)
- Focus on what customers would search for to buy this product

Example approach:
If product is "2pc cortina blackout 37x96 white" with part number "AGE-PPR-7696-EL-WHITE" and brand "ACME"
Good terms: ["AGE-PPR-7696-EL-WHITE", "ACME AGE-PPR-7696-EL-WHITE", "blackout curtain 37x96 white", "ACME blackout curtain 37x96"]
Bad terms: ["AGE-PPR-7696-EL-WHITE white background", "D9F42A48-E4C0-471D", "white isolated"]

If product is "Estufa de gas 24 pulgadas" with part number "EM6038CFIX" and brand "MABE"
Good terms: ["EM6038CFIX", "MABE EM6038CFIX", "MABE estufa gas 24 pulgadas", "MABE EM6038CFIX estufa"]
Bad terms: ["estufa gas", "24 pulgadas", "generic stove"] (too generic)

If product is "1/2 -13 x 3/4 tornillo" with part number "43305" and brand "HILLMAN"  
Good terms: ["43305", "HILLMAN 43305", "HILLMAN 43305 screw", "HILLMAN 43305 stainless steel", "HILLMAN cap screw 43305"]
Bad terms: ["1/2", "tornillo", "screw"] (too generic)

If product is "cabinet pull bronze" with NO clear part number and brand "Liberty"
Good terms: ["Liberty cabinet pull bronze", "Liberty bronze hardware", "bronze drawer pull Liberty"]
Bad terms: ["hardware", "bronze", "cabinet"] (too generic)

Respond ONLY with this JSON:
{
  "mainTerms": ["specific_term1", "specific_term2", "specific_term3"],
  "variations": ["variation1", "variation2"]
}`,
		product.Name,
		product.Sku,
		getStringValue(product.ManufacturerPartNumber),
		getStringValue(product.Gtin),
		getBrandName(product),
		getStringValue(product.ShortDescription),
		getStringValue(product.FullDescription),
		getStringValue(product.ExternalCodes))

	request := ClaudeRequest{
		Model: "claude-haiku-4-5-20251001",
		Messages: []Message{
			{
				Role:    "user",
				Content: prompt,
			},
		},
		MaxTokens: 400,
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("error marshaling request: %v", err)
	}

	callCtx, cancelCall := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelCall()
	respBody, err := postClaude(callCtx, requestBody)
	if err != nil {
		return nil, err
	}

	var claudeResponse ClaudeResponse
	if err := json.Unmarshal(respBody, &claudeResponse); err != nil {
		return nil, fmt.Errorf("error decoding response: %v", err)
	}

	if len(claudeResponse.Content) == 0 {
		return nil, fmt.Errorf("empty response from Claude")
	}

	// Extraer y limpiar el JSON de la respuesta
	responseText := claudeResponse.Content[0].Text
	responseText = strings.TrimSpace(responseText)

	// Encontrar el JSON en la respuesta
	startIndex := strings.Index(responseText, "{")
	endIndex := strings.LastIndex(responseText, "}")

	if startIndex == -1 || endIndex == -1 {
		return nil, fmt.Errorf("no JSON found in response: %s", responseText)
	}

	jsonText := responseText[startIndex : endIndex+1]

	// Parsear la respuesta JSON
	var imageTermsResponse ImageSearchTermsResponse
	if err := json.Unmarshal([]byte(jsonText), &imageTermsResponse); err != nil {
		return nil, fmt.Errorf("error parsing response JSON: %v, text: %s", err, jsonText)
	}

	// Combinar todos los términos con prioridad
	var allTerms []string

	// PRIORIDAD MÁXIMA: Barcode/GTIN como primer término si es válido
	// El barcode identifica el producto de forma exacta — la IA no siempre lo prioriza
	if product.Gtin != nil {
		gtin := strings.TrimSpace(*product.Gtin)
		if gtin != "" && !isUUIDString(gtin) && len(gtin) >= 6 {
			fmt.Printf("✅ Adding barcode/GTIN as first search term: %s\n", gtin)
			allTerms = append(allTerms, gtin)
			// Si tiene marca, agregar también "MARCA + GTIN"
			brand := getBrandName(product)
			if brand != "" && !isGenericBrand(brand) && !isUUIDString(brand) {
				allTerms = append(allTerms, fmt.Sprintf("%s %s", brand, gtin))
			}
		}
	}

	// Añadir términos principales de Claude (más específicos)
	allTerms = append(allTerms, imageTermsResponse.MainTerms...)

	// Añadir términos con modificadores de fondo blanco
	allTerms = append(allTerms, imageTermsResponse.WithBackground...)

	// Añadir variaciones si se necesitan más términos
	if len(allTerms) < 4 {
		allTerms = append(allTerms, imageTermsResponse.Variations...)
	}

	// Filtrar términos vacíos y duplicados
	var finalTerms []string
	seenTerms := make(map[string]bool)

	for _, term := range allTerms {
		term = strings.TrimSpace(term)
		if term != "" && !seenTerms[term] {
			finalTerms = append(finalTerms, term)
			seenTerms[term] = true
		}
	}

	if len(finalTerms) == 0 {
		return nil, fmt.Errorf("no valid search terms generated")
	}

	return finalTerms, nil
}

// getBrandName extrae el nombre de la marca del producto
func getBrandName(product *Product) string {
	// PRIORIDAD 1: Usar BrandName del JOIN con tabla de marcas
	if product.BrandName != "" {
		fmt.Printf("✅ Found brand from database: %s\n", product.BrandName)
		return product.BrandName
	}

	// PRIORIDAD 2: Intentar extraer desde PIMAttributes
	if product.PIMAttributes != nil {
		var pimData map[string]interface{}
		if err := json.Unmarshal([]byte(*product.PIMAttributes), &pimData); err == nil {
			// Log para debugging
			fmt.Printf("🔍 PIMAttributes data: %+v\n", pimData)

			if brand, ok := pimData["brand"].(string); ok && brand != "" {
				fmt.Printf("✅ Found brand in PIMAttributes: %s\n", brand)
				return brand
			}
			if brandName, ok := pimData["brandName"].(string); ok && brandName != "" {
				fmt.Printf("✅ Found brandName in PIMAttributes: %s\n", brandName)
				return brandName
			}
			if manufacturer, ok := pimData["manufacturer"].(string); ok && manufacturer != "" {
				fmt.Printf("✅ Found manufacturer in PIMAttributes: %s\n", manufacturer)
				return manufacturer
			}
		} else {
			fmt.Printf("❌ Error unmarshaling PIMAttributes: %v\n", err)
		}
	}

	// PRIORIDAD 3: Usar BrandId como último recurso (UUID)
	if product.BrandId != "" {
		fmt.Printf("⚠️ Using BrandId as fallback (UUID): %s\n", product.BrandId)
		return product.BrandId
	}

	fmt.Printf("❌ No brand found for product\n")
	return ""
}

// extractValidGTIN retorna el primer código de barras válido del producto.
// Revisa en orden: campo Gtin (del catálogo/ERP) y campo Barcodes (del PIM UI).
// El campo Barcodes puede ser JSON como [{"Code":"085081703422","UoM":"UNIDAD UN"},...],
// o un string con el código directamente, por lo que se extrae el primer número largo encontrado.
// Un GTIN válido: no vacío, no tiene formato UUID, longitud ≥ 6.
func extractValidGTIN(product *Product) string {
	// 1. Revisar campo Gtin primero
	if product.Gtin != nil {
		gtin := strings.TrimSpace(*product.Gtin)
		if gtin != "" && !isUUIDString(gtin) && len(gtin) >= 6 {
			fmt.Printf("✅ [GTIN] Found in Gtin field: %s\n", gtin)
			return gtin
		}
	}

	// 2. Fallback: campo PrimaryBarcode (subquery desde catalog.tblBarcode, tabla del PIM UI)
	if product.PrimaryBarcode != "" {
		bc := strings.TrimSpace(product.PrimaryBarcode)
		if !isUUIDString(bc) && len(bc) >= 6 {
			fmt.Printf("✅ [GTIN] Found in tblBarcode (PIM UI): %s\n", bc)
			return bc
		}
	}

	// 3. Último fallback: campo Barcodes de tblproduct (campo legacy/ERP denormalizado)
	if product.Barcodes == nil || *product.Barcodes == "" {
		return ""
	}
	raw := strings.TrimSpace(*product.Barcodes)

	// Intentar extraer con regex: primer bloque de 6+ dígitos consecutivos
	// Cubre formatos: JSON ({"Code":"085081703422",...}), CSV, texto plano, etc.
	digitRe := regexp.MustCompile(`\b(\d{6,})\b`)
	match := digitRe.FindString(raw)
	if match != "" && !isUUIDString(match) {
		fmt.Printf("✅ [GTIN] Extracted from Barcodes field (legacy): %s\n", match)
		return match
	}

	return ""
}

// generateHybridSearchTermsWithVariation genera términos de búsqueda con soporte para variación
// Consolidado a 3 variaciones para maximizar diferencia entre reintentos:
// variation=0: búsqueda estándar completa (part number + marca + nombre + specs + categoría extraída + inglés)
// variation=1: contexto de BD (proveedor + categoría + subcategoría + departamento de la base de datos)
// variation=2: IA creativa con todos los datos enriquecidos (Claude genera términos genuinamente diferentes)
func generateHybridSearchTermsWithVariation(product *Product, variation int) []string {
	fmt.Printf("🔄 Generating search terms with variation=%d\n", variation)

	// Generar los términos según la variación solicitada
	var variationTerms []string
	switch variation {
	case 1:
		// VARIACIÓN 1: Contexto de BD - usa datos reales de taxonomía y proveedor
		variationTerms = generateDatabaseContextSearchTerms(product)
	case 2:
		// VARIACIÓN 2: IA creativa con datos enriquecidos de BD
		variationTerms = generateEnrichedAISearchTerms(product)
	default:
		// VARIACIÓN 0 (default): Búsqueda estándar completa
		variationTerms = generateStandardCompleteSearchTerms(product)
	}

	// PRIORIDAD MÁXIMA: Inyectar todos los barcodes del PIM UI como primeros términos.
	// Cada barcode es una búsqueda exacta que identifica el producto en tiendas online.
	var finalTerms []string
	seen := make(map[string]bool)

	// Obtener marca válida para combinaciones "MARCA BARCODE"
	brand := getBrandName(product)
	validBrand := brand != "" && !isGenericBrand(brand) && !isUUIDString(brand)

	// 1. Todos los barcodes de catalog.tblBarcode (fuente más confiable)
	if len(product.AllBarcodes) > 0 {
		fmt.Printf("✅ [Barcode Priority] Injecting %d barcodes: %v\n", len(product.AllBarcodes), product.AllBarcodes)
		for _, bc := range product.AllBarcodes {
			if bc == "" {
				continue
			}
			// Barcode solo
			if !seen[strings.ToLower(bc)] {
				finalTerms = append(finalTerms, bc)
				seen[strings.ToLower(bc)] = true
			}
			// Combinación "MARCA BARCODE" solo para el primero (default) para no saturar
			if bc == product.AllBarcodes[0] && validBrand {
				combo := fmt.Sprintf("%s %s", brand, bc)
				if !seen[strings.ToLower(combo)] {
					finalTerms = append(finalTerms, combo)
					seen[strings.ToLower(combo)] = true
				}
			}
		}
	} else {
		// 2. Fallback: GTIN del ERP o campo Barcodes
		gtin := extractValidGTIN(product)
		if gtin != "" {
			fmt.Printf("✅ [GTIN Priority] Injecting barcode as first search term: %s\n", gtin)
			finalTerms = append(finalTerms, gtin)
			seen[strings.ToLower(gtin)] = true
			if validBrand {
				combo := fmt.Sprintf("%s %s", brand, gtin)
				finalTerms = append(finalTerms, combo)
				seen[strings.ToLower(combo)] = true
			}
		}
	}

	if len(finalTerms) == 0 {
		return variationTerms
	}

	// Añadir los términos de la variación, evitando duplicados
	for _, t := range variationTerms {
		if t != "" && !seen[strings.ToLower(t)] {
			finalTerms = append(finalTerms, t)
			seen[strings.ToLower(t)] = true
		}
	}

	return finalTerms
}

// generateStandardCompleteSearchTerms - Variación 0: Combina part number, marca, nombre,
// especificaciones, categoría extraída e inglés en una sola búsqueda robusta.
// Fusiona lo mejor de las antiguas variaciones 0, 1, 2 y 3.
func generateStandardCompleteSearchTerms(product *Product) []string {
	fmt.Printf("🔄 [Variation 0] Generating STANDARD COMPLETE terms\n")

	// Primero intentar la estrategia segmentada existente (que ya es buena para var 0)
	segmentedTerms := generateSegmentedSearchTerms(product)
	if len(segmentedTerms) >= 3 {
		// Complementar con términos en inglés y descriptivos si hay espacio
		brand := getBrandName(product)
		productName := strings.TrimSpace(strings.ReplaceAll(product.Name, "\"", ""))
		productCategory := extractProductCategory(productName)
		_, catEN := getCategoryTranslations(productCategory)
		keySpecs := extractKeySpecsFromName(productName)

		seen := make(map[string]bool)
		for _, t := range segmentedTerms {
			seen[t] = true
		}

		// Agregar categoría EN + specs si no está ya
		if catEN != "" && len(keySpecs) > 0 {
			term := fmt.Sprintf("%s %s", catEN, strings.Join(keySpecs, " "))
			if !seen[term] {
				segmentedTerms = append(segmentedTerms, term)
				seen[term] = true
			}
		}

		// Agregar marca + categoría EN si no está ya
		if brand != "" && !isGenericBrand(brand) && !isUUIDString(brand) && catEN != "" {
			term := fmt.Sprintf("%s %s", brand, catEN)
			if !seen[term] {
				segmentedTerms = append(segmentedTerms, term)
			}
		}

		fmt.Printf("   Generated %d standard complete terms: %v\n", len(segmentedTerms), segmentedTerms)
		return segmentedTerms
	}

	// Fallback: generar con Claude + método específico
	return generateHybridSearchTerms(product)
}

// generateDatabaseContextSearchTerms - Variación 1: Usa datos reales de la BD
// (proveedor, categoría, subcategoría, departamento) como CONTEXTO ADICIONAL,
// siempre anclado al nombre del producto para mantener especificidad.
// Regla: nunca generar un término sin palabras clave del producto.
func generateDatabaseContextSearchTerms(product *Product) []string {
	var terms []string
	brand := getBrandName(product)
	productName := strings.TrimSpace(strings.ReplaceAll(product.Name, "\"", ""))
	providerName := strings.TrimSpace(product.ProviderName)
	categoryName := strings.TrimSpace(product.CategoryName)
	subCategoryName := strings.TrimSpace(product.SubCategoryName)
	departmentName := strings.TrimSpace(product.DepartmentName)
	keySpecs := extractKeySpecsFromName(productName)

	// Extraer palabras clave significativas del nombre del producto
	smartKeywords := extractSmartKeywords(productName)
	shortName := productName
	if len(smartKeywords) > 0 {
		// Usar las 3 palabras clave más relevantes como ancla
		limit := len(smartKeywords)
		if limit > 3 {
			limit = 3
		}
		shortName = strings.Join(smartKeywords[:limit], " ")
	}

	fmt.Printf("🔄 [Variation 1] Generating DB CONTEXT terms (provider=%s, dept=%s, cat=%s, subCat=%s, shortName=%s)\n",
		providerName, departmentName, categoryName, subCategoryName, shortName)

	seen := make(map[string]bool)
	addTerm := func(term string) {
		term = strings.TrimSpace(term)
		if term != "" && !seen[term] {
			terms = append(terms, term)
			seen[term] = true
			fmt.Printf("   Added: %s\n", term)
		}
	}

	// 1. Subcategoría de BD + nombre del producto (reemplaza la categoría extraída del nombre)
	if subCategoryName != "" {
		addTerm(fmt.Sprintf("%s %s", subCategoryName, shortName))
	}

	// 2. Marca + subcategoría + specs (combinación diferente a var 0)
	if brand != "" && !isGenericBrand(brand) && !isUUIDString(brand) && subCategoryName != "" {
		term := fmt.Sprintf("%s %s", brand, subCategoryName)
		if len(keySpecs) > 0 {
			term = fmt.Sprintf("%s %s", term, strings.Join(keySpecs, " "))
		}
		addTerm(term)
	}

	// 3. Categoría de BD + nombre del producto + marca
	if categoryName != "" {
		term := fmt.Sprintf("%s %s", categoryName, shortName)
		if brand != "" && !isGenericBrand(brand) && !isUUIDString(brand) {
			term = fmt.Sprintf("%s %s", term, brand)
		}
		addTerm(term)
	}

	// 4. Departamento + nombre del producto (contexto más amplio)
	if departmentName != "" {
		addTerm(fmt.Sprintf("%s %s", departmentName, shortName))
	}

	// 5. Subcategoría + marca + nombre (orden diferente para resultados distintos)
	if subCategoryName != "" && brand != "" && !isGenericBrand(brand) && !isUUIDString(brand) {
		addTerm(fmt.Sprintf("%s %s %s", subCategoryName, brand, shortName))
	}

	// Fallback: si no hay datos de BD, usar nombre con traducción inteligente
	if len(terms) == 0 {
		fmt.Printf("   ⚠️ No DB context data found, using product name fallback\n")
		addTerm(productName)
		if brand != "" && !isGenericBrand(brand) && !isUUIDString(brand) {
			addTerm(fmt.Sprintf("%s %s", brand, productName))
		}
		fallbackTerm := generateIntelligentFallback(product)
		if fallbackTerm != "" {
			addTerm(fallbackTerm)
		}
	}

	return filterAndValidateSegmentedTerms(terms)
}

// generateEnrichedAISearchTerms - Variación 2: Llama a Claude con TODOS los datos
// (nombre, part number, marca + categoría, subcategoría, departamento, proveedor de BD)
// para generar términos creativos genuinamente diferentes a las variaciones 0 y 1.
func generateEnrichedAISearchTerms(product *Product) []string {
	fmt.Printf("🔄 [Variation 2] Generating ENRICHED AI terms\n")

	// Intentar generar con IA enriquecida
	terms, err := generateEnrichedImageSearchTermsWithClaude(product)
	if err != nil {
		fmt.Printf("   ⚠️ Enriched AI generation failed: %v, using DB context fallback\n", err)
		// Fallback: usar variación 1 (contexto de BD) como respaldo
		return generateDatabaseContextSearchTerms(product)
	}

	return terms
}

// generateEnrichedImageSearchTermsWithClaude genera términos de búsqueda usando Claude con datos enriquecidos
func generateEnrichedImageSearchTermsWithClaude(product *Product) ([]string, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not configured")
	}

	categoryName := strings.TrimSpace(product.CategoryName)
	subCategoryName := strings.TrimSpace(product.SubCategoryName)
	departmentName := strings.TrimSpace(product.DepartmentName)
	//providerName := strings.TrimSpace(product.ProviderName)

	prompt := fmt.Sprintf(`You are a product image search specialist. Generate search terms to find images of this SPECIFIC product.
This is a RETRY - previous searches did not find good results. You must generate DIFFERENT combinations, but ALWAYS keep the search SPECIFIC to this product.

Product details:
- Name: %s
- SKU: %s
- Manufacturer Part Number: %s
- Brand: %s
- Category: %s
- SubCategory: %s
- Department: %s
- Short Description: %s

CRITICAL RULES:
1. EVERY search term MUST include the product name or its key identifying words (brand, model, key specs from the name). NEVER generate generic category-only terms.
2. Use the Category/SubCategory/Department as CONTEXT to complement the product name, not as standalone terms.
3. Combine product keywords with different contexts: brand+model+category, name+subcategory, brand+specs+department.
4. Include both English and Spanish terms, but always anchored to product-specific words.
5. If there is a manufacturer part number, include it in at least one term.

BAD examples (too generic - NEVER do this):
- "ductless mini-split air conditioner" (no brand, no model)
- "refrigeration appliances" (category only)
- "snacks" (category only)

GOOD examples (specific - DO this):
- "MABE MSM12CDBP split air conditioner" (brand + model + category)
- "MABE inverter 12000 BTU split" (brand + specs from name + type)
- "aire acondicionado MABE MSM12CDBP" (Spanish + brand + model)

Generate 3-5 search terms. Each MUST contain at least the brand or product name keywords.

Respond ONLY with this JSON:
{
  "mainTerms": ["specific_term1", "specific_term2", "specific_term3"],
  "variations": ["specific_alt1", "specific_alt2"]
}`,
		product.Name,
		product.Sku,
		getStringValue(product.ManufacturerPartNumber),
		getBrandName(product),
		categoryName,
		subCategoryName,
		departmentName,
		getStringValue(product.ShortDescription))

	request := ClaudeRequest{
		Model: "claude-haiku-4-5-20251001",
		Messages: []Message{
			{
				Role:    "user",
				Content: prompt,
			},
		},
		MaxTokens: 400,
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("error marshaling request: %v", err)
	}

	callCtx, cancelCall := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelCall()
	respBody, err := postClaude(callCtx, requestBody)
	if err != nil {
		return nil, err
	}

	var claudeResponse ClaudeResponse
	if err := json.Unmarshal(respBody, &claudeResponse); err != nil {
		return nil, fmt.Errorf("error decoding response: %v", err)
	}

	if len(claudeResponse.Content) == 0 {
		return nil, fmt.Errorf("empty response from Claude")
	}

	responseText := claudeResponse.Content[0].Text
	responseText = strings.TrimSpace(responseText)

	startIndex := strings.Index(responseText, "{")
	endIndex := strings.LastIndex(responseText, "}")

	if startIndex == -1 || endIndex == -1 {
		return nil, fmt.Errorf("no JSON found in response: %s", responseText)
	}

	jsonText := responseText[startIndex : endIndex+1]

	var imageTermsResponse ImageSearchTermsResponse
	if err := json.Unmarshal([]byte(jsonText), &imageTermsResponse); err != nil {
		return nil, fmt.Errorf("error parsing response JSON: %v, text: %s", err, jsonText)
	}

	var allTerms []string
	allTerms = append(allTerms, imageTermsResponse.MainTerms...)
	allTerms = append(allTerms, imageTermsResponse.Variations...)

	var finalTerms []string
	seenTerms := make(map[string]bool)
	for _, term := range allTerms {
		term = strings.TrimSpace(term)
		if term != "" && !seenTerms[term] {
			finalTerms = append(finalTerms, term)
			seenTerms[term] = true
		}
	}

	if len(finalTerms) == 0 {
		return nil, fmt.Errorf("no valid search terms generated")
	}

	return finalTerms, nil
}

func generateHybridSearchTerms(product *Product) []string {
	// NUEVA ESTRATEGIA: Búsqueda segmentada combinando múltiples campos
	segmentedTerms := generateSegmentedSearchTerms(product)
	if len(segmentedTerms) > 0 {
		fmt.Printf("Generated %d segmented search terms: %v\n", len(segmentedTerms), segmentedTerms)
		return segmentedTerms
	}

	// Intentar generar términos con Claude como fallback
	claudeTerms, err := generateImageSearchTermsWithClaude(product)
	if err != nil {
		fmt.Printf("Claude search terms failed (%v), using improved fallback method\n", err)
		// Si falla Claude, usar método mejorado como respaldo
		return generateSpecificSearchTerms(product)
	}

	fmt.Printf("Claude generated %d search terms: %v\n", len(claudeTerms), claudeTerms)

	// Si Claude genera pocos términos, complementar con método específico
	if len(claudeTerms) < 2 {
		fallbackTerms := generateSpecificSearchTerms(product)
		// Combinar sin duplicar
		seenTerms := make(map[string]bool)
		var combinedTerms []string

		// Añadir términos de Claude primero (mayor prioridad)
		for _, term := range claudeTerms {
			if !seenTerms[term] {
				combinedTerms = append(combinedTerms, term)
				seenTerms[term] = true
			}
		}

		// Añadir términos específicos si es necesario
		for _, term := range fallbackTerms {
			if !seenTerms[term] && len(combinedTerms) < 4 {
				combinedTerms = append(combinedTerms, term)
				seenTerms[term] = true
			}
		}

		return combinedTerms
	}

	return claudeTerms
}

// Método mejorado de fallback que prioriza número de manufactura
func generateSpecificSearchTerms(product *Product) []string {
	var terms []string

	// PRIORIDAD MÁXIMA: Número de manufactura válido (no UUIDs)
	manufacturerPart := getStringValue(product.ManufacturerPartNumber)
	brand := getBrandName(product)

	// Filtrar UUIDs y códigos internos
	if manufacturerPart != "" && isValidPartNumber(manufacturerPart) {
		// Términos con número de manufactura (máxima prioridad)
		terms = append(terms, manufacturerPart)

		if brand != "" {
			terms = append(terms, brand+" "+manufacturerPart)
		}

		// Agregar categoría del producto si se puede extraer
		if product.Name != "" {
			productCategory := extractProductCategory(product.Name)
			if productCategory != "" {
				terms = append(terms, manufacturerPart+" "+productCategory)
			}
		}
	} else {
		// Fallback inteligente: usar nombre del producto y características
		if product.Name != "" {
			cleanName := strings.ReplaceAll(product.Name, "\"", "")
			cleanName = strings.TrimSpace(cleanName)

			// Extraer palabras clave del nombre del producto
			productKeywords := extractProductKeywords(cleanName)

			// Generar términos basados en las palabras clave
			for _, keyword := range productKeywords {
				if brand != "" && brand != "GENÉRICO" && !strings.Contains(strings.ToUpper(brand), "GENÉRICO") {
					terms = append(terms, brand+" "+keyword)
				}
				terms = append(terms, keyword)

				if len(terms) >= 3 {
					break
				}
			}

			// Estrategia 3: Usar nombre del producto pero con más especificidad
			if brand != "" && brand != "GENÉRICO" && !strings.Contains(brand, "GENÉRICO") {
				terms = append(terms, brand+" "+cleanName)

				// Agregar especificaciones técnicas si existen
				if strings.Contains(cleanName, "\"") || strings.Contains(cleanName, "'") ||
					strings.Contains(cleanName, "mm") || strings.Contains(cleanName, "cm") {
					terms = append(terms, brand+" "+cleanName+" white background")
				}
			} else {
				// Solo nombre del producto si no hay marca válida
				terms = append(terms, cleanName)
				if len(cleanName) > 15 { // Nombre más descriptivo
					terms = append(terms, cleanName+" product image")
				}
			}
		}
	}

	// Si aún no hay términos suficientes, usar método original como último recurso
	if len(terms) < 2 {
		originalTerms := generateImageSearchTerms(product)
		terms = append(terms, originalTerms...)
	}

	return terms
}

// extractProductCategory intenta extraer la categoría del producto del nombre
func extractProductCategory(productName string) string {
	// Lista de categorías comunes en español e inglés
	// IMPORTANTE: El orden importa - categorías más específicas primero para evitar falsos positivos
	categories := []string{
		"tirador", "handle", "pull", "knob", "pomo",
		"abanico", "fan", "ventilador", "ceiling fan",
		"alfombra", "rug", "carpet", "mat",
		"cortina", "curtain", "drape", "blind",
		"lampara", "lamp", "light", "lighting",
		"detector", "sensor", "alarm",
		"cerradura", "lock", "latch",
		"bisagra", "hinge",
		"cenefa", "border", "trim", "molding", "tile border",
		"azulejo", "tile", "ceramic", "ceramic tile",
		"tornillo", "screw", "bolt", "fastener",
		"tuerca", "washer", "hardware",
		// NOTA: "nut" removido de hardware porque causa conflicto con "peanut"
		"estufa", "stove", "cooktop", "range",
		// Golosinas y dulces - categorías específicas primero
		"peanut", "mani", "cacahuate", // Variantes de maní - ANTES de otras categorías
		"dispensador", "dispenser", // NOTA: "disp" removido porque es ambiguo (se confunde con "display")
		"chocolate", "candy", "dulce", "golosina",
		"galleta", "cookie", "biscuit",
		"cereal", "snack",
		// Electrónica y celulares
		"celular", "smartphone", "phone", "tablet",
		"televisor", "television", "tv",
		"laptop", "notebook", "computadora", "computer",
		"audifonos", "auriculares", "headphones", "earbuds",
		"bocina", "parlante", "speaker",
		"camara", "camera",
		// Mascotas
		"bocadillo", "dog treat", "cat treat",
		"perro", "gato", "mascota", "pet",
	}

	lowerName := strings.ToLower(productName)

	for _, category := range categories {
		if strings.Contains(lowerName, category) {
			return category
		}
	}

	// Detectar marcas conocidas de productos que implican una categoría
	// IMPORTANTE: Para M&M, la categoría depende del contexto del nombre del producto
	knownBrands := map[string]string{
		"m&m":       "chocolate", // Por defecto es chocolate, NO dispenser
		"m&ms":      "chocolate",
		"snickers":  "chocolate",
		"skittles":  "candy",
		"twix":      "chocolate",
		"milky way": "chocolate",
		"oreo":      "cookie",
		"pringles":  "snack",
		"doritos":   "snack",
		"cheetos":   "snack",
		"lays":      "snack",
		"coca cola": "beverage",
		"pepsi":     "beverage",
		"gatorade":  "beverage",
		// Mascotas
		"pedigree": "dog treat",
		"purina":   "pet",
		"whiskas":  "cat treat",
		"dog chow": "dog treat",
	}

	for brand, category := range knownBrands {
		if strings.Contains(lowerName, brand) {
			// CASO ESPECIAL: Si el nombre contiene "dispenser" o "dispensador" (palabras completas),
			// y la marca es de dulces, cambiar categoría a "candy dispenser"
			// NOTA: "disp" removido porque Brave lo interpreta como "display" causando resultados de monitores
			if (brand == "m&m" || brand == "m&ms" || brand == "skittles") &&
				(strings.Contains(lowerName, "dispenser") || strings.Contains(lowerName, "dispensador")) {
				fmt.Printf("🔍 Detected known brand '%s' WITH dispenser context -> category 'candy dispenser'\n", brand)
				return "candy dispenser"
			}
			fmt.Printf("🔍 Detected known brand '%s' -> category '%s'\n", brand, category)
			return category
		}
	}

	return ""
}

// getCategoryTranslations devuelve la categoría en español e inglés para búsquedas bilingües
func getCategoryTranslations(category string) (spanish string, english string) {
	translations := map[string]struct{ es, en string }{
		// Tiradores y manijas
		"tirador": {"tirador", "cabinet pull"},
		"handle":  {"manija", "handle"},
		"pull":    {"tirador", "pull"},
		"knob":    {"pomo", "knob"},
		"pomo":    {"pomo", "knob"},
		// Ventiladores
		"abanico":     {"abanico", "ceiling fan"},
		"fan":         {"ventilador", "fan"},
		"ventilador":  {"ventilador", "fan"},
		"ceiling fan": {"ventilador de techo", "ceiling fan"},
		// Alfombras
		"alfombra": {"alfombra", "rug"},
		"rug":      {"alfombra", "rug"},
		"carpet":   {"alfombra", "carpet"},
		"mat":      {"tapete", "mat"},
		// Cortinas
		"cortina": {"cortina", "curtain"},
		"curtain": {"cortina", "curtain"},
		"drape":   {"cortina", "drape"},
		"blind":   {"persiana", "blind"},
		// Iluminación
		"lampara":  {"lampara", "lamp"},
		"lamp":     {"lampara", "lamp"},
		"light":    {"luz", "light"},
		"lighting": {"iluminacion", "lighting"},
		// Seguridad
		"detector": {"detector", "detector"},
		"sensor":   {"sensor", "sensor"},
		"alarm":    {"alarma", "alarm"},
		// Cerraduras
		"cerradura": {"cerradura", "lock"},
		"lock":      {"cerradura", "lock"},
		"latch":     {"pestillo", "latch"},
		// Bisagras
		"bisagra": {"bisagra", "hinge"},
		"hinge":   {"bisagra", "hinge"},
		// Azulejos
		"cenefa":  {"cenefa", "tile border"},
		"azulejo": {"azulejo", "tile"},
		"tile":    {"azulejo", "tile"},
		"ceramic": {"ceramica", "ceramic"},
		// Tornillería
		"tornillo": {"tornillo", "screw"},
		"screw":    {"tornillo", "screw"},
		"bolt":     {"perno", "bolt"},
		"fastener": {"sujetador", "fastener"},
		"tuerca":   {"tuerca", "nut"},
		"nut":      {"tuerca", "nut"},
		"washer":   {"arandela", "washer"},
		// Electrodomésticos
		"estufa":  {"estufa", "stove"},
		"stove":   {"estufa", "stove"},
		"cooktop": {"cocina", "cooktop"},
		"range":   {"estufa", "range"},
		// Golosinas y dulces
		"peanut":          {"mani", "peanut"},
		"mani":            {"mani", "peanut"},
		"cacahuate":       {"cacahuate", "peanut"},
		"dispensador":     {"dispensador", "dispenser"},
		"dispenser":       {"dispensador", "dispenser"},
		"disp":            {"dispensador", "dispenser"},
		"candy dispenser": {"dispensador de dulces", "candy dispenser"},
		"chocolate":       {"chocolate", "chocolate"},
		"candy":           {"dulce", "candy"},
		"dulce":           {"dulce", "candy"},
		"golosina":        {"golosina", "candy"},
		"galleta":         {"galleta", "cookie"},
		"cookie":          {"galleta", "cookie"},
		"biscuit":         {"galleta", "biscuit"},
		"cereal":          {"cereal", "cereal"},
		"snack":           {"snack", "snack"},
		"beverage":        {"bebida", "beverage"},
		// Electrónica y celulares
		"celular":     {"celular", "smartphone"},
		"smartphone":  {"celular", "smartphone"},
		"phone":       {"celular", "phone"},
		"tablet":      {"tableta", "tablet"},
		"televisor":   {"televisor", "television"},
		"television":  {"televisor", "television"},
		"tv":          {"televisor", "tv"},
		"laptop":      {"laptop", "laptop"},
		"notebook":    {"notebook", "notebook"},
		"computadora": {"computadora", "computer"},
		"computer":    {"computadora", "computer"},
		"audifonos":   {"audifonos", "headphones"},
		"auriculares": {"auriculares", "headphones"},
		"headphones":  {"audifonos", "headphones"},
		"earbuds":     {"audifonos", "earbuds"},
		"bocina":      {"bocina", "speaker"},
		"parlante":    {"parlante", "speaker"},
		"speaker":     {"bocina", "speaker"},
		"camara":      {"camara", "camera"},
		"camera":      {"camara", "camera"},
		// Mascotas
		"bocadillo": {"bocadillo para perros", "dog treat"},
		"dog treat": {"bocadillo para perros", "dog treat"},
		"cat treat": {"bocadillo para gatos", "cat treat"},
		"perro":     {"perro", "dog"},
		"gato":      {"gato", "cat"},
		"mascota":   {"mascota", "pet"},
		"pet":       {"mascota", "pet"},
	}

	lowerCat := strings.ToLower(category)
	if trans, ok := translations[lowerCat]; ok {
		return trans.es, trans.en
	}
	// Si no hay traducción, devolver la misma categoría
	return category, category
}

// extractKeySpecsFromName extrae especificaciones clave del nombre del producto (material, color, tamaño)
func extractKeySpecsFromName(productName string) []string {
	var specs []string
	lowerName := strings.ToLower(productName)

	// Materiales
	materials := []string{"acrilico", "acrylic", "metal", "chrome", "brass", "bronze", "steel", "stainless", "plastic", "wood", "glass", "ceramic"}
	for _, mat := range materials {
		if strings.Contains(lowerName, mat) {
			specs = append(specs, mat)
			break // Solo un material
		}
	}

	// Colores/acabados
	colors := []string{"transparente", "transparent", "clear", "white", "black", "silver", "gold", "polished", "brushed", "matte", "satin"}
	for _, color := range colors {
		if strings.Contains(lowerName, color) {
			specs = append(specs, color)
			break // Solo un color
		}
	}

	return specs
}

// extractSmartKeywords extrae palabras clave significativas del nombre del producto
// de forma inteligente, filtrando palabras comunes, medidas y números irrelevantes
// Ejemplo: "ABC123 Widget Pro 500ml pack x 24" -> ["ABC123", "Widget", "Pro"]
func extractSmartKeywords(productName string) []string {
	// Palabras a ignorar (stop words + medidas + cantidades)
	stopWords := map[string]bool{
		// Artículos y preposiciones en español
		"de": true, "del": true, "la": true, "el": true, "las": true, "los": true,
		"en": true, "con": true, "para": true, "por": true, "sin": true,
		"a": true, "al": true, "un": true, "una": true, "unos": true, "unas": true,
		// Artículos y preposiciones en inglés
		"the": true, "an": true, "of": true, "on": true,
		"at": true, "to": true, "for": true, "with": true, "by": true, "from": true,
		"and": true, "or": true, "but": true,
		// Palabras de cantidad/empaque
		"pack": true, "pck": true, "pk": true, "paq": true, "paquete": true,
		"set": true, "kit": true, "combo": true,
		"pc": true, "pcs": true, "pz": true, "pza": true, "pieza": true, "piezas": true,
		"unidad": true, "unidades": true, "und": true,
		"caja": true, "cajas": true, "box": true, "boxes": true,
		"bolsa": true, "bag": true, "bags": true,
		// Unidades de medida
		"ml": true, "lt": true, "lts": true, "l": true, "oz": true, "fl": true,
		"g": true, "gr": true, "kg": true, "lb": true, "lbs": true,
		"mm": true, "cm": true, "m": true, "in": true, "inch": true, "ft": true,
		// Conectores
		"x": true, "-": true, "/": true, "&": true, "+": true,
		// Descriptores genéricos
		"mini": true, "maxi": true, "grande": true, "pequeño": true,
		"new": true, "nuevo": true, "original": true,
		"disp": true, "dispensador": true, "dispenser": true,
	}

	// Limpiar el nombre
	cleanName := strings.ReplaceAll(productName, "\"", "")
	cleanName = strings.ReplaceAll(cleanName, "'", "")
	words := strings.Fields(cleanName)

	var keywords []string
	seenWords := make(map[string]bool)

	for _, word := range words {
		// Limpiar la palabra
		cleanWord := strings.Trim(word, ".,;:!?()[]{}\"'")
		lowerWord := strings.ToLower(cleanWord)

		// Ignorar palabras vacías o muy cortas
		if len(cleanWord) < 2 {
			continue
		}

		// Ignorar stop words
		if stopWords[lowerWord] {
			continue
		}

		// Ignorar números puros que parecen cantidades (1, 12, 24, 100, etc.)
		if regexp.MustCompile(`^\d+$`).MatchString(cleanWord) {
			// Pero mantener si parece un modelo (ej: "500" en "Pro 500")
			if len(cleanWord) <= 3 {
				continue // Ignorar números pequeños como cantidades
			}
		}

		// Ignorar patrones de medida (12x24, 500ml, 1.5lt, etc.)
		if regexp.MustCompile(`^\d+[x×]\d+$`).MatchString(lowerWord) ||
			regexp.MustCompile(`^\d+(\.\d+)?(ml|lt|lts|l|oz|g|gr|kg|lb|mm|cm|m|in|ft)$`).MatchString(lowerWord) {
			continue
		}

		// Evitar duplicados
		if seenWords[lowerWord] {
			continue
		}
		seenWords[lowerWord] = true

		// Agregar la palabra (mantener capitalización original si parece marca/modelo)
		if regexp.MustCompile(`^[A-Z]`).MatchString(cleanWord) || regexp.MustCompile(`[A-Z0-9]{2,}`).MatchString(cleanWord) {
			keywords = append(keywords, cleanWord)
		} else {
			keywords = append(keywords, cleanWord)
		}
	}

	// Limitar a las 4 palabras más significativas
	if len(keywords) > 4 {
		keywords = keywords[:4]
	}

	fmt.Printf("🧠 Smart keywords extracted from '%s': %v\n", productName, keywords)
	return keywords
}

// extractKnownBrandFromName extrae marcas conocidas del nombre del producto
// Útil para productos donde la marca está en el nombre pero el campo de marca es "GENÉRICO"
// Ejemplo: "M&m mini disp 12 x 24" -> "M&M"
func extractKnownBrandFromName(productName string) string {
	lowerName := strings.ToLower(productName)

	// Mapeo de patrones de marca a nombre de búsqueda optimizado
	// El valor es el nombre que funciona mejor en motores de búsqueda
	knownBrands := map[string]string{
		// Chocolates y dulces
		"m&m":       "M&M",
		"m&ms":      "M&M",
		"snickers":  "Snickers",
		"twix":      "Twix",
		"milky way": "Milky Way",
		"skittles":  "Skittles",
		"starburst": "Starburst",
		"hershey":   "Hershey",
		"reese":     "Reese's",
		"kit kat":   "Kit Kat",
		"kitkat":    "Kit Kat",
		// Galletas
		"oreo":       "Oreo",
		"chips ahoy": "Chips Ahoy",
		"ritz":       "Ritz",
		"nabisco":    "Nabisco",
		// Snacks
		"pringles": "Pringles",
		"doritos":  "Doritos",
		"cheetos":  "Cheetos",
		"lays":     "Lay's",
		"lay's":    "Lay's",
		"ruffles":  "Ruffles",
		"tostitos": "Tostitos",
		"fritos":   "Fritos",
		// Bebidas
		"coca cola": "Coca-Cola",
		"coca-cola": "Coca-Cola",
		"pepsi":     "Pepsi",
		"sprite":    "Sprite",
		"fanta":     "Fanta",
		"gatorade":  "Gatorade",
		"powerade":  "Powerade",
		"red bull":  "Red Bull",
		"monster":   "Monster Energy",
		// Cereales
		"cheerios":       "Cheerios",
		"frosted flakes": "Frosted Flakes",
		"corn flakes":    "Corn Flakes",
		"froot loops":    "Froot Loops",
		"lucky charms":   "Lucky Charms",
		// Otras marcas conocidas
		"nutella": "Nutella",
		"nestle":  "Nestle",
		"kraft":   "Kraft",
		"kellogg": "Kellogg's",
		// Mascotas
		"pedigree":    "Pedigree",
		"purina":      "Purina",
		"whiskas":     "Whiskas",
		"dog chow":    "Dog Chow",
		"royal canin": "Royal Canin",
		// Electrónica
		"galaxy":   "Samsung Galaxy",
		"iphone":   "iPhone",
		"xiaomi":   "Xiaomi",
		"huawei":   "Huawei",
		"motorola": "Motorola",
		"nokia":    "Nokia",
		"lg":       "LG",
	}

	// Buscar coincidencias (priorizar patrones más largos primero)
	var foundBrand string
	var foundPatternLength int

	for pattern, brandName := range knownBrands {
		if strings.Contains(lowerName, pattern) {
			// Preferir patrones más largos (más específicos)
			if len(pattern) > foundPatternLength {
				foundBrand = brandName
				foundPatternLength = len(pattern)
			}
		}
	}

	return foundBrand
}

// extractModelFromName extrae el modelo/serie del nombre del producto
// Ejemplo: "ABANICO DEMPSEY 52" BRUSH N." -> "DEMPSEY 52"
// Ejemplo: "VENTILADOR HUNTER BUILDER PLUS 52" -> "BUILDER PLUS 52"
func extractModelFromName(productName string) string {
	// Limpiar el nombre
	cleanName := strings.ReplaceAll(productName, "\"", "")
	cleanName = strings.TrimSpace(cleanName)

	// Palabras a ignorar (categorías y palabras comunes)
	ignoreWords := map[string]bool{
		"ABANICO": true, "VENTILADOR": true, "FAN": true, "CEILING": true,
		"LAMPARA": true, "LAMP": true, "LIGHT": true, "LED": true,
		"TIRADOR": true, "HANDLE": true, "PULL": true, "KNOB": true,
		"CORTINA": true, "CURTAIN": true, "DRAPE": true,
		"ALFOMBRA": true, "RUG": true, "CARPET": true,
		"ESTUFA": true, "STOVE": true, "RANGE": true,
		"REFRIGERADOR": true, "REFRIGERATOR": true,
		"AIRE": true, "ACONDICIONADO": true, "AIR": true, "CONDITIONER": true,
		"PACK": true, "SET": true, "PC": true, "PCS": true,
		"DE": true, "CON": true, "PARA": true, "EN": true,
		"WITH": true, "FOR": true, "AND": true, "THE": true,
		"BRUSH": true, "BRUSHED": true, "POLISHED": true, "MATTE": true,
		"WHITE": true, "BLACK": true, "CHROME": true, "NICKEL": true,
		"N.": true, "N": true, // Para abreviaciones como "BRUSH N."
	}

	words := strings.Fields(strings.ToUpper(cleanName))

	// Buscar patrón: PALABRA(S) EN MAYÚSCULAS + NÚMERO (ej: "DEMPSEY 52", "BUILDER PLUS 52")
	var modelParts []string
	foundModelWord := false

	for _, word := range words {
		// Ignorar palabras de categoría
		if ignoreWords[word] {
			// Si ya encontramos parte del modelo, detener
			if foundModelWord && len(modelParts) > 0 {
				break
			}
			continue
		}

		// Si es un número (posible tamaño del modelo como "52")
		if regexp.MustCompile(`^\d+$`).MatchString(word) {
			if len(modelParts) > 0 {
				modelParts = append(modelParts, word)
				break // Número encontrado, terminar
			}
			continue
		}

		// Si es una palabra alfanumérica que podría ser modelo
		if len(word) >= 2 && regexp.MustCompile(`^[A-Z0-9-]+$`).MatchString(word) {
			modelParts = append(modelParts, word)
			foundModelWord = true
		}
	}

	if len(modelParts) > 0 {
		model := strings.Join(modelParts, " ")
		fmt.Printf("🔍 Extracted model from name: '%s' -> '%s'\n", productName, model)
		return model
	}

	return ""
}

// isValidPartNumber filtra UUIDs y códigos internos del sistema
func isValidPartNumber(partNumber string) bool {
	// Filtrar UUIDs (formato: 8-4-4-4-12 caracteres hexadecimales)
	uuidPattern := regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$`)
	if uuidPattern.MatchString(strings.ToUpper(partNumber)) {
		return false
	}

	// Filtrar códigos muy cortos
	if len(partNumber) < 3 {
		return false
	}

	// FILTRAR SKUs INTERNOS del sistema (patrón: letra + 3 dígitos + guión + 6 dígitos)
	// Ejemplos: M021-000275, A020-123456, B001-000001
	internalSkuPattern := regexp.MustCompile(`^[A-Z]\d{3}-\d{6}$`)
	if internalSkuPattern.MatchString(strings.ToUpper(partNumber)) {
		fmt.Printf("⚠️ Rejected internal SKU pattern: %s\n", partNumber)
		return false
	}

	// CRITERIOS FLEXIBLES para números de manufactura:
	hasLetter := regexp.MustCompile(`[a-zA-Z]`).MatchString(partNumber)
	hasNumber := regexp.MustCompile(`[0-9]`).MatchString(partNumber)

	// Caso 1: Números de manufactura alfanuméricos tradicionales (letras + números)
	if hasLetter && hasNumber {
		return true
	}

	// Caso 2: Números puros de 4-8 dígitos (ej: "43305", "12345") - común en hardware
	if hasNumber && !hasLetter && len(partNumber) >= 4 && len(partNumber) <= 8 {
		return true
	}

	// Caso 3: Códigos con guiones (ej: "1/2-13", "M8-1.25") - pero NO si es SKU interno
	if strings.Contains(partNumber, "-") || strings.Contains(partNumber, "/") {
		return true
	}

	return false
}

// extractProductKeywords extrae palabras clave relevantes del nombre del producto
func extractProductKeywords(productName string) []string {
	var keywords []string
	lowerName := strings.ToLower(productName)
	words := strings.Fields(lowerName)

	// Construir combinaciones inteligentes
	category := extractProductCategory(productName)

	// Filtrar palabras irrelevantes (artículos, preposiciones, etc.)
	stopWords := map[string]bool{
		"de": true, "del": true, "la": true, "el": true, "en": true, "con": true, "para": true,
		"a": true, "an": true, "the": true, "and": true, "or": true, "but": true, "in": true,
		"on": true, "at": true, "to": true, "for": true, "of": true, "with": true, "by": true,
		"x": true, "-": true, "/": true, "\"": true, "'": true,
	}

	// PRIORIDAD 1: Categoría del producto (más importante)
	if category != "" {
		keywords = append(keywords, category)
	}

	// PRIORIDAD 2: Extraer palabras significativas del nombre
	var significantWords []string
	for _, word := range words {
		cleanWord := strings.Trim(word, "\"',-./")
		if len(cleanWord) > 2 && !stopWords[cleanWord] && cleanWord != category {
			significantWords = append(significantWords, cleanWord)
		}
	}

	// PRIORIDAD 3: Palabras que indican material, color, o características
	for _, word := range significantWords {
		if isAttribute(word) {
			keywords = append(keywords, word)
		}
	}

	// PRIORIDAD 4: Palabras descriptivas largas (≥4 caracteres)
	for _, word := range significantWords {
		if len(word) >= 4 && !isAttribute(word) {
			keywords = append(keywords, word)
		}
	}

	// PRIORIDAD 5: Combinaciones útiles si tenemos categoría
	if category != "" && len(significantWords) > 0 {
		// Combinar categoría con primera palabra significativa
		combo := fmt.Sprintf("%s %s", category, significantWords[0])
		keywords = append(keywords, combo)
	}

	// FALLBACK: Si no hay palabras clave útiles, usar nombre completo limpio
	if len(keywords) == 0 {
		cleanName := strings.ReplaceAll(productName, "\"", "")
		cleanName = strings.TrimSpace(cleanName)
		if cleanName != "" {
			keywords = append(keywords, cleanName)
		}
	}

	// Limitar a máximo 5 keywords para evitar términos excesivos
	if len(keywords) > 5 {
		keywords = keywords[:5]
	}

	return keywords
}

// isAttribute determina si una palabra es un atributo relevante (material, color, etc.)
func isAttribute(word string) bool {
	attributes := []string{
		"white", "black", "bronze", "chrome", "steel", "brass", "silver", "gold",
		"plastic", "metal", "wood", "glass", "acrylic", "vinyl",
		"blackout", "sheer", "thermal", "insulated",
		"pack", "set", "pair", "pc", "piece",
		"arcilla", "clay", "ceramic", "porcelain", "stone",
		"qaqa", "decorative", "ornamental",
	}

	for _, attr := range attributes {
		if word == attr || strings.Contains(word, attr) {
			return true
		}
	}

	return false
}

// analyzeImageRelevance analiza qué tan relevante es una imagen para el producto específico
func analyzeImageRelevance(imageTitle, productName, manufacturerPart, brandName, productCategory, knownBrand string) int {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		// Si no hay API key, usar scoring básico por palabras clave
		return basicRelevanceScoreWithCategory(imageTitle, productName, productCategory, knownBrand)
	}

	// Construir información de marca efectiva (priorizar marca conocida detectada en nombre)
	effectiveBrand := brandName
	if knownBrand != "" && (brandName == "" || isGenericBrand(brandName)) {
		effectiveBrand = knownBrand
	}

	// Construir información de categoría
	categoryInfo := ""
	if productCategory != "" {
		catES, catEN := getCategoryTranslations(productCategory)
		categoryInfo = fmt.Sprintf("\n- Categoría del producto: %s / %s", catES, catEN)
	}

	// Prompt específico para análisis de relevancia de imágenes
	prompt := fmt.Sprintf(`Analiza la relevancia de esta imagen para el producto específico solicitado.

PRODUCTO BUSCADO:
- Nombre: %s
- Marca: %s
- Número de manufactura: %s%s

TÍTULO/DESCRIPCIÓN DE LA IMAGEN:
"%s"

INSTRUCCIONES DE ANÁLISIS:
1. PRIMERO verifica si la imagen es de la MISMA CATEGORÍA que el producto buscado
2. Si las categorías NO coinciden, el score DEBE ser 0-1
3. Verifica coincidencia EXACTA de especificaciones técnicas (medidas, colores, materiales)
4. Evalúa si es el MISMO producto o uno similar/genérico
5. Prioriza coincidencias de número de manufactura, medidas exactas, características específicas
6. Penaliza productos similares pero con especificaciones diferentes

REGLA DE CATEGORÍA:
- La categoría del producto y la categoría de la imagen DEBEN coincidir para scores >= 4
- Si la imagen es claramente de una categoría diferente al producto, score = 0
- Patrones como "NxM" pueden ser cantidades (ej: 9 unidades x 36g) o medidas físicas según el contexto del producto

CRITERIOS DE PUNTUACIÓN:
- 10: Producto idéntico (número de manufactura exacto Y marca exacta)
- 8-9: Muy probable que sea el mismo producto (marca correcta + especificaciones exactas)
- 6-7: Producto de la misma familia/serie con especificaciones similares Y MISMA MARCA
- 4-5: Producto de la misma categoría y marca correcta
- 2-3: Producto relacionado de la misma marca
- 0-1: Producto de categoría similar pero DIFERENTE MARCA o no relacionado

CRÍTICO: La marca DEBE coincidir para scores ≥ 4. Si la marca del producto buscado es MABE y la imagen es de LG/Samsung/otra marca, el score DEBE ser ≤ 3.

IMPORTANTE: 
- Para electrodomésticos (aires acondicionados, refrigeradores), la marca es FUNDAMENTAL. Un aire MABE NO puede tener score alto si la imagen es de LG.
- Para productos de hardware (tornillos, tuercas, conectores), si el número de manufactura coincide EXACTAMENTE pero la marca es diferente, dar score 3-4 (podría ser el mismo producto vendido por diferentes distribuidores).
- Si tanto el número de manufactura como la marca coinciden exactamente, dar score 8-10.

RESPONDE SOLO CON UN NÚMERO DEL 0 AL 10:`,
		productName,
		effectiveBrand,
		manufacturerPart,
		categoryInfo,
		imageTitle)

	request := ClaudeRequest{
		Model: "claude-haiku-4-5-20251001",
		Messages: []Message{
			{
				Role:    "user",
				Content: prompt,
			},
		},
		MaxTokens: 50, // Solo necesitamos un número
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		return basicRelevanceScore(imageTitle, productName)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewBuffer(requestBody))
	if err != nil {
		return basicRelevanceScore(imageTitle, productName)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := client.Do(req)
	if err != nil {
		return basicRelevanceScore(imageTitle, productName)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return basicRelevanceScore(imageTitle, productName)
	}

	var claudeResponse ClaudeResponse
	if err := json.NewDecoder(resp.Body).Decode(&claudeResponse); err != nil {
		return basicRelevanceScore(imageTitle, productName)
	}

	if len(claudeResponse.Content) == 0 {
		return basicRelevanceScore(imageTitle, productName)
	}

	// Extraer el número de la respuesta
	responseText := strings.TrimSpace(claudeResponse.Content[0].Text)

	// Buscar el primer número en la respuesta
	re := regexp.MustCompile(`\b([0-9]|10)\b`)
	match := re.FindString(responseText)

	if match != "" {
		if score, err := strconv.Atoi(match); err == nil && score >= 0 && score <= 10 {
			return score
		}
	}

	// Fallback al scoring básico si no se puede parsear
	return basicRelevanceScore(imageTitle, productName)
}

// basicRelevanceScore scoring básico por palabras clave para productos técnicos
func basicRelevanceScore(imageTitle, productName string) int {
	if imageTitle == "" || productName == "" {
		return 0
	}

	lowerTitle := strings.ToLower(imageTitle)
	lowerProduct := strings.ToLower(productName)

	// SCORING ESPECIAL PARA NÚMEROS DE PARTE TÉCNICOS
	// Detectar si el producto es un número de parte (ej: P65015L-PC-U) o número puro (43305)
	isPartNumber := regexp.MustCompile(`^[A-Z0-9]+-[A-Z0-9-]+$`).MatchString(strings.ToUpper(productName))
	isPureNumber := regexp.MustCompile(`^\d{4,8}$`).MatchString(productName)

	if isPartNumber || isPureNumber {
		score := 0

		if isPureNumber {
			// Para números puros como "43305", buscar coincidencia exacta en el título
			if strings.Contains(lowerTitle, productName) {
				score += 5 // Puntuación alta por coincidencia exacta de número
			}
		} else {
			// Para números de parte, buscar patrones similares
			productParts := strings.Split(strings.ToUpper(productName), "-")

			for _, part := range productParts {
				if len(part) > 2 && strings.Contains(strings.ToUpper(lowerTitle), part) {
					score += 3 // Mayor peso para partes del número de manufactura
				}
			}

			// Buscar patrones similares (ej: P65015L vs P65025)
			if len(productParts) > 0 {
				mainPart := productParts[0]
				if len(mainPart) > 4 {
					// Buscar los primeros caracteres (familia del producto)
					familyPattern := mainPart[:4]
					if strings.Contains(strings.ToUpper(lowerTitle), familyPattern) {
						score += 2
					}
				}
			}
		}

		return score
	}

	// SCORING NORMAL PARA PRODUCTOS DESCRIPTIVOS
	// Extraer palabras clave importantes del nombre del producto
	productWords := strings.Fields(lowerProduct)
	var keyWords []string

	for _, word := range productWords {
		if len(word) > 2 && !isStopWord(word) {
			keyWords = append(keyWords, word)
		}
	}

	if len(keyWords) == 0 {
		return 1
	}

	// Calcular porcentaje de palabras clave que aparecen en el título
	matches := 0
	for _, keyword := range keyWords {
		if strings.Contains(lowerTitle, keyword) {
			matches++
		}
	}

	percentage := float64(matches) / float64(len(keyWords))

	// Convertir porcentaje a score 0-10
	score := int(percentage * 10)

	// Bonus por coincidencias exactas importantes
	if strings.Contains(lowerTitle, lowerProduct) {
		score += 2
	}

	// Limitar a máximo 10
	if score > 10 {
		score = 10
	}

	return score
}

// basicRelevanceScoreWithCategory - versión mejorada que considera categoría y marca conocida
func basicRelevanceScoreWithCategory(imageTitle, productName, productCategory, knownBrand string) int {
	if imageTitle == "" || productName == "" {
		return 0
	}

	lowerTitle := strings.ToLower(imageTitle)

	// Si tenemos categoría, verificar si aparece en el título de la imagen
	categoryMatch := false
	if productCategory != "" {
		catES, catEN := getCategoryTranslations(productCategory)
		categoryMatch = strings.Contains(lowerTitle, strings.ToLower(catES)) ||
			strings.Contains(lowerTitle, strings.ToLower(catEN)) ||
			strings.Contains(lowerTitle, strings.ToLower(productCategory))
	}

	// Verificar si la marca conocida aparece en el título
	brandMatch := false
	if knownBrand != "" {
		brandMatch = strings.Contains(lowerTitle, strings.ToLower(knownBrand))
	}

	// Obtener score base
	baseScore := basicRelevanceScore(imageTitle, productName)

	// Ajustar score según coincidencias de categoría y marca
	if categoryMatch && brandMatch {
		// Categoría Y marca coinciden: máxima confianza
		adjusted := baseScore + 3
		if adjusted > 10 {
			adjusted = 10
		}
		return adjusted
	} else if brandMatch {
		// Solo marca coincide: buena confianza
		adjusted := baseScore + 2
		if adjusted > 10 {
			adjusted = 10
		}
		return adjusted
	} else if categoryMatch {
		// Solo categoría coincide: confianza media
		adjusted := baseScore + 1
		if adjusted > 10 {
			adjusted = 10
		}
		return adjusted
	} else if productCategory != "" && knownBrand != "" {
		// Tenemos categoría y marca pero ninguna coincide: penalizar
		fmt.Printf("   ⚠️ No match for category '%s' or brand '%s' in: %s\n", productCategory, knownBrand, imageTitle)
		adjusted := baseScore - 2
		if adjusted < 0 {
			adjusted = 0
		}
		return adjusted
	}

	return baseScore
}

// generateSegmentedSearchTerms - NUEVA FUNCIÓN para búsqueda segmentada
// Combina múltiples campos del producto en términos de búsqueda estructurados
func generateSegmentedSearchTerms(product *Product) []string {
	var segmentedTerms []string

	// Extraer todos los campos relevantes del producto
	brand := getBrandName(product)
	manufacturerPart := getStringValue(product.ManufacturerPartNumber)
	productName := strings.TrimSpace(strings.ReplaceAll(product.Name, "\"", ""))
	sku := product.Sku
	gtin := getStringValue(product.Gtin)

	// Debug: Log de campos extraídos
	fmt.Printf("🔍 Product fields - Brand: '%s', ManufacturerPart: '%s', ProductName: '%s', SKU: '%s'\n", brand, manufacturerPart, productName, sku)
	fmt.Printf("🔍 Brand validation - IsGeneric: %v, IsUUID: %v\n", isGenericBrand(brand), isUUIDString(brand))

	// Extraer categoría y especificaciones del nombre del producto
	productCategory := extractProductCategory(productName)
	specifications := extractProductSpecifications(productName)

	// Debug: Log de categoría y especificaciones extraídas
	fmt.Printf("🔍 Product category: '%s', Specifications: %v\n", productCategory, specifications)

	// ESTRATEGIA 1: COMBINACIÓN MARCA + NÚMERO DE MANUFACTURA (Máxima precisión)
	if brand != "" && manufacturerPart != "" && isValidPartNumber(manufacturerPart) && !isUUIDString(brand) && !isGenericBrand(brand) {
		fmt.Printf("✅ Strategy 1: Brand + ManufacturerPart combination\n")

		// 1.1: Marca + Número de manufactura (combinación más precisa y segura)
		// IMPORTANTE: Siempre buscar con marca primero para evitar que el número solo
		// devuelva resultados de otros productos (ej: W3-183351 → Audioengine W3)
		brandPart := fmt.Sprintf("%s %s", brand, manufacturerPart)
		segmentedTerms = append(segmentedTerms, brandPart)
		fmt.Printf("   Added: %s\n", brandPart)

		// 1.2: Nombre descriptivo del producto (IMPORTANTE: buscar temprano para que no se pierda
		// por la condición de parada. El nombre completo como "Celular Galaxy A12 64gb negro"
		// retorna el producto real, mientras que solo el part number retorna repuestos/partes)
		if productName != "" && !strings.EqualFold(productName, manufacturerPart) {
			// Si el nombre no contiene la marca, agregarla
			if !strings.Contains(strings.ToLower(productName), strings.ToLower(brand)) {
				brandName := fmt.Sprintf("%s %s", brand, productName)
				segmentedTerms = append(segmentedTerms, brandName)
				fmt.Printf("   Added (brand+name): %s\n", brandName)
			} else {
				segmentedTerms = append(segmentedTerms, productName)
				fmt.Printf("   Added (product name): %s\n", productName)
			}
		}

		// 1.3: Número de manufactura solo (puede dar resultado exacto, pero también ruido)
		segmentedTerms = append(segmentedTerms, manufacturerPart)
		fmt.Printf("   Added: %s\n", manufacturerPart)

		// 1.4: Si tenemos categoría, triple combinación
		if productCategory != "" {
			triple := fmt.Sprintf("%s %s %s", brand, manufacturerPart, productCategory)
			segmentedTerms = append(segmentedTerms, triple)
			fmt.Printf("   Added: %s\n", triple)
		}

		// 1.5: Términos comerciales usando la CATEGORÍA REAL del producto
		if productCategory != "" {
			// Obtener traducciones de la categoría detectada
			catES, catEN := getCategoryTranslations(productCategory)

			// Marca + categoría (genérico pero relevante)
			commercial := fmt.Sprintf("%s %s", brand, productCategory)
			segmentedTerms = append(segmentedTerms, commercial)
			fmt.Printf("   Added (brand+category): %s\n", commercial)

			// 1.6: Términos con categoría en INGLÉS para e-commerce internacional
			if catEN != "" && catEN != productCategory {
				amazonStyleEN := fmt.Sprintf("%s %s %s", brand, manufacturerPart, catEN)
				segmentedTerms = append(segmentedTerms, amazonStyleEN)
				fmt.Printf("   Added (brand+part+catEN): %s\n", amazonStyleEN)
			}

			// 1.7: Términos con categoría en ESPAÑOL para cobertura regional
			if catES != "" && catES != catEN {
				amazonStyleES := fmt.Sprintf("%s %s %s", brand, manufacturerPart, catES)
				segmentedTerms = append(segmentedTerms, amazonStyleES)
				fmt.Printf("   Added (brand+part+catES): %s\n", amazonStyleES)
			}
		} else {
			// Si no hay categoría detectada, usar términos genéricos pero NO de ferretería
			// Solo agregar marca + part number + "product" como fallback neutral
			neutralTerm := fmt.Sprintf("%s %s product", brand, manufacturerPart)
			segmentedTerms = append(segmentedTerms, neutralTerm)
			fmt.Printf("   Added (neutral fallback): %s\n", neutralTerm)
		}
	} else {
		fmt.Printf("❌ Strategy 1 skipped - Brand: '%s', ManufacturerPart: '%s', ValidPart: %v, IsUUID: %v, IsGeneric: %v\n",
			brand, manufacturerPart, isValidPartNumber(manufacturerPart), isUUIDString(brand), isGenericBrand(brand))

		// ESTRATEGIA 1B: PRODUCTOS CON MARCA VÁLIDA PERO SIN NÚMERO DE MANUFACTURA
		// Usar: Brand + Model (extraído del nombre) + Category
		if brand != "" && !isUUIDString(brand) && !isGenericBrand(brand) && productName != "" {
			fmt.Printf("✅ Strategy 1B: Brand + Model + Category (no valid part number)\n")

			// Extraer modelo del nombre del producto (ej: "DEMPSEY 52" de "ABANICO DEMPSEY 52")
			model := extractModelFromName(productName)

			// Obtener traducciones de la categoría
			var catES, catEN string
			if productCategory != "" {
				catES, catEN = getCategoryTranslations(productCategory)
				fmt.Printf("   Category translations - ES: '%s', EN: '%s'\n", catES, catEN)
			}

			// 1B.1: MÁXIMA PRIORIDAD - Brand + Model + Category EN
			// Ejemplo: "Hunter Dempsey 52 ceiling fan"
			if model != "" && catEN != "" {
				brandModelCat := fmt.Sprintf("%s %s %s", brand, model, catEN)
				segmentedTerms = append(segmentedTerms, brandModelCat)
				fmt.Printf("   [PRIORITY 1] Brand + Model + Category EN: %s\n", brandModelCat)
			}

			// 1B.2: Brand + Model (sin categoría)
			// Ejemplo: "Hunter Dempsey 52"
			if model != "" {
				brandModel := fmt.Sprintf("%s %s", brand, model)
				segmentedTerms = append(segmentedTerms, brandModel)
				fmt.Printf("   [PRIORITY 2] Brand + Model: %s\n", brandModel)
			}

			// 1B.3: Brand + Full Product Name + Category EN
			// Ejemplo: "Hunter ABANICO DEMPSEY 52 ceiling fan"
			if catEN != "" && !strings.Contains(strings.ToLower(productName), strings.ToLower(brand)) {
				brandNameCat := fmt.Sprintf("%s %s %s", brand, productName, catEN)
				segmentedTerms = append(segmentedTerms, brandNameCat)
				fmt.Printf("   [PRIORITY 3] Brand + Name + Category EN: %s\n", brandNameCat)
			}

			// 1B.4: Brand + Category EN (más genérico pero efectivo)
			// Ejemplo: "Hunter ceiling fan"
			if catEN != "" {
				brandCat := fmt.Sprintf("%s %s", brand, catEN)
				segmentedTerms = append(segmentedTerms, brandCat)
				fmt.Printf("   [PRIORITY 4] Brand + Category EN: %s\n", brandCat)
			}

			// 1B.5: Model + Category EN (sin marca, para cobertura adicional)
			// Ejemplo: "Dempsey 52 ceiling fan"
			if model != "" && catEN != "" {
				modelCat := fmt.Sprintf("%s %s", model, catEN)
				segmentedTerms = append(segmentedTerms, modelCat)
				fmt.Printf("   [PRIORITY 5] Model + Category EN: %s\n", modelCat)
			}

			// 1B.6: Brand + Category ES (cobertura regional)
			// Ejemplo: "Hunter ventilador de techo"
			if catES != "" && catES != catEN {
				brandCatES := fmt.Sprintf("%s %s", brand, catES)
				segmentedTerms = append(segmentedTerms, brandCatES)
				fmt.Printf("   [PRIORITY 6] Brand + Category ES: %s\n", brandCatES)
			}
		}
	}

	// ESTRATEGIA 2: TÉRMINOS COMERCIALES BASADOS EN ESPECIFICACIONES DEL PRODUCTO
	// Solo ejecutar si tenemos marca + manufacturerPart válidos Y hay especificaciones extraídas
	if brand != "" && manufacturerPart != "" && isValidPartNumber(manufacturerPart) && !isUUIDString(brand) && !isGenericBrand(brand) {
		// Extraer especificaciones reales del producto (material, color, etc.)
		productSpecs := extractKeySpecsFromName(productName)

		if len(productSpecs) > 0 {
			fmt.Printf("✅ Strategy 2: Adding terms with REAL product specs: %v\n", productSpecs)
			// Usar especificaciones REALES del producto, no términos genéricos de ferretería
			for _, spec := range productSpecs {
				specTerm := fmt.Sprintf("%s %s %s", brand, manufacturerPart, spec)
				segmentedTerms = append(segmentedTerms, specTerm)
				fmt.Printf("   Added with real spec: %s\n", specTerm)
			}
		} else if productCategory != "" {
			// Si no hay especificaciones, usar categoría detectada
			fmt.Printf("✅ Strategy 2: No specs found, using category '%s'\n", productCategory)
			catES, catEN := getCategoryTranslations(productCategory)
			if catEN != "" {
				catTerm := fmt.Sprintf("%s %s %s", brand, manufacturerPart, catEN)
				segmentedTerms = append(segmentedTerms, catTerm)
				fmt.Printf("   Added with category EN: %s\n", catTerm)
			}
			if catES != "" && catES != catEN {
				catTermES := fmt.Sprintf("%s %s %s", brand, manufacturerPart, catES)
				segmentedTerms = append(segmentedTerms, catTermES)
				fmt.Printf("   Added with category ES: %s\n", catTermES)
			}
		}
		// NOTA: Ya NO añadimos "stainless steel", "cap screw", "bolt", "hardware" de forma hardcodeada
	}

	// ESTRATEGIA 3: MARCA + PRODUCTO (Para productos sin número de manufactura válido)
	if productName != "" && brand != "" && !isUUIDString(brand) && !isGenericBrand(brand) {
		// SIEMPRE priorizar marca + nombre (primer término)
		if !strings.Contains(strings.ToLower(productName), strings.ToLower(brand)) {
			segmentedTerms = append(segmentedTerms, fmt.Sprintf("%s %s", brand, productName))
		}

		// Término secundario: Nombre completo del producto solo
		segmentedTerms = append(segmentedTerms, productName)

		// Si hay número de manufactura válido, combinar con nombre
		if manufacturerPart != "" && isValidPartNumber(manufacturerPart) {
			segmentedTerms = append(segmentedTerms, fmt.Sprintf("%s %s", productName, manufacturerPart))
		}
	}

	// ESTRATEGIA 4: PRODUCTOS SIN MARCA VÁLIDA (Combinar número de manufactura + contexto del producto)
	if productName != "" && (brand == "" || isUUIDString(brand) || isGenericBrand(brand)) {
		fmt.Printf("✅ Strategy 4: No valid brand - analyzing product name for known brands first\n")

		// Extraer especificaciones clave del nombre (material, color)
		keySpecs := extractKeySpecsFromName(productName)
		fmt.Printf("   Extracted key specs: %v\n", keySpecs)

		// Obtener traducciones de la categoría
		var catES, catEN string
		if productCategory != "" {
			catES, catEN = getCategoryTranslations(productCategory)
			fmt.Printf("   Category translations - ES: '%s', EN: '%s'\n", catES, catEN)
		}

		// DETECTAR MARCAS CONOCIDAS EN EL NOMBRE DEL PRODUCTO PRIMERO
		// Esto es CRÍTICO para productos como M&M, Oreo, etc. donde la marca está en el nombre
		// Si detectamos una marca conocida, DEBE ser la PRIMERA búsqueda, no el nombre completo
		knownBrandInName := extractKnownBrandFromName(productName)
		if knownBrandInName != "" {
			fmt.Printf("   🎯 Found known brand in product name: '%s'\n", knownBrandInName)
		}

		// 4.0: MÁXIMA PRIORIDAD - Número de manufactura CON CONTEXTO
		// El número de manufactura puede identificar el producto exacto en e-commerce.
		// Si los resultados son irrelevantes, el filtro de relevancia los descartará.
		if manufacturerPart != "" && isValidPartNumber(manufacturerPart) && catEN != "" {
			// Número + categoría en inglés (mejor para e-commerce)
			partWithCatEN := fmt.Sprintf("%s %s", manufacturerPart, catEN)
			segmentedTerms = append(segmentedTerms, partWithCatEN)
			fmt.Printf("   [PRIORITY 0] Part + category EN: %s\n", partWithCatEN)

			// Número + especificaciones + categoría (más específico)
			if len(keySpecs) > 0 {
				specsStr := strings.Join(keySpecs, " ")
				partWithSpecs := fmt.Sprintf("%s %s %s", manufacturerPart, specsStr, catEN)
				segmentedTerms = append(segmentedTerms, partWithSpecs)
				fmt.Printf("   [PRIORITY 0b] Part + specs + category: %s\n", partWithSpecs)
			}

			// Número + categoría en español
			if catES != "" && catES != catEN {
				partWithCatES := fmt.Sprintf("%s %s", manufacturerPart, catES)
				segmentedTerms = append(segmentedTerms, partWithCatES)
				fmt.Printf("   [PRIORITY 0c] Part + category ES: %s\n", partWithCatES)
			}
		}

		// 4.1: Marca conocida o nombre del producto
		if knownBrandInName != "" && productCategory != "" {
			// Marca conocida + categoría ORIGINAL
			brandCatOriginal := fmt.Sprintf("%s %s", knownBrandInName, productCategory)
			segmentedTerms = append(segmentedTerms, brandCatOriginal)
			fmt.Printf("   [PRIORITY 1] Known brand + category: %s\n", brandCatOriginal)

			// Solo la marca conocida como segundo término
			segmentedTerms = append(segmentedTerms, knownBrandInName)
			fmt.Printf("   [PRIORITY 2] Known brand only: %s\n", knownBrandInName)

			// Nombre completo del producto como tercer término (respaldo)
			segmentedTerms = append(segmentedTerms, productName)
			fmt.Printf("   [PRIORITY 3] Full product name: %s\n", productName)
		} else if knownBrandInName != "" {
			// Si hay marca conocida pero no categoría
			segmentedTerms = append(segmentedTerms, knownBrandInName)
			fmt.Printf("   [PRIORITY 1] Known brand only (no category): %s\n", knownBrandInName)

			// Nombre completo del producto como segundo término
			segmentedTerms = append(segmentedTerms, productName)
			fmt.Printf("   [PRIORITY 2] Full product name: %s\n", productName)
		} else {
			// Sin marca conocida: usar nombre completo
			segmentedTerms = append(segmentedTerms, productName)
			fmt.Printf("   [PRIORITY 1] Full product name: %s\n", productName)
		}

		// 4.2: Búsqueda descriptiva sin número de parte (cobertura adicional)
		if catEN != "" && len(keySpecs) > 0 {
			// Descripción en inglés: "clear acrylic cabinet pull"
			specsStr := strings.Join(keySpecs, " ")
			descriptiveEN := fmt.Sprintf("%s %s", specsStr, catEN)
			segmentedTerms = append(segmentedTerms, descriptiveEN)
			fmt.Printf("   [PRIORITY 5] Descriptive EN: %s\n", descriptiveEN)
		}

		// 4.3: Categoría sola (si no tenemos marca conocida)
		if catEN != "" && knownBrandInName == "" {
			segmentedTerms = append(segmentedTerms, catEN)
			fmt.Printf("   [PRIORITY 6] Category EN only: %s\n", catEN)
		}

		// 4.4: FALLBACK INTELIGENTE MEJORADO - Uso de descripciones del producto
		// Cuando no hay marca conocida ni categoría, extraer información de las descripciones
		if knownBrandInName == "" && catEN == "" {
			fmt.Printf("   🧠 Activating ENHANCED fallback with product descriptions\n")

			// 4.4.0: PRIMERO intentar extraer términos de ShortDescription (más concisa y relevante)
			shortDesc := getStringValue(product.ShortDescription)
			if shortDesc != "" && len(shortDesc) > 5 {
				cleanShortDesc := strings.TrimSpace(shortDesc)
				if len(cleanShortDesc) <= 100 { // Solo si es razonablemente corta
					segmentedTerms = append(segmentedTerms, cleanShortDesc)
					fmt.Printf("   [PRIORITY 6] Short description: %s\n", cleanShortDesc)
				} else {
					// Extraer primeras palabras significativas de la descripción
					shortDescKeywords := extractSmartKeywords(cleanShortDesc)
					if len(shortDescKeywords) > 0 {
						shortDescTerm := strings.Join(shortDescKeywords, " ")
						segmentedTerms = append(segmentedTerms, shortDescTerm)
						fmt.Printf("   [PRIORITY 6] Short description keywords: %s\n", shortDescTerm)
					}
				}
			}

			// 4.4.1: Intentar extraer términos de FullDescription si ShortDescription no ayudó
			fullDesc := getStringValue(product.FullDescription)
			if fullDesc != "" && len(fullDesc) > 10 {
				// Extraer palabras clave de la descripción completa
				fullDescKeywords := extractSmartKeywords(fullDesc)
				if len(fullDescKeywords) >= 2 {
					maxWords := 3
					if len(fullDescKeywords) < maxWords {
						maxWords = len(fullDescKeywords)
					}
					fullDescTerm := strings.Join(fullDescKeywords[:maxWords], " ")
					segmentedTerms = append(segmentedTerms, fullDescTerm)
					fmt.Printf("   [PRIORITY 6B] Full description keywords: %s\n", fullDescTerm)
				}
			}

			// 4.4.2: Extracción automática de keywords del nombre del producto
			smartKeywords := extractSmartKeywords(productName)

			if len(smartKeywords) > 0 {
				// Todas las keywords juntas
				allKeywords := strings.Join(smartKeywords, " ")
				segmentedTerms = append(segmentedTerms, allKeywords)
				fmt.Printf("   [PRIORITY 7] Smart keywords combined: %s\n", allKeywords)

				// Primera keyword sola (posible marca/modelo)
				if len(smartKeywords) >= 1 {
					segmentedTerms = append(segmentedTerms, smartKeywords[0])
					fmt.Printf("   [PRIORITY 8] First keyword (possible brand): %s\n", smartKeywords[0])
				}

				// Primeras 2 keywords (posible marca + modelo)
				if len(smartKeywords) >= 2 {
					first2 := strings.Join(smartKeywords[:2], " ")
					segmentedTerms = append(segmentedTerms, first2)
					fmt.Printf("   [PRIORITY 9] First 2 keywords: %s\n", first2)
				}
			}
		}
		// NOTA: El nombre completo del producto ya se agregó al principio con PRIORITY 0
	}

	// ESTRATEGIA 5: Manufacturer Part SOLO como último recurso (si no hay nombre)
	if manufacturerPart != "" && isValidPartNumber(manufacturerPart) && productName == "" {
		if brand != "" && !isGenericBrand(brand) && !isUUIDString(brand) {
			segmentedTerms = append(segmentedTerms, fmt.Sprintf("%s %s", brand, manufacturerPart))
		} else {
			// Solo si no hay absolutamente nada más
			segmentedTerms = append(segmentedTerms, manufacturerPart)
		}
	}

	// ESTRATEGIA 6: Brand + Product Category + Specifications (Búsqueda Específica)
	if brand != "" && productCategory != "" && !isGenericBrand(brand) && !isUUIDString(brand) {
		fmt.Printf("✅ Strategy 6: Brand + Category + Specifications\n")
		if len(specifications) > 0 {
			// Combinar categoría con las especificaciones más relevantes
			mainSpecs := specifications
			if len(specifications) > 2 {
				mainSpecs = specifications[:2] // Limitar a 2 especificaciones principales
			}
			for _, spec := range mainSpecs {
				segmentedTerms = append(segmentedTerms, fmt.Sprintf("%s %s %s", brand, spec, productCategory))
			}
		} else {
			// Brand + category sin especificaciones
			segmentedTerms = append(segmentedTerms, fmt.Sprintf("%s %s", brand, productCategory))
		}
	}

	// ESTRATEGIA 7: Product Name Segmented (Nombre Inteligentemente Segmentado)
	if productName != "" {
		fmt.Printf("✅ Strategy 7: Product Name Segmented\n")
		// Limpiar y segmentar el nombre del producto
		cleanSegments := segmentProductName(productName)

		for _, segment := range cleanSegments {
			if brand != "" && !isGenericBrand(brand) && !isUUIDString(brand) && !strings.Contains(strings.ToLower(segment), strings.ToLower(brand)) {
				// Agregar marca si no está incluida en el segmento Y la marca es válida
				brandSegment := fmt.Sprintf("%s %s", brand, segment)
				segmentedTerms = append(segmentedTerms, brandSegment)
				fmt.Printf("   Strategy 7 - Added with brand: %s\n", brandSegment)
			}
			// Agregar el segmento solo
			segmentedTerms = append(segmentedTerms, segment)
			fmt.Printf("   Strategy 7 - Added segment: %s\n", segment)
		}
	}

	// ESTRATEGIA 4: SKU/GTIN + Category (Backup con códigos alternativos)
	if productCategory != "" {
		if sku != "" && isValidPartNumber(sku) {
			segmentedTerms = append(segmentedTerms, fmt.Sprintf("%s %s", sku, productCategory))
		}
		if gtin != "" && len(gtin) > 6 && gtin != manufacturerPart && gtin != sku {
			segmentedTerms = append(segmentedTerms, fmt.Sprintf("%s %s", gtin, productCategory))
		}
	}

	// Filtrar términos duplicados y validar
	return filterAndValidateSegmentedTerms(segmentedTerms)
}

// extractProductSpecifications extrae especificaciones técnicas del nombre del producto
func extractProductSpecifications(productName string) []string {
	var specs []string
	lowerName := strings.ToLower(productName)

	// Patrones para medidas
	measurementPatterns := []string{
		`\d+x\d+`, `\d+\"\s*x\s*\d+\"`, `\d+\s*x\s*\d+`, `\d+cm`, `\d+mm`, `\d+\"`, `\d+'`,
	}

	// Buscar medidas
	for _, pattern := range measurementPatterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindAllString(lowerName, -1)
		for _, match := range matches {
			if !containsString(specs, match) {
				specs = append(specs, match)
			}
		}
	}

	// Materiales y características especiales
	materials := []string{
		"blackout", "sheer", "thermal", "insulated", "vinyl", "plastic", "metal", "wood", "glass",
		"bronze", "chrome", "steel", "brass", "silver", "gold", "white", "black",
		"arcilla", "clay", "ceramic", "porcelain", "stone",
	}

	words := strings.Fields(lowerName)
	for _, word := range words {
		for _, material := range materials {
			if strings.Contains(word, material) && !containsString(specs, material) {
				specs = append(specs, material)
			}
		}
	}

	return specs
}

// segmentProductName divide el nombre del producto en segmentos útiles para búsqueda
func segmentProductName(productName string) []string {
	var segments []string

	// Limpiar el nombre
	cleanName := strings.ReplaceAll(productName, "\"", "")
	cleanName = strings.TrimSpace(cleanName)

	words := strings.Fields(cleanName)

	// Crear segmentos de diferentes longitudes
	if len(words) >= 3 {
		// Segmento completo (máximo 4 palabras)
		if len(words) <= 4 {
			segments = append(segments, cleanName)
		} else {
			segments = append(segments, strings.Join(words[:4], " "))
		}

		// Segmento de palabras clave (primeras 3 palabras significativas)
		var keyWords []string
		for _, word := range words {
			if len(word) > 2 && !isStopWord(word) {
				keyWords = append(keyWords, word)
				if len(keyWords) == 3 {
					break
				}
			}
		}
		if len(keyWords) >= 2 {
			segments = append(segments, strings.Join(keyWords, " "))
		}
	} else if len(words) == 2 {
		segments = append(segments, cleanName)
	} else if len(words) == 1 && len(cleanName) > 3 {
		segments = append(segments, cleanName)
	}

	return segments
}

// filterAndValidateSegmentedTerms filtra duplicados y valida términos
func filterAndValidateSegmentedTerms(terms []string) []string {
	seen := make(map[string]bool)
	var validTerms []string

	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term == "" || seen[term] {
			continue
		}

		// Validar que el término sea útil para búsqueda
		if isValidSearchTerm(term) && len(term) > 2 {
			validTerms = append(validTerms, term)
			seen[term] = true
		}

		// Limitar a máximo 6 términos para mantener eficiencia
		if len(validTerms) >= 6 {
			break
		}
	}

	return validTerms
}

// Helper function para verificar si un slice contiene un elemento (específico para search_terms_ai)
func containsString(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// isUUIDString verifica si una cadena es un UUID
func isUUIDString(s string) bool {
	uuidPattern := regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$`)
	return uuidPattern.MatchString(strings.ToUpper(s))
}

// isGenericBrand verifica si una marca es genérica o no útil para búsquedas
func isGenericBrand(brand string) bool {
	genericBrands := []string{
		"GENÉRICO", "GENERICO", "GENERIC",
		"NO BRAND", "NOBRAND", "UNKNOWN",
		"SIN MARCA", "N/A", "NA", "NULL",
		// Marcas que son categorías, no marcas comerciales reales
		"COMESTIBLE", "COMESTIBLES", "FOOD", "ALIMENTO", "ALIMENTOS",
		"FERRETERIA", "FERRETERÍA", "HARDWARE",
		"HOGAR", "HOME", "CASA",
		"LIMPIEZA", "CLEANING",
		"VARIOS", "MISCELANEOS", "MISCELLANEOUS",
		"IMPORTADO", "IMPORTED",
	}

	upperBrand := strings.ToUpper(strings.TrimSpace(brand))
	for _, generic := range genericBrands {
		if upperBrand == generic {
			return true
		}
	}

	return false
}
