# Traefik Maintenance Plugin

A robust middleware plugin for Traefik that checks for maintenance status from an API and blocks requests if maintenance is active.

## Features

- Checks maintenance status from a configurable API endpoint
- Automatically refreshes maintenance status in the background at configurable intervals
- Allows specific IP addresses to bypass maintenance mode
- Supports a wildcard (`*`) for allowing all traffic during maintenance
- Allows specific URL prefixes to bypass maintenance checks
- Allows specific hosts to bypass maintenance checks (including wildcard domains)
- **Full CORS support for preflight and actual requests during maintenance**
- Configurable request timeout for maintenance status API 
- Thread-safe implementation with zero impact on request performance
- Customizable maintenance status code
- Graceful startup and shutdown handling
- High performance design with no bottlenecks under load
- Enhanced client IP detection for Kubernetes and proxy environments

## CORS Support

The plugin provides comprehensive CORS support to handle modern web applications:

### CORS Features
- **Preflight Request Handling**: Properly handles OPTIONS requests with appropriate CORS headers
- **Origin Validation**: Reflects the requesting origin in `Access-Control-Allow-Origin` header
- **Credentials Support**: Sets `Access-Control-Allow-Credentials: true` for authenticated requests
- **Maintenance Mode Integration**: CORS headers are included even when returning maintenance responses
- **Complete Header Support**: Provides all necessary CORS headers including `Access-Control-Allow-Methods`, `Access-Control-Allow-Headers`, and `Access-Control-Max-Age`
- **Standard Headers**: Supports common headers like `Accept`, `Authorization`, `Content-Type`, `X-CSRF-Token`

### CORS During Maintenance
When maintenance mode is active:
1. **Preflight requests (OPTIONS)** receive proper CORS headers and are blocked/allowed based on IP whitelist
2. **Regular requests** that are blocked due to maintenance receive full CORS headers for proper client handling, including:
   - `Access-Control-Allow-Origin` (reflects the requesting origin)
   - `Access-Control-Allow-Methods` (GET, POST, PUT, DELETE, OPTIONS)
   - `Access-Control-Allow-Headers` (Accept, Authorization, Content-Type, X-CSRF-Token)
   - `Access-Control-Allow-Credentials` (true)
   - `Access-Control-Max-Age` (86400)
3. **Successful requests** (whitelisted IPs) pass through normally with backend CORS handling

This ensures that frontend applications receive proper CORS responses even during maintenance, preventing browser console errors and enabling graceful degradation of XHR/fetch requests.

## Production Ready

This plugin has been thoroughly tested and is production-ready with:
- ✅ Comprehensive test coverage including edge cases
- ✅ CORS functionality fully tested
- ✅ Error handling and graceful degradation
- ✅ Performance optimization and zero request overhead
- ✅ Memory safety and concurrent access protection
- ✅ Kubernetes and proxy environment support

## Usage

### Configuration

```yaml
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: maintenance-check
  namespace: default
spec:
  plugin:
    maintenanceCheck:
      environmentEndpoints:
        ".world": "http://admin-service.stage-admin/admin/api/system-config/?format=json"
        ".pro": "http://admin-service.develop-admin/admin/api/system-config/?format=json"
        "": "http://admin-service.admin/admin/api/system-config/?format=json"
      environmentSecrets:
        ".world":
          header: "X-Plugin-Secret"
          value: "stage-secret-token"
        ".pro":
          header: "X-Plugin-Secret"
          value: "develop-secret-token"
        "":
          header: "X-Plugin-Secret"
          value: "admin-secret-token"
      cacheDurationInSeconds: 60
      requestTimeoutInSeconds: 5
      skipPrefixes:      # optional URL prefixes to bypass maintenance checks
        - /admin
        - /pgadmin
      skipHosts:         # optional hostnames to bypass maintenance checks
        - admin.example.com
        - monitoring.example.com
        - *.internal.example.com
      maintenanceStatusCode: 512  # HTTP status code when in maintenance
      debug: false       # set to true to enable detailed logging
```

### Helm Configuration Example

```yaml
# values.yaml
traefik:
  additionalArguments:
    - "--experimental.plugins.maintenanceCheck.modulename=github.com/CitronusAcademy/traefik-maintenance-plugin"
    - "--experimental.plugins.maintenanceCheck.version=v0.1.0"

middleware:
  maintenance-check:
    spec:
      plugin:
        maintenanceCheck:
          environmentEndpoints:
            ".world": "http://admin-service.stage-admin/admin/api/system-config/?format=json"
            ".pro": "http://admin-service.develop-admin/admin/api/system-config/?format=json"
            "": "http://admin-service.admin/admin/api/system-config/?format=json"
          environmentSecrets:
            ".world":
              header: "X-Plugin-Secret"
              value: "stage-secret-token"
            ".pro":
              header: "X-Plugin-Secret"
              value: "develop-secret-token"
            "":
              header: "X-Plugin-Secret"
              value: "admin-secret-token"
          cacheDurationInSeconds: 60
          requestTimeoutInSeconds: 5
          maintenanceStatusCode: 512
          debug: false
```

### Environment-Based Endpoint Selection

The plugin automatically selects the appropriate maintenance API endpoint based on the request domain suffix. **Any domain suffix can be configured** - the plugin is not limited to specific domains and works with any configuration you provide.

**Default configuration (can be completely overridden):**
- **`.com` domains**: `http://admin-service.admin/admin/api/system-config/?format=json` (production)
- **`.world` domains**: `http://admin-service.stage-admin/admin/api/system-config/?format=json` (staging)
- **`.pro` domains**: `http://admin-service.develop-admin/admin/api/system-config/?format=json` (development)
- **Other domains**: `http://admin-service.admin/admin/api/system-config/?format=json` (fallback)

**Production-ready examples:**

```yaml
# Production configuration with custom domains
environmentEndpoints:
  ".com": "https://api.yourcompany.com/admin/api/system-config/?format=json"
  ".staging": "https://api-staging.yourcompany.com/admin/api/system-config/?format=json"
  ".dev": "https://api-dev.yourcompany.com/admin/api/system-config/?format=json"
  "": "https://api.yourcompany.com/admin/api/system-config/?format=json"  # default fallback

# Multiple production environments
environmentEndpoints:
  ".com": "https://prod-api.yourcompany.com/admin/api/system-config/?format=json"
  ".eu": "https://eu-api.yourcompany.com/admin/api/system-config/?format=json"
  ".asia": "https://asia-api.yourcompany.com/admin/api/system-config/?format=json"
  "": "https://global-api.yourcompany.com/admin/api/system-config/?format=json"

# Single endpoint for all domains
environmentEndpoints:
  "": "https://universal-api.yourcompany.com/admin/api/system-config/?format=json"
```

**Key features:**
- ✅ **Supports any domain suffix** (`.com`, `.org`, `.net`, `.local`, `.production`, etc.)
- ✅ **Configuration overrides defaults** - hardcoded values are only fallbacks
- ✅ **Production-ready** with proper async handling and no race conditions
- ✅ **Per-environment secret headers** for secure API access
- ✅ **Separate maintenance states** per domain suffix

This allows a single Traefik instance to handle multiple environments with completely separate maintenance states and API endpoints.

### API Format

The maintenance API should return a JSON response in the following format:

```json
{
  "system_config": {
    "maintenance": {
      "is_active": false,
      "whitelist": [
        "192.168.1.1",
        "10.0.0.5",
        "172.16.0.0/16"
      ]
    }
  }
}
```

When `is_active` is `true`, only IPs in the whitelist will be allowed to access the service. If the whitelist contains `"*"`, all IPs will be allowed.

## Plugin vs Frontend Access Control

If your maintenance API endpoint is also used by frontend applications to check maintenance status, you may want the plugin to receive full information (including IP whitelist) while the frontend receives only basic status information without sensitive data.

To achieve this, configure the `secretHeader` and `secretHeaderValue` parameters:

```yaml
secretHeader: "X-Plugin-Secret"
secretHeaderValue: "your-secret-token-here"
```

When configured, the plugin will send this header with all requests to your maintenance API. Your server can then:

1. **For requests WITH the secret header**: Return full response including IP whitelist for the plugin
2. **For requests WITHOUT the secret header**: Return only basic info for frontend

Example server logic:
```python
def get_maintenance_status(request):
    is_plugin_request = request.headers.get('X-Plugin-Secret') == 'your-secret-token-here'
    
    if is_plugin_request:
        # Return full info for plugin
        return {
            "system_config": {
                "maintenance": {
                    "is_active": True,
                    "whitelist": ["192.168.1.1", "10.0.0.0/8"]  # Include IPs
                }
            }
        }
    else:
        # Return basic info for frontend
        return {
            "system_config": {
                "maintenance": {
                    "is_active": True
                    # No whitelist for frontend
                }
            }
        }
```

**Security Note**: Keep the secret token secure and use HTTPS for your maintenance API endpoint to prevent token interception.

## Local Testing

This plugin is integrated with the Kubernetes infrastructure in this repository. To test it locally:

1. Run the `plugins/setup-maintenance-plugin.sh` script to set up a Kind cluster with the plugin mounted.
2. Use the `curl` command to test the plugin with different maintenance configurations.

## Integration with Kubernetes Infrastructure

This plugin is part of the Kubernetes infrastructure in this repository. It is loaded locally by Traefik when deployed using the Helm chart.

## Configuration

### Static Configuration

```yaml
experimental:
  plugins:
    maintenance:
      moduleName: "github.com/CitronusAcademy/traefik-maintenance-plugin"
      version: "v0.1.0"
```

### Dynamic Configuration

```yaml
http:
  middlewares:
    maintenance-check:
      plugin:
        maintenance:
          environmentEndpoints:
            ".world": "http://admin-service.stage-admin/admin/api/system-config/?format=json"
            ".pro": "http://admin-service.develop-admin/admin/api/system-config/?format=json"
            "": "http://admin-service.admin/admin/api/system-config/?format=json"
          environmentSecrets:
            ".world":
              header: "X-Plugin-Secret"
              value: "stage-secret-token"
            ".pro":
              header: "X-Plugin-Secret"
              value: "develop-secret-token"
            "":
              header: "X-Plugin-Secret"
              value: "admin-secret-token"
          cacheDurationInSeconds: 10
          requestTimeoutInSeconds: 5
          skipPrefixes:
            - "/admin"
            - "/pgadmin"
          skipHosts:
            - "pgadmin.example.com"
            - "grafana.example.com"
            - "*.internal.example.com"
          maintenanceStatusCode: 512
          debug: false
```

## Endpoint Format

The plugin automatically queries different maintenance status endpoints based on the request domain and expects a JSON response in the following format:

```json
{
  "system_config": {
    "maintenance": {
      "is_active": false,
      "whitelist": [
        "192.168.1.1",
        "10.0.0.5",
        "172.16.0.0/16"
      ]
    }
  }
}
```

When `maintenance.is_active` is `true`, the middleware will check the whitelist:

1. If the whitelist contains `"*"`, all users will be allowed to access the service.
2. If the client's IP address matches any entry in the whitelist, they will be allowed through.
3. CIDR notation (like `192.168.0.0/24`) is supported for allowing entire IP ranges.
4. Otherwise, the configured maintenance status code (default: 512) with the message "Service is in maintenance mode" will be returned.

The plugin extracts client IPs by checking headers in the following order:
1. Cf-Connecting-Ip (CloudFlare)
2. True-Client-Ip (Akamai/Cloudflare)
3. X-Forwarded-For (standard proxy header, first IP if multiple are present)
4. X-Real-Ip (Nginx)
5. X-Client-Ip (Common)
6. Forwarded (RFC 7239 standard)
7. X-Original-Forwarded-For (Traefik specific)
8. Request's RemoteAddr (as last resort)

This extensive header checking ensures the plugin works correctly in complex proxy environments like Kubernetes.

## Parameters

- `environmentEndpoints` (optional): Map of domain suffixes to maintenance API endpoints. Default includes `.world`, `.pro`, and fallback endpoints.
- `environmentSecrets` (optional): Map of domain suffixes to secret headers for each environment. Each entry contains `header` and `value` fields.
- `cacheDurationInSeconds` (optional): How often the background process refreshes maintenance status, specified in seconds. Default is 10.
- `requestTimeoutInSeconds` (optional): Timeout for API requests in seconds. Default is 5.
- `skipPrefixes` (optional): List of URL path prefixes that should bypass maintenance checks, useful for admin interfaces. Default is empty list.
- `maintenanceStatusCode` (optional): HTTP status code to return when in maintenance mode. Default is 512 (Service Unavailable).
- `skipHosts` (optional): List of hostnames that should bypass maintenance checks, useful for admin interfaces, monitoring tools, etc. Supports wildcard patterns like "*.example.com". Default is empty list.
- `debug` (optional): Enable debug logging for troubleshooting. Default is false. When enabled, detailed information about decision making will be logged to stdout.
- `secretHeader` (optional): Legacy fallback secret header name. Use `environmentSecrets` for per-environment configuration.
- `secretHeaderValue` (optional): Legacy fallback secret header value. Use `environmentSecrets` for per-environment configuration.

## Production Readiness Features

- **Background refreshing**: Maintenance status is checked in a background process at regular intervals, completely separate from the request path
- **Zero request overhead**: User requests never trigger maintenance API calls, resulting in consistent performance under any load level
- **Error resilience**: If the maintenance API is unavailable or returns errors, the plugin gracefully falls back to cached values
- **Graceful shutdown**: Properly handles Traefik restart/reload without leaking resources
- **Optimized concurrency**: Read-optimized locking strategy for maximum throughput
- **Immediate startup**: Synchronous initial check ensures the plugin is ready to serve requests as soon as it starts
- **HTTP transport tuning**: Optimized HTTP client with connection pooling and proper timeouts
- **Clean failure modes**: Safe defaults in case of any failures

## Installation

Add the plugin to `traefik_values.yaml`:
```yaml
experimental:
  plugins:
    maintenanceCheck:
      moduleName: github.com/CitronusAcademy/traefik-maintenance-plugin
      version: "v0.1.0"
```
