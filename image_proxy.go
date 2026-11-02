package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// proxyImageHandler maneja el proxy de imágenes externas para resolver problemas de CSP
func proxyImageHandler(c *gin.Context) {
	imageURL := c.Query("url")
	nocache := c.Query("nocache") // Parámetro para bypass del cache

	if imageURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "URL parameter is required"})
		return
	}

	// Validar esquema y host; la IP real se valida al conectar (ver safehttp.go),
	// por lo que también cubre redirecciones y DNS rebinding.
	if _, err := validatePublicURL(imageURL); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid URL"})
		return
	}

	// Validar dominio (comentado para permitir cualquier HTTPS por ahora)
	// Lista de dominios permitidos para mayor seguridad (opcional)
	// allowedDomains := []string{
	//     "images.unsplash.com",
	//     "cdn.shopify.com",
	//     "m.media-amazon.com",
	//     "images.pexels.com",
	//     "pixabay.com",
	//     "static.wixstatic.com",
	//     "img.icons8.com",
	//     // Agregar más dominios según sea necesario
	// }

	// Descomenta la siguiente línea para habilitar la validación de dominios
	// if !isAllowedDomain(imageURL, allowedDomains) {
	//     c.JSON(http.StatusForbidden, gin.H{"error": "Domain not allowed"})
	//     return
	// }

	// Timeout reducido a 6s — imágenes que tardan más no valen la pena esperar
	client := newSafeHTTPClient(6 * time.Second)

	req, err := http.NewRequest("GET", imageURL, nil)
	if err != nil {
		serveTransparentPixel(c)
		return
	}

	// User-Agent de browser real — CDNs de Brave, Amazon, etc. bloquean bots
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "image/webp,image/apng,image/*,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	// Referer necesario para thumbnails de Brave CDN (imgs.search.brave.com)
	if strings.Contains(imageURL, "brave.com") {
		req.Header.Set("Referer", "https://search.brave.com/")
	}

	resp, err := client.Do(req)
	if err != nil {
		// Timeout o red caída → pixel transparente en vez de error
		serveTransparentPixel(c)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		serveTransparentPixel(c)
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "image/") {
		serveTransparentPixel(c)
		return
	}

	// Configurar headers de respuesta para CSP y cache inteligente
	c.Header("Content-Type", contentType)
	c.Header("X-Content-Type-Options", "nosniff")

	// Cache inteligente basado en el resultado y parámetro nocache
	if nocache == "true" {
		// Bypass completo del cache cuando se solicita explícitamente
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Header("Pragma", "no-cache")
		c.Header("Expires", "0")
	} else if resp.StatusCode == http.StatusOK {
		// Cache exitoso por 5 minutos con revalidación
		c.Header("Cache-Control", "public, max-age=300, must-revalidate")
		c.Header("ETag", fmt.Sprintf(`"img-%d"`, time.Now().Unix()/300)) // ETag cada 5 min
	} else {
		// No cachear errores
		c.Header("Cache-Control", "no-cache, no-store")
	}

	// Leer y servir la imagen
	imageData, err := readLimited(resp.Body, 5*1024*1024) // máximo 5MB, medido mientras se lee
	if err != nil {
		if errors.Is(err, errTooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Image too large (max 5MB)"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read image data"})
		return
	}

	c.Data(http.StatusOK, contentType, imageData)
}

// serveTransparentPixel devuelve un PNG 1x1 transparente.
// Se usa cuando el proxy no puede cargar la imagen — evita mostrar el ícono de imagen rota
// y no bloquea la UI con errores JSON inesperados en una etiqueta <img>.
func serveTransparentPixel(c *gin.Context) {
	// PNG 1x1 transparente (67 bytes, formato estándar)
	pixel := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, // PNG signature
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52, // IHDR length + type
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, // 1x1
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, // bit depth, color type (RGBA), CRC
		0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41, // IDAT
		0x54, 0x78, 0x9c, 0x62, 0x00, 0x01, 0x00, 0x00, // compressed pixel data
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, // CRC
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, // IEND
		0x42, 0x60, 0x82, // IEND CRC
	}
	c.Header("Cache-Control", "public, max-age=3600")
	c.Data(http.StatusOK, "image/png", pixel)
}

// isAllowedDomain verifica si una URL pertenece a un dominio permitido
func isAllowedDomain(imageURL string, allowedDomains []string) bool {
	for _, domain := range allowedDomains {
		if strings.Contains(imageURL, domain) {
			return true
		}
	}
	return false
}
