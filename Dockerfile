# Multi-stage build para optimizar el tamaño de la imagen final
FROM golang:1.25-alpine AS builder

# Instalar dependencias del sistema necesarias
RUN apk --no-cache add ca-certificates git

# Establecer directorio de trabajo
WORKDIR /app

# Copiar archivos de dependencias Go
COPY go.mod go.sum ./

# Descargar dependencias
RUN go mod download

# Copiar código fuente
COPY . .

# Compilar la aplicación
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o engine .

# Imagen final ligera
FROM alpine:latest

# Instalar certificados CA para conexiones HTTPS y wget para health check
RUN apk --no-cache add ca-certificates tzdata wget

# Crear usuario no-root para seguridad
RUN addgroup -g 1001 -S appgroup && \
    adduser -u 1001 -S appuser -G appgroup

# Establecer directorio de trabajo
WORKDIR /app

# Copiar el binario compilado desde la etapa builder
COPY --from=builder /app/engine .

# Cambiar ownership del archivo al usuario no-root
RUN chown appuser:appgroup /app/engine

# Cambiar al usuario no-root
USER appuser

# Exponer puerto 8080
EXPOSE 8080

# Configurar health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

# Comando para ejecutar la aplicación
CMD ["./engine"]