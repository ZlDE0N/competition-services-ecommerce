package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/google/uuid"
)

type AzureStorageClient struct {
	client        *azblob.Client
	accountName   string
	containerName string
}

type AzureUploadResult struct {
	URL         string `json:"url"`
	FileName    string `json:"fileName"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	Success     bool   `json:"success"`
	Error       string `json:"error,omitempty"`
}

var azureClient *AzureStorageClient

func initAzureStorage() error {
	accountName := os.Getenv("AZURE_STORAGE_ACCOUNT")
	accountKey := os.Getenv("AZURE_STORAGE_KEY")
	containerName := os.Getenv("AZURE_CONTAINER_NAME")

	if accountName == "" || accountKey == "" || containerName == "" {
		return fmt.Errorf("azure storage configuration missing. Required: AZURE_STORAGE_ACCOUNT, AZURE_STORAGE_KEY, AZURE_CONTAINER_NAME")
	}

	// Create Azure Storage client using the new SDK
	serviceURL := fmt.Sprintf("https://%s.blob.core.windows.net/", accountName)

	credential, err := azblob.NewSharedKeyCredential(accountName, accountKey)
	if err != nil {
		return fmt.Errorf("failed to create Azure credentials: %v", err)
	}

	client, err := azblob.NewClientWithSharedKeyCredential(serviceURL, credential, nil)
	if err != nil {
		return fmt.Errorf("failed to create Azure client: %v", err)
	}

	azureClient = &AzureStorageClient{
		client:        client,
		accountName:   accountName,
		containerName: containerName,
	}

	log.Printf("Azure Storage initialized: Account=%s, Container=%s", accountName, containerName)
	return nil
}

func (az *AzureStorageClient) UploadImageFromURL(imageURL, productId, imageType string, metadata map[string]string) (*AzureUploadResult, error) {

	// Download image from URL
	resp, err := safeGet(newSafeHTTPClient(30*time.Second), imageURL)
	if err != nil {
		return &AzureUploadResult{Success: false, Error: fmt.Sprintf("Failed to download image: %v", err)}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &AzureUploadResult{Success: false, Error: fmt.Sprintf("Failed to download image: HTTP %d", resp.StatusCode)}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// Read image data
	imageData, err := readLimited(resp.Body, maxImageDownloadBytes)
	if err != nil {
		return &AzureUploadResult{Success: false, Error: fmt.Sprintf("Failed to read image data: %v", err)}, err
	}

	// Generate filename
	fileName := az.generateFileName(productId, imageType, filepath.Ext(imageURL))

	// Generate blob path
	blobPath := fmt.Sprintf("%s/Product/%s/original/%s",
		os.Getenv("TENANT_ID"), // Assuming tenant ID is available
		productId,
		fileName)

	// Upload to Azure
	return az.uploadBytesToBlob(blobPath, imageData, resp.Header.Get("Content-Type"), metadata)
}

func (az *AzureStorageClient) uploadBytesToBlob(blobPath string, data []byte, contentType string, metadata map[string]string) (*AzureUploadResult, error) {
	ctx := context.Background()

	// Detect content type if not provided
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}

	// Convert metadata to Azure format
	azureMetadata := map[string]*string{}
	for k, v := range metadata {
		value := v
		azureMetadata[k] = &value
	}

	// Upload options
	uploadOptions := &azblob.UploadBufferOptions{
		Metadata: azureMetadata,
	}

	// Upload the blob
	_, err := az.client.UploadBuffer(ctx, az.containerName, blobPath, data, uploadOptions)
	if err != nil {
		return &AzureUploadResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to upload to Azure: %v", err),
		}, err
	}

	// Generate public URL
	publicURL := fmt.Sprintf("https://%s.blob.core.windows.net/%s/%s", az.accountName, az.containerName, blobPath)

	return &AzureUploadResult{
		URL:         publicURL,
		FileName:    filepath.Base(blobPath),
		ContentType: contentType,
		Size:        int64(len(data)),
		Success:     true,
	}, nil
}

func (az *AzureStorageClient) generateFileName(productId, imageType string, extension string) string {
	// Generate random suffix
	randomSuffix := strings.Split(uuid.New().String(), "-")[4] // Last part of UUID

	// Map image type to prefix
	prefix := az.getFilePrefix(imageType)

	// Clean extension
	if extension == "" {
		extension = ".jpg"
	}
	if !strings.HasPrefix(extension, ".") {
		extension = "." + extension
	}

	return fmt.Sprintf("%s_%s_%s_%d_%s%s",
		productId,
		prefix,
		time.Now().Format("2006-01-02T15-04-05-000Z"),
		0, // Order number, can be parameterized
		randomSuffix,
		extension)
}

func (az *AzureStorageClient) getFilePrefix(imageType string) string {
	switch imageType {
	case "principal":
		return "MAIN"
	case "secundaria":
		return "ALT"
	case "ambiente":
		return "ENV"
	default:
		return "IMG"
	}
}

// Cleanup functions for temporary/unused images
func (az *AzureStorageClient) CleanupTempImages(olderThan time.Duration) error {
	// This would implement cleanup logic for temporary images
	// For now, we'll just log that cleanup is needed
	log.Printf("Cleanup routine: Would clean images older than %v", olderThan)
	return nil
}
