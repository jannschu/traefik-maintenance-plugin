package traefik_maintenance_plugin

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"
)

// Config holds the plugin configuration
type Config struct {
	Endpoint      string        `json:"endpoint,omitempty"`
	CacheDuration time.Duration `json:"cacheDuration,omitempty"`
}

// CreateConfig creates a default config
func CreateConfig() *Config {
	return &Config{
		CacheDuration: 10 * time.Second, // Default cache duration
	}
}

// MaintenanceCheck is a Traefik middleware plugin
type MaintenanceCheck struct {
	next          http.Handler
	endpoint      string
	cacheDuration time.Duration
	cache         struct {
		mutex  sync.RWMutex
		value  bool
		expiry time.Time
	}
}

// New creates a new MaintenanceCheck middleware
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if config.Endpoint == "" {
		return nil, errors.New("endpoint is required")
	}

	return &MaintenanceCheck{
		next:          next,
		endpoint:      config.Endpoint,
		cacheDuration: config.CacheDuration,
	}, nil
}

// ServeHTTP processes incoming requests
func (m *MaintenanceCheck) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if m.isMaintenanceMode() {
		http.Error(rw, "Service is in maintenance mode", 512)
		return
	}
	m.next.ServeHTTP(rw, req)
}

// isMaintenanceMode checks the maintenance status with caching
func (m *MaintenanceCheck) isMaintenanceMode() bool {
	m.cache.mutex.RLock()
	if time.Now().Before(m.cache.expiry) {
		defer m.cache.mutex.RUnlock()
		return m.cache.value
	}
	m.cache.mutex.RUnlock()

	m.cache.mutex.Lock()
	defer m.cache.mutex.Unlock()

	resp, err := http.Get(m.endpoint)
	if err != nil {
		log.Printf("Failed to fetch maintenance status: %v", err)
		return false // Default to allowing traffic in case of failure
	}
	defer resp.Body.Close()

	var result struct {
		SystemConfig struct {
			Maintenance struct {
				IsActive bool `json:"is_active"`
			} `json:"maintenance"`
		} `json:"system_config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("Failed to decode maintenance status: %v", err)
		return false
	}

	m.cache.value = result.SystemConfig.Maintenance.IsActive
	m.cache.expiry = time.Now().Add(m.cacheDuration)
	return result.SystemConfig.Maintenance.IsActive
}
