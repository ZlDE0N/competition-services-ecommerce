package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// notifyEnrichmentQueued sends an email via SendGrid when a new enrichment
// is saved to the approval queue. Recipients are configured via env vars.
// Non-blocking — errors are only logged, never propagated.
func notifyEnrichmentQueued(item EnrichmentQueueItem) {
	apiKey := os.Getenv("SENDGRID_APIKEY")
	if apiKey == "" {
		log.Printf("⚠️ SENDGRID_APIKEY not set — skipping email notification")
		return
	}

	recipients := buildRecipients()
	if len(recipients) == 0 {
		log.Printf("⚠️ No notification recipients configured — skipping email")
		return
	}

	subject := fmt.Sprintf("🤖 PIM IA — Enriquecimiento pendiente de aprobación: %s", item.Sku)
	htmlBody := buildEmailHTML(item)

	for _, to := range recipients {
		if err := sendEmail(apiKey, to, subject, htmlBody); err != nil {
			log.Printf("⚠️ Email notification failed to %s: %v", to, err)
		} else {
			log.Printf("✅ Notification sent to %s for SKU %s", to, item.Sku)
		}
	}
}

func notifyEnrichmentQueuedInApp(item EnrichmentQueueItem) {
	createInAppNotifications(item)
}

func buildRecipients() []string {
	// ENRICHMENT_NOTIFY_EMAILS=ops@example.com,catalog@example.com
	raw := os.Getenv("ENRICHMENT_NOTIFY_EMAILS")
	if raw == "" {
		return nil
	}
	var out []string
	for _, e := range strings.Split(raw, ",") {
		e = strings.TrimSpace(e)
		if e != "" {
			out = append(out, e)
		}
	}
	return out
}

func sendEmail(apiKey, to, subject, htmlBody string) error {
	payload := map[string]interface{}{
		"personalizations": []map[string]interface{}{
			{"to": []map[string]string{{"email": to}}},
		},
		"from":    map[string]string{"email": envOr("NOTIFY_FROM_EMAIL", "noreply@example.com"), "name": envOr("NOTIFY_FROM_NAME", "Catalog Enrichment")},
		"subject": subject,
		"content": []map[string]string{
			{"type": "text/html", "value": htmlBody},
		},
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", "https://api.sendgrid.com/v3/mail/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("sendgrid status %d", resp.StatusCode)
	}
	return nil
}

// envOr returns the environment variable or a default when it is empty.
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// portalBaseURL devuelve la URL base de la aplicación para armar links en
// correos. Se configura con PORTAL_URL; por defecto, el propio servicio.
func portalBaseURL() string {
	return strings.TrimRight(envOr("PORTAL_URL", "http://localhost:8080"), "/")
}

func buildEmailHTML(item EnrichmentQueueItem) string {
	queueURL := portalBaseURL() + envOr("QUEUE_PATH", "/#queue")

	createdAt := item.CreatedAt.In(time.FixedZone("CST", -6*3600)).Format("02/01/2006 15:04")

	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<body style="font-family:Arial,sans-serif;background:#f4f4f4;padding:20px;">
  <div style="max-width:600px;margin:0 auto;background:#fff;border-radius:8px;padding:30px;box-shadow:0 2px 4px rgba(0,0,0,0.1);">
    <h2 style="color:#324755;margin-top:0;">🤖 PIM IA — Enriquecimiento pendiente de aprobación</h2>
    <p>Se generó un nuevo enriquecimiento con Inteligencia Artificial que requiere tu revisión y aprobación.</p>
    <table style="width:100%%;border-collapse:collapse;margin:20px 0;">
      <tr style="background:#f8f9fa;">
        <td style="padding:10px;font-weight:bold;width:40%%;border:1px solid #dee2e6;">SKU</td>
        <td style="padding:10px;border:1px solid #dee2e6;"><code>%s</code></td>
      </tr>
      <tr>
        <td style="padding:10px;font-weight:bold;border:1px solid #dee2e6;">Producto</td>
        <td style="padding:10px;border:1px solid #dee2e6;">%s</td>
      </tr>
      <tr style="background:#f8f9fa;">
        <td style="padding:10px;font-weight:bold;border:1px solid #dee2e6;">Generado</td>
        <td style="padding:10px;border:1px solid #dee2e6;">%s</td>
      </tr>
      <tr>
        <td style="padding:10px;font-weight:bold;border:1px solid #dee2e6;">Generado por</td>
        <td style="padding:10px;border:1px solid #dee2e6;">%s</td>
      </tr>
    </table>
    <div style="text-align:center;margin:30px 0;">
      <a href="%s" style="background:#28a745;color:#fff;padding:12px 30px;border-radius:5px;text-decoration:none;font-weight:bold;">
        Revisar en el Portal →
      </a>
    </div>
    <hr style="border:none;border-top:1px solid #dee2e6;margin:20px 0;">
    <p style="color:#6c757d;font-size:12px;text-align:center;">
      Solo usuarios con perfil <strong>Products Admin</strong> o <strong>Super Admin</strong> pueden aprobar o rechazar enriquecimientos.
    </p>
  </div>
</body>
</html>`,
		html.EscapeString(item.Sku),
		html.EscapeString(item.ProductName),
		html.EscapeString(createdAt),
		html.EscapeString(item.CreatedBy),
		html.EscapeString(queueURL),
	)
}
