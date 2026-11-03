package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/denisenkom/go-mssqldb"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

var (
	uuidRegex = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$|^[a-zA-Z0-9\-]+$`)
	rdb       *redis.Client
	db        *sql.DB
	ctx       = context.Background()
	// singleflight: evita que el prewarm del cliente + el click del usuario
	// generen dos llamadas a Claude para el mismo producto simultáneamente.
	enhanceSF singleflight.Group
)

func init() {
	godotenv.Load() // No es necesario loguear si no encuentra .env

	// Conectar a Redis (opcional para testing)
	if err := connectToRedis(); err != nil {
		log.Printf("⚠️ Redis no disponible - cache deshabilitado: %v", err)
		rdb = nil // Graceful degradation
	}

	// Conectar a la base de datos (opcional para testing)
	if err := connectToDatabase(); err != nil {
		log.Printf("⚠️ Base de datos no disponible - algunas funciones deshabilitadas: %v", err)
		db = nil // Graceful degradation
	}
}

func connectToRedis() error {
	redisHost := os.Getenv("REDIS_HOST")
	redisPort := os.Getenv("REDIS_PORT")
	redisPassword := os.Getenv("REDIS_PASSWORD")

	if redisHost == "" {
		redisHost = "localhost"
	}
	if redisPort == "" {
		redisPort = "6379"
	}

	// Configuración específica para Azure Redis Cache con TLS
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: redisHost,
	}

	rdb = redis.NewClient(&redis.Options{
		Addr:         redisHost + ":" + redisPort,
		Password:     redisPassword,
		DB:           0,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		PoolTimeout:  5 * time.Second,
		TLSConfig:    tlsConfig, // TLS requerido para Azure Redis en puerto 6380
	})

	// Probar la conexión con timeout reducido para no bloquear el startup
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		rdb.Close()
		// Plaintext fallback only when explicitly allowed (local development).
		if os.Getenv("REDIS_ALLOW_INSECURE") != "true" {
			return fmt.Errorf("failed to connect to Redis over TLS: %v (set REDIS_ALLOW_INSECURE=true only for local development)", err)
		}
		log.Printf("⚠️  Redis: falling back to an unencrypted connection (REDIS_ALLOW_INSECURE=true)")

		rdb = redis.NewClient(&redis.Options{
			Addr:         redisHost + ":6379", // Puerto no-SSL
			Password:     redisPassword,
			DB:           0,
			DialTimeout:  5 * time.Second,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 5 * time.Second,
			PoolTimeout:  5 * time.Second,
			TLSConfig:    nil,
		})

		// Segundo intento sin TLS
		ctx2, cancel2 := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel2()

		_, err = rdb.Ping(ctx2).Result()
		if err != nil {
			return fmt.Errorf("failed to connect to Redis after multiple attempts: %v", err)
		}
	}
	return nil
}

func validateProductID(id string) *ValidationError {
	if id == "" {
		return &ValidationError{
			Field:   "id",
			Message: "Product ID cannot be empty",
			Value:   id,
		}
	}

	if len(id) < 3 {
		return &ValidationError{
			Field:   "id",
			Message: "Product ID must be at least 3 characters long",
			Value:   id,
		}
	}

	if len(id) > 100 {
		return &ValidationError{
			Field:   "id",
			Message: "Product ID must be less than 100 characters",
			Value:   id,
		}
	}

	return nil
}

func handleError(c *gin.Context, statusCode int, code string, message string, details string) {
	log.Printf("Error [%s]: %s - %s", code, message, details)

	response := ErrorResponse{
		Error:   message,
		Code:    code,
		Details: details,
	}

	c.JSON(statusCode, response)
}

func handleValidationError(c *gin.Context, valErr *ValidationError) {
	log.Printf("Validation Error: Field %s - %s", valErr.Field, valErr.Message)

	response := struct {
		Error      string           `json:"error"`
		Code       string           `json:"code"`
		Validation *ValidationError `json:"validation"`
	}{
		Error:      "Validation failed",
		Code:       "VALIDATION_ERROR",
		Validation: valErr,
	}

	c.JSON(http.StatusBadRequest, response)
}

func connectToDatabase() error {
	server := os.Getenv("DB_SERVER")
	database := os.Getenv("DB_DATABASE")
	user := os.Getenv("DB_USER")
	password := os.Getenv("DB_PASSWORD")
	port := os.Getenv("DB_PORT")

	if port == "" {
		port = "1433"
	}

	connString := fmt.Sprintf("server=%s;user id=%s;password=%s;port=%s;database=%s;encrypt=true;TrustServerCertificate=false",
		server, user, password, port, database)

	var err error
	db, err = sql.Open("sqlserver", connString)
	if err != nil {
		return fmt.Errorf("error opening database: %v", err)
	}

	// Test the connection with timeout to avoid blocking startup
	ctxDB, cancelDB := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDB()
	if err = db.PingContext(ctxDB); err != nil {
		return fmt.Errorf("error connecting to database: %v", err)
	}

	return nil
}

func findProductByID(id string) (*Product, error) {
	query := `
		SELECT TOP 1 
			CAST(p.ProductId AS NVARCHAR(36)) AS ProductId, 
			CAST(p.FamilyId AS NVARCHAR(36)) AS FamilyId, 
			p.Sku, p.SystemCode, p.Name, p.ShortDescription, p.FullDescription,
			p.AdminComment, 
			CAST(p.SegmentId AS NVARCHAR(36)) AS SegmentId, 
			CAST(p.BrandId AS NVARCHAR(36)) AS BrandId,
			CAST(p.ManufacturerId AS NVARCHAR(36)) AS ManufacturerId, 
			CAST(p.TaxCategoryId AS NVARCHAR(36)) AS TaxCategoryId,
			p.ManufacturerPartNumber, p.Gtin, p.Price, p.Weight, p.Length, p.Width, p.Height,
			CAST(p.UoMGroupId AS NVARCHAR(36)) AS UoMGroupId, 
			p.AppEnabled, p.CreatedDate, p.CreatedBy, p.LastModifiedDate, p.LastModifiedBy,
			p.Status, p.TimeStamp, p.Tags, p.Cost, 
			CAST(p.ProductTypeId AS NVARCHAR(36)) AS ProductTypeId, 
			p.ShortName, p.ExternalCodes,
			p.Barcodes, p.IsActive, p.CustomerApp, p.OutStockThresholdQty, p.PIMAttributes,
			ISNULL(b.Name, '') AS BrandName,
			ISNULL((
				SELECT TOP 1 c.Name
				FROM catalog.linkProductCategorySubCategory lpcs
				INNER JOIN catalog.pltblCategory c ON lpcs.CategoryId = c.CategoryId
				WHERE lpcs.ProductId = p.ProductId
			), '') AS CategoryName,
			ISNULL((
				SELECT TOP 1 sc.Name
				FROM catalog.linkProductCategorySubCategory lpcs
				INNER JOIN catalog.pltblSubCategory sc ON lpcs.SubCategoryId = sc.SubCategoryId
				WHERE lpcs.ProductId = p.ProductId
			), '') AS SubCategoryName,
			ISNULL((
				SELECT TOP 1 d.Name
				FROM catalog.linkProductCategorySubCategory lpcs
				INNER JOIN catalog.pltblDepartment d ON lpcs.DepartmentId = d.DepartmentId
				WHERE lpcs.ProductId = p.ProductId
			), '') AS DepartmentName,
			ISNULL((
				SELECT TOP 1 prov.Name
				FROM catalog.linkProviderProduct pp
				INNER JOIN catalog.pltblProvider prov ON pp.ProviderId = prov.ProviderId
				WHERE pp.ProductId = p.ProductId AND pp.[Default] = 1
			), ISNULL((
				SELECT TOP 1 prov2.Name
				FROM catalog.linkProviderProduct pp2
				INNER JOIN catalog.pltblProvider prov2 ON pp2.ProviderId = prov2.ProviderId
				WHERE pp2.ProductId = p.ProductId
			), '')) AS ProviderName
		FROM catalog.tblproduct p
		LEFT JOIN catalog.tblBrand b ON p.BrandId = b.BrandId
		WHERE p.ProductId = CAST(@p1 AS UNIQUEIDENTIFIER)
		   OR p.Sku = @p1
		   OR p.Barcodes LIKE '%' + @p2 + '%' ESCAPE '\'
		   OR p.ManufacturerPartNumber = @p1
		   OR p.FamilyId = CAST(@p1 AS UNIQUEIDENTIFIER)
		   OR p.ExternalCodes LIKE '%' + @p2 + '%' ESCAPE '\'
	`

	var product Product
	row := db.QueryRow(query, id, escapeLike(id))

	// Use NullString for all non-pointer string fields — DB may store NULL for any of them
	var (
		sku, systemCode, name, brandId, taxCategoryId     sql.NullString
		appEnabled, createdBy, lastModifiedBy, status, ts sql.NullString
		productTypeId, shortName, isActive, customerApp   sql.NullString
	)

	err := row.Scan(
		&product.ProductId, &product.FamilyId, &sku, &systemCode,
		&name, &product.ShortDescription, &product.FullDescription,
		&product.AdminComment, &product.SegmentId, &brandId,
		&product.ManufacturerId, &taxCategoryId, &product.ManufacturerPartNumber,
		&product.Gtin, &product.Price, &product.Weight, &product.Length, &product.Width,
		&product.Height, &product.UoMGroupId, &appEnabled, &product.CreatedDate,
		&createdBy, &product.LastModifiedDate, &lastModifiedBy,
		&status, &ts, &product.Tags, &product.Cost,
		&productTypeId, &shortName, &product.ExternalCodes,
		&product.Barcodes, &isActive, &customerApp,
		&product.OutStockThresholdQty, &product.PIMAttributes, &product.BrandName,
		&product.CategoryName, &product.SubCategoryName, &product.DepartmentName, &product.ProviderName,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("error scanning product: %v", err)
	}

	product.Sku = sku.String
	product.SystemCode = systemCode.String
	product.Name = name.String
	product.BrandId = brandId.String
	product.TaxCategoryId = taxCategoryId.String
	product.AppEnabled = appEnabled.String
	product.CreatedBy = createdBy.String
	product.LastModifiedBy = lastModifiedBy.String
	product.Status = status.String
	product.TimeStamp = ts.String
	product.ProductTypeId = productTypeId.String
	product.ShortName = shortName.String
	product.IsActive = isActive.String
	product.CustomerApp = customerApp.String

	return &product, nil
}

// REMOVED: updateBlobUrl endpoint - el cliente now handles all database operations directly

func main() {
	// Cargar variables de entorno
	err := godotenv.Load()
	if err != nil {
		log.Printf("Warning: Error loading .env file: %v", err)
	}

	// Cron nocturno de enriquecimiento automático
	// Se inicia solo si la DB está disponible (init() ya la conectó)
	if db != nil {
		startNightlyEnrichmentJob()
		log.Printf("🌙 NightlyEnrichmentJob: scheduler iniciado")
		ensureNotificationsTable()
	} else {
		log.Printf("⚠️ NightlyEnrichmentJob: DB no disponible — scheduler no iniciado")
	}

	// Inicializar Azure Storage
	if err := initAzureStorage(); err != nil {
		log.Printf("Warning: Azure Storage initialization failed: %v", err)
		log.Printf("Azure upload features will be disabled")
	} else {
		log.Printf("Successfully initialized Azure Storage")
	}

	router := gin.Default()

	// Seguridad: cabeceras, CORS por lista y autenticación por clave API
	setupSecurity(router)

	// Interfaz web y modo demo (sin base de datos ni claves; ver demo.go y ui.go)
	registerUI(router)
	if demoEnabled() {
		demoRegister(router)
	} else {
		log.Printf("Demo API disabled (ENVIRONMENT=production; set DEMO_MODE=true to enable it)")
	}

	router.POST("/products/:id/enhance", enhanceProductAI)
	router.GET("/products/:id/images/search", searchProductImages)
	router.POST("/products/:id/images/process", processSelectedImages)

	// estandarizar descripción de garantía con IA
	router.POST("/warranty/standardize-description", standardizeWarrantyDescription)

	// Gestión de prompts por taxonomía
	router.GET("/prompts/config", getPromptConfig)
	router.PUT("/prompts/general", requireApprover(), updateGeneralPrompt)
	router.POST("/prompts/taxonomy", requireApprover(), createTaxonomyPrompt)
	router.PUT("/prompts/taxonomy/:id", requireApprover(), updateTaxonomyPrompt)
	router.DELETE("/prompts/taxonomy/:id", requireApprover(), deleteTaxonomyPrompt)
	router.GET("/prompts/preview/:id", previewPromptForProduct)
	router.GET("/taxonomy/options", getTaxonomyOptions)
	router.GET("/prompts/match", matchPromptForTaxonomy)

	// REMOVED: processAndUploadToAzure endpoint - el cliente now handles database insertion

	// REMOVED: updateBlobUrl endpoint - el cliente now handles all database operations

	// Endpoints para MediaResources y mapeo de tipos
	router.GET("/media-resources", getAvailableMediaResources)
	router.GET("/image-types/mappings", getImageTypeMappings)
	router.POST("/image-types/configure", requireApprover(), configureImageTypeMappings)

	// Endpoints para procesamiento de imágenes
	router.GET("/image-processing/config", getImageProcessingConfig)

	// REMOVED: MediaCategory debug endpoints - functions were removed with debug files

	// Debug endpoints para investigar estructura de BD (solo en desarrollo)
	if os.Getenv("ENVIRONMENT") == "development" {
		router.GET("/debug/image-type-mappings", debugImageTypeMappings)
	}

	// Cola de aprobación IA — router.GET("/enrichment-queue", getEnrichmentQueue)
	router.POST("/enrichment-enqueue", enqueueEnrichment) // encolar al aplicar
	router.GET("/enrichment-queue/:id", getEnrichmentQueueItem)
	router.POST("/enrichment-queue/:id/approve", approveEnrichmentQueueItem)
	router.POST("/enrichment-queue/:id/reject", rejectEnrichmentQueueItem)
	router.PATCH("/enrichment-queue/:id", editEnrichmentQueueItem)

	// Configuración de taxonomías para enriquecimiento automático
	router.GET("/enrichment-taxonomy-config", getTaxonomyConfigs)
	router.POST("/enrichment-taxonomy-config", createTaxonomyConfig)
	router.DELETE("/enrichment-taxonomy-config/:id", deleteTaxonomyConfig)

	// Notificaciones internas — router.GET("/notifications", getNotifications)
	router.PATCH("/notifications/:id/read", markNotificationRead)

	router.GET("/health", healthCheck)
	router.GET("/cache/stats", getCacheStats)

	// Proxy para imágenes externas (soluciona CSP)
	router.GET("/proxy-image", proxyImageHandler)

	// Manejo de cierre graceful
	go func() {
		sigchan := make(chan os.Signal, 1)
		signal.Notify(sigchan, os.Interrupt, syscall.SIGTERM)
		<-sigchan
		if db != nil {
			db.Close()
		}
		os.Exit(0)
	}()

	// Iniciar el servidor con puerto configurable
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Server starting on port %s...", port)
	router.Run(":" + port)
}

func enhanceProductAI(c *gin.Context) {
	startTime := time.Now()
	id := c.Param("id")

	// Validar ID del producto
	if valErr := validateProductID(id); valErr != nil {
		handleValidationError(c, valErr)
		return
	}

	// refresh=true → el usuario pide datos frescos (no usar caché de producto)
	// El caché de Brave web search se mantiene para acelerar las búsquedas (~6s ahorrados)
	forceRefresh := c.Query("refresh") == "true"

	cacheKey := generateCacheKey("enhanced_product", id)
	if !forceRefresh {
		var cachedResult AIEnhancementResponse
		if found, err := getCache(cacheKey, &cachedResult); err == nil && found {
			fullDesc := cachedResult.EnhancedData.FullDescription
			tituloSEO := cachedResult.EnhancedData.TituloSEO
			if fullDesc != "" && tituloSEO != "" {
				// Backfill ruleApplied for cached results generated before this field existed
				if cachedResult.RuleApplied.Level == "" {
					cachedResult.RuleApplied = RuleAppliedInfo{Level: "general"}
				}
				c.JSON(http.StatusOK, cachedResult)
				return
			}
			log.Printf("⚠️ Cache hit for %s but fields empty — regenerating", id)
		}
	} else {
		log.Printf("🔄 Force refresh requested for %s — bypassing product cache", id)
	}

	// Buscar producto en la base de datos
	product, err := findProductByID(id)
	if err != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR",
			"Error querying database", err.Error())
		return
	}

	if product == nil {
		handleError(c, http.StatusNotFound, "PRODUCT_NOT_FOUND",
			"Product not found", fmt.Sprintf("Product with ID '%s' does not exist", id))
		return
	}

	if product.Name == "" && (product.ShortDescription == nil || *product.ShortDescription == "") {
		handleError(c, http.StatusBadRequest, "INSUFFICIENT_DATA",
			"Este producto no tiene nombre ni descripción. No es posible generar contenido IA.", "")
		return
	}

	// ⚡ singleflight: si el prewarm del cliente ya lanzó este mismo productId,
	// la segunda request comparte el resultado en vez de lanzar otro pipeline.
	sfResult, err, shared := enhanceSF.Do(id, func() (interface{}, error) {
		return enhanceWithClaudeAndSearch(*product)
	})
	if shared {
		log.Printf("⚡ SINGLEFLIGHT: resultado compartido para %s (prewarm + click)", id)
	}
	if err != nil {
		// Retry automático una vez — el singleflight ya terminó, este es un intento limpio
		log.Printf("⚠️ First enhance attempt failed for %s: %v — retrying once", id, err)
		sfResult, err, _ = enhanceSF.Do(id+":retry", func() (interface{}, error) {
			return enhanceWithClaudeAndSearch(*product)
		})
		if err != nil {
			log.Printf("❌ Retry also failed for %s: %v", id, err)
			handleError(c, http.StatusServiceUnavailable, "SERVICE_ERROR",
				"El servicio de mejora de productos no está disponible temporalmente. Intente de nuevo más tarde.", "")
			return
		}
	}
	enhancedData := sfResult.(*EnhancedProduct)

	// Obtener regla de taxonomía aplicada para incluirla en el response
	appliedTaxonomyCtx := GetTaxonomyContextForProduct(product)

	// Formatear respuesta paral cliente
	response := formatForClient(id, *product, *enhancedData, time.Since(startTime), appliedTaxonomyCtx)

	// GUARDAR RESULTADO COMPLETO EN CACHÉ
	// 1h (antes 10min): regenerar/re-encolar el mismo producto dentro de la
	// ventana reusa el resultado sin re-correr el pipeline (ni llamadas Claude).
	// refresh=true sigue forzando datos frescos cuando el usuario lo pide.
	setCache(cacheKey, response, 1*time.Hour)

	// GUARDAR NOMBRE/DESCRIPCIÓN ENRIQUECIDA PARA MEJORAR BÚSQUEDA DE IMÁGENES (US: IMG usar data sección GENERAL)
	if enhancedData.CommercialContent != nil {
		enhancedContext := map[string]string{
			"name":             response.EnhancedData.Name,
			"shortDescription": response.EnhancedData.ShortDescription,
		}
		setCache(fmt.Sprintf("PIM:AI:enhanced_context:%s", id), enhancedContext, 30*time.Minute)
	}

	// "Generar con IA" es solo PREVIEW. El encolado a la cola de
	// aprobación ocurre cuando el usuario presiona "Aplicar Datos Mejorados"
	// (POST /enrichment-queue/enqueue), no en cada generación. Esto evita
	// duplicados en la cola por cada click de "Generar".

	c.JSON(http.StatusOK, response)
}

// combineUniqueTags combina dos listas de tags eliminando duplicados
func combineUniqueTags(originalTags []string, newTags []string) []string {
	tagMap := make(map[string]bool)
	var combinedTags []string

	// Agregar tags originales
	for _, tag := range originalTags {
		if tag != "" && !tagMap[strings.ToLower(tag)] {
			tagMap[strings.ToLower(tag)] = true
			combinedTags = append(combinedTags, tag)
		}
	}

	// Agregar nuevos tags
	for _, tag := range newTags {
		if tag != "" && !tagMap[strings.ToLower(tag)] {
			tagMap[strings.ToLower(tag)] = true
			combinedTags = append(combinedTags, tag)
		}
	}

	return combinedTags
}

func formatForClient(productId string, originalProduct Product, enhanced EnhancedProduct, processingTime time.Duration, taxonomyCtx *TaxonomyPrompt) AIEnhancementResponse {
	// Parsear tags del producto original
	originalTags := parseTagsFromString(getStringValue(originalProduct.Tags))
	originalVisualTags := parseTagsFromString(getStringValue(originalProduct.VisualTags))

	// PARALELO: name improvement + visual tags corren al mismo tiempo (ambos llaman a Claude ~1s cada uno)
	type nameResult struct{ name string }
	type tagsResult struct{ tags []string }
	nameCh := make(chan nameResult, 1)
	tagsCh := make(chan tagsResult, 1)

	go func() {
		nameCh <- nameResult{improveProductName(originalProduct.Name, enhanced.Keywords)}
	}()
	go func() {
		tagsCh <- tagsResult{generateVisualTagsWithBothSources(&originalProduct, enhanced.Keywords, enhanced.TechnicalDocs, enhanced.SearchResults, enhanced.CommercialContent)}
	}()

	nr := <-nameCh
	tr := <-tagsCh
	enhancedName := nr.name
	enhancedTags := combineUniqueTags(originalTags, enhanced.Keywords)
	enhancedVisualTags := combineUniqueTags(originalVisualTags, tr.tags)

	// SISTEMA UNIFICADO: Usar contenido comercial generado por Claude
	// Claude ya recibe todas las fuentes (fichas técnicas > PIM > web scraping > nombre)
	// y genera contenido adaptado al tipo de producto
	var shortDesc, fullDesc string
	if enhanced.CommercialContent != nil {
		// Para shortDescription: usar highlightBullets como características principales en formato de lista
		shortDesc = buildShortDescriptionFromBullets(enhanced.CommercialContent)
		fullDesc = enhanced.CommercialContent.LongDescription
	}

	// Fallback solo si CommercialContent falló completamente
	if shortDesc == "" {
		shortDesc = generateImprovedShortDescription(originalProduct, enhanced.TechnicalDocs, enhanced.SearchResults, enhanced.CommercialContent)
	}
	if fullDesc == "" {
		fullDesc = generateStructuredLongDescription(originalProduct, enhanced.TechnicalDocs, enhanced.SearchResults, enhanced.CommercialContent)
	}
	// Último recurso: step 4 siempre genera EnhancedDescription (con fallback propio).
	// Cubre productos con datos mínimos donde extractKeyFeatures y generateProductNarrative
	// también retornan vacío (ej. hardware, bisagras, etc. sin technical docs ni search results).
	if fullDesc == "" && enhanced.EnhancedDescription != "" {
		fullDesc = enhanceTextWithFormatting(enhanced.EnhancedDescription)
		log.Printf("⚠️ fullDesc fallback to EnhancedDescription for %s", originalProduct.Sku)
	}

	// VALIDACIÓN FINAL: Preservar marca solo si estaba mencionada en el nombre original
	originalBrand := getBrandNameFromDatabase(originalProduct.BrandId)
	if originalBrand != "" && strings.Contains(strings.ToLower(originalProduct.Name), strings.ToLower(originalBrand)) {
		if !strings.Contains(strings.ToLower(enhancedName), strings.ToLower(originalBrand)) {
			enhancedName = incorporateBrandInName(enhancedName, originalBrand)
			log.Printf("✅ MARCA PRESERVADA: %s en producto %s", originalBrand, originalProduct.Sku)
		}
	}

	// VALIDACIÓN FINAL: Consistencia de datos
	finalValidation := map[string]interface{}{
		"name":             enhancedName,
		"shortDescription": shortDesc,
		"fullDescription":  fullDesc,
	}

	finalInconsistencies := validateProductConsistency(originalProduct, finalValidation)
	if len(finalInconsistencies) > 0 {
		log.Printf("🚨 INCONSISTENCIAS FINALES en producto %s:", originalProduct.Sku)
		for _, inconsistency := range finalInconsistencies {
			log.Printf("   - %s", inconsistency)
		}
	}

	ruleApplied := RuleAppliedInfo{Level: "general"}
	if taxonomyCtx != nil {
		level := "department"
		if taxonomyCtx.SubCategory != "" {
			level = "subcategory"
		} else if taxonomyCtx.Category != "" {
			level = "category"
		}
		ruleApplied = RuleAppliedInfo{
			Level:       level,
			ID:          taxonomyCtx.ID,
			Department:  taxonomyCtx.Department,
			Category:    taxonomyCtx.Category,
			SubCategory: taxonomyCtx.SubCategory,
			Tone:        taxonomyCtx.Tone,
			Format:      taxonomyCtx.Format,
		}
	}

	return AIEnhancementResponse{
		ProductId: productId,
		OriginalData: OriginalDataFormat{
			Name:             originalProduct.Name,
			Sku:              originalProduct.Sku,
			ShortDescription: getStringValue(originalProduct.ShortDescription),
			FullDescription:  getStringValue(originalProduct.FullDescription),
			Tags:             originalTags,
			VisualTags:       originalVisualTags,
		},
		EnhancedData: EnhancedDataFormat{
			Name:              enhancedName,
			Sku:               originalProduct.Sku,
			ShortDescription:  shortDesc,
			FullDescription:   fullDesc,
			Tags:              enhancedTags,
			VisualTags:        enhancedVisualTags,
			CommercialContent: enhanced.CommercialContent,
			TituloSEO: func() string {
				t := getSEOTitle(enhanced.CommercialContent)
				if t == "" {
					t = getSEOTitleFallback(enhancedName)
				}
				return t
			}(),
			DescripcionSEO: func() string {
				d := getSEODescription(enhanced.CommercialContent)
				if d == "" {
					d = getSEODescriptionFallback(shortDesc)
				}
				return d
			}(),
		},
		Confidence:     calculateConfidence(enhanced),
		ProcessingTime: processingTime.Milliseconds(),
		RuleApplied:    ruleApplied,
	}
}

func parseTagsFromString(tagsStr string) []string {
	if tagsStr == "" {
		return []string{}
	}

	// Intentar parsear como JSON primero
	var tags []string
	if err := json.Unmarshal([]byte(tagsStr), &tags); err == nil {
		return tags
	}

	// Si falla, dividir por comas
	tags = []string{}
	parts := regexp.MustCompile(`[,;]`).Split(tagsStr, -1)
	for _, part := range parts {
		if trimmed := regexp.MustCompile(`^\s+|\s+$`).ReplaceAllString(part, ""); trimmed != "" {
			tags = append(tags, trimmed)
		}
	}

	return tags
}

func getStringValue(ptr *string) string {
	if ptr == nil {
		return ""
	}
	return *ptr
}

// applySpanishGrammarRules aplica reglas gramaticales del español a nombres de productos
func applySpanishGrammarRules(productName string) string {
	if productName == "" {
		return productName
	}

	// Limpiar comillas extras del inicio y final
	cleanName := strings.Trim(productName, "\"")

	// Aplicar reglas de capitalización en español
	cleanName = fixSpanishCapitalization(cleanName)

	// Agregar comillas después de medidas para indicar pulgadas
	cleanName = addInchQuotesToMeasurements(cleanName)

	// Corregir espaciado
	cleanName = fixSpacing(cleanName)

	return cleanName
}

// fixSpanishCapitalization aplica reglas de capitalización del español
func fixSpanishCapitalization(text string) string {
	if text == "" {
		return text
	}

	// Convertir a minúsculas primero
	text = strings.ToLower(text)

	// Palabras que siempre van en mayúscula (marcas, medidas, etc.)
	alwaysCapital := []string{
		"pack", "led", "pvc", "abs", "uv", "ac", "dc", "usb", "wifi", "gps",
		"3d", "hd", "4k", "xl", "xxl", "s", "m", "l",
	}

	// Palabras que van en minúscula (preposiciones, artículos, etc.)
	alwaysLower := []string{
		"de", "del", "la", "el", "los", "las", "en", "con", "por", "para",
		"y", "o", "u", "a", "ante", "bajo", "cabe", "contra", "desde",
		"durante", "entre", "hacia", "hasta", "mediante", "mod", "ref",
	}

	words := strings.Fields(text)

	for i, word := range words {
		// Preservar números y medidas
		if regexp.MustCompile(`^\d+(\.\d+)?$`).MatchString(word) {
			continue
		}

		// Preservar medidas con formato como "20x30", "1/2", "3/8"
		if regexp.MustCompile(`^\d+[x/]\d+$`).MatchString(word) {
			continue
		}

		// Primera palabra siempre en mayúscula
		if i == 0 {
			words[i] = strings.Title(word)
			continue
		}

		wordLower := strings.ToLower(word)

		// Verificar si debe ir en mayúscula
		shouldCapitalize := false
		for _, capital := range alwaysCapital {
			if wordLower == strings.ToLower(capital) {
				words[i] = strings.ToUpper(word)
				shouldCapitalize = true
				break
			}
		}

		if shouldCapitalize {
			continue
		}

		// Verificar si debe permanecer en minúscula
		shouldLower := false
		for _, lower := range alwaysLower {
			if wordLower == lower {
				words[i] = wordLower
				shouldLower = true
				break
			}
		}

		if shouldLower {
			continue
		}

		// Por defecto, capitalizar sustantivos, adjetivos y verbos
		words[i] = strings.Title(word)
	}

	return strings.Join(words, " ")
}

// addInchQuotesToMeasurements agrega comillas dobles después de medidas para indicar pulgadas
func addInchQuotesToMeasurements(text string) string {
	result := text

	// Aplicar patrón para medidas con 'x' (como 20x30, 10 x 5)
	result = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*x\s*(\d+(?:\.\d+)?)`).ReplaceAllStringFunc(result, func(match string) string {
		// Si ya tiene comillas, no agregar
		if strings.Contains(match, "\"") {
			return match
		}

		// Extraer las medidas
		parts := regexp.MustCompile(`(\d+(?:\.\d+)?)\s*x\s*(\d+(?:\.\d+)?)`).FindStringSubmatch(match)
		if len(parts) >= 3 {
			return parts[1] + "x" + parts[2] + "\""
		}
		return match
	})

	// Aplicar patrón para fracciones (como 1/2, 3/8, 1-1/2)
	result = regexp.MustCompile(`(\d+(?:-\d+)?/\d+)(?:\s|$)`).ReplaceAllStringFunc(result, func(match string) string {
		if strings.Contains(match, "\"") {
			return match
		}

		parts := regexp.MustCompile(`(\d+(?:-\d+)?/\d+)`).FindStringSubmatch(match)
		if len(parts) >= 2 {
			// Preservar el espacio al final si lo había
			if strings.HasSuffix(match, " ") {
				return parts[1] + "\" "
			}
			return parts[1] + "\""
		}
		return match
	})

	// Aplicar patrón para números decimales aislados que podrían ser medidas
	result = regexp.MustCompile(`\b(\d+(?:\.\d+)?)\s+(inch|pulgada)s?\b`).ReplaceAllString(result, `$1"`)

	return strings.TrimSpace(result)
}

// fixSpacing corrige el espaciado en el texto
func fixSpacing(text string) string {
	// Remover espacios múltiples
	text = regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")

	// Corregir comillas múltiples consecutivas (dejar solo una)
	text = regexp.MustCompile(`"+`).ReplaceAllString(text, "\"")

	// Corregir espacios alrededor de comillas
	text = regexp.MustCompile(`\s+"`).ReplaceAllString(text, "\"")
	text = regexp.MustCompile(`"\s+`).ReplaceAllString(text, "\" ")

	// Asegurar espacio después de comillas al final de medidas
	text = regexp.MustCompile(`"(\w)`).ReplaceAllString(text, "\" $1")

	return strings.TrimSpace(text)
}

func improveProductName(originalName string, keywords []string) string {
	if originalName == "" {
		return originalName
	}

	// Intentar mejora inteligente con Claude AI
	improvedName, err := improveProductNameWithClaude(originalName)
	if err != nil {
		log.Printf("Error improving product name with Claude: %v, using fallback", err)
		// Fallback: aplicar reglas básicas sin agregar keywords duplicados
		return applyBasicNameImprovement(originalName)
	}

	return improvedName
}

// ═══════════════════════════════════════════════════════════════════════════════
// callClaudeWithCache — Anthropic Prompt Caching
// Separa el prompt en:
//   - systemPrompt (estático, instrucciones que NO cambian entre productos)
//     → se marca con cache_control: {type: "ephemeral"} → cacheado 5 min en Anthropic
//   - userMessage (dinámico, datos específicos del producto)
//
// Resultado: TTFT reducido hasta 85% en llamadas subsecuentes que usen
//
//	el mismo system prompt.
//
// ═══════════════════════════════════════════════════════════════════════════════
func callClaudeWithCache(systemPrompt, userMessage string, maxTokens int) (*ClaudeResponse, error) {
	request := CachedClaudeRequest{
		Model: "claude-haiku-4-5-20251001",
		System: []SystemContentBlock{
			{
				Type: "text",
				Text: systemPrompt,
				CacheControl: &CacheControl{
					Type: "ephemeral",
				},
			},
		},
		Messages: []Message{
			{
				Role:    "user",
				Content: userMessage,
			},
		},
		MaxTokens: maxTokens,
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("error marshaling cached request: %v", err)
	}

	body, err := postClaude(context.Background(), requestBody)
	if err != nil {
		return nil, err
	}

	var claudeResp ClaudeResponse
	if err := json.Unmarshal(body, &claudeResp); err != nil {
		return nil, fmt.Errorf("error parsing response: %v", err)
	}

	// Log cache hits para verificar que funciona
	if claudeResp.Usage != nil {
		if claudeResp.Usage.CacheReadInputTokens > 0 {
			log.Printf("⚡ CACHE HIT: %d tokens leídos de caché (ahorro ~85%% TTFT)", claudeResp.Usage.CacheReadInputTokens)
		} else if claudeResp.Usage.CacheCreationInputTokens > 0 {
			log.Printf("📦 CACHE CREATED: %d tokens cacheados (próximas llamadas serán ~85%% más rápidas)", claudeResp.Usage.CacheCreationInputTokens)
		}
	}

	return &claudeResp, nil
}

// improveProductNameWithClaude usa Claude AI para mejorar nombres de productos inteligentemente
func improveProductNameWithClaude(originalName string) (string, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return "", fmt.Errorf("ANTHROPIC_API_KEY not configured")
	}

	prompt := fmt.Sprintf(`You are an expert in e-commerce product naming. Your task is to create a clear and professional product name.

CRITICAL: The output name MUST BE IN SPANISH. Instructions are in English for better processing accuracy.

ORIGINAL NAME: "%s"

UNIVERSAL PRINCIPLES:
1. Analyze what type of product it is (appliance, hardware, tool, decoration, etc.)
2. Improve clarity and readability while maintaining all essential information
3. Use natural Spanish capitalization: first word capitalized, main nouns capitalized
4. Preserve ALL technical information: measurements, quantities, models, codes
5. Translate ONLY COMPLETE and CLEAR words (WHITE→Blanco, BLACK→Negro)
6. NEVER translate abbreviations or incomplete words - keep them EXACTLY as they are
7. Keep technical terms and brands in their original language
8. WORK ONLY with information present in the original name
9. FORBIDDEN: using external knowledge or additional product context
10. NEVER add capacities, measurements, or specifications not present
11. If there is ANY doubt about a word's meaning, keep it in ORIGINAL form

CRITICAL - FINISH/MATERIAL TERMS THAT MUST NEVER BE TRANSLATED:
These are hardware/product finish names — NEVER translate them:
- "satin nickel" or "satin ni" → KEEP AS "Níquel Satinado" (it is a FINISH, not fabric)
- "brushed nickel" → "Níquel Cepillado"
- "oil rubbed bronze" → "Bronce Frotado"
- "matte black" → "Negro Mate"
- "polished chrome" → "Cromo Pulido"
- "antique brass" → "Latón Antiguo"
NOTE: "satin" in hardware context = a FINISH (acabado satinado), NOT the fabric "satén"

CRITICAL - MEASUREMENT ABBREVIATIONS IN PRODUCT DIMENSIONS:
- "l" after " = length (largo), "w" = width (ancho), "h" = height (alto)
- Example: '1.75"l x 2.65"w x 4"h' means 1.75" largo x 2.65" ancho x 4" alto
- CORRECT: "Soporte 1.75" L x 2.65" A x 4" H" or keep dimensions as-is
- NEVER interpret dimension notation as fabric or clothing measurements

CRITICAL - TECHNICAL TERMS THAT MUST NEVER BE TRANSLATED:
These terms are industry-standard and must ALWAYS remain in their original form:
- "No Frost" → KEEP AS "No Frost" (NEVER "Sin Escarcha" or "Libre de Escarcha")
- "Frost Free" → KEEP AS "Frost Free"
- "Top Mount" → KEEP AS "Top Mount"
- "French Door" → KEEP AS "French Door"
- "Side by Side" → KEEP AS "Side by Side"
- "All-in-One" → KEEP AS "All-in-One"
- "Smart" → KEEP AS "Smart"
- "Bluetooth" → KEEP AS "Bluetooth"
- "Wi-Fi" → KEEP AS "Wi-Fi"
- "LED" → KEEP AS "LED"
- "LCD" → KEEP AS "LCD"
- "Full HD" → KEEP AS "Full HD"
- "Air Fryer" → KEEP AS "Air Fryer"
- "Inverter" → KEEP AS "Inverter"
- "Blackout" → KEEP AS "Blackout"
- Any technology name (Total Fresh Flow, Home Energy Saver, etc.)
VIOLATION OF THIS RULE IS A CRITICAL ERROR.

CRITICAL - SIZE/PORTION TERMS (TRANSLATE MEANING, NOT LITERALLY):
- "King Size" → "Tamaño King" or "Tamaño Grande" (NEVER "Rey de..." which is nonsense)
- "Fun Size" → "Tamaño Mini" or "Tamaño Diversión"
- "Family Size" → "Tamaño Familiar"
- "Snack Size" → "Tamaño Snack"
- "Share Size" → "Para Compartir"
These are SIZE DESCRIPTORS, not literal phrases to translate word-by-word.

CRITICAL - BRAND NAMES MUST NEVER BE MODIFIED:
Keep ALL brand names exactly as written: M&M's, Coca-Cola, 3M, Stanley, Oreo, etc.

CRITICAL - ABBREVIATIONS MUST STAY IN THE FINAL NAME:
- NEVER translate or expand abbreviations - their meaning is UNCERTAIN
- NEVER REMOVE abbreviations - they are ESSENTIAL product identifiers
- Keep abbreviations in the final name, only fix capitalization: "BRUSH N." → "Brush N."
- Only translate COMPLETE words: "WHITE" → "Blanco", "BLACK" → "Negro"
- MANDATORY EXAMPLES (abbreviations MUST appear in final name):
  * "ABANICO DEMPSEY 52" BRUSH N." → "Abanico Dempsey 52" Brush N." ✓ CORRECT
  * "ABANICO DEMPSEY 52" BRUSH N." → "Abanico Dempsey de 52"" ✗ WRONG (removed Brush N.!)
  * "REFRIGERADOR SS 20 CF" → "Refrigerador SS 20 CF" ✓ CORRECT
- REMOVING an abbreviation = LOSING important product information = ERROR

CRITICAL - PACKAGING QUANTITIES:
- Formats like "6 x 24", "12 x 6" indicate quantity packaging (e.g., 6 boxes of 24 units)
- Preserve this information clearly: "Caja 6x24" or "6 paquetes de 24 unidades"

MANDATORY PRESERVATION OF QUANTITIES AND MEASUREMENTS (CRITICAL):
- If there is "2pc", "2 pack", "10 pack", "5 pieces", "3pcs" → ALWAYS include in final name
- Common quantity formats to preserve: "2pc", "3pcs", "2 pack", "set of 2", "juego de 3"
- If there are measurements "1/4"", "3/8"", "76x96"" → ALWAYS include in final name
- Acceptable output formats: "Set de 2", "2 Piezas", "Paquete de 10", "Juego de 5"
- CORRECT examples:
  * "2pc cortina 76x96 mabel" → "Set de 2 Cortinas Mabel de 76 x 96"" ✓ (keeps quantity!)
  * "2 pack 1/4"" bisagras" → "Set de 2 Bisagras de 1/4""
  * "10 pack 3/8"" tornillos" → "Paquete de 10 Tornillos de 3/8""
- WRONG examples:
  * "2pc cortina 76x96 mabel" → "Cortina Mabel de 76 x 96"" ✗ (removed "2pc"!)
- CRITICAL: Quantities (2pc, 3pcs, etc.) are ESSENTIAL - NEVER remove them

CRITICAL TECHNICAL FORMAT:
- Measurements: ALWAYS preserve symbols " ' ft mm cm in ° / x EXACTLY
- IMPORTANT: In product names, "" means INCHES, NOT centimeters
- CRITICAL examples:
  * 1-1/4" MUST be kept EXACTLY as 1-1/4" (NOT "1-1/4 pulgadas" NOR "1-1/4 cm")
  * 37x96"" MUST be kept as 37 x 96" (NOT "37 x 96 cm" NOR "37 x 96")
  * MANDATORY: If original has measurements with ", result MUST include the " symbol
- Quantities: keep pack, set, pc, units
- Models: preserve exact alphanumeric codes
- Clean extra quotes: ""text"" → "text", but preserve measurements: 3/8" → 3/8"
- Remove unnecessary dots and spaces at the end
- ABSOLUTE RULE: NEVER replace measurement symbols (") with words or other units

SMART APPROACH:
- If name is clear → improve only capitalization and format
- If confusing → reorganize for better understanding
- NEVER ADD information not in original name
- NEVER INVENT measurements, capacities, or specifications
- KEEP original name if in doubt

CRITICAL FINAL REMINDERS:
1. If original name contains measurements with symbols (like 37x96"", 1/4""),
   the improved name MUST include those exact symbols in the main name.
2. If original name contains quantities (like "2 pack", "10 units"),
   the improved name MUST include that quantity in the main name.
3. If original name contains ABBREVIATIONS (like "BRUSH N.", "SS", "BLK"),
   the improved name MUST include those abbreviations - DO NOT remove them!
4. PERFECT EXAMPLE: "ABANICO DEMPSEY 52" BRUSH N." → "Abanico Dempsey 52" Brush N."
5. WRONG EXAMPLE: "ABANICO DEMPSEY 52" BRUSH N." → "Abanico Dempsey de 52"" (ERROR: removed BRUSH N.)
6. NEVER omit measurements, quantities, or abbreviations - they are key product identifiers.

IMPORTANT: Respond ONLY with the improved product name IN SPANISH.
DO NOT add prefixes like "Improved name:", "Response:", explanations, or additional text.
Only the clean, improved name.`, originalName)

	request := ClaudeRequest{
		Model: "claude-haiku-4-5-20251001",
		Messages: []Message{
			{
				Role:    "user",
				Content: prompt,
			},
		},
		MaxTokens: 150,
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("error marshaling request: %v", err)
	}

	callCtx, cancelCall := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelCall()
	respBody, err := postClaude(callCtx, requestBody)
	if err != nil {
		return "", err
	}

	var claudeResponse ClaudeResponse
	if err := json.Unmarshal(respBody, &claudeResponse); err != nil {
		return "", fmt.Errorf("error decoding response: %v", err)
	}

	if len(claudeResponse.Content) == 0 {
		return "", fmt.Errorf("empty response from Claude")
	}

	// Limpiar y validar la respuesta
	improvedName := strings.TrimSpace(claudeResponse.Content[0].Text)
	improvedName = strings.Trim(improvedName, "\"'")

	// Validación de seguridad: si el resultado es muy diferente al original, usar fallback
	if !isValidNameImprovement(originalName, improvedName) {
		log.Printf("Claude improved name seems invalid, using fallback. Original: %s, Improved: %s", originalName, improvedName)
		return applyBasicNameImprovement(originalName), nil
	}

	log.Printf("Product name improved by Claude: '%s' → '%s'", originalName, improvedName)

	// Post-procesamiento: restaurar medidas con símbolos exactos
	finalName := restoreMeasurementSymbols(originalName, improvedName)
	if finalName != improvedName {
		log.Printf("✅ MEDIDAS RESTAURADAS: '%s' → '%s'", improvedName, finalName)
	}

	return finalName, nil
}

// restoreMeasurementSymbols restaura símbolos de medidas faltantes del nombre original
func restoreMeasurementSymbols(originalName, improvedName string) string {
	result := improvedName

	// Paso 1: Limpiar comillas dobles extras primero (""" → ")
	result = regexp.MustCompile(`""+`).ReplaceAllString(result, `"`)

	// Paso 2: Extraer todas las medidas del nombre original (normalizadas)
	originalMeasurements := extractNormalizedMeasurements(originalName)

	// Paso 3: Para cada medida original, restaurar símbolo si falta
	for _, measurement := range originalMeasurements {
		measurementWithoutSymbol := strings.TrimSuffix(measurement, `"`)

		// Verificar si la medida sin símbolo está presente en el resultado
		if strings.Contains(result, measurementWithoutSymbol) {
			// Verificar que no esté ya seguida de símbolo de pulgadas
			if !strings.Contains(result, measurement) {
				// Reemplazar solo la última aparición para evitar duplicaciones
				result = strings.Replace(result, measurementWithoutSymbol, measurement, 1)
				log.Printf("🔧 Símbolo restaurado: %s → %s", measurementWithoutSymbol, measurement)
			}
		}
	}

	return result
}

// extractNormalizedMeasurements extrae y normaliza medidas con símbolos del nombre original
func extractNormalizedMeasurements(productName string) []string {
	measurements := []string{}

	// Patrón mejorado para medidas individuales: captura 1-1/4"", 3/8"", etc.
	// Incluye variaciones con "", ", '' y '
	measurementPattern := `\d+(?:-\d+)?(?:/\d+)?(?:""|"|''|')`
	matches := regexp.MustCompile(measurementPattern).FindAllString(productName, -1)

	for _, match := range matches {
		// Normalizar: convertir todas las variaciones a formato estándar con "
		normalized := regexp.MustCompile(`""+|''+|'+`).ReplaceAllString(match, `"`)
		if normalized != "" && !contains(measurements, normalized) {
			measurements = append(measurements, normalized)
		}
	}

	// Patrón para dimensiones: captura 37x96"", 12x24", etc.
	dimensionPattern := `\d+(?:-\d+)?(?:/\d+)?\s*[xX×]\s*\d+(?:-\d+)?(?:/\d+)?(?:""|"|''|')`
	dimensionMatches := regexp.MustCompile(dimensionPattern).FindAllString(productName, -1)

	for _, match := range dimensionMatches {
		// Normalizar espacios y símbolos
		normalized := regexp.MustCompile(`\s+`).ReplaceAllString(match, " ")
		normalized = regexp.MustCompile(`[xX×]`).ReplaceAllString(normalized, "x")
		normalized = regexp.MustCompile(`""+|''+|'+`).ReplaceAllString(normalized, `"`)
		normalized = strings.TrimSpace(normalized)

		if normalized != "" && !contains(measurements, normalized) {
			measurements = append(measurements, normalized)
		}
	}

	return measurements
}

// applyBasicNameImprovement aplica mejoras básicas como fallback
func applyBasicNameImprovement(originalName string) string {
	// Limpiar espacios extras
	improved := regexp.MustCompile(`\s+`).ReplaceAllString(strings.TrimSpace(originalName), " ")

	// Corregir comillas dobles para pulgadas
	improved = regexp.MustCompile(`(\d+)""`).ReplaceAllString(improved, `$1"`)

	// Capitalizar palabras apropiadamente
	words := strings.Fields(improved)
	for i, word := range words {
		wordLower := strings.ToLower(word)

		// Preservar números con medidas como "52"", "20x30", "1/2"
		if regexp.MustCompile(`^\d+[x/\-\.]?\d*["A-Za-z]*$`).MatchString(word) {
			// Solo corregir comillas dobles en medidas
			words[i] = regexp.MustCompile(`""`).ReplaceAllString(word, `"`)
			continue
		}

		// Códigos específicos que deben capitalizarse de cierta manera
		if isProductCode(wordLower) {
			words[i] = capitalizeProductCode(word)
			continue
		}

		// Capitalizar primera letra para palabras principales
		if len(word) > 0 {
			if i == 0 || !isCommonLowercaseWord(wordLower) {
				words[i] = strings.ToUpper(string(word[0])) + strings.ToLower(word[1:])
			} else {
				words[i] = wordLower
			}
		}
	}

	return strings.Join(words, " ")
}

// isProductCode identifica códigos de producto comunes
func isProductCode(word string) bool {
	codes := []string{"brush", "white", "black", "chrome", "bronze", "nickel", "brass", "led", "rgb"}
	for _, code := range codes {
		if strings.Contains(word, code) {
			return true
		}
	}
	return false
}

// capitalizeProductCode aplica capitalización adecuada a códigos de producto
func capitalizeProductCode(word string) string {
	// Casos especiales para códigos comunes
	wordLower := strings.ToLower(word)

	specialCases := map[string]string{
		"brush":  "Brush",
		"white":  "White",
		"black":  "Black",
		"chrome": "Chrome",
		"bronze": "Bronze",
		"nickel": "Nickel",
		"brass":  "Brass",
		"led":    "LED",
		"rgb":    "RGB",
	}

	for key, value := range specialCases {
		if strings.Contains(wordLower, key) {
			return strings.ReplaceAll(wordLower, key, value)
		}
	}

	// Capitalización por defecto
	if len(word) > 0 {
		return strings.ToUpper(string(word[0])) + strings.ToLower(word[1:])
	}
	return word
}

// isCommonLowercaseWord verifica si una palabra debe ir en minúscula (lista básica)
func isCommonLowercaseWord(word string) bool {
	commonWords := []string{"de", "del", "la", "el", "los", "las", "en", "con", "por", "para", "y", "o"}
	for _, common := range commonWords {
		if word == common {
			return true
		}
	}
	return false
}

// isValidNameImprovement valida que la mejora del nombre sea razonable
func isValidNameImprovement(original, improved string) bool {
	// El nombre mejorado no puede estar vacío
	if strings.TrimSpace(improved) == "" {
		return false
	}

	// El nombre mejorado no puede ser excesivamente más largo
	if len(improved) > len(original)*2 {
		return false
	}

	// Verificar que no haya duplicaciones obvias (solo palabras significativas)
	improvedLower := strings.ToLower(improved)
	words := strings.Fields(strings.ToLower(original))

	for _, word := range words {
		// Solo verificar palabras significativas > 4 caracteres, excluyendo números y medidas
		if len(word) > 4 && !regexp.MustCompile(`^\d+`).MatchString(word) && !regexp.MustCompile(`["/'-]`).MatchString(word) {
			if strings.Count(improvedLower, word) > 2 { // Permitir hasta 2 repeticiones
				return false // Duplicación excesiva detectada
			}
		}
	}

	return true
}

// extractProductSpecs extrae especificaciones relevantes priorizando fuentes oficiales
func extractProductSpecs(product Product, technicalDocs *TechnicalDocumentResult, searchResults []map[string]interface{}) []string {
	specs := []string{}

	// PRIORIDAD 1: Extraer de fichas técnicas primero
	techSpecs := extractSpecsFromTechnicalDocs(technicalDocs)

	// PRIORIDAD 2: Complementar con datos del producto original
	productSpecs := extractSpecsFromProductData(product)

	// PRIORIDAD 3: Solo si no hay suficiente info, usar web (filtrado)
	webSpecs := []string{}
	if len(techSpecs)+len(productSpecs) < 3 {
		webSpecs = extractSpecsFromWebResults(searchResults, product)
	}

	// Combinar priorizando fuentes oficiales
	specs = append(specs, techSpecs...)

	// Agregar specs de producto que no dupliquen las técnicas
	for _, productSpec := range productSpecs {
		if !isDuplicateSpec(productSpec, specs) {
			specs = append(specs, productSpec)
		}
	}

	// Agregar specs web que no dupliquen las existentes
	for _, webSpec := range webSpecs {
		if !isDuplicateSpec(webSpec, specs) && len(specs) < 6 {
			specs = append(specs, webSpec)
		}
	}

	return specs
}

// isExtractedTextUseful verifica si el texto extraído de PDF es útil o corrupto
func isExtractedTextUseful(text string) bool {
	if len(text) < 50 {
		return false
	}

	// Contar palabras reales vs caracteres sueltos
	words := regexp.MustCompile(`\b[a-zA-ZáéíóúñüÁÉÍÓÚÑÜ]{3,}\b`).FindAllString(text, -1)
	wordCount := len(words)

	// Debe tener al menos 10 palabras reales
	if wordCount < 10 {
		return false
	}

	// Verificar ratio de caracteres útiles
	printableChars := 0
	for _, r := range text {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == ' ' {
			printableChars++
		}
	}

	ratio := float64(printableChars) / float64(len(text))
	return ratio > 0.4 // Al menos 40% de caracteres útiles
}

// extractSpecsFromTechnicalDocs extrae especificaciones de fichas técnicas con máxima precisión
func extractSpecsFromTechnicalDocs(technicalDocs *TechnicalDocumentResult) []string {
	if technicalDocs == nil {
		return []string{}
	}

	specs := []string{}
	techText := ""

	// LOG: Verificar si hay fichas técnicas disponibles
	if technicalDocs.AvailableCount == 0 {
		log.Printf("⚠️ NO HAY FICHAS TÉCNICAS DISPONIBLES")
		return specs
	}

	log.Printf("📋 PROCESANDO %d FICHAS TÉCNICAS:", technicalDocs.AvailableCount)

	// Combinar solo texto de documentos técnicos
	for _, doc := range technicalDocs.Documents {
		if doc.IsAvailable && doc.ExtractedData != "" {
			log.Printf("📄 Ficha encontrada: %s (%s) - %d caracteres", doc.ResourceName, doc.MediaType, len(doc.ExtractedData))
			techText += doc.ExtractedData + " "
		}
	}

	if techText == "" {
		log.Printf("❌ NO SE EXTRAJO CONTENIDO DE LAS FICHAS TÉCNICAS")
		return specs
	}

	log.Printf("📋 TEXTO TOTAL DE FICHAS: %d caracteres", len(techText))

	// Verificar si el texto extraído es legible
	if !isExtractedTextUseful(techText) {
		log.Printf("❌ TEXTO DE FICHAS TÉCNICAS NO ES LEGIBLE (posible PDF escaneado/protegido)")
		log.Printf("📋 MUESTRA DEL CONTENIDO: >%s", truncateString(techText, 100))
		return specs // Retornar vacío para que use otras fuentes
	}

	// Patrones PRECISOS para fichas técnicas oficiales
	patterns := map[string]*regexp.Regexp{
		// Especificaciones técnicas críticas con unidades exactas
		"Peso":        regexp.MustCompile(`(?i)(?:peso|weight)[:\s-]*([0-9]+(?:[.,][0-9]+)?)\s*(kg|kilogramos?|g|gramos?|lb|libras?)`),
		"Dimensiones": regexp.MustCompile(`(?i)(?:dimensiones?|medidas?|dimensions?|size)[:\s-]*([0-9]+(?:[.,][0-9]+)?\s*[x×]\s*[0-9]+(?:[.,][0-9]+)?(?:\s*[x×]\s*[0-9]+(?:[.,][0-9]+)?)?)\s*(cm|mm|m|in|pulgadas?|inches?)`),
		"Capacidad":   regexp.MustCompile(`(?i)(?:capacidad|capacity)[:\s-]*([0-9]+(?:[.,][0-9]+)?)\s*(pies(?:\s*c[úu]bicos?)?|ft³?|litros?|L|ml|galones?|gal)`),
		"Voltaje":     regexp.MustCompile(`(?i)(?:voltaje|voltage|volt)[:\s-]*([0-9]+(?:[.,][0-9]+)?)\s*(v|voltios?|volts?)`),
		"Potencia":    regexp.MustCompile(`(?i)(?:potencia|power|wattage)[:\s-]*([0-9]+(?:[.,][0-9]+)?)\s*(w|watts?|kw|kilowatts?|hp)`),
		"Corriente":   regexp.MustCompile(`(?i)(?:corriente|current|amperaje)[:\s-]*([0-9]+(?:[.,][0-9]+)?)\s*(a|amp|amper|amperes?)`),
		"Temperatura": regexp.MustCompile(`(?i)(?:temperatura|temperature|temp)[:\s-]*([0-9\-+]+(?:[.,][0-9]+)?)\s*(°?c|celsius|°?f|fahrenheit)`),
		"Presión":     regexp.MustCompile(`(?i)(?:presi[óo]n|pressure)[:\s-]*([0-9]+(?:[.,][0-9]+)?)\s*(psi|bar|pa|kpa|mpa)`),
		"Velocidad":   regexp.MustCompile(`(?i)(?:velocidad|speed|rpm)[:\s-]*([0-9]+(?:[.,][0-9]+)?)\s*(rpm|m/s|km/h|mph)`),
		"Eficiencia":  regexp.MustCompile(`(?i)(?:eficiencia|efficiency|rating)[:\s-]*([A-Z\+\-]+|\d+(?:[.,]\d+)?%?)`),

		// Información descriptiva técnica
		"Material":      regexp.MustCompile(`(?i)(?:material|materials?)[:\s-]*([a-záéíóúñ\s,]{5,50})`),
		"Acabado":       regexp.MustCompile(`(?i)(?:acabado|finish|coating)[:\s-]*([a-záéíóúñ\s]{3,25})`),
		"Tecnología":    regexp.MustCompile(`(?i)(?:tecnolog[íi]a|technology|system)[:\s-]*([a-záéíóúñ\s]{5,40})`),
		"Certificación": regexp.MustCompile(`(?i)(?:certificaci[óo]n|certification|standard)[:\s-]*([A-Z0-9\s\-/]{3,25})`),
	}

	// Extraer con máxima precisión y logging detallado
	for label, pattern := range patterns {
		matches := pattern.FindStringSubmatch(techText)
		if len(matches) >= 3 {
			value := strings.TrimSpace(matches[1])
			unit := strings.TrimSpace(matches[2])
			if value != "" {
				spec := fmt.Sprintf("%s: %s %s", label, value, unit)
				specs = append(specs, spec)
				log.Printf("✅ EXTRAÍDO DE FICHA TÉCNICA: %s", spec)
			}
		} else if len(matches) == 2 {
			// Para campos sin unidades como Material, Acabado, etc.
			value := strings.TrimSpace(matches[1])
			if value != "" && len(value) > 2 {
				spec := fmt.Sprintf("%s: %s", label, value)
				specs = append(specs, spec)
				log.Printf("✅ EXTRAÍDO DE FICHA TÉCNICA: %s", spec)
			}
		}
	}

	if len(specs) == 0 {
		log.Printf("❌ NO SE PUDIERON EXTRAER ESPECIFICACIONES DE LAS FICHAS TÉCNICAS")
		log.Printf("📋 MUESTRA DEL CONTENIDO: %s", func() string {
			if len(techText) > 200 {
				return techText[:200] + "..."
			}
			return techText
		}())
	} else {
		log.Printf("✅ EXTRAÍDAS %d ESPECIFICACIONES DE FICHAS TÉCNICAS", len(specs))
	}

	return specs
}

// extractSpecsFromProductData extrae especificaciones básicas del producto
func extractSpecsFromProductData(product Product) []string {
	specs := []string{}
	productText := ""

	// Combinar descripciones del producto
	if getStringValue(product.FullDescription) != "" {
		productText += getStringValue(product.FullDescription) + " "
	}
	if getStringValue(product.ShortDescription) != "" {
		productText += getStringValue(product.ShortDescription) + " "
	}

	// Patrones UNIVERSALES para cualquier tipo de producto
	patterns := map[string]*regexp.Regexp{
		// Información básica aplicable a cualquier producto
		"Tipo":       regexp.MustCompile(`(?i)tipo[:\s]*([a-záéíóúñ\s]{3,25})`),
		"Material":   regexp.MustCompile(`(?i)material[:\s]*([a-záéíóúñ\s]{3,25})`),
		"Color":      regexp.MustCompile(`(?i)color[:\s]*([a-záéíóúñ\s]{3,20})`),
		"Sistema":    regexp.MustCompile(`(?i)sistema[:\s]*([a-záéíóúñ\s]{3,30})`),
		"Tecnología": regexp.MustCompile(`(?i)tecnolog[íi]a[:\s]*([a-záéíóúñ\s]{3,35})`),

		// Medidas universales
		"Medida":    regexp.MustCompile(`(?i)(?:medida|medidas|tamaño)[:\s]*([0-9/"'\s\-x×]+(?:cm|mm|in|pulgadas?)?)`),
		"Capacidad": regexp.MustCompile(`(?i)capacidad[:\s]*([0-9.,\s]*(pies³?|ft³?|litros?|L|galones?|ml|oz))`),

		// Especificaciones técnicas opcionales
		"Cantidad": regexp.MustCompile(`(?i)(?:cantidad|pack|set|juego)[:\s]*([0-9]+)\s*(?:piezas?|unidades?|pc|pcs)?`),
		"Diseño":   regexp.MustCompile(`(?i)diseño[:\s]*([a-záéíóúñ\s]{3,25})`),
	}

	// Buscar cada patrón en el texto del producto
	for label, pattern := range patterns {
		if matches := pattern.FindStringSubmatch(productText); len(matches) > 1 {
			value := strings.TrimSpace(matches[1])
			// Validar que el valor no esté vacío y sea significativo
			if value != "" && len(value) > 1 && !strings.HasPrefix(value, ":") {
				spec := label + ": " + value
				// Validar longitud total de la especificación
				if len(spec) > 8 && len(spec) < 100 {
					specs = append(specs, spec)
				}
			}
		}
	}

	return specs
}

// extractSpecsFromWebResults extrae especificaciones de resultados web de forma conservadora
func extractSpecsFromWebResults(searchResults []map[string]interface{}, product Product) []string {
	specs := []string{}
	productBrand := extractProductBrand(product)
	webText := ""

	// Filtrar y combinar solo contenido relevante
	for _, result := range searchResults {
		if title, ok := result["title"].(string); ok {
			if description, ok := result["description"].(string); ok {
				// Solo usar si no es de competidor
				if productBrand == "" || !isContentFromCompetitor(title+" "+description, productBrand) {
					webText += title + " " + description + " "
				} else {
					log.Printf("🚫 COMPETITOR CONTENT FILTERED: Brand='%s', Content='%s'", productBrand, title)
				}
			}
		}
	}

	// Patrones muy conservadores para información web
	patterns := map[string]*regexp.Regexp{
		"Característica": regexp.MustCompile(`(?i)(?:característica|feature)[:\s]*([a-záéíóúñ\s]{5,30})`),
		"Aplicación":     regexp.MustCompile(`(?i)(?:aplicación|uso|para)[:\s]*([a-záéíóúñ\s]{5,25})`),
	}

	for label, pattern := range patterns {
		if matches := pattern.FindStringSubmatch(webText); len(matches) > 1 {
			value := strings.TrimSpace(matches[1])
			if value != "" && len(value) > 4 && len(value) < 40 {
				spec := label + ": " + value
				specs = append(specs, spec)
			}
		}
	}

	return specs
}

// isDuplicateSpec verifica si una especificación ya existe en la lista
func isDuplicateSpec(newSpec string, existingSpecs []string) bool {
	newSpecLower := strings.ToLower(newSpec)

	for _, existing := range existingSpecs {
		existingLower := strings.ToLower(existing)

		// Verificar duplicados exactos
		if existingLower == newSpecLower {
			return true
		}

		// Verificar si es el mismo tipo de especificación (ej: dos "Peso:")
		newParts := strings.Split(newSpec, ":")
		existingParts := strings.Split(existing, ":")

		if len(newParts) > 0 && len(existingParts) > 0 {
			newType := strings.TrimSpace(newParts[0])
			existingType := strings.TrimSpace(existingParts[0])
			if strings.EqualFold(newType, existingType) {
				return true
			}
		}
	}

	return false
}

// enhanceTextWithFormatting añade etiquetas HTML de formato (strong, em) al texto
func enhanceTextWithFormatting(text string) string {
	if text == "" {
		return ""
	}

	// Patrones para identificar texto que debe ir en negrita (strong)
	strongPatterns := []struct {
		pattern *regexp.Regexp
		replace string
	}{
		// Especificaciones técnicas (Tipo:, Medidas:, etc.)
		{regexp.MustCompile(`(?i)\b(tipo|medidas?|dimensiones?|capacidad|voltaje|potencia|peso|material|color|marca|modelo|serie|código|referencia):\s*([^.\n,]+)`), `<strong>$1:</strong> $2`},
		// Nombres de productos o marcas al inicio de párrafos
		{regexp.MustCompile(`(?i)\b([A-Z][A-Za-z]+(?:\s+[A-Z][A-Za-z]+)*)\s+(refrigerador|nevera|estufa|lavadora|secadora|horno|microondas|lavavajillas|aire\s+acondicionado|ventilador|calentador|bomba|motor|compresor|válvula|filtro|sensor|control|panel|display|led|lcd|inverter|smart|inteligente)`), `<strong>$1</strong> $2`},
		// Números de modelo, serie o referencia
		{regexp.MustCompile(`(?i)\b(modelo|serie|ref\.?|referencia|código|part\s+number|sku)[\s:]+([A-Z0-9\-]+)`), `<strong>$1:</strong> $2`},
		// Características principales (términos técnicos importantes)
		{regexp.MustCompile(`(?i)\b(inverter|no\s+frost|multi\s+air\s+flow|linear\s+cooling|door\s+cooling|smart\s+inverter|energy\s+star|led\s+lighting|acero\s+inoxidable|digital|automático|programable|eficiencia\s+energética)\b`), `<strong>$1</strong>`},
		// Medidas y capacidades
		{regexp.MustCompile(`(?i)\b(\d+(?:\.\d+)?)\s*(litros?|pies\s*cúbicos?|ft³?|kg|libras?|lbs?|watts?|w|voltios?|v|amperios?|amp|hz|btu|cfm|rpm)\b`), `<strong>$1 $2</strong>`},
	}

	// Patrones para identificar texto que debe ir en cursiva (em)
	emPatterns := []struct {
		pattern *regexp.Regexp
		replace string
	}{
		// Beneficios y características destacadas
		{regexp.MustCompile(`(?i)\b(ideal\s+para|perfecto\s+para|diseñado\s+para|garantiza|asegura|proporciona|ofrece|permite|incluye|cuenta\s+con|equipado\s+con)\s+([^.\n,]+)`), `<em>$1</em> $2`},
		// Términos de funcionalidad y comodidad
		{regexp.MustCompile(`(?i)\b(fácil\s+instalación|instalación\s+sencilla|bajo\s+mantenimiento|alta\s+durabilidad|larga\s+vida\s+útil|resistente\s+al\s+óxido|resistente\s+a\s+la\s+corrosión|antibacteriano|antihongos|silencioso|ultra\s+silencioso|económico|eco\s+friendly|ecológico|sustentable)\b`), `<em>$1</em>`},
		// Aplicaciones y usos
		{regexp.MustCompile(`(?i)\b(aplicaciones?|usos?|recomendado\s+para|adecuado\s+para|compatible\s+con)\s+([^.\n,]+)`), `<em>$1</em> $2`},
		// Términos de calidad y certificaciones
		{regexp.MustCompile(`(?i)\b(certificado|aprobado|homologado|garantía|calidad\s+premium|calidad\s+superior|alta\s+calidad|profesional|industrial|comercial|residencial)\b`), `<em>$1</em>`},
	}

	result := text

	// Aplicar patrones de negrita (strong)
	for _, pattern := range strongPatterns {
		result = pattern.pattern.ReplaceAllString(result, pattern.replace)
	}

	// Aplicar patrones de cursiva (em)
	for _, pattern := range emPatterns {
		result = pattern.pattern.ReplaceAllString(result, pattern.replace)
	}

	// Limpiar etiquetas anidadas o duplicadas
	result = cleanNestedTags(result)

	return result
}

// cleanNestedTags limpia etiquetas HTML anidadas o duplicadas
func cleanNestedTags(text string) string {
	// Remover etiquetas anidadas <strong> dentro de <strong>
	text = regexp.MustCompile(`<strong>([^<]*)<strong>([^<]*)</strong>([^<]*)</strong>`).ReplaceAllString(text, `<strong>$1$2$3</strong>`)

	// Remover etiquetas anidadas <em> dentro de <em>
	text = regexp.MustCompile(`<em>([^<]*)<em>([^<]*)</em>([^<]*)</em>`).ReplaceAllString(text, `<em>$1$2$3</em>`)

	// Remover etiquetas duplicadas consecutivas
	text = regexp.MustCompile(`</strong>\s*<strong>`).ReplaceAllString(text, " ")
	text = regexp.MustCompile(`</em>\s*<em>`).ReplaceAllString(text, " ")

	return text
}

// convertTextToHTML convierte texto con bullets y saltos de línea a HTML
func convertTextToHTML(text string) string {
	if text == "" {
		return ""
	}

	// Primero aplicar formato de negrita y cursiva
	text = enhanceTextWithFormatting(text)

	// Dividir en líneas
	lines := strings.Split(text, "\n")
	var htmlBuilder strings.Builder

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Detectar si es una viñeta
		if strings.HasPrefix(line, "•") || strings.HasPrefix(line, "-") || strings.HasPrefix(line, "*") {
			// Remover el bullet character al inicio
			bulletText := strings.TrimSpace(line[1:])
			htmlBuilder.WriteString("<li>" + sanitizeHTML(bulletText) + "</li>")
		} else {
			// Texto normal como párrafo
			htmlBuilder.WriteString("<p>" + sanitizeHTML(line) + "</p>")
		}
	}

	// Si hay elementos li, envolverlos en ul
	result := htmlBuilder.String()
	if strings.Contains(result, "<li>") {
		result = strings.ReplaceAll(result, "<li>", "<ul><li>")
		result = strings.ReplaceAll(result, "</li>", "</li></ul>")
		// Corregir múltiples ul anidados
		result = strings.ReplaceAll(result, "</ul><ul>", "")
	}

	return result
}

// buildShortDescriptionFromBullets construye la descripción corta usando los highlightBullets
// que contienen las características más importantes identificadas por la IA
// buildShortDescriptionFromBullets construye la descripción corta en formato <ul><li>.
// SOLO usa bullets con formato "Label: Valor" (contienen ":").
// Si no hay bullets Label:Valor, devuelve "" para que formatForClient use el fallback
// que genera specs estructuradas desde los datos del producto.
func buildShortDescriptionFromBullets(content *CommercialContent) string {
	if content == nil {
		return ""
	}

	// Solo aceptar bullets con formato "Label: Valor" — descartar texto narrativo
	var labelValueBullets []string
	for _, bullet := range content.HighlightBullets {
		bullet = strings.TrimSpace(bullet)
		if bullet == "" || isGenericPlaceholderContent(bullet) {
			continue
		}
		// Requerir formato "Label: Valor" (contiene ":" que no sea al final)
		colonIdx := strings.Index(bullet, ":")
		if colonIdx > 0 && colonIdx < len(bullet)-1 {
			labelValueBullets = append(labelValueBullets, bullet)
		}
	}

	// Hay suficientes bullets Label:Valor → devolver como lista HTML
	if len(labelValueBullets) >= 2 {
		var sb strings.Builder
		sb.WriteString("<ul>\n")
		for _, bullet := range labelValueBullets {
			sb.WriteString("<li>" + sanitizeHTML(bullet) + "</li>\n")
		}
		sb.WriteString("</ul>")
		return sb.String()
	}

	// Sin bullets Label:Valor → devolver "" para que el fallback genere specs estructuradas
	// (el fallback usa generateImprovedShortDescription que extrae specs del producto)
	return ""
}

func generateImprovedShortDescription(product Product, technicalDocs *TechnicalDocumentResult, searchResults []map[string]interface{}, commercialContent *CommercialContent) string {
	// PRIORIDAD 1: commercialContent.ShortDescription — SOLO si tiene formato Label:Valor
	// Si es texto narrativo (no contiene ":"), ignorar y generar specs desde datos del producto
	if commercialContent != nil && len(commercialContent.ShortDescription) > 20 {
		shortDesc := commercialContent.ShortDescription
		if !isGenericPlaceholderContent(shortDesc) && strings.Contains(shortDesc, ":") {
			// Tiene formato Label:Valor → formatear como <ul><li>
			var items []string
			for _, line := range regexp.MustCompile(`\n|\.\s+`).Split(shortDesc, -1) {
				line = strings.TrimSpace(line)
				if len(line) > 3 && strings.Contains(line, ":") {
					items = append(items, line)
				}
			}
			if len(items) >= 2 {
				var sb strings.Builder
				sb.WriteString("<ul>\n")
				for _, item := range items {
					sb.WriteString("<li>" + sanitizeHTML(item) + "</li>\n")
				}
				sb.WriteString("</ul>")
				return sb.String()
			}
		}
		// Texto narrativo sin ":" → ignorar, continuar con specs del producto
	}

	// PRIORIDAD 2: Specs técnicas generadas desde datos del producto (siempre Label:Valor)
	context := analyzeProductContext(product, technicalDocs, searchResults)
	keySpecs := generateSmartSpecs(context, product, technicalDocs, searchResults, commercialContent)
	keySpecs = filterGenericSpecs(keySpecs)

	if len(keySpecs) < 5 {
		additionalSpecs := extractAdditionalSpecs(context, product, technicalDocs, searchResults, commercialContent, keySpecs)
		additionalSpecs = filterGenericSpecs(additionalSpecs)
		keySpecs = append(keySpecs, additionalSpecs...)
		keySpecs = deduplicateAndLimit(keySpecs, 8)
	}

	// PRIORIDAD 3: Si no hay specs, generar mínimo desde keywords y nombre del producto
	if len(keySpecs) == 0 {
		keySpecs = buildMinimalSpecsFromProduct(product, commercialContent)
	}

	if len(keySpecs) == 0 {
		return ""
	}

	// SIEMPRE devolver en formato <ul><li> — nunca texto plano
	var sb strings.Builder
	sb.WriteString("<ul>\n")
	for _, spec := range keySpecs {
		sb.WriteString("<li>" + sanitizeHTML(spec) + "</li>\n")
	}
	sb.WriteString("</ul>")
	return sb.String()
}

// buildMinimalSpecsFromProduct genera specs básicas Label:Valor cuando no hay otra fuente
func buildMinimalSpecsFromProduct(product Product, commercialContent *CommercialContent) []string {
	var specs []string

	// Tipo de producto desde el nombre
	name := strings.ToLower(product.Name)
	tipo := extractProductType(name)
	if tipo != "" {
		specs = append(specs, "Tipo: "+tipo)
	}

	// Marca desde BD si existe
	if product.BrandName != "" && !isUUIDString(product.BrandName) {
		specs = append(specs, "Marca: "+product.BrandName)
	} else {
		specs = append(specs, "Marca: Sin Marca")
	}

	// Usar keywords de commercialContent como specs adicionales
	if commercialContent != nil {
		for _, attr := range commercialContent.Attributes {
			attr = strings.TrimSpace(attr)
			if len(attr) > 3 {
				specs = append(specs, attr)
			}
			if len(specs) >= 4 {
				break
			}
		}
	}

	// Medidas del nombre si las hay
	measurements := extractMeasurements(product.Name)
	for _, m := range measurements {
		specs = append(specs, "Medida: "+m)
		if len(specs) >= 5 {
			break
		}
	}

	return specs
}

// extractProductType extrae el tipo/categoría del nombre del producto
func extractProductType(nameLower string) string {
	types := map[string]string{
		"bisagra": "Bisagra", "bisagras": "Bisagra", "candado": "Candado",
		"candados": "Candado", "tirador": "Tirador", "tornillo": "Tornillo",
		"cerradura": "Cerradura", "manija": "Manija", "perno": "Perno",
		"clavija": "Clavija", "gancho": "Gancho", "soporte": "Soporte",
	}
	for key, val := range types {
		if strings.Contains(nameLower, key) {
			return val
		}
	}
	return ""
}

// extractMeasurements extrae medidas del nombre del producto
func extractMeasurements(name string) []string {
	re := regexp.MustCompile(`\d+[-/]?\d*["''´]?\s*(?:mm|cm|pulgadas?|in\.?|inches?)?`)
	matches := re.FindAllString(name, -1)
	var results []string
	seen := make(map[string]bool)
	for _, m := range matches {
		m = strings.TrimSpace(m)
		if m != "" && !seen[m] {
			results = append(results, m)
			seen[m] = true
		}
	}
	return results
}

// isGenericPlaceholderContent verifica si el contenido es genérico/placeholder sin valor
func isGenericPlaceholderContent(content string) bool {
	lowerContent := strings.ToLower(content)
	genericPatterns := []string{
		"tipo: producto",
		"tipo:producto",
		"modelo: tradicional",
		"modelo: estándar",
		"color: variados",
		"color: es variados",
		"color: varios",
	}
	for _, pattern := range genericPatterns {
		if strings.Contains(lowerContent, pattern) {
			return true
		}
	}
	return false
}

// filterGenericSpecs filtra especificaciones genéricas que no aportan valor
func filterGenericSpecs(specs []string) []string {
	var filtered []string
	for _, spec := range specs {
		if !isGenericPlaceholderContent(spec) {
			filtered = append(filtered, spec)
		}
	}
	return filtered
}

// ProductContext representa el análisis contextual del producto
type ProductContext struct {
	ProductType      string   // Tipo de producto identificado
	InfoQuality      string   // "rich", "moderate", "minimal"
	AvailableSources []string // Fuentes de información disponibles
	MissingData      []string // Tipos de datos faltantes
	Strategy         string   // Estrategia recomendada
}

// analyzeProductContext analiza contexto disponible y determina estrategia
func analyzeProductContext(product Product, technicalDocs *TechnicalDocumentResult, searchResults []map[string]interface{}) ProductContext {
	context := ProductContext{
		AvailableSources: []string{},
		MissingData:      []string{},
	}

	// Identificar tipo de producto desde el nombre
	context.ProductType = identifyProductType(product.Name)

	// Analizar calidad de información disponible
	var infoScore int

	// Evaluar fichas técnicas
	if technicalDocs != nil && technicalDocs.AvailableCount > 0 {
		context.AvailableSources = append(context.AvailableSources, "technical_docs")
		infoScore += 40 // Las fichas técnicas son la fuente más valiosa
	} else {
		context.MissingData = append(context.MissingData, "technical_specs")
	}

	// Evaluar datos existentes del producto
	if product.ShortDescription != nil && len(*product.ShortDescription) > 20 {
		context.AvailableSources = append(context.AvailableSources, "short_description")
		infoScore += 15
	}
	if product.FullDescription != nil && len(*product.FullDescription) > 50 {
		context.AvailableSources = append(context.AvailableSources, "full_description")
		infoScore += 15
	}
	if product.PIMAttributes != nil && len(*product.PIMAttributes) > 30 {
		context.AvailableSources = append(context.AvailableSources, "pim_attributes")
		infoScore += 20
	}

	// Evaluar nombre del producto
	if len(product.Name) > 10 {
		context.AvailableSources = append(context.AvailableSources, "product_name")
		infoScore += 10
	}

	// Evaluar resultados de búsqueda web
	if len(searchResults) > 0 {
		context.AvailableSources = append(context.AvailableSources, "web_search")
		infoScore += 5
	}

	// Determinar calidad general de información
	if infoScore >= 50 {
		context.InfoQuality = "rich"
		context.Strategy = "extract_and_refine"
	} else if infoScore >= 25 {
		context.InfoQuality = "moderate"
		context.Strategy = "complement_and_enhance"
	} else {
		context.InfoQuality = "minimal"
		context.Strategy = "infer_and_generate"
	}

	// Identificar datos faltantes comunes
	if !contains(context.AvailableSources, "technical_docs") {
		context.MissingData = append(context.MissingData, "specifications")
	}

	return context
}

// identifyProductType identifica el tipo de producto basado en el nombre
func identifyProductType(productName string) string {
	name := strings.ToUpper(productName)

	// Categorizar productos por tipo
	productTypes := map[string][]string{
		"electrodomestico": {"REFRIGERADOR", "NEVERA", "ESTUFA", "HORNO", "MICROONDAS", "LAVADORA", "SECADORA"},
		"hardware":         {"BISAGRA", "TIRADOR", "MANIJA", "TORNILLO", "TUERCA", "CERRADURA"},
		"decoracion":       {"ALFOMBRA", "CORTINA", "COJIN", "ESPEJO", "CUADRO", "LAMPARA"},
		"herramienta":      {"TALADRO", "DESTORNILLADOR", "MARTILLO", "SIERRA", "LLAVE"},
		"iluminacion":      {"FOCO", "LED", "BOMBILLO", "LAMPARA", "LUZ", "REFLECTOR"},
		"plomeria":         {"LLAVE", "TUBO", "VALVULA", "GRIFO", "MANGUERA", "CONECTOR"},
		"jardin":           {"MANGUERA", "ASPERSOR", "MACETA", "TIERRA", "SEMILLA"},
	}

	for productType, keywords := range productTypes {
		for _, keyword := range keywords {
			if strings.Contains(name, keyword) {
				return productType
			}
		}
	}

	return "general"
}

// generateSmartSpecs genera especificaciones usando estrategia inteligente
func generateSmartSpecs(context ProductContext, product Product, technicalDocs *TechnicalDocumentResult, searchResults []map[string]interface{}, commercialContent *CommercialContent) []string {
	var specs []string

	switch context.Strategy {
	case "extract_and_refine":
		// Información rica: extraer y refinar
		specs = extractUniversalKeySpecs(product, technicalDocs, searchResults, commercialContent)

	case "complement_and_enhance":
		// Información moderada: extraer lo que hay + completar con IA
		baseSpecs := extractUniversalKeySpecs(product, technicalDocs, searchResults, commercialContent)
		enhancedSpecs := enhanceSpecsWithAI(context, product, baseSpecs)
		specs = append(baseSpecs, enhancedSpecs...)

	case "infer_and_generate":
		// Información mínima: generar usando IA + nombre del producto
		nameSpecs := extractFromProductName(product.Name)
		aiSpecs := generateSpecsFromAI(context, product)
		specs = append(nameSpecs, aiSpecs...)

	default:
		// Fallback al método original
		specs = extractUniversalKeySpecs(product, technicalDocs, searchResults, commercialContent)
	}

	// Deduplicar pero mantener suficientes especificaciones
	return deduplicateAndLimit(specs, 8) // Máximo 8 para permitir selección
}

// enhanceSpecsWithAI usa IA para completar especificaciones faltantes
func enhanceSpecsWithAI(context ProductContext, product Product, existingSpecs []string) []string {
	// Por ahora retornar vacío - implementar cuando sea necesario
	return []string{}
}

// generateSpecsFromAI genera especificaciones usando solo IA y contexto
func generateSpecsFromAI(context ProductContext, product Product) []string {
	// Por ahora retornar vacío - implementar cuando sea necesario
	return []string{}
}

// deduplicateAndLimit remueve duplicados por tipo de especificación y limita cantidad
func deduplicateAndLimit(specs []string, maxCount int) []string {
	unique := []string{}
	seenTypes := make(map[string]bool)

	// Ordenar por prioridad: Material, Modelo, Tecnología, Capacidad, etc.
	priorityOrder := []string{"material", "modelo", "medida", "capacidad", "cantidad", "tecnología", "sistema", "color", "acabado", "voltaje", "potencia", "peso", "dimensiones"}
	priorityMap := make(map[string]int)
	for i, p := range priorityOrder {
		priorityMap[p] = i
	}

	// Separar specs por prioridad
	prioritizedSpecs := make([][]string, len(priorityOrder)+1) // +1 para otros

	for _, spec := range specs {
		specType := ""
		if colonIndex := strings.Index(spec, ":"); colonIndex != -1 {
			specType = strings.TrimSpace(strings.ToLower(spec[:colonIndex]))
		} else {
			specType = strings.TrimSpace(strings.ToLower(spec))
		}

		if priority, exists := priorityMap[specType]; exists {
			prioritizedSpecs[priority] = append(prioritizedSpecs[priority], spec)
		} else {
			prioritizedSpecs[len(priorityOrder)] = append(prioritizedSpecs[len(priorityOrder)], spec)
		}
	}

	// Agregar en orden de prioridad, evitando duplicados
	for _, specGroup := range prioritizedSpecs {
		for _, spec := range specGroup {
			specType := ""
			if colonIndex := strings.Index(spec, ":"); colonIndex != -1 {
				specType = strings.TrimSpace(strings.ToLower(spec[:colonIndex]))
			} else {
				specType = strings.TrimSpace(strings.ToLower(spec))
			}

			if !seenTypes[specType] && len(unique) < maxCount {
				unique = append(unique, spec)
				seenTypes[specType] = true
			}
		}
	}

	return unique
}

// extractAdditionalSpecs busca especificaciones adicionales cuando no hay suficientes
func extractAdditionalSpecs(context ProductContext, product Product, technicalDocs *TechnicalDocumentResult, searchResults []map[string]interface{}, commercialContent *CommercialContent, existingSpecs []string) []string {
	additionalSpecs := []string{}
	existingTypes := make(map[string]bool)

	// Mapear tipos ya existentes
	for _, spec := range existingSpecs {
		if colonIndex := strings.Index(spec, ":"); colonIndex != -1 {
			specType := strings.TrimSpace(strings.ToLower(spec[:colonIndex]))
			existingTypes[specType] = true
		}
	}

	// Buscar en contenido comercial si disponible
	if commercialContent != nil {
		for _, bullet := range commercialContent.HighlightBullets {
			if spec := parseSpecFromText(bullet); spec != "" {
				if colonIndex := strings.Index(spec, ":"); colonIndex != -1 {
					specType := strings.TrimSpace(strings.ToLower(spec[:colonIndex]))
					if !existingTypes[specType] {
						additionalSpecs = append(additionalSpecs, spec)
						existingTypes[specType] = true
					}
				}
			}
		}
	}

	// Si aún faltan, inferir especificaciones básicas del tipo de producto
	if len(existingSpecs)+len(additionalSpecs) < 5 {
		inferredSpecs := inferSpecsByProductType(context.ProductType, product, existingTypes)
		additionalSpecs = append(additionalSpecs, inferredSpecs...)
	}

	return additionalSpecs
}

// parseSpecFromText intenta extraer una especificación de un texto libre
func parseSpecFromText(text string) string {
	// Patrones para extraer especificaciones de texto libre
	patterns := map[string]*regexp.Regexp{
		"Capacidad": regexp.MustCompile(`(?i)capacidad[:\s]*([0-9]+(?:[.,][0-9]+)?\s*(?:pies|ft|litros?|L))`),
		"Modelo":    regexp.MustCompile(`(?i)modelo[:\s]*([A-Z0-9\-]{3,15})`),
		"Material":  regexp.MustCompile(`(?i)material[:\s]*([a-záéíóúñ\s]{3,20})`),
		"Color":     regexp.MustCompile(`(?i)color[:\s]*([a-záéíóúñ\s]{3,20})(?:\s|$|[,\.])`),
	}

	for label, pattern := range patterns {
		if match := pattern.FindStringSubmatch(text); len(match) > 1 {
			return label + ": " + strings.TrimSpace(match[1])
		}
	}

	return ""
}

// inferSpecsByProductType infiere especificaciones básicas según el tipo de producto
func inferSpecsByProductType(productType string, product Product, existingTypes map[string]bool) []string {
	specs := []string{}

	// Agregar especificaciones genéricas si no existen
	genericSpecs := []struct {
		type_ string
		value string
	}{
		{"marca", extractBrandFromName(product.Name)},
		{"tipo", getProductTypeDescription(productType)},
	}

	for _, generic := range genericSpecs {
		if !existingTypes[generic.type_] && generic.value != "" {
			specs = append(specs, strings.Title(generic.type_)+": "+generic.value)
			existingTypes[generic.type_] = true
		}
	}

	return specs
}

// extractBrandFromName extrae marca del nombre del producto
func extractBrandFromName(productName string) string {
	commonBrands := []string{"MABE", "WHIRLPOOL", "LG", "SAMSUNG", "BOSCH", "ELECTROLUX", "COCINOVA", "STERILITE"}
	nameUpper := strings.ToUpper(productName)

	for _, brand := range commonBrands {
		if strings.Contains(nameUpper, brand) {
			return strings.Title(strings.ToLower(brand))
		}
	}

	return ""
}

// getProductTypeDescription retorna descripción del tipo de producto
func getProductTypeDescription(productType string) string {
	typeDescriptions := map[string]string{
		"electrodomestico": "Electrodoméstico",
		"hardware":         "Accesorio de ferreteria",
		"decoracion":       "Artículo decorativo",
		"herramienta":      "Herramienta",
		"iluminacion":      "Iluminación",
		"plomeria":         "Accesorio de plomería",
		"jardin":           "Artículo de jardín",
	}

	if desc, exists := typeDescriptions[productType]; exists {
		return desc
	}

	return "Producto"
}

// extractUniversalKeySpecs extrae las especificaciones más relevantes para cualquier producto
// RESPETANDO ORDEN DE PRIORIDADES: 1) Fichas técnicas 2) Datos del producto 3) Nombre 4) Web filtrado
func extractUniversalKeySpecs(product Product, technicalDocs *TechnicalDocumentResult, searchResults []map[string]interface{}, commercialContent *CommercialContent) []string {
	specs := []string{}

	// PRIORIDAD 1: FICHAS TÉCNICAS (fuente más confiable)
	technicalText := ""
	if technicalDocs != nil {
		for _, doc := range technicalDocs.Documents {
			if doc.IsAvailable && doc.ExtractedData != "" {
				technicalText += " " + doc.ExtractedData
			}
		}
	}
	if technicalText != "" {
		techSpecs := extractMostRelevantSpecs(technicalText, product.Name)
		specs = append(specs, techSpecs...)
	}

	// PRIORIDAD 2: INFORMACIÓN EXISTENTE DEL PRODUCTO
	productText := ""
	if product.ShortDescription != nil && *product.ShortDescription != "" {
		// Limpiar HTML y verificar si tiene contenido útil
		cleanShort := cleanHTMLAndExtractText(*product.ShortDescription)
		if len(cleanShort) > 10 && !isPoorDescription(cleanShort) {
			productText += " " + cleanShort
		}
	}
	if product.FullDescription != nil && *product.FullDescription != "" {
		cleanFull := cleanHTMLAndExtractText(*product.FullDescription)
		if len(cleanFull) > 10 && !isPoorDescription(cleanFull) {
			productText += " " + cleanFull
		}
	}
	if productText != "" && len(specs) < 5 {
		productSpecs := extractMostRelevantSpecs(productText, product.Name)
		for _, spec := range productSpecs {
			if !containsSpec(specs, spec) {
				specs = append(specs, spec)
				if len(specs) >= 5 {
					break
				}
			}
		}
	}

	// PRIORIDAD 3: NOMBRE DEL PRODUCTO (información básica confiable)
	if len(specs) < 5 {
		nameSpecs := extractFromProductName(product.Name)
		for _, spec := range nameSpecs {
			if !containsSpec(specs, spec) {
				specs = append(specs, spec)
				if len(specs) >= 5 {
					break
				}
			}
		}
	}

	// PRIORIDAD 4: CONTENIDO COMERCIAL (solo como complemento)
	if commercialContent != nil && len(specs) < 5 {
		commercialText := ""
		if commercialContent.ShortDescription != "" {
			commercialText += " " + commercialContent.ShortDescription
		}
		if commercialText != "" {
			commercialSpecs := extractMostRelevantSpecs(commercialText, product.Name)
			for _, spec := range commercialSpecs {
				if !containsSpec(specs, spec) {
					specs = append(specs, spec)
					if len(specs) >= 5 {
						break
					}
				}
			}
		}
	}

	return specs[:min(len(specs), 5)] // Máximo 5 especificaciones
}

// cleanHTMLAndExtractText limpia HTML y extrae texto útil
func cleanHTMLAndExtractText(html string) string {
	// Remover etiquetas HTML
	text := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(html, " ")
	// Limpiar espacios múltiples
	text = regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

// isPoorDescription detecta descripciones pobres o genéricas
func isPoorDescription(text string) bool {
	lowerText := strings.ToLower(text)
	poorKeywords := []string{"cortas", "larga", "sin descripción", "producto", "artículo", "elemento"}
	for _, poor := range poorKeywords {
		if strings.Contains(lowerText, poor) && len(text) < 20 {
			return true
		}
	}
	return len(text) < 5
}

// containsSpec verifica si una especificación ya está en la lista
func containsSpec(specs []string, newSpec string) bool {
	newSpecLower := strings.ToLower(newSpec)
	for _, existing := range specs {
		existingLower := strings.ToLower(existing)
		// Verificar si es la misma especificación (mismo tipo)
		if strings.Contains(existingLower, ":") && strings.Contains(newSpecLower, ":") {
			existingType := strings.Split(existingLower, ":")[0]
			newType := strings.Split(newSpecLower, ":")[0]
			if existingType == newType {
				return true // Mismo tipo de especificación
			}
		}
		// Verificar contenido similar
		if strings.Contains(existingLower, newSpecLower) || strings.Contains(newSpecLower, existingLower) {
			return true
		}
	}
	return false
}

// extractMostRelevantSpecs usa AI inteligente para identificar las especificaciones más importantes
func extractMostRelevantSpecs(allText, productName string) []string {
	specs := []string{}

	// PATRONES UNIVERSALES DE ESPECIFICACIONES CRÍTICAS
	criticalPatterns := map[string]*regexp.Regexp{
		// MEDIDAS Y CAPACIDADES (UNIVERSAL) - LOS MÁS IMPORTANTES PRIMERO
		"Capacidad":      regexp.MustCompile(`(?i)(?:capacidad|capacity)[:\s]*([0-9]+(?:[.,][0-9]+)?)\s*(pies(?:\s*c[úu]bicos?)?|ft³?|litros?|L|ml|galones?|gal)`),
		"Peso":           regexp.MustCompile(`(?i)(?:peso|weight)[:\s]*([0-9]+(?:[.,][0-9]+)?)\s*(kg|lb|g|oz)`),
		"Medidas":        regexp.MustCompile(`(?i)(?:medida|medidas|tamaño|size)[:\s]*([0-9/\s×x"-]+(?:cm|mm|m|in|pulgadas?)?(?:\s*\([^)]+\))?)`),
		"Dimensiones":    regexp.MustCompile(`(?i)(?:dimensiones?|dimensions?)[:\s]*([0-9]+(?:[.,][0-9]+)?\s*[x×]\s*[0-9]+(?:[.,][0-9]+)?(?:\s*[x×]\s*[0-9]+(?:[.,][0-9]+)?)?\s*(?:cm|mm|m|in)?(?:\s*\([^)]+\))?)`),
		"Capacidad_Desc": regexp.MustCompile(`(?i)(?:capacidad|capacity)[:\s]*([0-9]+\s*(?:platos?|tazas?|vasos?|cubiertos?)(?:[^.]{0,50})?)`),

		// IDENTIFICACIÓN DEL PRODUCTO
		"Modelo": regexp.MustCompile(`(?i)\bmodelo\b[:\s]*([A-Z0-9\-]{2,15})`),

		// CARACTERÍSTICAS TÉCNICAS (UNIVERSAL)
		"Material":  regexp.MustCompile(`(?i)(?:acabado|material)[:\s]+([a-záéíóúñA-Z\s-]{3,60})(?:[\s]*[,\.]|$)`),
		"Color":     regexp.MustCompile(`(?i)(?:color|colour)[:\s]*([a-záéíóúñA-Z\s-]{3,40})(?:[\s]*[,\.]|$)`),
		"Voltaje":   regexp.MustCompile(`(?i)(?:voltaje|voltage)[:\s]*([0-9]+(?:[.,][0-9]+)?)\s*(?:v|voltios?|volts?)`),
		"Potencia":  regexp.MustCompile(`(?i)(?:potencia|power)[:\s]*([0-9]+(?:[.,][0-9]+)?)\s*(?:w|watts?|kw|hp)`),
		"Velocidad": regexp.MustCompile(`(?i)(?:velocidad|speed|rpm)[:\s]*([0-9]+(?:[.,][0-9]+)?)\s*(?:rpm|m/s|km/h)`),

		// TECNOLOGÍAS Y SISTEMAS (DETECTA AUTOMÁTICAMENTE) - EXPANDIDO
		"Tecnología":    regexp.MustCompile(`(?i)(?:tecnolog[íi]a|technology)[:\s]*([a-záéíóúñA-Z\s\+&-]{5,100})(?:[\s]*[,\.]|$)`),
		"Sistema":       regexp.MustCompile(`(?i)(?:sistema|system)[:\s]*([a-záéíóúñA-Z\s\+&-]{5,80})(?:[\s]*[,\.]|$)`),
		"Certificación": regexp.MustCompile(`(?i)(?:certificaci[óo]n|certification)[:\s]*([A-Z0-9\s\-]{3,25})(?:[\s]*[,\.]|$)`),
		"Eficiencia":    regexp.MustCompile(`(?i)(?:eficiencia|efficiency)[:\s]*([a-záéíóúñA-Z\+\-\s]{5,40})(?:[\s]*[,\.]|$)`),
	}

	// EXTRAER DEL NOMBRE DEL PRODUCTO PRIMERO (info más confiable)
	productInfo := extractFromProductName(productName)
	for _, info := range productInfo {
		if !contains(specs, info) {
			specs = append(specs, info)
		}
	}

	// EXTRAER USANDO PATRONES UNIVERSALES
	for label, pattern := range criticalPatterns {
		matches := pattern.FindStringSubmatch(allText)
		if len(matches) >= 2 {
			value := strings.TrimSpace(matches[1])
			if len(matches) >= 3 && strings.TrimSpace(matches[2]) != "" {
				// Incluir unidad si está presente
				value += " " + strings.TrimSpace(matches[2])
			}

			if isValueMeaningful(value) && !isPartialText(value) {
				specStr := label + ": " + value
				if !contains(specs, specStr) && len(specs) < 6 {
					specs = append(specs, specStr)
				}
			}
		}
	}

	// DETECTAR TECNOLOGÍAS ESPECÍFICAS SOLO PARA PRODUCTOS TECNOLÓGICOS
	if isElectricalProduct(productName) {
		specificTechs := detectSpecificTechnologies(allText)
		for _, tech := range specificTechs {
			techSpec := "Tecnología: " + tech
			if !contains(specs, techSpec) && len(specs) < 6 {
				specs = append(specs, techSpec)
			}
		}
	}

	return specs[:min(len(specs), 5)] // Máximo 5 especificaciones más relevantes
}

// isRefrigerationProduct determina si un producto es de refrigeración
func isRefrigerationProduct(productName string) bool {
	nameUpper := strings.ToUpper(productName)
	refrigerationKeywords := []string{"REFRIGERADOR", "NEVERA", "CONGELADOR", "FREEZER", "NO FROST", "FROST FREE"}
	for _, keyword := range refrigerationKeywords {
		if strings.Contains(nameUpper, keyword) {
			return true
		}
	}
	return false
}

// isElectricalProduct determina si un producto es tecnológico/eléctrico
func isElectricalProduct(productName string) bool {
	nameUpper := strings.ToUpper(productName)
	// Productos que NO son tecnológicos
	nonTechKeywords := []string{"ESCURRIDOR", "BANDEJA", "PLATO", "TAZA", "VASO", "CUCHILLO", "TENEDOR", "CUCHARA", "SARTEN", "OLLA", "RECIPIENTE", "ORGANIZADOR", "ESTANTE", "REPISA", "CANASTA", "CESTO", "TOALLA", "ALFOMBRA"}
	for _, keyword := range nonTechKeywords {
		if strings.Contains(nameUpper, keyword) {
			return false
		}
	}
	// Productos que SÍ son tecnológicos
	techKeywords := []string{"REFRIGERADOR", "NEVERA", "CONGELADOR", "MICROONDAS", "HORNO", "ESTUFA", "LAVADORA", "SECADORA", "LAVAVAJILLAS", "AIRE", "VENTILADOR", "CALENTADOR", "BOMBA", "MOTOR"}
	for _, keyword := range techKeywords {
		if strings.Contains(nameUpper, keyword) {
			return true
		}
	}
	return false // Por defecto, asumir que no es tecnológico para evitar "Smart" falsos
}

// extractFromProductName extrae información clave directamente del nombre del producto
func extractFromProductName(productName string) []string {
	info := []string{}
	name := strings.ToUpper(productName)

	// Detectar dimensiones primero (ej: 37x96", 12x24")
	if match := regexp.MustCompile(`(\d+(?:-\d+)?(?:/\d+)?)\s*[xX]\s*(\d+(?:-\d+)?(?:/\d+)?)\s*(?:""|"|PULGAD|INCH)`).FindStringSubmatch(name); len(match) > 2 {
		info = append(info, "Medidas: "+match[1]+"x"+match[2]+" pulgadas")
	} else if match := regexp.MustCompile(`(\d+(?:-\d+)?(?:/\d+)?)\s*(?:"|'|PULGAD|INCH)`).FindStringSubmatch(name); len(match) > 1 {
		// Solo si no hay dimensiones, detectar medidas individuales
		info = append(info, "Medida: "+match[1]+" pulgadas")
	}

	// Detectar capacidades - patrones más amplios
	if match := regexp.MustCompile(`(\d+(?:\.\d+)?)\s*(?:PIES|FT³?|PIE)`).FindStringSubmatch(name); len(match) > 1 {
		info = append(info, "Capacidad: "+match[1]+" pies cúbicos")
	}

	// Detectar capacidades numéricas SOLO para productos de refrigeración
	if isRefrigerationProduct(name) {
		if match := regexp.MustCompile(`\b(\d{1,2})(?:\s|$)`).FindStringSubmatch(name); len(match) > 1 {
			if num, _ := strconv.Atoi(match[1]); num >= 7 && num <= 30 {
				info = append(info, "Capacidad: "+match[1]+" pies cúbicos")
			}
		}
	}

	// Detectar cantidades/paquetes para productos que vienen en sets
	if match := regexp.MustCompile(`(?i)\b(\d+)\s*(?:pack|paquete|unidades?|piezas?)\b`).FindStringSubmatch(name); len(match) > 1 {
		info = append(info, "Cantidad: "+match[1]+" unidades")
	}

	// Detectar modelos
	if match := regexp.MustCompile(`\b([A-Z]{2,}[0-9]{2,}[A-Z0-9]*)\b`).FindStringSubmatch(name); len(match) > 1 {
		info = append(info, "Modelo: "+match[1])
	}

	// Detectar materiales/acabados comunes
	materials := []string{"ACERO", "INOXIDABLE", "GRAFITO", "BRONCE", "NÍQUEL", "CROMO", "PLÁSTICO", "MADERA"}
	for _, material := range materials {
		if strings.Contains(name, material) {
			info = append(info, "Material: "+strings.Title(strings.ToLower(material)))
		}
	}

	return info
}

// detectSpecificTechnologies busca tecnologías específicas mencionadas
func detectSpecificTechnologies(text string) []string {
	techs := []string{}
	text = strings.ToUpper(text)

	technologies := map[string]string{
		"NO FROST":          "No Frost",
		"TOTAL FRESH FLOW":  "Total Fresh Flow",
		"HOME ENERGY SAVER": "Home Energy Saver",
		"INVERTER":          "Inverter",
		"BLUETOOTH":         "Bluetooth",
		"WI-FI":             "Wi-Fi",
		"WIFI":              "Wi-Fi",
		"SMART":             "Smart",
		"DIGITAL":           "Digital",
		"AUTOMATICO":        "Automático",
		"SENSOR":            "Sensor",
	}

	for pattern, tech := range technologies {
		if strings.Contains(text, pattern) {
			techs = append(techs, tech)
		}
	}

	return techs
}

// isPartialText detecta si el texto capturado está truncado o incompleto
func isPartialText(value string) bool {
	value = strings.TrimSpace(value)

	// Detectar palabras cortadas (terminan de forma abrupta)
	if len(value) < 3 {
		return true
	}

	// Detectar si termina con palabras parciales comunes
	partialWords := []string{"para", "de", "es", "la", "el", "con", "sin", "por", "en"}
	for _, partial := range partialWords {
		if value == partial || strings.HasSuffix(strings.ToLower(value), " "+partial) {
			return true
		}
	}

	// Detectar texto que termina abruptamente (sin vocal al final para palabras españolas)
	if len(value) > 4 && strings.HasSuffix(value, "r") && !strings.HasSuffix(value, "or") && !strings.HasSuffix(value, "ar") {
		return true
	}

	return false
}

// Funciones auxiliares
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if strings.EqualFold(s, item) {
			return true
		}
	}
	return false
}

func isValueMeaningful(value string) bool {
	return len(value) > 1 && !strings.EqualFold(value, "de") && !strings.EqualFold(value, "si") && !strings.EqualFold(value, "no")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// extractSpecsFromCommercialContent extrae especificaciones del contenido comercial
func extractSpecsFromCommercialContent(commercialContent *CommercialContent) []string {
	specs := []string{}

	// Usar los highlightBullets como especificaciones
	for _, bullet := range commercialContent.HighlightBullets {
		if bullet != "" {
			specs = append(specs, bullet)
		}
	}

	// Si no hay highlightBullets, usar algunos attributes
	if len(specs) == 0 {
		maxAttributes := 4 // Limitar a 4 atributos principales
		for i, attr := range commercialContent.Attributes {
			if i >= maxAttributes {
				break
			}
			if attr != "" {
				specs = append(specs, attr)
			}
		}
	}

	return specs
}

// generateStructuredLongDescription crea una descripción larga estructurada según el formato requerido
func generateStructuredLongDescription(product Product, technicalDocs *TechnicalDocumentResult, searchResults []map[string]interface{}, commercialContent *CommercialContent) string {
	var longDesc strings.Builder

	// Obtener nombre del producto para validaciones internas
	productName := product.Name
	if productName != "" {
		productName = strings.Trim(productName, "\"")
		_ = productName // Variable utilizada para validaciones internas
	}

	// Omitir specs duplicados - ya están manejados en shortDescription

	// Usar información del commercialContent si está disponible y es más rica
	if commercialContent != nil {
		// Agregar bullets desarrollados como características principales
		if len(commercialContent.DevelopedBullets) > 0 {
			longDesc.WriteString("<ul>\n") // Eliminar subtítulo según feedback comercial
			for _, bullet := range commercialContent.DevelopedBullets {
				enhancedBullet := enhanceTextWithFormatting(bullet)
				longDesc.WriteString("<li>" + sanitizeHTML(enhancedBullet) + "</li>\n")
			}
			longDesc.WriteString("</ul>\n")
		}

		// Agregar descripción larga del commercialContent
		if commercialContent.LongDescription != "" {
			// Limpiar y formatear la descripción comercial
			cleanLongDesc := strings.Trim(commercialContent.LongDescription, "\"")
			// Dividir en líneas y agregar cada una
			lines := strings.Split(cleanLongDesc, "\n")
			for _, line := range lines {
				if strings.TrimSpace(line) != "" {
					enhancedLine := enhanceTextWithFormatting(strings.TrimSpace(line))
					longDesc.WriteString("<p>" + enhancedLine + "</p>\n")
				}
			}
		}
	} else {
		// Fallback al método original si no hay commercialContent
		features := extractKeyFeatures(product, technicalDocs, searchResults)

		// Agregar características como bullet points
		if len(features) > 0 {
			longDesc.WriteString("<ul>\n") // Eliminar subtítulo según feedback comercial
			for _, feature := range features {
				longDesc.WriteString("<li>" + sanitizeHTML(feature) + "</li>\n")
			}
			longDesc.WriteString("</ul>\n")
		}

		// Agregar descripción narrativa mejorada
		narrative := generateProductNarrative(product, technicalDocs, searchResults)
		if narrative != "" {
			longDesc.WriteString("<p>" + narrative + "</p>\n")
		}

		// Agregar características técnicas adicionales al final
		additionalFeatures := extractAdditionalFeatures(product, technicalDocs, searchResults)
		if len(additionalFeatures) > 0 {
			longDesc.WriteString("<ul>\n")
			for _, feature := range additionalFeatures {
				longDesc.WriteString("<li>" + sanitizeHTML(feature) + "</li>\n")
			}
			longDesc.WriteString("</ul>\n")
		}
	}

	result := strings.TrimSpace(longDesc.String())

	return result
}

// extractKeyFeatures extrae las características principales del producto
func extractKeyFeatures(product Product, technicalDocs *TechnicalDocumentResult, searchResults []map[string]interface{}) []string {
	features := []string{}
	allText := ""

	// Combinar texto de todas las fuentes
	if getStringValue(product.FullDescription) != "" {
		allText += getStringValue(product.FullDescription) + " "
	}

	if technicalDocs != nil {
		for _, doc := range technicalDocs.Documents {
			if doc.IsAvailable && doc.ExtractedData != "" {
				allText += doc.ExtractedData + " "
			}
		}
	}

	for _, result := range searchResults {
		if description, ok := result["description"].(string); ok {
			allText += description + " "
		}
	}

	// Patrones para identificar características importantes
	featurePatterns := []string{
		`(?i)(inverter|inversor)`,
		`(?i)(multi[\s-]?air[\s-]?flow|flujo[\s-]?de[\s-]?aire[\s-]?múltiple)`,
		`(?i)(linear[\s-]?cooling|enfriamiento[\s-]?lineal)`,
		`(?i)(door[\s-]?cooling|enfriamiento[\s-]?de[\s-]?puerta)`,
		`(?i)(smart[\s-]?inverter|inversor[\s-]?inteligente)`,
		`(?i)(no[\s-]?frost|libre[\s-]?de[\s-]?escarcha)`,
		`(?i)(led[\s-]?lighting|iluminación[\s-]?led)`,
		`(?i)(energy[\s-]?star|eficiencia[\s-]?energética)`,
	}

	for _, pattern := range featurePatterns {
		if matched := regexp.MustCompile(pattern).FindString(allText); matched != "" {
			// Buscar contexto alrededor de la característica
			context := extractFeatureContext(allText, matched)
			if context != "" {
				features = append(features, context)
			}
		}
	}

	return features
}

// extractFeatureContext extrae el contexto descriptivo de una característica
func extractFeatureContext(text, feature string) string {
	// Buscar oraciones que contengan la característica
	sentences := regexp.MustCompile(`[.!?]+\s*`).Split(text, -1)

	for _, sentence := range sentences {
		if regexp.MustCompile(`(?i)` + regexp.QuoteMeta(feature)).MatchString(sentence) {
			sentence = strings.TrimSpace(sentence)
			if len(sentence) > 20 && len(sentence) < 200 {
				return feature + ": " + sentence + "."
			}
		}
	}

	return feature
}

// generateProductNarrative genera texto narrativo del producto
func generateProductNarrative(product Product, technicalDocs *TechnicalDocumentResult, searchResults []map[string]interface{}) string {
	// Combinar información para crear narrativa
	allInfo := ""

	if getStringValue(product.FullDescription) != "" {
		allInfo += getStringValue(product.FullDescription) + " "
	}

	if technicalDocs != nil {
		for _, doc := range technicalDocs.Documents {
			if doc.IsAvailable && doc.ExtractedData != "" {
				allInfo += doc.ExtractedData + " "
			}
		}
	}

	// Extraer párrafos descriptivos
	paragraphs := regexp.MustCompile(`\n\s*\n`).Split(allInfo, -1)

	var bestParagraphs []string
	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if len(para) > 100 && len(para) < 500 {
			// Priorizar párrafos que hablen de beneficios y características
			if regexp.MustCompile(`(?i)(permite|ofrece|garantiza|asegura|proporciona|diseñado)`).MatchString(para) {
				bestParagraphs = append(bestParagraphs, para)
			}
		}
	}

	if len(bestParagraphs) > 0 {
		return strings.Join(bestParagraphs[:min(2, len(bestParagraphs))], "\n\n")
	}

	return ""
}

// extractAdditionalFeatures extrae características técnicas adicionales
func extractAdditionalFeatures(product Product, technicalDocs *TechnicalDocumentResult, searchResults []map[string]interface{}) []string {
	features := []string{}

	// Buscar características técnicas específicas en documentos
	if technicalDocs != nil {
		for _, doc := range technicalDocs.Documents {
			if doc.IsAvailable && doc.ExtractedData != "" {
				// Buscar listas de características
				lines := strings.Split(doc.ExtractedData, "\n")
				for _, line := range lines {
					line = strings.TrimSpace(line)
					// Identificar líneas que parecen características técnicas
					if len(line) > 10 && len(line) < 80 {
						if regexp.MustCompile(`^[A-Z][A-Za-z\s]+$`).MatchString(line) {
							features = append(features, line)
						}
					}
				}
			}
		}
	}

	return features
}

// validateVisualTags - función simple que solo elimina tags vacíos
func validateVisualTags(tags []string) []string {
	validTags := []string{}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag != "" {
			validTags = append(validTags, tag)
		}
	}
	return validTags
}

// generateVisualTagsWithBothSources combina información original del producto con información generada por Claude
func generateVisualTagsWithBothSources(originalProduct *Product, keywords []string, technicalDocs *TechnicalDocumentResult, searchResults []map[string]interface{}, commercialContent *CommercialContent) []string {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		log.Printf("⚠️ WARNING: ANTHROPIC_API_KEY not configured for visual tags")
		return []string{}
	}

	// COMBINAR toda la información disponible del producto
	var infoSources strings.Builder

	// 1. INFORMACIÓN ORIGINAL DEL PRODUCTO (fuente primaria)
	infoSources.WriteString("=== INFORMACIÓN ORIGINAL DEL PRODUCTO ===\n")
	if originalProduct.Name != "" {
		infoSources.WriteString(fmt.Sprintf("Nombre: %s\n", strings.Trim(originalProduct.Name, "\"")))
	}
	if originalProduct.Sku != "" {
		infoSources.WriteString(fmt.Sprintf("SKU: %s\n", originalProduct.Sku))
	}
	if originalProduct.ShortDescription != nil && *originalProduct.ShortDescription != "" {
		infoSources.WriteString(fmt.Sprintf("Descripción corta: %s\n", *originalProduct.ShortDescription))
	}
	if originalProduct.FullDescription != nil && *originalProduct.FullDescription != "" {
		infoSources.WriteString(fmt.Sprintf("Descripción completa: %s\n", *originalProduct.FullDescription))
	}
	if originalProduct.PIMAttributes != nil && *originalProduct.PIMAttributes != "" {
		infoSources.WriteString(fmt.Sprintf("Atributos PIM: %s\n", *originalProduct.PIMAttributes))
	}

	// 2. INFORMACIÓN GENERADA POR CLAUDE (fuente secundaria)
	infoSources.WriteString("\n=== INFORMACIÓN GENERADA POR IA ===\n")
	if commercialContent != nil {
		if commercialContent.Title != "" {
			infoSources.WriteString(fmt.Sprintf("Título generado: %s\n", commercialContent.Title))
		}
		if commercialContent.ShortDescription != "" {
			infoSources.WriteString(fmt.Sprintf("Descripción corta generada: %s\n", commercialContent.ShortDescription))
		}
		if len(commercialContent.Attributes) > 0 {
			infoSources.WriteString(fmt.Sprintf("Atributos generados: %s\n", strings.Join(commercialContent.Attributes, ", ")))
		}
	}

	prompt := fmt.Sprintf(`Analiza este producto y describe SOLO características VISUALES que se verían en una fotografía:

%s

Incluye únicamente aspectos VISIBLES:
- Colores (rojo, azul, negro, blanco, etc.)
- Texturas (liso, rugoso, brillante, mate, etc.)
- Patrones (rayado, puntos, floral, etc.)
- Formas (rectangular, redondo, etc.)
- Acabados (metálico, transparente, etc.)

NO incluyas:
- Tamaños o medidas
- Marcas o nombres
- Funciones o usos
- Cantidades

Responde SOLO con JSON:
{"visualTags": ["color1", "textura1", "forma1"]}`, infoSources.String())

	request := ClaudeRequest{
		Model: "claude-haiku-4-5-20251001",
		Messages: []Message{
			{
				Role:    "user",
				Content: prompt,
			},
		},
		MaxTokens: 300,
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		log.Printf("❌ Error creating visual tags request: %v", err)
		return []string{}
	}

	// Realizar petición
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewBuffer(requestBody))
	if err != nil {
		log.Printf("❌ Error creating HTTP request for visual tags: %v", err)
		return []string{}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("⚠️ Visual tags API request failed: %v", err)
		return []string{}
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("⚠️ Visual tags API returned status %d, using fallback", resp.StatusCode)
		return generateVisualTags(keywords, technicalDocs, searchResults, commercialContent)
	}

	var claudeResponse ClaudeResponse
	if err := json.NewDecoder(resp.Body).Decode(&claudeResponse); err != nil {
		log.Printf("❌ Error decoding visual tags response: %v, using fallback", err)
		return generateVisualTags(keywords, technicalDocs, searchResults, commercialContent)
	}

	if len(claudeResponse.Content) == 0 {
		log.Printf("❌ Empty visual tags response from Claude, using fallback")
		return generateVisualTags(keywords, technicalDocs, searchResults, commercialContent)
	}

	// Extraer y parsear JSON
	responseText := strings.TrimSpace(claudeResponse.Content[0].Text)

	jsonStart := strings.Index(responseText, "{")
	jsonEnd := strings.LastIndex(responseText, "}")

	if jsonStart == -1 || jsonEnd == -1 {
		log.Printf("⚠️ No valid JSON found in visual tags response, using fallback")
		return generateVisualTags(keywords, technicalDocs, searchResults, commercialContent)
	}

	jsonText := responseText[jsonStart : jsonEnd+1]

	// Limpiar el JSON para casos donde Claude incluye texto explicativo
	jsonText = cleanJSONText(jsonText)

	var visualTagsResponse struct {
		VisualTags []string `json:"visualTags"`
	}

	if err := json.Unmarshal([]byte(jsonText), &visualTagsResponse); err != nil {
		// Nuclear fix: 2 passes
		nuclear := strings.ReplaceAll(jsonText, `\\\"`, `"`)
		nuclear = strings.ReplaceAll(nuclear, `\"`, `"`)
		nuclear = fixUnescapedInchSymbols(nuclear)
		if err2 := json.Unmarshal([]byte(nuclear), &visualTagsResponse); err2 != nil {
			log.Printf("❌ Error parsing visual tags JSON: %v", err)
			log.Printf("❌ JSON problemático: %q", jsonText)
			log.Printf("⚠️ Using fallback method for visual tags")
			return generateVisualTags(keywords, technicalDocs, searchResults, commercialContent)
		}
		log.Printf("⚡ NUCLEAR FIX visual tags: repaired")
	}

	if len(visualTagsResponse.VisualTags) > 0 {
		// Validar y filtrar visual tags según las reglas especificadas
		validTags := validateVisualTags(visualTagsResponse.VisualTags)

		if len(validTags) > 0 {
			log.Printf("✅ Generated %d valid visual tags: %v", len(validTags), validTags)
			return validTags
		} else {
			log.Printf("⚠️ All generated visual tags were invalid, using fallback")
			return generateVisualTags(keywords, technicalDocs, searchResults, commercialContent)
		}
	}

	log.Printf("⚠️ No visual tags generated by Claude, using fallback method")
	// Fallback: usar método de patrones si Claude falla
	return generateVisualTags(keywords, technicalDocs, searchResults, commercialContent)
}

func generateVisualTags(keywords []string, technicalDocs *TechnicalDocumentResult, searchResults []map[string]interface{}, commercialContent *CommercialContent) []string {
	visualTags := []string{}
	tagMap := make(map[string]bool)

	// Combinar texto de fuentes más específicas y confiables
	allText := ""

	// Priorizar commercialContent que suele tener información más limpia
	if commercialContent != nil {
		allText += " " + commercialContent.LongDescription
		allText += " " + strings.Join(commercialContent.Attributes, " ")
		allText += " " + strings.Join(commercialContent.HighlightBullets, " ")
	}

	// Agregar keywords solo los más relevantes (primeros 5)
	maxKeywords := 5
	if len(keywords) > maxKeywords {
		allText += " " + strings.Join(keywords[:maxKeywords], " ")
	} else {
		allText += " " + strings.Join(keywords, " ")
	}

	// Agregar solo información técnica más relevante
	if technicalDocs != nil {
		for _, doc := range technicalDocs.Documents {
			if doc.IsAvailable && doc.ExtractedData != "" {
				// Limitar el texto de documentos técnicos para evitar ruido
				docText := doc.ExtractedData
				if len(docText) > 500 {
					docText = docText[:500]
				}
				allText += " " + docText
			}
		}
	}

	// Convertir a minúsculas para análisis
	lowerText := strings.ToLower(allText)

	// Extraer COLORES con contexto para evitar falsos positivos
	colorPatterns := map[string]*regexp.Regexp{
		"Blanco":   regexp.MustCompile(`(?i)\b(color\s+)?blanco\b|\bwhite\b`),
		"Negro":    regexp.MustCompile(`(?i)\b(color\s+)?negro\b|\bblack\b`),
		"Gris":     regexp.MustCompile(`(?i)\b(color\s+)?gris\b|\bgray\b|\bgrey\b`),
		"Azul":     regexp.MustCompile(`(?i)\b(color\s+)?azul\b|\bblue\b`),
		"Rojo":     regexp.MustCompile(`(?i)\b(color\s+)?rojo\b|\bred\b`),
		"Verde":    regexp.MustCompile(`(?i)\b(color\s+)?verde\b|\bgreen\b`),
		"Dorado":   regexp.MustCompile(`(?i)\b(color\s+)?dorado\b|\bgold\b`),
		"Plateado": regexp.MustCompile(`(?i)\b(color\s+)?plateado\b|\bsilver\b`),
		"Bronce":   regexp.MustCompile(`(?i)\b(acabado\s+)?bronce\b|\bbronze\b`),
		"Cobre":    regexp.MustCompile(`(?i)\b(acabado\s+)?cobre\b|\bcopper\b`),
	}

	// Extraer solo colores que estén en contexto apropiado
	colorCount := 0
	maxColors := 3
	for colorName, pattern := range colorPatterns {
		if colorCount >= maxColors {
			break
		}
		if pattern.MatchString(lowerText) && !tagMap[strings.ToLower(colorName)] {
			visualTags = append(visualTags, colorName)
			tagMap[strings.ToLower(colorName)] = true
			colorCount++
		}
	}

	// Extraer MATERIALES principales con contexto
	materialPatterns := map[string]*regexp.Regexp{
		"Acero":    regexp.MustCompile(`(?i)\b(acero|steel|inoxidable)\b`),
		"Aluminio": regexp.MustCompile(`(?i)\baluminio\b|\baluminum\b`),
		"Plástico": regexp.MustCompile(`(?i)\bplástico\b|\bplastic\b|\bpvc\b`),
		"Madera":   regexp.MustCompile(`(?i)\bmadera\b|\bwood\b|\bwooden\b`),
		"Vidrio":   regexp.MustCompile(`(?i)\bvidrio\b|\bglass\b`),
		"Cerámica": regexp.MustCompile(`(?i)\bcerámica\b|\bceramic\b|\bporcelana\b`),
		"Metal":    regexp.MustCompile(`(?i)\bmetal\b|\bmetálico\b`),
		"Hierro":   regexp.MustCompile(`(?i)\bhierro\b|\biron\b`),
	}

	// Limitar a máximo 2 materiales principales
	materialCount := 0
	maxMaterials := 2
	for materialName, pattern := range materialPatterns {
		if materialCount >= maxMaterials {
			break
		}
		if pattern.MatchString(lowerText) && !tagMap[strings.ToLower(materialName)] {
			visualTags = append(visualTags, materialName)
			tagMap[strings.ToLower(materialName)] = true
			materialCount++
		}
	}

	// NO agregar tags por defecto - solo devolver los encontrados en los datos reales
	return visualTags
}

func calculateConfidence(enhanced EnhancedProduct) float64 {
	confidence := 0.7 // Base confidence

	// Incrementar confianza basada en la cantidad de información encontrada
	if len(enhanced.SearchResults) > 3 {
		confidence += 0.1
	}
	if len(enhanced.Keywords) > 3 {
		confidence += 0.1
	}
	if len(enhanced.EnhancedDescription) > 200 {
		confidence += 0.1
	}

	// Incrementar confianza si se generó contenido comercial exitosamente
	if enhanced.CommercialContent != nil {
		if enhanced.CommercialContent.Title != "" {
			confidence += 0.05
		}
		if len(enhanced.CommercialContent.HighlightBullets) > 0 {
			confidence += 0.05
		}
	}

	// Máximo 95% de confianza
	if confidence > 0.95 {
		confidence = 0.95
	}

	return confidence
}

func getSEOTitle(commercialContent *CommercialContent) string {
	if commercialContent != nil && commercialContent.TituloSEO != "" {
		return commercialContent.TituloSEO
	}
	// Fallback: usar el título comercial si existe
	if commercialContent != nil && commercialContent.Title != "" {
		t := commercialContent.Title
		if len(t) > 60 {
			t = t[:57] + "..."
		}
		return t
	}
	return ""
}

func getSEODescription(commercialContent *CommercialContent) string {
	if commercialContent != nil && commercialContent.DescripcionSEO != "" {
		return commercialContent.DescripcionSEO
	}
	// Fallback: usar shortDescription si existe
	if commercialContent != nil && commercialContent.ShortDescription != "" {
		d := commercialContent.ShortDescription
		// Limpiar HTML tags para el meta description
		d = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(d, " ")
		d = strings.TrimSpace(d)
		if len(d) > 160 {
			d = d[:157] + "..."
		}
		return d
	}
	return ""
}

// getSEOTitleFallback genera un título SEO desde datos básicos del producto
// cuando CommercialContent falla completamente
func getSEOTitleFallback(name string) string {
	if name == "" {
		return ""
	}
	clean := strings.TrimSpace(name)
	if len(clean) > 60 {
		clean = clean[:57] + "..."
	}
	return clean
}

// getSEODescriptionFallback genera una descripción SEO desde datos básicos
func getSEODescriptionFallback(shortDesc string) string {
	if shortDesc == "" {
		return ""
	}
	// Limpiar HTML
	clean := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(shortDesc, " ")
	clean = strings.Join(strings.Fields(clean), " ")
	if len(clean) > 160 {
		clean = clean[:157] + "..."
	}
	return clean
}

// findLastValidJSONDelimiter busca el último } o ] válido en el JSON
func findLastValidJSONDelimiter(jsonText string) int {
	braceCount := 0
	bracketCount := 0
	inString := false
	escaped := false
	lastValidPos := -1

	for i, char := range jsonText {
		if escaped {
			escaped = false
			continue
		}

		if char == '\\' && inString {
			escaped = true
			continue
		}

		if char == '"' {
			inString = !inString
			continue
		}

		if !inString {
			switch char {
			case '{':
				braceCount++
			case '}':
				braceCount--
				if braceCount == 0 && bracketCount == 0 {
					lastValidPos = i
				}
			case '[':
				bracketCount++
			case ']':
				bracketCount--
				if braceCount == 0 && bracketCount == 0 {
					lastValidPos = i
				}
			}
		}
	}

	return lastValidPos
}

// applyMinimalJSONFixes aplica solo las correcciones JSON mínimas necesarias
func applyMinimalJSONFixes(jsonText string) string {
	// 1. Escapar saltos de línea dentro de strings JSON
	var result strings.Builder
	inString := false
	escaped := false

	for i := 0; i < len(jsonText); i++ {
		char := jsonText[i]

		if escaped {
			result.WriteByte(char)
			escaped = false
			continue
		}

		if char == '\\' && inString {
			result.WriteByte(char)
			escaped = true
			continue
		}

		if char == '"' {
			inString = !inString
			result.WriteByte(char)
			continue
		}

		// Solo escapar caracteres problemáticos dentro de strings
		if inString {
			switch char {
			case '\n':
				result.WriteString("\\n")
			case '\r':
				result.WriteString("\\r")
			case '\t':
				result.WriteString("\\t")
			default:
				result.WriteByte(char)
			}
		} else {
			result.WriteByte(char)
		}
	}

	jsonText = result.String()

	// 2. Corregir solo patrones específicos problemáticos
	// Comas después de dos puntos sin valor
	commaAfterColonPattern := regexp.MustCompile(`("[^"]*")\s*:\s*,`)
	jsonText = commaAfterColonPattern.ReplaceAllString(jsonText, `$1: null,`)

	// Comas duplicadas
	jsonText = regexp.MustCompile(`,\s*,`).ReplaceAllString(jsonText, `,`)

	// Comas antes de } o ]
	jsonText = regexp.MustCompile(`,\s*([}\]])`).ReplaceAllString(jsonText, `$1`)

	// FASE FINAL: eliminar cualquier \n literal (backslash + n como 2 chars)
	// que quede en posiciones estructurales (fuera de strings JSON).
	// Esto es la última línea de defensa contra errores de parsing.
	jsonText = fixStructuralBackslashN(jsonText)

	return jsonText
}

// fixStructuralBackslashN recorre el JSON char a char y convierte el par
// backslash+n (\n como 2 caracteres) a newline REAL cuando está fuera de strings.
// Dentro de strings los deja intactos (son escapes válidos de JSON).
// fixOverescapedJSON detecta y corrige JSON donde Claude escapó todos los delimitadores.
// Patrón: {\"key\": \"value\"} en vez de {"key": "value"}
// Detección: el JSON comienza con { seguido de espacios/newline seguido de \"
// Corrección: reemplazar \" → " globalmente. Los inch-symbols se re-escapan después
// por fixUnescapedInchSymbols (que corre en ÚLTIMA DEFENSA 2).
func fixOverescapedJSON(jsonText string) string {
	// Detectar si el JSON usa \" como delimitadores en todas partes
	overescapedPattern := regexp.MustCompile(`^\{\s*\\"`)
	if !overescapedPattern.MatchString(jsonText) {
		return jsonText // JSON normal, no tocar
	}
	// Reemplazar todos los \" (2 chars: \ + ") con "
	// Los inch symbols como 1/4" quedan como 1/4" y serán re-escapados por fixUnescapedInchSymbols
	return strings.ReplaceAll(jsonText, `\"`, `"`)
}

// fixStructuralBackslashN convierte \n literal (2 chars: \ + n) en posiciones
// estructurales del JSON (fuera de strings) a newlines reales.
// Dentro de strings, los escapes se preservan intactos.
func fixStructuralBackslashN(jsonText string) string {
	var b strings.Builder
	b.Grow(len(jsonText))
	inString := false
	i := 0
	for i < len(jsonText) {
		ch := jsonText[i]
		if inString {
			b.WriteByte(ch)
			if ch == '\\' && i+1 < len(jsonText) {
				// Consumir el siguiente carácter como parte del escape
				i++
				b.WriteByte(jsonText[i])
			} else if ch == '"' {
				inString = false
			}
		} else {
			if ch == '"' {
				inString = true
				b.WriteByte(ch)
			} else if ch == '\\' && i+1 < len(jsonText) && jsonText[i+1] == 'n' {
				// \n literal en posición estructural → reemplazar con newline real
				b.WriteByte('\n')
				i++ // saltar la 'n'
			} else {
				b.WriteByte(ch)
			}
		}
		i++
	}
	return b.String()
}

// cleanJSONText limpia el texto JSON de manera segura
func cleanJSONText(jsonText string) string {

	// FASE -3: Detectar y corregir JSON con todos los delimitadores sobre-escapados.
	// Claude a veces devuelve JSON donde TODOS los " son \"  (ej: {\"key\": \"value\"}).
	// Detección: si el JSON empieza con {\s*\"  →  todos los delimitadores están escapados.
	// Solución: reemplazar \" → " globalmente (el contenido inch-symbols se re-escapa después).
	jsonText = fixOverescapedJSON(jsonText)

	// FASE -1: Convertir comillas simples usadas como delimitadores de string a dobles.
	// Claude a veces devuelve: {"key": 'value'} en vez de {"key": "value"}
	// Patrón: valor precedido por ": ' y seguido de '  (con posible coma o })
	jsonText = regexp.MustCompile(`(:\s*)'([^']*)'(\s*[,}\]])`).ReplaceAllString(jsonText, `$1"$2"$3`)
	// Caso borde: primer o único campo: {'key': 'val'}
	jsonText = regexp.MustCompile(`([\{,]\s*)'([^']+)'(\s*:)`).ReplaceAllString(jsonText, `$1"$2"$3`)

	// FASE 0: Convertir literal \n (backslash + n) entre campos JSON a newlines reales
	// Esto maneja cuando Claude devuelve: "field": "value",\n  "nextField"
	// El patrón busca ,\n seguido de espacios y comilla (inicio de siguiente campo)
	jsonText = regexp.MustCompile(`",\\\\n\s*"`).ReplaceAllString(jsonText, "\",\n\"")
	jsonText = regexp.MustCompile(`",\\n\s*"`).ReplaceAllString(jsonText, "\",\n\"")
	jsonText = regexp.MustCompile(`\{\\\\n\s*"`).ReplaceAllString(jsonText, "{\n\"")
	jsonText = regexp.MustCompile(`\{\\n\s*"`).ReplaceAllString(jsonText, "{\n\"")
	jsonText = regexp.MustCompile(`\[\\\\n\s*"`).ReplaceAllString(jsonText, "[\n\"")
	jsonText = regexp.MustCompile(`\[\\n\s*"`).ReplaceAllString(jsonText, "[\n\"")
	jsonText = regexp.MustCompile(`"\\\\n\s*\]`).ReplaceAllString(jsonText, "\"\n]")
	jsonText = regexp.MustCompile(`"\\n\s*\]`).ReplaceAllString(jsonText, "\"\n]")
	jsonText = regexp.MustCompile(`"\\\\n\s*\}`).ReplaceAllString(jsonText, "\"\n}")
	jsonText = regexp.MustCompile(`"\\n\s*\}`).ReplaceAllString(jsonText, "\"\n}")

	// FASE 1: Arreglar dobles backslashes en estructura JSON
	// {\\n -> {\n, }\\n -> }\n, [\\n -> [\n, ]\\n -> ]\n, ,\\n -> ,\n
	jsonText = strings.ReplaceAll(jsonText, "{\\\\n", "{\n")
	jsonText = strings.ReplaceAll(jsonText, "}\\\\n", "}\n")
	jsonText = strings.ReplaceAll(jsonText, "[\\\\n", "[\n")
	jsonText = strings.ReplaceAll(jsonText, "]\\\\n", "]\n")
	jsonText = strings.ReplaceAll(jsonText, ",\\\\n", ",\n")
	jsonText = strings.ReplaceAll(jsonText, ":\\\\n", ":\n")

	// FASE 2: Arreglar \\n dentro de strings JSON usando enfoque más simple
	// Usar múltiples pasadas para manejar casos complejos
	for i := 0; i < 3; i++ {
		// \\n -> \n dentro de strings
		jsonText = regexp.MustCompile(`("[^"]*?)\\\\n([^"]*?")`).ReplaceAllString(jsonText, `$1\n$2`)
		jsonText = regexp.MustCompile(`("[^"]*?)\\\\r([^"]*?")`).ReplaceAllString(jsonText, `$1\r$2`)
		jsonText = regexp.MustCompile(`("[^"]*?)\\\\t([^"]*?")`).ReplaceAllString(jsonText, `$1\t$2`)
	}

	// FASE 3: Casos especiales al inicio y final
	if strings.HasPrefix(jsonText, "\\\\n") {
		jsonText = "\n" + jsonText[3:]
	}
	if strings.HasSuffix(jsonText, "\\\\n") {
		jsonText = jsonText[:len(jsonText)-3] + "\n"
	}

	// FASE 4: SOLO escapar newlines DENTRO de strings JSON, NO en la estructura
	// Usar un enfoque que preserve la estructura pero escape el contenido
	result := strings.Builder{}
	inString := false
	escaped := false

	for _, char := range jsonText {
		if escaped {
			result.WriteRune(char)
			escaped = false
			continue
		}

		if char == '\\' && inString {
			escaped = true
			result.WriteRune(char)
			continue
		}

		if char == '"' {
			inString = !inString
			result.WriteRune(char)
			continue
		}

		// Solo escapar caracteres de control DENTRO de strings
		if inString {
			switch char {
			case '\n':
				result.WriteString("\\n")
			case '\r':
				result.WriteString("\\r")
			case '\t':
				result.WriteString("\\t")
			default:
				result.WriteRune(char)
			}
		} else {
			result.WriteRune(char)
		}
	}

	jsonText = result.String()

	// FASE 5: Limpiar backslashes problemáticos antes de closing
	jsonText = regexp.MustCompile(`\\"\s*([,\]\}])`).ReplaceAllString(jsonText, `"$1`)

	// FASE 6: Limpiar backslashes excesivos
	jsonText = regexp.MustCompile(`\\{3,}`).ReplaceAllString(jsonText, `\\`)

	// FASE 7: Manejar secuencias Unicode problemáticas
	// Arreglar secuencias \\u que no son válidas
	jsonText = regexp.MustCompile(`\\\\u([0-9a-fA-F]{4})`).ReplaceAllString(jsonText, `\u$1`)

	// Remover secuencias \u incompletas o inválidas que puedan causar problemas de parsing
	jsonText = regexp.MustCompile(`\\u(?:[^0-9a-fA-F]|[0-9a-fA-F]{0,3}(?:[^0-9a-fA-F]|$))`).ReplaceAllString(jsonText, "")

	// FASE 8: Escapar comillas dobles no escapadas dentro de strings JSON
	// Buscar patrones como ": "texto con " comillas" y escaparlas
	jsonText = fixUnescapedQuotesInJSONValues(jsonText)

	// FASE 9: Validar que tenemos llaves balanceadas
	openBraces := strings.Count(jsonText, "{")
	closeBraces := strings.Count(jsonText, "}")
	if openBraces != closeBraces {
		// Intentar balancear agregando llaves faltantes al final
		for openBraces > closeBraces {
			jsonText += "}"
			closeBraces++
		}
	}

	// ÚLTIMA DEFENSA 1: \n literal en posiciones estructurales → newline real
	jsonText = fixStructuralBackslashN(jsonText)

	// ÚLTIMA DEFENSA 2: símbolo de pulgadas sin escapar (1/4" → 1/4\")
	jsonText = fixUnescapedInchSymbols(jsonText)

	// NOTA: fixInvalidBackslashEscapes eliminado — introducía bugs al encontrar \"key\"
	// porque trataba \" como escape-dentro-de-string y rompía el tracking de contexto

	// ÚLTIMA DEFENSA 3: comilla sin escapar en medio de string (no medidas)
	// Ej: "El "mejor" producto" → "El \"mejor\" producto"
	jsonText = fixUnescapedQuotesInStrings(jsonText)

	// ÚLTIMA DEFENSA 5: "key": "val" dentro de array → "key: val" (object syntax en array)
	// Ej: ["Medidas: 1/4": "bisagra"] → ["Medidas: 1/4: bisagra"]
	jsonText = regexp.MustCompile(`([\[,]\s*)"([^"]+)":\s*"([^"]+)"`).ReplaceAllString(jsonText, `$1"$2: $3"`)

	// ÚLTIMA DEFENSA 6: array sin coma entre elementos ["a" "b"] → ["a", "b"]
	jsonText = regexp.MustCompile(`"(\s+)"([a-zA-ZáéíóúüñÁÉÍÓÚÜÑ])`).ReplaceAllString(jsonText, `", "$2`)

	return jsonText
}

// fixInvalidBackslashEscapes corrige backslashes seguidos de caracteres que no son
// escapes JSON válidos. JSON solo acepta: \", \\, \/, \b, \f, \n, \r, \t, \uXXXX
func fixInvalidBackslashEscapes(jsonText string) string {
	var b strings.Builder
	b.Grow(len(jsonText))
	inStr := false
	i := 0
	validEscapes := map[byte]bool{'"': true, '\\': true, '/': true, 'b': true, 'f': true, 'n': true, 'r': true, 't': true, 'u': true}
	for i < len(jsonText) {
		ch := jsonText[i]
		if !inStr {
			if ch == '"' {
				inStr = true
			}
			b.WriteByte(ch)
		} else {
			if ch == '"' {
				inStr = false
				b.WriteByte(ch)
			} else if ch == '\\' && i+1 < len(jsonText) {
				next := jsonText[i+1]
				if validEscapes[next] {
					b.WriteByte(ch)
					b.WriteByte(next)
					i++
				} else {
					// Escape inválido → doblar el backslash
					b.WriteByte('\\')
					b.WriteByte('\\')
				}
			} else {
				b.WriteByte(ch)
			}
		}
		i++
	}
	return b.String()
}

// fixUnescapedQuotesInStrings detecta y escapa comillas sin escapar dentro de strings JSON
// que NO son medidas (ya manejadas por fixUnescapedInchSymbols).
// Detecta el patrón: espacio/letra + " + espacio + letra (comilla decorativa)
func fixUnescapedQuotesInStrings(jsonText string) string {
	// Patrón abre: espacio + " + letra (abre comilla decorativa: El "mejor)
	re1 := regexp.MustCompile(`(\s)"([a-zA-ZáéíóúüñÁÉÍÓÚÜÑ])`)
	jsonText = re1.ReplaceAllString(jsonText, `$1\"$2`)
	// Patrón cierra: letra + " + espacio (cierra comilla decorativa: mejor" producto)
	re2 := regexp.MustCompile(`([a-zA-ZáéíóúüñÁÉÍÓÚÜÑ])"(\s)`)
	jsonText = re2.ReplaceAllString(jsonText, `$1\"$2`)
	return jsonText
}

// fixUnescapedInchSymbols escapa el símbolo de pulgadas (") cuando aparece en medio de
// un valor de string JSON sin estar correctamente escapado.
// Ejemplo: "Medidas: 1/4"" → "Medidas: 1/4\""
func fixUnescapedInchSymbols(jsonText string) string {
	// Caso 1: dígito/fracción + " + carácter que NO es cierre de string JSON
	// Ej: 1/4" para → 1/4\" para | 3/8" satin → 3/8\" satin
	re1 := regexp.MustCompile(`(\d+(?:[./]\d+)?)"([^"\\,\]\}\n])`)
	jsonText = re1.ReplaceAllStringFunc(jsonText, func(m string) string {
		groups := re1.FindStringSubmatch(m)
		if len(groups) >= 3 {
			return groups[1] + `\"` + groups[2]
		}
		return m
	})

	// Caso 2: dígito/fracción + " + : (el error exacto que vemos: "Medidas: 1/4": )
	// Necesita tratamiento especial porque : es separador JSON válido
	re2 := regexp.MustCompile(`(\d+(?:[./]\d+)?)"(:)`)
	jsonText = re2.ReplaceAllString(jsonText, `$1\"$2`)

	return jsonText
}

// extractValidJSON intenta extraer JSON válido usando decoder progresivo
func extractValidJSON(text string) (string, bool) {
	// Encontrar el primer { y intentar parsear desde ahí
	start := strings.Index(text, "{")
	if start == -1 {
		return "", false
	}

	// Usar decoder progresivo para encontrar donde termina el JSON válido
	decoder := json.NewDecoder(strings.NewReader(text[start:]))
	var result json.RawMessage

	err := decoder.Decode(&result)
	if err != nil {
		return "", false
	}

	// Calcular el offset donde terminó el JSON válido
	offset := decoder.InputOffset()

	if offset > 0 && start+int(offset) <= len(text) {
		validJSON := text[start : start+int(offset)]
		return validJSON, true
	}

	return "", false
}

// fixUnescapedQuotesInJSONValues escapa comillas dobles no escapadas dentro de valores JSON
func fixUnescapedQuotesInJSONValues(jsonText string) string {
	// Usar una aproximación más simple: encontrar valores entre comillas y escapar comillas internas
	result := strings.Builder{}
	inString := false
	escaped := false

	for i, char := range jsonText {
		if escaped {
			result.WriteRune(char)
			escaped = false
			continue
		}

		if char == '\\' {
			escaped = true
			result.WriteRune(char)
			continue
		}

		if char == '"' {
			if !inString {
				// Iniciando una string - verificar si es una clave o valor
				inString = true
			} else {
				// Terminando una string - verificar si es realmente el final
				// Mirar hacia adelante para ver si hay : o ,
				nextNonSpace := i + 1
				for nextNonSpace < len(jsonText) && (jsonText[nextNonSpace] == ' ' || jsonText[nextNonSpace] == '\n' || jsonText[nextNonSpace] == '\t') {
					nextNonSpace++
				}

				if nextNonSpace < len(jsonText) && (jsonText[nextNonSpace] == ':' || jsonText[nextNonSpace] == ',' || jsonText[nextNonSpace] == '}' || jsonText[nextNonSpace] == ']') {
					// Es el final legítimo de una string
					inString = false
				} else {
					// Es una comilla dentro de una string - escaparla
					result.WriteString("\\")
				}
			}
		}

		result.WriteRune(char)
	}

	return result.String()
}

func generateSearchTermsWithClaude(product Product) ([]string, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not configured")
	}

	prompt := fmt.Sprintf(`You are a search optimization expert. Analyze the following product and generate search terms to find technical specifications and product information.

Product:
- Name: %s
- SKU: %s
- Short Description: %s
- Full Description: %s
- Barcode: %s
- External Codes: %s
- Manufacturer Part Number: %s
- Family/Line ID: %s

CRITICAL RULES FOR SEARCH TERMS:
1. KEEP brand names and model names EXACTLY as they appear (Hunter, Dempsey, Mabe, etc.)
2. NEVER translate abbreviations - keep them in original form (BRUSH N., SS, BLK, etc.)
3. Use Spanish for product TYPE words (abanico de techo, refrigerador, ventilador)
4. Combine: Spanish product type + English brand/model (e.g., "ventilador de techo Hunter Dempsey 52")
5. Include at least ONE term with the EXACT original product name for best results
6. NEVER guess what abbreviations mean - keep them as-is

EXAMPLES OF CORRECT SEARCH TERMS:
- Product: "ABANICO DEMPSEY 52" BRUSH N."
  CORRECT: ["Hunter Dempsey 52 ceiling fan", "abanico de techo Dempsey 52", "Hunter Dempsey BRUSH N. specs"]
  WRONG: ["abanico dempsey cepillo", "ventilador con cepillo"] (translated BRUSH incorrectly!)

- Product: "REFRIGERADOR MABE SS 20 CF"
  CORRECT: ["refrigerador Mabe SS 20 pies", "Mabe SS 20 CF specifications"]
  WRONG: ["refrigerador acero inoxidable"] (assumed SS = Stainless Steel)

Generate 3-5 search terms mixing Spanish product types with original English brand/model names:

Respond ONLY with JSON:
{
  "searchTerms": ["term1", "term2", "term3"]
}`,
		strings.ReplaceAll(product.Name, `"`, `''`), // sanitize " to avoid JSON escaping issues
		product.Sku,
		getStringValue(product.ShortDescription),
		getStringValue(product.FullDescription),
		getStringValue(product.Barcodes),
		getStringValue(product.ExternalCodes),
		getStringValue(product.ManufacturerPartNumber),
		getStringValue(product.FamilyId))

	request := ClaudeRequest{
		Model: "claude-haiku-4-5-20251001",
		Messages: []Message{
			{
				Role:    "user",
				Content: prompt,
			},
		},
		MaxTokens: 500,
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}

	// Realizar petición con reintentos
	var resp *http.Response
	var lastErr error

	for attempt := 0; attempt < 3; attempt++ {
		client := &http.Client{Timeout: 30 * time.Second}
		req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewBuffer(requestBody))
		if err != nil {
			return nil, fmt.Errorf("error creating request: %v", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")

		resp, err = client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("network error on attempt %d: %v", attempt+1, err)
			if attempt < 2 {
				time.Sleep(time.Duration(attempt+1) * time.Second)
			}
			continue
		}

		if resp.StatusCode == http.StatusOK {
			break
		}

		body, _ := ioutil.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("rate limit exceeded on attempt %d", attempt+1)
			if attempt < 2 {
				time.Sleep(time.Duration(attempt+2) * time.Second)
			}
			continue
		}

		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("server error %d on attempt %d: %s", resp.StatusCode, attempt+1, string(body))
			if attempt < 2 {
				time.Sleep(time.Duration(attempt+1) * time.Second)
			}
			continue
		}

		return nil, fmt.Errorf("claude API client error %d: %s", resp.StatusCode, string(body))
	}

	if resp == nil || resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed after 3 attempts: %v", lastErr)
	}

	defer resp.Body.Close()

	var claudeResp ClaudeResponse
	if err := json.NewDecoder(resp.Body).Decode(&claudeResp); err != nil {
		return nil, err
	}

	if len(claudeResp.Content) == 0 {
		return nil, fmt.Errorf("respuesta vacía de Claude")
	}

	var termsData struct {
		SearchTerms []string `json:"searchTerms"`
	}

	// Limpiar la respuesta JSON antes de procesarla
	claudeText := claudeResp.Content[0].Text

	// Intentar extraer solo el JSON válido si hay texto adicional
	jsonStart := strings.Index(claudeText, "{")
	jsonEnd := strings.LastIndex(claudeText, "}")

	if jsonStart == -1 || jsonEnd == -1 || jsonStart >= jsonEnd {
		return nil, fmt.Errorf("no se encontró JSON válido en la respuesta de Claude")
	}

	jsonText := claudeText[jsonStart : jsonEnd+1]

	// Validación adicional: verificar que no hay texto después del JSON
	if jsonEnd+1 < len(claudeText) {
		remainingText := strings.TrimSpace(claudeText[jsonEnd+1:])
		if len(remainingText) > 0 {
		}
	}

	// Limpiar el JSON para manejar comillas problemáticas
	jsonText = cleanJSONText(jsonText)

	// NUCLEAR FALLBACK: 2 passes (\\\" → " primero, luego \" → ")
	var testParse interface{}
	if err := json.Unmarshal([]byte(jsonText), &testParse); err != nil {
		nuclear := strings.ReplaceAll(jsonText, `\\\"`, `"`)
		nuclear = strings.ReplaceAll(nuclear, `\"`, `"`)
		nuclear = fixUnescapedInchSymbols(nuclear)
		if err2 := json.Unmarshal([]byte(nuclear), &testParse); err2 == nil {
			jsonText = nuclear
			log.Printf("⚡ NUCLEAR FIX search terms: over-escaped JSON repaired")
		} else {
			fallbackTerms := extractSearchTermsFallback(claudeText)
			if len(fallbackTerms) > 0 {
				return fallbackTerms, nil
			}
			return nil, fmt.Errorf("error procesando respuesta de Claude: %v", err)
		}
	}

	if err := json.Unmarshal([]byte(jsonText), &termsData); err != nil {
		return nil, fmt.Errorf("error procesando estructura de respuesta de Claude: %v", err)
	}

	return termsData.SearchTerms, nil
}

// extractSearchTermsFallback extrae términos de búsqueda de la respuesta de Claude usando métodos alternativos
func extractSearchTermsFallback(claudeText string) []string {
	var terms []string

	// Método 1: Buscar patrones de términos en español entre comillas
	quotedTermsPattern := regexp.MustCompile(`"([^"]{5,50})"`)
	matches := quotedTermsPattern.FindAllStringSubmatch(claudeText, -1)

	jsonKeyPattern := regexp.MustCompile(`^[a-z][a-zA-Z]+$`) // camelCase = JSON key name
	for _, match := range matches {
		if len(match) > 1 {
			term := strings.TrimSpace(match[1])
			// Filtrar términos válidos: contienen letras, espacios, números
			if !regexp.MustCompile(`^[a-zA-ZáéíóúñÁÉÍÓÚÑ\s\d\-\_]{5,50}$`).MatchString(term) {
				continue
			}
			// Excluir palabras sueltas en camelCase (son nombres de claves JSON, no términos)
			if jsonKeyPattern.MatchString(term) {
				continue
			}
			// Requiere al menos 2 palabras (términos de búsqueda reales tienen espacios)
			if !strings.Contains(term, " ") {
				continue
			}
			terms = append(terms, term)
			if len(terms) >= 5 {
				break
			}
		}
	}

	// Método 2: Si no encontramos suficientes términos, usar palabras clave del texto
	if len(terms) < 3 {
		// Buscar palabras técnicas comunes en español
		technicalWordsPattern := regexp.MustCompile(`\b(refrigerador|nevera|estufa|lavadora|secadora|horno|microondas|aire\s+acondicionado|ventilador|bomba|motor|compresor|válvula|filtro|sensor|control|panel|inverter|inoxidable|capacidad|litros|watts|voltios)\b`)
		technicalMatches := technicalWordsPattern.FindAllString(strings.ToLower(claudeText), -1)

		for _, word := range technicalMatches {
			if len(terms) < 5 && !contains(terms, word) {
				terms = append(terms, word)
			}
		}
	}

	return terms
}

func generateCommercialContentWithClaude(product Product, technicalDocs *TechnicalDocumentResult, searchResults []map[string]interface{}) (*CommercialContent, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not configured")
	}

	// Obtener contexto de taxonomía específica para este producto (Matriz de Taxonomía)
	taxonomyCtx := GetTaxonomyContextForProduct(&product)
	if taxonomyCtx != nil {
		fmt.Printf("✅ Taxonomy matrix match for product (dept=%s, cat=%s, sub=%s) → tone=%s format=%s attrs=%d\n",
			product.DepartmentName, product.CategoryName, product.SubCategoryName,
			taxonomyCtx.Tone, taxonomyCtx.Format, len(taxonomyCtx.KeyAttributes))
	}

	// Preparar información de documentos técnicos y búsqueda para el prompt comercial
	additionalInfo := ""
	// Instrucciones libres adicionales del campo Prompt (si existen)
	if taxonomyCtx != nil && taxonomyCtx.Prompt != "" {
		additionalInfo += fmt.Sprintf("\n\n=== INSTRUCCIONES ADICIONALES POR TAXONOMÍA ===\n%s\n=== FIN INSTRUCCIONES TAXONOMÍA ===\n", taxonomyCtx.Prompt)
	}

	// Incluir PIMAttributes si están disponibles
	if product.PIMAttributes != nil && *product.PIMAttributes != "" {
		additionalInfo += "\n\n=== ATRIBUTOS PIM (INFORMACIÓN OFICIAL DEL PRODUCTO) ===\n"
		additionalInfo += *product.PIMAttributes
		additionalInfo += "\n=== FIN ATRIBUTOS PIM ===\n"
	}

	// PRIORIDAD 1: Información de documentos técnicos del proveedor
	if technicalDocs != nil && technicalDocs.AvailableCount > 0 {
		additionalInfo += "\n\n=== INFORMACIÓN TÉCNICA OFICIAL DEL PROVEEDOR (USAR PARA CONTENIDO COMERCIAL) ===\n"
		for _, doc := range technicalDocs.Documents {
			if doc.IsAvailable && doc.ExtractedData != "" {
				additionalInfo += fmt.Sprintf("\n--- %s (%s) ---\n", doc.ResourceName, doc.MediaType)
				additionalInfo += doc.ExtractedData + "\n"
			}
		}
		additionalInfo += "\n=== FIN INFORMACIÓN TÉCNICA OFICIAL ===\n"
	}

	// PRIORIDAD 2: Información complementaria de internet — truncada para reducir tokens
	if len(searchResults) > 0 {
		additionalInfo += "\n\nInformación adicional de internet (para enriquecer contenido comercial):\n"
		for i, result := range searchResults {
			if i >= 5 { // Reducido 10→5 resultados max
				break
			}
			if title, ok := result["title"].(string); ok {
				if description, ok := result["description"].(string); ok {
					// Truncar a 200 chars para reducir tokens
					if len(description) > 200 {
						description = description[:200] + "..."
					}
					additionalInfo += fmt.Sprintf("- %s: %s\n", title, description)
				}
			}
		}
	}

	// ─── PROMPT CACHING STEP 5: system prompt dinámico por taxonomía + user dinámico ───
	// ─── STEP 5: Formato TEXTO PLANO con separadores — evita todos los problemas de JSON ───
	systemPromptStep5 := buildSystemPromptForTaxonomy(taxonomyCtx)

	// Sanitizar el nombre del producto para evitar confusión con comillas
	safeProductName5 := strings.ReplaceAll(product.Name, `"`, `''`)

	userMessageStep5 := fmt.Sprintf("Product: %s | SKU: %s | Short: %s | Full: %s | MPN: %s\n%s",
		safeProductName5, product.Sku,
		getStringValue(product.ShortDescription), getStringValue(product.FullDescription),
		getStringValue(product.ManufacturerPartNumber),
		additionalInfo)

	claudeResp, err := callClaudeWithCache(systemPromptStep5, userMessageStep5, 1400)
	if err != nil {
		return nil, fmt.Errorf("error en Claude (step 5 commercial): %v", err)
	}
	if len(claudeResp.Content) == 0 {
		return nil, fmt.Errorf("respuesta vacía de Claude")
	}

	// Parsear formato texto plano — sin JSON, sin problemas de escaping
	claudeText5 := strings.TrimSpace(claudeResp.Content[0].Text)
	claudeText5 = strings.TrimPrefix(claudeText5, "\ufeff")

	var commercialData CommercialContent

	for _, line := range strings.Split(claudeText5, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "SHORT:") {
			commercialData.ShortDescription = strings.TrimSpace(strings.TrimPrefix(line, "SHORT:"))
		} else if strings.HasPrefix(line, "LONG:") {
			commercialData.LongDescription = strings.TrimSpace(strings.TrimPrefix(line, "LONG:"))
		} else if strings.HasPrefix(line, "TITLE:") {
			commercialData.Title = strings.TrimSpace(strings.TrimPrefix(line, "TITLE:"))
		} else if strings.HasPrefix(line, "BULLETS:") {
			raw := strings.TrimSpace(strings.TrimPrefix(line, "BULLETS:"))
			for _, b := range strings.Split(raw, "|") {
				b = strings.TrimSpace(b)
				if b != "" {
					commercialData.HighlightBullets = append(commercialData.HighlightBullets, b)
				}
			}
		} else if strings.HasPrefix(line, "BENEFITS:") {
			raw := strings.TrimSpace(strings.TrimPrefix(line, "BENEFITS:"))
			for _, b := range strings.Split(raw, "|") {
				b = strings.TrimSpace(b)
				if b != "" {
					commercialData.DevelopedBullets = append(commercialData.DevelopedBullets, b)
				}
			}
		} else if strings.HasPrefix(line, "ATTRS:") {
			raw := strings.TrimSpace(strings.TrimPrefix(line, "ATTRS:"))
			for _, a := range strings.Split(raw, "|") {
				a = strings.TrimSpace(a)
				if a != "" {
					commercialData.Attributes = append(commercialData.Attributes, a)
				}
			}
		} else if strings.HasPrefix(line, "SEO_TITLE:") {
			commercialData.TituloSEO = strings.TrimSpace(strings.TrimPrefix(line, "SEO_TITLE:"))
		} else if strings.HasPrefix(line, "SEO_DESC:") {
			commercialData.DescripcionSEO = strings.TrimSpace(strings.TrimPrefix(line, "SEO_DESC:"))
		}
	}

	// Fallback si el formato no se siguió — usar texto completo como shortDescription
	if commercialData.ShortDescription == "" && commercialData.LongDescription == "" {
		log.Printf("⚠️ Step5: plain text format not found for %s, using full response", product.Name)
		commercialData.ShortDescription = claudeText5
	}

	log.Printf("✅ Step5 commercial content generated for: %s (bullets: %d)", product.Name, len(commercialData.HighlightBullets))

	log.Printf("✅ Successfully parsed commercial JSON for: %s", product.Name)
	log.Printf("✅ Generated title: %s", commercialData.Title)

	// Limpiar contenido comercial problemático
	cleanCommercialContent(&commercialData)

	return &commercialData, nil
}

// cleanCommercialContent limpia contenido comercial problemático
func cleanCommercialContent(content *CommercialContent) {
	// Limpiar shortDescription con filtrado específico de información técnica irrelevante
	content.ShortDescription = cleanShortDescription(content.ShortDescription)

	// Limpiar longDescription
	content.LongDescription = cleanContentField(content.LongDescription)

	// Limpiar title
	content.Title = cleanContentField(content.Title)

	// Limpiar tituloSEO
	content.TituloSEO = cleanContentField(content.TituloSEO)

	// Limpiar descripcionSEO
	content.DescripcionSEO = cleanContentField(content.DescripcionSEO)

	// Limpiar arrays
	content.HighlightBullets = cleanStringArray(content.HighlightBullets)
	content.DevelopedBullets = cleanStringArray(content.DevelopedBullets)
	content.Attributes = cleanStringArray(content.Attributes)
}

// cleanContentField limpia un campo de texto individual
func cleanContentField(field string) string {
	if field == "" {
		return ""
	}

	// Remover caracteres problemáticos comunes
	field = strings.TrimSpace(field)

	// Remover comas al inicio
	field = strings.TrimPrefix(field, ",")
	field = strings.TrimSpace(field)

	// Convertir secuencias de escape a texto normal (NO eliminar)
	field = strings.ReplaceAll(field, "\\n", "\n")
	field = strings.ReplaceAll(field, "\\r", "\r")
	field = strings.ReplaceAll(field, "\\t", "\t")

	// Luego normalizar espacios en blanco
	field = strings.ReplaceAll(field, "\n", " ")
	field = strings.ReplaceAll(field, "\r", " ")
	field = strings.ReplaceAll(field, "\t", " ")

	// Remover espacios múltiples
	field = regexp.MustCompile(`\s+`).ReplaceAllString(field, " ")
	field = strings.TrimSpace(field)

	// Si queda solo puntuación o está muy corto, retornar vacío
	if len(field) < 3 || regexp.MustCompile(`^[,.\s\-_]*$`).MatchString(field) {
		return ""
	}

	return field
}

// cleanShortDescription limpia específicamente la descripción corta eliminando información técnica irrelevante
func cleanShortDescription(description string) string {
	if description == "" {
		return ""
	}

	cleaned := description

	// PASO 1: Remover HTML tags completamente
	htmlTagPattern := regexp.MustCompile(`<[^>]*>`)
	cleaned = htmlTagPattern.ReplaceAllString(cleaned, " ")

	// Aplicar limpieza básica
	cleaned = cleanContentField(cleaned)
	if cleaned == "" {
		return ""
	}

	// Patrones de información técnica a eliminar
	technicalPatterns := []string{
		// SKU patterns
		`(?i)SKU\s*:?\s*[A-Z0-9\-]+`,
		`(?i)Código\s*:?\s*[A-Z0-9\-]+`,
		`(?i)Code\s*:?\s*[A-Z0-9\-]+`,

		// Manufacturer part numbers
		`(?i)Número\s+de\s+manufactura\s*:?\s*[A-Z0-9\-]+`,
		`(?i)Part\s+Number\s*:?\s*[A-Z0-9\-]+`,
		`(?i)P/N\s*:?\s*[A-Z0-9\-]+`,
		`(?i)MPN\s*:?\s*[A-Z0-9\-]+`,

		// Barcode patterns
		`(?i)UPC\s*:?\s*\d+`,
		`(?i)EAN\s*:?\s*\d+`,
		`(?i)GTIN\s*:?\s*\d+`,
		`(?i)Código\s+de\s+barras\s*:?\s*\d+`,
		`(?i)Barcode\s*:?\s*\d+`,

		// Internal codes (UUID format)
		`[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}`,

		// Generic placeholders - CRITICAL: these provide no value to customers
		`(?i)Tipo\s*:?\s*Producto`,
		`(?i)Modelo\s*:?\s*tradicional`,
		`(?i)Modelo\s*:?\s*estándar`,
		`(?i)Type\s*:?\s*Product`,
		`(?i)Model\s*:?\s*standard`,
		`(?i)Color\s*:?\s*es\s+variados?`,
		`(?i)Color\s*:?\s*variados?`,
		`(?i)Colores?\s*:?\s*variados?`,
		`(?i)Color\s*:?\s*varios`,
		`(?i)Tamaño\s*:?\s*estándar`,
		`(?i)Material\s*:?\s*estándar`,
		`(?i)Peso\s*:?\s*estándar`,
		`(?i)Dimensiones\s*:?\s*estándar`,
	}

	// Aplicar cada patrón
	for _, pattern := range technicalPatterns {
		re := regexp.MustCompile(pattern)
		cleaned = re.ReplaceAllString(cleaned, "")
	}

	// Limpiar espacios y puntuación sobrante después del filtrado
	cleaned = regexp.MustCompile(`\s+`).ReplaceAllString(cleaned, " ")
	cleaned = strings.TrimSpace(cleaned)

	// Limpiar comas, puntos o guiones al inicio/final
	cleaned = strings.Trim(cleaned, ",.-/ ")
	cleaned = strings.TrimSpace(cleaned)

	// Si queda muy corto después de filtrar, retornar vacío
	if len(cleaned) < 10 {
		return ""
	}

	return cleaned
}

// cleanStringArray limpia un array de strings
func cleanStringArray(arr []string) []string {
	if len(arr) == 0 {
		return arr
	}

	var cleaned []string
	for _, item := range arr {
		cleanItem := cleanContentField(item)
		if cleanItem != "" {
			cleaned = append(cleaned, cleanItem)
		}
	}

	return cleaned
}

// cleanEnhancementData limpia datos de enhancement problemáticos
// extractKeywordsFromText extrae palabras clave básicas de un texto (fallback)
func extractKeywordsFromText(text string) []string {
	words := strings.Fields(strings.ToLower(text))
	seen := make(map[string]bool)
	var kws []string
	skip := map[string]bool{"de": true, "el": true, "la": true, "los": true, "las": true, "un": true, "una": true, "y": true, "en": true, "con": true, "para": true, "por": true}
	for _, w := range words {
		w = strings.Trim(w, `"'.,;:()`)
		if len(w) > 3 && !skip[w] && !seen[w] {
			kws = append(kws, w)
			seen[w] = true
		}
		if len(kws) >= 5 {
			break
		}
	}
	return kws
}

func cleanEnhancementData(data *struct {
	EnhancedDescription string   `json:"enhancedDescription"`
	Keywords            []string `json:"keywords"`
}) {
	// Limpiar campo de texto
	data.EnhancedDescription = cleanContentField(data.EnhancedDescription)

	// Limpiar array de keywords
	data.Keywords = cleanStringArray(data.Keywords)
}

func enhanceWithClaudeAndSearch(product Product) (*EnhancedProduct, error) {
	// PASO INICIAL: Limpiar datos corruptos del producto
	product.Name = cleanProductName(product.Name)
	if product.ShortDescription != nil {
		cleaned := cleanProductName(*product.ShortDescription)
		product.ShortDescription = &cleaned
	}
	if product.FullDescription != nil {
		cleaned := cleanProductName(*product.FullDescription)
		product.FullDescription = &cleaned
	}

	processSteps := []ProcessStep{}
	parallelStart := time.Now()

	// PASOS 1 Y 2 EN PARALELO: Generar términos + buscar documentos técnicos al mismo tiempo
	type step1Result struct {
		terms []string
		err   error
	}
	type step2Result struct {
		docs *TechnicalDocumentResult
		err  error
	}
	step1Ch := make(chan step1Result, 1)
	step2Ch := make(chan step2Result, 1)

	go func() {
		terms, err := generateSearchTermsWithClaude(product)
		step1Ch <- step1Result{terms, err}
	}()
	go func() {
		docs, err := enhanceProductWithTechnicalDocs(&product)
		step2Ch <- step2Result{docs, err}
	}()

	s1 := <-step1Ch
	if s1.err != nil {
		return nil, fmt.Errorf("error generating search terms: %v", s1.err)
	}
	searchTerms := s1.terms

	s2 := <-step2Ch
	var technicalDocs *TechnicalDocumentResult
	if s2.err == nil {
		technicalDocs = s2.docs
	}

	log.Printf("⚡ Steps 1+2 parallel in %s — %d search terms generated", time.Since(parallelStart), len(searchTerms))
	for i, term := range searchTerms {
		log.Printf("   [%d] %s", i+1, term)
	}

	processSteps = append(processSteps,
		ProcessStep{Step: 1, Description: "Términos de búsqueda generados (paralelo)", Timestamp: parallelStart},
		ProcessStep{Step: 2, Description: "Documentos técnicos buscados (paralelo)", Timestamp: parallelStart},
	)
	processSteps[0].Duration = time.Since(parallelStart).String()
	processSteps[1].Duration = time.Since(parallelStart).String()

	// PASO 3: Búsquedas web EN PARALELO (máx 3 concurrent para no saturar rate limit de Brave)
	step3Start := time.Now()
	processSteps = append(processSteps, ProcessStep{
		Step:        3,
		Description: "Búsquedas web paralelas con Brave Search",
		Timestamp:   step3Start,
	})

	log.Printf("🔍 WEB SEARCH PARALLEL: %d términos para '%s'", len(searchTerms), product.Name)

	type webResult struct {
		results []map[string]interface{}
		term    string
	}
	webCh := make(chan webResult, len(searchTerms))
	webSem := make(chan struct{}, 5) // Aumentado 3→5 workers (Brave count bajó de 5 a 3)
	var webWg sync.WaitGroup

	for _, term := range searchTerms {
		webWg.Add(1)
		webSem <- struct{}{}
		go func(t string) {
			defer webWg.Done()
			defer func() { <-webSem }()
			results, err := searchWithBrave(t)
			if err != nil {
				log.Printf("⚠️ WEB SEARCH ERROR for '%s': %v", t, err)
				webCh <- webResult{nil, t}
				return
			}
			log.Printf("✅ WEB SEARCH '%s': %d resultados", t, len(results))
			webCh <- webResult{results, t}
		}(term)
	}

	webWg.Wait()
	close(webCh)

	var allSearchResults []map[string]interface{}
	for wr := range webCh {
		allSearchResults = append(allSearchResults, wr.results...)
	}
	log.Printf("⚡ Step 3 parallel web search done in %s — %d resultados totales", time.Since(step3Start), len(allSearchResults))
	processSteps[2].Duration = time.Since(step3Start).String()

	// ═════════════════════════════════════════════════════════════════════════
	// FAN-OUT REAL: Step 5 se lanza AQUÍ (antes de que step 4 empiece) para que
	// corran verdaderamente en paralelo. Mientras step 4 bloquea ~4s haciendo su
	// llamada a Claude, step 5 YA lleva ese tiempo corriendo también.
	// Resultado: max(step4, step5) en vez de step4 + step5.
	// ═════════════════════════════════════════════════════════════════════════
	parallelStart45 := time.Now()

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not configured")
	}
	if product.Name == "" && (product.ShortDescription == nil || *product.ShortDescription == "") {
		return nil, fmt.Errorf("product must have at least name or description")
	}

	processSteps = append(processSteps,
		ProcessStep{Step: 4, Description: "Descripción + keywords (paralelo fan-out)", Timestamp: parallelStart45},
		ProcessStep{Step: 5, Description: "Contenido comercial (paralelo fan-out)", Timestamp: parallelStart45},
	)

	// ⚡ STEP 5 GOROUTINE — lanzar ANTES de que step 4 empiece
	type step5Res struct {
		content *CommercialContent
		err     error
	}
	step5Ch := make(chan step5Res, 1)
	go func() {
		c, e := generateCommercialContentWithClaude(product, technicalDocs, allSearchResults)
		step5Ch <- step5Res{c, e}
	}()
	log.Printf("⚡ Step 5 (commercial content) lanzado en paralelo con step 4")

	// Preparar información de documentos técnicos y búsqueda para Claude
	searchInfo := ""

	// PRIORIDAD 0: Información de los atributos PIM
	if product.PIMAttributes != nil && *product.PIMAttributes != "" {
		searchInfo += "\n\n=== ATRIBUTOS PIM (MÁXIMA PRIORIDAD) ===\n"
		searchInfo += *product.PIMAttributes
		searchInfo += "\n=== FIN ATRIBUTOS PIM ===\n"
	}

	// PRIORIDAD 1: Información de documentos técnicos del proveedor
	if technicalDocs != nil && technicalDocs.AvailableCount > 0 {
		searchInfo += "\n\n=== FICHA TÉCNICA OFICIAL ===\n"
		for _, doc := range technicalDocs.Documents {
			if doc.IsAvailable && doc.ExtractedData != "" {
				searchInfo += fmt.Sprintf("\n--- %s (%s) ---\n", doc.ResourceName, doc.MediaType)
				searchInfo += doc.ExtractedData + "\n"
			}
		}
		searchInfo += "\n=== FIN FICHA TÉCNICA ===\n"
	}

	// PRIORIDAD 2: Información de internet — truncada
	if len(allSearchResults) > 0 {
		productBrand := extractProductBrand(product)
		searchInfo += "\n\nInformación de internet:\n"
		validResultsCount := 0
		for _, result := range allSearchResults {
			if validResultsCount >= 5 {
				break
			}
			if title, ok := result["title"].(string); ok {
				if description, ok := result["description"].(string); ok {
					if productBrand != "" && isContentFromCompetitor(title+" "+description, productBrand) {
						continue
					}
					if len(description) > 200 {
						description = description[:200] + "..."
					}
					searchInfo += fmt.Sprintf("- %s: %s\n", title, description)
					validResultsCount++
				}
			}
		}
	}

	// ─── STEP 4: Formato TEXTO PLANO para evitar problemas de JSON con caracteres especiales ───
	// El producto puede tener " en el nombre (ej: 1-3/16"30MM) y Claude escapa todo con \"
	// Con formato de texto plano no hay problema de JSON parsing.
	systemPromptStep4 := `You are a professional product content specialist.
CRITICAL: ALL OUTPUT IN SPANISH. Instructions in English for accuracy.
PRIORITIZATION: TECHNICAL SHEET > PIM ATTRIBUTES > WEB > Original info.
RULES: Preserve brand. No contradictions. No invented specs. Plain text, 300-500 words.

Respond in EXACTLY this format (plain text, no JSON, no quotes, no markdown):
DESCRIPTION: <enhanced product description in Spanish here>
KEYWORDS: <keyword1>, <keyword2>, <keyword3>, <keyword4>, <keyword5>

Do NOT use any other format. Start with "DESCRIPTION:" on the first line.`

	// Sanitizar el nombre del producto para evitar que Claude se confunda con las comillas
	safeProductName := strings.ReplaceAll(product.Name, `"`, `''`)

	userMessageStep4 := fmt.Sprintf("Product: %s | SKU: %s | Short: %s | Full: %s | MPN: %s\n%s",
		safeProductName, product.Sku,
		getStringValue(product.ShortDescription), getStringValue(product.FullDescription),
		getStringValue(product.ManufacturerPartNumber),
		searchInfo)

	// Usar callClaudeWithCache para cachear el system prompt
	claudeResp, err := callClaudeWithCache(systemPromptStep4, userMessageStep4, 1500)
	if err != nil {
		return nil, fmt.Errorf("error en Claude (step 4): %v", err)
	}
	if len(claudeResp.Content) == 0 {
		return nil, fmt.Errorf("respuesta vacía de Claude")
	}

	var enhancementData struct {
		EnhancedDescription string   `json:"enhancedDescription"`
		Keywords            []string `json:"keywords"`
	}

	// Parsear formato texto plano DESCRIPTION: / KEYWORDS: — sin JSON, sin problemas de escaping
	claudeText := strings.TrimSpace(claudeResp.Content[0].Text)
	claudeText = strings.TrimPrefix(claudeText, "\ufeff")

	for _, line := range strings.Split(claudeText, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "DESCRIPTION:") {
			enhancementData.EnhancedDescription = strings.TrimSpace(strings.TrimPrefix(line, "DESCRIPTION:"))
		} else if strings.HasPrefix(line, "KEYWORDS:") {
			kwStr := strings.TrimSpace(strings.TrimPrefix(line, "KEYWORDS:"))
			for _, kw := range strings.Split(kwStr, ",") {
				kw = strings.TrimSpace(kw)
				if kw != "" {
					enhancementData.Keywords = append(enhancementData.Keywords, kw)
				}
			}
		}
	}

	// Fallback: si no encontró el formato esperado, usar el texto completo como descripción
	if enhancementData.EnhancedDescription == "" {
		log.Printf("⚠️ Step4: plain text format not found, using full response as description")
		enhancementData.EnhancedDescription = claudeText
	}
	if len(enhancementData.Keywords) == 0 {
		// Extraer palabras clave del nombre del producto como fallback
		enhancementData.Keywords = extractKeywordsFromText(product.Name)
	}

	log.Printf("⚡ Step 4 (desc+keywords) done in %s — %d keywords", time.Since(parallelStart45), len(enhancementData.Keywords))

	// Limpiar datos de enhancement problemáticos
	cleanEnhancementData(&enhancementData)
	processSteps[3].Duration = time.Since(parallelStart45).String()
	log.Printf("⚡ Step 4 (desc+keywords) done in %s", time.Since(parallelStart45))

	// CPU work (instantáneo) mientras step5 corre en background (prob. casi terminado)
	enhancedDescription := enhanceTextWithFormatting(enhancementData.EnhancedDescription)

	enhancedName := product.Name
	originalBrand := extractProductBrand(product)
	if originalBrand != "" {
		if !strings.Contains(strings.ToLower(product.Name), strings.ToLower(originalBrand)) {
			enhancedName = incorporateBrandInName(product.Name, originalBrand)
		}
	}

	tempEnhancedData := map[string]interface{}{
		"name":             enhancedName,
		"shortDescription": "",
		"fullDescription":  enhancedDescription,
	}
	inconsistencies := validateProductConsistency(product, tempEnhancedData)
	if len(inconsistencies) > 0 {
		log.Printf("⚠️ INCONSISTENCIAS DETECTADAS en producto %s:", product.Sku)
		for _, inconsistency := range inconsistencies {
			log.Printf("   - %s", inconsistency)
		}
	}

	// Fan-in: recoger resultado del step 5
	s5 := <-step5Ch
	var commercialContent *CommercialContent
	if s5.err != nil {
		log.Printf("❌ Commercial content generation failed: %v", s5.err)
	} else {
		commercialContent = s5.content
		if commercialContent != nil {
			log.Printf("✅ Successfully generated commercial content for: %s", product.Name)
		}
	}
	processSteps[4].Duration = time.Since(parallelStart45).String()
	log.Printf("⚡ Steps 4+5 fan-out total: %s (parallelStart45)", time.Since(parallelStart45))

	enhancedProduct := &EnhancedProduct{
		Product: product,
		OriginalData: ProductSummary{
			Name:             enhancedName,
			Sku:              product.Sku,
			ShortDescription: getStringValue(product.ShortDescription),
			FullDescription:  getStringValue(product.FullDescription),
		},
		SearchTerms:         searchTerms,
		SearchResults:       allSearchResults,
		EnhancedDescription: enhancedDescription,
		Keywords:            enhancementData.Keywords,
		EnhancedAt:          time.Now(),
		ProcessSteps:        processSteps,
		TechnicalDocs:       technicalDocs,
		CommercialContent:   commercialContent,
	}

	return enhancedProduct, nil
}

func searchWithBrave(query string) ([]map[string]interface{}, error) {
	apiKey := os.Getenv("BRAVE_SEARCH_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("BRAVE_SEARCH_API_KEY not configured")
	}

	// Sin cache — siempre busca fresco para que QA vea resultados actualizados
	// y los cambios de términos de búsqueda tengan efecto inmediato.

	// Reintentos con backoff exponencial
	var resp *http.Response
	var lastErr error

	for attempt := 0; attempt < 3; attempt++ {
		// Timeout de 3.5s por búsqueda — búsquedas lentas se abortan en vez de
		// bloquear el pipeline hasta 6-7s. Las 3 búsquedas corren en paralelo,
		// así que el peor caso de step 3 pasa de ~6.5s a máx ~3.5s.
		searchCtx, searchCancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
		defer searchCancel()

		client := &http.Client{Timeout: 4 * time.Second}

		req, err := http.NewRequestWithContext(searchCtx, "GET", "https://api.search.brave.com/res/v1/web/search", nil)
		if err != nil {
			searchCancel()
			return nil, fmt.Errorf("error creating request: %v", err)
		}

		q := req.URL.Query()
		q.Add("q", query)
		q.Add("count", "3") // Reducido 5→3 para bajar latencia y contexto enviado a Claude
		q.Add("search_lang", "es")
		q.Add("country", "ES")
		req.URL.RawQuery = q.Encode()

		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Subscription-Token", apiKey)

		resp, err = client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("network error on attempt %d: %v", attempt+1, err)
			if attempt < 2 {
				time.Sleep(time.Duration(attempt+1) * time.Second)
			}
			continue
		}

		if resp.StatusCode == http.StatusOK {
			break
		}

		// Handle specific error codes
		body, _ := ioutil.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("rate limit exceeded on attempt %d", attempt+1)
			if attempt < 2 {
				time.Sleep(time.Duration(attempt+2) * time.Second)
			}
			continue
		}

		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("server error %d on attempt %d: %s", resp.StatusCode, attempt+1, string(body))
			if attempt < 2 {
				time.Sleep(time.Duration(attempt+1) * time.Second)
			}
			continue
		}

		// Client errors (4xx) don't retry
		return nil, fmt.Errorf("brave search API client error %d: %s", resp.StatusCode, string(body))
	}

	if resp == nil || resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed after 3 attempts: %v", lastErr)
	}

	defer resp.Body.Close()

	var braveResp BraveSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&braveResp); err != nil {
		return nil, fmt.Errorf("error parsing response: %v", err)
	}

	var results []map[string]interface{}
	for _, result := range braveResp.Web.Results {
		results = append(results, map[string]interface{}{
			"title":       result.Title,
			"url":         result.URL,
			"description": result.Description,
		})
	}

	return results, nil
}

func healthCheck(c *gin.Context) {
	// Verificar conexión a la base de datos
	var dbErr error
	dbStatus := "connected"
	if db == nil {
		dbErr = fmt.Errorf("database not initialized")
		dbStatus = "disconnected"
	} else {
		dbErr = db.Ping()
		if dbErr != nil {
			dbStatus = "disconnected"
		}
	}

	// Verificar conexión a Redis
	var redisErr error
	redisStatus := "disabled"
	if rdb != nil {
		ctxPing, cancelPing := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancelPing()
		redisErr = rdb.Ping(ctxPing).Err()
		if redisErr != nil {
			redisStatus = "disconnected"
		} else {
			redisStatus = "connected"
		}
	}

	// Determinar estado general (Redis no es crítico)
	overall := "healthy"
	if dbErr != nil {
		overall = "unhealthy"
	}

	status := gin.H{
		"status":    overall,
		"database":  dbStatus,
		"redis":     redisStatus,
		"timestamp": time.Now(),
	}

	if dbErr != nil {
		status["database_error"] = dbErr.Error()
	}
	if redisErr != nil {
		status["redis_error"] = redisErr.Error()
	}

	// Always return 200 so Azure App Service doesn't restart the container.
	// DB/Redis issues are operational, not app-level failures.
	c.JSON(http.StatusOK, status)
}

// extractProductBrand extrae la marca del producto desde diferentes fuentes
func extractProductBrand(product Product) string {
	// Intentar extraer de atributos PIM primero
	if product.PIMAttributes != nil && *product.PIMAttributes != "" {
		if brand := extractBrandFromPIMAttributes(*product.PIMAttributes); brand != "" {
			return brand
		}
	}

	// Intentar extraer del nombre del producto
	if brand := extractBrandFromProductName(product.Name); brand != "" {
		return brand
	}

	// Como fallback, consultar base de datos para obtener nombre real de la marca
	if product.BrandId != "" && product.BrandId != "00000000-0000-0000-0000-000000000000" {
		if brandName := getBrandNameFromDatabase(product.BrandId); brandName != "" {
			return brandName
		}
	}

	return ""
}

// getBrandNameFromDatabase obtiene el nombre real de la marca desde la base de datos
func getBrandNameFromDatabase(brandId string) string {
	var brandName string

	// Consultar la tabla de marcas en la base de datos
	query := `SELECT name FROM brands WHERE id = $1`
	err := db.QueryRow(query, brandId).Scan(&brandName)

	if err != nil {
		// Si no se encuentra en BD, intentar extraer de tablas relacionadas
		// Consultar si existe información en otras tablas
		alternativeQuery := `
			SELECT COALESCE(b.name, p.brand_name, '') 
			FROM brands b 
			RIGHT JOIN products p ON p.brand_id = b.id 
			WHERE b.id = $1 OR p.brand_id = $1 
			LIMIT 1`

		err2 := db.QueryRow(alternativeQuery, brandId).Scan(&brandName)
		if err2 != nil {
			log.Printf("⚠️ No se pudo obtener nombre de marca para ID: %s", brandId)
			return ""
		}
	}

	return strings.TrimSpace(brandName)
}

// extractBrandFromPIMAttributes extrae la marca de los atributos PIM
func extractBrandFromPIMAttributes(pimAttributes string) string {
	// Buscar patrones comunes de marca en atributos PIM
	brandPatterns := []string{
		`(?i)marca[:\s]*["']?([^"\n\r,;:{}]+?)["']?`,
		`(?i)brand[:\s]*["']?([^"\n\r,;:{}]+?)["']?`,
		`(?i)fabricante[:\s]*["']?([^"\n\r,;:{}]+?)["']?`,
		`(?i)manufacturer[:\s]*["']?([^"\n\r,;:{}]+?)["']?`,
	}

	for _, pattern := range brandPatterns {
		if match := regexp.MustCompile(pattern).FindStringSubmatch(pimAttributes); len(match) > 1 {
			brand := strings.TrimSpace(match[1])
			brand = strings.Trim(brand, "\"'")

			// Validar que la marca extraída sea válida
			if isValidBrand(brand) {
				return brand
			}
		}
	}

	return ""
}

// isValidBrand verifica si la marca extraída es válida
func isValidBrand(brand string) bool {
	if brand == "" || len(brand) < 2 {
		return false
	}

	// Rechazar marcas que parecen códigos JSON o errores de parsing
	invalidPatterns := []string{
		`Code`,
		`^\d+$`,             // Solo números
		`:`,                 // Contiene dos puntos (JSON)
		`{|}`,               // Contiene llaves (JSON)
		`^\d+["\s]*$`,       // Números con comillas
		`^["\s]*\d+["\s]*$`, // Números rodeados de comillas/espacios
	}

	for _, pattern := range invalidPatterns {
		if matched, _ := regexp.MatchString(pattern, brand); matched {
			return false
		}
	}

	// Debe contener al menos una letra
	hasLetter := regexp.MustCompile(`[a-zA-ZáéíóúñüÁÉÍÓÚÑÜ]`).MatchString(brand)

	return hasLetter && len(brand) <= 50 // Marcas razonablemente cortas
}

// cleanProductName limpia nombres de productos corruptos o mal formateados
func cleanProductName(productName string) string {
	if productName == "" {
		return productName
	}

	// Arreglar comillas dobles escapadas incorrectamente
	cleanName := strings.ReplaceAll(productName, `""`, `"`)

	// Remover comillas al inicio y final si están duplicadas
	cleanName = regexp.MustCompile(`^"(.+)"$`).ReplaceAllString(cleanName, `$1`)

	// Limpiar espacios extra
	cleanName = regexp.MustCompile(`\s+`).ReplaceAllString(cleanName, " ")
	cleanName = strings.TrimSpace(cleanName)

	return cleanName
}

// extractBrandFromProductName intenta extraer la marca del nombre del producto usando patrones inteligentes
func extractBrandFromProductName(productName string) string {
	// Limpiar el nombre del producto
	cleanName := strings.Trim(productName, "\"")
	words := strings.Fields(cleanName)

	if len(words) == 0 {
		return ""
	}

	// Buscar marca al FINAL del nombre (patrón común: "producto de [marca]")
	lastWord := words[len(words)-1]
	lastWord = regexp.MustCompile(`[^\w]`).ReplaceAllString(lastWord, "")

	if isBrandLikely(lastWord) {
		return strings.Title(strings.ToLower(lastWord))
	}

	// Buscar marca al INICIO del nombre
	firstWord := strings.Trim(words[0], "\"")
	firstWord = regexp.MustCompile(`[^\w]`).ReplaceAllString(firstWord, "")

	if isBrandLikely(firstWord) {
		return strings.Title(strings.ToLower(firstWord))
	}

	// Buscar en el medio usando patrones como "de [marca]", "marca:", etc.
	nameUpper := strings.ToUpper(cleanName)
	brandPatterns := []string{
		` DE ([A-Z]{3,})`, // "producto de MARCA"
		` ([A-Z]{3,}):`,   // "MARCA: producto"
		`([A-Z]{3,}) -`,   // "MARCA - producto"
		`- ([A-Z]{3,})`,   // "producto - MARCA"
	}

	for _, pattern := range brandPatterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(nameUpper)
		if len(matches) > 1 && isBrandLikely(matches[1]) {
			return strings.Title(strings.ToLower(matches[1]))
		}
	}

	return ""
}

// isBrandLikely determina si una palabra parece ser una marca usando heurísticas
func isBrandLikely(word string) bool {
	if len(word) < 3 {
		return false
	}

	// No debe ser solo números
	if regexp.MustCompile(`^\d+$`).MatchString(word) {
		return false
	}

	// No debe ser una medida común
	commonMeasures := []string{"PACK", "PULGADA", "PULGADAS", "INCH", "CM", "MM", "KG", "GR", "UNIDAD", "UNIDADES", "PCS", "PIECES"}
	wordUpper := strings.ToUpper(word)
	for _, measure := range commonMeasures {
		if wordUpper == measure {
			return false
		}
	}

	// No debe ser un descriptor común, color, material o palabra de test
	commonDescriptors := []string{
		// Productos/partes
		"TIRADOR", "BISAGRA", "TORNILLO", "TUERCA", "CABLE", "CONECTOR", "ADAPTADOR", "SOPORTE",
		"REFRIGERADOR", "ESTUFA", "LAVADORA", "SECADORA", "MICROONDAS", "LICUADORA", "VENTILADOR",
		// Colores (CRÍTICO - no confundir con marcas)
		"GRAFITO", "GRAPHITE", "NEGRO", "BLACK", "BLANCO", "WHITE", "GRIS", "GRAY", "GREY",
		"PLATEADO", "SILVER", "DORADO", "GOLD", "ROJO", "RED", "AZUL", "BLUE", "VERDE", "GREEN",
		"CAFE", "BROWN", "BEIGE", "CREMA", "CREAM", "CORAL", "MARFIL", "IVORY", "CHAMPAGNE",
		"ACERO", "STEEL", "INOXIDABLE", "STAINLESS", "CROMADO", "CHROME", "BRONCE", "BRONZE",
		// Materiales
		"METAL", "METALICO", "PLASTICO", "PLASTIC", "MADERA", "WOOD", "VIDRIO", "GLASS",
		"CERAMICA", "CERAMIC", "PORCELANA", "PORCELAIN", "ALUMINIO", "ALUMINUM",
		// Formas/acabados
		"SOLIDO", "SÓLIDO", "REDONDO", "CUADRADO", "MATE", "MATTE", "BRILLANTE", "GLOSSY",
		// Test/demos
		"DEMO", "TEST", "SAMPLE", "PRUEBA", "EJEMPLO",
	}
	for _, descriptor := range commonDescriptors {
		if wordUpper == descriptor {
			return false
		}
	}

	// Debe tener al menos algunas letras
	letterCount := len(regexp.MustCompile(`[a-zA-Z]`).FindAllString(word, -1))
	if float64(letterCount)/float64(len(word)) < 0.7 {
		return false
	}

	return true
}

// isContentFromCompetitor determina si el contenido proviene de un competidor
func isContentFromCompetitor(content string, productBrand string) bool {
	if productBrand == "" {
		return false // Si no tenemos marca, no podemos filtrar
	}

	contentLower := strings.ToLower(content)
	brandLower := strings.ToLower(productBrand)

	// Lista de vendedores/marketplaces (EXPANDIDA para mercado hispano)
	vendorIndicators := []string{
		// Marketplaces internacionales
		"amazon", "ebay", "mercadolibre", "aliexpress", "alibaba", "wish", "shopee",
		"newegg", "bestbuy", "walmart", "target", "homedepot", "lowes",

		// Vendedores españoles/hispanos
		"pccomponentes", "mediamarkt", "carrefour", "elcorteingles", "leroy merlin",
		"fnac", "worten", "coolmod", "alternate", "ldlc", "redcoon", "micom",
		"bricomart", "bricodepot", "aki", "bauhaus", "sodimac", "easy", "homecenter",
		"construrama", "coppel", "liverpool", "palacio de hierro", "falabella",

		// Indicadores comerciales
		"comprar en", "precio en", "oferta en", "disponible en", "envío desde",
		"tienda online", "envío gratis", "mejor precio", "precio más bajo",
		"descuento", "promoción", "liquidación", "rebaja", "outlet",
		"vs ", " vs", "comparar", "comparativa", "comparación",
		"review", "análisis", "opiniones", "reseña", "comentario", "evaluación",
		"test", "prueba", "ranking", "top 10", "mejores", "recomendado",
		"compra aquí", "añadir al carrito", "stock disponible", "entrega inmediata",
	}

	// Si el contenido contiene indicadores de vendedores, es probable que sea de competidor
	for _, indicator := range vendorIndicators {
		if strings.Contains(contentLower, indicator) {
			log.Printf("🚫 FILTERED COMPETITOR CONTENT (vendor indicator): '%s' found in content", indicator)
			return true
		}
	}

	// Detectar marcas competidoras específicas del contexto del producto
	// Esto es más dinámico que una lista fija
	competitorBrandPatterns := []string{
		// Patrones que indican menciones de múltiples marcas (comparativo)
		brandLower + " vs ", " vs " + brandLower,
		brandLower + " o ", brandLower + " y ",
		"entre " + brandLower, brandLower + " similar",
		"alternativa a " + brandLower, "equivalente a " + brandLower,
		"mejor que " + brandLower, brandLower + " competencia",
	}

	for _, pattern := range competitorBrandPatterns {
		if strings.Contains(contentLower, pattern) {
			log.Printf("🚫 FILTERED COMPETITOR CONTENT (comparative): pattern '%s' found", pattern)
			return true
		}
	}

	// Si menciona múltiples marcas (contenido comparativo)
	brandMentions := strings.Count(contentLower, brandLower)
	if brandMentions > 0 {
		// Buscar palabras que indican múltiples opciones/marcas
		multiOptionIndicators := []string{
			"también disponible", "otros modelos", "otras marcas", "diferentes marcas",
			"variedad de", "selección de", "múltiples opciones", "diversas marcas",
		}

		for _, indicator := range multiOptionIndicators {
			if strings.Contains(contentLower, indicator) {
				log.Printf("🚫 FILTERED COMPETITOR CONTENT (multiple options): '%s' found", indicator)
				return true
			}
		}
	}

	return false
}

// validateProductConsistency valida la consistencia de los datos del producto enriquecido
func validateProductConsistency(original Product, enhanced map[string]interface{}) []string {
	var inconsistencies []string

	// Extraer marca original
	originalBrand := extractProductBrand(original)

	// Validar que se preserve la marca en el nombre
	if enhancedName, ok := enhanced["name"].(string); ok && enhancedName != "" {
		if originalBrand != "" {
			enhancedNameLower := strings.ToLower(enhancedName)
			originalBrandLower := strings.ToLower(originalBrand)
			if !strings.Contains(enhancedNameLower, originalBrandLower) {
				inconsistencies = append(inconsistencies, fmt.Sprintf("Marca perdida: '%s' no está presente en el nombre mejorado", originalBrand))
			}
		}
	}

	// Validar consistencia en descripciones
	if shortDesc, ok := enhanced["shortDescription"].(string); ok {
		if fullDesc, ok2 := enhanced["fullDescription"].(string); ok2 {
			if detectMaterialInconsistency(shortDesc, fullDesc) {
				inconsistencies = append(inconsistencies, "Inconsistencia detectada en materiales entre descripciones")
			}
		}
	}

	return inconsistencies
}

// detectMaterialInconsistency detecta inconsistencias en materiales
func detectMaterialInconsistency(shortDesc, fullDesc string) bool {
	materials := []string{"acero", "aluminio", "plástico", "madera", "vidrio", "metal", "zamak", "hierro", "cobre"}

	shortMaterials := []string{}
	fullMaterials := []string{}

	shortDescLower := strings.ToLower(shortDesc)
	fullDescLower := strings.ToLower(fullDesc)

	for _, material := range materials {
		if strings.Contains(shortDescLower, material) {
			shortMaterials = append(shortMaterials, material)
		}
		if strings.Contains(fullDescLower, material) {
			fullMaterials = append(fullMaterials, material)
		}
	}

	// Verificar conflictos directos (ej: plástico vs metal)
	conflictingMaterials := map[string][]string{
		"plástico": {"metal", "acero", "aluminio", "hierro"},
		"madera":   {"metal", "plástico"},
		"vidrio":   {"plástico", "metal"},
	}

	allMaterials := append(shortMaterials, fullMaterials...)
	for _, material1 := range allMaterials {
		if conflicts, exists := conflictingMaterials[material1]; exists {
			for _, material2 := range allMaterials {
				for _, conflict := range conflicts {
					if material2 == conflict {
						return true // Conflicto detectado
					}
				}
			}
		}
	}

	return false
}

// incorporateBrandInName incorpora la marca en el nombre del producto de manera natural
func incorporateBrandInName(productName, brand string) string {
	// Si ya contiene la marca, devolver como está
	if strings.Contains(strings.ToLower(productName), strings.ToLower(brand)) {
		return productName
	}

	// Limpiar comillas del nombre original si las tiene
	cleanName := strings.Trim(productName, "\"")
	cleanName = strings.TrimSpace(cleanName)

	// Incorporar la marca al inicio del nombre de forma natural
	brandCapitalized := strings.Title(strings.ToLower(brand))

	// Si el nombre empieza con un número o medida, poner la marca antes
	if matched, _ := regexp.MatchString(`^\d`, cleanName); matched {
		return fmt.Sprintf("%s %s", brandCapitalized, cleanName)
	}

	// Para otros casos, analizar estructura del nombre
	words := strings.Fields(cleanName)
	if len(words) > 0 {
		// Si la primera palabra parece ser una medida o número, insertar marca al inicio
		firstWord := strings.ToLower(words[0])
		if strings.Contains(firstWord, "pack") || strings.Contains(firstWord, "\"") ||
			strings.Contains(firstWord, "pulgada") || regexp.MustCompile(`^\d`).MatchString(firstWord) {
			return fmt.Sprintf("%s %s", brandCapitalized, cleanName)
		}
	}

	// Por defecto, agregar la marca al inicio
	return fmt.Sprintf("%s %s", brandCapitalized, cleanName)
}

// getProductBarcodes obtiene todos los códigos de barras activos del producto
// desde catalog.tblBarcode. El DefaultBarcode va primero; el resto en orden de creación.
func getProductBarcodes(productId string) []string {
	if db == nil {
		log.Printf("⚠️ getProductBarcodes: DB not available")
		return nil
	}

	query := `
		SELECT Barcode, ISNULL(DefaultBarcode, 0) AS DefaultBarcode
		FROM catalog.tblBarcode
		WHERE ProductId = CAST(@p1 AS UNIQUEIDENTIFIER) AND Status = 1
		ORDER BY ISNULL(DefaultBarcode, 0) DESC, CreatedDate ASC`

	rows, err := db.Query(query, productId)
	if err != nil {
		log.Printf("⚠️ getProductBarcodes: query error: %v", err)
		return nil
	}
	defer rows.Close()

	var barcodes []string
	seen := make(map[string]bool)
	for rows.Next() {
		var barcode string
		var isDefault bool
		if err := rows.Scan(&barcode, &isDefault); err != nil {
			continue
		}
		barcode = strings.TrimSpace(barcode)
		if barcode == "" || seen[barcode] {
			continue
		}
		seen[barcode] = true
		barcodes = append(barcodes, barcode)
	}

	if len(barcodes) > 0 {
		log.Printf("🔖 getProductBarcodes: found %d barcodes for %s: %v", len(barcodes), productId, barcodes)
	} else {
		log.Printf("🔖 getProductBarcodes: no barcodes found for product %s", productId)
	}
	return barcodes
}

// getFirstProductBarcode devuelve solo el primer barcode (el default si existe).
func getFirstProductBarcode(productId string) string {
	barcodes := getProductBarcodes(productId)
	if len(barcodes) == 0 {
		return ""
	}
	return barcodes[0]
}
