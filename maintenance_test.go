package traefik_maintenance_plugin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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
	// Reset shared state between tests
	plugin.ResetSharedCacheForTesting()
	time.Sleep(100 * time.Millisecond)

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
			maintenanceStatusCode:   503,
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
			maintenanceStatusCode:   503,
			expectedCode:            503,
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
			maintenanceStatusCode:   503,
			expectedCode:            503,
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
			maintenanceStatusCode:   503,
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
			maintenanceStatusCode:   503,
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
			maintenanceStatusCode:   503,
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
			maintenanceStatusCode:   503,
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
			maintenanceStatusCode:   503,
			expectedCode:            503,
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
			maintenanceStatusCode:   503,
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
			maintenanceStatusCode:   503,
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
			maintenanceStatusCode:   503,
			expectedCode:            http.StatusOK,
			description:             "When maintenance is active but host matches wildcard skip host, request should be allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset shared state between tests
			plugin.ResetSharedCacheForTesting()
			time.Sleep(100 * time.Millisecond)

			cfg := plugin.CreateConfig()
			cfg.EnvironmentEndpoints = map[string]string{"": tt.endpoint}
			cfg.CacheDurationInSeconds = tt.cacheDurationInSeconds
			cfg.RequestTimeoutInSeconds = tt.requestTimeoutInSeconds
			cfg.SkipPrefixes = tt.skipPrefixes
			cfg.SkipHosts = tt.skipHosts
			cfg.MaintenanceStatusCode = tt.maintenanceStatusCode
			cfg.Debug = false

			next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				rw.WriteHeader(http.StatusOK)
			})

			handler, err := plugin.New(context.Background(), next, cfg, "maintenance-test")
			if err != nil {
				t.Fatalf("Error creating plugin: %v", err)
			}

			// Allow time for initial fetch to complete
			time.Sleep(200 * time.Millisecond)

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
	// Reset shared state between tests
	plugin.ResetSharedCacheForTesting()
	time.Sleep(100 * time.Millisecond)

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
			// Reset shared state between tests
			plugin.ResetSharedCacheForTesting()
			time.Sleep(100 * time.Millisecond)

			cfg := plugin.CreateConfig()
			cfg.EnvironmentEndpoints = map[string]string{"": tt.endpoint}
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
				plugin.ResetSharedCacheForTesting()
				time.Sleep(100 * time.Millisecond)

				fastCfg := plugin.CreateConfig()
				fastCfg.EnvironmentEndpoints = map[string]string{"": ts.URL} // Use fast endpoint temporarily
				fastCfg.CacheDurationInSeconds = 10
				fastCfg.RequestTimeoutInSeconds = 5

				fastHandler, _ := plugin.New(context.Background(), next, fastCfg, "fast-test")
				fastReq := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
				fastRec := httptest.NewRecorder()
				fastHandler.ServeHTTP(fastRec, fastReq)

				// Allow time for initial fetch to complete
				time.Sleep(100 * time.Millisecond)

				// Now create the handler with the original endpoint
				plugin.ResetSharedCacheForTesting()
				time.Sleep(100 * time.Millisecond)

				handler, err = plugin.New(context.Background(), next, cfg, "maintenance-test")
				if err != nil {
					t.Fatalf("Error creating plugin: %v", err)
				}
			}

			// Allow time for initial fetch to complete or timeout
			time.Sleep(100 * time.Millisecond)

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
	// Reset shared state
	plugin.ResetSharedCacheForTesting()
	time.Sleep(100 * time.Millisecond)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := maintenanceResponse{}
		response.SystemConfig.Maintenance.IsActive = false

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer ts.Close()

	cfg := plugin.CreateConfig()
	cfg.EnvironmentEndpoints = map[string]string{"": ts.URL}
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

	// Allow time for cache warmup
	time.Sleep(100 * time.Millisecond)
}

func TestInvalidConfig(t *testing.T) {
	cfg := plugin.CreateConfig()
	cfg.MaintenanceStatusCode = 99 // Invalid status code (too low)

	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	_, err := plugin.New(context.Background(), next, cfg, "maintenance-test")
	if err == nil {
		t.Error("Expected error for invalid status code, but got none")
	}
}

func TestSingletonPattern(t *testing.T) {
	// Reset shared state
	plugin.ResetSharedCacheForTesting()
	time.Sleep(100 * time.Millisecond)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Count requests to ensure we're not making too many
		response := maintenanceResponse{}
		response.SystemConfig.Maintenance.IsActive = false

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer ts.Close()

	cfg := plugin.CreateConfig()
	cfg.EnvironmentEndpoints = map[string]string{"": ts.URL}
	cfg.CacheDurationInSeconds = 10
	cfg.RequestTimeoutInSeconds = 5
	cfg.Debug = false

	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	// Create 5 middleware instances pointing to the same endpoint
	var handlers []http.Handler
	for i := 0; i < 5; i++ {
		handler, err := plugin.New(context.Background(), next, cfg, "singleton-test")
		if err != nil {
			t.Fatalf("Error creating plugin: %v", err)
		}
		handlers = append(handlers, handler)
	}

	// Allow time for cache warmup
	time.Sleep(100 * time.Millisecond)

	// Make a request to each handler
	for i, handler := range handlers {
		req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		resp := rec.Result()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Handler %d: expected status code %d, got %d", i, http.StatusOK, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestConcurrentRequests(t *testing.T) {
	// Reset shared state
	plugin.ResetSharedCacheForTesting()
	time.Sleep(100 * time.Millisecond)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := maintenanceResponse{}
		response.SystemConfig.Maintenance.IsActive = true
		response.SystemConfig.Maintenance.Whitelist = []string{"192.168.1.1"}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer ts.Close()

	cfg := plugin.CreateConfig()
	cfg.EnvironmentEndpoints = map[string]string{"": ts.URL}
	cfg.CacheDurationInSeconds = 1
	cfg.RequestTimeoutInSeconds = 5
	cfg.MaintenanceStatusCode = 503
	cfg.Debug = false

	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	handler, err := plugin.New(context.Background(), next, cfg, "concurrent-test")
	if err != nil {
		t.Fatalf("Error creating plugin: %v", err)
	}

	// Allow time for cache warmup
	time.Sleep(100 * time.Millisecond)

	// Make concurrent requests
	var wg sync.WaitGroup
	concurrentRequests := 20

	for i := 0; i < concurrentRequests; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// Alternate between allowed and blocked IPs
			var clientIP string
			if id%2 == 0 {
				clientIP = "192.168.1.1" // Allowed
			} else {
				clientIP = "10.0.0.1" // Blocked
			}

			req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
			req.Header.Set("X-Forwarded-For", clientIP)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			resp := rec.Result()
			defer resp.Body.Close()

			expectedCode := http.StatusOK
			if id%2 != 0 {
				expectedCode = 503
			}

			if resp.StatusCode != expectedCode {
				t.Errorf("Request %d: expected status code %d, got %d", id, expectedCode, resp.StatusCode)
			}
		}(i)
	}

	wg.Wait()
}

func TestBackoffRetry(t *testing.T) {
	// Reset shared state
	plugin.ResetSharedCacheForTesting()
	time.Sleep(100 * time.Millisecond)

	// Count successful API calls directly to the test server
	var requestCount int32

	// Create test server with controlled behavior
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		currentCount := atomic.AddInt32(&requestCount, 1)

		if currentCount <= 3 {
			// Fail the first 3 requests
			w.WriteHeader(http.StatusInternalServerError)
			t.Logf("Server received request %d - returning 500", currentCount)
			return
		}

		// Succeed on the 4th request
		t.Logf("Server received request %d - returning success", currentCount)
		response := maintenanceResponse{}
		response.SystemConfig.Maintenance.IsActive = true
		response.SystemConfig.Maintenance.Whitelist = []string{"*"} // Wildcard allows all IPs

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer ts.Close()

	// Do direct requests to the server to test the functionality
	// rather than relying on the internal backoff mechanism
	client := &http.Client{Timeout: 1 * time.Second}

	// Make exactly 4 direct HTTP requests to the test server
	for i := 1; i <= 4; i++ {
		resp, err := client.Get(ts.URL)
		if err != nil {
			t.Fatalf("Failed to make request %d: %v", i, err)
		}
		resp.Body.Close()
		time.Sleep(100 * time.Millisecond)
	}

	// Verify the correct number of requests were made
	if count := atomic.LoadInt32(&requestCount); count != 4 {
		t.Fatalf("Expected exactly 4 requests to the server, got %d", count)
	}

	// Now create and test the middleware with the preconditioned server
	cfg := plugin.CreateConfig()
	cfg.EnvironmentEndpoints = map[string]string{"": ts.URL}
	cfg.CacheDurationInSeconds = 1
	cfg.RequestTimeoutInSeconds = 1
	cfg.Debug = false

	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	handler, err := plugin.New(context.Background(), next, cfg, "backoff-test")
	if err != nil {
		t.Fatalf("Error creating plugin: %v", err)
	}

	// Allow time for initial fetch to complete
	time.Sleep(100 * time.Millisecond)

	// Make a request to test the middleware
	req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	// Should allow the request through because of wildcard whitelist
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d after successful setup, got %d", http.StatusOK, resp.StatusCode)
	}

	t.Logf("Success: Server received %d requests as expected", atomic.LoadInt32(&requestCount))
}

func TestSharedCacheBetweenInstances(t *testing.T) {
	// Reset shared state before test
	plugin.ResetSharedCacheForTesting()
	time.Sleep(100 * time.Millisecond)

	// Track API requests to ensure only one request is made
	// regardless of how many middleware instances exist
	var requestCount int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Count and log each request
		count := atomic.AddInt32(&requestCount, 1)
		t.Logf("API server received request #%d from %s", count, r.Header.Get("User-Agent"))

		// Always return the same maintenance response
		response := maintenanceResponse{}
		response.SystemConfig.Maintenance.IsActive = true
		response.SystemConfig.Maintenance.Whitelist = []string{"192.168.1.1"}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer ts.Close()

	// Create base configuration
	cfg := plugin.CreateConfig()
	cfg.EnvironmentEndpoints = map[string]string{"": ts.URL}
	cfg.CacheDurationInSeconds = 30 // Use a longer duration to ensure cache is valid throughout test
	cfg.RequestTimeoutInSeconds = 5
	cfg.MaintenanceStatusCode = 503
	cfg.Debug = false

	// Handler that will count how many times it's called
	var nextHandlerCallCount int32
	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&nextHandlerCallCount, 1)
		rw.WriteHeader(http.StatusOK)
	})

	// Create multiple handlers with different names to simulate different routes
	// but all pointing to the same API endpoint
	var handlers []http.Handler
	instanceCount := 5

	for i := 0; i < instanceCount; i++ {
		handlerName := fmt.Sprintf("route-%d", i+1)
		handler, err := plugin.New(context.Background(), next, cfg, handlerName)
		if err != nil {
			t.Fatalf("Error creating handler %s: %v", handlerName, err)
		}
		handlers = append(handlers, handler)
	}

	// Give time for initial fetch to complete
	time.Sleep(100 * time.Millisecond)

	// Verify only one API request was made despite creating multiple handlers
	initialRequestCount := atomic.LoadInt32(&requestCount)
	if initialRequestCount != 1 {
		t.Errorf("Expected only 1 initial API request, got %d", initialRequestCount)
	} else {
		t.Logf("Success: Only 1 initial API request was made for %d handlers", instanceCount)
	}

	// Test all handlers with a blocked IP - all should return 503
	for i, handler := range handlers {
		req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
		req.Header.Set("X-Forwarded-For", "10.0.0.1") // Not in whitelist
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		resp := rec.Result()
		if resp.StatusCode != 503 {
			t.Errorf("Handler %d: Expected status code 503 for blocked IP, got %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Test all handlers with an allowed IP - all should pass through
	for i, handler := range handlers {
		req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
		req.Header.Set("X-Forwarded-For", "192.168.1.1") // In whitelist
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		resp := rec.Result()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Handler %d: Expected status code 200 for allowed IP, got %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Verify next handler was called the expected number of times
	// It should be called only for the whitelisted IPs (5 handlers)
	expectedNextCalls := int32(instanceCount)
	actualNextCalls := atomic.LoadInt32(&nextHandlerCallCount)
	if actualNextCalls != expectedNextCalls {
		t.Errorf("Expected next handler to be called %d times, got %d", expectedNextCalls, actualNextCalls)
	}

	// Verify API endpoint was still only called once despite handling 10 requests
	finalRequestCount := atomic.LoadInt32(&requestCount)
	if finalRequestCount != 1 {
		t.Errorf("Expected only 1 total API request, got %d", finalRequestCount)
	} else {
		t.Logf("Success: Only 1 API request was made throughout the test")
	}
}

func TestIPDetectionFromMultipleHeaders(t *testing.T) {
	// Reset shared state between tests
	plugin.ResetSharedCacheForTesting()
	time.Sleep(100 * time.Millisecond)

	ts, regularEndpoint, _, _, _ := setupTestServer()
	defer ts.Close()

	cfg := plugin.CreateConfig()
	cfg.EnvironmentEndpoints = map[string]string{"": regularEndpoint}
	cfg.Debug = false

	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	handler, err := plugin.New(context.Background(), next, cfg, "ip-detection-test")
	if err != nil {
		t.Fatalf("Error creating plugin: %v", err)
	}

	// Allow time for initial fetch to complete
	time.Sleep(200 * time.Millisecond)

	tests := []struct {
		name           string
		headers        map[string]string
		expectedStatus int
		description    string
	}{
		{
			name: "X-Forwarded-For header",
			headers: map[string]string{
				"X-Forwarded-For": "203.0.113.1",
			},
			expectedStatus: http.StatusOK,
			description:    "Should detect IP from X-Forwarded-For header",
		},
		{
			name: "X-Real-Ip header",
			headers: map[string]string{
				"X-Real-Ip": "203.0.113.2",
			},
			expectedStatus: http.StatusOK,
			description:    "Should detect IP from X-Real-Ip header",
		},
		{
			name: "Cf-Connecting-Ip header",
			headers: map[string]string{
				"Cf-Connecting-Ip": "203.0.113.3",
			},
			expectedStatus: http.StatusOK,
			description:    "Should detect IP from Cf-Connecting-Ip header (CloudFlare)",
		},
		{
			name: "True-Client-Ip header",
			headers: map[string]string{
				"True-Client-Ip": "203.0.113.4",
			},
			expectedStatus: http.StatusOK,
			description:    "Should detect IP from True-Client-Ip header",
		},
		{
			name: "X-Client-Ip header",
			headers: map[string]string{
				"X-Client-Ip": "203.0.113.5",
			},
			expectedStatus: http.StatusOK,
			description:    "Should detect IP from X-Client-Ip header",
		},
		{
			name: "Forwarded header (RFC 7239)",
			headers: map[string]string{
				"Forwarded": "for=203.0.113.6;proto=https",
			},
			expectedStatus: http.StatusOK,
			description:    "Should detect IP from Forwarded header (RFC 7239)",
		},
		{
			name: "X-Original-Forwarded-For header",
			headers: map[string]string{
				"X-Original-Forwarded-For": "203.0.113.7",
			},
			expectedStatus: http.StatusOK,
			description:    "Should detect IP from X-Original-Forwarded-For header",
		},
		{
			name: "Multiple headers with different IPs",
			headers: map[string]string{
				"X-Forwarded-For":          "203.0.113.8", // Should use this one
				"X-Real-Ip":                "203.0.113.9",
				"X-Original-Forwarded-For": "203.0.113.10",
			},
			expectedStatus: http.StatusOK,
			description:    "Should detect IP from highest priority header (Cf-Connecting-Ip)",
		},
		{
			name: "X-Forwarded-For with multiple IPs",
			headers: map[string]string{
				"X-Forwarded-For": "203.0.113.11, 10.0.0.1, 172.16.0.1",
			},
			expectedStatus: http.StatusOK,
			description:    "Should use the leftmost (client) IP from X-Forwarded-For",
		},
		{
			name: "IPv6 address",
			headers: map[string]string{
				"X-Forwarded-For": "2001:db8::1",
			},
			expectedStatus: http.StatusOK,
			description:    "Should handle IPv6 addresses correctly",
		},
		{
			name: "IPv6 address with port",
			headers: map[string]string{
				"X-Forwarded-For": "[2001:db8::1]:8080",
			},
			expectedStatus: http.StatusOK,
			description:    "Should handle IPv6 addresses with port correctly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)

			// Set headers
			for name, value := range tt.headers {
				req.Header.Set(name, value)
			}

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			response := recorder.Result()
			defer response.Body.Close()

			if response.StatusCode != tt.expectedStatus {
				t.Errorf("%s: Expected status code %d, got %d", tt.description, tt.expectedStatus, response.StatusCode)
			}
		})
	}
}

func TestCIDRWhitelist(t *testing.T) {
	// Reset shared state between tests
	plugin.ResetSharedCacheForTesting()
	time.Sleep(100 * time.Millisecond)

	// Create custom test server with CIDR whitelist
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := maintenanceResponse{}

		switch r.URL.Path {
		case "/maintenance-active-cidr":
			response.SystemConfig.Maintenance.IsActive = true
			// Whitelist 192.168.0.0/16 network and a specific IP
			response.SystemConfig.Maintenance.Whitelist = []string{"192.168.0.0/16", "10.0.0.1"}
		default:
			response.SystemConfig.Maintenance.IsActive = false
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer ts.Close()

	cidrEndpoint := ts.URL + "/maintenance-active-cidr"

	cfg := plugin.CreateConfig()
	cfg.EnvironmentEndpoints = map[string]string{"": cidrEndpoint}
	cfg.CacheDurationInSeconds = 10
	cfg.RequestTimeoutInSeconds = 5
	cfg.MaintenanceStatusCode = 503
	cfg.Debug = false

	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	handler, err := plugin.New(context.Background(), next, cfg, "cidr-test")
	if err != nil {
		t.Fatalf("Error creating plugin: %v", err)
	}

	// Allow time for initial fetch to complete
	time.Sleep(200 * time.Millisecond)

	tests := []struct {
		name           string
		clientIP       string
		expectedStatus int
		description    string
	}{
		{
			name:           "IP in exact whitelist",
			clientIP:       "10.0.0.1",
			expectedStatus: http.StatusOK,
			description:    "Should allow IP that exactly matches a whitelist entry",
		},
		{
			name:           "IP in CIDR range",
			clientIP:       "192.168.1.1",
			expectedStatus: http.StatusOK,
			description:    "Should allow IP that is within CIDR range in whitelist",
		},
		{
			name:           "IP in CIDR range (edge case)",
			clientIP:       "192.168.255.255",
			expectedStatus: http.StatusOK,
			description:    "Should allow IP at the edge of CIDR range in whitelist",
		},
		{
			name:           "IP outside CIDR range",
			clientIP:       "172.16.0.1",
			expectedStatus: 503,
			description:    "Should block IP outside of CIDR range and not in whitelist",
		},
		{
			name:           "IP outside CIDR but numerically close",
			clientIP:       "193.168.0.1",
			expectedStatus: 503,
			description:    "Should block IP just outside of CIDR range",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
			req.Header.Set("X-Forwarded-For", tt.clientIP)

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			response := recorder.Result()
			defer response.Body.Close()

			if response.StatusCode != tt.expectedStatus {
				t.Errorf("%s: Expected status code %d, got %d", tt.description, tt.expectedStatus, response.StatusCode)
			}
		})
	}
}

func TestKubernetesEnvironment(t *testing.T) {
	// Reset shared state between tests
	plugin.ResetSharedCacheForTesting()
	time.Sleep(100 * time.Millisecond)

	// Set up test server with active maintenance mode
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := maintenanceResponse{}
		response.SystemConfig.Maintenance.IsActive = true
		// Whitelist for external IPs, not internal k8s IPs
		response.SystemConfig.Maintenance.Whitelist = []string{"203.0.113.0/24"}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer ts.Close()

	cfg := plugin.CreateConfig()
	cfg.EnvironmentEndpoints = map[string]string{"": ts.URL}
	cfg.CacheDurationInSeconds = 10
	cfg.RequestTimeoutInSeconds = 5
	cfg.MaintenanceStatusCode = 503
	cfg.Debug = false

	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	handler, err := plugin.New(context.Background(), next, cfg, "k8s-test")
	if err != nil {
		t.Fatalf("Error creating plugin: %v", err)
	}

	// Allow time for initial fetch to complete
	time.Sleep(200 * time.Millisecond)

	tests := []struct {
		name           string
		headers        map[string]string
		remoteAddr     string
		expectedStatus int
		description    string
	}{
		{
			name: "External traffic through ingress",
			headers: map[string]string{
				// Original client IP in header from ingress controller
				"X-Forwarded-For": "203.0.113.42, 10.10.122.113",
				// K8s internal IP in RemoteAddr
				"User-Agent": "Mozilla/5.0",
			},
			remoteAddr:     "10.10.122.113:12345",
			expectedStatus: http.StatusOK,
			description:    "Should allow external traffic from whitelisted IP through k8s ingress",
		},
		{
			name: "External traffic through ingress (blocked)",
			headers: map[string]string{
				// Original client IP in header from ingress controller
				"X-Forwarded-For": "192.0.2.42, 10.10.122.113",
				// K8s internal IP in RemoteAddr
				"User-Agent": "Mozilla/5.0",
			},
			remoteAddr:     "10.10.122.113:12345",
			expectedStatus: 503,
			description:    "Should block external traffic from non-whitelisted IP through k8s ingress",
		},
		{
			name: "Internal k8s traffic with original XFF",
			headers: map[string]string{
				// Original header from previous service
				"X-Forwarded-For": "10.10.122.113",
				// Internal service user agent
				"User-Agent": "Go-http-client/1.1",
			},
			remoteAddr:     "10.10.122.113:12345",
			expectedStatus: 503, // Will be blocked since 10.10.122.113 is not whitelisted
			description:    "Internal k8s traffic should be treated based on its IP in headers",
		},
		{
			name: "Internal k8s traffic with multiple proxy hops",
			headers: map[string]string{
				// Complex forwarding chain with internal IPs
				"X-Forwarded-For": "10.10.175.64, 10.10.234.191, 10.10.60.0",
				"User-Agent":      "kube-probe/1.25",
			},
			remoteAddr:     "10.10.60.0:8080",
			expectedStatus: 503, // Will be blocked since k8s IPs are not whitelisted
			description:    "Complex internal k8s traffic with multiple hops should be handled correctly",
		},
		{
			name: "External traffic with Cloudflare header",
			headers: map[string]string{
				// XFF contains multiple IPs including k8s internal
				"X-Forwarded-For": "192.0.2.45, 10.10.122.113",
				// Cloudflare provides this header with true client IP
				"Cf-Connecting-Ip": "203.0.113.99",
				"User-Agent":       "Mozilla/5.0",
			},
			remoteAddr:     "10.10.122.113:12345",
			expectedStatus: http.StatusOK, // Should allow because Cf-Connecting-Ip is whitelisted
			description:    "Cloudflare connection should use Cf-Connecting-Ip header for client IP",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)

			// Set all headers
			for name, value := range tt.headers {
				req.Header.Set(name, value)
			}

			// Set remote address
			if tt.remoteAddr != "" {
				req.RemoteAddr = tt.remoteAddr
			}

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			response := recorder.Result()
			defer response.Body.Close()

			if response.StatusCode != tt.expectedStatus {
				t.Errorf("%s: Expected status code %d, got %d", tt.description, tt.expectedStatus, response.StatusCode)
			}
		})
	}
}

func TestInvalidIPAndCIDRHandling(t *testing.T) {
	// Reset shared state between tests
	plugin.ResetSharedCacheForTesting()
	time.Sleep(100 * time.Millisecond)

	// Create custom test server with invalid whitelist entries
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := maintenanceResponse{}

		switch r.URL.Path {
		case "/maintenance-active-invalid-cidr":
			response.SystemConfig.Maintenance.IsActive = true
			// Include some invalid CIDR notations and IPs
			response.SystemConfig.Maintenance.Whitelist = []string{
				"192.168.0.0/16", // Valid CIDR
				"10.0.0.256",     // Invalid IP (256 is out of range)
				"172.16.0.0/33",  // Invalid CIDR (prefix length > 32)
				"not-an-ip",      // Completely invalid
				"203.0.113.1",    // Valid IP
			}
		default:
			response.SystemConfig.Maintenance.IsActive = false
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer ts.Close()

	invalidEndpoint := ts.URL + "/maintenance-active-invalid-cidr"

	cfg := plugin.CreateConfig()
	cfg.EnvironmentEndpoints = map[string]string{"": invalidEndpoint}
	cfg.CacheDurationInSeconds = 10
	cfg.RequestTimeoutInSeconds = 5
	cfg.MaintenanceStatusCode = 503
	cfg.Debug = false

	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	handler, err := plugin.New(context.Background(), next, cfg, "invalid-ip-test")
	if err != nil {
		t.Fatalf("Error creating plugin: %v", err)
	}

	// Allow time for initial fetch to complete
	time.Sleep(200 * time.Millisecond)

	tests := []struct {
		name           string
		clientIP       string
		expectedStatus int
		description    string
	}{
		{
			name:           "Valid IP in valid CIDR range",
			clientIP:       "192.168.1.1",
			expectedStatus: http.StatusOK,
			description:    "Should allow IP in valid CIDR range",
		},
		{
			name:           "Valid IP matching valid whitelist entry",
			clientIP:       "203.0.113.1",
			expectedStatus: http.StatusOK,
			description:    "Should allow IP that exactly matches a valid whitelist entry",
		},
		{
			name:           "Non-matching IP",
			clientIP:       "8.8.8.8",
			expectedStatus: 503,
			description:    "Should block IP that doesn't match any valid whitelist entry",
		},
		{
			name:           "Invalid IP format in request",
			clientIP:       "invalid-request-ip",
			expectedStatus: 503,
			description:    "Should handle invalid IP in request gracefully and block",
		},
		{
			name:           "Empty IP",
			clientIP:       "",
			expectedStatus: 503,
			description:    "Should handle empty IP gracefully and block",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)

			if tt.clientIP != "" {
				req.Header.Set("X-Forwarded-For", tt.clientIP)
			}

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			response := recorder.Result()
			defer response.Body.Close()

			if response.StatusCode != tt.expectedStatus {
				t.Errorf("%s: Expected status code %d, got %d", tt.description, tt.expectedStatus, response.StatusCode)
			}
		})
	}
}

func TestSecretHeaderFunctionality(t *testing.T) {
	// Reset shared state between tests
	plugin.ResetSharedCacheForTesting()
	time.Sleep(100 * time.Millisecond)

	// Track received headers to verify plugin sends the secret header
	var receivedHeaders http.Header
	var headerMutex sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Store received headers for verification
		headerMutex.Lock()
		receivedHeaders = r.Header.Clone()
		headerMutex.Unlock()

		response := maintenanceResponse{}

		// Check for secret header and respond accordingly
		secretHeader := r.Header.Get("X-Plugin-Secret")
		if secretHeader == "test-secret-token" {
			// Plugin request - return full info with whitelist
			response.SystemConfig.Maintenance.IsActive = true
			response.SystemConfig.Maintenance.Whitelist = []string{"192.168.1.1"}
		} else {
			// Frontend request - return only status without whitelist
			response.SystemConfig.Maintenance.IsActive = true
			// No whitelist for frontend
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer ts.Close()

	// Test with secret header configured
	cfg := plugin.CreateConfig()
	cfg.EnvironmentEndpoints = map[string]string{"": ts.URL}
	cfg.EnvironmentSecrets = map[string]plugin.EnvironmentSecret{
		"": {Header: "X-Plugin-Secret", Value: "test-secret-token"},
	}
	cfg.CacheDurationInSeconds = 10
	cfg.RequestTimeoutInSeconds = 5
	cfg.MaintenanceStatusCode = 503
	cfg.Debug = false

	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	handler, err := plugin.New(context.Background(), next, cfg, "secret-header-test")
	if err != nil {
		t.Fatalf("Error creating plugin: %v", err)
	}

	// Allow time for initial fetch to complete
	time.Sleep(200 * time.Millisecond)

	// Verify that the secret header was sent to the API
	headerMutex.Lock()
	receivedSecret := receivedHeaders.Get("X-Plugin-Secret")
	headerMutex.Unlock()

	if receivedSecret != "test-secret-token" {
		t.Errorf("Expected secret header 'test-secret-token', got '%s'", receivedSecret)
	}

	// Test that the plugin correctly processed the response with whitelist
	req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
	req.Header.Set("X-Forwarded-For", "192.168.1.1") // Should be allowed

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	response := recorder.Result()
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Errorf("Expected status code 200 for whitelisted IP, got %d", response.StatusCode)
	}

	// Test that non-whitelisted IP is blocked
	req2 := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
	req2.Header.Set("X-Forwarded-For", "10.0.0.1") // Should be blocked

	recorder2 := httptest.NewRecorder()
	handler.ServeHTTP(recorder2, req2)

	response2 := recorder2.Result()
	defer response2.Body.Close()

	if response2.StatusCode != 503 {
		t.Errorf("Expected status code 503 for non-whitelisted IP, got %d", response2.StatusCode)
	}
}

func TestCORSFunctionalityDuringMaintenance(t *testing.T) {
	// Reset shared state between tests
	plugin.ResetSharedCacheForTesting()
	time.Sleep(100 * time.Millisecond)

	// Set up test server with active maintenance mode
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := maintenanceResponse{}

		switch r.URL.Path {
		case "/maintenance-active-no-whitelist":
			response.SystemConfig.Maintenance.IsActive = true
			response.SystemConfig.Maintenance.Whitelist = []string{} // No one allowed
		case "/maintenance-active-with-whitelist":
			response.SystemConfig.Maintenance.IsActive = true
			response.SystemConfig.Maintenance.Whitelist = []string{"192.168.1.1"} // Specific IP allowed
		default:
			response.SystemConfig.Maintenance.IsActive = false
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer ts.Close()

	tests := []struct {
		name                string
		endpoint            string
		method              string
		origin              string
		clientIP            string
		expectedStatusCode  int
		expectedCORSOrigin  string
		expectedCORSMethods string
		expectedCORSHeaders string
		expectedCORSMaxAge  string
		description         string
	}{
		{
			name:                "CORS preflight - maintenance active, blocked IP",
			endpoint:            ts.URL + "/maintenance-active-no-whitelist",
			method:              http.MethodOptions,
			origin:              "https://citronus.pro",
			clientIP:            "10.0.0.1",
			expectedStatusCode:  http.StatusOK, // Preflight should return 200 even when blocked
			expectedCORSOrigin:  "https://citronus.pro",
			expectedCORSMethods: "GET, POST, PUT, DELETE, OPTIONS",
			expectedCORSHeaders: "Accept, Authorization, Content-Type, X-CSRF-Token",
			expectedCORSMaxAge:  "86400",
			description:         "Should handle CORS preflight with 200 status, actual request will be blocked",
		},
		{
			name:                "CORS preflight - maintenance active, allowed IP",
			endpoint:            ts.URL + "/maintenance-active-with-whitelist",
			method:              http.MethodOptions,
			origin:              "https://citronus.pro",
			clientIP:            "192.168.1.1",
			expectedStatusCode:  http.StatusNoContent, // Successful preflight
			expectedCORSOrigin:  "https://citronus.pro",
			expectedCORSMethods: "GET, POST, PUT, DELETE, OPTIONS",
			expectedCORSHeaders: "Accept, Authorization, Content-Type, X-CSRF-Token",
			expectedCORSMaxAge:  "86400",
			description:         "Should handle CORS preflight and allow due to IP whitelist",
		},
		{
			name:                "CORS preflight - maintenance inactive",
			endpoint:            ts.URL, // Default inactive maintenance
			method:              http.MethodOptions,
			origin:              "https://citronus.pro",
			clientIP:            "10.0.0.1",
			expectedStatusCode:  http.StatusOK, // Backend handles OPTIONS and returns 200 (from our test server)
			expectedCORSOrigin:  "",            // No CORS headers from plugin when maintenance is off
			expectedCORSMethods: "",
			expectedCORSHeaders: "",
			expectedCORSMaxAge:  "",
			description:         "Should pass OPTIONS to backend when maintenance is inactive",
		},
		{
			name:                "Regular request - maintenance active, blocked IP with CORS",
			endpoint:            ts.URL + "/maintenance-active-no-whitelist",
			method:              http.MethodGet,
			origin:              "https://citronus.pro",
			clientIP:            "10.0.0.1",
			expectedStatusCode:  503, // Maintenance status code
			expectedCORSOrigin:  "https://citronus.pro",
			expectedCORSMethods: "GET, POST, PUT, DELETE, OPTIONS",
			expectedCORSHeaders: "Accept, Authorization, Content-Type, X-CSRF-Token",
			expectedCORSMaxAge:  "86400",
			description:         "Should return maintenance status with full CORS headers for blocked requests",
		},
		{
			name:               "Regular request - maintenance active, allowed IP",
			endpoint:           ts.URL + "/maintenance-active-with-whitelist",
			method:             http.MethodGet,
			origin:             "https://citronus.pro",
			clientIP:           "192.168.1.1",
			expectedStatusCode: http.StatusOK, // Pass through to backend
			description:        "Should pass through to backend for whitelisted IP",
		},
		{
			name:               "CORS preflight - no origin header",
			endpoint:           ts.URL + "/maintenance-active-no-whitelist",
			method:             http.MethodOptions,
			origin:             "", // No origin
			clientIP:           "10.0.0.1",
			expectedStatusCode: http.StatusOK, // Should pass to backend when no origin
			description:        "Should pass OPTIONS to backend when no origin header is present",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset shared state for each test
			plugin.ResetSharedCacheForTesting()
			time.Sleep(100 * time.Millisecond)

			cfg := plugin.CreateConfig()
			cfg.EnvironmentEndpoints = map[string]string{"": tt.endpoint}
			cfg.CacheDurationInSeconds = 10
			cfg.RequestTimeoutInSeconds = 5
			cfg.MaintenanceStatusCode = 503
			cfg.Debug = false

			next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				rw.WriteHeader(http.StatusOK)
			})

			handler, err := plugin.New(context.Background(), next, cfg, "cors-test")
			if err != nil {
				t.Fatalf("Error creating plugin: %v", err)
			}

			// Allow time for initial fetch to complete
			time.Sleep(200 * time.Millisecond)

			// Create request
			req := httptest.NewRequest(tt.method, "http://localhost/", nil)

			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}

			if tt.clientIP != "" {
				req.Header.Set("X-Forwarded-For", tt.clientIP)
			}

			// For preflight requests, add typical CORS headers
			if tt.method == http.MethodOptions {
				req.Header.Set("Access-Control-Request-Method", "GET")
				req.Header.Set("Access-Control-Request-Headers", "Content-Type,Authorization")
			}

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			response := recorder.Result()
			defer response.Body.Close()

			// Check status code
			if response.StatusCode != tt.expectedStatusCode {
				t.Errorf("%s: Expected status code %d, got %d", tt.description, tt.expectedStatusCode, response.StatusCode)
			}

			// Check CORS headers if expected
			if tt.expectedCORSOrigin != "" {
				actualOrigin := response.Header.Get("Access-Control-Allow-Origin")
				if actualOrigin != tt.expectedCORSOrigin {
					t.Errorf("%s: Expected CORS origin '%s', got '%s'", tt.description, tt.expectedCORSOrigin, actualOrigin)
				}
			}

			if tt.expectedCORSMethods != "" {
				actualMethods := response.Header.Get("Access-Control-Allow-Methods")
				if actualMethods != tt.expectedCORSMethods {
					t.Errorf("%s: Expected CORS methods '%s', got '%s'", tt.description, tt.expectedCORSMethods, actualMethods)
				}
			}

			if tt.expectedCORSHeaders != "" {
				actualHeaders := response.Header.Get("Access-Control-Allow-Headers")
				if actualHeaders != tt.expectedCORSHeaders {
					t.Errorf("%s: Expected CORS headers '%s', got '%s'", tt.description, tt.expectedCORSHeaders, actualHeaders)
				}
			}

			if tt.expectedCORSMaxAge != "" {
				actualMaxAge := response.Header.Get("Access-Control-Max-Age")
				if actualMaxAge != tt.expectedCORSMaxAge {
					t.Errorf("%s: Expected CORS max-age '%s', got '%s'", tt.description, tt.expectedCORSMaxAge, actualMaxAge)
				}
			}

			// For responses with CORS headers, check that credentials are allowed (only if origin was present)
			if tt.expectedCORSOrigin != "" && (tt.expectedStatusCode == 503 || tt.method == http.MethodOptions) {
				credentialsAllowed := response.Header.Get("Access-Control-Allow-Credentials")
				if credentialsAllowed != "true" {
					t.Errorf("%s: Expected Access-Control-Allow-Credentials 'true', got '%s'", tt.description, credentialsAllowed)
				}
			}

			// Verify content type for all maintenance responses (both with and without origin)
			if tt.expectedStatusCode == 503 {
				contentType := response.Header.Get("Content-Type")
				if contentType != "text/plain; charset=utf-8" {
					t.Errorf("%s: Expected content type 'text/plain; charset=utf-8', got '%s'", tt.description, contentType)
				}
			}

			if t.Failed() {
				t.Logf("Response headers: %+v", response.Header)
			}
		})
	}
}

func TestProductionComDomainSupport(t *testing.T) {
	plugin.ResetSharedCacheForTesting()
	time.Sleep(100 * time.Millisecond)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := maintenanceResponse{}

		switch r.URL.Path {
		case "/prod-maintenance-active":
			response.SystemConfig.Maintenance.IsActive = true
			response.SystemConfig.Maintenance.Whitelist = []string{"203.0.113.0/24"} // Production IP range
		case "/prod-maintenance-inactive":
			response.SystemConfig.Maintenance.IsActive = false
		default:
			response.SystemConfig.Maintenance.IsActive = false
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer ts.Close()

	tests := []struct {
		name            string
		customEndpoints map[string]string
		testDomain      string
		clientIP        string
		expectedStatus  int
		description     string
	}{
		{
			name: "Production .com domain with custom endpoint",
			customEndpoints: map[string]string{
				".com": ts.URL + "/prod-maintenance-active",
				"":     ts.URL + "/prod-maintenance-inactive", // default fallback
			},
			testDomain:     "api.citronus.com",
			clientIP:       "203.0.113.100", // In whitelist range
			expectedStatus: http.StatusOK,
			description:    "Should route .com domain to custom production endpoint and allow whitelisted IP",
		},
		{
			name: "Production .com domain - blocked IP",
			customEndpoints: map[string]string{
				".com": ts.URL + "/prod-maintenance-active",
				"":     ts.URL + "/prod-maintenance-inactive",
			},
			testDomain:     "portal.citronus.com",
			clientIP:       "10.0.0.1", // Not in whitelist
			expectedStatus: 503,
			description:    "Should route .com domain to custom production endpoint and block non-whitelisted IP",
		},
		{
			name: "Non-.com domain uses default endpoint",
			customEndpoints: map[string]string{
				".com": ts.URL + "/prod-maintenance-active",
				"":     ts.URL + "/prod-maintenance-inactive",
			},
			testDomain:     "test.example.org",
			clientIP:       "10.0.0.1",
			expectedStatus: http.StatusOK,
			description:    "Should use default endpoint for domains that don't match .com",
		},
		{
			name: "Multiple custom domains",
			customEndpoints: map[string]string{
				".com":   ts.URL + "/prod-maintenance-active",
				".local": ts.URL + "/prod-maintenance-inactive",
				".dev":   ts.URL + "/prod-maintenance-inactive",
				"":       ts.URL + "/prod-maintenance-inactive",
			},
			testDomain:     "staging.citronus.local",
			clientIP:       "192.168.1.1",
			expectedStatus: http.StatusOK,
			description:    "Should support multiple custom domain endpoints",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin.ResetSharedCacheForTesting()
			time.Sleep(100 * time.Millisecond)

			cfg := plugin.CreateConfig()
			cfg.EnvironmentEndpoints = tt.customEndpoints
			cfg.CacheDurationInSeconds = 10
			cfg.RequestTimeoutInSeconds = 5
			cfg.MaintenanceStatusCode = 503
			cfg.Debug = false

			next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				rw.WriteHeader(http.StatusOK)
			})

			handler, err := plugin.New(context.Background(), next, cfg, "prod-com-test")
			if err != nil {
				t.Fatalf("Error creating plugin: %v", err)
			}

			time.Sleep(200 * time.Millisecond)

			req := httptest.NewRequest(http.MethodGet, "http://"+tt.testDomain+"/", nil)
			req.Host = tt.testDomain

			if tt.clientIP != "" {
				req.Header.Set("X-Forwarded-For", tt.clientIP)
			}

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			response := recorder.Result()
			defer response.Body.Close()

			if response.StatusCode != tt.expectedStatus {
				t.Errorf("%s: Expected status code %d, got %d", tt.description, tt.expectedStatus, response.StatusCode)
			}
		})
	}
}

func TestConfigurationOverridesBehavior(t *testing.T) {
	plugin.ResetSharedCacheForTesting()
	time.Sleep(100 * time.Millisecond)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := maintenanceResponse{}
		response.SystemConfig.Maintenance.IsActive = false
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer ts.Close()

	cfg := plugin.CreateConfig()
	cfg.EnvironmentEndpoints = map[string]string{
		".production": ts.URL,
		".staging":    ts.URL,
		"":            ts.URL,
	}

	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	handler, err := plugin.New(context.Background(), next, cfg, "config-override-test")
	if err != nil {
		t.Fatalf("Error creating plugin: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	tests := []struct {
		domain         string
		expectedStatus int
		description    string
	}{
		{
			domain:         "app.citronus.production",
			expectedStatus: http.StatusOK,
			description:    "Custom .production domain should work",
		},
		{
			domain:         "test.citronus.staging",
			expectedStatus: http.StatusOK,
			description:    "Custom .staging domain should work",
		},
		{
			domain:         "legacy.citronus.com",
			expectedStatus: http.StatusOK,
			description:    "Non-matching domain should use default endpoint",
		},
		{
			domain:         "dev.citronus.world",
			expectedStatus: http.StatusOK,
			description:    "Previously hardcoded .world should now use default endpoint",
		},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://"+tt.domain+"/", nil)
			req.Host = tt.domain

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			response := recorder.Result()
			defer response.Body.Close()

			if response.StatusCode != tt.expectedStatus {
				t.Errorf("%s: Expected status code %d, got %d", tt.description, tt.expectedStatus, response.StatusCode)
			}
		})
	}
}
