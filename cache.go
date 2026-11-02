package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// redisEnv returns the environment prefix for Redis keys (e.g. "qa", "prod").
// Reads ENVIRONMENT_REDIS env var; defaults to "default" to avoid cross-env collisions.
func redisEnv() string {
	if env := os.Getenv("ENVIRONMENT_REDIS"); env != "" {
		return env
	}
	return "default"
}

// rk adds the environment prefix to a Redis key so QA and prod never share data.
func rk(key string) string {
	return redisEnv() + ":" + key
}

// Redis cache functions
func setCache(key string, value interface{}, duration time.Duration) error {
	if rdb == nil {
		return nil // Graceful degradation
	}

	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("failed to marshal cache data: %v", err)
	}

	err = rdb.Set(ctx, rk(key), data, duration).Err()
	if err != nil {
		return nil // Don't fail the operation
	}

	return nil
}

func getCache(key string, dest interface{}) (bool, error) {
	if rdb == nil {
		return false, nil // Cache miss when Redis unavailable
	}

	val, err := rdb.Get(ctx, rk(key)).Result()
	if err == redis.Nil {
		return false, nil // Key does not exist
	}
	if err != nil {
		return false, nil // Treat as cache miss
	}

	err = json.Unmarshal([]byte(val), dest)
	if err != nil {
		return false, nil // Treat as cache miss
	}

	return true, nil
}

func generateCacheKey(prefix string, productId string, data ...string) string {
	if prefix == "enhanced_product" {
		// Versión v2 para invalidar caché anterior con lógica de resumen mejorada
		return fmt.Sprintf("PIM:AI:enhanced_product:v2:%s", productId)
	}
	// Fallback para otros casos
	h := sha256.New()
	h.Write([]byte(prefix))
	for _, d := range data {
		h.Write([]byte(d))
	}
	return "PIM:" + prefix + ":" + hex.EncodeToString(h.Sum(nil))[:16]
}

func getCacheStats(c *gin.Context) {
	var status string
	var memoryUsage string

	if rdb == nil {
		status = "disabled"
		memoryUsage = "N/A - Redis not available"
	} else {
		// Obtener información de Redis
		info, err := rdb.Info(ctx, "memory").Result()
		if err != nil {
			status = "error"
			memoryUsage = fmt.Sprintf("error: %v", err)
		} else {
			status = "connected"
			memoryUsage = info
		}
	}

	stats := map[string]interface{}{
		"redis_cache": map[string]interface{}{
			"status":      status,
			"ttl":         "10 minutes",
			"memory_info": memoryUsage,
		},
		"timestamp": time.Now(),
	}

	c.JSON(http.StatusOK, stats)
}
