// luistervink_client.go implements a Luistervink API client for uploading bird detections.
package luistervink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/tphakala/birdnet-go/internal/conf"
	"github.com/tphakala/birdnet-go/internal/datastore"
	"github.com/tphakala/birdnet-go/internal/errors"
	"github.com/tphakala/birdnet-go/internal/logging"
)

// Package-level logger specific to luistervink service
var (
	serviceLogger   *slog.Logger
	serviceLevelVar = new(slog.LevelVar)
	closeLogger     func() error
)

func init() {
	var err error
	// Define log file path relative to working directory
	logFilePath := filepath.Join("logs", "luistervink.log")
	initialLevel := slog.LevelDebug
	serviceLevelVar.Set(initialLevel)

	// Initialize the service-specific file logger
	serviceLogger, closeLogger, err = logging.NewFileLogger(logFilePath, "luistervink", serviceLevelVar)
	if err != nil {
		log.Printf("FATAL: Failed to initialize luistervink file logger at %s: %v. Service logging disabled.", logFilePath, err)
		fbHandler := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: serviceLevelVar})
		serviceLogger = slog.New(fbHandler).With("service", "luistervink")
		closeLogger = func() error { return nil }
	}
}

// DetectionResponse represents the JSON structure of the response from the Luistervink API.
type DetectionResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	ID      string `json:"id,omitempty"`
}

// LvClient holds the configuration for interacting with the Luistervink API.
type LvClient struct {
	Settings   *conf.Settings
	Token      string
	Latitude   float64
	Longitude  float64
	HTTPClient *http.Client
}

// maskURL masks sensitive tokens in URLs for safe logging
func (l *LvClient) maskURL(urlStr string) string {
	if l.Token == "" {
		return urlStr
	}
	return strings.ReplaceAll(urlStr, l.Token, "***")
}

// Interface defines what methods a LuistervinkClient must have
type Interface interface {
	Publish(note *datastore.Note, pcmData []byte) error
	PostDetection(timestamp, commonName, scientificName string, confidence float64) error
	TestConnection(ctx context.Context, resultChan chan<- TestResult)
	Close()
}

// New creates and initializes a new LvClient with the given settings.
func New(settings *conf.Settings) (*LvClient, error) {
	serviceLogger.Info("Creating new Luistervink client")
	client := &LvClient{
		Settings:   settings,
		Token:      settings.Realtime.Luistervink.Token,
		Latitude:   settings.BirdNET.Latitude,
		Longitude:  settings.BirdNET.Longitude,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
	return client, nil
}

// handleNetworkError handles network errors and returns a more specific error message.
func handleNetworkError(err error, url string, timeout time.Duration, operation string) *errors.EnhancedError {
	if err == nil {
		return errors.New(fmt.Errorf("nil error")).
			Component("luistervink").
			Category(errors.CategoryGeneric).
			Build()
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		descriptiveErr := fmt.Errorf("luistervink %s timeout: %w", operation, err)
		serviceLogger.Warn("Network request timed out", "operation", operation, "error", err)
		return errors.New(descriptiveErr).
			Component("luistervink").
			Category(errors.CategoryNetwork).
			NetworkContext(url, timeout).
			Context("error_type", "timeout").
			Context("operation", operation).
			Build()
	}
	var urlErr *neturl.Error
	if errors.As(err, &urlErr) {
		var dnsErr *net.DNSError
		if errors.As(urlErr.Err, &dnsErr) {
			descriptiveErr := fmt.Errorf("luistervink %s DNS resolution failed: %w", operation, err)
			serviceLogger.Error("DNS resolution failed", "operation", operation, "url", url, "error", err)
			return errors.New(descriptiveErr).
				Component("luistervink").
				Category(errors.CategoryNetwork).
				NetworkContext(url, timeout).
				Context("error_type", "dns_resolution").
				Context("operation", operation).
				Build()
		}
	}
	descriptiveErr := fmt.Errorf("luistervink %s network error: %w", operation, err)
	serviceLogger.Error("Network error occurred", "operation", operation, "error", err)
	return errors.New(descriptiveErr).
		Component("luistervink").
		Category(errors.CategoryNetwork).
		NetworkContext(url, timeout).
		Context("error_type", "generic_network").
		Context("operation", operation).
		Build()
}

// handleHTTPResponse processes HTTP response and returns the body or error
func handleHTTPResponse(resp *http.Response, expectedStatus int, operation, maskedURL string) ([]byte, error) {
	if resp.StatusCode != expectedStatus {
		responseBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			serviceLogger.Error("Failed to read response body after non-expected status",
				"operation", operation,
				"url", maskedURL,
				"expected_status", expectedStatus,
				"actual_status", resp.StatusCode,
				"read_error", readErr)
			return nil, fmt.Errorf("%s failed with status %d, failed to read response: %w", operation, resp.StatusCode, readErr)
		}

		err := fmt.Errorf("%s failed with status %d: %s", operation, resp.StatusCode, string(responseBody))
		serviceLogger.Error("Request failed with non-expected status",
			"operation", operation,
			"url", maskedURL,
			"expected_status", expectedStatus,
			"actual_status", resp.StatusCode,
			"response_body", string(responseBody))
		return nil, errors.New(err).
			Component("luistervink").
			Category(errors.CategoryNetwork).
			Context("status_code", resp.StatusCode).
			Context("operation", operation).
			Build()
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		serviceLogger.Error("Failed to read response body",
			"operation", operation,
			"url", maskedURL,
			"status_code", resp.StatusCode,
			"error", err)
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return responseBody, nil
}

// PostDetection posts a detection to the Luistervink API.
func (l *LvClient) PostDetection(timestamp, commonName, scientificName string, confidence float64) (err error) {
	startTime := time.Now()
	defer func() {
		duration := time.Since(startTime)
		if err != nil {
			var enhancedErr *errors.EnhancedError
			if errors.As(err, &enhancedErr) {
				enhancedErr.Context["operation_duration_ms"] = duration.Milliseconds()
				enhancedErr.Context["operation"] = "detection_post"
			} else {
				err = errors.New(err).
					Component("luistervink").
					Category(errors.CategoryNetwork).
					Timing("detection_post", duration).
					Context("timestamp", timestamp).
					Build()
			}
			serviceLogger.Warn("Detection post failed", "duration_ms", duration.Milliseconds(), "error", err)
		} else {
			serviceLogger.Info("Detection post completed", "duration_ms", duration.Milliseconds())
		}
	}()

	serviceLogger.Info("Starting detection post", "timestamp", timestamp, "common_name", commonName, "scientific_name", scientificName, "confidence", confidence)

	// Simple input validation
	if timestamp == "" || commonName == "" || scientificName == "" {
		enhancedErr := errors.New(fmt.Errorf("invalid input: all string parameters must be non-empty")).
			Component("luistervink").
			Category(errors.CategoryValidation).
			Context("timestamp", timestamp).
			Context("common_name", commonName).
			Context("scientific_name", scientificName).
			Build()
		serviceLogger.Error("Detection post failed: Invalid input", "timestamp", timestamp, "error", enhancedErr)
		return enhancedErr
	}

	// Luistervink API endpoint with token as query parameter
	detectionURL := fmt.Sprintf("https://api.luistervink.nl/api/detections/?token=%s", neturl.QueryEscape(l.Token))
	maskedDetectionURL := l.maskURL(detectionURL)

	// Prepare JSON payload for POST request matching Luistervink API schema
	postData := struct {
		Timestamp      string  `json:"timestamp"`
		Latitude       float64 `json:"lat"`
		Longitude      float64 `json:"lon"`
		CommonName     string  `json:"commonName,omitempty"`
		ScientificName string  `json:"scientificName"`
		Confidence     float64 `json:"confidence"`
		SoundscapeID   string  `json:"soundscapeId"`
	}{
		Timestamp:      timestamp,
		Latitude:       l.Latitude,
		Longitude:      l.Longitude,
		CommonName:     commonName,
		ScientificName: scientificName,
		Confidence:     confidence,
		SoundscapeID:   fmt.Sprintf("birdnet-go-%d", time.Now().Unix()),
	}

	postDataBytes, err := json.Marshal(postData)
	if err != nil {
		serviceLogger.Error("Failed to marshal detection JSON data", "error", err)
		return fmt.Errorf("failed to marshal JSON data: %w", err)
	}

	if l.Settings.Realtime.Luistervink.Debug {
		serviceLogger.Debug("Detection JSON Payload", "payload", string(postDataBytes))
	}

	serviceLogger.Info("Posting detection", "url", maskedDetectionURL, "scientific_name", scientificName)
	resp, err := l.HTTPClient.Post(detectionURL, "application/json", bytes.NewBuffer(postDataBytes))
	if err != nil {
		serviceLogger.Error("Detection post request failed", "url", maskedDetectionURL, "error", err)
		return handleNetworkError(err, maskedDetectionURL, 30*time.Second, "detection post")
	}
	if resp == nil {
		serviceLogger.Error("Detection post received nil response", "url", maskedDetectionURL)
		return fmt.Errorf("received nil response")
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			serviceLogger.Debug("Failed to close response body", "error", err)
		}
	}()

	serviceLogger.Debug("Received detection post response", "url", maskedDetectionURL, "status_code", resp.StatusCode)

	_, err = handleHTTPResponse(resp, http.StatusCreated, "detection post", maskedDetectionURL)
	if err != nil {
		var enhancedErr *errors.EnhancedError
		if errors.As(err, &enhancedErr) {
			enhancedErr.Context["scientific_name"] = scientificName
		}
		return err
	}

	serviceLogger.Info("Detection posted successfully", "scientific_name", scientificName)
	return nil
}

// Publish function handles the uploading of detected clips and their details to Luistervink.
func (l *LvClient) Publish(note *datastore.Note, pcmData []byte) (err error) {
	startTime := time.Now()
	defer func() {
		duration := time.Since(startTime)
		if err != nil {
			var enhancedErr *errors.EnhancedError
			if errors.As(err, &enhancedErr) {
				enhancedErr.Context["operation_duration_ms"] = duration.Milliseconds()
				enhancedErr.Context["operation"] = "publish"
			} else {
				err = errors.New(err).
					Component("luistervink").
					Category(errors.CategoryNetwork).
					Timing("publish", duration).
					Context("common_name", note.CommonName).
					Context("scientific_name", note.ScientificName).
					Build()
			}
			serviceLogger.Warn("Publish failed", "common_name", note.CommonName, "scientific_name", note.ScientificName, "duration_ms", duration.Milliseconds(), "error", err)
		} else {
			serviceLogger.Info("Publish completed", "common_name", note.CommonName, "scientific_name", note.ScientificName, "duration_ms", duration.Milliseconds())
		}
	}()

	serviceLogger.Info("Starting publish process", "date", note.Date, "time", note.Time, "common_name", note.CommonName, "scientific_name", note.ScientificName, "confidence", note.Confidence)

	// Use system's local timezone for timestamp parsing
	loc := time.Local

	// Combine date and time from note to form a full timestamp string
	dateTimeString := fmt.Sprintf("%sT%s", note.Date, note.Time)

	// Parse the timestamp using the given format and the system's local timezone
	parsedTime, err := time.ParseInLocation("2006-01-02T15:04:05", dateTimeString, loc)
	if err != nil {
		serviceLogger.Error("Error parsing date/time for publish", "date", note.Date, "time", note.Time, "error", err)
		return fmt.Errorf("error parsing date: %w", err)
	}

	// Format the parsed time to the required timestamp format with timezone information
	timestamp := parsedTime.Format("2006-01-02T15:04:05.000-0700")
	serviceLogger.Debug("Formatted timestamp for publish", "timestamp", timestamp)

	// Post the detection details to Luistervink
	serviceLogger.Debug("Calling PostDetection", "timestamp", timestamp, "note", note)
	err = l.PostDetection(timestamp, note.CommonName, note.ScientificName, note.Confidence)
	if err != nil {
		serviceLogger.Error("Publish failed: Error during detection post", "timestamp", timestamp, "note", note, "error", err)
		return fmt.Errorf("failed to post detection to Luistervink: %w", err)
	}
	serviceLogger.Debug("PostDetection completed")

	serviceLogger.Info("Publish process completed successfully", "scientific_name", note.ScientificName)
	return nil
}

// Close properly cleans up the LvClient resources
func (l *LvClient) Close() {
	serviceLogger.Info("Closing Luistervink client")
	if l.HTTPClient != nil && l.HTTPClient.Transport != nil {
		type transporter interface {
			CloseIdleConnections()
		}
		if transport, ok := l.HTTPClient.Transport.(transporter); ok {
			serviceLogger.Debug("Closing idle HTTP connections")
			transport.CloseIdleConnections()
		}
		l.HTTPClient = nil
	}

	if closeLogger != nil {
		serviceLogger.Debug("Closing luistervink service log file")
		if err := closeLogger(); err != nil {
			log.Printf("ERROR: Failed to close luistervink log file: %v", err)
		}
		closeLogger = nil
	}

	if l.Settings.Realtime.Luistervink.Debug {
		serviceLogger.Info("Luistervink client closed")
	}
}
