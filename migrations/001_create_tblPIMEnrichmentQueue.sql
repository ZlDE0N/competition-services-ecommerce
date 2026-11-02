-- Migration: 001_create_tblPIMEnrichmentQueue
-- Purpose: Cola de aprobación para enriquecimientos generados por IA
-- Schema:  catalog (created by 000_create_schema.sql)

IF NOT EXISTS (
    SELECT 1 FROM INFORMATION_SCHEMA.TABLES
    WHERE TABLE_SCHEMA = 'catalog' AND TABLE_NAME = 'tblPIMEnrichmentQueue'
)
BEGIN
    CREATE TABLE catalog.tblPIMEnrichmentQueue (
        -- Identificadores
        Id              UNIQUEIDENTIFIER    NOT NULL DEFAULT NEWSEQUENTIALID()  CONSTRAINT PK_PIMEnrichmentQueue PRIMARY KEY,
        ProductId       UNIQUEIDENTIFIER    NOT NULL,
        Sku             NVARCHAR(100)       NOT NULL,
        ProductName     NVARCHAR(500)       NOT NULL,

        -- Datos enriquecidos (JSON completo tal como devuelve /enhance)
        EnrichedData    NVARCHAR(MAX)       NOT NULL,   -- JSON: AIEnhancementResponse
        EnrichmentModel NVARCHAR(100)       NULL,       -- ej. "claude-sonnet-4-6"

        -- Estado del ciclo de vida
        -- pending | approved | rejected
        Status          NVARCHAR(20)        NOT NULL DEFAULT 'pending'
                        CONSTRAINT CHK_PIMEnrichmentQueue_Status CHECK (Status IN ('pending','approved','rejected')),

        -- Auditoría de creación
        CreatedAt       DATETIME2(0)        NOT NULL DEFAULT SYSUTCDATETIME(),
        CreatedBy       NVARCHAR(200)       NULL,   -- email del usuario que disparó el enriquecimiento

        -- Auditoría de revisión
        ReviewedAt      DATETIME2(0)        NULL,
        ReviewedBy      NVARCHAR(200)       NULL,   -- email del aprobador / rechazador

        -- Motivo de rechazo (opcional)
        RejectionReason NVARCHAR(1000)      NULL,

        -- Para edición previa a aprobación (JSON patch parcial sobre EnrichedData)
        EditedData      NVARCHAR(MAX)       NULL
    );

    -- Índice para la vista principal: cola pendiente ordenada por fecha
    CREATE INDEX IX_PIMEnrichmentQueue_Status_CreatedAt
        ON catalog.tblPIMEnrichmentQueue (Status, CreatedAt DESC);

    -- Índice para buscar rápido por producto
    CREATE INDEX IX_PIMEnrichmentQueue_ProductId
        ON catalog.tblPIMEnrichmentQueue (ProductId);

    PRINT 'Table catalog.tblPIMEnrichmentQueue created successfully.';
END
ELSE
BEGIN
    PRINT 'Table catalog.tblPIMEnrichmentQueue already exists — skipping.';
END
