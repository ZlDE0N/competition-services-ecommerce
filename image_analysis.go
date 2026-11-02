package main

import (
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type ImageTransparencyAnalyzer struct {
	client *http.Client
}

// QualityLabel categoriza la imagen visualmente para el usuario PIM
type QualityLabel string

const (
	QualityExcelente QualityLabel = "Excelente" // Fondo transparente/blanco >70% — ideal para imagen principal
	QualityBuena     QualityLabel = "Buena"     // Fondo claro 40-70% — buena para secundaria
	QualityRegular   QualityLabel = "Regular"   // Fondo claro 15-40% — puede servir para ambiental
	QualityMala      QualityLabel = "Mala"      // Fondo oscuro/complejo <15% — poco recomendada
)

type TransparencyAnalysis struct {
	TransparencyPercentage    float64      `json:"transparencyPercentage"`
	WhiteBackgroundPercentage float64      `json:"whiteBackgroundPercentage"`
	TotalClearBackground      float64      `json:"totalClearBackground"`
	Priority                  int          `json:"priority"`
	IsMainImageCandidate      bool         `json:"isMainImageCandidate"`
	Algorithm                 string       `json:"algorithm"`
	ImageFormat               string       `json:"imageFormat"`
	ProcessingTime            int64        `json:"processingTime"`
	QualityLabel              QualityLabel `json:"qualityLabel"`    // Excelente/Buena/Regular/Mala
	RecommendedType           string       `json:"recommendedType"` // principal/secundaria/ambiente
}

func NewImageTransparencyAnalyzer() *ImageTransparencyAnalyzer {
	return &ImageTransparencyAnalyzer{
		client: &http.Client{
			Timeout: 8 * time.Second, // Reducido de 30s — imágenes lentas no vale la pena esperar
		},
	}
}

func (a *ImageTransparencyAnalyzer) AnalyzeImage(imageURL string) (*TransparencyAnalysis, error) {
	startTime := time.Now()

	// Descargar la imagen
	img, format, err := a.downloadAndDecodeImage(imageURL)
	if err != nil {
		return nil, fmt.Errorf("error downloading/decoding image: %v", err)
	}

	var analysis TransparencyAnalysis
	analysis.ImageFormat = format

	// Detectar transparencia real (solo en PNG y formatos con canal alpha)
	if format == "png" || hasAlphaChannel(img) {
		analysis.TransparencyPercentage = a.calculateTransparency(img)
		analysis.Algorithm = "transparency"
	}

	// Detectar fondo blanco (siempre)
	analysis.WhiteBackgroundPercentage = a.calculateWhiteBackground(img)

	// Si no se detectó transparencia, usar detección de blanco
	if analysis.TransparencyPercentage == 0 {
		analysis.Algorithm = "white_detection"
	}

	// Calcular score total
	analysis.TotalClearBackground = analysis.TransparencyPercentage + analysis.WhiteBackgroundPercentage

	// Asignar prioridad y candidatura
	analysis.Priority = a.calculatePriority(analysis.TotalClearBackground)
	analysis.IsMainImageCandidate = analysis.Priority <= 3

	// Asignar etiqueta de calidad visible para el usuario PIM
	analysis.QualityLabel, analysis.RecommendedType = assignQualityLabel(analysis.TotalClearBackground, analysis.Priority)

	analysis.ProcessingTime = time.Since(startTime).Milliseconds()

	return &analysis, nil
}

// assignQualityLabel convierte el score numérico en etiqueta legible y tipo recomendado
func assignQualityLabel(totalClear float64, priority int) (QualityLabel, string) {
	switch {
	case totalClear >= 70 || priority == 1:
		return QualityExcelente, "principal"
	case totalClear >= 40 || priority <= 3:
		return QualityBuena, "principal"
	case totalClear >= 15 || priority <= 4:
		return QualityRegular, "secundaria"
	default:
		return QualityMala, "ambiente"
	}
}

func (a *ImageTransparencyAnalyzer) downloadAndDecodeImage(imageURL string) (image.Image, string, error) {
	// Validar URL antes de descargar
	if imageURL == "" {
		return nil, "", fmt.Errorf("empty image URL")
	}

	// Descargar imagen
	resp, err := a.client.Get(imageURL)
	if err != nil {
		log.Printf("Download failed for image analysis: %v", err)
		return nil, "", fmt.Errorf("error downloading image: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("HTTP error %d downloading image for analysis", resp.StatusCode)
		return nil, "", fmt.Errorf("failed to download image, status code: %d", resp.StatusCode)
	}

	// Leer contenido completo
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("error reading image data: %v", err)
	}

	// Detectar formato por Content-Type y URL
	contentType := resp.Header.Get("Content-Type")
	format := detectImageFormat(contentType, imageURL)

	// Decodificar según formato
	reader := strings.NewReader(string(data))

	switch format {
	case "png":
		img, err := png.Decode(reader)
		return img, "png", err
	case "jpeg", "jpg":
		img, err := jpeg.Decode(reader)
		return img, "jpeg", err
	default:
		// Intentar decodificar automáticamente
		reader = strings.NewReader(string(data))
		img, format, err := image.Decode(reader)
		return img, format, err
	}
}

func (a *ImageTransparencyAnalyzer) calculateTransparency(img image.Image) float64 {
	bounds := img.Bounds()
	totalPixels := bounds.Dx() * bounds.Dy()
	transparentPixels := 0
	alphaThreshold := uint32(16384) // ~25% transparencia (65536 * 0.25)

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, alpha := img.At(x, y).RGBA()
			// Si alpha está por debajo del threshold, considerarlo transparente
			if alpha < alphaThreshold {
				transparentPixels++
			}
		}
	}

	return float64(transparentPixels) / float64(totalPixels) * 100
}

func (a *ImageTransparencyAnalyzer) calculateWhiteBackground(img image.Image) float64 {
	bounds := img.Bounds()
	whitePixels := 0

	// Umbrales más flexibles para mejor detección
	whiteThreshold := uint32(50000) // ~190 en 8-bit (más permisivo)
	grayTolerance := uint32(8000)   // Mayor tolerancia para grises claros

	// Muestreo más eficiente para imágenes grandes
	stepSize := 1
	if bounds.Dx() > 200 || bounds.Dy() > 200 {
		stepSize = maxInt(bounds.Dx()/200, bounds.Dy()/200)
		if stepSize < 1 {
			stepSize = 1
		}
	}

	sampledPixels := 0

	for y := bounds.Min.Y; y < bounds.Max.Y; y += stepSize {
		for x := bounds.Min.X; x < bounds.Max.X; x += stepSize {
			r, g, b, _ := img.At(x, y).RGBA()
			sampledPixels++

			// Verificar si es blanco o gris muy claro
			if r >= whiteThreshold && g >= whiteThreshold && b >= whiteThreshold {
				// Verificar que los canales RGB estén balanceados (no colorido)
				maxDiff := max(max(abs(r, g), abs(g, b)), abs(r, b))
				if maxDiff <= grayTolerance {
					whitePixels++
				}
			}
		}
	}

	percentage := float64(whitePixels) / float64(sampledPixels) * 100

	return percentage
}

func (a *ImageTransparencyAnalyzer) calculatePriority(totalClearBackground float64) int {
	// Criterios más flexibles para mejor clasificación
	switch {
	case totalClearBackground >= 50:
		return 1 // PRIORIDAD MÁXIMA - Imagen perfecta
	case totalClearBackground >= 30:
		return 2 // ALTA - Muy buena para principal
	case totalClearBackground >= 15:
		return 3 // MEDIA-ALTA - Candidata principal
	case totalClearBackground >= 8:
		return 4 // MEDIA - Imagen secundaria
	case totalClearBackground >= 3:
		return 5 // BAJA - Imagen ambiente
	default:
		return 6 // MUY BAJA - Solo para galería
	}
}

// Funciones auxiliares
func detectImageFormat(contentType, url string) string {
	if strings.Contains(contentType, "png") {
		return "png"
	}
	if strings.Contains(contentType, "jpeg") || strings.Contains(contentType, "jpg") {
		return "jpeg"
	}

	// Detectar por extensión en URL
	if strings.Contains(strings.ToLower(url), ".png") {
		return "png"
	}
	if strings.Contains(strings.ToLower(url), ".jpg") || strings.Contains(strings.ToLower(url), ".jpeg") {
		return "jpeg"
	}

	return "unknown"
}

func hasAlphaChannel(img image.Image) bool {
	// Verificar si la imagen tiene canal alpha
	switch img.(type) {
	case *image.RGBA, *image.NRGBA:
		return true
	default:
		return false
	}
}

func abs(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

func max(a, b uint32) uint32 {
	if a > b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
