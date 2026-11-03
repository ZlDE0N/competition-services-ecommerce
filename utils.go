package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/dslipak/pdf"           // Extracción directa de texto PDF
	oldpdf "github.com/ledongthuc/pdf" // Fallback de extracción directa
	"github.com/unidoc/unioffice/document"
)

// findTechnicalDocuments busca documentos técnicos asociados a un producto
func findTechnicalDocuments(productID string) (*TechnicalDocumentResult, error) {

	query := `
		SELECT
			catalog.tblMedia.MediaId,
			catalog.tblMedia.URL,
			catalog.pltblMediaResource.Name as ResourceName,
			catalog.tblMedia.Status
		FROM
			catalog.tblProduct
			INNER JOIN catalog.linkProductMedia ON catalog.tblProduct.ProductId = catalog.linkProductMedia.ProductId
			INNER JOIN catalog.tblMedia ON catalog.linkProductMedia.MediaId = catalog.tblMedia.MediaId
			INNER JOIN catalog.pltblMediaResource ON catalog.tblMedia.MediaResourceId = catalog.pltblMediaResource.MediaResourceId
		WHERE catalog.tblProduct.ProductId = CAST(@p1 AS UNIQUEIDENTIFIER)
			AND (catalog.tblMedia.URL LIKE '%.pdf' OR catalog.tblMedia.URL LIKE '%.docx')
			AND catalog.tblMedia.Status = 1
			AND catalog.pltblMediaResource.Name IN ('Ficha Tecnica Pequeña', 'Datasheets', 'Ficha Tecnica Grande')
	`

	rows, err := db.Query(query, productID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := &TechnicalDocumentResult{
		ProductID:   productID,
		Documents:   []TechnicalDocument{},
		LastChecked: time.Now(),
	}

	for rows.Next() {
		var doc TechnicalDocument
		var mediaID sql.NullString
		var url sql.NullString
		var resourceName sql.NullString
		var status sql.NullBool

		err := rows.Scan(&mediaID, &url, &resourceName, &status)
		if err != nil {
			continue
		}

		if mediaID.Valid {
			doc.MediaID = mediaID.String
		}
		if url.Valid {
			doc.URL = url.String
			doc.FileExtension = getFileExtension(url.String)
		}
		if resourceName.Valid {
			doc.ResourceName = resourceName.String
		}
		if status.Valid {
			// Convertir bool a int: true = 1, false = 0
			if status.Bool {
				doc.Status = 1
			} else {
				doc.Status = 0
			}
		}

		doc.ProductID = productID
		doc.MediaType = determineMediaType(doc.ResourceName)
		doc.LastChecked = time.Now()
		doc.DataSource = "document"

		// Por ahora solo marcamos como disponible si tiene URL
		// La verificación real de disponibilidad se hará cuando implementes la lectura
		doc.IsAvailable = doc.URL != ""

		result.Documents = append(result.Documents, doc)
	}

	if err = rows.Err(); err != nil {
		return nil, err
	}

	result.TotalDocuments = len(result.Documents)
	result.AvailableCount = countAvailableDocuments(result.Documents)

	return result, nil
}

// getFileExtension extrae la extensión del archivo de una URL
func getFileExtension(url string) string {
	url = strings.ToLower(url)
	if strings.HasSuffix(url, ".pdf") {
		return "pdf"
	} else if strings.HasSuffix(url, ".docx") {
		return "docx"
	} else if strings.HasSuffix(url, ".doc") {
		return "doc"
	}
	return "unknown"
}

// determineMediaType determina el tipo de documento basado en el nombre del recurso
func determineMediaType(resourceName string) string {
	switch resourceName {
	case "Ficha Tecnica Pequeña":
		return "technical_sheet_small"
	case "Ficha Tecnica Grande":
		return "technical_sheet_large"
	case "Datasheets":
		return "datasheet"
	default:
		return "other"
	}
}

// countAvailableDocuments cuenta cuántos documentos están disponibles
func countAvailableDocuments(docs []TechnicalDocument) int {
	count := 0
	for _, doc := range docs {
		if doc.IsAvailable {
			count++
		}
	}
	return count
}

// readPDFFromURL lee el contenido de un archivo PDF usando método híbrido escalonado
func readPDFFromURL(url string) (string, error) {
	log.Printf("🔄 Iniciando extracción híbrida para PDF: %s", truncateString(url, 100))

	// Descargar el archivo PDF — timeout reducido a 10s (era 30s)
	client := newSafeHTTPClient(10 * time.Second)
	resp, err := safeGet(client, url)
	if err != nil {
		return "", fmt.Errorf("error downloading PDF: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP error %d when downloading PDF", resp.StatusCode)
	}

	// Leer el contenido
	content, err := readLimited(resp.Body, maxDocDownloadBytes)
	if err != nil {
		return "", fmt.Errorf("error reading PDF content: %v", err)
	}

	log.Printf("📥 PDF descargado: %d bytes", len(content))

	// MÉTODO 1: dslipak/pdf — 4s timeout
	// Si falla rápido = PDF escaneado → ir directo a Claude, no perder tiempo con ledongthuc
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	if text, success := tryDslipakPDFWithTimeout(ctx, content); success {
		log.Printf("✅ ÉXITO con dslipak/pdf: %d caracteres extraídos", len(text))
		return text, nil
	}

	// NOTA: Se omite ledongthuc — si dslipak falló en un PDF, ledongthuc
	// casi nunca lo resuelve y añade ~8s extra sin beneficio.
	// Ir directo a Claude que es el más capaz para PDFs escaneados.

	// MÉTODO 2: Claude AI para PDFs escaneados — timeout 20s
	if isClaudeAvailable() {
		log.Printf("🤖 Intentando lectura con Claude AI...")
		if text, success := tryClaudePDFWithTimeout(content, 20*time.Second); success {
			log.Printf("✅ ÉXITO con Claude AI: %d caracteres extraídos", len(text))
			return text, nil
		}
	} else {
		log.Printf("⚠️ Claude AI no disponible (ANTHROPIC_API_KEY no configurada)")
	}

	log.Printf("❌ Todos los métodos de extracción fallaron")
	return "", fmt.Errorf("no se pudo extraer texto legible del PDF con ningún método disponible")
}

// tryDslipakPDFWithTimeout intenta extraer texto usando la librería dslipak/pdf con timeout
func tryDslipakPDFWithTimeout(ctx context.Context, content []byte) (string, bool) {
	log.Printf("🔄 Intentando dslipak/pdf...")

	resultChan := make(chan struct {
		text    string
		success bool
	}, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("❌ dslipak/pdf panic: %v", r)
				resultChan <- struct {
					text    string
					success bool
				}{text: "", success: false}
			}
		}()

		text, success := tryDslipakPDF(content)
		resultChan <- struct {
			text    string
			success bool
		}{text: text, success: success}
	}()

	select {
	case result := <-resultChan:
		return result.text, result.success
	case <-ctx.Done():
		log.Printf("⏰ dslipak/pdf timeout tras 8 segundos")
		return "", false
	}
}

// tryDslipakPDF intenta extraer texto usando la librería dslipak/pdf
func tryDslipakPDF(content []byte) (string, bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ dslipak/pdf panic: %v", r)
		}
	}()

	// Crear archivo temporal para dslipak (requiere archivo, no bytes)
	tmpFile, err := ioutil.TempFile("", "pdf_*.pdf")
	if err != nil {
		log.Printf("❌ dslipak/pdf: Error creando archivo temporal: %v", err)
		return "", false
	}
	defer os.Remove(tmpFile.Name())

	// Escribir contenido al archivo temporal
	_, err = tmpFile.Write(content)
	if err != nil {
		tmpFile.Close()
		log.Printf("❌ dslipak/pdf: Error escribiendo archivo temporal: %v", err)
		return "", false
	}
	tmpFile.Close()

	// Abrir PDF
	r, err := pdf.Open(tmpFile.Name())
	if err != nil {
		log.Printf("❌ dslipak/pdf: Error abriendo PDF: %v", err)
		return "", false
	}

	// Extraer texto
	var buf bytes.Buffer
	plainText, err := r.GetPlainText()
	if err != nil {
		log.Printf("❌ dslipak/pdf: Error extrayendo texto: %v", err)
		return "", false
	}

	_, err = buf.ReadFrom(plainText)
	if err != nil {
		log.Printf("❌ dslipak/pdf: Error leyendo texto: %v", err)
		return "", false
	}

	text := buf.String()
	cleanedText := cleanExtractedText(text)

	if isTextMostlyReadable(cleanedText) && len(cleanedText) > 50 {
		return cleanedText, true
	}

	log.Printf("⚠️ dslipak/pdf: Texto extraído no es útil")
	return "", false
}

// tryLedongthucPDFWithTimeout intenta extraer texto usando la librería ledongthuc/pdf con timeout
func tryLedongthucPDFWithTimeout(ctx context.Context, content []byte) (string, bool) {
	log.Printf("🔄 Intentando ledongthuc/pdf...")

	resultChan := make(chan struct {
		text    string
		success bool
	}, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("❌ ledongthuc/pdf panic: %v", r)
				resultChan <- struct {
					text    string
					success bool
				}{text: "", success: false}
			}
		}()

		text, success := tryLedongthucPDF(content)
		resultChan <- struct {
			text    string
			success bool
		}{text: text, success: success}
	}()

	select {
	case result := <-resultChan:
		return result.text, result.success
	case <-ctx.Done():
		log.Printf("⏰ ledongthuc/pdf timeout tras 8 segundos")
		return "", false
	}
}

// tryLedongthucPDF intenta extraer texto usando la librería ledongthuc/pdf (método original)
func tryLedongthucPDF(content []byte) (string, bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ ledongthuc/pdf panic: %v", r)
		}
	}()

	reader := bytes.NewReader(content)

	// Extraer texto del PDF usando la API correcta
	text, err := oldpdf.NewReader(reader, int64(len(content)))
	if err != nil {
		log.Printf("❌ ledongthuc/pdf: Error creando reader: %v", err)
		return "", false
	}

	var extractedText strings.Builder
	totalPages := text.NumPage()

	for pageNum := 1; pageNum <= totalPages; pageNum++ {
		page := text.Page(pageNum)
		if page.V.IsNull() {
			continue
		}

		// Método 1: GetPlainText con fuentes
		fonts := make(map[string]*oldpdf.Font)
		if text1, err := page.GetPlainText(fonts); err == nil {
			pageText := cleanExtractedText(text1)
			if len(pageText) > 20 && isTextMostlyReadable(pageText) {
				extractedText.WriteString(pageText)
				extractedText.WriteString("\n")
			}
		}
	}

	finalText := strings.TrimSpace(extractedText.String())

	if len(finalText) > 50 && isTextMostlyReadable(finalText) {
		return finalText, true
	}

	log.Printf("⚠️ ledongthuc/pdf: Texto extraído no es útil")
	return "", false
}

// cleanExtractedText limpia texto extraído de caracteres de control y binarios
func cleanExtractedText(text string) string {
	// Reemplazar caracteres de control y binarios con espacios
	cleanText := regexp.MustCompile(`[\x00-\x08\x0B\x0C\x0E-\x1F\x7F-\x9F]`).ReplaceAllString(text, " ")

	// Remover caracteres problemáticos comunes en PDFs mal extraídos
	cleanText = regexp.MustCompile(`[^\w\s\.,\-:;()%°ñáéíóúüÑÁÉÍÓÚÜ/]`).ReplaceAllString(cleanText, " ")

	// Normalizar espacios múltiples
	cleanText = regexp.MustCompile(`\s+`).ReplaceAllString(cleanText, " ")

	// Limpiar caracteres Unicode problemáticos
	cleanText = strings.ReplaceAll(cleanText, "�", "")
	cleanText = strings.ReplaceAll(cleanText, "\uFFFD", "")

	// Remover secuencias de caracteres raros repetidos (simplificado)
	cleanText = regexp.MustCompile(`([^\w\s]){3,}`).ReplaceAllString(cleanText, "")

	return strings.TrimSpace(cleanText)
}

// isTextLegible verifica si el texto extraído es legible (no binario corrupto)
func isTextLegible(text string) bool {
	if len(text) == 0 {
		return false
	}

	// Verificar que al menos 70% sean caracteres imprimibles
	printableCount := 0
	totalCount := len(text)

	for _, r := range text {
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			printableCount++
		}
	}

	printableRatio := float64(printableCount) / float64(totalCount)

	// Si menos del 70% son caracteres legibles, considerarlo corrupto
	if printableRatio < 0.7 {
		log.Printf("❌ Texto no legible: solo %.1f%% caracteres imprimibles", printableRatio*100)
		return false
	}

	// Verificar que no sea principalmente caracteres de control
	controlCount := 0
	for _, r := range text {
		if r < 32 && r != '\n' && r != '\r' && r != '\t' {
			controlCount++
		}
	}

	if float64(controlCount)/float64(totalCount) > 0.1 {
		log.Printf("❌ Texto corrupto: demasiados caracteres de control (%d/%d)", controlCount, totalCount)
		return false
	}

	return true
}

// isTextMostlyReadable verifica si el texto es mayoritariamente legible (menos estricto que isTextLegible)
func isTextMostlyReadable(text string) bool {
	if len(text) == 0 {
		return false
	}

	// Verificar que al menos 50% sean caracteres imprimibles (más permisivo)
	printableCount := 0
	totalCount := len(text)

	for _, r := range text {
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			printableCount++
		}
	}

	printableRatio := float64(printableCount) / float64(totalCount)
	return printableRatio >= 0.5
}

// readDocxFromURL lee el contenido de un archivo Word (.docx) desde una URL
func readDocxFromURL(url string) (string, error) {
	// Descargar el archivo DOCX
	resp, err := safeGet(newSafeHTTPClient(30*time.Second), url)
	if err != nil {
		return "", fmt.Errorf("error downloading DOCX file: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to download DOCX file: status %d", resp.StatusCode)
	}

	// Crear un archivo temporal
	tmpFile, err := ioutil.TempFile("", "docx_*.docx")
	if err != nil {
		return "", fmt.Errorf("error creating temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Copiar el contenido descargado al archivo temporal
	_, err = io.Copy(tmpFile, io.LimitReader(resp.Body, maxDocDownloadBytes))
	if err != nil {
		return "", fmt.Errorf("error writing to temp file: %v", err)
	}

	// Cerrar el archivo para poder abrirlo con unioffice
	tmpFile.Close()

	// Abrir el documento DOCX
	doc, err := document.Open(tmpFile.Name())
	if err != nil {
		return "", fmt.Errorf("error opening DOCX file: %v", err)
	}
	defer doc.Close()

	// Extraer todo el texto del documento
	var textContent strings.Builder

	// Obtener todos los párrafos del documento
	for _, para := range doc.Paragraphs() {
		for _, run := range para.Runs() {
			text := run.Text()
			if strings.TrimSpace(text) != "" {
				textContent.WriteString(text)
			}
		}
		textContent.WriteString("\n")
	}

	// Obtener texto de las tablas si las hay
	for _, table := range doc.Tables() {
		for _, row := range table.Rows() {
			for _, cell := range row.Cells() {
				for _, para := range cell.Paragraphs() {
					for _, run := range para.Runs() {
						cellText := run.Text()
						if strings.TrimSpace(cellText) != "" {
							textContent.WriteString(cellText)
							textContent.WriteString(" | ")
						}
					}
				}
			}
			textContent.WriteString("\n")
		}
		textContent.WriteString("\n")
	}

	extractedText := textContent.String()
	if strings.TrimSpace(extractedText) == "" {
		return "", fmt.Errorf("no text content found in DOCX file")
	}

	return extractedText, nil
}

// extractDocumentContent extrae el contenido de un documento técnico
func extractDocumentContent(doc *TechnicalDocument) error {
	if doc.URL == "" {
		return fmt.Errorf("document has no URL")
	}

	var content string
	var err error

	switch doc.FileExtension {
	case "pdf":
		content, err = readPDFFromURL(doc.URL)
	case "docx":
		content, err = readDocxFromURL(doc.URL)
	case "doc":
		// Los archivos .doc son más complejos de leer, por ahora los omitimos
		return fmt.Errorf("DOC format not supported yet, only DOCX")
	default:
		return fmt.Errorf("unsupported file format: %s", doc.FileExtension)
	}

	if err != nil {
		doc.IsAvailable = false
		return fmt.Errorf("error extracting content: %v", err)
	}

	// Limpiar y limitar el contenido extraído
	content = strings.TrimSpace(content)
	if len(content) > 20000 { // Limitar a 20KB de texto para capturar más información técnica
		content = content[:20000] + "... [truncated]"
	}

	doc.ExtractedData = content
	doc.IsAvailable = true
	doc.LastChecked = time.Now()

	return nil
}

// enhanceProductWithTechnicalDocs mejora la información del producto con datos de documentos técnicos
func enhanceProductWithTechnicalDocs(product *Product) (*TechnicalDocumentResult, error) {

	docsResult, err := findTechnicalDocuments(product.ProductId)
	if err != nil {
		return nil, err
	}

	if docsResult.TotalDocuments > 0 {
		// Cap de documentos a leer por producto. Antes se leían TODOS los PDFs
		// (cada uno = una llamada Sonnet-4 con el PDF completo), lo que disparaba
		// el costo en productos con varios datasheets. Los primeros documentos
		// son los más relevantes; el resto rara vez aporta datos del SKU.
		// Override con PDF_MAX_DOCS_PER_PRODUCT.
		maxDocs := maxDocsPerProduct()
		readCount := 0
		for i := range docsResult.Documents {
			if readCount >= maxDocs {
				log.Printf("ℹ️ Cap de %d documentos alcanzado — %d PDFs restantes no se leen con IA", maxDocs, docsResult.TotalDocuments-readCount)
				break
			}
			err := extractDocumentContent(&docsResult.Documents[i])
			if err != nil {
				docsResult.Documents[i].IsAvailable = false
			}
			readCount++
		}

		docsResult.AvailableCount = countAvailableDocuments(docsResult.Documents)
	}

	return docsResult, nil
}
