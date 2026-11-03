package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

type TaxonomyConfig struct {
	Id              string    `json:"id"`
	DepartmentId    string    `json:"departmentId"`
	CategoryId      *string   `json:"categoryId"`
	SubCategoryId   *string   `json:"subCategoryId"`
	DepartmentName  string    `json:"departmentName"`
	CategoryName    string    `json:"categoryName"`
	SubCategoryName string    `json:"subCategoryName"`
	Threshold       int       `json:"threshold"`
	DaysBack        int       `json:"daysBack"`
	IsActive        bool      `json:"isActive"`
	CreatedAt       time.Time `json:"createdAt"`
	CreatedBy       string    `json:"createdBy"`
}

type CreateTaxonomyConfigRequest struct {
	DepartmentId    string  `json:"departmentId"  binding:"required"`
	CategoryId      *string `json:"categoryId"`
	SubCategoryId   *string `json:"subCategoryId"`
	DepartmentName  string  `json:"departmentName"`
	CategoryName    string  `json:"categoryName"`
	SubCategoryName string  `json:"subCategoryName"`
	Threshold       int     `json:"threshold"`
	DaysBack        int     `json:"daysBack"`
	CreatedBy       string  `json:"createdBy"`
}

// ---------------------------------------------------------------------------
// GET /enrichment-taxonomy-config
// ---------------------------------------------------------------------------

func getTaxonomyConfigs(c *gin.Context) {
	if db == nil {
		handleError(c, http.StatusServiceUnavailable, "DATABASE_ERROR", "Base de datos no disponible", "")
		return
	}

	rows, err := db.Query(`
		SELECT
			CAST(Id AS NVARCHAR(36)),
			CAST(DepartmentId AS NVARCHAR(36)),
			CAST(CategoryId AS NVARCHAR(36)),
			CAST(SubCategoryId AS NVARCHAR(36)),
			ISNULL(DepartmentName, ''),
			ISNULL(CategoryName, ''),
			ISNULL(SubCategoryName, ''),
			Threshold, DaysBack, IsActive, CreatedAt,
			ISNULL(CreatedBy, '')
		FROM catalog.tblPIMEnrichmentTaxonomyConfig
		ORDER BY CreatedAt DESC
	`)
	if err != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al obtener configuración", err.Error())
		return
	}
	defer rows.Close()

	configs := make([]TaxonomyConfig, 0)
	for rows.Next() {
		var cfg TaxonomyConfig
		var catId, subCatId *string
		if err := rows.Scan(
			&cfg.Id, &cfg.DepartmentId, &catId, &subCatId,
			&cfg.DepartmentName, &cfg.CategoryName, &cfg.SubCategoryName,
			&cfg.Threshold, &cfg.DaysBack, &cfg.IsActive, &cfg.CreatedAt, &cfg.CreatedBy,
		); err != nil {
			log.Printf("⚠️ TaxonomyConfig scan error: %v", err)
			continue
		}
		cfg.CategoryId = catId
		cfg.SubCategoryId = subCatId
		configs = append(configs, cfg)
	}
	c.JSON(http.StatusOK, configs)
}

// ---------------------------------------------------------------------------
// POST /enrichment-taxonomy-config
// ---------------------------------------------------------------------------

func createTaxonomyConfig(c *gin.Context) {
	if db == nil {
		handleError(c, http.StatusServiceUnavailable, "DATABASE_ERROR", "Base de datos no disponible", "")
		return
	}

	var req CreateTaxonomyConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		handleError(c, http.StatusBadRequest, "INVALID_BODY", "Body inválido", err.Error())
		return
	}

	// Auth: use X-User-Email for role check (set by the trusted caller, validated against DB).
	// createdBy from body is only used for audit trail — role check uses the header
	// so a spoofed body field cannot grant access (hasQueueApprovalRole hits the DB).
	authEmail := actorEmail(c)
	if !hasQueueApprovalRole(authEmail) {
		handleError(c, http.StatusForbidden, "FORBIDDEN", "Se requiere rol Products Admin o Super Admin", "")
		return
	}
	// Audit identity is the verified actor, not a name supplied in the body.
	createdBy := authEmail

	// Validate departmentId is a valid UUID
	if _, err := uuid.Parse(req.DepartmentId); err != nil {
		handleError(c, http.StatusBadRequest, "INVALID_DEPT", "DepartmentId inválido", "")
		return
	}

	if req.Threshold <= 0 {
		req.Threshold = 20
	}
	if req.Threshold > 500 {
		req.Threshold = 500
	}
	if req.DaysBack <= 0 {
		req.DaysBack = 7
	}
	if req.DaysBack > 365 {
		req.DaysBack = 365
	}

	id := uuid.New().String()
	_, err := db.Exec(`
		INSERT INTO catalog.tblPIMEnrichmentTaxonomyConfig
			(Id, DepartmentId, CategoryId, SubCategoryId,
			 DepartmentName, CategoryName, SubCategoryName,
			 Threshold, DaysBack, IsActive, CreatedAt, CreatedBy)
		VALUES
			(@p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9, 1, SYSUTCDATETIME(), @p10)
	`, id, req.DepartmentId, req.CategoryId, req.SubCategoryId,
		req.DepartmentName, req.CategoryName, req.SubCategoryName,
		req.Threshold, req.DaysBack, createdBy,
	)
	if err != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al crear configuración", err.Error())
		return
	}

	log.Printf("✅ TaxonomyConfig created id=%s dept=%s by=%s", id, req.DepartmentId, createdBy)
	c.JSON(http.StatusCreated, gin.H{"id": id, "status": "created"})
}

// ---------------------------------------------------------------------------
// DELETE /enrichment-taxonomy-config/:id
// ---------------------------------------------------------------------------

func deleteTaxonomyConfig(c *gin.Context) {
	if db == nil {
		handleError(c, http.StatusServiceUnavailable, "DATABASE_ERROR", "Base de datos no disponible", "")
		return
	}

	editorEmail := strings.TrimSpace(c.GetHeader("X-User-Email"))
	if !hasQueueApprovalRole(editorEmail) {
		handleError(c, http.StatusForbidden, "FORBIDDEN", "Se requiere rol Products Admin o Super Admin", "")
		return
	}

	id := c.Param("id")
	result, err := db.Exec(`
		UPDATE catalog.tblPIMEnrichmentTaxonomyConfig
		SET IsActive = 0
		WHERE CAST(Id AS NVARCHAR(36)) = @p1
	`, id)
	if err != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al desactivar configuración", err.Error())
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		handleError(c, http.StatusNotFound, "NOT_FOUND", "Configuración no encontrada", "")
		return
	}

	log.Printf("✅ TaxonomyConfig deactivated id=%s by=%s", id, editorEmail)
	c.JSON(http.StatusOK, gin.H{"status": "deactivated", "id": id})
}

// ---------------------------------------------------------------------------
// fetchActiveTaxonomyConfigs — used by nightly cron
// ---------------------------------------------------------------------------

func fetchActiveTaxonomyConfigs() ([]TaxonomyConfig, error) {
	rows, err := db.Query(`
		SELECT
			CAST(Id AS NVARCHAR(36)),
			CAST(DepartmentId AS NVARCHAR(36)),
			CAST(CategoryId AS NVARCHAR(36)),
			CAST(SubCategoryId AS NVARCHAR(36)),
			ISNULL(DepartmentName, ''),
			ISNULL(CategoryName, ''),
			ISNULL(SubCategoryName, ''),
			Threshold, DaysBack
		FROM catalog.tblPIMEnrichmentTaxonomyConfig
		WHERE IsActive = 1
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []TaxonomyConfig
	for rows.Next() {
		var cfg TaxonomyConfig
		var catId, subCatId *string
		if err := rows.Scan(
			&cfg.Id, &cfg.DepartmentId, &catId, &subCatId,
			&cfg.DepartmentName, &cfg.CategoryName, &cfg.SubCategoryName,
			&cfg.Threshold, &cfg.DaysBack,
		); err != nil {
			continue
		}
		cfg.CategoryId = catId
		cfg.SubCategoryId = subCatId
		configs = append(configs, cfg)
	}
	return configs, nil
}

// ---------------------------------------------------------------------------
// fetchProductsForNightlyEnrichment — products matching active taxonomy configs
// that don't already have an approved or pending queue entry
// ---------------------------------------------------------------------------

func fetchProductsForNightlyEnrichment(cfg TaxonomyConfig) ([]Product, error) {
	// Build the taxonomy filter: department always required; category/subcategory optional
	catFilter := "AND (tc.CategoryId IS NULL OR tc.CategoryId = lpcs.CategoryId)"
	subCatFilter := "AND (tc.SubCategoryId IS NULL OR tc.SubCategoryId = lpcs.SubCategoryId)"

	query := `
		SELECT DISTINCT TOP ` + fmt.Sprintf("%d", cfg.Threshold) + `
			CAST(p.ProductId AS NVARCHAR(36)) AS ProductId,
			p.Sku,
			p.Name
		FROM catalog.tblproduct p
		JOIN catalog.linkProductCategorySubCategory lpcs ON lpcs.ProductId = p.ProductId
		JOIN catalog.tblPIMEnrichmentTaxonomyConfig tc
			ON tc.DepartmentId = lpcs.DepartmentId
			` + catFilter + `
			` + subCatFilter + `
			AND CAST(tc.Id AS NVARCHAR(36)) = @p1
			AND tc.IsActive = 1
		WHERE p.CreatedDate >= DATEADD(day, -@p2, GETUTCDATE())
		  AND p.IsActive = 'true'
		  AND p.Name IS NOT NULL AND p.Name <> ''
		  AND NOT EXISTS (
			  SELECT 1 FROM catalog.tblPIMEnrichmentQueue q
			  WHERE q.ProductId = p.ProductId AND q.Status IN ('approved', 'pending')
		  )
		ORDER BY p.CreatedDate DESC
	`

	rows, err := db.Query(query, cfg.Id, cfg.DaysBack)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var products []Product
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ProductId, &p.Sku, &p.Name); err != nil {
			continue
		}
		products = append(products, p)
	}
	return products, nil
}
