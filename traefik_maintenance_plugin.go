package traefik_maintenance_plugin

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config holds the plugin configuration
type Config struct {
	Endpoint      string `json:"endpoint,omitempty"`
	CacheDuration int    `json:"cacheDuration,omitempty"`
}

// CreateConfig creates a default config
func CreateConfig() *Config {
	return &Config{
		CacheDuration: 10, // Default cache duration in seconds
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
	next          http.Handler
	endpoint      string
	cacheDuration time.Duration
	cache         struct {
		mutex     sync.RWMutex
		isActive  bool
		whitelist []string
		expiry    time.Time
	}
}

// New creates a new MaintenanceCheck middleware
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if config.Endpoint == "" {
		return nil, errors.New("endpoint is required")
	}

	// Convert seconds to duration
	cacheDuration := time.Duration(config.CacheDuration) * time.Second

	return &MaintenanceCheck{
		next:          next,
		endpoint:      config.Endpoint,
		cacheDuration: cacheDuration,
	}, nil
}

// ServeHTTP processes incoming requests
func (m *MaintenanceCheck) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	isActive, whitelist := m.getMaintenanceStatus()

	log.Printf("Maintenance check: isActive=%v, whitelist=%v, URI=%s, Host=%s",
		isActive, whitelist, req.RequestURI, req.Host)

	// If maintenance mode is active, check whitelist
	if isActive {
		log.Printf("Maintenance mode is active for request %s", req.URL.String())
		// Check if the whitelist contains a wildcard "*"
		for _, entry := range whitelist {
			if entry == "*" {
				// Allow all users if "*" is in whitelist
				log.Printf("Wildcard entry found in whitelist, allowing request")
				m.next.ServeHTTP(rw, req)
				return
			}
		}

		// Get client IP address
		clientIP := getClientIP(req)
		log.Printf("Client IP: %s", clientIP)

		// Check if client IP is in whitelist
		if !isIPWhitelisted(clientIP, whitelist) {
			log.Printf("Client IP %s is not in whitelist, blocking request", clientIP)
			http.Error(rw, "Service is in maintenance mode", 512)
			return
		}
		log.Printf("Client IP %s is in whitelist, allowing request", clientIP)
	} else {
		log.Printf("Maintenance mode is NOT active, allowing request to %s", req.URL.String())
	}

	m.next.ServeHTTP(rw, req)
}

// getMaintenanceStatus checks the maintenance status with caching
func (m *MaintenanceCheck) getMaintenanceStatus() (bool, []string) {
	m.cache.mutex.RLock()
	if time.Now().Before(m.cache.expiry) {
		defer m.cache.mutex.RUnlock()
		log.Printf("Using cached maintenance status: isActive=%v, whitelist=%v",
			m.cache.isActive, m.cache.whitelist)
		return m.cache.isActive, m.cache.whitelist
	}
	m.cache.mutex.RUnlock()

	m.cache.mutex.Lock()
	defer m.cache.mutex.Unlock()

	log.Printf("Fetching maintenance status from %s", m.endpoint)
	resp, err := http.Get(m.endpoint)
	if err != nil {
		log.Printf("Failed to fetch maintenance status: %v", err)
		return false, nil // Default to allowing traffic in case of failure
	}
	defer resp.Body.Close()

	log.Printf("Maintenance API response status: %d", resp.StatusCode)

	var result MaintenanceResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("Failed to decode maintenance status: %v", err)
		return false, nil
	}

	log.Printf("Received maintenance status: isActive=%v, whitelist=%v",
		result.SystemConfig.Maintenance.IsActive,
		result.SystemConfig.Maintenance.Whitelist)

	m.cache.isActive = result.SystemConfig.Maintenance.IsActive
	m.cache.whitelist = result.SystemConfig.Maintenance.Whitelist
	m.cache.expiry = time.Now().Add(m.cacheDuration)

	return m.cache.isActive, m.cache.whitelist
}

// getClientIP extracts the client IP address from the request
func getClientIP(req *http.Request) string {
	// Check X-Forwarded-For header first
	xForwardedFor := req.Header.Get("X-Forwarded-For")
	if xForwardedFor != "" {
		// X-Forwarded-For can contain multiple IPs, get the first one
		ips := strings.Split(xForwardedFor, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}

	// Check X-Real-IP header
	if ip := req.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}

	// Fall back to RemoteAddr
	return strings.Split(req.RemoteAddr, ":")[0]
}

// isIPWhitelisted checks if the client IP is in the whitelist
func isIPWhitelisted(clientIP string, whitelist []string) bool {
	for _, ip := range whitelist {
		if ip == clientIP {
			return true
		}
	}
	return false
}
