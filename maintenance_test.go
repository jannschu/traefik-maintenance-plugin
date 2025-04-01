package traefik_maintenance_plugin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	plugin "github.com/CitronusAcademy/traefik-maintenance-plugin"
)

func TestMaintenanceCheck(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := struct {
			SystemConfig struct {
				Maintenance struct {
					IsActive  bool     `json:"is_active"`
					Whitelist []string `json:"whitelist,omitempty"`
				} `json:"maintenance"`
			} `json:"system_config"`
		}{}

		switch r.URL.Path {
		case "/maintenance-active":
			response.SystemConfig.Maintenance.IsActive = true
			response.SystemConfig.Maintenance.Whitelist = []string{}
		case "/maintenance-active-wildcard":
			response.SystemConfig.Maintenance.IsActive = true
			response.SystemConfig.Maintenance.Whitelist = []string{"*"}
		case "/maintenance-active-specific-ip":
			response.SystemConfig.Maintenance.IsActive = true
			response.SystemConfig.Maintenance.Whitelist = []string{"192.168.1.1"}
		default:
			response.SystemConfig.Maintenance.IsActive = false
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer ts.Close()

	tests := []struct {
		name          string
		endpoint      string
		cacheDuration int
		clientIP      string
		expectedCode  int
	}{
		{
			name:          "Maintenance inactive",
			endpoint:      ts.URL,
			cacheDuration: 10,
			clientIP:      "10.0.0.1",
			expectedCode:  http.StatusOK,
		},
		{
			name:          "Maintenance active - no whitelist",
			endpoint:      ts.URL + "/maintenance-active",
			cacheDuration: 10,
			clientIP:      "10.0.0.1",
			expectedCode:  512,
		},
		{
			name:          "Maintenance active - IP not in whitelist",
			endpoint:      ts.URL + "/maintenance-active-specific-ip",
			cacheDuration: 10,
			clientIP:      "10.0.0.1",
			expectedCode:  512,
		},
		{
			name:          "Maintenance active - IP in whitelist",
			endpoint:      ts.URL + "/maintenance-active-specific-ip",
			cacheDuration: 10,
			clientIP:      "192.168.1.1",
			expectedCode:  http.StatusOK,
		},
		{
			name:          "Maintenance active - wildcard whitelist",
			endpoint:      ts.URL + "/maintenance-active-wildcard",
			cacheDuration: 10,
			clientIP:      "10.0.0.1",
			expectedCode:  http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := plugin.CreateConfig()
			cfg.Endpoint = tt.endpoint
			cfg.CacheDuration = tt.cacheDuration

			next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				rw.WriteHeader(http.StatusOK)
			})

			handler, err := plugin.New(context.Background(), next, cfg, "maintenance-test")
			if err != nil {
				t.Fatalf("Error creating plugin: %v", err)
			}

			req := httptest.NewRequest(http.MethodGet, "http://localhost", nil)

			if tt.clientIP != "" {
				req.Header.Set("X-Forwarded-For", tt.clientIP)
				req.Header.Set("X-Real-IP", tt.clientIP)
			}

			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			response := recorder.Result()
			defer response.Body.Close()

			if response.StatusCode != tt.expectedCode {
				t.Errorf("Expected status code %d, got %d", tt.expectedCode, response.StatusCode)
			}
		})
	}
}
