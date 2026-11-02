package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Server-side authorization
// ---------------------------------------------------------------------------

// hasQueueApprovalRole checks dbo.AspNetUsers + AspNetUserRoles against the
// actual DB so the permission cannot be bypassed by spoofing the X-User-Email header.
func hasQueueApprovalRole(email string) bool {
	if db == nil || strings.TrimSpace(email) == "" {
		return false
	}
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*)
		FROM dbo.AspNetUsers u
		JOIN dbo.AspNetUserRoles ur ON ur.UserId = u.Id
		JOIN dbo.AspNetRoles r      ON r.Id      = ur.RoleId
		WHERE u.Email = @p1
		  AND r.Name IN ('Super Admin', 'Products Admin')
	`, email).Scan(&count)
	return err == nil && count > 0
}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

type EnrichmentQueueItem struct {
	Id              string          `json:"id"`
	ProductId       string          `json:"productId"`
	Sku             string          `json:"sku"`
	ProductName     string          `json:"productName"`
	EnrichedData    json.RawMessage `json:"enrichedData"`
	EnrichmentModel string          `json:"enrichmentModel"`
	Status          string          `json:"status"` // pending | approved | rejected
	CreatedAt       time.Time       `json:"createdAt"`
	CreatedBy       string          `json:"createdBy"`
	ReviewedAt      *time.Time      `json:"reviewedAt"`
	ReviewedBy      string          `json:"reviewedBy"`
	RejectionReason string          `json:"rejectionReason"`
	EditedData      json.RawMessage `json:"editedData,omitempty"`
}

type QueueListResponse struct {
	Items      []EnrichmentQueueItem `json:"items"`
	Total      int                   `json:"total"`
	Page       int                   `json:"page"`
	PageSize   int                   `json:"pageSize"`
	TotalPages int                   `json:"totalPages"`
}

type ApproveRequest struct {
	ReviewedBy string `json:"reviewedBy"`
}

type RejectRequest struct {
	ReviewedBy      string `json:"reviewedBy"`
	RejectionReason string `json:"rejectionReason"`
}

type EditQueueItemRequest struct {
	EditedData json.RawMessage `json:"editedData"`
}

// ---------------------------------------------------------------------------
// saveToEnrichmentQueue — called from enhanceProductAI after generating data
// ---------------------------------------------------------------------------

func saveToEnrichmentQueue(productId string, product *Product, response AIEnhancementResponse, createdBy string) (string, error) {
	if db == nil {
		return "", fmt.Errorf("database not available")
	}

	enrichedJSON, err := json.Marshal(response)
	if err != nil {
		return "", fmt.Errorf("marshal enriched data: %w", err)
	}

	sku := product.Sku
	name := product.Name

	// dedupe — si ya existe un item PENDING para este ProductId,
	// actualizarlo en vez de insertar otro. Evita duplicados en la cola
	// cuando se aplica el enriquecimiento varias veces para el mismo producto.
	var existingId string
	dupErr := db.QueryRow(`
		SELECT TOP 1 CAST(Id AS NVARCHAR(36))
		FROM catalog.tblPIMEnrichmentQueue
		WHERE CAST(ProductId AS NVARCHAR(36)) = @p1 AND Status = 'pending'
		ORDER BY CreatedAt DESC
	`, productId).Scan(&existingId)

	if dupErr == nil && existingId != "" {
		_, err = db.Exec(`
			UPDATE catalog.tblPIMEnrichmentQueue
			SET EnrichedData = @p2, EditedData = NULL, ProductName = @p3, Sku = @p4,
			    EnrichmentModel = @p5, CreatedAt = SYSUTCDATETIME(), CreatedBy = @p6
			WHERE CAST(Id AS NVARCHAR(36)) = @p1
		`, existingId, string(enrichedJSON), name, sku, "claude-sonnet-4-6", createdBy)
		if err != nil {
			return "", fmt.Errorf("update existing queue item: %w", err)
		}
		log.Printf("✅ EnrichmentQueue: actualizado pending existente id=%s sku=%s", existingId, sku)
		return existingId, nil
	}

	id := uuid.New().String()
	_, err = db.Exec(`
		INSERT INTO catalog.tblPIMEnrichmentQueue
			(Id, ProductId, Sku, ProductName, EnrichedData, EnrichmentModel, Status, CreatedAt, CreatedBy)
		VALUES
			(@p1, @p2, @p3, @p4, @p5, @p6, 'pending', SYSUTCDATETIME(), @p7)
	`,
		id, productId, sku, name, string(enrichedJSON), "claude-sonnet-4-6", createdBy,
	)
	if err != nil {
		return "", fmt.Errorf("insert queue item: %w", err)
	}

	log.Printf("✅ EnrichmentQueue: guardado id=%s sku=%s", id, sku)

	// Notify approvers asynchronously — non-blocking (in-app + email)
	queueItem := EnrichmentQueueItem{
		Id:          id,
		ProductId:   productId,
		Sku:         sku,
		ProductName: name,
		CreatedAt:   time.Now().UTC(),
		CreatedBy:   createdBy,
	}
	go notifyEnrichmentQueuedInApp(queueItem)
	go notifyEnrichmentQueued(queueItem)

	return id, nil
}

// ---------------------------------------------------------------------------
// POST /enrichment-enqueue
// Body: { "productId": "<id>" }  Header: X-User-Email
// Encola a la cola de aprobación el resultado IA previamente generado
// (cacheado en /products/:id/enhance). El producto NO se modifica hasta
// que un revisor aprueba. Usa dedupe por ProductId.
// ---------------------------------------------------------------------------

func enqueueEnrichment(c *gin.Context) {
	if db == nil {
		handleError(c, http.StatusServiceUnavailable, "DATABASE_ERROR", "Base de datos no disponible", "")
		return
	}

	var req struct {
		ProductId string `json:"productId"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.ProductId) == "" {
		handleError(c, http.StatusBadRequest, "INVALID_BODY", "productId requerido", "")
		return
	}
	productId := strings.TrimSpace(req.ProductId)

	if valErr := validateProductID(productId); valErr != nil {
		handleValidationError(c, valErr)
		return
	}

	// Recuperar el resultado IA ya generado desde caché
	cacheKey := generateCacheKey("enhanced_product", productId)
	var response AIEnhancementResponse
	found, err := getCache(cacheKey, &response)
	if err != nil || !found {
		handleError(c, http.StatusConflict, "NO_ENRICHMENT",
			"No hay datos enriquecidos para aplicar. Genera de nuevo con IA antes de enviar a aprobación.", "")
		return
	}

	product, err := findProductByID(productId)
	if err != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al buscar producto", err.Error())
		return
	}
	if product == nil {
		handleError(c, http.StatusNotFound, "PRODUCT_NOT_FOUND", "Producto no encontrado", "")
		return
	}

	createdBy := c.GetHeader("X-User-Email")
	queueId, qErr := saveToEnrichmentQueue(productId, product, response, createdBy)
	if qErr != nil {
		handleError(c, http.StatusInternalServerError, "QUEUE_ERROR", "Error al encolar para aprobación", qErr.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "queued",
		"queueId": queueId,
		"message": "Enriquecimiento enviado a la cola de aprobación",
	})
}

// ---------------------------------------------------------------------------
// GET /enrichment-queue
// Query params: status (pending|approved|rejected), page (1-based), pageSize
// ---------------------------------------------------------------------------

func getEnrichmentQueue(c *gin.Context) {
	if db == nil {
		handleError(c, http.StatusServiceUnavailable, "DATABASE_ERROR", "Base de datos no disponible", "")
		return
	}

	status := c.DefaultQuery("status", "pending")
	if status != "pending" && status != "approved" && status != "rejected" && status != "all" {
		status = "pending"
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize

	whereClause := "WHERE Status = @p1"
	args := []interface{}{status}
	countArgs := []interface{}{status}

	if status == "all" {
		whereClause = "WHERE 1=1"
		args = []interface{}{}
		countArgs = []interface{}{}
	}

	// Total count
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM catalog.tblPIMEnrichmentQueue %s", whereClause)
	var total int
	var err error
	if status == "all" {
		err = db.QueryRow(countQuery).Scan(&total)
	} else {
		err = db.QueryRow(countQuery, countArgs...).Scan(&total)
	}
	if err != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al contar items", err.Error())
		return
	}

	// Paginated items
	dataQuery := fmt.Sprintf(`
		SELECT
			CAST(Id AS NVARCHAR(36)),
			CAST(ProductId AS NVARCHAR(36)),
			Sku,
			ProductName,
			EnrichedData,
			ISNULL(EnrichmentModel, ''),
			Status,
			CreatedAt,
			ISNULL(CreatedBy, ''),
			ReviewedAt,
			ISNULL(ReviewedBy, ''),
			ISNULL(RejectionReason, ''),
			EditedData
		FROM catalog.tblPIMEnrichmentQueue
		%s
		ORDER BY CreatedAt DESC
		OFFSET %d ROWS FETCH NEXT %d ROWS ONLY
	`, whereClause, offset, pageSize)

	var rows *rowsResult
	if status == "all" {
		rows = queryRows(dataQuery, args...)
	} else {
		rows = queryRows(dataQuery, args...)
	}
	if rows.err != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al obtener items", rows.err.Error())
		return
	}
	defer rows.rows.Close()

	items := make([]EnrichmentQueueItem, 0)
	for rows.rows.Next() {
		var item EnrichmentQueueItem
		var enrichedStr string
		var editedStr *string
		var reviewedAt *time.Time

		if err := rows.rows.Scan(
			&item.Id, &item.ProductId, &item.Sku, &item.ProductName,
			&enrichedStr, &item.EnrichmentModel,
			&item.Status, &item.CreatedAt, &item.CreatedBy,
			&reviewedAt, &item.ReviewedBy, &item.RejectionReason,
			&editedStr,
		); err != nil {
			log.Printf("⚠️ EnrichmentQueue scan error: %v", err)
			continue
		}
		item.EnrichedData = json.RawMessage(enrichedStr)
		item.ReviewedAt = reviewedAt
		if editedStr != nil && *editedStr != "" {
			item.EditedData = json.RawMessage(*editedStr)
		}
		items = append(items, item)
	}

	totalPages := (total + pageSize - 1) / pageSize
	c.JSON(http.StatusOK, QueueListResponse{
		Items:      items,
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages,
	})
}

// ---------------------------------------------------------------------------
// GET /enrichment-queue/:id
// ---------------------------------------------------------------------------

func getEnrichmentQueueItem(c *gin.Context) {
	if db == nil {
		handleError(c, http.StatusServiceUnavailable, "DATABASE_ERROR", "Base de datos no disponible", "")
		return
	}
	id := c.Param("id")
	if id == "" {
		handleError(c, http.StatusBadRequest, "INVALID_ID", "ID requerido", "")
		return
	}

	item, err := fetchQueueItem(id)
	if err != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al obtener item", err.Error())
		return
	}
	if item == nil {
		handleError(c, http.StatusNotFound, "NOT_FOUND", "Item no encontrado", "")
		return
	}
	c.JSON(http.StatusOK, item)
}

// ---------------------------------------------------------------------------
// POST /enrichment-queue/:id/approve
// ---------------------------------------------------------------------------

func approveEnrichmentQueueItem(c *gin.Context) {
	if db == nil {
		handleError(c, http.StatusServiceUnavailable, "DATABASE_ERROR", "Base de datos no disponible", "")
		return
	}
	id := c.Param("id")

	var req ApproveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		req.ReviewedBy = ""
	}
	reviewedBy := strings.TrimSpace(req.ReviewedBy)

	// Server-side role check — X-User-Email alone is not trusted
	if !hasQueueApprovalRole(reviewedBy) {
		handleError(c, http.StatusForbidden, "FORBIDDEN", "Se requiere rol Products Admin o Super Admin", "")
		return
	}

	// Fetch the queue item to get ProductId + data to apply
	item, fetchErr := fetchQueueItem(id)
	if fetchErr != nil || item == nil {
		handleError(c, http.StatusNotFound, "NOT_FOUND", "Item no encontrado", "")
		return
	}
	if item.Status != "pending" {
		handleError(c, http.StatusConflict, "CONFLICT", "Item ya fue procesado", "")
		return
	}

	// Prefer EditedData if the approver modified it; fall back to EnrichedData
	dataToApply := item.EnrichedData
	if len(item.EditedData) > 0 && string(item.EditedData) != "null" {
		dataToApply = item.EditedData
	}

	// Parse enriched fields from stored JSON (AIEnhancementResponse shape)
	var enriched struct {
		EnhancedData struct {
			Name             string `json:"name"`
			ShortDescription string `json:"shortDescription"`
			FullDescription  string `json:"fullDescription"`
		} `json:"enhancedData"`
	}
	if parseErr := json.Unmarshal(dataToApply, &enriched); parseErr != nil {
		handleError(c, http.StatusInternalServerError, "PARSE_ERROR", "Error al parsear datos enriquecidos", parseErr.Error())
		return
	}

	// Apply within a transaction: update product + mark queue approved atomically
	tx, txErr := db.Begin()
	if txErr != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al iniciar transacción", txErr.Error())
		return
	}
	var approveCommitOk bool
	defer func() {
		if !approveCommitOk {
			tx.Rollback()
		}
	}()

	// Update the real product — only fields the AI generates today.
	// Fields not yet generated by AI (barcode, brand, supplier, part number)
	// will be added when the AI enrichment is expanded to produce them.
	// Note: LastModifiedBy is uniqueidentifier — not setting it here since we
	// only have the email string; the DB audit trail is in tblPIMEnrichmentQueue.
	// LastModifiedDate is computed here (Panama local, UTC-5, no DST) instead
	// of GETDATE()/SYSUTCDATETIME() — Azure SQL Database always runs in UTC
	// regardless of which SQL function is used, but the portal renders this
	// column with no timezone info, so it must receive an already-local value
	// (matching how the .NET portal API writes it on manual edits).
	panamaNow := time.Now().UTC().Add(-5 * time.Hour)
	var err error
	_, err = tx.Exec(`
		UPDATE catalog.tblproduct
		SET
			Name             = @p2,
			ShortDescription = @p3,
			FullDescription  = @p4,
			LastModifiedDate = @p5
		WHERE CAST(ProductId AS NVARCHAR(36)) = @p1
	`, item.ProductId,
		enriched.EnhancedData.Name,
		enriched.EnhancedData.ShortDescription,
		enriched.EnhancedData.FullDescription,
		panamaNow,
	)
	if err != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al actualizar producto", err.Error())
		return
	}

	// Mark queue item as approved
	_, err = tx.Exec(`
		UPDATE catalog.tblPIMEnrichmentQueue
		SET Status = 'approved', ReviewedAt = SYSUTCDATETIME(), ReviewedBy = @p2
		WHERE CAST(Id AS NVARCHAR(36)) = @p1
	`, id, reviewedBy)
	if err != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al actualizar cola", err.Error())
		return
	}

	if commitErr := tx.Commit(); commitErr != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al confirmar transacción", commitErr.Error())
		return
	}
	approveCommitOk = true

	log.Printf("✅ EnrichmentQueue approved id=%s productId=%s by=%s", id, item.ProductId, reviewedBy)
	c.JSON(http.StatusOK, gin.H{"status": "approved", "id": id, "productId": item.ProductId, "reviewedBy": reviewedBy})
}

// ---------------------------------------------------------------------------
// POST /enrichment-queue/:id/reject
// ---------------------------------------------------------------------------

func rejectEnrichmentQueueItem(c *gin.Context) {
	if db == nil {
		handleError(c, http.StatusServiceUnavailable, "DATABASE_ERROR", "Base de datos no disponible", "")
		return
	}
	id := c.Param("id")

	var req RejectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		handleError(c, http.StatusBadRequest, "INVALID_BODY", "Body inválido", err.Error())
		return
	}

	// Server-side role check
	if !hasQueueApprovalRole(req.ReviewedBy) {
		handleError(c, http.StatusForbidden, "FORBIDDEN", "Se requiere rol Products Admin o Super Admin", "")
		return
	}

	// Fetch item before rejecting — need ProductId, Sku, etc. for re-enqueue
	item, fetchErr := fetchQueueItem(id)
	if fetchErr != nil || item == nil {
		handleError(c, http.StatusNotFound, "NOT_FOUND", "Item no encontrado", "")
		return
	}
	if item.Status != "pending" {
		handleError(c, http.StatusConflict, "CONFLICT", "Item ya fue procesado", "")
		return
	}

	tx, txErr := db.Begin()
	if txErr != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al iniciar transacción", txErr.Error())
		return
	}
	var commitOk bool
	defer func() {
		if !commitOk {
			tx.Rollback()
		}
	}()

	// Mark as rejected
	result, err := tx.Exec(`
		UPDATE catalog.tblPIMEnrichmentQueue
		SET Status = 'rejected', ReviewedAt = SYSUTCDATETIME(), ReviewedBy = @p2, RejectionReason = @p3
		WHERE CAST(Id AS NVARCHAR(36)) = @p1 AND Status = 'pending'
	`, id, req.ReviewedBy, req.RejectionReason)
	if err != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al rechazar", err.Error())
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		handleError(c, http.StatusConflict, "CONFLICT", "Item no encontrado o ya fue procesado", "")
		return
	}

	if commitErr := tx.Commit(); commitErr != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al confirmar transacción", commitErr.Error())
		return
	}
	commitOk = true

	// el rechazo NO re-encola. El item pasa a 'rejected' y desaparece
	// de la pestaña Pendientes. Antes se insertaba un nuevo item 'pending' con
	// CreatedBy='system-requeue', lo que generaba duplicado visual, alteraba el
	// "Creado por" y mantenía el badge sin descontar. Si se requiere reprocesar
	// un producto rechazado, se genera de nuevo desde el detalle del producto.

	log.Printf("✅ EnrichmentQueue rejected id=%s by=%s", id, req.ReviewedBy)
	c.JSON(http.StatusOK, gin.H{"status": "rejected", "id": id})
}

// ---------------------------------------------------------------------------
// PATCH /enrichment-queue/:id — edit enriched data before approving
// ---------------------------------------------------------------------------

func editEnrichmentQueueItem(c *gin.Context) {
	if db == nil {
		handleError(c, http.StatusServiceUnavailable, "DATABASE_ERROR", "Base de datos no disponible", "")
		return
	}
	id := c.Param("id")

	var req EditQueueItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		handleError(c, http.StatusBadRequest, "INVALID_BODY", "Body inválido", err.Error())
		return
	}

	// Server-side role check — editor must also have approval role
	editorEmail := strings.TrimSpace(c.GetHeader("X-User-Email"))
	if !hasQueueApprovalRole(editorEmail) {
		handleError(c, http.StatusForbidden, "FORBIDDEN", "Se requiere rol Products Admin o Super Admin", "")
		return
	}

	editedStr := string(req.EditedData)
	result, err := db.Exec(`
		UPDATE catalog.tblPIMEnrichmentQueue
		SET EditedData = @p2
		WHERE CAST(Id AS NVARCHAR(36)) = @p1 AND Status = 'pending'
	`, id, editedStr)
	if err != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al editar", err.Error())
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		handleError(c, http.StatusConflict, "CONFLICT", "Item no encontrado o ya fue procesado", "")
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "edited", "id": id})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type rowsResult struct {
	rows interface {
		Next() bool
		Scan(...interface{}) error
		Close() error
	}
	err error
}

func queryRows(query string, args ...interface{}) *rowsResult {
	r, err := db.Query(query, args...)
	return &rowsResult{rows: r, err: err}
}

func fetchQueueItem(id string) (*EnrichmentQueueItem, error) {
	row := db.QueryRow(`
		SELECT
			CAST(Id AS NVARCHAR(36)),
			CAST(ProductId AS NVARCHAR(36)),
			Sku, ProductName, EnrichedData,
			ISNULL(EnrichmentModel, ''),
			Status, CreatedAt, ISNULL(CreatedBy, ''),
			ReviewedAt, ISNULL(ReviewedBy, ''),
			ISNULL(RejectionReason, ''),
			EditedData
		FROM catalog.tblPIMEnrichmentQueue
		WHERE CAST(Id AS NVARCHAR(36)) = @p1
	`, id)

	var item EnrichmentQueueItem
	var enrichedStr string
	var editedStr *string
	var reviewedAt *time.Time

	err := row.Scan(
		&item.Id, &item.ProductId, &item.Sku, &item.ProductName,
		&enrichedStr, &item.EnrichmentModel,
		&item.Status, &item.CreatedAt, &item.CreatedBy,
		&reviewedAt, &item.ReviewedBy, &item.RejectionReason,
		&editedStr,
	)
	if err != nil {
		return nil, nil
	}
	item.EnrichedData = json.RawMessage(enrichedStr)
	item.ReviewedAt = reviewedAt
	if editedStr != nil && *editedStr != "" {
		item.EditedData = json.RawMessage(*editedStr)
	}
	return &item, nil
}
