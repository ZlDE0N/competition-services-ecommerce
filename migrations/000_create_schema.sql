-- Migration: 000_create_schema
-- Purpose: schema that holds the catalog and the enrichment tables.
-- Safe to re-run.
IF NOT EXISTS (SELECT 1 FROM sys.schemas WHERE name = 'catalog')
    EXEC('CREATE SCHEMA catalog');
