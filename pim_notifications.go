package main

import (
	"database/sql"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func ensureNotificationsTable() {
	if db == nil {
		return
	}
	_, err := db.Exec(`
		IF NOT EXISTS (SELECT * FROM sysobjects WHERE name='tblPIMNotification' AND xtype='U')
		CREATE TABLE catalog.tblPIMNotification (
			Id           NVARCHAR(36)   NOT NULL PRIMARY KEY,
			UserEmail    NVARCHAR(200)  NOT NULL,
			Type         NVARCHAR(50)   NOT NULL,
			Title        NVARCHAR(200)  NOT NULL,
			Body         NVARCHAR(500)  NOT NULL,
			RelatedId    NVARCHAR(36)   NULL,
			IsRead       BIT            NOT NULL DEFAULT 0,
			CreatedAt    DATETIME2      NOT NULL DEFAULT SYSUTCDATETIME()
		)
	`)
	if err != nil {
		log.Printf("⚠️ ensureNotificationsTable: %v", err)
	} else {
		log.Printf("✅ tblPIMNotification ready")
	}
}

func createInAppNotifications(item EnrichmentQueueItem) {
	if db == nil {
		return
	}

	rows, err := db.Query(`
		SELECT u.Email
		FROM dbo.AspNetUsers u
		INNER JOIN dbo.AspNetUserRoles ur ON u.Id = ur.UserId
		INNER JOIN dbo.AspNetRoles r ON ur.RoleId = r.Id
		WHERE r.Name IN ('Products Admin', 'Super Admin')
		  AND u.Email IS NOT NULL
	`)
	if err != nil {
		log.Printf("⚠️ createInAppNotifications: query users error: %v", err)
		return
	}
	defer rows.Close()

	title := "PIM IA — Enriquecimiento pendiente: " + item.Sku
	body := "El producto " + item.ProductName + " fue enriquecido por " + item.CreatedBy + " y requiere aprobación."

	var count int
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil || strings.TrimSpace(email) == "" {
			continue
		}
		id := uuid.New().String()
		_, insertErr := db.Exec(`
			INSERT INTO catalog.tblPIMNotification
				(Id, UserEmail, Type, Title, Body, RelatedId, IsRead, CreatedAt)
			VALUES
				(@p1, @p2, @p3, @p4, @p5, @p6, 0, SYSUTCDATETIME())
		`, id, email, "enrichment_queued", title, body, item.Id)
		if insertErr != nil {
			log.Printf("⚠️ createInAppNotifications: insert error for %s: %v", email, insertErr)
		} else {
			count++
		}
	}
	log.Printf("✅ createInAppNotifications: %d notifications inserted for sku=%s", count, item.Sku)
}

// ---------------------------------------------------------------------------
// GET /notifications
// ---------------------------------------------------------------------------

type NotificationItem struct {
	Id        string    `json:"id"`
	UserEmail string    `json:"userEmail"`
	Type      string    `json:"type"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	RelatedId *string   `json:"relatedId"`
	IsRead    bool      `json:"isRead"`
	CreatedAt time.Time `json:"createdAt"`
}

type NotificationsResponse struct {
	Items []NotificationItem `json:"items"`
	Total int                `json:"total"`
}

func getNotifications(c *gin.Context) {
	if db == nil {
		handleError(c, http.StatusServiceUnavailable, "DATABASE_ERROR", "Base de datos no disponible", "")
		return
	}

	// X-User-Email is client-supplied — this is a known limitation.
	// Real identity binding requires JWT middleware (tracked for next sprint).
	// Query param removed: header-only reduces one attack surface but is NOT
	// a complete IDOR fix.
	userEmail := strings.TrimSpace(c.GetHeader("X-User-Email"))
	if userEmail == "" {
		handleError(c, http.StatusBadRequest, "MISSING_PARAM", "X-User-Email header requerido", "")
		return
	}

	unreadOnly := strings.ToLower(c.Query("unreadOnly")) == "true"

	query := `
		SELECT Id, UserEmail, Type, Title, Body, RelatedId, IsRead, CreatedAt
		FROM catalog.tblPIMNotification
		WHERE LOWER(UserEmail) = LOWER(@p1)
	`
	if unreadOnly {
		query += " AND IsRead = 0"
	}
	query += " ORDER BY CreatedAt DESC"

	rows, err := db.Query(query, userEmail)
	if err != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al obtener notificaciones", err.Error())
		return
	}
	defer rows.Close()

	items := make([]NotificationItem, 0)
	for rows.Next() {
		var n NotificationItem
		var relatedId sql.NullString
		var isReadBit bool
		if err := rows.Scan(&n.Id, &n.UserEmail, &n.Type, &n.Title, &n.Body, &relatedId, &isReadBit, &n.CreatedAt); err != nil {
			log.Printf("⚠️ getNotifications scan error: %v", err)
			continue
		}
		n.IsRead = isReadBit
		if relatedId.Valid {
			n.RelatedId = &relatedId.String
		}
		items = append(items, n)
	}

	c.JSON(http.StatusOK, NotificationsResponse{Items: items, Total: len(items)})
}

// ---------------------------------------------------------------------------
// PATCH /notifications/:id/read
// ---------------------------------------------------------------------------

func markNotificationRead(c *gin.Context) {
	if db == nil {
		handleError(c, http.StatusServiceUnavailable, "DATABASE_ERROR", "Base de datos no disponible", "")
		return
	}

	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		handleError(c, http.StatusBadRequest, "INVALID_ID", "ID requerido", "")
		return
	}

	// X-User-Email is client-supplied — NOT a complete IDOR fix.
	// WHERE clause scopes to header value, but header is spoofable without JWT.
	// Real fix: JWT middleware next sprint.
	callerEmail := strings.TrimSpace(c.GetHeader("X-User-Email"))
	if callerEmail == "" {
		handleError(c, http.StatusBadRequest, "MISSING_AUTH", "X-User-Email requerido", "")
		return
	}

	result, err := db.Exec(`
		UPDATE catalog.tblPIMNotification
		SET IsRead = 1
		WHERE Id = @p1 AND LOWER(UserEmail) = LOWER(@p2)
	`, id, callerEmail)
	if err != nil {
		handleError(c, http.StatusInternalServerError, "DATABASE_ERROR", "Error al marcar notificación", err.Error())
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		handleError(c, http.StatusNotFound, "NOT_FOUND", "Notificación no encontrada", "")
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "read", "id": id})
}
