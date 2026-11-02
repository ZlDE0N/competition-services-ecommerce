-- Tabla de taxonomías habilitadas para enriquecimiento automático nocturno
-- Seguro re-ejecutar: IF NOT EXISTS guard.

IF NOT EXISTS (
    SELECT 1 FROM INFORMATION_SCHEMA.TABLES
    WHERE TABLE_SCHEMA = 'catalog' AND TABLE_NAME = 'tblPIMEnrichmentTaxonomyConfig'
)
BEGIN
    CREATE TABLE catalog.tblPIMEnrichmentTaxonomyConfig (
        Id              UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID()  PRIMARY KEY,
        DepartmentId    UNIQUEIDENTIFIER NOT NULL,
        CategoryId      UNIQUEIDENTIFIER NULL,      -- NULL = todos las categorías del departamento
        SubCategoryId   UNIQUEIDENTIFIER NULL,      -- NULL = todas las subcategorías
        DepartmentName  NVARCHAR(200)    NULL,      -- desnormalizado para display
        CategoryName    NVARCHAR(200)    NULL,
        SubCategoryName NVARCHAR(200)    NULL,
        Threshold       INT              NOT NULL DEFAULT 20,  -- máx productos por ejecución
        DaysBack        INT              NOT NULL DEFAULT 7,   -- antigüedad máxima del producto
        IsActive        BIT              NOT NULL DEFAULT 1,
        CreatedAt       DATETIME2        NOT NULL DEFAULT SYSUTCDATETIME(),
        CreatedBy       NVARCHAR(200)    NULL
    );

    CREATE INDEX IX_EnrichmentTaxonomyConfig_Active
        ON catalog.tblPIMEnrichmentTaxonomyConfig (IsActive, DepartmentId);

    PRINT 'tblPIMEnrichmentTaxonomyConfig creada correctamente.';
END
ELSE
BEGIN
    PRINT 'tblPIMEnrichmentTaxonomyConfig ya existe — sin cambios.';
END
