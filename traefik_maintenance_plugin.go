package traefik_maintenance_plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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

type MaintenanceCheck struct {
	next                  http.Handler
	endpoint              string
	cacheDuration         time.Duration
	requestTimeout        time.Duration
	skipPrefixes          []string
	skipHosts             []string
	client                *http.Client
	maintenanceStatusCode int
	debug                 bool
	userAgent             string
	cache                 struct {
		mutex     sync.RWMutex
		isActive  bool
		whitelist []string
		expiry    time.Time
	}
	inProgress int32 // Atomic flag to prevent multiple concurrent API calls
}

func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if config.Endpoint == "" {
		return nil, errors.New("endpoint is required")
	}

	cacheDuration := time.Duration(config.CacheDurationInSeconds) * time.Second
	requestTimeout := time.Duration(config.RequestTimeoutInSeconds) * time.Second

	userAgent := fmt.Sprintf("TraefikMaintenancePlugin/%s", name)

	transport := &http.Transport{
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
	}

	client := &http.Client{
		Timeout:   requestTimeout,
		Transport: transport,
	}

	m := &MaintenanceCheck{
		next:                  next,
		endpoint:              config.Endpoint,
		cacheDuration:         cacheDuration,
		requestTimeout:        requestTimeout,
		skipPrefixes:          config.SkipPrefixes,
		skipHosts:             config.SkipHosts,
		client:                client,
		maintenanceStatusCode: config.MaintenanceStatusCode,
		debug:                 config.Debug,
		userAgent:             userAgent,
	}

	m.cache.isActive = false
	m.cache.whitelist = []string{}
	m.cache.expiry = time.Now()

	// Make initial request to warm up cache
	go func() {
		time.Sleep(100 * time.Millisecond)

		reqCtx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, config.Endpoint, nil)
		if err != nil {
			if config.Debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Cache warmup: Error creating request: %v\n", err)
			}
			return
		}

		req.Header.Set("User-Agent", userAgent)

		resp, err := client.Do(req)
		if err != nil {
			if config.Debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Cache warmup: Error making request: %v\n", err)
			}
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			if config.Debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Cache warmup: API returned status code: %d\n", resp.StatusCode)
			}
			return
		}

		var result MaintenanceResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			if config.Debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Cache warmup: Error parsing JSON: %v\n", err)
			}
			return
		}

		m.cache.mutex.Lock()
		m.cache.isActive = result.SystemConfig.Maintenance.IsActive
		m.cache.whitelist = result.SystemConfig.Maintenance.Whitelist
		m.cache.expiry = time.Now().Add(cacheDuration)
		m.cache.mutex.Unlock()

		if config.Debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Cache warmup completed successfully: active=%v, whitelist count=%d\n",
				result.SystemConfig.Maintenance.IsActive, len(result.SystemConfig.Maintenance.Whitelist))
		}
	}()

	return m, nil
}

func (m *MaintenanceCheck) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
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

	isActive, whitelist := m.getMaintenanceStatus()
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
		http.Error(rw, "Service is in maintenance mode", m.maintenanceStatusCode)
		return
	}

	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Maintenance mode is inactive, allowing request\n")
	}
	m.next.ServeHTTP(rw, req)
}

func (m *MaintenanceCheck) getMaintenanceStatus() (bool, []string) {
	m.cache.mutex.RLock()
	if time.Now().Before(m.cache.expiry) {
		defer m.cache.mutex.RUnlock()
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Using cached status (valid): active=%v, whitelist count=%d\n",
				m.cache.isActive, len(m.cache.whitelist))
		}
		return m.cache.isActive, m.cache.whitelist
	}
	m.cache.mutex.RUnlock()

	if !atomic.CompareAndSwapInt32(&m.inProgress, 0, 1) {
		m.cache.mutex.RLock()
		defer m.cache.mutex.RUnlock()
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Another request is fetching status, using cached values\n")
		}
		return m.cache.isActive, m.cache.whitelist
	}

	defer atomic.StoreInt32(&m.inProgress, 0)

	m.cache.mutex.Lock()
	defer m.cache.mutex.Unlock()

	if time.Now().Before(m.cache.expiry) {
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Cache was updated by another goroutine while waiting for lock, using fresh values\n")
		}
		return m.cache.isActive, m.cache.whitelist
	}

	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Fetching maintenance status from '%s'\n", m.endpoint)
	}

	ctx, cancel := context.WithTimeout(context.Background(), m.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.endpoint, nil)
	if err != nil {
		m.cache.expiry = time.Now().Add(m.cacheDuration / 2)
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Error creating request: %v\n", err)
		}
		return m.cache.isActive, m.cache.whitelist
	}

	req.Header.Set("User-Agent", m.userAgent)

	resp, err := m.client.Do(req)
	if err != nil {
		m.cache.expiry = time.Now().Add(m.cacheDuration / 2)
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Error making request: %v\n", err)
		}
		return m.cache.isActive, m.cache.whitelist
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		m.cache.expiry = time.Now().Add(m.cacheDuration / 2)
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] API returned status code: %d\n", resp.StatusCode)
		}
		return m.cache.isActive, m.cache.whitelist
	}

	var result MaintenanceResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		m.cache.expiry = time.Now().Add(m.cacheDuration / 2)
		if m.debug {
			fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Error parsing JSON: %v\n", err)
		}
		return m.cache.isActive, m.cache.whitelist
	}

	m.cache.isActive = result.SystemConfig.Maintenance.IsActive
	m.cache.whitelist = result.SystemConfig.Maintenance.Whitelist
	m.cache.expiry = time.Now().Add(m.cacheDuration)

	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Successfully updated maintenance status: active=%v, whitelist count=%d\n",
			m.cache.isActive, len(m.cache.whitelist))
	}

	return m.cache.isActive, m.cache.whitelist
}

func getClientIP(req *http.Request, debug bool) string {
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
	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Checking host '%s' against skipHosts: %v\n", host, m.skipHosts)
	}

	for _, skipHost := range m.skipHosts {
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
	if m.debug {
		fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Checking path '%s' against skipPrefixes: %v\n", path, m.skipPrefixes)
	}

	for _, prefix := range m.skipPrefixes {
		if strings.HasPrefix(path, prefix) {
			if m.debug {
				fmt.Fprintf(os.Stdout, "[MaintenanceCheck] Path '%s' matches prefix '%s'\n", path, prefix)
			}
			return true
		}
	}

	return false
}
