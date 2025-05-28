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
- **Standard Headers**: Supports common headers like `Accept`, `Authorization`, `Content-Type`, `X-CSRF-Token`

### CORS During Maintenance
When maintenance mode is active:
1. **Preflight requests (OPTIONS)** receive proper CORS headers and are blocked/allowed based on IP whitelist
2. **Regular requests** that are blocked due to maintenance still receive CORS headers for proper client handling
3. **Successful requests** (whitelisted IPs) pass through normally with backend CORS handling

This ensures that frontend applications receive proper CORS responses even during maintenance, preventing browser console errors and enabling graceful degradation.

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
      endpoint: http://your-maintenance-api-service/maintenance
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
      secretHeader: "X-Plugin-Secret"      # optional header name for plugin identification
      secretHeaderValue: "your-secret-key" # optional header value for plugin identification
```

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
2. **For requests WITHOUT the secret header**: Return only basic maintenance status for frontend

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
          endpoint: "https://example.com/maintenance-status"
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
          secretHeader: "X-Plugin-Secret"
          secretHeaderValue: "your-secret-key"
```

## Endpoint Format

The plugin expects your maintenance status endpoint to return a JSON response in the following format:

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

- `endpoint` (required): URL to check maintenance status
- `cacheDurationInSeconds` (optional): How often the background process refreshes maintenance status, specified in seconds. Default is 10.
- `requestTimeoutInSeconds` (optional): Timeout for API requests in seconds. Default is 5.
- `skipPrefixes` (optional): List of URL path prefixes that should bypass maintenance checks, useful for admin interfaces. Default is empty list.
- `maintenanceStatusCode` (optional): HTTP status code to return when in maintenance mode. Default is 512 (Service Unavailable).
- `skipHosts` (optional): List of hostnames that should bypass maintenance checks, useful for admin interfaces, monitoring tools, etc. Supports wildcard patterns like "*.example.com". Default is empty list.
- `debug` (optional): Enable debug logging for troubleshooting. Default is false. When enabled, detailed information about decision making will be logged to stdout.
- `secretHeader` (optional): Name of the HTTP header that will be sent with requests to identify the plugin. This allows the server to return different responses for plugin vs frontend requests. Default is empty (no header sent).
- `secretHeaderValue` (optional): Value of the secret header. This should be a secret known only to the server and plugin configuration. Default is empty (no header sent).

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
