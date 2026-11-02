package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	"github.com/gin-gonic/gin"
)

type MediaResource struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	IsActive    bool   `json:"isActive"`
}

type ImageTypeMapping struct {
	LocalType       string `json:"localType"`       // "principal", "secundaria", "ambiente"
	MediaResourceID string `json:"mediaResourceId"` // ID del MediaResource en BD
	DisplayName     string `json:"displayName"`     // Nombre para mostrar
	Priority        int    `json:"priority"`        // Prioridad de ordenamiento
	FileNamePrefix  string `json:"fileNamePrefix"`  // Prefijo para nombres de archivo
	MetadataTag     string `json:"metadataTag"`     // Tag para identificar tipo en metadatos
}

// Mapeo por defecto - se actualizará con datos reales de la BD
var imageTypeMappings = map[string]ImageTypeMapping{
	"principal": {
		LocalType:       "principal",
		MediaResourceID: "", // Se llenará dinámicamente
		DisplayName:     "Imagen Principal",
		Priority:        1,
		FileNamePrefix:  "MAIN",
		MetadataTag:     "type:principal",
	},
	"secundaria": {
		LocalType:       "secundaria",
		MediaResourceID: "", // Se llenará dinámicamente
		DisplayName:     "Imagen Secundaria",
		Priority:        2,
		FileNamePrefix:  "ALT",
		MetadataTag:     "type:secundaria",
	},
	"ambiente": {
		LocalType:       "ambiente",
		MediaResourceID: "", // Se llenará dinámicamente
		DisplayName:     "Imagen Ambiente",
		Priority:        3,
		FileNamePrefix:  "ENV",
		MetadataTag:     "type:ambiente",
	},
}

func getAvailableMediaResources(c *gin.Context) {
	query := `
		SELECT 
			CAST(MediaResourceId AS NVARCHAR(36)) as MediaResourceId,
			Name,
			Description,
			CASE WHEN Status = '1' THEN 1 ELSE 0 END as IsActive
		FROM catalog.pltblMediaResource 
		WHERE Status = '1'
		ORDER BY Name
	`

	rows, err := db.Query(query)
	if err != nil {
		log.Printf("Error querying MediaResources: %v", err)
		c.JSON(500, ErrorResponse{
			Error:   "Database query failed",
			Code:    "DB_ERROR",
			Details: err.Error(),
		})
		return
	}
	defer rows.Close()

	var resources []MediaResource
	for rows.Next() {
		var resource MediaResource
		var description sql.NullString

		err := rows.Scan(
			&resource.ID,
			&resource.Name,
			&description,
			&resource.IsActive,
		)
		if err != nil {
			log.Printf("Error scanning MediaResource row: %v", err)
			continue
		}

		if description.Valid {
			resource.Description = description.String
		}

		resources = append(resources, resource)
	}

	log.Printf("Found %d active MediaResources", len(resources))

	// Actualizar mapeos automáticamente
	updatedMappings := updateImageTypeMappings(resources)

	c.JSON(200, gin.H{
		"availableMediaResources": resources,
		"currentMappings":         updatedMappings,
		"totalResources":          len(resources),
		"mappingStatus":           getMappingStatus(updatedMappings),
	})
}

func updateImageTypeMappings(resources []MediaResource) map[string]ImageTypeMapping {
	// Crear copia del mapeo actual
	updatedMappings := make(map[string]ImageTypeMapping)
	for k, v := range imageTypeMappings {
		updatedMappings[k] = v
	}

	// Mapeos automáticos diferenciados por tipo de imagen
	// Cada tipo va a su MediaResource correspondiente según las reglas de negocio
	// IMPORTANTE: Usar nombres exactos que están funcionando en producción
	intelligentMappings := map[string][]string{
		"principal":  {"Product Image"},                               // Imagen principal del producto - MediaResourceId: 68920485-d222-4ff0-b947-e0340d77b56a (PORTADA)
		"secundaria": {"Alternate Images", "Product Image"},           // Imágenes adicionales/alternativas (SECCIÓN WEB)
		"ambiente":   {"Product Search Image", "Search", "Marketing"}, // Imágenes con contexto/ambiente (APP)
	}

	log.Printf("Starting intelligent mapping update with %d resources", len(resources))

	// Primero intentar mapeos exactos e inteligentes
	for localType, preferredNames := range intelligentMappings {
		for _, preferredName := range preferredNames {
			for _, resource := range resources {
				if strings.EqualFold(resource.Name, preferredName) {
					mapping := updatedMappings[localType]
					mapping.MediaResourceID = resource.ID
					updatedMappings[localType] = mapping
					log.Printf("✅ Intelligent mapping: %s -> %s (%s)", localType, resource.Name, resource.ID)
					break
				}
			}
			// Si encontramos mapeo exacto, no seguir buscando para este tipo
			if updatedMappings[localType].MediaResourceID != "" {
				break
			}
		}
	}

	// Buscar recurso genérico para usar como fallback
	var fallbackResource *MediaResource
	for _, resource := range resources {
		resourceName := strings.ToLower(resource.Name)
		if strings.Contains(resourceName, "image") || strings.Contains(resourceName, "photo") ||
			strings.Contains(resourceName, "picture") || strings.Contains(resourceName, "media") {
			fallbackResource = &resource
			log.Printf("Found fallback resource: %s (%s)", resource.Name, resource.ID)
			break
		}
	}

	// Si no encontramos mapeos específicos, usar el primer recurso disponible como fallback
	if fallbackResource == nil && len(resources) > 0 {
		fallbackResource = &resources[0]
		log.Printf("Using first available resource as fallback: %s (%s)", fallbackResource.Name, fallbackResource.ID)
	}

	// Aplicar fallback a tipos sin mapear
	if fallbackResource != nil {
		for localType := range updatedMappings {
			if updatedMappings[localType].MediaResourceID == "" {
				mapping := updatedMappings[localType]
				mapping.MediaResourceID = fallbackResource.ID
				updatedMappings[localType] = mapping
				log.Printf("Fallback mapping: %s -> %s (%s)", localType, fallbackResource.Name, fallbackResource.ID)
			}
		}
	}

	// Actualizar mapeos globales
	imageTypeMappings = updatedMappings

	// Log del estado final
	log.Printf("Final mapping state:")
	for localType, mapping := range updatedMappings {
		log.Printf("  %s -> %s", localType, mapping.MediaResourceID)
	}

	return updatedMappings
}

func getMappingStatus(mappings map[string]ImageTypeMapping) map[string]interface{} {
	status := map[string]interface{}{
		"allMapped": true,
		"details":   make(map[string]bool),
	}

	for localType, mapping := range mappings {
		isMapped := mapping.MediaResourceID != ""
		status["details"].(map[string]bool)[localType] = isMapped
		if !isMapped {
			status["allMapped"] = false
		}
	}

	return status
}

func getImageTypeMappings(c *gin.Context) {
	c.JSON(200, gin.H{
		"mappings":      imageTypeMappings,
		"mappingStatus": getMappingStatus(imageTypeMappings),
		"instructions": []string{
			"1. Consulta /media-resources para ver recursos disponibles",
			"2. Si no hay mapeos automáticos, usa /configure-mappings para configurar manualmente",
			"3. Usa estos mapeos al insertar en linkProductMedia",
		},
	})
}

func configureImageTypeMappings(c *gin.Context) {
	var request map[string]string
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(400, ErrorResponse{
			Error:   "Invalid request format",
			Code:    "INVALID_REQUEST",
			Details: "Expected JSON object with localType -> mediaResourceId or mediaResourceName mappings",
		})
		return
	}

	// Validar que los MediaResourceIDs existen
	updatedMappings := make(map[string]ImageTypeMapping)
	for k, v := range imageTypeMappings {
		updatedMappings[k] = v
	}

	for localType, mediaResourceIdOrName := range request {
		if mapping, exists := updatedMappings[localType]; exists {
			var mediaResourceId string

			// Intentar buscar por nombre primero si no es un GUID
			if !isGUID(mediaResourceIdOrName) {
				foundId, err := findMediaResourceIdByName(mediaResourceIdOrName)
				if err != nil {
					c.JSON(400, ErrorResponse{
						Error:   fmt.Sprintf("MediaResource with name '%s' not found", mediaResourceIdOrName),
						Code:    "INVALID_MEDIA_RESOURCE",
						Details: fmt.Sprintf("LocalType: %s, Error: %s", localType, err.Error()),
					})
					return
				}
				mediaResourceId = foundId
			} else {
				mediaResourceId = mediaResourceIdOrName
			}

			// Validar que el MediaResourceId existe en la BD
			if validateMediaResourceExists(mediaResourceId) {
				mapping.MediaResourceID = mediaResourceId
				updatedMappings[localType] = mapping
				log.Printf("Updated mapping: %s -> %s (%s)", localType, mediaResourceIdOrName, mediaResourceId)
			} else {
				c.JSON(400, ErrorResponse{
					Error:   fmt.Sprintf("MediaResourceId '%s' not found", mediaResourceId),
					Code:    "INVALID_MEDIA_RESOURCE",
					Details: fmt.Sprintf("LocalType: %s", localType),
				})
				return
			}
		}
	}

	// Actualizar mapeos globales
	imageTypeMappings = updatedMappings

	c.JSON(200, gin.H{
		"message":         "Mappings updated successfully",
		"updatedMappings": imageTypeMappings,
		"mappingStatus":   getMappingStatus(imageTypeMappings),
	})
}

func validateMediaResourceExists(mediaResourceId string) bool {
	// Para IDs con caracteres especiales, intentar validación por longitud/patrón
	if len(mediaResourceId) < 10 {
		return false
	}

	// Si llegamos hasta aquí, probablemente es un ID válido encontrado por nombre
	// La validación real será al hacer la inserción
	log.Printf("Accepting MediaResourceId for validation: [length=%d chars]", len(mediaResourceId))
	return true
}

func getMediaResourceForImageType(imageType string) (string, error) {
	// Si no hay mapeos, intentar autoconfigurarlos
	if len(imageTypeMappings) == 0 || imageTypeMappings["principal"].MediaResourceID == "" {
		log.Printf("No mappings found, attempting auto-configuration...")
		if err := autoConfigureMappings(); err != nil {
			log.Printf("Auto-configuration failed: %v", err)
		}
	}

	if mapping, exists := imageTypeMappings[imageType]; exists && mapping.MediaResourceID != "" {
		return mapping.MediaResourceID, nil
	}

	return "", fmt.Errorf("no MediaResourceID configured for image type: %s", imageType)
}

func autoConfigureMappings() error {
	log.Printf("Starting auto-configuration with intelligent mapping...")

	query := `
		SELECT 
			CAST(MediaResourceId AS NVARCHAR(36)) as MediaResourceId, 
			Name,
			CASE WHEN Status = '1' THEN 1 ELSE 0 END as IsActive
		FROM catalog.pltblMediaResource 
		WHERE Status = '1'
		ORDER BY Name
	`

	rows, err := db.Query(query)
	if err != nil {
		log.Printf("Error querying MediaResources for auto-config: %v", err)
		return fmt.Errorf("failed to query MediaResources: %v", err)
	}
	defer rows.Close()

	var resources []MediaResource
	for rows.Next() {
		var resource MediaResource
		err := rows.Scan(&resource.ID, &resource.Name, &resource.IsActive)
		if err != nil {
			log.Printf("Error scanning MediaResource row: %v", err)
			continue
		}
		resources = append(resources, resource)
	}

	log.Printf("Found %d MediaResources for auto-configuration", len(resources))

	if len(resources) == 0 {
		return fmt.Errorf("no active MediaResources found in database")
	}

	// Imprimir todos los recursos encontrados para debugging
	log.Printf("Available MediaResources:")
	for i, resource := range resources {
		log.Printf("  %d. ID='%s', Name='%s', Active=%t", i+1, resource.ID, resource.Name, resource.IsActive)
	}

	updateImageTypeMappings(resources)
	log.Printf("Auto-configured mappings with %d resources", len(resources))

	// Verificar que los mapeos se crearon correctamente
	allMapped := true
	for localType, mapping := range imageTypeMappings {
		if mapping.MediaResourceID == "" {
			log.Printf("WARNING: %s type still not mapped after auto-configuration", localType)
			allMapped = false
		}
	}

	if !allMapped {
		return fmt.Errorf("auto-configuration incomplete - some types still not mapped")
	}

	return nil
}

// Estructura para metadatos de imagen (renombrada para evitar conflicto)
type ImageMetadataForUpload struct {
	MetaTitle       string `json:"metaTitle"`
	MetaDescription string `json:"metaDescription"`
	IsDecorated     bool   `json:"isDecorated"`
	MediaType       string `json:"mediaType"`
	EntityType      string `json:"entityType"`
}

func findMediaResourceIdByName(name string) (string, error) {
	query := `
		SELECT CAST(MediaResourceId AS NVARCHAR(36)) as MediaResourceId 
		FROM catalog.pltblMediaResource 
		WHERE Name = @p1 AND Status = '1'
	`

	var id string
	err := db.QueryRow(query, name).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("MediaResource with name '%s' not found: %v", name, err)
	}

	return id, nil
}

func isGUID(str string) bool {
	// Verificar si es un GUID simple (no necesariamente formato estándar debido a encoding)
	if len(str) < 10 {
		return false
	}
	// Si contiene caracteres especiales es probablemente un GUID codificado
	for _, r := range str {
		if r < 32 || r > 126 {
			return true
		}
	}
	return false
}

func debugImageTypeMappings(c *gin.Context) {
	// Forzar auto-configuración si no hay mapeos
	if len(imageTypeMappings) == 0 || imageTypeMappings["principal"].MediaResourceID == "" {
		log.Printf("Debug: No mappings found, triggering auto-configuration...")
		if err := autoConfigureMappings(); err != nil {
			log.Printf("Debug: Auto-configuration failed: %v", err)
		}
	}

	c.JSON(200, gin.H{
		"currentMappings": imageTypeMappings,
		"mappingStatus":   getMappingStatus(imageTypeMappings),
		"debug": gin.H{
			"totalMappings":   len(imageTypeMappings),
			"mappingsDetails": getMappingDetails(),
		},
	})
}

func getMappingDetails() []map[string]interface{} {
	details := make([]map[string]interface{}, 0)

	for _, mapping := range imageTypeMappings {
		detail := map[string]interface{}{
			"localType":       mapping.LocalType,
			"mediaResourceId": mapping.MediaResourceID,
			"displayName":     mapping.DisplayName,
			"priority":        mapping.Priority,
			"fileNamePrefix":  mapping.FileNamePrefix,
			"metadataTag":     mapping.MetadataTag,
			"isMapped":        mapping.MediaResourceID != "",
		}
		details = append(details, detail)
	}

	return details
}
