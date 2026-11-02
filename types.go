package main

import "time"

// Brand representa una marca del sistema
type Brand struct {
	BrandId   string `json:"BrandId"`
	BrandName string `json:"BrandName"`
}

type Product struct {
	ProductId                                    string     `json:"ProductId"`
	FamilyId                                     *string    `json:"FamilyId"`
	Sku                                          string     `json:"Sku"`
	SystemCode                                   string     `json:"SystemCode"`
	Name                                         string     `json:"Name"`
	ShortDescription                             *string    `json:"ShortDescription"`
	FullDescription                              *string    `json:"FullDescription"`
	AdminComment                                 *string    `json:"AdminComment"`
	SegmentId                                    *string    `json:"SegmentId"`
	BrandId                                      string     `json:"BrandId"`
	ManufacturerId                               *string    `json:"ManufacturerId"`
	TaxCategoryId                                string     `json:"TaxCategoryId"`
	AllowCustomerReviews                         *bool      `json:"AllowCustomerReviews"`
	ApprovedRatingSum                            *int       `json:"ApprovedRatingSum"`
	NotApprovedRatingSum                         *int       `json:"NotApprovedRatingSum"`
	ApprovedTotalReviews                         *int       `json:"ApprovedTotalReviews"`
	NotApprovedTotalReviews                      *int       `json:"NotApprovedTotalReviews"`
	SubjectToAcl                                 *bool      `json:"SubjectToAcl"`
	ManufacturerPartNumber                       *string    `json:"ManufacturerPartNumber"`
	Gtin                                         *string    `json:"Gtin"`
	IsGiftCard                                   *bool      `json:"IsGiftCard"`
	IsTaxExempt                                  *bool      `json:"IsTaxExempt"`
	OrderMinimumQuantity                         *int       `json:"OrderMinimumQuantity"`
	OrderMaximumQuantity                         *int       `json:"OrderMaximumQuantity"`
	AllowAddingOnlyExistingAttributeCombinations *bool      `json:"AllowAddingOnlyExistingAttributeCombinations"`
	NotReturnable                                *bool      `json:"NotReturnable"`
	AvailableForPreOrder                         *bool      `json:"AvailableForPreOrder"`
	PreOrderAvailabilityStartDateTimeUtc         *time.Time `json:"PreOrderAvailabilityStartDateTimeUtc"`
	Price                                        float64    `json:"Price"`
	MarkAsNew                                    *bool      `json:"MarkAsNew"`
	MarkAsNewStartDateTimeUtc                    *time.Time `json:"MarkAsNewStartDateTimeUtc"`
	MarkAsNewEndDateTimeUtc                      *time.Time `json:"MarkAsNewEndDateTimeUtc"`
	HasTierPrices                                *bool      `json:"HasTierPrices"`
	HasDiscountsApplied                          *bool      `json:"HasDiscountsApplied"`
	Weight                                       *float64   `json:"Weight"`
	Length                                       *float64   `json:"Length"`
	Width                                        *float64   `json:"Width"`
	Height                                       *float64   `json:"Height"`
	AvailableStartDateTimeUtc                    *time.Time `json:"AvailableStartDateTimeUtc"`
	AvailableEndDateTimeUtc                      *time.Time `json:"AvailableEndDateTimeUtc"`
	DisplayOrder                                 *int       `json:"DisplayOrder"`
	UoMGroupId                                   *string    `json:"UoMGroupId"`
	AppEnabled                                   string     `json:"AppEnabled"`
	CreatedDate                                  time.Time  `json:"CreatedDate"`
	CreatedBy                                    string     `json:"CreatedBy"`
	LastModifiedDate                             time.Time  `json:"LastModifiedDate"`
	LastModifiedBy                               string     `json:"LastModifiedBy"`
	Status                                       string     `json:"Status"`
	TimeStamp                                    string     `json:"TimeStamp"`
	Tags                                         *string    `json:"Tags"`
	Cost                                         *float64   `json:"Cost"`
	FreightCost                                  *float64   `json:"FreightCost"`
	InsuranceCost                                *float64   `json:"InsuranceCost"`
	CustomsDuties                                *float64   `json:"CustomsDuties"`
	TotalCost                                    *float64   `json:"TotalCost"`
	ProductTypeId                                string     `json:"ProductTypeId"`
	ShortName                                    string     `json:"ShortName"`
	ExternalCodes                                *string    `json:"ExternalCodes"`
	Barcodes                                     *string    `json:"Barcodes"`
	GroupStockQty                                *float64   `json:"GroupStockQty"`
	FulfillmentPlanId                            *string    `json:"FulfillmentPlanId"`
	AppliesForWarranty                           *bool      `json:"AppliesForWarranty"`
	AppliesForExtendedWarranty                   *bool      `json:"AppliesForExtendedWarranty"`
	IsExtendedWarranty                           *bool      `json:"IsExtendedWarranty"`
	IsActive                                     string     `json:"IsActive"`
	CustomerApp                                  string     `json:"CustomerApp"`
	KeyWord                                      *string    `json:"KeyWord"`
	AllowZeroPrice                               *bool      `json:"AllowZeroPrice"`
	IsNonOrderable                               *bool      `json:"IsNonOrderable"`
	AppliesForBonus                              string     `json:"AppliesForBonus"`
	AllowFractionalQty                           *bool      `json:"AllowFractionalQty"`
	OutStockThresholdQty                         *float64   `json:"OutStockThresholdQty"`
	MinProfitMargin                              *float64   `json:"MinProfitMargin"`
	Installable                                  *bool      `json:"Installable"`
	IsInSalesInventory                           *bool      `json:"IsInSalesInventory"`
	UnAStockThresholdQty                         *float64   `json:"UnAStockThresholdQty"`
	LeadTime                                     *int       `json:"LeadTime"`
	InventoryNotRequired                         *bool      `json:"InventoryNotRequired"`
	AttributeFilter                              *string    `json:"AttributeFilter"`
	VisualTags                                   *string    `json:"VisualTags"`
	PIMAttributes                                *string    `json:"PIMAttributes"`
	BrandName                                    string     `json:"BrandName,omitempty"` // Campo adicional para nombre de marca
	CategoryName                                 string     `json:"CategoryName,omitempty"`
	SubCategoryName                              string     `json:"SubCategoryName,omitempty"`
	DepartmentName                               string     `json:"DepartmentName,omitempty"`
	ProviderName                                 string     `json:"ProviderName,omitempty"`
	PrimaryBarcode                               string     `json:"PrimaryBarcode,omitempty"` // Primer barcode de catalog.tblBarcode (preferido DefaultBarcode=1)
	AllBarcodes                                  []string   `json:"-"`                        // Todos los barcodes activos del producto
}

type EnhancedProduct struct {
	Product
	OriginalData        ProductSummary           `json:"originalData"`
	SearchTerms         []string                 `json:"searchTerms"`
	SearchResults       []map[string]interface{} `json:"searchResults"`
	EnhancedDescription string                   `json:"enhancedDescription"`
	Keywords            []string                 `json:"keywords"`
	EnhancedAt          time.Time                `json:"enhancedAt"`
	ProcessSteps        []ProcessStep            `json:"processSteps"`
	TechnicalDocs       *TechnicalDocumentResult `json:"technicalDocs,omitempty"`
	CommercialContent   *CommercialContent       `json:"commercialContent,omitempty"`
}

type AIEnhancementRequest struct {
	ProductId string `json:"productId"`
}

type RuleAppliedInfo struct {
	Level       string `json:"level"` // "subcategory" | "category" | "department" | "general"
	ID          string `json:"id"`    // ID de la regla, vacío si aplica prompt general
	Department  string `json:"department"`
	Category    string `json:"category"`
	SubCategory string `json:"subCategory"`
	Tone        string `json:"tone"`
	Format      string `json:"format"`
}

type AIEnhancementResponse struct {
	ProductId      string             `json:"productId"`
	OriginalData   OriginalDataFormat `json:"originalData"`
	EnhancedData   EnhancedDataFormat `json:"enhancedData"`
	Confidence     float64            `json:"confidence"`
	ProcessingTime int64              `json:"processingTime"`
	RuleApplied    RuleAppliedInfo    `json:"ruleApplied"`
	QueueId        string             `json:"queueId,omitempty"` // set when saved to approval queue
}

type OriginalDataFormat struct {
	Name             string   `json:"name"`
	Sku              string   `json:"sku"`
	ShortDescription string   `json:"shortDescription"`
	FullDescription  string   `json:"fullDescription"`
	Tags             []string `json:"tags"`
	VisualTags       []string `json:"visualTags"`
}

type EnhancedDataFormat struct {
	Name              string             `json:"name"`
	Sku               string             `json:"sku"`
	ShortDescription  string             `json:"shortDescription"`
	FullDescription   string             `json:"fullDescription"`
	Tags              []string           `json:"tags"`
	VisualTags        []string           `json:"visualTags"`
	CommercialContent *CommercialContent `json:"commercialContent,omitempty"`
	TituloSEO         string             `json:"tituloSEO"`
	DescripcionSEO    string             `json:"descripcionSEO"`
}

type ProductSummary struct {
	Name             string `json:"name"`
	Sku              string `json:"sku"`
	ShortDescription string `json:"shortDescription"`
	FullDescription  string `json:"fullDescription"`
}

type ProcessStep struct {
	Step        int       `json:"step"`
	Description string    `json:"description"`
	Timestamp   time.Time `json:"timestamp"`
	Duration    string    `json:"duration"`
}

// ClaudeRequest for simple text messages
type ClaudeRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens"`
}

// Message for simple text content
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ClaudeMultimodalRequest for messages with documents (PDFs, images)
type ClaudeMultimodalRequest struct {
	Model     string              `json:"model"`
	Messages  []MultimodalMessage `json:"messages"`
	MaxTokens int                 `json:"max_tokens"`
}

// MultimodalMessage supports mixed content (text + documents)
type MultimodalMessage struct {
	Role    string              `json:"role"`
	Content []MultimodalContent `json:"content"`
}

// MultimodalContent can be text or a document
type MultimodalContent struct {
	Type   string     `json:"type"`             // "text" or "document"
	Text   string     `json:"text,omitempty"`   // for type="text"
	Source *DocSource `json:"source,omitempty"` // for type="document"
}

// DocSource contains the document data
type DocSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "application/pdf"
	Data      string `json:"data"`       // base64 encoded content
}

type ClaudeResponse struct {
	Content []ContentBlock `json:"content"`
	Usage   *ClaudeUsage   `json:"usage,omitempty"`
}

type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ClaudeUsage muestra info de cache hits para debugging
type ClaudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// ─── Anthropic Prompt Caching types ─────────────────────────────────────────
// Docs: https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching
// Las instrucciones estáticas se marcan con cache_control para que Anthropic
// las cachee en sus servidores por 5 min. Reduce TTFT hasta 85%.

type CachedClaudeRequest struct {
	Model     string               `json:"model"`
	System    []SystemContentBlock `json:"system"`
	Messages  []Message            `json:"messages"`
	MaxTokens int                  `json:"max_tokens"`
}

type SystemContentBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type CacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

type CommercialContent struct {
	ShortDescription string   `json:"shortDescription"`
	LongDescription  string   `json:"longDescription"`
	Title            string   `json:"title"`
	HighlightBullets []string `json:"highlightBullets"`
	DevelopedBullets []string `json:"developedBullets"`
	Attributes       []string `json:"attributes"`
	TituloSEO        string   `json:"tituloSEO"`
	DescripcionSEO   string   `json:"descripcionSEO"`
}

// TechnicalDocument representa un documento técnico asociado a un producto
type TechnicalDocument struct {
	MediaID       string    `json:"media_id"`
	ProductID     string    `json:"product_id"`
	URL           string    `json:"url"`
	MediaType     string    `json:"media_type"`
	ResourceName  string    `json:"resource_name"`
	FileExtension string    `json:"file_extension"`
	Status        int       `json:"status"`
	LastChecked   time.Time `json:"last_checked"`
	IsAvailable   bool      `json:"is_available"`
	ExtractedData string    `json:"extracted_data,omitempty"`
	DataSource    string    `json:"data_source"` // "document", "scraping", "ai"
}

// TechnicalDocumentResult contiene los resultados de la búsqueda de documentos técnicos
type TechnicalDocumentResult struct {
	ProductID      string              `json:"product_id"`
	Documents      []TechnicalDocument `json:"documents"`
	TotalDocuments int                 `json:"total_documents"`
	AvailableCount int                 `json:"available_count"`
	LastChecked    time.Time           `json:"last_checked"`
}

type BraveSearchResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

type ErrorResponse struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
	Details string `json:"details,omitempty"`
}

type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
	Value   string `json:"value,omitempty"`
}

// ImageMetadata estructura para metadata que el cliente necesita para inserción
type ImageMetadata struct {
	MediaId               string `json:"mediaId"`
	MediaResourceId       string `json:"mediaResourceId"`
	MediaCategoryId       string `json:"mediaCategoryId"`
	OriginalFileName      string `json:"originalFileName"`
	ContentType           string `json:"contentType"`
	MetaTitle             string `json:"metaTitle"`
	MetaDescription       string `json:"metaDescription"`
	DefaultProductPicture bool   `json:"defaultProductPicture"`
	Order                 int    `json:"order"`
	IsDecorated           bool   `json:"isDecorated"`
	CompanyId             string `json:"companyId,omitempty"`
}

// ProcessedImageForClient representa una imagen procesada con metadata paral cliente
type ProcessedImageForClient struct {
	OriginalURL    string        `json:"originalUrl"`     // URL original de la imagen
	ImageType      string        `json:"imageType"`       // principal, secundaria, ambiente
	Title          string        `json:"title"`           // Título de la imagen
	Source         string        `json:"source"`          // Dominio origen
	Success        bool          `json:"success"`         // Procesada exitosamente
	Error          string        `json:"error,omitempty"` // Error si falló
	Metadata       ImageMetadata `json:"metadata"`        // Metadata completo para inserción
	ProcessingTime int64         `json:"processingTime"`  // Tiempo de procesamiento en ms
}

// ProcessedImagesResponse respuesta paral cliente con metadata para inserción
type ProcessedImagesResponse struct {
	ProductId       string                    `json:"productId"`
	ProcessedImages []ProcessedImageForClient `json:"processedImages"`
	TotalProcessed  int                       `json:"totalProcessed"`
	TotalSuccess    int                       `json:"totalSuccess"`
	TotalErrors     int                       `json:"totalErrors"`
	ProcessingTime  int64                     `json:"processingTime"`
	Message         string                    `json:"message"`
	NextSteps       []string                  `json:"nextSteps"`
}
