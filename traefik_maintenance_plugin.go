package traefik_maintenance_plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config holds the plugin configuration
type Config struct {
	Endpoint                string   `json:"endpoint,omitempty"`
	CacheDurationInSeconds  int      `json:"cacheDurationInSeconds,omitempty"`
	SkipPrefixes            []string `json:"skipPrefixes,omitempty"`
	RequestTimeoutInSeconds int      `json:"requestTimeoutInSeconds,omitempty"`
	MaintenanceStatusCode   int      `json:"maintenanceStatusCode,omitempty"`
}

// CreateConfig creates a default config
func CreateConfig() *Config {
	return &Config{
		CacheDurationInSeconds:  10,
		SkipPrefixes:            []string{},
		RequestTimeoutInSeconds: 5,
		MaintenanceStatusCode:   512,
	}
}

// MaintenanceResponse represents the API response
type MaintenanceResponse struct {
	SystemConfig struct {
		Maintenance struct {
			IsActive  bool     `json:"is_active"`
			Whitelist []string `json:"whitelist"`
		} `json:"maintenance"`
	} `json:"system_config"`
}

// MaintenanceCheck is a Traefik middleware plugin
type MaintenanceCheck struct {
	next                  http.Handler
	endpoint              string
	cacheDuration         time.Duration
	requestTimeout        time.Duration
	skipPrefixes          []string
	client                *http.Client
	maintenanceStatusCode int
	cache                 struct {
		mutex     sync.RWMutex
		isActive  bool
		whitelist []string
		expiry    time.Time
	}
	inProgress int32 // Atomic flag to prevent multiple concurrent API calls
}

// New creates a new MaintenanceCheck middleware
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
		client:                client,
		maintenanceStatusCode: config.MaintenanceStatusCode,
	}

	m.cache.isActive = false
	m.cache.whitelist = []string{}
	m.cache.expiry = time.Now()

	// Make initial request to warm up cache
	go func() {
		reqCtx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, config.Endpoint, nil)
		if err == nil {
			req.Header.Set("User-Agent", userAgent)
			_, _ = m.client.Do(req) // Ignore errors, just trying to warm up cache
		}
	}()

	return m, nil
}

// ServeHTTP processes incoming requests
func (m *MaintenanceCheck) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	for _, prefix := range m.skipPrefixes {
		if strings.HasPrefix(req.URL.Path, prefix) {
			m.next.ServeHTTP(rw, req)
			return
		}
	}

	isActive, whitelist := m.getMaintenanceStatus()

	if isActive {
		for _, entry := range whitelist {
			if entry == "*" {
				m.next.ServeHTTP(rw, req)
				return
			}
		}

		clientIP := getClientIP(req)

		if !isIPWhitelisted(clientIP, whitelist) {
			http.Error(rw, "Service is in maintenance mode", m.maintenanceStatusCode)
			return
		}
	}

	m.next.ServeHTTP(rw, req)
}

// getMaintenanceStatus checks the maintenance status with caching
func (m *MaintenanceCheck) getMaintenanceStatus() (bool, []string) {
	m.cache.mutex.RLock()
	if time.Now().Before(m.cache.expiry) {
		defer m.cache.mutex.RUnlock()
		return m.cache.isActive, m.cache.whitelist
	}
	m.cache.mutex.RUnlock()

	if !atomic.CompareAndSwapInt32(&m.inProgress, 0, 1) {
		m.cache.mutex.RLock()
		defer m.cache.mutex.RUnlock()
		return m.cache.isActive, m.cache.whitelist
	}

	defer atomic.StoreInt32(&m.inProgress, 0)

	m.cache.mutex.Lock()
	defer m.cache.mutex.Unlock()

	if time.Now().Before(m.cache.expiry) {
		return m.cache.isActive, m.cache.whitelist
	}

	ctx, cancel := context.WithTimeout(context.Background(), m.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.endpoint, nil)
	if err != nil {
		m.cache.expiry = time.Now().Add(m.cacheDuration / 2)
		return m.cache.isActive, m.cache.whitelist
	}

	req.Header.Set("User-Agent", "TraefikMaintenancePlugin")

	resp, err := m.client.Do(req)
	if err != nil {
		m.cache.expiry = time.Now().Add(m.cacheDuration / 2)
		return m.cache.isActive, m.cache.whitelist
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		m.cache.expiry = time.Now().Add(m.cacheDuration / 2)
		return m.cache.isActive, m.cache.whitelist
	}

	var result MaintenanceResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		m.cache.expiry = time.Now().Add(m.cacheDuration / 2)
		return m.cache.isActive, m.cache.whitelist
	}

	m.cache.isActive = result.SystemConfig.Maintenance.IsActive
	m.cache.whitelist = result.SystemConfig.Maintenance.Whitelist
	m.cache.expiry = time.Now().Add(m.cacheDuration)

	return m.cache.isActive, m.cache.whitelist
}

// getClientIP extracts the client IP address from the request
func getClientIP(req *http.Request) string {
	for _, h := range []string{"X-Forwarded-For", "X-Real-Ip"} {
		addresses := req.Header.Get(h)
		if addresses != "" {
			parts := strings.Split(addresses, ",")
			if len(parts) > 0 {
				return strings.TrimSpace(parts[0])
			}
		}
	}

	ip := req.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	return ip
}

// isIPWhitelisted checks if the client IP is in the whitelist
func isIPWhitelisted(clientIP string, whitelist []string) bool {
	if len(whitelist) == 0 {
		return false
	}

	for _, ip := range whitelist {
		if ip == clientIP {
			return true
		}
	}
	return false
}
