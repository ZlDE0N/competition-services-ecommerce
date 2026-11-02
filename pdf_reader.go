package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// Límite de tamaño de PDF en MB para Claude.
	// Bajado de 32→10 MB: un datasheet escaneado de 10MB ya son ~50-100k tokens
	// de input en Sonnet. PDFs mayores casi siempre son catálogos completos que
	// no aportan datos del SKU y disparan el costo. Override con PDF_MAX_MB.
	defaultMaxPDFSizeForClaudeMB = 10

	// Timeout para lectura de PDF con Claude
	claudePDFTimeout = 120 * time.Second

	// Modelo por defecto para lectura de documentos.
	// Sonnet 4 soporta el document API (Haiku históricamente no). Configurable
	// vía PDF_READER_MODEL para poder A/B-probar un modelo más barato sin redeploy.
	// Sonnet 4: $3/M input, $15/M output.
	defaultClaudeDocumentModel = "claude-sonnet-4-20250514"

	// MaxTokens de salida para extracción de PDF. Bajado de 8000→3000: la
	// extracción de texto de un datasheet rara vez supera 3k tokens; 8k solo
	// infla el techo de costo de salida ($15/M). Override con PDF_MAX_TOKENS.
	defaultPDFMaxTokens = 3000

	// TTL del caché de extracción de PDF. Los datasheets son estáticos, así que
	// el mismo PDF no se reenvía a Claude entre productos/corridas.
	pdfCacheTTL = 720 * time.Hour // 30 días
)

// pdfReaderModel devuelve el modelo configurado para leer PDFs.
func pdfReaderModel() string {
	if m := strings.TrimSpace(os.Getenv("PDF_READER_MODEL")); m != "" {
		return m
	}
	return defaultClaudeDocumentModel
}

// maxPDFSizeMB devuelve el límite de tamaño de PDF (MB) configurado.
func maxPDFSizeMB() float64 {
	if v := strings.TrimSpace(os.Getenv("PDF_MAX_MB")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return defaultMaxPDFSizeForClaudeMB
}

// pdfMaxTokens devuelve el MaxTokens de salida configurado para extracción.
func pdfMaxTokens() int {
	if v := strings.TrimSpace(os.Getenv("PDF_MAX_TOKENS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultPDFMaxTokens
}

// maxDocsPerProduct devuelve cuántos PDFs leer con IA por producto (default 2).
func maxDocsPerProduct() int {
	if v := strings.TrimSpace(os.Getenv("PDF_MAX_DOCS_PER_PRODUCT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 2
}

// PDFReadResult contiene el resultado de la lectura del PDF
type PDFReadResult struct {
	Content       string  `json:"content"`
	PageCount     int     `json:"pageCount,omitempty"`
	Confidence    float64 `json:"confidence,omitempty"`
	ProcessedWith string  `json:"processedWith"` // "claude", "dslipak", "ledongthuc"
}

// readPDFWithClaude lee un PDF usando la API multimodal de Claude
// Funciona tanto con PDFs nativos como escaneados (imágenes)
func readPDFWithClaude(pdfData []byte) (*PDFReadResult, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not configured")
	}

	// Verificar tamaño del PDF
	pdfSizeMB := float64(len(pdfData)) / (1024 * 1024)
	if limit := maxPDFSizeMB(); pdfSizeMB > limit {
		return nil, fmt.Errorf("PDF demasiado grande (%.1f MB). Límite para Claude: %.0f MB", pdfSizeMB, limit)
	}

	// CACHÉ por hash de contenido — los datasheets son estáticos, así que el
	// mismo PDF nunca se reenvía a Claude (ahorro principal de créditos).
	pdfHash := fmt.Sprintf("%x", sha256.Sum256(pdfData))
	cacheKey := generateCacheKey("pdf_extract", pdfHash)
	var cached PDFReadResult
	if found, err := getCache(cacheKey, &cached); err == nil && found && cached.Content != "" {
		log.Printf("⚡ PDF cache hit (%.2f MB ahorrados) hash=%s", pdfSizeMB, pdfHash[:12])
		return &cached, nil
	}

	log.Printf("📄 Enviando PDF a Claude (%.2f MB) modelo=%s", pdfSizeMB, pdfReaderModel())

	// Convertir PDF a base64
	pdfBase64 := base64.StdEncoding.EncodeToString(pdfData)

	// Crear request multimodal
	request := ClaudeMultimodalRequest{
		Model: pdfReaderModel(),
		Messages: []MultimodalMessage{
			{
				Role: "user",
				Content: []MultimodalContent{
					{
						Type: "document",
						Source: &DocSource{
							Type:      "base64",
							MediaType: "application/pdf",
							Data:      pdfBase64,
						},
					},
					{
						Type: "text",
						Text: `Extract ALL text content from this PDF document.

INSTRUCTIONS:
1. Extract all visible text, including:
   - Titles and headers
   - Paragraphs and descriptions
   - Tables (formatted as readable text)
   - Technical specifications
   - Lists and bullet points
   - Model numbers, codes, measurements

2. Preserve the document structure as much as possible
3. For tables, represent them in a readable format
4. Extract technical specifications clearly
5. Do not omit any relevant text

IMPORTANT: The document content is in SPANISH. Keep the extracted text in its ORIGINAL LANGUAGE (Spanish).

RESPONSE FORMAT:
Return ONLY the extracted text, without additional comments or explanations.
If the document is empty or has no readable text, respond: "DOCUMENTO_SIN_TEXTO"`,
					},
				},
			},
		},
		MaxTokens: pdfMaxTokens(),
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("error marshaling request: %v", err)
	}

	// Crear contexto con timeout
	ctx, cancel := context.WithTimeout(context.Background(), claudePDFTimeout)
	defer cancel()

	// Realizar petición con reintentos
	var resp *http.Response
	var lastErr error

	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			log.Printf("🔄 Reintento %d/3 para lectura de PDF con Claude", attempt+1)
			time.Sleep(time.Duration(attempt*2) * time.Second)
		}

		client := &http.Client{Timeout: claudePDFTimeout}
		req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewBuffer(requestBody))
		if err != nil {
			lastErr = fmt.Errorf("error creating request: %v", err)
			continue
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")

		resp, err = client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("error making request: %v", err)
			continue
		}

		if resp.StatusCode == 200 {
			break
		}

		// Leer error body para debugging
		errorBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lastErr = fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(errorBody))

		// No reintentar en errores 4xx (excepto 429)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
			return nil, lastErr
		}
	}

	if resp == nil {
		return nil, fmt.Errorf("todos los reintentos fallaron: %v", lastErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, lastErr
	}

	var claudeResponse ClaudeResponse
	if err := json.NewDecoder(resp.Body).Decode(&claudeResponse); err != nil {
		return nil, fmt.Errorf("error decoding response: %v", err)
	}

	if len(claudeResponse.Content) == 0 {
		return nil, fmt.Errorf("respuesta vacía de Claude")
	}

	extractedText := strings.TrimSpace(claudeResponse.Content[0].Text)

	// Verificar si el documento no tenía texto
	if extractedText == "DOCUMENTO_SIN_TEXTO" || extractedText == "" {
		return nil, fmt.Errorf("el PDF no contiene texto extraíble")
	}

	log.Printf("✅ Claude extrajo %d caracteres del PDF", len(extractedText))

	result := &PDFReadResult{
		Content:       extractedText,
		ProcessedWith: "claude",
		Confidence:    0.95, // Claude tiene alta confianza
	}
	// Persistir en caché por hash — evita reenviar el mismo PDF a Claude.
	if err := setCache(cacheKey, result, pdfCacheTTL); err != nil {
		log.Printf("⚠️ No se pudo cachear extracción de PDF: %v", err)
	}
	return result, nil
}

// readPDFFromURLWithClaude descarga un PDF desde URL y lo lee con Claude
func readPDFFromURLWithClaude(pdfURL string) (*PDFReadResult, error) {
	log.Printf("📥 Descargando PDF para Claude: %s", truncateURL(pdfURL))

	// Descargar PDF
	client := &http.Client{
		Timeout: 60 * time.Second,
	}

	resp, err := client.Get(pdfURL)
	if err != nil {
		return nil, fmt.Errorf("error descargando PDF: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("error HTTP %d descargando PDF", resp.StatusCode)
	}

	// Verificar Content-Type
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "pdf") && !strings.Contains(contentType, "octet-stream") {
		log.Printf("⚠️ Content-Type inesperado: %s (continuando de todos modos)", contentType)
	}

	// Leer contenido
	pdfData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error leyendo PDF: %v", err)
	}

	if len(pdfData) == 0 {
		return nil, fmt.Errorf("PDF vacío")
	}

	return readPDFWithClaude(pdfData)
}

// tryClaudePDFWithTimeout intenta leer un PDF con Claude con timeout
func tryClaudePDFWithTimeout(pdfData []byte, timeout time.Duration) (string, bool) {
	resultChan := make(chan *PDFReadResult, 1)
	errChan := make(chan error, 1)

	go func() {
		result, err := readPDFWithClaude(pdfData)
		if err != nil {
			errChan <- err
			return
		}
		resultChan <- result
	}()

	select {
	case result := <-resultChan:
		return result.Content, true
	case err := <-errChan:
		log.Printf("❌ Error leyendo PDF con Claude: %v", err)
		return "", false
	case <-time.After(timeout):
		log.Printf("⏰ Timeout leyendo PDF con Claude")
		return "", false
	}
}

// isClaudeAvailable verifica si la API de Claude está configurada
func isClaudeAvailable() bool {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	return apiKey != ""
}

// truncateURL acorta una URL para logs
func truncateURL(url string) string {
	if len(url) > 80 {
		return url[:77] + "..."
	}
	return url
}
