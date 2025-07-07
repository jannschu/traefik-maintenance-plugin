package traefik_maintenance_plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rickb777/accept"
)

// Initialize random source for jitter calculations
var (
	randSource = rand.New(rand.NewSource(time.Now().UnixNano()))
	randMutex  sync.Mutex // Mutex to protect randSource as it's not concurrent-safe
)

type Config struct {
	EnvironmentEndpoints    map[string]string            `json:"environmentEndpoints,omitempty"`
	EnvironmentSecrets      map[string]EnvironmentSecret `json:"environmentSecrets,omitempty"`
	CacheDurationInSeconds  int                          `json:"cacheDurationInSeconds,omitempty"`
	SkipPrefixes            []string                     `json:"skipPrefixes,omitempty"`
	SkipHosts               []string                     `json:"skipHosts,omitempty"`
	RequestTimeoutInSeconds int                          `json:"requestTimeoutInSeconds,omitempty"`
	MaintenanceStatusCode   int                          `json:"maintenanceStatusCode,omitempty"`
	Debug                   bool                         `json:"debug,omitempty"`
	SecretHeader            string                       `json:"secretHeader,omitempty"`
	SecretHeaderValue       string                       `json:"secretHeaderValue,omitempty"`
	Content                 string                       `json:"content,omitempty"`
}

type EnvironmentSecret struct {
	Header string `json:"header"`
	Value  string `json:"value"`
}

func CreateConfig() *Config {
	return &Config{
		EnvironmentEndpoints:    map[string]string{},
		EnvironmentSecrets:      map[string]EnvironmentSecret{},
		CacheDurationInSeconds:  10,
		SkipPrefixes:            []string{},
		SkipHosts:               []string{},
		RequestTimeoutInSeconds: 5,
		MaintenanceStatusCode:   503,
		Debug:                   false,
		Content:                 "",
	}
}

type MaintenanceStatus struct {
	IsActive  bool     `json:"is_active"`
	Whitelist []string `json:"whitelist"`
}

type EnvironmentCache struct {
	isActive            bool
	whitelist           []string
	expiry              time.Time
	failedAttempts      int
	lastSuccessfulFetch time.Time
}

// sharedCache holds maintenance status for multiple environments
var (
	sharedCache struct {
		sync.RWMutex
		environments         map[string]*EnvironmentCache
		environmentEndpoints map[string]string
		environmentSecrets   map[string]EnvironmentSecret
		cacheDuration        time.Duration
		requestTimeout       time.Duration
		client               *http.Client
		debug                bool
		initialized          bool
		refresherRunning     bool
		stopCh               chan struct{}
		userAgent            string
		secretHeader         string
		secretHeaderValue    string
	}
	initLock     sync.Mutex
	refreshLock  sync.Mutex
	shutdownOnce sync.Once // Ensure clean shutdown happens only once
)

type MaintenanceCheck struct {
	next                  http.Handler
	skipPrefixes          []string
	skipHosts             []string
	maintenanceStatusCode int
	debug                 bool
	content               string
}

func ensureSharedCacheInitialized(environmentEndpoints map[string]string, environmentSecrets map[string]EnvironmentSecret, cacheDuration, requestTimeout time.Duration, debug bool, userAgent string, secretHeader, secretHeaderValue string) {
	if sharedCache.initialized {
		return
	}

	initLock.Lock()
	defer initLock.Unlock()

	if sharedCache.initialized {
		return
	}

	if cacheDuration <= 0 {
		if debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Warning: invalid cache duration %v, using default of 10s\n", cacheDuration)
		}
		cacheDuration = 10 * time.Second
	}

	if requestTimeout <= 0 {
		if debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Warning: invalid request timeout %v, using default of 5s\n", requestTimeout)
		}
		requestTimeout = 5 * time.Second
	}

	var wg sync.WaitGroup
	if sharedCache.initialized && sharedCache.refresherRunning && sharedCache.stopCh != nil {
		wg.Add(1)
		oldStopCh := sharedCache.stopCh

		sharedCache.stopCh = make(chan struct{})

		sharedCache.Lock()
		shutdownInProgress := true
		sharedCache.Unlock()

		close(oldStopCh)

		go func() {
			shutdownTimer := time.NewTimer(500 * time.Millisecond)
			defer shutdownTimer.Stop()

			<-shutdownTimer.C

			sharedCache.Lock()
			if shutdownInProgress && sharedCache.refresherRunning {
				if debug {
					fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Refresher didn't shut down in time, forcing cleanup\n")
				}
				sharedCache.refresherRunning = false
			}
			sharedCache.Unlock()

			wg.Done()
		}()

		wg.Wait()
	} else {
		sharedCache.stopCh = make(chan struct{})
	}

	transport := &http.Transport{
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
	}

	client := &http.Client{
		Timeout:   requestTimeout,
		Transport: transport,
	}

	sharedCache.Lock()
	sharedCache.client = client
	sharedCache.environments = make(map[string]*EnvironmentCache)
	sharedCache.environmentEndpoints = environmentEndpoints
	sharedCache.environmentSecrets = environmentSecrets
	sharedCache.cacheDuration = cacheDuration
	sharedCache.requestTimeout = requestTimeout
	sharedCache.debug = debug
	sharedCache.userAgent = userAgent
	sharedCache.initialized = true
	sharedCache.refresherRunning = false
	sharedCache.secretHeader = secretHeader
	sharedCache.secretHeaderValue = secretHeaderValue
	sharedCache.Unlock()

	// Perform initial fetch for all environments
	if debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Performing initial fetch for all environments\n")
	}

	firstEnv := true
	for envSuffix := range environmentEndpoints {
		if firstEnv {
			// Load the first environment synchronously
			retryDelay := 100 * time.Millisecond
			for range 5 {
				if refreshMaintenanceStatusForEnvironment(envSuffix) {
					break
				}

				if debug {
					fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Initial fetch failed for environment '%s', retrying in %v\n", envSuffix, retryDelay)
				}
				time.Sleep(retryDelay)
				retryDelay *= 2
			}
			firstEnv = false
		} else {
			go func(env string) {
				retryDelay := 100 * time.Millisecond
				for range 5 {
					if refreshMaintenanceStatusForEnvironment(env) {
						break
					}

					if debug {
						fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Initial fetch failed for environment '%s', retrying in %v\n", env, retryDelay)
					}
					select {
					case <-sharedCache.stopCh:
						return
					case <-time.After(retryDelay):
					}
					retryDelay *= 2
				}
			}(envSuffix)
		}
	}

	startBackgroundRefresher()
}

func startBackgroundRefresher() {
	if sharedCache.refresherRunning {
		return
	}

	sharedCache.Lock()
	if sharedCache.refresherRunning {
		sharedCache.Unlock()
		return
	}

	sharedCache.refresherRunning = true
	stopCh := sharedCache.stopCh
	debug := sharedCache.debug
	cacheDuration := sharedCache.cacheDuration
	sharedCache.Unlock()

	if debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Started shared background refresher with interval of %v\n", cacheDuration)
	}

	go func() {
		ticker := time.NewTicker(cacheDuration)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				refreshAllEnvironments()
			case <-stopCh:
				if debug {
					fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Shared background refresher stopped\n")
				}

				sharedCache.Lock()
				sharedCache.refresherRunning = false
				sharedCache.Unlock()
				return
			}
		}
	}()
}

func refreshAllEnvironments() {
	sharedCache.RLock()
	environmentEndpoints := sharedCache.environmentEndpoints
	sharedCache.RUnlock()

	for envSuffix := range environmentEndpoints {
		refreshMaintenanceStatusForEnvironment(envSuffix)
	}
}

func refreshMaintenanceStatusForEnvironment(envSuffix string) bool {
	if !refreshLock.TryLock() {
		return true
	}

	defer func() {
		refreshLock.Unlock()
	}()

	sharedCache.RLock()
	client := sharedCache.client
	requestTimeout := sharedCache.requestTimeout
	userAgent := sharedCache.userAgent
	cacheDuration := sharedCache.cacheDuration
	debug := sharedCache.debug

	envCache, exists := sharedCache.environments[envSuffix]
	if !exists {
		envCache = &EnvironmentCache{
			expiry: time.Now().Add(-1 * time.Minute),
		}
	}
	needsRefresh := time.Now().After(envCache.expiry)
	currentFailedAttempts := envCache.failedAttempts

	var secretHeader, secretHeaderValue string
	if envSecret, exists := sharedCache.environmentSecrets[envSuffix]; exists {
		secretHeader = envSecret.Header
		secretHeaderValue = envSecret.Value
	} else if sharedCache.secretHeader != "" && sharedCache.secretHeaderValue != "" {
		secretHeader = sharedCache.secretHeader
		secretHeaderValue = sharedCache.secretHeaderValue
	}

	sharedCache.RUnlock()

	if !needsRefresh {
		if debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Environment '%s' cache is still valid, skipping refresh\n", envSuffix)
		}
		return true
	}

	endpoint := getEndpointForDomain(envSuffix)

	if endpoint == "" {
		if debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] No endpoint found for environment '%s', skipping maintenance check\n", envSuffix)
		}
		result := MaintenanceStatus{
			IsActive:  false,
			Whitelist: []string{},
		}
		updateEnvironmentCache(envSuffix, &result, cacheDuration, 0, true)
		return true
	}

	if debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Fetching maintenance status from '%s' for environment '%s'\n", endpoint, envSuffix)
	}

	var result MaintenanceStatus

	// Is endpoint a local file?
	if after, ok := strings.CutPrefix(endpoint, "file://"); ok {
		filePath := after
		// Check if path exists
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			if debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] File '%s' does not exist, skipping maintenance check\n", filePath)
			}
			result = MaintenanceStatus{
				IsActive:  false,
				Whitelist: []string{},
			}
		} else {
			file, err := os.Open(filePath)
			if err != nil {
				if debug {
					fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Error reading file: %v\n", err)
				}
				backoffTime := calculateBackoff(currentFailedAttempts)
				updateEnvironmentCache(envSuffix, nil, backoffTime, currentFailedAttempts+1, false)
				return false
			} else {
				var status MaintenanceStatus
				decoder := json.NewDecoder(file)
				if err := decoder.Decode(&status); err != nil {
					if debug {
						fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Error parsing JSON: %v\n", err)
					}
					backoffTime := calculateBackoff(currentFailedAttempts)
					updateEnvironmentCache(envSuffix, nil, backoffTime, currentFailedAttempts+1, false)
					return false
				}
				result = MaintenanceStatus{
					IsActive:  status.IsActive,
					Whitelist: status.Whitelist,
				}
			}
		}
	} else {
		if client == nil {
			if debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] HTTP client is nil, skipping refresh\n")
			}
			return false
		}

		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			if debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Error creating request: %v\n", err)
			}

			backoffTime := calculateBackoff(currentFailedAttempts)
			updateEnvironmentCache(envSuffix, nil, backoffTime, currentFailedAttempts+1, false)
			return false
		}

		req.Header.Set("User-Agent", userAgent)

		if secretHeader != "" && secretHeaderValue != "" {
			req.Header.Set(secretHeader, secretHeaderValue)
			if debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Added secret header '%s' for environment '%s'\n", secretHeader, envSuffix)
			}
		}

		resp, err := client.Do(req)
		if err != nil {
			if debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Error making request: %v\n", err)
			}

			backoffTime := calculateBackoff(currentFailedAttempts)
			updateEnvironmentCache(envSuffix, nil, backoffTime, currentFailedAttempts+1, false)
			return false
		}

		if resp != nil && resp.Body != nil {
			defer func() {
				err := resp.Body.Close()
				if err != nil && debug {
					fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Error closing response body: %v\n", err)
				}
			}()
		}

		if resp == nil {
			if debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Nil response received\n")
			}

			backoffTime := calculateBackoff(currentFailedAttempts)
			updateEnvironmentCache(envSuffix, nil, backoffTime, currentFailedAttempts+1, false)
			return false
		}

		if resp.StatusCode != http.StatusOK {
			if debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] API returned status code: %d\n", resp.StatusCode)
			}

			backoffTime := calculateBackoff(currentFailedAttempts)
			updateEnvironmentCache(envSuffix, nil, backoffTime, currentFailedAttempts+1, false)
			return false
		}

		const maxResponseSize = 10 * 1024 * 1024
		limitedReader := http.MaxBytesReader(nil, resp.Body, maxResponseSize)

		type MaintenanceResponse struct {
			SystemConfig struct {
				Maintenance struct {
					IsActive  bool     `json:"is_active"`
					Whitelist []string `json:"whitelist"`
				} `json:"maintenance"`
			} `json:"system_config"`
		}
		var maintenanceResponse MaintenanceResponse
		decoder := json.NewDecoder(limitedReader)
		if err := decoder.Decode(&maintenanceResponse); err != nil {
			if debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Error parsing JSON: %v\n", err)
			}

			backoffTime := calculateBackoff(currentFailedAttempts)
			updateEnvironmentCache(envSuffix, nil, backoffTime, currentFailedAttempts+1, false)
			return false
		}

		result = MaintenanceStatus{
			IsActive:  maintenanceResponse.SystemConfig.Maintenance.IsActive,
			Whitelist: maintenanceResponse.SystemConfig.Maintenance.Whitelist,
		}
	}

	updateEnvironmentCache(envSuffix, &result, cacheDuration, 0, true)

	if debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Successfully updated maintenance status for environment '%s': active=%v, whitelist count=%d\n",
			envSuffix, result.IsActive, len(result.Whitelist))
	}

	return true
}

func updateEnvironmentCache(envSuffix string, result *MaintenanceStatus, duration time.Duration, failedAttempts int, success bool) {
	sharedCache.Lock()
	defer sharedCache.Unlock()

	if sharedCache.environments == nil {
		sharedCache.environments = make(map[string]*EnvironmentCache)
	}

	envCache, exists := sharedCache.environments[envSuffix]
	if !exists {
		envCache = &EnvironmentCache{}
		sharedCache.environments[envSuffix] = envCache
	}

	if success && result != nil {
		envCache.isActive = result.IsActive
		envCache.whitelist = append([]string{}, result.Whitelist...) // Ensure we have a copy
		envCache.expiry = time.Now().Add(duration)
		envCache.failedAttempts = 0
		envCache.lastSuccessfulFetch = time.Now()
	} else {
		envCache.expiry = time.Now().Add(duration)
		envCache.failedAttempts = failedAttempts
	}
}

func getMaintenanceStatusForDomain(domain string) (bool, []string) {
	sharedCache.RLock()
	defer sharedCache.RUnlock()

	if !sharedCache.initialized {
		return false, []string{}
	}

	var envSuffix string
	for suffix := range sharedCache.environmentEndpoints {
		if suffix == "" {
			continue
		}
		if strings.HasSuffix(domain, suffix) {
			envSuffix = suffix
			break
		}
	}

	if envSuffix == "" {
		envSuffix = ""
	}

	envCache, exists := sharedCache.environments[envSuffix]
	if !exists {
		return false, []string{}
	}

	if sharedCache.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Using cached status for environment '%s' (domain: %s): active=%v, whitelist count=%d\n",
			envSuffix, domain, envCache.isActive, len(envCache.whitelist))
	}

	whitelistCopy := append([]string(nil), envCache.whitelist...)
	return envCache.isActive, whitelistCopy
}

// calculateBackoff returns an exponential backoff duration with jitter
func calculateBackoff(attempts int) time.Duration {
	if attempts <= 0 {
		return 5 * time.Second // Minimum backoff
	}

	// Cap maximum number of attempts for backoff calculation to avoid excessive delays
	if attempts > 10 {
		attempts = 10
	}

	// Base exponential backoff: 5s, 10s, 20s, 40s, etc. up to ~1h
	backoff := 5 * time.Second * time.Duration(1<<uint(attempts))

	// Add jitter of +/- 20% to avoid thundering herd problem
	randMutex.Lock()
	jitterFactor := 0.8 + 0.4*randSource.Float64()
	randMutex.Unlock()

	jitter := time.Duration(float64(backoff) * jitterFactor)

	// Cap maximum backoff at 1 hour
	maxBackoff := 1 * time.Hour
	if jitter > maxBackoff {
		return maxBackoff
	}

	return jitter
}

func getEndpointForDomain(domain string) string {
	sharedCache.RLock()
	defer sharedCache.RUnlock()

	for suffix, endpoint := range sharedCache.environmentEndpoints {
		if suffix == "" {
			continue
		}
		if strings.HasSuffix(domain, suffix) {
			return endpoint
		}
	}

	if defaultEndpoint, exists := sharedCache.environmentEndpoints[""]; exists {
		return defaultEndpoint
	}

	return ""
}

func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if config.MaintenanceStatusCode < 100 || config.MaintenanceStatusCode > 599 {
		return nil, fmt.Errorf("invalid maintenance status code: %d (must be between 100-599)",
			config.MaintenanceStatusCode)
	}

	cacheDuration := time.Duration(config.CacheDurationInSeconds) * time.Second
	requestTimeout := time.Duration(config.RequestTimeoutInSeconds) * time.Second
	userAgent := fmt.Sprintf("TraefikMaintenancePlugin/%s", name)

	if cacheDuration <= 0 {
		cacheDuration = 10 * time.Second
	}

	if requestTimeout <= 0 {
		requestTimeout = 5 * time.Second
	}

	ensureSharedCacheInitialized(config.EnvironmentEndpoints, config.EnvironmentSecrets, cacheDuration, requestTimeout, config.Debug, userAgent, config.SecretHeader, config.SecretHeaderValue)

	skipPrefixesCopy := append([]string{}, config.SkipPrefixes...)
	skipHostsCopy := append([]string{}, config.SkipHosts...)

	m := &MaintenanceCheck{
		next:                  next,
		skipPrefixes:          skipPrefixesCopy,
		skipHosts:             skipHostsCopy,
		maintenanceStatusCode: config.MaintenanceStatusCode,
		debug:                 config.Debug,
		content:               config.Content,
	}

	go func() {
		<-ctx.Done()
		if config.Debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Context cancelled for middleware instance\n")
		}
	}()

	return m, nil
}

func (m *MaintenanceCheck) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req == nil {
		http.Error(rw, "Bad Request: nil request received", http.StatusBadRequest)
		return
	}

	if m.handleCORSPreflightRequest(rw, req) {
		return
	}

	m.logRequestHeadersForDebugging(req)

	normalizedHost := m.extractHostWithoutPort(req.Host)

	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Evaluating request: host=%s, path=%s\n", normalizedHost, req.URL.Path)
	}

	if m.isHostSkipped(normalizedHost) {
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Host '%s' is in skip list, bypassing maintenance check\n", normalizedHost)
		}
		m.next.ServeHTTP(rw, req)
		return
	}

	if m.isPrefixSkipped(req.URL.Path) {
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Path '%s' matches skip prefix, bypassing maintenance check\n", req.URL.Path)
		}
		m.next.ServeHTTP(rw, req)
		return
	}

	isActive, whitelist := getMaintenanceStatusForDomain(normalizedHost)
	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Maintenance status: active=%v, whitelist=%v\n", isActive, whitelist)
	}

	if isActive {
		if m.isClientAllowed(req, whitelist) {
			m.next.ServeHTTP(rw, req)
			return
		}

		m.sendMaintenanceResponseWithCORS(rw, req)
		return
	}

	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Maintenance mode is inactive, allowing request\n")
	}
	m.next.ServeHTTP(rw, req)
}

func (m *MaintenanceCheck) handleCORSPreflightRequest(rw http.ResponseWriter, req *http.Request) bool {
	if req.Method != http.MethodOptions {
		return false
	}

	isActive, whitelist := getMaintenanceStatusForDomain(req.Host)
	if !isActive {
		return false
	}

	clientOrigin := req.Header.Get("Origin")
	m.setCORSPreflightHeaders(rw, clientOrigin)

	if !m.isClientAllowed(req, whitelist) {
		m.sendBlockedPreflightResponse(rw)
		return true
	}

	m.sendSuccessfulPreflightResponse(rw)
	return true
}

func (m *MaintenanceCheck) setCORSPreflightHeaders(rw http.ResponseWriter, origin string) {
	if origin == "" {
		return
	}

	corsHeaders := map[string]string{
		"Access-Control-Allow-Origin":      origin,
		"Access-Control-Allow-Methods":     "GET, POST, PUT, DELETE, OPTIONS",
		"Access-Control-Allow-Headers":     "Accept, Authorization, Content-Type, X-CSRF-Token",
		"Access-Control-Allow-Credentials": "true",
		"Access-Control-Max-Age":           "86400",
	}

	for headerName, headerValue := range corsHeaders {
		rw.Header().Set(headerName, headerValue)
	}

	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] CORS preflight handled for origin: %s\n", origin)
	}
}

func (m *MaintenanceCheck) isMaintenanceActiveForClient(req *http.Request) bool {
	isActive, whitelist := getMaintenanceStatusForDomain(req.Host)
	return isActive && !m.isClientAllowed(req, whitelist)
}

func (m *MaintenanceCheck) sendBlockedPreflightResponse(rw http.ResponseWriter) {
	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] CORS preflight completed, but actual request will be blocked due to maintenance mode\n")
	}

	// Preflight must always return 2xx status according to CORS spec
	rw.WriteHeader(http.StatusOK)
}

func (m *MaintenanceCheck) sendSuccessfulPreflightResponse(rw http.ResponseWriter) {
	rw.WriteHeader(http.StatusNoContent)
}

func (m *MaintenanceCheck) logRequestHeadersForDebugging(req *http.Request) {
	if !m.debug {
		return
	}

	fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Request headers for diagnostics:\n")
	for headerName, headerValues := range req.Header {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck]   %s: %s\n", headerName, strings.Join(headerValues, ", "))
	}

	fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Using header priority order: Cf-Connecting-Ip > True-Client-Ip > X-Forwarded-For > X-Real-Ip > X-Client-Ip > Forwarded > X-Original-Forwarded-For > RemoteAddr\n")
}

func (m *MaintenanceCheck) extractHostWithoutPort(originalHost string) string {
	host := originalHost
	if colonIndex := strings.IndexByte(host, ':'); colonIndex > 0 {
		host = host[:colonIndex]
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Normalized host from '%s' to '%s'\n", originalHost, host)
		}
	}
	return host
}

func (m *MaintenanceCheck) sendMaintenanceResponseWithCORS(rw http.ResponseWriter, req *http.Request) {
	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Access denied, returning status code %d\n", m.maintenanceStatusCode)
	}

	m.addCORSHeadersToMaintenanceResponse(rw, req)

	if m.content != "" {
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Probing custom maintenance content from '%s'\n", m.content)
		}
		acceptHeader := req.Header.Get("Accept")
		accept, err := accept.Parse(acceptHeader)
		if err != nil {
			if m.debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Error parsing Accept header: %v\n", err)
			}
		} else {
			send := func(mime string, ext string) bool {
				if !accept.Accepts(mime) {
					return false
				}
				file, err := os.Open(m.content + ext)
				if err != nil {
					return false
				}
				rw.Header().Set("Content-Type", mime)
				rw.WriteHeader(m.maintenanceStatusCode)
				_, err = io.Copy(rw, file)
				if err != nil && m.debug {
					fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Error sending maintenance content: %v\n", err)
				}
				// even in case of error we still want to return true
				// another try should not be made
				return true
			}
			types := []struct {
				mime string
				ext  string
			}{
				{"text/html", ".html"},
				{"application/json", ".json"},
				{"text/plain", ".txt"},
			}
			for _, t := range types {
				if send(t.mime, t.ext) {
					return
				}
			}
		}
	}
	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] No custom maintenance content found, sending default response\n")
	}
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.WriteHeader(m.maintenanceStatusCode)
	_, err := rw.Write([]byte("Service is in maintenance mode"))
	if err != nil && m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Error writing maintenance response: %v\n", err)
	}
}

func (m *MaintenanceCheck) addCORSHeadersToMaintenanceResponse(rw http.ResponseWriter, req *http.Request) {
	clientOrigin := req.Header.Get("Origin")
	if clientOrigin == "" {
		return
	}

	rw.Header().Set("Access-Control-Allow-Origin", clientOrigin)
	rw.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	rw.Header().Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type, X-CSRF-Token")
	rw.Header().Set("Access-Control-Allow-Credentials", "true")
	rw.Header().Set("Access-Control-Max-Age", "86400")

	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Added CORS headers to maintenance response for origin: %s\n", clientOrigin)
	}
}

func getClientIP(req *http.Request, debug bool) string {
	// Guard against nil request
	if req == nil {
		return ""
	}

	// Ordered list of headers to check, with Traefik-specific headers first
	// Traefik sets Cf-Connecting-Ip, True-Client-Ip, and X-Real-Ip headers
	headers := []string{
		"Cf-Connecting-Ip",         // CloudFlare
		"True-Client-Ip",           // Akamai/Cloudflare
		"X-Forwarded-For",          // Standard
		"X-Real-Ip",                // Nginx
		"X-Client-Ip",              // Common
		"Forwarded",                // RFC 7239
		"X-Original-Forwarded-For", // Traefik specific
	}

	if debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Attempting to extract client IP from request headers\n")
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Header priority order: %v\n", headers)

		// Log all available header values for IP detection troubleshooting
		for _, h := range headers {
			val := req.Header.Get(h)
			if val != "" {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Found header %s = %s\n", h, val)
			}
		}
	}

	// First try all headers that might have the real client IP
	for _, h := range headers {
		addresses := req.Header.Get(h)
		if addresses != "" {
			if debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Processing header %s with value: %s\n", h, addresses)
			}

			// Special handling for Forwarded header (RFC 7239)
			if h == "Forwarded" {
				// Forwarded: for=192.0.2.60;proto=http;by=203.0.113.43
				parts := strings.Split(addresses, ";")
				for _, part := range parts {
					if after, ok := strings.CutPrefix(part, "for="); ok {
						ip := after
						// Remove possible port and IPv6
						ip = strings.TrimSuffix(strings.TrimPrefix(ip, "["), "]")
						if idx := strings.LastIndex(ip, ":"); idx != -1 {
							ip = ip[:idx]
						}
						if debug {
							fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Extracted IP from Forwarded header: %s\n", ip)
						}
						return strings.TrimSpace(ip)
					}
				}
				if debug {
					fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Could not extract IP from Forwarded header, no 'for=' parameter found\n")
				}
			} else {
				// Normal comma-separated header value, take the leftmost (client) IP
				parts := strings.Split(addresses, ",")
				if len(parts) > 0 {
					ip := strings.TrimSpace(parts[0])
					// Check for IPv6 bracket notation [IPv6]:port
					ip = strings.TrimSuffix(strings.TrimPrefix(ip, "["), "]")
					if debug {
						if len(parts) > 1 {
							fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Multiple IPs found in %s, using leftmost: %s (full chain: %s)\n",
								h, ip, addresses)
						} else {
							fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Extracted IP from %s header: %s\n", h, ip)
						}
					}
					return ip
				}
			}
		}
	}

	// Fallback to RemoteAddr if no headers found
	ip := req.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	if debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] No proxy headers found, using RemoteAddr IP: %s\n", ip)
	}
	return ip
}

func (m *MaintenanceCheck) isClientAllowed(req *http.Request, whitelist []string) bool {
	// Guard against nil request or whitelist
	if req == nil {
		return false
	}

	if len(whitelist) == 0 {
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Whitelist is empty, blocking request\n")
		}
		return false
	}

	// Extended debug info for whitelist
	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Maintenance whitelist entries: %d items\n", len(whitelist))
		for i, entry := range whitelist {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck]   Whitelist[%d]: %s\n", i, entry)
		}
	}

	if slices.Contains(whitelist, "*") {
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Wildcard (*) found in whitelist, allowing request\n")
		}
		return true
	}

	clientIP := getClientIP(req, m.debug)

	// Invalid IP address
	if clientIP == "" {
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Could not determine client IP, blocking request\n")
		}
		return false
	}

	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Client IP for whitelist check: %s\n", clientIP)
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Beginning whitelist evaluation for IP %s\n", clientIP)
	}

	for _, ip := range whitelist {
		if ip == clientIP {
			if m.debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] IP '%s' matches whitelist entry '%s', allowing request\n", clientIP, ip)
			}
			return true
		}

		// Add support for CIDR notation if the whitelist entry contains a slash
		if strings.Contains(ip, "/") {
			if m.debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Checking if IP '%s' is in CIDR range '%s'\n", clientIP, ip)
			}

			match, err := isCIDRMatch(clientIP, ip)
			if err != nil {
				if m.debug {
					fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Error checking CIDR match: %v\n", err)
				}
			} else if match {
				if m.debug {
					fmt.Fprintf(os.Stdout, "[MaintenanceCheck] IP '%s' is in CIDR range '%s', allowing request\n", clientIP, ip)
				}
				return true
			} else if m.debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] IP '%s' is NOT in CIDR range '%s'\n", clientIP, ip)
			}
		}
	}

	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] IP '%s' not in whitelist, blocking request\n", clientIP)
	}
	return false
}

func (m *MaintenanceCheck) isHostSkipped(host string) bool {
	// Guard against empty hosts
	if host == "" {
		return false
	}

	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Checking host '%s' against skipHosts: %v\n", host, m.skipHosts)
	}

	for _, skipHost := range m.skipHosts {
		// Skip empty entries
		if skipHost == "" {
			continue
		}

		// Check for wildcard domain pattern (*.example.com)
		if strings.HasPrefix(skipHost, "*.") {
			suffix := skipHost[1:] // ".example.com"
			if strings.HasSuffix(host, suffix) {
				if m.debug {
					fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Host '%s' matches wildcard pattern '%s'\n", host, skipHost)
				}
				return true
			}
		} else if skipHost == host {
			if m.debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Host '%s' matches exact host '%s'\n", host, skipHost)
			}
			return true
		}
	}

	return false
}

func (m *MaintenanceCheck) isPrefixSkipped(path string) bool {
	// Guard against nil path
	if path == "" {
		return false
	}

	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Checking path '%s' against skipPrefixes: %v\n", path, m.skipPrefixes)
	}

	for _, prefix := range m.skipPrefixes {
		// Skip empty prefixes
		if prefix == "" {
			continue
		}

		if strings.HasPrefix(path, prefix) {
			if m.debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Path '%s' matches prefix '%s'\n", path, prefix)
			}
			return true
		}
	}

	return false
}

// CloseSharedCache should be called if you need to clean up resources
func CloseSharedCache() {
	shutdownOnce.Do(func() {
		initLock.Lock()
		defer initLock.Unlock()

		if sharedCache.initialized && sharedCache.stopCh != nil {
			if sharedCache.debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Beginning shared cache cleanup\n")
			}

			close(sharedCache.stopCh)

			// Wait a bit for goroutines to terminate
			time.Sleep(200 * time.Millisecond)

			// Clear client to release connections
			sharedCache.client = nil
			sharedCache.initialized = false

			if sharedCache.debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Shared cache resources cleaned up\n")
			}
		}
	})
}

// ResetSharedCacheForTesting resets all shared state for testing
func ResetSharedCacheForTesting() {
	initLock.Lock()
	defer initLock.Unlock()

	if sharedCache.initialized && sharedCache.stopCh != nil {
		close(sharedCache.stopCh)

		time.Sleep(300 * time.Millisecond)

		maxWait := 1 * time.Second
		startTime := time.Now()
		for time.Since(startTime) < maxWait {
			sharedCache.RLock()
			isRunning := sharedCache.refresherRunning
			sharedCache.RUnlock()

			if !isRunning {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	sharedCache = struct {
		sync.RWMutex
		environments         map[string]*EnvironmentCache
		environmentEndpoints map[string]string
		environmentSecrets   map[string]EnvironmentSecret
		cacheDuration        time.Duration
		requestTimeout       time.Duration
		client               *http.Client
		debug                bool
		initialized          bool
		refresherRunning     bool
		stopCh               chan struct{}
		userAgent            string
		secretHeader         string
		secretHeaderValue    string
	}{}

	shutdownOnce = sync.Once{}
}

// isCIDRMatch checks if an IP is contained within a CIDR range
func isCIDRMatch(ip, cidr string) (bool, error) {
	// Parse the CIDR notation
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return false, fmt.Errorf("invalid CIDR notation %s: %v", cidr, err)
	}

	// Parse the IP
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return false, fmt.Errorf("invalid IP address %s (cannot be parsed as IPv4 or IPv6)", ip)
	}

	// Check if the IP is the correct version (IPv4/IPv6) for the CIDR
	if (ipNet.IP.To4() == nil) != (parsedIP.To4() == nil) {
		return false, fmt.Errorf("IP version mismatch: CIDR %s is %s but IP %s is %s",
			cidr,
			ipVersionName(ipNet.IP),
			ip,
			ipVersionName(parsedIP))
	}

	// Check if the IP is contained in the CIDR range
	return ipNet.Contains(parsedIP), nil
}

// ipVersionName returns a string indicating whether an IP is IPv4 or IPv6
func ipVersionName(ip net.IP) string {
	if ip.To4() != nil {
		return "IPv4"
	}
	return "IPv6"
}
