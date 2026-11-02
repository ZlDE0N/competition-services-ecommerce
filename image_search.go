package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// searchVariationTracker rastrea la última variación usada por producto
// para auto-rotar en búsquedas repetidas (cuando el frontend no envía variation)
var (
	searchVariationTracker     = make(map[string]searchVariationEntry)
	searchVariationTrackerLock sync.Mutex
)

type searchVariationEntry struct {
	LastVariation int
	LastSearchAt  time.Time
}

const maxVariations = 3 // variaciones 0-2

// getNextVariation devuelve la siguiente variación para un producto.
// Si han pasado más de 10 minutos desde la última búsqueda, reinicia a 0.
func getNextVariation(productId string) int {
	searchVariationTrackerLock.Lock()
	defer searchVariationTrackerLock.Unlock()

	entry, exists := searchVariationTracker[productId]
	if !exists || time.Since(entry.LastSearchAt) > 10*time.Minute {
		// Primera búsqueda o búsqueda después de 10 min: empezar con 0
		searchVariationTracker[productId] = searchVariationEntry{
			LastVariation: 0,
			LastSearchAt:  time.Now(),
		}
		return 0
	}

	// Incrementar variación
	nextVariation := (entry.LastVariation + 1) % maxVariations
	searchVariationTracker[productId] = searchVariationEntry{
		LastVariation: nextVariation,
		LastSearchAt:  time.Now(),
	}
	return nextVariation
}

type ImageSearchRequest struct {
	ProductId string `json:"productId" binding:"required"`
}

type BraveImageSearchResponse struct {
	Results []BraveImageResult `json:"results"`
}

type BraveImageResult struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Thumbnail struct {
		Src    string `json:"src"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	} `json:"thumbnail"`
	Properties struct {
		URL         string `json:"url"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		Placeholder string `json:"placeholder"`
	} `json:"properties"`
}

type ImageSearchResponse struct {
	ProductId      string      `json:"productId"`
	SearchTerms    []string    `json:"searchTerms"`
	Images         []ImageInfo `json:"images"`
	TotalImages    int         `json:"totalImages"`
	ProcessingTime int64       `json:"processingTime"`
	SearchedAt     time.Time   `json:"searchedAt"`
}

type ImageInfo struct {
	Title        string `json:"title"`
	URL          string `json:"url"`
	ThumbnailURL string `json:"thumbnailUrl"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Format       string `json:"format"`
	Size         int    `json:"size"`
	AspectRatio  string `json:"aspectRatio"`
	Source       string `json:"source"`
	Dimensions   string `json:"dimensions"`
	Selected     bool   `json:"selected"`
}

type EnhancedImageInfo struct {
	ImageInfo
	TransparencyAnalysis `json:"transparencyAnalysis"`
}

type EnhancedImageSearchResponse struct {
	ProductId       string              `json:"productId"`
	SearchTerms     []string            `json:"searchTerms"`
	Images          []EnhancedImageInfo `json:"images"`
	TotalImages     int                 `json:"totalImages"`
	ProcessingTime  int64               `json:"processingTime"`
	SearchedAt      time.Time           `json:"searchedAt"`
	MainCandidates  int                 `json:"mainCandidates"`
	AnalysisMethod  string              `json:"analysisMethod"`
	SearchVariation int                 `json:"searchVariation"` // 0=estándar completo, 1=contexto BD, 2=IA creativa
	PrimaryBarcode  string              `json:"primaryBarcode,omitempty"`
	BarcodesUsed    []string            `json:"barcodesUsed,omitempty"` // barcodes enviados a UPCitemdb
	UPCitemdbImages int                 `json:"upcitemdbImages"`        // cuántas imágenes aportó UPCitemdb
	BraveImages     int                 `json:"braveImages"`            // cuántas imágenes aportó Brave
}

func searchProductImages(c *gin.Context) {
	startTime := time.Now()

	productId := c.Param("id")

	// Parámetro de variación para "buscar nuevamente" con resultados diferentes
	// Si el frontend envía variation explícitamente, se usa ese valor.
	// Si no envía variation (o envía "auto"), se auto-rota: 0→1→2→0→...
	variationStr := c.DefaultQuery("variation", "auto")
	variation := 0
	if variationStr == "auto" {
		// Auto-rotar: cada llamada para el mismo producto usa la siguiente variación
		variation = getNextVariation(productId)
		log.Printf("🔄 Auto-rotated variation to %d for product %s", variation, productId)
	} else if v, err := strconv.Atoi(variationStr); err == nil && v >= 0 && v <= 2 {
		variation = v
	}

	log.Printf("🔍 Starting image search for product ID: %s (variation: %d)", productId, variation)

	if !uuidRegex.MatchString(productId) {
		log.Printf("❌ Invalid product ID format: %s", productId)
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:   "Invalid product ID format",
			Code:    "INVALID_PRODUCT_ID",
			Details: "Product ID must be a valid UUID or alphanumeric string",
		})
		return
	}

	// Verificar conexión a base de datos
	if db == nil {
		log.Printf("❌ Database connection is nil")
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error:   "Database connection not available",
			Code:    "DATABASE_CONNECTION_ERROR",
			Details: "Database connection is not initialized",
		})
		return
	}

	// Buscar producto en base de datos
	log.Printf("🔍 Searching for product in database: %s", productId)
	product, err := findProductByID(productId)
	if err != nil {
		log.Printf("❌ Product search failed: %v", err)
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error:   "Product not found",
			Code:    "PRODUCT_NOT_FOUND",
			Details: err.Error(),
		})
		return
	}

	log.Printf("✅ Product found: %s (Category=%s, SubCategory=%s, Department=%s, Provider=%s)",
		product.Name, product.CategoryName, product.SubCategoryName, product.DepartmentName, product.ProviderName)

	// Cargar todos los códigos de barra desde catalog.tblBarcode (sección "Códigos de barra" de la interfaz)
	if len(product.AllBarcodes) == 0 {
		product.AllBarcodes = getProductBarcodes(productId)
		if len(product.AllBarcodes) > 0 {
			product.PrimaryBarcode = product.AllBarcodes[0]
		}
	}
	log.Printf("🔖 Barcodes: primary='%s' all=%v", product.PrimaryBarcode, product.AllBarcodes)

	// US: Usar datos enriquecidos de la sección GENERAL si ya se generaron en /enhance
	// Esto mejora la búsqueda de imágenes usando el nombre y descripción mejorados por la IA
	var enhancedContext map[string]string
	if found, _ := getCache(fmt.Sprintf("PIM:AI:enhanced_context:%s", productId), &enhancedContext); found {
		if enhName, ok := enhancedContext["name"]; ok && enhName != "" && enhName != product.Name {
			log.Printf("🧠 Using AI-enhanced name for image search: '%s' → '%s'", product.Name, enhName)
			product.Name = enhName // Usar nombre mejorado para generar mejores términos
		}
		if enhDesc, ok := enhancedContext["shortDescription"]; ok && enhDesc != "" {
			log.Printf("🧠 Using AI-enhanced description for image search")
			product.ShortDescription = &enhDesc
		}
	}

	// UPCitemdb PRIMERO — fuente de mayor precisión cuando hay barcodes.
	// Brave Image Search devuelve 0 resultados para códigos UPC numéricos
	// (los interpreta como teléfonos/referencias, no como product codes).
	// UPCitemdb consulta directamente el catálogo de fabricante/retailers.
	var upcFirstImages []ImageInfo
	var upcModel string
	if len(product.AllBarcodes) > 0 {
		upcFirstImages, upcModel = fetchImagesFromUPCitemdb(product.AllBarcodes)
		if len(upcFirstImages) > 0 {
			log.Printf("✅ UPCitemdb aportó %d imágenes (fuente primaria por barcode)", len(upcFirstImages))
		}
	}

	// Generar términos de búsqueda para Brave — cuando hay barcodes, se rota la variación
	// solicitada +1 antes de generar los términos de cola (el barcode en sí ya se inyecta
	// como primer término dentro de generateHybridSearchTermsWithVariation, para CUALQUIER
	// variación, así que rotar aquí solo sirve para que 0/1/2 pedidos por el frontend sigan
	// mapeando a 3 conjuntos de términos de cola distintos entre sí -- si en vez de rotar se
	// forzaba siempre variation=1 (código anterior), una petición de variation=0 y una de
	// variation=1 quedaban usando exactamente los mismos términos de cola, dando resultados
	// idénticos en "Buscar de nuevo".
	log.Printf("🔍 Generating search terms for product: %s (variation: %d)", product.Name, variation)
	effectiveVariation := variation
	if len(product.AllBarcodes) > 0 {
		effectiveVariation = (variation + 1) % 3
	}
	searchTerms := generateHybridSearchTermsWithVariation(product, effectiveVariation)
	log.Printf("✅ Generated search terms (requested variation %d -> effective %d): %v", variation, effectiveVariation, searchTerms)

	// si UPCitemdb devolvió número de modelo, agregarlo como término de alta
	// precisión (restaura la especificidad que tenían los ~8 patrones de búsqueda
	// antes de consolidar a 3 variaciones — ver comentario QA de Cristhian Saldaña).
	if upcModel != "" {
		alreadyPresent := false
		for _, t := range searchTerms {
			if strings.EqualFold(strings.TrimSpace(t), upcModel) {
				alreadyPresent = true
				break
			}
		}
		if !alreadyPresent {
			searchTerms = append(searchTerms, upcModel)
			log.Printf("✅ Added UPCitemdb model as extra search term: %s", upcModel)
		}
	}

	// Buscar imágenes usando Brave Search API
	log.Printf("🔍 Starting Brave Search API call with %d search terms", len(searchTerms))
	images, err := searchImagesWithBrave(searchTerms, product)
	if err != nil {
		log.Printf("❌ Brave Search API failed: %v", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error:   "Failed to search images",
			Code:    "IMAGE_SEARCH_FAILED",
			Details: err.Error(),
		})
		return
	}

	log.Printf("✅ Brave Search API returned %d images", len(images))

	// Procesar y filtrar resultados con IA
	processedImages := processImageResults(images, product)

	// Si no hay resultados después del filtrado Y es variación 0, intentar variaciones de fallback
	if len(processedImages) == 0 && variation == 0 {
		log.Printf("⚠️ No relevant images found with variation 0, trying automatic fallback variations...")

		for fallbackVariation := 1; fallbackVariation <= 2; fallbackVariation++ {
			log.Printf("🔄 Trying automatic fallback with variation %d", fallbackVariation)
			fallbackTerms := generateHybridSearchTermsWithVariation(product, fallbackVariation)
			log.Printf("   Fallback terms: %v", fallbackTerms)

			fallbackImages, err := searchImagesWithBrave(fallbackTerms, product)
			if err != nil {
				log.Printf("   ❌ Fallback variation %d failed: %v", fallbackVariation, err)
				continue
			}

			processedImages = processImageResults(fallbackImages, product)
			if len(processedImages) > 0 {
				log.Printf("   ✅ Fallback variation %d found %d relevant images!", fallbackVariation, len(processedImages))
				searchTerms = fallbackTerms
				variation = fallbackVariation
				break
			}
			log.Printf("   ⚠️ Fallback variation %d returned 0 relevant images", fallbackVariation)
		}
	}

	// Anteponer imágenes de UPCitemdb al conjunto final de Brave.
	// Se hace DESPUÉS del filtrado de Brave para no perder ninguna imagen de retailer.
	if len(upcFirstImages) > 0 {
		processedImages = append(upcFirstImages, processedImages...)
	}

	// Analizar transparencia de cada imagen — PARALELO con semáforo de 5 workers
	analyzer := NewImageTransparencyAnalyzer()
	enhancedImages := make([]EnhancedImageInfo, len(processedImages))
	mainCandidates := 0

	log.Printf("Starting PARALLEL transparency analysis for %d images", len(processedImages))
	parallelStart := time.Now()

	const maxParallelAnalysis = 5
	sem := make(chan struct{}, maxParallelAnalysis)
	var wgAnalysis sync.WaitGroup

	for idx, img := range processedImages {
		wgAnalysis.Add(1)
		sem <- struct{}{} // adquirir slot
		go func(i int, imgInfo ImageInfo) {
			defer wgAnalysis.Done()
			defer func() { <-sem }() // liberar slot

			analysis, err := analyzer.AnalyzeImage(imgInfo.URL)
			if err != nil {
				log.Printf("Error analyzing transparency for %s: %v", imgInfo.URL, err)
				analysis = &TransparencyAnalysis{
					TransparencyPercentage:    0,
					WhiteBackgroundPercentage: 0,
					TotalClearBackground:      0,
					Priority:                  6,
					IsMainImageCandidate:      false,
					Algorithm:                 "error_fallback",
					ImageFormat:               "unknown",
					ProcessingTime:            0,
					QualityLabel:              QualityMala,
					RecommendedType:           "ambiente",
				}
			}
			// UPCitemdb es la representación oficial del código de barras —
			// siempre "Excelente", sin importar el score de análisis visual.
			if imgInfo.Source == "UPCitemdb" {
				analysis.QualityLabel = QualityExcelente
				analysis.RecommendedType = "principal"
				analysis.Priority = 1
				analysis.IsMainImageCandidate = true
			}
			enhancedImages[i] = EnhancedImageInfo{
				ImageInfo:            imgInfo,
				TransparencyAnalysis: *analysis,
			}
		}(idx, img)
	}

	wgAnalysis.Wait()
	log.Printf("✅ Parallel transparency analysis done in %s", time.Since(parallelStart))

	for _, img := range enhancedImages {
		if img.TransparencyAnalysis.IsMainImageCandidate {
			mainCandidates++
		}
	}

	// ORDENAMIENTO: UPCitemdb siempre primero (son imágenes exactas del fabricante/retailers),
	// luego por transparencia dentro de cada grupo.
	log.Printf("Sorting %d images by source+transparency priority", len(enhancedImages))
	sort.SliceStable(enhancedImages, func(i, j int) bool {
		iIsUPC := enhancedImages[i].ImageInfo.Source == "UPCitemdb"
		jIsUPC := enhancedImages[j].ImageInfo.Source == "UPCitemdb"
		// UPCitemdb siempre gana contra Brave
		if iIsUPC != jIsUPC {
			return iIsUPC
		}
		// Mismo grupo: ordenar por prioridad de transparencia (1 = mejor)
		if enhancedImages[i].TransparencyAnalysis.Priority != enhancedImages[j].TransparencyAnalysis.Priority {
			return enhancedImages[i].TransparencyAnalysis.Priority < enhancedImages[j].TransparencyAnalysis.Priority
		}
		return enhancedImages[i].TransparencyAnalysis.TotalClearBackground > enhancedImages[j].TransparencyAnalysis.TotalClearBackground
	})

	log.Printf("Analysis complete. Found %d main candidates out of %d images", mainCandidates, len(enhancedImages))

	// Contar imágenes por fuente para diagnóstico QA
	upcCount, braveCount := 0, 0
	for _, img := range enhancedImages {
		if img.ImageInfo.Source == "UPCitemdb" {
			upcCount++
		} else {
			braveCount++
		}
	}

	response := EnhancedImageSearchResponse{
		ProductId:       productId,
		SearchTerms:     searchTerms,
		Images:          enhancedImages,
		TotalImages:     len(enhancedImages),
		ProcessingTime:  time.Since(startTime).Milliseconds(),
		SearchedAt:      time.Now(),
		MainCandidates:  mainCandidates,
		AnalysisMethod:  "ai_enhanced_search_with_transparency_analysis",
		SearchVariation: variation,
		PrimaryBarcode:  product.PrimaryBarcode,
		BarcodesUsed:    product.AllBarcodes,
		UPCitemdbImages: upcCount,
		BraveImages:     braveCount,
	}

	c.JSON(http.StatusOK, response)
}

func generateImageSearchTerms(product *Product) []string {
	var terms []string

	// Limpiar y mejorar el nombre del producto
	if product.Name != "" {
		// Remover comillas y caracteres especiales del nombre
		cleanName := strings.ReplaceAll(product.Name, "\"", "")
		cleanName = strings.ReplaceAll(cleanName, "\\", "")

		// Dividir en palabras clave principales
		words := strings.Fields(cleanName)

		// Filtrar palabras muy cortas y agregar términos útiles
		var filteredWords []string
		for _, word := range words {
			if len(word) > 2 && !isStopWord(word) {
				filteredWords = append(filteredWords, word)
			}
		}

		if len(filteredWords) > 0 {
			// Crear combinaciones de términos más cortas y específicas
			if len(filteredWords) > 3 {
				terms = append(terms, strings.Join(filteredWords[:3], " "))
			} else {
				terms = append(terms, strings.Join(filteredWords, " "))
			}
		}

		// También agregar el nombre completo limpio
		if len(cleanName) > 0 && len(cleanName) < 50 {
			terms = append(terms, cleanName)
		}
	}

	// Obtener marca y modelo de atributos PIM
	if product.PIMAttributes != nil {
		var pimData map[string]interface{}
		if err := json.Unmarshal([]byte(*product.PIMAttributes), &pimData); err == nil {
			var brandName string
			if brand, ok := pimData["brand"].(string); ok && brand != "" {
				brandName = brand
				terms = append(terms, brand)
			}
			if model, ok := pimData["model"].(string); ok && model != "" {
				if brandName != "" {
					terms = append(terms, brandName+" "+model)
				} else {
					terms = append(terms, model)
				}
			}
		}
	}

	// Agregar términos genéricos útiles para hardware/ferretería
	if len(terms) == 0 {
		if strings.Contains(strings.ToLower(product.Name), "tirador") {
			terms = append(terms, "cabinet handle hardware")
		} else if strings.Contains(strings.ToLower(product.Name), "pomo") {
			terms = append(terms, "door knob hardware")
		} else {
			terms = append(terms, "hardware product")
		}
	}

	return terms
}

func isStopWord(word string) bool {
	stopWords := []string{"pack", "de", "el", "la", "los", "las", "un", "una", "y", "o", "con"}
	word = strings.ToLower(word)
	for _, stop := range stopWords {
		if word == stop {
			return true
		}
	}
	return false
}

func searchImagesWithBrave(searchTerms []string, product *Product) ([]BraveImageResult, error) {
	braveApiKey := os.Getenv("BRAVE_SEARCH_API_KEY")
	if braveApiKey == "" {
		return nil, fmt.Errorf("BRAVE_SEARCH_API_KEY not configured")
	}

	// Filtrar términos válidos (eliminar UUIDs y códigos internos)
	var validTerms []string
	for _, term := range searchTerms {
		if isValidSearchTerm(term) {
			validTerms = append(validTerms, term)
		}
	}

	// NUEVA ESTRATEGIA: Búsqueda combinada y segmentada con filtrado inteligente
	var allImages []BraveImageResult
	var validImageCount int

	// Paso 1: Usar términos segmentados combinados (más específicos primero)
	// IMPORTANTE: No parar prematuramente - buscar con TODOS los términos importantes
	// (especialmente los que incluyen número de manufactura) para que el filtro de
	// relevancia pueda descartar los irrelevantes y quedarse con los buenos.

	for i, term := range validTerms {
		// Limitar búsquedas pero dar más oportunidad a términos de mayor calidad
		maxSearches := 6 // Aumentamos para compensar el filtrado
		if i >= maxSearches {
			break
		}

		log.Printf("🔍 Segmented search #%d with query: %s", i+1, term)

		// Búsqueda con parámetros optimizados para productos
		apiUrl := fmt.Sprintf("https://api.search.brave.com/res/v1/images/search?q=%s&count=15&safesearch=strict&spellcheck=1",
			url.QueryEscape(term))

		images, err := performBraveSearch(apiUrl, braveApiKey)
		if err != nil {
			log.Printf("❌ Error in segmented search with term '%s': %v", term, err)
			// Para rate limits, esperar y REINTENTAR (no saltar el término)
			if strings.Contains(err.Error(), "429") {
				log.Printf("⏳ Rate limit encountered for term '%s', waiting 1.5s and retrying...", term)
				time.Sleep(1500 * time.Millisecond)
				images, err = performBraveSearch(apiUrl, braveApiKey)
				if err != nil {
					log.Printf("❌ Retry also failed for term '%s': %v", term, err)
					continue
				}
				log.Printf("✅ Retry successful for term '%s'", term)
			} else {
				continue
			}
		}

		log.Printf("✅ Found %d images for term '%s'", len(images), term)

		// Contar imágenes válidas antes de agregar
		validImagesInBatch := 0
		for _, img := range images {
			imageURL := img.Thumbnail.Src
			if imageURL == "" {
				imageURL = img.Properties.URL
			}
			if isValidImageURL(imageURL) {
				validImagesInBatch++
			}
		}

		log.Printf("📊 %d valid images found in this batch", validImagesInBatch)
		allImages = append(allImages, images...)
		validImageCount += validImagesInBatch

		// Solo parar si tenemos suficientes imágenes Y ya hemos buscado con al menos 3 términos
		// Esto asegura que se busque con número de manufactura y términos descriptivos
		// antes de detenerse, dando al filtro de relevancia más opciones para elegir
		if validImageCount >= 15 && i >= 2 {
			log.Printf("📊 Sufficient valid images (%d) with %d terms searched, stopping search", validImageCount, i+1)
			break
		}
	}

	// Paso 2: VERIFICAR RELEVANCIA DE CATEGORÍA - Detectar si los resultados son del producto correcto
	relevancePercent, shouldFallback := checkResultsCategoryRelevance(allImages, product)

	if shouldFallback && len(allImages) > 0 {
		log.Printf("🔄 Category relevance too low (%d%%), activating descriptive fallback", relevancePercent)

		// Generar términos descriptivos sin número de manufactura
		descriptiveTerms := generateDescriptiveFallbackTerms(product)
		log.Printf("📝 Generated %d descriptive fallback terms", len(descriptiveTerms))

		// Buscar con términos descriptivos
		for i, term := range descriptiveTerms {
			if i >= 3 { // Máximo 3 búsquedas de fallback
				break
			}

			log.Printf("🔍 Descriptive fallback search #%d with query: %s", i+1, term)

			fallbackUrl := fmt.Sprintf("https://api.search.brave.com/res/v1/images/search?q=%s&count=15&safesearch=strict&spellcheck=1",
				url.QueryEscape(term))

			images, err := performBraveSearch(fallbackUrl, braveApiKey)
			if err != nil {
				log.Printf("❌ Descriptive fallback search failed: %v", err)
				if strings.Contains(err.Error(), "429") {
					log.Printf("⏳ Rate limit on fallback, waiting 1.5s and retrying...")
					time.Sleep(1500 * time.Millisecond)
					images, err = performBraveSearch(fallbackUrl, braveApiKey)
					if err != nil {
						log.Printf("❌ Retry also failed: %v", err)
						continue
					}
				} else {
					continue
				}
			}

			log.Printf("✅ Descriptive fallback added %d images for term '%s'", len(images), term)
			allImages = append(allImages, images...)

			// Verificar si los nuevos resultados mejoran la relevancia
			newRelevance, stillNeedsFallback := checkResultsCategoryRelevance(allImages, product)
			if !stillNeedsFallback || newRelevance >= 50 {
				log.Printf("✅ Relevance improved to %d%%, stopping fallback", newRelevance)
				break
			}
		}
	}

	// Paso 3: Si aún no hay suficientes imágenes válidas, usar fallback inteligente básico
	if validImageCount < 3 {
		log.Printf("🔄 Insufficient valid images (%d), trying basic fallback", validImageCount)

		fallbackTerm := generateIntelligentFallback(product)
		fallbackUrl := fmt.Sprintf("https://api.search.brave.com/res/v1/images/search?q=%s&count=15&safesearch=strict",
			url.QueryEscape(fallbackTerm))

		images, err := performBraveSearch(fallbackUrl, braveApiKey)
		if err != nil {
			log.Printf("❌ Basic fallback search failed: %v", err)
			if strings.Contains(err.Error(), "429") {
				log.Printf("⏳ Rate limit on basic fallback, waiting 1.5s and retrying...")
				time.Sleep(1500 * time.Millisecond)
				images, err = performBraveSearch(fallbackUrl, braveApiKey)
			}
			if err != nil {
				if len(allImages) == 0 {
					return nil, fmt.Errorf("all search strategies failed: %v", err)
				}
			}
		}
		if err == nil {
			log.Printf("✅ Basic fallback search added %d images", len(images))
			allImages = append(allImages, images...)
		}
	}

	// Paso 4: Deduplicar imágenes por URL
	deduplicatedImages := deduplicateImagesByURL(allImages)
	log.Printf("🧹 Deduplicated: %d → %d images", len(allImages), len(deduplicatedImages))

	return deduplicatedImages, nil
}

// performBraveSearch realiza una búsqueda individual en Brave API
func performBraveSearch(apiUrl, braveApiKey string) ([]BraveImageResult, error) {
	req, err := http.NewRequest("GET", apiUrl, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %v", err)
	}

	req.Header.Set("X-Subscription-Token", braveApiKey)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error making request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(body))
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response: %v", err)
	}

	log.Printf("Brave API response: %s", string(body))

	var braveResponse BraveImageSearchResponse
	if err := json.Unmarshal(body, &braveResponse); err != nil {
		return nil, fmt.Errorf("error parsing response: %v", err)
	}

	log.Printf("Found %d images from Brave Search", len(braveResponse.Results))
	return braveResponse.Results, nil
}

func processImageResults(braveImages []BraveImageResult, product *Product) []ImageInfo {
	var images []ImageInfo

	log.Printf("🔍 Processing %d images with AI relevance filtering...", len(braveImages))

	for _, braveImg := range braveImages {
		// Calcular aspect ratio
		aspectRatio := "unknown"
		if braveImg.Properties.Width > 0 && braveImg.Properties.Height > 0 {
			ratio := float64(braveImg.Properties.Width) / float64(braveImg.Properties.Height)
			if ratio > 1.3 {
				aspectRatio = "landscape"
			} else if ratio < 0.8 {
				aspectRatio = "portrait"
			} else {
				aspectRatio = "square"
			}
		}

		// Generar string de dimensiones
		dimensions := fmt.Sprintf("%dx%d", braveImg.Properties.Width, braveImg.Properties.Height)
		if braveImg.Properties.Width == 0 || braveImg.Properties.Height == 0 {
			dimensions = "Dimensiones no disponibles"
		}

		// Usar thumbnail como URL principal para mejor rendimiento
		imageURL := braveImg.Thumbnail.Src
		if imageURL == "" {
			imageURL = braveImg.Properties.URL
		}

		// FILTRAR IMÁGENES CON URLs PROBLEMÁTICAS
		if !isValidImageURL(imageURL) {
			log.Printf("🚫 Skipped image with invalid/problematic URL: %s", braveImg.Title)
			continue
		}

		// FILTRO DE RELEVANCIA CON IA REACTIVADO
		brandName := getBrandName(product)
		manufacturerPart := getStringValue(product.ManufacturerPartNumber)

		// Extraer categoría y marca conocida del producto para mejor filtrado
		productName := strings.TrimSpace(strings.ReplaceAll(product.Name, "\"", ""))
		productCategory := extractProductCategory(productName)
		knownBrand := extractKnownBrandFromName(productName)

		// Usar análisis de IA con categoría y marca conocida
		relevanceScore := analyzeImageRelevance(braveImg.Title, product.Name, manufacturerPart, brandName, productCategory, knownBrand)
		log.Printf("🔍 AI Relevance score %d for: %s (category: %s, knownBrand: %s)", relevanceScore, braveImg.Title, productCategory, knownBrand)

		// FILTRO MENOS RESTRICTIVO: Umbral muy bajo para permitir más resultados
		if relevanceScore == 0 {
			log.Printf("🎯 Filtered zero relevance (score %d): %s", relevanceScore, braveImg.Title)
			continue
		}

		// FILTRO DE MARCA MÁS FLEXIBLE: Solo aplicar para marcas específicas válidas
		if brandName != "" && !isGenericBrandImage(brandName) && !isUUID(brandName) && relevanceScore < 3 {
			// Solo filtrar por marca si la relevancia es muy baja Y no coincide la marca
			brandInTitle := strings.Contains(strings.ToLower(braveImg.Title), strings.ToLower(brandName))
			if !brandInTitle {
				log.Printf("🚫 Filtered wrong brand with low relevance - Expected: %s, Score: %d, Title: %s", brandName, relevanceScore, braveImg.Title)
				continue
			}
		}

		image := ImageInfo{
			Title:        braveImg.Title,
			URL:          imageURL, // Usar thumbnail para carga más rápida
			ThumbnailURL: braveImg.Thumbnail.Src,
			Width:        braveImg.Properties.Width,
			Height:       braveImg.Properties.Height,
			Format:       "", // No disponible en esta API
			Size:         0,  // No disponible en esta API
			AspectRatio:  aspectRatio,
			Source:       "Brave Search",
			Dimensions:   dimensions,
			Selected:     false,
		}

		// Filtrar imágenes válidas (con URL)
		if image.URL != "" {
			images = append(images, image)
			log.Printf("✅ Added highly relevant image (score %d): %s", relevanceScore, image.Title)
		} else {
			log.Printf("❌ Skipped image without URL: %s", braveImg.Title)
		}
	}

	log.Printf("📊 Image processing results: %d images processed, %d images kept with AI relevance and brand filtering", len(braveImages), len(images))
	return images
}

// isValidSearchTerm filtra UUIDs, GUIDs y términos problemáticos
func isValidSearchTerm(term string) bool {
	term = strings.TrimSpace(term)

	// Filtrar términos vacíos
	if term == "" {
		return false
	}

	// Filtrar UUIDs (formato: 8-4-4-4-12 caracteres hexadecimales)
	uuidPattern := regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$`)
	if uuidPattern.MatchString(strings.ToUpper(term)) {
		return false
	}

	// Filtrar términos que contienen UUIDs
	if regexp.MustCompile(`[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}`).MatchString(strings.ToUpper(term)) {
		return false
	}

	// Filtrar códigos alfanuméricos largos sin espacios, pero permitir números de manufactura válidos
	if len(term) > 15 && !strings.Contains(term, " ") && regexp.MustCompile(`^[A-Z0-9-]+$`).MatchString(strings.ToUpper(term)) {
		// Permitir si contiene patrones típicos de números de manufactura (letras + números + guiones)
		hasMultipleSegments := strings.Count(term, "-") >= 1 && strings.Count(term, "-") <= 3
		hasReasonableLength := len(term) <= 25
		if !hasMultipleSegments || !hasReasonableLength {
			return false
		}
	}

	// Filtrar códigos que empiezan con A020, B020, etc. (parecen códigos de sistema)
	if regexp.MustCompile(`^[A-Z]\d{3}-\d{6}$`).MatchString(strings.ToUpper(term)) {
		return false
	}

	// Aceptar términos que parecen descripciones de productos
	return true
}

// generateIntelligentFallback crea un término de búsqueda basado en el nombre del producto
func generateIntelligentFallback(product *Product) string {
	if product.Name == "" {
		return "decorative product"
	}

	cleanName := strings.ReplaceAll(product.Name, "\"", "")
	cleanName = strings.TrimSpace(cleanName)
	lowerName := strings.ToLower(cleanName)

	// Mapeo de términos en español a inglés para mejorar resultados
	translations := map[string]string{
		"cenefa":   "decorative border",
		"arcilla":  "clay",
		"azulejo":  "ceramic tile",
		"cortina":  "curtain",
		"tirador":  "cabinet handle",
		"pomo":     "door knob",
		"bisagra":  "hinge",
		"lampara":  "lamp",
		"abanico":  "ceiling fan",
		"alfombra": "rug",
	}

	// Intentar traducir términos clave
	words := strings.Fields(lowerName)
	var translatedTerms []string

	for _, word := range words {
		if translation, exists := translations[word]; exists {
			translatedTerms = append(translatedTerms, translation)
		} else if len(word) > 3 { // Mantener palabras significativas
			translatedTerms = append(translatedTerms, word)
		}
	}

	// Si tenemos traducciones, usarlas
	if len(translatedTerms) > 0 {
		fallback := strings.Join(translatedTerms, " ")
		// Limitar a 3 palabras para mejores resultados
		words := strings.Fields(fallback)
		if len(words) > 3 {
			return strings.Join(words[:3], " ")
		}
		return fallback
	}

	// Fallback final: usar el nombre original pero limitado
	words = strings.Fields(cleanName)
	if len(words) > 3 {
		return strings.Join(words[:3], " ")
	}

	return cleanName
}

// deduplicateImagesByURL elimina imágenes duplicadas usando múltiples criterios:
// 1. URL exacta (Properties.URL)
// 2. Título + dominio fuente (misma imagen con diferentes resoluciones de thumbnail)
func deduplicateImagesByURL(images []BraveImageResult) []BraveImageResult {
	seenURL := make(map[string]bool)
	seenTitleDomain := make(map[string]bool)
	var deduplicated []BraveImageResult

	for _, img := range images {
		// Criterio 1: URL exacta
		imgURL := img.Properties.URL
		if imgURL == "" {
			imgURL = img.Thumbnail.Src
		}

		// Criterio 2: título normalizado + dominio fuente
		// Esto captura la misma imagen con diferentes resoluciones de thumbnail
		titleKey := ""
		if img.Title != "" {
			// Normalizar título: minúsculas, primeros 60 chars
			title := strings.ToLower(strings.TrimSpace(img.Title))
			if len(title) > 60 {
				title = title[:60]
			}
			// Extraer dominio de la URL del resultado
			domain := ""
			if img.URL != "" {
				if parsed, err := url.Parse(img.URL); err == nil {
					domain = parsed.Hostname()
				}
			}
			titleKey = title + "|" + domain
		}

		isDupeURL := imgURL != "" && seenURL[imgURL]
		isDupeTitle := titleKey != "" && seenTitleDomain[titleKey]

		if !isDupeURL && !isDupeTitle {
			deduplicated = append(deduplicated, img)
			if imgURL != "" {
				seenURL[imgURL] = true
			}
			if titleKey != "" {
				seenTitleDomain[titleKey] = true
			}
		}
	}

	return deduplicated
}

// isValidImageURL valida si una URL de imagen es accesible y confiable
func isValidImageURL(imageURL string) bool {
	if imageURL == "" {
		return false
	}

	// Decodificar URLs de Brave Search que contienen base64
	actualURL := imageURL
	if strings.Contains(imageURL, "imgs.search.brave.com") {
		if decodedURL := decodeBase64URL(imageURL); decodedURL != "" {
			actualURL = decodedURL
			log.Printf("🔍 Successfully decoded URL: %s", decodedURL)
		} else {
			// Si no se puede decodificar, verificar patrones directamente en la URL encoded
			log.Printf("⚠️ Could not decode URL, checking patterns directly: %s", imageURL[:100])
		}
	}

	// Lista de dominios problemáticos conocidos
	problematicDomains := []string{
		"manualscat.com",  // Retorna error 422
		"manualzilla.com", // Problemas de acceso
		"manualsdir.com",  // URLs temporales
		"temp-files.org",  // Archivos temporales
		"cache-proxy.com", // Proxies problemáticos
		"manualowl.com",   // URLs problemáticas
		"manualslib.com",  // Frecuentemente inaccesible
	}

	// Verificar dominios problemáticos en la URL original Y decodificada
	lowerOriginal := strings.ToLower(imageURL)
	lowerActual := strings.ToLower(actualURL)

	for _, domain := range problematicDomains {
		if strings.Contains(lowerOriginal, domain) || strings.Contains(lowerActual, domain) {
			log.Printf("🚫 Filtered problematic domain %s in URL: %s", domain, imageURL)
			return false
		}
	}

	// FILTRADO ADICIONAL: Verificar patrones base64 encoded para dominios problemáticos
	// Convertir dominios problemáticos a base64 y buscar en la URL
	for _, domain := range problematicDomains {
		// Codificar el dominio en base64 para buscar en URLs encoded
		domainBase64 := base64.StdEncoding.EncodeToString([]byte(domain))
		if strings.Contains(imageURL, domainBase64) {
			log.Printf("🚫 Filtered base64 encoded problematic domain %s in URL", domain)
			return false
		}

		// También buscar partes del dominio encoded (como "bWFudWFsc2NhdA" para "manualscat")
		if len(domain) > 8 {
			partialDomain := domain[:8] // Primeros 8 caracteres
			partialBase64 := base64.StdEncoding.EncodeToString([]byte(partialDomain))
			if strings.Contains(imageURL, partialBase64) {
				log.Printf("🚫 Filtered partial base64 encoded problematic domain %s in URL", domain)
				return false
			}
		}
	}

	// Verificar patrones de URLs problemáticas
	problematicPatterns := []string{
		"/thumb.png", // Thumbnails que fallan frecuentemente
		"/temp/",     // Archivos temporales
		"/cache/",    // Archivos de cache
		"?expired=",  // URLs con expiración
		"temp-",      // URLs temporales
		"/viewer/",   // Visualizadores que fallan
		"/thumbnail", // Thumbnails problemáticos
	}

	for _, pattern := range problematicPatterns {
		if strings.Contains(lowerOriginal, pattern) || strings.Contains(lowerActual, pattern) {
			log.Printf("🚫 Filtered problematic pattern %s in URL: %s", pattern, imageURL)
			return false
		}
	}

	// Verificar que sea una URL válida con protocolo HTTPS preferido
	if !strings.HasPrefix(imageURL, "http://") && !strings.HasPrefix(imageURL, "https://") {
		return false
	}

	// Preferir HTTPS y dominios confiables
	trustedDomains := []string{
		"mlstatic.com",          // Mercado Libre (muy confiable)
		"falabella.com",         // Falabella (muy confiable)
		"mabe.cl",               // Sitio oficial Mabe
		"mabeglobal.com",        // Sitio oficial Mabe Global
		"hites.com",             // Retailer confiable
		"sodimac.cl",            // Retailer confiable
		"imgs.search.brave.com", // Brave Search images (solo si no son problemáticos)
	}

	// Si es de un dominio confiable Y no contiene patrones problemáticos, darle prioridad
	for _, domain := range trustedDomains {
		if strings.Contains(strings.ToLower(imageURL), domain) {
			// Para Brave Search, verificar también que no sea de dominio problemático
			if domain == "imgs.search.brave.com" {
				for _, problematicDomain := range problematicDomains {
					if strings.Contains(lowerActual, problematicDomain) {
						return false
					}
				}
			}
			return true
		}
	}

	// Para otros dominios, aplicar validaciones más estrictas
	// Rechazar URLs muy largas (probablemente encoded/problematic)
	if len(imageURL) > 500 {
		return false
	}

	// Validar extensiones de imagen válidas
	validExtensions := []string{".jpg", ".jpeg", ".png", ".webp", ".gif"}
	hasValidExtension := false

	for _, ext := range validExtensions {
		if strings.Contains(strings.ToLower(imageURL), ext) {
			hasValidExtension = true
			break
		}
	}

	return hasValidExtension
}

// isUUID verifica si una cadena es un UUID
func isUUID(s string) bool {
	uuidPattern := regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$`)
	return uuidPattern.MatchString(strings.ToUpper(s))
}

// isGenericBrandImage verifica si una marca es genérica o no útil para búsquedas de imágenes
func isGenericBrandImage(brand string) bool {
	genericBrands := []string{
		"GENÉRICO", "GENERICO", "GENERIC",
		"NO BRAND", "NOBRAND", "UNKNOWN",
		"SIN MARCA", "N/A", "NA", "NULL",
	}

	upperBrand := strings.ToUpper(strings.TrimSpace(brand))
	for _, generic := range genericBrands {
		if upperBrand == generic {
			return true
		}
	}

	return false
}

// decodeBase64URL intenta decodificar URLs base64 de Brave Search para verificar el dominio real
func decodeBase64URL(braveURL string) string {
	// Buscar el patrón base64 en URLs de Brave Search
	// Formato típico: https://imgs.search.brave.com/.../g:ce/[BASE64_ENCODED_URL]

	// Buscar después de "g:ce/"
	if idx := strings.Index(braveURL, "g:ce/"); idx != -1 {
		base64Part := braveURL[idx+5:] // Saltar "g:ce/"

		// El base64 puede estar dividido en múltiples segmentos por "/"
		// Reconstruir el base64 completo
		base64Segments := strings.Split(base64Part, "/")
		fullBase64 := strings.Join(base64Segments, "")

		// Intentar decodificar
		decoded, err := base64.StdEncoding.DecodeString(fullBase64)
		if err != nil {
			// Intentar con base64 URL-safe
			decoded, err = base64.URLEncoding.DecodeString(fullBase64)
			if err != nil {
				// Intentar con padding manual si es necesario
				for padding := 0; padding < 4; padding++ {
					paddedBase64 := fullBase64 + strings.Repeat("=", padding)
					decoded, err = base64.StdEncoding.DecodeString(paddedBase64)
					if err == nil {
						break
					}
				}
			}
		}

		if err == nil {
			decodedStr := string(decoded)
			log.Printf("🔍 Decoded URL from base64: %s", decodedStr)
			// Si parece una URL, devolverla
			if strings.HasPrefix(decodedStr, "http") {
				return decodedStr
			}
		} else {
			log.Printf("❌ Failed to decode base64: %s", fullBase64[:50]) // Solo primeros 50 chars para logs
		}
	}

	return ""
}

// checkResultsCategoryRelevance verifica si los resultados de búsqueda son relevantes para la categoría del producto
// Retorna el porcentaje de resultados relevantes (0-100) y si se debe activar el fallback
func checkResultsCategoryRelevance(images []BraveImageResult, product *Product) (relevancePercent int, shouldFallback bool) {
	if len(images) == 0 {
		return 0, true
	}

	// Extraer categoría del producto
	productCategory := extractProductCategory(product.Name)
	if productCategory == "" {
		// Si no hay categoría, no podemos validar relevancia
		return 100, false
	}

	// Obtener traducciones de la categoría para búsqueda bilingüe
	catES, catEN := getCategoryTranslations(productCategory)

	// Palabras clave que indican que el resultado NO es del producto (ruido común)
	noiseKeywords := []string{
		"pantone", "color palette", "color code", "hex color", "rgb",
		"t-shirt", "tshirt", "mockup", "template",
		"wallpaper", "background image", "gradient",
		"fashion", "clothing", "apparel",
		"artwork", "illustration", "graphic design",
	}

	// Palabras clave que indican que el resultado SÍ es relevante
	relevantKeywords := []string{
		catES, catEN,
		"hardware", "cabinet", "drawer", "kitchen", "bathroom",
		"home improvement", "home depot", "lowes", "amazon",
	}

	relevantCount := 0
	noiseCount := 0

	for _, img := range images {
		titleLower := strings.ToLower(img.Title)

		// Verificar si es ruido
		isNoise := false
		for _, noise := range noiseKeywords {
			if strings.Contains(titleLower, noise) {
				isNoise = true
				noiseCount++
				break
			}
		}

		if isNoise {
			continue
		}

		// Verificar si es relevante
		for _, keyword := range relevantKeywords {
			if keyword != "" && strings.Contains(titleLower, strings.ToLower(keyword)) {
				relevantCount++
				break
			}
		}
	}

	// Calcular porcentaje de relevancia
	totalChecked := len(images)
	if totalChecked == 0 {
		return 0, true
	}

	relevancePercent = (relevantCount * 100) / totalChecked
	noisePercent := (noiseCount * 100) / totalChecked

	log.Printf("📊 Category relevance check: %d%% relevant, %d%% noise, category: '%s'/'%s'",
		relevancePercent, noisePercent, catES, catEN)

	// Activar fallback si:
	// - Menos del 30% de resultados son relevantes, O
	// - Más del 50% son ruido (Pantone, t-shirts, etc.)
	shouldFallback = relevancePercent < 30 || noisePercent > 50

	if shouldFallback {
		log.Printf("⚠️ Low category relevance detected - activating descriptive fallback")
	}

	return relevancePercent, shouldFallback
}

// generateDescriptiveFallbackTerms genera términos de búsqueda descriptivos sin número de manufactura
func generateDescriptiveFallbackTerms(product *Product) []string {
	var terms []string

	productCategory := extractProductCategory(product.Name)
	catES, catEN := getCategoryTranslations(productCategory)
	keySpecs := extractKeySpecsFromName(product.Name)

	// Término 1: Especificaciones + categoría en inglés (más efectivo para e-commerce)
	if len(keySpecs) > 0 && catEN != "" {
		specsStr := strings.Join(keySpecs, " ")
		terms = append(terms, fmt.Sprintf("%s %s", specsStr, catEN))
		log.Printf("   [FALLBACK 1] Specs + category EN: %s %s", specsStr, catEN)
	}

	// Término 2: Categoría + especificaciones en inglés (orden alternativo)
	if catEN != "" && len(keySpecs) > 0 {
		terms = append(terms, fmt.Sprintf("%s %s product", catEN, keySpecs[0]))
		log.Printf("   [FALLBACK 2] Category + spec: %s %s product", catEN, keySpecs[0])
	}

	// Término 3: Solo categoría en inglés con "product image"
	if catEN != "" {
		terms = append(terms, fmt.Sprintf("%s product image", catEN))
		log.Printf("   [FALLBACK 3] Category + product image: %s product image", catEN)
	}

	// Término 4: Categoría en español (cobertura regional)
	if catES != "" && catES != catEN {
		terms = append(terms, catES)
		log.Printf("   [FALLBACK 4] Category ES: %s", catES)
	}

	// Si no hay términos generados, usar el nombre del producto limpio
	if len(terms) == 0 {
		cleanName := strings.ReplaceAll(product.Name, "\"", "")
		cleanName = strings.TrimSpace(cleanName)
		terms = append(terms, cleanName)
		log.Printf("   [FALLBACK 5] Clean product name: %s", cleanName)
	}

	return terms
}

// fetchImagesFromUPCitemdb consulta la API de UPCitemdb para obtener imágenes del producto
// por código de barras. Actúa como fuente primaria cuando Brave Search no indexa el UPC.
// Free tier: 100 req/día sin clave; clave opcional via UPCITEMDB_API_KEY.
// fetchImagesFromUPCitemdb también devuelve el número de modelo del primer item
// que lo traiga — se usa como término de búsqueda adicional de alta precisión para
// Brave (ver la versión anterior generaba términos con número de modelo,
// se perdió al simplificar la generación de términos).
func fetchImagesFromUPCitemdb(barcodes []string) ([]ImageInfo, string) {
	if len(barcodes) == 0 {
		return nil, ""
	}

	client := &http.Client{Timeout: 10 * time.Second}
	seen := make(map[string]bool)
	var images []ImageInfo
	var upcModel string

	for _, barcode := range barcodes {
		barcode = strings.TrimSpace(barcode)
		if barcode == "" {
			continue
		}

		apiURL := fmt.Sprintf("https://api.upcitemdb.com/prod/trial/lookup?upc=%s", url.QueryEscape(barcode))
		log.Printf("🔖 UPCitemdb lookup: barcode=%s", barcode)

		req, err := http.NewRequest("GET", apiURL, nil)
		if err != nil {
			log.Printf("⚠️ UPCitemdb: error creando request para barcode %s: %v", barcode, err)
			continue
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "PIM-Enrichment-Service/2.3.0")
		if key := os.Getenv("UPCITEMDB_API_KEY"); key != "" {
			req.Header.Set("user_key", key)
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("⚠️ UPCitemdb: request fallido para barcode %s: %v", barcode, err)
			continue
		}

		body, _ := ioutil.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("⚠️ UPCitemdb: status %d para barcode %s", resp.StatusCode, barcode)
			continue
		}

		var result struct {
			Code  string `json:"code"`
			Items []struct {
				Title  string   `json:"title"`
				Brand  string   `json:"brand"`
				Model  string   `json:"model"`
				Color  string   `json:"color"`
				Images []string `json:"images"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			log.Printf("⚠️ UPCitemdb: decode error para barcode %s: %v", barcode, err)
			continue
		}

		barcodeImages := 0
		for _, item := range result.Items {
			if upcModel == "" && strings.TrimSpace(item.Model) != "" {
				upcModel = strings.TrimSpace(item.Model)
				log.Printf("🔖 UPCitemdb: número de modelo encontrado: %s", upcModel)
			}
			// Solo la primera imagen por item: es la del CDN de UPCitemdb.
			// Las adicionales vienen de Walmart/Newegg y bloquean hotlinking (cargan vacías).
			if len(item.Images) == 0 {
				continue
			}
			imgURL := strings.TrimSpace(item.Images[0])
			if imgURL == "" || seen[imgURL] {
				continue
			}
			seen[imgURL] = true
			barcodeImages++
			images = append(images, ImageInfo{
				Title:        item.Title,
				URL:          imgURL,
				ThumbnailURL: imgURL,
				Source:       "UPCitemdb",
				AspectRatio:  "unknown",
				Dimensions:   "Dimensiones no disponibles",
				Selected:     false,
			})
		}

		log.Printf("✅ UPCitemdb: %d imágenes para barcode %s", barcodeImages, barcode)

		// Parar en el primer barcode con resultados para respetar el rate limit del free tier
		if len(images) > 0 {
			break
		}
	}

	return images, upcModel
}
