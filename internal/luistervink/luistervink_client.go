// luistervink_client.go this code implements a Luistervink API client for uploading soundscapes and detections.
package luistervink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tphakala/birdnet-go/internal/conf"
	"github.com/tphakala/birdnet-go/internal/datastore"
	"github.com/tphakala/birdnet-go/internal/errors"
	"github.com/tphakala/birdnet-go/internal/logger"
	"github.com/tphakala/birdnet-go/internal/myaudio"
)

// GetLogger returns the luistervink package logger
func GetLogger() logger.Logger {
	return logger.Global().Module("luistervink")
}

// targetIntegratedLoudnessLUFS defines the target loudness for normalization.
// EBU R128 standard target is -23 LUFS.
const targetIntegratedLoudnessLUFS = -23.0

// HTTP and timeout constants
const (
	// httpClientTimeout is the default timeout for HTTP requests
	httpClientTimeout = 45 * time.Second

	// encodingTimeout is the timeout for audio encoding operations
	encodingTimeout = 30 * time.Second

	// detectionDurationSeconds is the duration added to timestamp for end time
	detectionDurationSeconds = 3
)

// BirdNET algorithm constants
const (
	// birdnetAlgorithmVersion is the BirdNET model version identifier for API submissions
	birdnetAlgorithmVersion = "2p4"
)

// Geographic constants
const (
	// metersPerDegree is the approximate number of meters in one degree of latitude
	metersPerDegree = 111000.0

	// coordinatePrecisionFactor is used to truncate coordinates to 4 decimal places
	coordinatePrecisionFactor = 10000.0

	// randomOffsetMultiplier is used in coordinate randomization
	randomOffsetMultiplier = 2.0

	// randomCenterOffset is used to center random values around zero (-0.5 to +0.5)
	randomCenterOffset = 0.5
)

// HTML/Response preview constants
const (
	// errorSnippetBefore is the number of characters to show before an error pattern
	errorSnippetBefore = 50

	// errorSnippetAfter is the number of characters to show after an error pattern
	errorSnippetAfter = 100

	// maxHTMLPreview is the maximum length of HTML preview in error messages
	maxHTMLPreview = 200

	// maxResponsePreview is the maximum length of response preview in logs
	maxResponsePreview = 500
)

// File permission constants
const (
	// dirPermission is the permission mode for created directories
	dirPermission = 0o750

	// filePermission is the permission mode for created files
	filePermission = 0o600
)

// SoundscapeResponse represents the JSON structure of the response from the Luistervink API when uploading a soundscape.
type SoundscapeResponse struct {
	Success    bool `json:"success"`
	Soundscape struct {
		ID        int     `json:"id"`
		StationID int     `json:"stationId"`
		Timestamp string  `json:"timestamp"`
		URL       *string `json:"url"` // Pointer to handle null
		Filesize  int     `json:"filesize"`
		Extension string  `json:"extension"`
		Duration  float64 `json:"duration"` // Duration in seconds
	} `json:"soundscape"`
}

// LvClient holds the configuration for interacting with the Luistervink API.
type LvClient struct {
	Settings     *conf.Settings
	LuistervinkID string
	Accuracy     float64
	Latitude     float64
	Longitude    float64
	HTTPClient   *http.Client
}

// maskURL masks sensitive LuistervinkID tokens in URLs for safe logging.
// Uses a descriptive marker [LUISTERVINK_ID] for consistency with privacy package patterns.
func (l *LvClient) maskURL(urlStr string) string {
	if l.LuistervinkID == "" {
		return urlStr
	}
	return strings.ReplaceAll(urlStr, l.LuistervinkID, "[LUISTERVINK_ID]")
}

// closeResponseBody safely closes an HTTP response body and logs any errors
// This helper reduces code duplication across HTTP request handlers
func closeResponseBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	if err := resp.Body.Close(); err != nil {
		log := GetLogger()
		log.Debug("Failed to close response body", logger.Error(err))
	}
}

// Interface defines what methods a LuistervinkClient must have
type Interface interface {
	Publish(note *datastore.Note, pcmData []byte) error
	UploadSoundscape(timestamp string, pcmData []byte) (soundscapeID string, err error)
	PostDetection(soundscapeID, timestamp, commonName, scientificName string, confidence float64) error
	TestConnection(ctx context.Context, resultChan chan<- TestResult)
	Close()
}

// New creates and initializes a new LvClient with the given settings.
// The HTTP client is configured with httpClientTimeout to prevent hanging requests.
func New(settings *conf.Settings) (*LvClient, error) {
	log := GetLogger()
	log.Info("Creating new Luistervink client")
	// We expect that Luistervink ID is validated before this function is called
	client := &LvClient{
		Settings:      settings,
		LuistervinkID: settings.Realtime.Luistervink.ID,
		Accuracy:      settings.Realtime.Luistervink.LocationAccuracy,
		Latitude:      settings.BirdNET.Latitude,
		Longitude:     settings.BirdNET.Longitude,
		HTTPClient:    &http.Client{Timeout: httpClientTimeout},
	}
	return client, nil
}

// RandomizeLocation adds a random offset to the given latitude and longitude to fuzz the location
// within a specified radius in meters for privacy, truncating the result to 4 decimal places.
// radiusMeters - the maximum radius in meters to adjust the coordinates
func (l *LvClient) RandomizeLocation(radiusMeters float64) (latitude, longitude float64) {
	log := GetLogger()

	// Create a new local random generator seeded with current Unix time
	rnd := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(time.Now().UnixNano()))) //nolint:gosec // G404: weak randomness acceptable for upload retry jitter, not security-critical

	// Calculate the degree offset using metersPerDegree approximation
	degreeOffset := radiusMeters / metersPerDegree

	// Generate random offsets within +/- degreeOffset
	latOffset := (rnd.Float64() - randomCenterOffset) * randomOffsetMultiplier * degreeOffset
	lonOffset := (rnd.Float64() - randomCenterOffset) * randomOffsetMultiplier * degreeOffset

	// Apply the offsets to the original coordinates and truncate to 4 decimal places
	latitude = math.Floor((l.Latitude+latOffset)*coordinatePrecisionFactor) / coordinatePrecisionFactor
	longitude = math.Floor((l.Longitude+lonOffset)*coordinatePrecisionFactor) / coordinatePrecisionFactor

	log.Debug("Randomized location",
		logger.Float64("original_lat", l.Latitude),
		logger.Float64("original_lon", l.Longitude),
		logger.Float64("radius_meters", radiusMeters),
		logger.Float64("fuzzed_lat", latitude),
		logger.Float64("fuzzed_lon", longitude))

	return latitude, longitude
}

// handleNetworkError handles network errors and returns a more specific error message.
func handleNetworkError(err error, url string, timeout time.Duration, operation string) *errors.EnhancedError {
	log := GetLogger()

	if err == nil {
		return errors.New(fmt.Errorf("nil error")).
			Component("luistervink").
			Category(errors.CategoryGeneric).
			Build()
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		// Create descriptive error message with operation context
		descriptiveErr := fmt.Errorf("Luistervink %s timeout: %w", operation, err)
		log.Warn("Network request timed out",
			logger.String("operation", operation),
			logger.Error(err))
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
			descriptiveErr := fmt.Errorf("Luistervink %s DNS resolution failed: %w", operation, err)
			log.Error("DNS resolution failed",
				logger.String("operation", operation),
				logger.String("url", url),
				logger.Error(err))
			return errors.New(descriptiveErr).
				Component("luistervink").
				Category(errors.CategoryNetwork).
				NetworkContext(url, timeout).
				Context("error_type", "dns_resolution").
				Context("operation", operation).
				Build()
		}
	}
	descriptiveErr := fmt.Errorf("Luistervink %s network error: %w", operation, err)
	log.Error("Network error occurred",
		logger.String("operation", operation),
		logger.Error(err))
	return errors.New(descriptiveErr).
		Component("luistervink").
		Category(errors.CategoryNetwork).
		NetworkContext(url, timeout).
		Context("error_type", "generic_network").
		Context("operation", operation).
		Build()
}

// isHTMLResponse checks if the response content type indicates HTML
func isHTMLResponse(resp *http.Response) bool {
	contentType := resp.Header.Get("Content-Type")
	return strings.Contains(strings.ToLower(contentType), "text/html")
}

// extractHTMLError attempts to extract error message from HTML response
// This handles common error page patterns from web servers and proxies
func extractHTMLError(htmlContent string) string {
	// Common patterns for error messages in HTML
	// Look for title tags first as they often contain the error summary
	titleStart := strings.Index(htmlContent, "<title>")
	titleEnd := strings.Index(htmlContent, "</title>")
	if titleStart != -1 && titleEnd != -1 && titleEnd > titleStart {
		title := htmlContent[titleStart+7 : titleEnd]
		title = strings.TrimSpace(title)
		if title != "" {
			return fmt.Sprintf("HTML error page: %s", title)
		}
	}

	// Look for common error patterns in body
	lowerHTML := strings.ToLower(htmlContent)
	errorPatterns := []string{
		"error",
		"not found",
		"unauthorized",
		"forbidden",
		"bad request",
		"internal server error",
		"service unavailable",
		"gateway timeout",
		"too many requests",
	}

	for _, pattern := range errorPatterns {
		if !strings.Contains(lowerHTML, pattern) {
			continue
		}
		// Try to extract a reasonable snippet around the error
		index := strings.Index(lowerHTML, pattern)
		start := max(index-errorSnippetBefore, 0)
		end := min(index+errorSnippetAfter, len(htmlContent))
		snippet := htmlContent[start:end]
		// Remove HTML tags for cleaner output
		snippet = strings.ReplaceAll(snippet, "<", " <")
		snippet = strings.ReplaceAll(snippet, ">", "> ")
		// Clean up whitespace
		fields := strings.Fields(snippet)
		snippet = strings.Join(fields, " ")
		return fmt.Sprintf("HTML error detected: %s", snippet)
	}

	// If no specific error found, return generic message with beginning of content
	maxLen := min(len(htmlContent), maxHTMLPreview)
	preview := strings.TrimSpace(htmlContent[:maxLen])
	return fmt.Sprintf("Unexpected HTML response (first %d chars): %s", maxLen, preview)
}

// handleHTTPResponse processes HTTP response and handles both JSON and HTML responses
func handleHTTPResponse(resp *http.Response, expectedStatus int, operation, maskedURL string) ([]byte, error) {
	log := GetLogger()

	// Check status code first
	if resp.StatusCode != expectedStatus {
		responseBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			log.Error("Failed to read response body after non-expected status",
				logger.String("operation", operation),
				logger.String("url", maskedURL),
				logger.Int("expected_status", expectedStatus),
				logger.Int("actual_status", resp.StatusCode),
				logger.Error(readErr))
			return nil, fmt.Errorf("%s failed with status %d, failed to read response: %w", operation, resp.StatusCode, readErr)
		}

		// Check if response is HTML
		if isHTMLResponse(resp) {
			htmlError := extractHTMLError(string(responseBody))
			log.Error("Received HTML error response instead of JSON",
				logger.String("operation", operation),
				logger.String("url", maskedURL),
				logger.Int("status_code", resp.StatusCode),
				logger.String("html_error", htmlError),
				logger.String("response_preview", string(responseBody[:min(len(responseBody), maxResponsePreview)])))

			// Determine category based on status code
			category := errors.CategoryNetwork
			if resp.StatusCode == 408 || resp.StatusCode == 504 || resp.StatusCode == 524 {
				// 408 Request Timeout, 504 Gateway Timeout, 524 Timeout (Cloudflare)
				category = errors.CategoryTimeout
			}

			return nil, errors.New(fmt.Errorf("%s failed: %s (status %d)", operation, htmlError, resp.StatusCode)).
				Component("luistervink").
				Category(category).
				Context("response_type", "html").
				Context("status_code", resp.StatusCode).
				Context("operation", operation).
				Build()
		}

		// Not HTML, return the raw response
		err := fmt.Errorf("%s failed with status %d: %s", operation, resp.StatusCode, string(responseBody))
		log.Error("Request failed with non-expected status",
			logger.String("operation", operation),
			logger.String("url", maskedURL),
			logger.Int("expected_status", expectedStatus),
			logger.Int("actual_status", resp.StatusCode),
			logger.String("response_body", string(responseBody)))
		return nil, errors.New(err).
			Component("luistervink").
			Category(errors.CategoryNetwork).
			Context("status_code", resp.StatusCode).
			Context("operation", operation).
			Build()
	}

	// Status is OK, read the body
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error("Failed to read response body",
			logger.String("operation", operation),
			logger.String("url", maskedURL),
			logger.Int("status_code", resp.StatusCode),
			logger.Error(err))
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return responseBody, nil
}

// encodeFlacUsingFFmpeg converts PCM data to FLAC format using FFmpeg directly into a bytes buffer.
// It applies a simple gain adjustment instead of dynamic loudness normalization to avoid pumping effects.
// This avoids writing temporary files to disk.
// It accepts a context for timeout/cancellation control and the explicit path to the FFmpeg executable.
func encodeFlacUsingFFmpeg(ctx context.Context, pcmData []byte, ffmpegPath string, settings *conf.Settings) (*bytes.Buffer, error) {
	log := GetLogger()

	log.Debug("Starting FLAC encoding process")
	// Add check for empty pcmData
	if len(pcmData) == 0 {
		log.Error("FLAC encoding failed: PCM data is empty")
		return nil, fmt.Errorf("pcmData is empty")
	}

	// ffmpegPath is now passed directly
	log.Debug("Using ffmpeg path", logger.String("path", ffmpegPath))

	// --- Pass 1: Analyze Loudness ---
	// Use the provided context for the analysis
	log.Debug("Performing loudness analysis (Pass 1)")
	loudnessStats, err := myaudio.AnalyzeAudioLoudnessWithContext(ctx, pcmData, ffmpegPath)
	if err != nil {
		// Check if the error is due to context cancellation
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			log.Warn("Loudness analysis cancelled or timed out", logger.Error(err))
			return nil, err // Propagate context error
		}

		log.Warn("Loudness analysis (Pass 1) failed, falling back to fixed gain adjustment", logger.Error(err))
		// Fallback to a conservative fixed gain adjustment
		// A fixed gain of 15dB is a reasonable middle ground for bird call recordings
		gainValue := 15.0
		volumeArgs := fmt.Sprintf("volume=%.1fdB", gainValue)
		customArgs := []string{
			"-af", volumeArgs, // Simple gain adjustment
			"-c:a", "flac",
			"-f", "flac",
		}

		// Use the provided context for the fallback export operation
		log.Debug("Starting fallback FLAC export with fixed gain", logger.Float64("gain_db", gainValue))
		buffer, err := myaudio.ExportAudioWithCustomFFmpegArgsContext(ctx, pcmData, ffmpegPath, customArgs)
		if err != nil {
			log.Error("Fallback FLAC export with fixed gain failed",
				logger.Float64("gain_db", gainValue),
				logger.Error(err))
			return nil, fmt.Errorf("fallback FLAC export with fixed gain failed: %w", err)
		}
		log.Info("Encoded PCM to FLAC using fixed gain (fallback)", logger.Float64("gain_db", gainValue))
		return buffer, nil
	}

	log.Debug("Loudness analysis results",
		logger.String("input_i", loudnessStats.InputI),
		logger.String("input_lra", loudnessStats.InputLRA),
		logger.String("input_tp", loudnessStats.InputTP),
		logger.String("input_thresh", loudnessStats.InputThresh))

	// --- Calculate gain needed to reach target loudness ---
	inputLUFS := parseDouble(loudnessStats.InputI, -70.0)
	gainNeeded := targetIntegratedLoudnessLUFS - inputLUFS

	// Apply safety limits to prevent excessive amplification or attenuation
	maxGain := 30.0 // Maximum gain in dB (absolute value)
	gainLimited := false
	if gainNeeded > maxGain {
		log.Warn("Limiting gain to prevent excessive amplification",
			logger.Float64("calculated_gain", gainNeeded),
			logger.Float64("max_gain", maxGain))
		gainNeeded = maxGain
		gainLimited = true
	} else if gainNeeded < -maxGain {
		log.Warn("Limiting gain to prevent excessive attenuation",
			logger.Float64("calculated_gain", gainNeeded),
			logger.Float64("min_gain", -maxGain))
		gainNeeded = -maxGain
		gainLimited = true
	}
	log.Debug("Calculated gain adjustment",
		logger.Float64("gain_db", gainNeeded),
		logger.Float64("target_lufs", targetIntegratedLoudnessLUFS),
		logger.Float64("measured_lufs", inputLUFS),
		logger.Bool("limited", gainLimited))

	// --- Pass 2: Apply simple gain adjustment and encode ---
	log.Debug("Applying gain adjustment and encoding to FLAC (Pass 2)", logger.Float64("gain_db", gainNeeded))

	// Use simple volume filter instead of loudnorm
	volumeArgs := fmt.Sprintf("volume=%.2fdB", gainNeeded)

	customArgs := []string{
		"-af", volumeArgs, // Simple gain adjustment filter
		"-c:a", "flac",    // Output codec: FLAC
		"-f", "flac",      // Output format: FLAC
	}

	// Use the provided context for the final encoding operation
	buffer, err := myaudio.ExportAudioWithCustomFFmpegArgsContext(ctx, pcmData, ffmpegPath, customArgs)
	if err != nil {
		log.Error("FFmpeg FLAC encoding with gain adjustment failed",
			logger.Float64("gain_db", gainNeeded),
			logger.Error(err))
		return nil, fmt.Errorf("failed to export PCM to FLAC with gain adjustment: %w", err)
	}

	log.Info("Encoded PCM to FLAC with gain adjustment", logger.Float64("gain_db", gainNeeded))

	// Return the buffer containing the FLAC data
	return buffer, nil
}

// parseDouble safely parses a string to float64, returning defaultValue on error.
func parseDouble(s string, defaultValue float64) float64 {
	val, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return defaultValue
	}
	return val
}

// UploadSoundscape uploads a soundscape file to the Luistervink API and returns the soundscape ID if successful.
// It handles the PCM to WAV conversion, compresses the data, and manages HTTP request creation and response handling safely.
func (l *LvClient) UploadSoundscape(timestamp string, pcmData []byte) (soundscapeID string, err error) {
	log := GetLogger()

	// Track performance timing for telemetry
	// Note: Wrapped in closure so soundscapeID is captured at execution time, not registration time
	startTime := time.Now()
	defer func() {
		trackOperationTiming(&err, "soundscape_upload", startTime, "timestamp", timestamp, "soundscape_id", soundscapeID)()
	}()

	log.Info("Starting soundscape upload", logger.String("timestamp", timestamp))

	// Validate input
	if len(pcmData) == 0 {
		return "", errors.New(fmt.Errorf("pcmData is empty")).
			Component("luistervink").
			Category(errors.CategoryValidation).
			Context("timestamp", timestamp).
			Build()
	}

	// Encode PCM data to audio format (FLAC with FFmpeg, or WAV fallback)
	encodingResult, err := encodeAudioForUpload(l.Settings, pcmData, timestamp)
	if err != nil {
		return "", errors.New(err).
			Component("luistervink").
			Category(errors.CategoryAudio).
			Context("timestamp", timestamp).
			Build()
	}
	audioBuffer := encodingResult.buffer
	audioExt := encodingResult.ext

	// If debug is enabled, save the audio file locally
	if l.Settings.Realtime.Luistervink.Debug {
		saveDebugAudioFile(audioBuffer, audioExt, timestamp)
	}

	// Create and execute the POST request
	// Note: FLAC is already compressed, so we don't gzip it
	soundscapeURL := fmt.Sprintf("https://api.luistervink.nl/api/v1/stations/%s/soundscapes?timestamp=%s&type=%s",
		l.LuistervinkID, neturl.QueryEscape(timestamp), audioExt)
	maskedURL := l.maskURL(soundscapeURL)
	log.Debug("Creating soundscape upload request",
		logger.String("url", maskedURL),
		logger.Int("audio_size", audioBuffer.Len()))
	req, err := http.NewRequest("POST", soundscapeURL, audioBuffer)
	if err != nil {
		log.Error("Failed to create soundscape POST request",
			logger.String("url", maskedURL),
			logger.Error(err))
		return "", fmt.Errorf("failed to create POST request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("User-Agent", "BirdNET-Go")

	// Execute the request
	log.Info("Uploading soundscape",
		logger.String("url", maskedURL),
		logger.String("format", audioExt))
	resp, err := l.HTTPClient.Do(req)
	if err != nil {
		log.Error("Soundscape upload request failed",
			logger.String("url", maskedURL),
			logger.Error(err))
		return "", handleNetworkError(err, maskedURL, httpClientTimeout, "soundscape upload")
	}
	if resp == nil {
		log.Error("Soundscape upload received nil response", logger.String("url", maskedURL))
		return "", fmt.Errorf("received nil response")
	}
	defer closeResponseBody(resp)
	log.Debug("Received soundscape upload response",
		logger.String("url", maskedURL),
		logger.Int("status_code", resp.StatusCode))

	// Process the response using the new handler
	responseBody, err := handleHTTPResponse(resp, http.StatusCreated, "soundscape upload", maskedURL)
	if err != nil {
		return "", err
	}

	if l.Settings.Realtime.Luistervink.Debug {
		log.Debug("Soundscape response body", logger.String("body", string(responseBody)))
	}

	// Parse and validate response
	soundscapeID, err = parseSoundscapeResponse(responseBody, maskedURL, resp.StatusCode)
	if err != nil {
		return "", err
	}

	log.Info("Soundscape uploaded successfully",
		logger.String("timestamp", timestamp),
		logger.String("soundscape_id", soundscapeID),
		logger.String("url", maskedURL))
	return soundscapeID, nil
}

// PostDetection posts a detection to the Luistervink API matching the specified soundscape ID.
// The actual API endpoint is POST /api/detections/ with device token as query parameter.
func (l *LvClient) PostDetection(soundscapeID, timestamp, commonName, scientificName string, confidence float64) (err error) {
	log := GetLogger()

	// Track performance timing for telemetry
	defer trackOperationTiming(&err, "detection_post", time.Now(), "soundscape_id", soundscapeID)()

	log.Info("Starting detection post",
		logger.String("soundscape_id", soundscapeID),
		logger.String("timestamp", timestamp),
		logger.String("common_name", commonName),
		logger.String("scientific_name", scientificName),
		logger.Float64("confidence", confidence))

	// Simple input validation
	if soundscapeID == "" || timestamp == "" || scientificName == "" {
		enhancedErr := errors.New(fmt.Errorf("invalid input: soundscapeID, timestamp, and scientificName must be non-empty")).
			Component("luistervink").
			Category(errors.CategoryValidation).
			Context("soundscape_id", soundscapeID).
			Context("timestamp", timestamp).
			Context("scientific_name", scientificName).
			Build()
		log.Error("Detection post failed: Invalid input",
			logger.String("soundscape_id", soundscapeID),
			logger.String("timestamp", timestamp),
			logger.String("scientific_name", scientificName),
			logger.Error(enhancedErr))
		return enhancedErr
	}

	// API uses token as query parameter
	detectionURL := fmt.Sprintf("https://api.luistervink.nl/api/detections/?token=%s", neturl.QueryEscape(l.LuistervinkID))
	maskedDetectionURL := l.maskURL(detectionURL)

	// Fuzz location coordinates with user defined accuracy
	fuzzedLatitude, fuzzedLongitude := l.RandomizeLocation(l.Accuracy)

	// Validate timestamp format
	_, err = time.Parse("2006-01-02T15:04:05.000-0700", timestamp)
	if err != nil {
		log.Error("Failed to parse timestamp for detection post",
			logger.String("timestamp", timestamp),
			logger.Error(err))
		return fmt.Errorf("failed to parse timestamp: %w", err)
	}

	// Calculate soundscape start and end times as doubles (seconds from start)
	soundscapeStartTime := 0.0                            // Detection starts at beginning of soundscape
	soundscapeEndTime := float64(detectionDurationSeconds) // Detection duration in seconds

	log.Debug("Calculated detection time range",
		logger.String("timestamp", timestamp),
		logger.Float64("soundscape_start_time", soundscapeStartTime),
		logger.Float64("soundscape_end_time", soundscapeEndTime))

	// Prepare JSON payload matching the actual API schema
	// Required: timestamp, scientificName, lat, lon, confidence, soundscapeId
	// Optional: commonName, soundscapeStartTime, soundscapeEndTime
	postData := struct {
		Timestamp           string  `json:"timestamp"`
		ScientificName      string  `json:"scientificName"`
		Latitude            float64 `json:"lat"`
		Longitude           float64 `json:"lon"`
		Confidence          float64 `json:"confidence"`
		SoundscapeID        string  `json:"soundscapeId"`
		CommonName          string  `json:"commonName,omitempty"`
		SoundscapeStartTime float64 `json:"soundscapeStartTime,omitempty"`
		SoundscapeEndTime   float64 `json:"soundscapeEndTime,omitempty"`
	}{
		Timestamp:           timestamp,
		ScientificName:      scientificName,
		Latitude:            fuzzedLatitude,
		Longitude:           fuzzedLongitude,
		Confidence:          confidence,
		SoundscapeID:        soundscapeID,
		CommonName:          commonName,
		SoundscapeStartTime: soundscapeStartTime,
		SoundscapeEndTime:   soundscapeEndTime,
	}

	// Marshal JSON data
	postDataBytes, err := json.Marshal(postData)
	if err != nil {
		log.Error("Failed to marshal detection JSON data", logger.Error(err))
		return fmt.Errorf("failed to marshal JSON data: %w", err)
	}

	if l.Settings.Realtime.Luistervink.Debug {
		log.Debug("Detection JSON Payload", logger.String("payload", string(postDataBytes)))
	}

	// Execute POST request
	log.Info("Posting detection",
		logger.String("url", maskedDetectionURL),
		logger.String("soundscape_id", soundscapeID),
		logger.String("scientific_name", scientificName))
	resp, err := l.HTTPClient.Post(detectionURL, "application/json", bytes.NewBuffer(postDataBytes))
	if err != nil {
		log.Error("Detection post request failed",
			logger.String("url", maskedDetectionURL),
			logger.String("soundscape_id", soundscapeID),
			logger.Error(err))
		return handleNetworkError(err, maskedDetectionURL, httpClientTimeout, "detection post")
	}
	if resp == nil {
		log.Error("Detection post received nil response",
			logger.String("url", maskedDetectionURL),
			logger.String("soundscape_id", soundscapeID))
		return fmt.Errorf("received nil response")
	}
	defer closeResponseBody(resp)
	log.Debug("Received detection post response",
		logger.String("url", maskedDetectionURL),
		logger.String("soundscape_id", soundscapeID),
		logger.Int("status_code", resp.StatusCode))

	// Handle response using the new handler
	_, err = handleHTTPResponse(resp, http.StatusCreated, "detection post", maskedDetectionURL)
	if err != nil {
		// Add additional context for detection-specific error
		var enhancedErr *errors.EnhancedError
		if errors.As(err, &enhancedErr) {
			enhancedErr.Context["soundscape_id"] = soundscapeID
			enhancedErr.Context["scientific_name"] = scientificName
		}
		return err
	}

	log.Info("Detection posted successfully",
		logger.String("soundscape_id", soundscapeID),
		logger.String("scientific_name", scientificName))
	return nil
}

// PostTestDetection posts a detection to the Luistervink test API endpoint.
// This is used for integration testing and does not affect production data.
// The test endpoint is POST /api/detections/test/ with device token as query parameter.
func (l *LvClient) PostTestDetection(soundscapeID, timestamp, commonName, scientificName string, confidence float64) (err error) {
	log := GetLogger()

	// Track performance timing for telemetry
	defer trackOperationTiming(&err, "test_detection_post", time.Now(), "soundscape_id", soundscapeID)()

	log.Info("Starting test detection post",
		logger.String("soundscape_id", soundscapeID),
		logger.String("timestamp", timestamp),
		logger.String("common_name", commonName),
		logger.String("scientific_name", scientificName),
		logger.Float64("confidence", confidence))

	// Simple input validation
	if soundscapeID == "" || timestamp == "" || scientificName == "" {
		enhancedErr := errors.New(fmt.Errorf("invalid input: soundscapeID, timestamp, and scientificName must be non-empty")).
			Component("luistervink").
			Category(errors.CategoryValidation).
			Context("soundscape_id", soundscapeID).
			Context("timestamp", timestamp).
			Context("scientific_name", scientificName).
			Build()
		log.Error("Test detection post failed: Invalid input",
			logger.String("soundscape_id", soundscapeID),
			logger.String("timestamp", timestamp),
			logger.String("scientific_name", scientificName),
			logger.Error(enhancedErr))
		return enhancedErr
	}

	// Use the test endpoint with token as query parameter
	detectionURL := fmt.Sprintf("https://api.luistervink.nl/api/detections/test/?token=%s", neturl.QueryEscape(l.LuistervinkID))
	maskedDetectionURL := l.maskURL(detectionURL)

	// Fuzz location coordinates with user defined accuracy
	fuzzedLatitude, fuzzedLongitude := l.RandomizeLocation(l.Accuracy)

	// Validate timestamp format
	_, err = time.Parse("2006-01-02T15:04:05.000-0700", timestamp)
	if err != nil {
		log.Error("Failed to parse timestamp for test detection post",
			logger.String("timestamp", timestamp),
			logger.Error(err))
		return fmt.Errorf("failed to parse timestamp: %w", err)
	}

	// Calculate soundscape start and end times as doubles (seconds from start)
	soundscapeStartTime := 0.0                            // Detection starts at beginning of soundscape
	soundscapeEndTime := float64(detectionDurationSeconds) // Detection duration in seconds

	log.Debug("Calculated detection time range",
		logger.String("timestamp", timestamp),
		logger.Float64("soundscape_start_time", soundscapeStartTime),
		logger.Float64("soundscape_end_time", soundscapeEndTime))

	// Prepare JSON payload matching the actual API schema
	postData := struct {
		Timestamp           string  `json:"timestamp"`
		ScientificName      string  `json:"scientificName"`
		Latitude            float64 `json:"lat"`
		Longitude           float64 `json:"lon"`
		Confidence          float64 `json:"confidence"`
		SoundscapeID        string  `json:"soundscapeId"`
		CommonName          string  `json:"commonName,omitempty"`
		SoundscapeStartTime float64 `json:"soundscapeStartTime,omitempty"`
		SoundscapeEndTime   float64 `json:"soundscapeEndTime,omitempty"`
	}{
		Timestamp:           timestamp,
		ScientificName:      scientificName,
		Latitude:            fuzzedLatitude,
		Longitude:           fuzzedLongitude,
		Confidence:          confidence,
		SoundscapeID:        soundscapeID,
		CommonName:          commonName,
		SoundscapeStartTime: soundscapeStartTime,
		SoundscapeEndTime:   soundscapeEndTime,
	}

	// Marshal JSON data
	postDataBytes, err := json.Marshal(postData)
	if err != nil {
		log.Error("Failed to marshal test detection JSON data", logger.Error(err))
		return fmt.Errorf("failed to marshal JSON data: %w", err)
	}

	if l.Settings.Realtime.Luistervink.Debug {
		log.Debug("Test Detection JSON Payload", logger.String("payload", string(postDataBytes)))
	}

	// Execute POST request
	log.Info("Posting test detection",
		logger.String("url", maskedDetectionURL),
		logger.String("soundscape_id", soundscapeID),
		logger.String("scientific_name", scientificName))
	resp, err := l.HTTPClient.Post(detectionURL, "application/json", bytes.NewBuffer(postDataBytes))
	if err != nil {
		log.Error("Test detection post request failed",
			logger.String("url", maskedDetectionURL),
			logger.String("soundscape_id", soundscapeID),
			logger.Error(err))
		return handleNetworkError(err, maskedDetectionURL, httpClientTimeout, "test detection post")
	}
	if resp == nil {
		log.Error("Test detection post received nil response",
			logger.String("url", maskedDetectionURL),
			logger.String("soundscape_id", soundscapeID))
		return fmt.Errorf("received nil response")
	}
	defer closeResponseBody(resp)
	log.Debug("Received test detection post response",
		logger.String("url", maskedDetectionURL),
		logger.String("soundscape_id", soundscapeID),
		logger.Int("status_code", resp.StatusCode))

	// Handle response using the new handler - test endpoint returns 200 on success
	_, err = handleHTTPResponse(resp, http.StatusOK, "test detection post", maskedDetectionURL)
	if err != nil {
		// Add additional context for detection-specific error
		var enhancedErr *errors.EnhancedError
		if errors.As(err, &enhancedErr) {
			enhancedErr.Context["soundscape_id"] = soundscapeID
			enhancedErr.Context["scientific_name"] = scientificName
		}
		return err
	}

	log.Info("Test detection posted successfully",
		logger.String("soundscape_id", soundscapeID),
		logger.String("scientific_name", scientificName))
	return nil
}

// Publish function handles posting detection details to Luistervink.
// It parses the timestamp from the note and posts the detection without uploading audio.
func (l *LvClient) Publish(note *datastore.Note, pcmData []byte) (err error) {
	log := GetLogger()

	// Track performance timing for telemetry
	defer trackOperationTiming(&err, "publish", time.Now(), "common_name", note.CommonName, "scientific_name", note.ScientificName)()

	log.Info("Starting publish process",
		logger.String("date", note.Date),
		logger.String("time", note.Time),
		logger.String("common_name", note.CommonName),
		logger.String("scientific_name", note.ScientificName),
		logger.Float64("confidence", note.Confidence))

	// Use system's local timezone for timestamp parsing
	loc := time.Local

	// Combine date and time from note to form a full timestamp string
	dateTimeString := fmt.Sprintf("%sT%s", note.Date, note.Time)

	// Parse the timestamp using the given format and the system's local timezone
	parsedTime, err := time.ParseInLocation("2006-01-02T15:04:05", dateTimeString, loc)
	if err != nil {
		log.Error("Error parsing date/time for publish",
			logger.String("date", note.Date),
			logger.String("time", note.Time),
			logger.Error(err))
		return fmt.Errorf("error parsing date: %w", err)
	}

	// Format the parsed time to the required timestamp format with timezone information
	timestamp := parsedTime.Format("2006-01-02T15:04:05.000-0700")
	log.Debug("Formatted timestamp for publish", logger.String("timestamp", timestamp))

	// Use fixed soundscape ID of "0" (no audio upload)
	soundscapeID := "0"

	// Post the detection details to Luistervink
	log.Debug("Calling PostDetection",
		logger.String("soundscape_id", soundscapeID),
		logger.String("timestamp", timestamp),
		logger.Any("note", note))
	err = l.PostDetection(soundscapeID, timestamp, note.CommonName, note.ScientificName, note.Confidence)
	if err != nil {
		log.Error("Publish failed: Error during detection post",
			logger.String("soundscape_id", soundscapeID),
			logger.String("timestamp", timestamp),
			logger.Any("note", note),
			logger.Error(err))
		return fmt.Errorf("failed to post detection to Luistervink: %w", err)
	}
	log.Debug("PostDetection completed", logger.String("soundscape_id", soundscapeID))

	log.Info("Publish process completed successfully",
		logger.String("soundscape_id", soundscapeID),
		logger.String("scientific_name", note.ScientificName))
	return nil
}

// Close properly cleans up the LvClient resources
// Currently this just cancels any pending HTTP requests
func (l *LvClient) Close() {
	log := GetLogger()

	log.Info("Closing Luistervink client")
	if l.HTTPClient != nil && l.HTTPClient.Transport != nil {
		// If the transport implements the CloseIdleConnections method, call it
		type transporter interface {
			CloseIdleConnections()
		}
		if transport, ok := l.HTTPClient.Transport.(transporter); ok {
			log.Debug("Closing idle HTTP connections")
			transport.CloseIdleConnections()
		}
		// Cancel any in-flight requests by using a new client
		l.HTTPClient = nil // Allow GC to collect the old client/transport
	}

	if l.Settings.Realtime.Luistervink.Debug {
		log.Info("Luistervink client closed")
	}
}

// createDebugDirectory creates a directory for debug files and returns any error encountered
func createDebugDirectory(path string) error {
	if err := os.MkdirAll(path, dirPermission); err != nil {
		return fmt.Errorf("couldn't create debug directory: %w", err)
	}
	return nil
}

// audioEncodingResult holds the result of audio encoding
type audioEncodingResult struct {
	buffer *bytes.Buffer
	ext    string
}

// encodeAudioForUpload handles the PCM to FLAC encoding using FFmpeg
// FFmpeg is required as Luistervink only accepts FLAC format
func encodeAudioForUpload(settings *conf.Settings, pcmData []byte, timestamp string) (*audioEncodingResult, error) {
	log := GetLogger()

	// Use the validated FFmpeg path from settings (validated at startup)
	// This avoids redundant exec.LookPath calls on every upload
	ffmpegPathForExec := settings.Realtime.Audio.FfmpegPath
	ffmpegAvailable := ffmpegPathForExec != ""
	log.Debug("Checking FFmpeg availability",
		logger.String("path", ffmpegPathForExec),
		logger.Bool("available", ffmpegAvailable))

	if !ffmpegAvailable {
		log.Error("FFmpeg not available, cannot encode to FLAC for Luistervink",
			logger.String("timestamp", timestamp))
		return nil, fmt.Errorf("FFmpeg is required for Luistervink uploads (FLAC encoding)")
	}

	return encodeWithFFmpeg(settings, pcmData, ffmpegPathForExec, timestamp)
}

// encodeWithFFmpeg encodes PCM to FLAC format using FFmpeg
func encodeWithFFmpeg(settings *conf.Settings, pcmData []byte, ffmpegPath, timestamp string) (*audioEncodingResult, error) {
	log := GetLogger()

	ctx, cancel := context.WithTimeout(context.Background(), encodingTimeout)
	defer cancel()

	audioBuffer, err := encodeFlacUsingFFmpeg(ctx, pcmData, ffmpegPath, settings)
	if err != nil {
		log.Error("FLAC encoding failed",
			logger.String("timestamp", timestamp),
			logger.Error(err))
		logFLACEncodingError(err)
		return nil, fmt.Errorf("FLAC encoding failed: %w", err)
	}
	log.Info("Encoded audio to FLAC format", logger.String("timestamp", timestamp))
	return &audioEncodingResult{buffer: audioBuffer, ext: "flac"}, nil
}

// logFLACEncodingError logs the appropriate message for FLAC encoding failures
func logFLACEncodingError(err error) {
	log := GetLogger()

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		log.Warn("FLAC encoding timed out or was cancelled", logger.Error(err))
	} else {
		log.Error("Failed to encode/normalize PCM to FLAC", logger.Error(err))
	}
}

// saveDebugAudioFile saves audio buffer to a debug file if debug mode is enabled
func saveDebugAudioFile(audioBuffer *bytes.Buffer, audioExt, timestamp string) {
	log := GetLogger()

	parsedTime, parseErr := time.Parse("2006-01-02T15:04:05.000-0700", timestamp)
	if parseErr != nil {
		log.Warn("Could not parse timestamp for debug file saving",
			logger.String("timestamp", timestamp),
			logger.String("format", audioExt),
			logger.Error(parseErr))
		return
	}

	debugDir := filepath.Join("debug", "luistervink", audioExt)
	debugFilename := filepath.Join(debugDir, fmt.Sprintf("lv_debug_%s.%s", parsedTime.Format("20060102_150405"), audioExt))
	endTime := parsedTime.Add(detectionDurationSeconds * time.Second)

	audioCopy := bytes.NewBuffer(audioBuffer.Bytes())
	if saveErr := saveBufferToFile(audioCopy, debugFilename, parsedTime, endTime); saveErr != nil {
		log.Warn("Could not save debug file",
			logger.String("filename", debugFilename),
			logger.Error(saveErr))
	} else {
		log.Debug("Saved debug file", logger.String("filename", debugFilename))
	}
}

// parseSoundscapeResponse parses the JSON response from soundscape upload
func parseSoundscapeResponse(responseBody []byte, maskedURL string, statusCode int) (string, error) {
	log := GetLogger()

	var sdata SoundscapeResponse
	if err := json.Unmarshal(responseBody, &sdata); err != nil {
		// Check if this might be HTML even though we got 200 OK
		if strings.Contains(string(responseBody), "<") && strings.Contains(string(responseBody), ">") {
			htmlError := extractHTMLError(string(responseBody))
			log.Error("Received HTML response with 200 OK status",
				logger.String("operation", "soundscape upload"),
				logger.String("url", maskedURL),
				logger.String("html_error", htmlError),
				logger.String("response_preview", string(responseBody[:min(len(responseBody), maxResponsePreview)])))
			return "", errors.New(fmt.Errorf("soundscape upload failed: %s", htmlError)).
				Component("luistervink").
				Category(errors.CategoryNetwork).
				Context("response_type", "html_with_200").
				Context("operation", "soundscape upload").
				Build()
		}
		log.Error("Failed to decode soundscape JSON response",
			logger.String("url", maskedURL),
			logger.Int("status_code", statusCode),
			logger.String("body", string(responseBody)),
			logger.Error(err))
		return "", fmt.Errorf("failed to decode JSON response: %w", err)
	}

	if !sdata.Success {
		log.Error("Soundscape upload was not successful according to API response",
			logger.String("url", maskedURL),
			logger.Int("status_code", statusCode),
			logger.Any("response", sdata))
		return "", fmt.Errorf("upload failed, response reported failure")
	}

	return fmt.Sprintf("%d", sdata.Soundscape.ID), nil
}

// trackOperationTiming creates a deferred timing tracker for operations
// Usage: defer trackOperationTiming(&err, "operation_name", time.Now(), contextFields...)()
//
//nolint:gocritic // errPtr must be a pointer to modify the error in the calling function's scope
func trackOperationTiming(errPtr *error, operation string, startTime time.Time, contextFields ...any) func() {
	return func() {
		log := GetLogger()

		duration := time.Since(startTime)
		if *errPtr != nil {
			// Add timing context to error
			var enhancedErr *errors.EnhancedError
			if errors.As(*errPtr, &enhancedErr) {
				// Initialize Context map if nil to prevent panic
				if enhancedErr.Context == nil {
					enhancedErr.Context = make(map[string]any)
				}
				enhancedErr.Context["operation_duration_ms"] = duration.Milliseconds()
				enhancedErr.Context["operation"] = operation
			} else {
				*errPtr = errors.New(*errPtr).
					Component("luistervink").
					Category(errors.CategoryNetwork).
					Timing(operation, duration).
					Build()
			}
			logArgs := append([]logger.Field{
				logger.String("operation", operation),
				logger.Int64("duration_ms", duration.Milliseconds()),
				logger.Error(*errPtr),
			}, convertToFields(contextFields)...)
			log.Warn(fmt.Sprintf("%s failed", operation), logArgs...)
		} else {
			logArgs := append([]logger.Field{
				logger.String("operation", operation),
				logger.Int64("duration_ms", duration.Milliseconds()),
			}, convertToFields(contextFields)...)
			log.Info(fmt.Sprintf("%s completed", operation), logArgs...)
		}
	}
}

// convertToFields converts variadic key-value pairs to logger.Field slice
func convertToFields(args []any) []logger.Field {
	fields := make([]logger.Field, 0, len(args)/2)
	for i := 0; i+1 < len(args); i += 2 {
		key, ok := args[i].(string)
		if !ok {
			continue
		}
		switch v := args[i+1].(type) {
		case string:
			fields = append(fields, logger.String(key, v))
		case int:
			fields = append(fields, logger.Int(key, v))
		case int64:
			fields = append(fields, logger.Int64(key, v))
		case float64:
			fields = append(fields, logger.Float64(key, v))
		case bool:
			fields = append(fields, logger.Bool(key, v))
		case error:
			fields = append(fields, logger.Error(v))
		default:
			fields = append(fields, logger.Any(key, v))
		}
	}
	return fields
}
