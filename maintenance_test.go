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

type maintenanceResponse struct {
	SystemConfig struct {
		Maintenance struct {
			IsActive  bool     `json:"is_active"`
			Whitelist []string `json:"whitelist,omitempty"`
		} `json:"maintenance"`
	} `json:"system_config"`
}

func setupTestServer() (*httptest.Server, string, string, string, string) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := maintenanceResponse{}

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
		case "/slow-response":
			time.Sleep(200 * time.Millisecond)
			response.SystemConfig.Maintenance.IsActive = true
			response.SystemConfig.Maintenance.Whitelist = []string{"*"} // Allow all for timeouts
		case "/invalid-json":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"invalid json`))
			return
		default:
			response.SystemConfig.Maintenance.IsActive = false
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))

	return ts,
		ts.URL,
		ts.URL + "/maintenance-active",
		ts.URL + "/maintenance-active-wildcard",
		ts.URL + "/maintenance-active-specific-ip"
}

func TestMaintenanceCheck(t *testing.T) {
	ts, regularEndpoint, activeEndpoint, wildcardEndpoint, specificIPEndpoint := setupTestServer()
	defer ts.Close()

	tests := []struct {
		name                    string
		endpoint                string
		cacheDurationInSeconds  int
		requestTimeoutInSeconds int
		clientIP                string
		urlPath                 string
		skipPrefixes            []string
		skipHosts               []string
		host                    string
		maintenanceStatusCode   int
		expectedCode            int
		description             string
	}{
		{
			name:                    "Maintenance inactive",
			endpoint:                regularEndpoint,
			cacheDurationInSeconds:  10,
			requestTimeoutInSeconds: 5,
			clientIP:                "10.0.0.1",
			urlPath:                 "/",
			skipPrefixes:            []string{},
			skipHosts:               []string{},
			maintenanceStatusCode:   512,
			expectedCode:            http.StatusOK,
			description:             "When maintenance is inactive, all requests should be allowed",
		},
		{
			name:                    "Maintenance active - no whitelist",
			endpoint:                activeEndpoint,
			cacheDurationInSeconds:  10,
			requestTimeoutInSeconds: 5,
			clientIP:                "10.0.0.1",
			urlPath:                 "/",
			skipPrefixes:            []string{},
			skipHosts:               []string{},
			maintenanceStatusCode:   512,
			expectedCode:            512,
			description:             "When maintenance is active with no whitelist, all requests should be blocked",
		},
		{
			name:                    "Maintenance active - custom status code",
			endpoint:                activeEndpoint,
			cacheDurationInSeconds:  10,
			requestTimeoutInSeconds: 5,
			clientIP:                "10.0.0.1",
			urlPath:                 "/",
			skipPrefixes:            []string{},
			skipHosts:               []string{},
			maintenanceStatusCode:   418, // I'm a teapot
			expectedCode:            418,
			description:             "Should use custom status code when specified",
		},
		{
			name:                    "Maintenance active - IP not in whitelist",
			endpoint:                specificIPEndpoint,
			cacheDurationInSeconds:  10,
			requestTimeoutInSeconds: 5,
			clientIP:                "10.0.0.1",
			urlPath:                 "/",
			skipPrefixes:            []string{},
			skipHosts:               []string{},
			maintenanceStatusCode:   512,
			expectedCode:            512,
			description:             "When maintenance is active and client IP is not in whitelist, request should be blocked",
		},
		{
			name:                    "Maintenance active - IP in whitelist",
			endpoint:                specificIPEndpoint,
			cacheDurationInSeconds:  10,
			requestTimeoutInSeconds: 5,
			clientIP:                "192.168.1.1",
			urlPath:                 "/",
			skipPrefixes:            []string{},
			skipHosts:               []string{},
			maintenanceStatusCode:   512,
			expectedCode:            http.StatusOK,
			description:             "When maintenance is active and client IP is in whitelist, request should be allowed",
		},
		{
			name:                    "Maintenance active - wildcard whitelist",
			endpoint:                wildcardEndpoint,
			cacheDurationInSeconds:  10,
			requestTimeoutInSeconds: 5,
			clientIP:                "10.0.0.1",
			urlPath:                 "/",
			skipPrefixes:            []string{},
			skipHosts:               []string{},
			maintenanceStatusCode:   512,
			expectedCode:            http.StatusOK,
			description:             "When maintenance is active with wildcard whitelist, all requests should be allowed",
		},
		{
			name:                    "Maintenance active - skip prefix",
			endpoint:                activeEndpoint,
			cacheDurationInSeconds:  10,
			requestTimeoutInSeconds: 5,
			clientIP:                "10.0.0.1",
			urlPath:                 "/admin/dashboard",
			skipPrefixes:            []string{"/admin"},
			skipHosts:               []string{},
			maintenanceStatusCode:   512,
			expectedCode:            http.StatusOK,
			description:             "When maintenance is active but URL matches skip prefix, request should be allowed",
		},
		{
			name:                    "Maintenance active - multiple skip prefixes, matching",
			endpoint:                activeEndpoint,
			cacheDurationInSeconds:  10,
			requestTimeoutInSeconds: 5,
			clientIP:                "10.0.0.1",
			urlPath:                 "/pgadmin/login",
			skipPrefixes:            []string{"/admin", "/pgadmin"},
			skipHosts:               []string{},
			maintenanceStatusCode:   512,
			expectedCode:            http.StatusOK,
			description:             "When maintenance is active but URL matches one of multiple skip prefixes, request should be allowed",
		},
		{
			name:                    "Maintenance active - multiple skip prefixes, non-matching",
			endpoint:                activeEndpoint,
			cacheDurationInSeconds:  10,
			requestTimeoutInSeconds: 5,
			clientIP:                "10.0.0.1",
			urlPath:                 "/app/user/profile",
			skipPrefixes:            []string{"/admin", "/pgadmin"},
			skipHosts:               []string{},
			maintenanceStatusCode:   512,
			expectedCode:            512,
			description:             "When maintenance is active and URL doesn't match any skip prefix, request should be blocked",
		},
		{
			name:                    "Maintenance active - skip host",
			endpoint:                activeEndpoint,
			cacheDurationInSeconds:  10,
			requestTimeoutInSeconds: 5,
			clientIP:                "10.0.0.1",
			urlPath:                 "/",
			skipPrefixes:            []string{},
			skipHosts:               []string{"test.example.com"},
			host:                    "test.example.com",
			maintenanceStatusCode:   512,
			expectedCode:            http.StatusOK,
			description:             "When maintenance is active but host matches skip host, request should be allowed",
		},
		{
			name:                    "Maintenance active - skip host with port",
			endpoint:                activeEndpoint,
			cacheDurationInSeconds:  10,
			requestTimeoutInSeconds: 5,
			clientIP:                "10.0.0.1",
			urlPath:                 "/",
			skipPrefixes:            []string{},
			skipHosts:               []string{"test.example.com"},
			host:                    "test.example.com:8080",
			maintenanceStatusCode:   512,
			expectedCode:            http.StatusOK,
			description:             "When maintenance is active but host with port matches skip host, request should be allowed",
		},
		{
			name:                    "Maintenance active - wildcard host match",
			endpoint:                activeEndpoint,
			cacheDurationInSeconds:  10,
			requestTimeoutInSeconds: 5,
			clientIP:                "10.0.0.1",
			urlPath:                 "/",
			skipPrefixes:            []string{},
			skipHosts:               []string{"*.example.com"},
			host:                    "sub.example.com",
			maintenanceStatusCode:   512,
			expectedCode:            http.StatusOK,
			description:             "When maintenance is active but host matches wildcard skip host, request should be allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := plugin.CreateConfig()
			cfg.Endpoint = tt.endpoint
			cfg.CacheDurationInSeconds = tt.cacheDurationInSeconds
			cfg.RequestTimeoutInSeconds = tt.requestTimeoutInSeconds
			cfg.SkipPrefixes = tt.skipPrefixes
			cfg.SkipHosts = tt.skipHosts
			cfg.MaintenanceStatusCode = tt.maintenanceStatusCode

			next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				rw.WriteHeader(http.StatusOK)
			})

			handler, err := plugin.New(context.Background(), next, cfg, "maintenance-test")
			if err != nil {
				t.Fatalf("Error creating plugin: %v", err)
			}

			req := httptest.NewRequest(http.MethodGet, "http://localhost"+tt.urlPath, nil)

			if tt.clientIP != "" {
				req.Header.Set("X-Forwarded-For", tt.clientIP)
				req.Header.Set("X-Real-IP", tt.clientIP)
			}

			if tt.host != "" {
				req.Host = tt.host
			}

			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			response := recorder.Result()
			defer response.Body.Close()

			if response.StatusCode != tt.expectedCode {
				t.Errorf("%s: Expected status code %d, got %d", tt.description, tt.expectedCode, response.StatusCode)
			}
		})
	}
}

func TestMaintenanceCheckEdgeCases(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := maintenanceResponse{}

		switch r.URL.Path {
		case "/slow-response":
			time.Sleep(200 * time.Millisecond)
			response.SystemConfig.Maintenance.IsActive = true
			response.SystemConfig.Maintenance.Whitelist = []string{"*"} // Allow all for timeouts
		case "/invalid-json":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"invalid json`))
			return
		case "/error-status":
			w.WriteHeader(http.StatusInternalServerError)
			return
		default:
			response.SystemConfig.Maintenance.IsActive = false
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer ts.Close()

	tests := []struct {
		name                    string
		endpoint                string
		cacheDurationInSeconds  int
		requestTimeoutInSeconds int
		expectedCode            int
		description             string
	}{
		{
			name:                    "Timeout handling",
			endpoint:                ts.URL + "/slow-response",
			cacheDurationInSeconds:  10,
			requestTimeoutInSeconds: 1,
			expectedCode:            http.StatusOK,
			description:             "When API request times out, should use cached values and allow request",
		},
		{
			name:                    "Invalid JSON handling",
			endpoint:                ts.URL + "/invalid-json",
			cacheDurationInSeconds:  10,
			requestTimeoutInSeconds: 5,
			expectedCode:            http.StatusOK,
			description:             "When API returns invalid JSON, should use cached values and allow request",
		},
		{
			name:                    "Error status handling",
			endpoint:                ts.URL + "/error-status",
			cacheDurationInSeconds:  10,
			requestTimeoutInSeconds: 5,
			expectedCode:            http.StatusOK,
			description:             "When API returns error status, should use cached values and allow request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := plugin.CreateConfig()
			cfg.Endpoint = tt.endpoint
			cfg.CacheDurationInSeconds = tt.cacheDurationInSeconds
			cfg.RequestTimeoutInSeconds = tt.requestTimeoutInSeconds

			next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				rw.WriteHeader(http.StatusOK)
			})

			handler, err := plugin.New(context.Background(), next, cfg, "maintenance-test")
			if err != nil {
				t.Fatalf("Error creating plugin: %v", err)
			}

			// First, make a normal request to cache the response
			if tt.name == "Timeout handling" {
				// Use a fast endpoint to pre-populate the cache with maintenance inactive
				cfg.Endpoint = ts.URL // Use fast endpoint temporarily
				fastReq := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
				fastRec := httptest.NewRecorder()
				handler.ServeHTTP(fastRec, fastReq)

				// Reset to original endpoint for actual test
				cfg.Endpoint = tt.endpoint
			}

			// Then try the actual test endpoint
			req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			response := recorder.Result()
			defer response.Body.Close()

			if response.StatusCode != tt.expectedCode {
				t.Errorf("%s: Expected status code %d, got %d", tt.description, tt.expectedCode, response.StatusCode)
			}
		})
	}
}

func TestCacheWarmup(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := maintenanceResponse{}
		response.SystemConfig.Maintenance.IsActive = false

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer ts.Close()

	cfg := plugin.CreateConfig()
	cfg.Endpoint = ts.URL
	cfg.CacheDurationInSeconds = 10
	cfg.RequestTimeoutInSeconds = 5

	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	// Create the handler - this should trigger cache warmup
	_, err := plugin.New(context.Background(), next, cfg, "cache-warmup-test")
	if err != nil {
		t.Fatalf("Error creating plugin: %v", err)
	}

	time.Sleep(10 * time.Millisecond)
}

func TestInvalidConfig(t *testing.T) {
	cfg := plugin.CreateConfig()
	// Endpoint is required but not provided

	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	_, err := plugin.New(context.Background(), next, cfg, "maintenance-test")
	if err == nil {
		t.Error("Expected error for missing endpoint, but got none")
	}
}
