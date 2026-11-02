# Data model

The engine reads a product catalog and keeps its own queue in SQL Server, in the schema `catalog` (created by `migrations/000_create_schema.sql`).

## Tables this repository creates

| Table | Created by | Holds |
|---|---|---|
| `catalog.tblPIMEnrichmentQueue` | `migrations/001` | Proposals awaiting a decision: `Id`, `ProductId`, `Sku`, `ProductName`, `EnrichedData` (JSON), `EnrichmentModel`, `Status` (`pending`/`approved`/`rejected`), `CreatedAt/By`, `ReviewedAt/By`, `RejectionReason`, `EditedData` (JSON) |
| `catalog.tblPIMEnrichmentTaxonomyConfig` | `migrations/002` | Taxonomies the nightly job covers: department/category/subcategory ids and names, `Threshold` (max products per run, capped at 500), `DaysBack` (max product age, capped at 365), `IsActive` |
| `catalog.tblPIMNotification` | the service, at startup | In-app notifications: `UserEmail`, `Type`, `Title`, `Body`, `RelatedId`, `IsRead` |

## Tables the engine expects to exist

You provide these (they are your catalog). The engine reads them; on approval it updates `Name`, `ShortDescription`, `FullDescription` and `LastModifiedDate` of `catalog.tblproduct`.

| Table | Used for |
|---|---|
| `catalog.tblproduct` | The product (see the `Product` struct in `types.go` for the columns read) |
| `catalog.tblBrand` | Brand name |
| `catalog.tblBarcode` | Barcodes (default first) used for image and part-number lookup |
| `catalog.pltblDepartment`, `pltblCategory`, `pltblSubCategory` | Taxonomy names |
| `catalog.linkProductCategorySubCategory` | Product ↔ taxonomy |
| `catalog.pltblProvider`, `linkProviderProduct` | Supplier name |
| `catalog.tblMedia`, `linkProductMedia`, `pltblMediaResource` | Product media and the media types for images |
| `dbo.AspNetUsers`, `AspNetUserRoles`, `AspNetRoles` | Who may approve: role names `Super Admin` or `Products Admin` |

## Honest limits

- This is **not** yet a database-agnostic engine: the SQL is written for this schema. Bringing your own catalog today means exposing views or tables with these names and columns. A storage adapter (an interface the pipeline calls instead of SQL) is the planned fix and would also remove the dependency on the identity tables for roles.
- The role names are fixed in the query in `queue.go` (`hasQueueApprovalRole`).
- Prompts and their cache live in Redis, not SQL.
