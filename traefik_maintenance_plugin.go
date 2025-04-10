package traefik_maintenance_plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Initialize random source for jitter calculations
var (
	randSource = rand.New(rand.NewSource(time.Now().UnixNano()))
	randMutex  sync.Mutex // Mutex to protect randSource as it's not concurrent-safe
)

type Config struct {
	Endpoint                string   `json:"endpoint,omitempty"`
	CacheDurationInSeconds  int      `json:"cacheDurationInSeconds,omitempty"`
	SkipPrefixes            []string `json:"skipPrefixes,omitempty"`
	SkipHosts               []string `json:"skipHosts,omitempty"`
	RequestTimeoutInSeconds int      `json:"requestTimeoutInSeconds,omitempty"`
	MaintenanceStatusCode   int      `json:"maintenanceStatusCode,omitempty"`
	Debug                   bool     `json:"debug,omitempty"`
}

func CreateConfig() *Config {
	return &Config{
		CacheDurationInSeconds:  10,
		SkipPrefixes:            []string{},
		SkipHosts:               []string{},
		RequestTimeoutInSeconds: 5,
		MaintenanceStatusCode:   512,
		Debug:                   false,
	}
}

type MaintenanceResponse struct {
	SystemConfig struct {
		Maintenance struct {
			IsActive  bool     `json:"is_active"`
			Whitelist []string `json:"whitelist"`
		} `json:"maintenance"`
	} `json:"system_config"`
}

// sharedCache holds the singleton cache instance for all middleware instances
var (
	sharedCache struct {
		sync.RWMutex
		isActive            bool
		whitelist           []string
		expiry              time.Time
		endpoint            string
		cacheDuration       time.Duration
		requestTimeout      time.Duration
		client              *http.Client
		debug               bool
		initialized         bool
		refresherRunning    bool
		refreshInProgress   atomic.Bool // Use atomic for faster checks without locks
		stopCh              chan struct{}
		userAgent           string
		failedAttempts      int       // Track failed attempts for exponential backoff
		lastSuccessfulFetch time.Time // Track when we last had a successful fetch
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
}

func ensureSharedCacheInitialized(endpoint string, cacheDuration, requestTimeout time.Duration, debug bool, userAgent string) {
	// Fast check without taking the lock
	if sharedCache.initialized && sharedCache.endpoint == endpoint {
		return
	}

	initLock.Lock()
	defer initLock.Unlock()

	if sharedCache.initialized && sharedCache.endpoint == endpoint {
		return
	}

	// Validate inputs before proceeding
	if endpoint == "" {
		if debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Error: empty endpoint provided\n")
		}
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

	// Proper cleanup of previous refresher if configuration changes
	var wg sync.WaitGroup
	if sharedCache.initialized && sharedCache.refresherRunning && sharedCache.stopCh != nil {
		wg.Add(1)
		oldStopCh := sharedCache.stopCh

		// Create a new stopCh before closing the old one
		sharedCache.stopCh = make(chan struct{})

		// Set a flag to track shutdown in progress
		sharedCache.Lock()
		shutdownInProgress := true
		sharedCache.Unlock()

		// Close the old channel to signal the refresher to stop
		close(oldStopCh)

		// Wait for the refresher to acknowledge shutdown with a timeout
		go func() {
			// Give the refresher time to shut down gracefully
			shutdownTimer := time.NewTimer(500 * time.Millisecond)
			defer shutdownTimer.Stop()

			<-shutdownTimer.C

			// Check if refresher is still running after timeout
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

	// Update configuration under lock
	sharedCache.Lock()
	sharedCache.client = client
	sharedCache.endpoint = endpoint
	sharedCache.cacheDuration = cacheDuration
	sharedCache.requestTimeout = requestTimeout
	sharedCache.debug = debug
	sharedCache.userAgent = userAgent
	sharedCache.initialized = true
	sharedCache.refresherRunning = false
	sharedCache.refreshInProgress.Store(false)
	sharedCache.failedAttempts = 0
	sharedCache.expiry = time.Now().Add(-1 * time.Minute)
	sharedCache.lastSuccessfulFetch = time.Time{} // Zero time
	sharedCache.Unlock()

	// Perform initial fetch exactly once
	go func() {
		if debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Scheduling initial fetch for endpoint '%s'\n", endpoint)
		}

		// Use exponential backoff for the first fetch
		var retryDelay time.Duration = 100 * time.Millisecond
		for i := 0; i < 5; i++ { // Try up to 5 times
			if refreshMaintenanceStatus() {
				break // Success, exit retry loop
			}

			// Failed, wait and retry
			if debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Initial fetch failed, retrying in %v\n", retryDelay)
			}
			select {
			case <-sharedCache.stopCh:
				// Stop retrying if shutdown requested
				return
			case <-time.After(retryDelay):
				// Continue with retry
			}
			retryDelay *= 2 // Exponential backoff
		}
	}()

	// Start background refresher
	startBackgroundRefresher()
}

func startBackgroundRefresher() {
	// Fast check without lock first
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

	// Start the refresher in a goroutine
	go func() {
		ticker := time.NewTicker(cacheDuration)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				sharedCache.RLock()
				lastFetch := sharedCache.lastSuccessfulFetch
				sharedCache.RUnlock()

				if !lastFetch.IsZero() && time.Since(lastFetch) > cacheDuration*10 {
					fmt.Fprintf(os.Stdout, "[MaintenanceCheck] WARNING: No successful fetch in %v, service may be unavailable\n",
						time.Since(lastFetch))
				}

				refreshMaintenanceStatus()
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

// refreshMaintenanceStatus returns true if refresh was successful, false otherwise
func refreshMaintenanceStatus() bool {
	// Fast path check - use atomic operation instead of locks for better performance
	if sharedCache.refreshInProgress.Load() {
		return true // Someone else is already refreshing, consider it successful
	}

	// Another quick check if we even need to refresh
	sharedCache.RLock()
	needsRefresh := time.Now().After(sharedCache.expiry)
	debug := sharedCache.debug
	sharedCache.RUnlock()

	if !needsRefresh {
		return true
	}

	// Try to acquire refresh lock - only one goroutine will succeed
	if !refreshLock.TryLock() {
		if debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Another goroutine is already refreshing, skipping\n")
		}
		return true // Someone else is already refreshing, consider it successful
	}

	// Set the atomic flag IMMEDIATELY before any other operations
	// This will block other threads at the fast path check above
	sharedCache.refreshInProgress.Store(true)

	// Release the lock at the end of this function
	defer func() {
		sharedCache.refreshInProgress.Store(false)
		refreshLock.Unlock()
	}()

	// Double-check after acquiring lock
	sharedCache.RLock()
	stillNeedsRefresh := time.Now().After(sharedCache.expiry)
	endpoint := sharedCache.endpoint
	client := sharedCache.client
	requestTimeout := sharedCache.requestTimeout
	userAgent := sharedCache.userAgent
	cacheDuration := sharedCache.cacheDuration
	currentFailedAttempts := sharedCache.failedAttempts
	sharedCache.RUnlock()

	if !stillNeedsRefresh {
		if debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Refresh no longer needed after acquiring lock\n")
		}
		return true // No refresh needed, consider it successful
	}

	if debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Fetching maintenance status from '%s'\n", endpoint)
	}

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

		// Use exponential backoff for retries on failure
		backoffTime := calculateBackoff(currentFailedAttempts)

		sharedCache.Lock()
		sharedCache.failedAttempts++
		sharedCache.expiry = time.Now().Add(backoffTime)
		sharedCache.Unlock()

		if debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Using backoff: %v, next retry at %v\n",
				backoffTime, time.Now().Add(backoffTime))
		}

		return false
	}

	req.Header.Set("User-Agent", userAgent)

	var resp *http.Response
	resp, err = client.Do(req)
	if err != nil {
		if debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Error making request: %v\n", err)
		}

		// Use exponential backoff for retries on failure
		backoffTime := calculateBackoff(currentFailedAttempts)

		sharedCache.Lock()
		sharedCache.failedAttempts++
		sharedCache.expiry = time.Now().Add(backoffTime)
		sharedCache.Unlock()

		if debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Using backoff: %v, next retry at %v\n",
				backoffTime, time.Now().Add(backoffTime))
		}

		return false
	}

	// Ensure body is always closed
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

		sharedCache.Lock()
		sharedCache.failedAttempts++
		sharedCache.expiry = time.Now().Add(backoffTime)
		sharedCache.Unlock()

		return false
	}

	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("API returned status code: %d", resp.StatusCode)
		if debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] %v\n", err)
		}

		// Use exponential backoff for retries on failure
		backoffTime := calculateBackoff(currentFailedAttempts)

		sharedCache.Lock()
		sharedCache.failedAttempts++
		sharedCache.expiry = time.Now().Add(backoffTime)
		sharedCache.Unlock()

		if debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Using backoff: %v, next retry at %v\n",
				backoffTime, time.Now().Add(backoffTime))
		}

		return false
	}

	// Limit size of response body to prevent memory exhaustion
	// Usually JSON responses are small, but adding protection against DoS
	const maxResponseSize = 10 * 1024 * 1024 // 10 MB
	limitedReader := http.MaxBytesReader(nil, resp.Body, maxResponseSize)

	// Parse response
	var result MaintenanceResponse
	decoder := json.NewDecoder(limitedReader)
	if err := decoder.Decode(&result); err != nil {
		if debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Error parsing JSON: %v\n", err)
		}

		// Use exponential backoff for retries on failure
		backoffTime := calculateBackoff(currentFailedAttempts)

		sharedCache.Lock()
		sharedCache.failedAttempts++
		sharedCache.expiry = time.Now().Add(backoffTime)
		sharedCache.Unlock()

		if debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Using backoff: %v, next retry at %v\n",
				backoffTime, time.Now().Add(backoffTime))
		}

		return false
	}

	isActive := result.SystemConfig.Maintenance.IsActive
	whitelist := result.SystemConfig.Maintenance.Whitelist

	// Make a copy of the whitelist to avoid potential race conditions
	// if the original slice is modified externally
	whitelistCopy := make([]string, len(whitelist))
	copy(whitelistCopy, whitelist)

	sharedCache.Lock()
	sharedCache.isActive = isActive
	sharedCache.whitelist = whitelistCopy
	sharedCache.expiry = time.Now().Add(cacheDuration)
	sharedCache.failedAttempts = 0 // Reset failed attempts counter on success
	sharedCache.lastSuccessfulFetch = time.Now()
	sharedCache.Unlock()

	if debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Successfully updated shared maintenance status: active=%v, whitelist count=%d\n",
			isActive, len(whitelist))
	}

	return true
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

func getMaintenanceStatus() (bool, []string) {
	sharedCache.RLock()
	defer sharedCache.RUnlock()

	if !sharedCache.initialized {
		return false, []string{}
	}

	if sharedCache.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Using shared cached status: active=%v, whitelist count=%d\n",
			sharedCache.isActive, len(sharedCache.whitelist))
	}

	// Create a copy of the whitelist to protect against potential race conditions
	whitelistCopy := make([]string, len(sharedCache.whitelist))
	copy(whitelistCopy, sharedCache.whitelist)

	return sharedCache.isActive, whitelistCopy
}

func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if config.Endpoint == "" {
		return nil, errors.New("endpoint is required")
	}

	if config.MaintenanceStatusCode < 100 || config.MaintenanceStatusCode > 599 {
		return nil, fmt.Errorf("invalid maintenance status code: %d (must be between 100-599)",
			config.MaintenanceStatusCode)
	}

	cacheDuration := time.Duration(config.CacheDurationInSeconds) * time.Second
	requestTimeout := time.Duration(config.RequestTimeoutInSeconds) * time.Second
	userAgent := fmt.Sprintf("TraefikMaintenancePlugin/%s", name)

	// Ensure cache duration and request timeout are sane
	if cacheDuration <= 0 {
		cacheDuration = 10 * time.Second
	}

	if requestTimeout <= 0 {
		requestTimeout = 5 * time.Second
	}

	// Initialize the shared cache if needed
	ensureSharedCacheInitialized(config.Endpoint, cacheDuration, requestTimeout, config.Debug, userAgent)

	// Make deep copies of slices to prevent modifications
	skipPrefixesCopy := make([]string, len(config.SkipPrefixes))
	copy(skipPrefixesCopy, config.SkipPrefixes)

	skipHostsCopy := make([]string, len(config.SkipHosts))
	copy(skipHostsCopy, config.SkipHosts)

	m := &MaintenanceCheck{
		next:                  next,
		skipPrefixes:          skipPrefixesCopy,
		skipHosts:             skipHostsCopy,
		maintenanceStatusCode: config.MaintenanceStatusCode,
		debug:                 config.Debug,
	}

	// Cleanup handler
	go func() {
		<-ctx.Done()
		if config.Debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Context cancelled for middleware instance\n")
		}
	}()

	return m, nil
}

func (m *MaintenanceCheck) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	// Guard against nil request to prevent panic
	if req == nil {
		http.Error(rw, "Bad Request: nil request received", http.StatusBadRequest)
		return
	}

	// Normalize host by removing port if present (e.g., "example.com:8080" -> "example.com")
	originalHost := req.Host
	host := originalHost
	if colonIndex := strings.IndexByte(host, ':'); colonIndex > 0 {
		host = host[:colonIndex]
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Normalized host from '%s' to '%s'\n", originalHost, host)
		}
	}

	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Evaluating request: host=%s, path=%s\n", host, req.URL.Path)
	}

	if m.isHostSkipped(host) {
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Host '%s' is in skip list, bypassing maintenance check\n", host)
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

	isActive, whitelist := getMaintenanceStatus()
	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Maintenance status: active=%v, whitelist=%v\n", isActive, whitelist)
	}

	if isActive {
		if m.isClientAllowed(req, whitelist) {
			m.next.ServeHTTP(rw, req)
			return
		}

		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Access denied, returning status code %d\n",
				m.maintenanceStatusCode)
		}

		// Set content type for better client compatibility
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		rw.WriteHeader(m.maintenanceStatusCode)
		_, _ = rw.Write([]byte("Service is in maintenance mode"))
		return
	}

	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Maintenance mode is inactive, allowing request\n")
	}
	m.next.ServeHTTP(rw, req)
}

func getClientIP(req *http.Request, debug bool) string {
	// Guard against nil request
	if req == nil {
		return ""
	}

	for _, h := range []string{"X-Forwarded-For", "X-Real-Ip"} {
		addresses := req.Header.Get(h)
		if addresses != "" {
			parts := strings.Split(addresses, ",")
			if len(parts) > 0 {
				ip := strings.TrimSpace(parts[0])
				if debug {
					fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Extracted IP from %s header: %s\n", h, ip)
				}
				return ip
			}
		}
	}

	ip := req.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	if debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Using RemoteAddr IP: %s\n", ip)
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

	for _, entry := range whitelist {
		if entry == "*" {
			if m.debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Wildcard (*) found in whitelist, allowing request\n")
			}
			return true
		}
	}

	clientIP := getClientIP(req, m.debug)

	// Invalid IP address
	if clientIP == "" {
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Could not determine client IP, blocking request\n")
		}
		return false
	}

	for _, ip := range whitelist {
		if ip == clientIP {
			if m.debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] IP '%s' in whitelist, allowing request\n", clientIP)
			}
			return true
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
