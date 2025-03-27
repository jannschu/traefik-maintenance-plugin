package traefik_maintenance_plugin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	plugin "github.com/CitronusAcademy/traefik-maintenance-plugin"
)

func TestMaintenanceCheck(t *testing.T) {
	// Create a test server to simulate the maintenance endpoint
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return different maintenance status based on URL path
		var isActive bool
		if r.URL.Path == "/maintenance-active" {
			isActive = true
		}

		// Create response in expected format
		response := struct {
			SystemConfig struct {
				Maintenance struct {
					IsActive bool `json:"is_active"`
				} `json:"maintenance"`
			} `json:"system_config"`
		}{}
		response.SystemConfig.Maintenance.IsActive = isActive

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer ts.Close()

	tests := []struct {
		name          string
		endpoint      string
		cacheDuration time.Duration
		expectedCode  int
	}{
		{
			name:          "Maintenance inactive",
			endpoint:      ts.URL,
			cacheDuration: 10 * time.Second,
			expectedCode:  http.StatusOK,
		},
		{
			name:          "Maintenance active",
			endpoint:      ts.URL + "/maintenance-active",
			cacheDuration: 10 * time.Second,
			expectedCode:  512, // Custom maintenance status code
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create config
			cfg := plugin.CreateConfig()
			cfg.Endpoint = tt.endpoint
			cfg.CacheDuration = tt.cacheDuration

			// Create a simple next handler that returns OK
			next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				rw.WriteHeader(http.StatusOK)
			})

			// Create handler with plugin
			handler, err := plugin.New(context.Background(), next, cfg, "maintenance-test")
			if err != nil {
				t.Fatalf("Error creating plugin: %v", err)
			}

			// Create test request
			req := httptest.NewRequest(http.MethodGet, "http://localhost", nil)

			// Record response
			recorder := httptest.NewRecorder()

			// Call ServeHTTP
			handler.ServeHTTP(recorder, req)

			// Check response
			response := recorder.Result()
			defer response.Body.Close()

			if response.StatusCode != tt.expectedCode {
				t.Errorf("Expected status code %d, got %d", tt.expectedCode, response.StatusCode)
			}
		})
	}
}
