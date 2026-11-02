package main

import (
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gin-gonic/gin"
)

type ImageProcessingRequest struct {
	ProductId string                     `json:"productId" binding:"required"`
	Images    []ImageProcessingSelection `json:"images" binding:"required"`
	Options   ProcessingOptions          `json:"options"`
}

type ImageProcessingSelection struct {
	URL           string `json:"url" binding:"required"`
	ImageType     string `json:"imageType" binding:"required"` // "principal", "secundaria", "ambiente"
	OriginalIndex int    `json:"originalIndex"`
	Priority      int    `json:"priority"`
	Title         string `json:"title"`
	// Nuevos campos para flujo secuencial manual
	MetaTitle       string `json:"metaTitle"`
	MetaDescription string `json:"metaDescription"`
	IsDecorated     bool   `json:"isDecorated"`
	Order           int    `json:"order"`
}

type ProcessingOptions struct {
	MaxWidth        int  `json:"maxWidth"`        // Default: 1200px
	MaxHeight       int  `json:"maxHeight"`       // Default: 1200px
	Quality         int  `json:"quality"`         // JPEG quality 1-100
	CreateThumbnail bool `json:"createThumbnail"` // Default: true
	ThumbnailSize   int  `json:"thumbnailSize"`   // Default: 300px
	SaveToDatabase  bool `json:"saveToDatabase"`  // Default: true
}

// REMOVED: DirectUpload structs - el cliente now handles database insertion directly

// Nueva estructura simplificada - solo metadatos paral cliente
type ImageMetadataResult struct {
	OriginalURL     string `json:"originalUrl"`
	ImageType       string `json:"imageType"`
	Title           string `json:"title"`
	Source          string `json:"source"`
	Dimensions      string `json:"dimensions,omitempty"`
	MediaResourceId string `json:"mediaResourceId"`
	Success         bool   `json:"success"`
	Error           string `json:"error,omitempty"`
	// Metadata para procesamiento en el cliente
	MetaTitle       string `json:"metaTitle"`
	MetaDescription string `json:"metaDescription"`
	IsDecorated     bool   `json:"isDecorated"`
	Order           int    `json:"order"`
}

// REMOVED: ProcessedImageResult struct - el cliente now handles database insertion directly

// Nueva respuesta simplificada - solo metadatos
type ImageMetadataResponse struct {
	ProductId      string                `json:"productId"`
	ImageMetadata  []ImageMetadataResult `json:"imageMetadata"`
	TotalProcessed int                   `json:"totalProcessed"`
	TotalSuccess   int                   `json:"totalSuccess"`
	TotalErrors    int                   `json:"totalErrors"`
	ProcessingTime int64                 `json:"processingTime"`
	Message        string                `json:"message"`
	NextSteps      []string              `json:"nextSteps"`
}

// REMOVED: ImageProcessingResponse struct - el cliente now handles database insertion directly

func processSelectedImages(c *gin.Context) {
	startTime := time.Now()
	productId := c.Param("id")

	var request ImageProcessingRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:   "Invalid request format",
			Code:    "INVALID_REQUEST",
			Details: err.Error(),
		})
		return
	}

	// Validar producto existe - usar ID de la URL si no viene en el request
	if request.ProductId == "" {
		request.ProductId = productId
	}

	if !uuidRegex.MatchString(request.ProductId) {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:   "Invalid product ID format",
			Code:    "INVALID_PRODUCT_ID",
			Details: "Product ID must be a valid UUID",
		})
		return
	}

	// Verificar que el producto existe
	product, err := findProductByID(request.ProductId)
	if err != nil {
		log.Printf("ERROR: Product not found: %s, Error: %v", request.ProductId, err)
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error:   "Product not found",
			Code:    "PRODUCT_NOT_FOUND",
			Details: err.Error(),
		})
		return
	}

	// COMENTADO: Conflictos ahora se manejan desdel cliente
	// err = handleExistingImagesConflicts(productId, request.Images)
	// if err != nil {
	//     log.Printf("Warning handling existing images conflicts: %v", err)
	//     // No fallar el procesamiento, solo logear advertencia
	// }

	// NUEVO FLUJO: Solo preparar metadatos completos para inserción desdel cliente
	var processedImages []ProcessedImageForClient
	successCount := 0
	errorCount := 0

	// CompanyId será manejado por el cliente durante la inserción
	companyId := "" // el cliente obtendrá esto del contexto

	// MediaCategoryId será manejado por el cliente durante la inserción
	mediaCategoryId := "5A511A32-64F3-4AE9-AB8C-D185BEFDDB20" // Fallback por defecto

	for _, imgSelection := range request.Images {
		imageStartTime := time.Now()

		// Obtener MediaResourceId para el tipo de imagen
		rawMediaResourceId, err := getMediaResourceForImageType(imgSelection.ImageType)
		if err != nil {
			processedImages = append(processedImages, ProcessedImageForClient{
				OriginalURL: imgSelection.URL,
				ImageType:   imgSelection.ImageType,
				Title:       imgSelection.Title,
				Source:      extractDomainFromURL(imgSelection.URL),
				Success:     false,
				Error:       fmt.Sprintf("MediaResourceId error: %v", err),
			})
			errorCount++
			continue
		}

		// Limpiar y validar el MediaResourceId
		mediaResourceId, err := cleanAndValidateUUID(rawMediaResourceId)
		if err != nil {
			log.Printf("ERROR: Invalid MediaResourceId format for type %s: %s",
				imgSelection.ImageType, err.Error())
			processedImages = append(processedImages, ProcessedImageForClient{
				OriginalURL: imgSelection.URL,
				ImageType:   imgSelection.ImageType,
				Title:       imgSelection.Title,
				Source:      extractDomainFromURL(imgSelection.URL),
				Success:     false,
				Error:       fmt.Sprintf("Invalid MediaResourceId format: %v", err),
			})
			errorCount++
			continue
		}

		// Generar metadatos basados en la imagen y el producto
		metaTitle, metaDescription := generateImageMetadataForProduct(product.Name, imgSelection.ImageType, imgSelection.Title)

		// Determinar si es imagen principal
		isDefaultPicture := (imgSelection.ImageType == "principal")

		// Ajustar Order para imagen principal
		imageOrder := imgSelection.Order
		if isDefaultPicture && imageOrder == 0 {
			imageOrder = 1 // Usar Order = 1 para imagen principal
		}

		// Generar UUID para MediaId
		mediaId := uuid.New().String()

		// Generar nombre de archivo por defecto - el cliente determinará el real
		originalFileName := fmt.Sprintf("image_%d.jpg", time.Now().Unix())
		contentType := "image/jpeg" // Asumir JPEG por defecto

		processedImages = append(processedImages, ProcessedImageForClient{
			OriginalURL: imgSelection.URL,
			ImageType:   imgSelection.ImageType,
			Title:       imgSelection.Title,
			Source:      extractDomainFromURL(imgSelection.URL),
			Success:     true,
			Metadata: ImageMetadata{
				MediaId:               mediaId,
				MediaResourceId:       mediaResourceId,
				MediaCategoryId:       mediaCategoryId,
				OriginalFileName:      originalFileName,
				ContentType:           contentType,
				MetaTitle:             metaTitle,
				MetaDescription:       metaDescription,
				DefaultProductPicture: isDefaultPicture,
				Order:                 imageOrder,
				IsDecorated:           imgSelection.IsDecorated,
				CompanyId:             companyId,
			},
			ProcessingTime: time.Since(imageStartTime).Milliseconds(),
		})
		successCount++
	}

	// Generar mensaje de resumen
	message := fmt.Sprintf("Metadata preparado para %d imágenes: %d exitosas, %d errores",
		len(request.Images), successCount, errorCount)
	if successCount > 0 {
		message += ". el cliente procesará mediante el servicio de medios (flujo manual)."
	}

	response := ProcessedImagesResponse{
		ProductId:       productId,
		ProcessedImages: processedImages,
		TotalProcessed:  len(request.Images),
		TotalSuccess:    successCount,
		TotalErrors:     errorCount,
		ProcessingTime:  time.Since(startTime).Milliseconds(),
		Message:         message,
		NextSteps: []string{
			"el cliente descargará las imágenes desde las URLs originales",
			"el cliente usará el servicio de medios (flujo idéntico a carga manual)",
			"El servicio de medios manejará automáticamente: Azure upload + BD insertion",
		},
	}

	c.JSON(http.StatusOK, response)
}

// Funciones auxiliares para el nuevo flujo de metadatos

func generateImageMetadataForProduct(productName, imageType, imageTitle string) (string, string) {
	// Limpiar y normalizar datos de entrada
	productName = strings.TrimSpace(productName)
	imageTitle = strings.TrimSpace(imageTitle)

	// Limpiar comillas problemáticas del nombre del producto
	productName = strings.ReplaceAll(productName, "\"", "")
	productName = strings.ReplaceAll(productName, "'", "")
	productName = strings.ReplaceAll(productName, `"`, "")
	productName = strings.ReplaceAll(productName, `"`, "")

	// Limpiar título de imagen
	imageTitle = strings.ReplaceAll(imageTitle, "\"", "")
	imageTitle = strings.ReplaceAll(imageTitle, "'", "")

	// Si el título de imagen está vacío, generar uno basado en el tipo
	if imageTitle == "" {
		switch imageType {
		case "principal":
			imageTitle = "Vista principal del producto"
		case "secundaria":
			imageTitle = "Vista alternativa del producto"
		case "ambiente":
			imageTitle = "Producto en su ambiente de uso"
		default:
			imageTitle = "Vista del producto"
		}
	}

	var metaTitle, metaDescription string

	// Generar meta título más descriptivo y balanceado
	switch imageType {
	case "principal":
		if len(productName) > 50 {
			metaTitle = fmt.Sprintf("%s - Principal", truncateStringAtWord(productName, 50))
		} else {
			metaTitle = fmt.Sprintf("%s - Imagen Principal", productName)
		}
	case "secundaria":
		// Estrategia inteligente para títulos de imagen secundaria
		suffix := " - Vista Alternativa"

		// Si hay título específico de imagen, intentar usarlo
		if len(imageTitle) > 10 && !strings.Contains(strings.ToLower(imageTitle), "vista") {
			// Extraer palabras clave del título de imagen
			imageKeywords := extractKeywords(imageTitle)
			if len(imageKeywords) > 0 {
				keyword := imageKeywords[0]
				if len(keyword) < 20 {
					suffix = fmt.Sprintf(" - %s", keyword)
				}
			}
		}

		// Calcular espacio disponible para nombre del producto
		availableSpace := 60 - len(suffix)
		if len(productName) > availableSpace {
			metaTitle = fmt.Sprintf("%s%s", truncateStringAtWord(productName, availableSpace), suffix)
		} else {
			metaTitle = fmt.Sprintf("%s%s", productName, suffix)
		}

	case "ambiente":
		if len(productName) > 45 {
			metaTitle = fmt.Sprintf("%s - En Uso", truncateStringAtWord(productName, 45))
		} else {
			metaTitle = fmt.Sprintf("%s - Ambiente de Uso", productName)
		}
	default:
		metaTitle = fmt.Sprintf("%s - %s", truncateStringAtWord(productName, 45), getImageTypeDisplayName(imageType))
	}

	// Generar meta descripción más rica y contextual (sin comillas problemáticas)
	switch imageType {
	case "principal":
		if strings.Contains(strings.ToLower(imageTitle), "frente") || strings.Contains(strings.ToLower(imageTitle), "frontal") {
			metaDescription = fmt.Sprintf("Vista frontal de %s. Imagen principal que muestra las características más importantes del producto.", productName)
		} else {
			metaDescription = fmt.Sprintf("Imagen principal de %s. Vista destacada que muestra el producto en detalle con sus características principales.", productName)
		}
	case "secundaria":
		// Generar descripción inteligente basada en keywords
		keywords := extractKeywords(imageTitle)
		if len(keywords) > 0 {
			keywordStr := strings.Join(keywords, ", ")
			metaDescription = fmt.Sprintf("Vista adicional de %s con características: %s. Muestra detalles específicos del producto.", productName, keywordStr)
		} else if strings.Contains(strings.ToLower(imageTitle), "lateral") {
			metaDescription = fmt.Sprintf("Vista lateral de %s. Muestra el perfil y detalles laterales del producto.", productName)
		} else if strings.Contains(strings.ToLower(imageTitle), "trasera") || strings.Contains(strings.ToLower(imageTitle), "posterior") {
			metaDescription = fmt.Sprintf("Vista posterior de %s. Muestra la parte trasera y detalles de instalación.", productName)
		} else if strings.Contains(strings.ToLower(imageTitle), "detalle") {
			metaDescription = fmt.Sprintf("Imagen en detalle de %s. Muestra características técnicas y acabados específicos.", productName)
		} else {
			// Evitar repetir el título problemático, usar descripción genérica
			metaDescription = fmt.Sprintf("Vista adicional de %s. Imagen complementaria que muestra detalles y características del producto.", productName)
		}
	case "ambiente":
		if strings.Contains(strings.ToLower(imageTitle), "uso") {
			metaDescription = fmt.Sprintf("%s en su ambiente natural de uso. Muestra cómo se integra el producto en su contexto real.", productName)
		} else {
			metaDescription = fmt.Sprintf("%s mostrado en contexto real. Imagen que ilustra cómo se ve y utiliza el producto en situaciones reales.", productName)
		}
	default:
		metaDescription = fmt.Sprintf("Imagen de %s. Vista del producto que muestra sus características y detalles.", productName)
	}

	// Aplicar límites de longitud con mejores cortes
	if len(metaTitle) > 60 {
		metaTitle = truncateString(metaTitle, 57) + "..."
	}

	if len(metaDescription) > 160 {
		// Intentar cortar en una palabra completa
		truncated := truncateString(metaDescription, 157)
		if lastSpace := strings.LastIndex(truncated, " "); lastSpace > 140 {
			metaDescription = truncated[:lastSpace] + "..."
		} else {
			metaDescription = truncated + "..."
		}
	}

	return metaTitle, metaDescription
}

// Limpiar y validar UUID para asegurar formato correcto
func cleanAndValidateUUID(rawUUID string) (string, error) {

	// Paso 1: Remover espacios en blanco y caracteres de control
	cleaned := strings.TrimSpace(rawUUID)

	// Paso 2: Verificar si ya es un UUID válido
	if _, err := uuid.Parse(cleaned); err == nil {
		return cleaned, nil
	}

	// Paso 3: Limpieza agresiva - remover caracteres no-ASCII y de control
	cleanedBytes := make([]byte, 0, len(cleaned))
	for _, b := range []byte(cleaned) {
		// Solo mantener caracteres hexadecimales y guiones
		if (b >= '0' && b <= '9') || (b >= 'A' && b <= 'F') || (b >= 'a' && b <= 'f') || b == '-' {
			cleanedBytes = append(cleanedBytes, b)
		}
	}
	cleaned = string(cleanedBytes)

	// Paso 4: Intentar parsear después de limpieza
	if _, err := uuid.Parse(cleaned); err == nil {
		return cleaned, nil
	}

	// Paso 5: Usar regex para extraer patrón UUID
	uuidRegex := regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	if uuidRegex.MatchString(cleaned) {
		match := uuidRegex.FindString(cleaned)
		if _, err := uuid.Parse(match); err == nil {
			return match, nil
		}
	}

	// Paso 6: Reconstruir desde caracteres hex solamente
	hexOnly := regexp.MustCompile(`[^0-9a-fA-F]`).ReplaceAllString(cleaned, "")

	if len(hexOnly) >= 32 {
		// Si tenemos exactamente 32 caracteres hex, reconstruir formato UUID
		if len(hexOnly) == 32 {
			reconstructed := fmt.Sprintf("%s-%s-%s-%s-%s",
				hexOnly[0:8], hexOnly[8:12], hexOnly[12:16], hexOnly[16:20], hexOnly[20:32])
			if _, err := uuid.Parse(reconstructed); err == nil {
				return reconstructed, nil
			}
		}
		// Si tenemos más de 32, tomar los primeros 32
		if len(hexOnly) > 32 {
			hexOnly = hexOnly[:32]
			reconstructed := fmt.Sprintf("%s-%s-%s-%s-%s",
				hexOnly[0:8], hexOnly[8:12], hexOnly[12:16], hexOnly[16:20], hexOnly[20:32])
			if _, err := uuid.Parse(reconstructed); err == nil {
				return reconstructed, nil
			}
		}
	}

	return "", fmt.Errorf("unable to clean UUID format from: '%s' (length: %d, hex_extracted: '%s')", rawUUID, len(rawUUID), hexOnly)
}

// REMOVED: media-service structs - el cliente now handles database insertion directly

// REMOVED: uploadImageViaMediaService - el cliente now handles database insertion directly

// REMOVED: insertImageToDatabase - el cliente now handles database insertion directly

// REMOVED: processAndUploadToAzure - el cliente now handles database insertion directly

func getImageTypeDisplayName(imageType string) string {
	switch imageType {
	case "principal":
		return "Imagen Principal"
	case "secundaria":
		return "Imagen Secundaria"
	case "ambiente":
		return "Imagen de Ambiente"
	default:
		return "Imagen"
	}
}

func extractDomainFromURL(url string) string {
	if strings.Contains(url, "://") {
		parts := strings.Split(url, "://")
		if len(parts) > 1 {
			domain := strings.Split(parts[1], "/")[0]
			return domain
		}
	}
	return "unknown"
}

func truncateString(str string, maxLen int) string {
	if len(str) <= maxLen {
		return str
	}
	return str[:maxLen]
}

// truncateStringAtWord trunca en la última palabra completa
func truncateStringAtWord(str string, maxLen int) string {
	if len(str) <= maxLen {
		return str
	}

	// Buscar el último espacio antes del límite
	truncated := str[:maxLen]
	lastSpace := strings.LastIndex(truncated, " ")

	if lastSpace > maxLen/2 { // Solo truncar en palabra si no es muy corto
		return str[:lastSpace]
	}

	return truncated
}

// extractKeywords extrae palabras clave útiles del título de imagen
func extractKeywords(title string) []string {
	// Limpiar y dividir en palabras
	words := strings.Fields(strings.ToLower(title))
	var keywords []string

	// Palabras útiles para describir imágenes
	useful := map[string]bool{
		"chrome": true, "bronze": true, "steel": true, "brass": true,
		"hinge": true, "handle": true, "knob": true, "pull": true,
		"overlay": true, "inset": true, "self-closing": true,
		"cabinet": true, "door": true, "drawer": true,
		"pack": true, "pair": true, "set": true,
	}

	for _, word := range words {
		// Remover caracteres no alfanuméricos
		cleaned := strings.Trim(word, ".,!?()[]{}\"'")
		if len(cleaned) > 2 && useful[cleaned] {
			keywords = append(keywords, strings.Title(cleaned))
			if len(keywords) >= 3 { // Máximo 3 keywords
				break
			}
		}
	}

	return keywords
}

// REMOVED: updateImageUrlAfterBlobUpload - el cliente now handles all database operations

// REMOVED: Helper functions for database insertion - el cliente now handles this

// Endpoints adicionales
func getImageProcessingConfig(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"defaultOptions": ProcessingOptions{
			MaxWidth:        1200,
			MaxHeight:       1200,
			Quality:         85,
			CreateThumbnail: true,
			ThumbnailSize:   300,
			SaveToDatabase:  false,
		},
		"supportedFormats": []string{"jpeg", "jpg", "png"},
		"maxFileSize":      "10MB",
		"imageTypes":       []string{"principal", "secundaria", "ambiente"},
	})
}
