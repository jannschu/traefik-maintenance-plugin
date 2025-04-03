# Traefik Maintenance Plugin

A robust middleware plugin for Traefik that checks for maintenance status from an API and blocks requests if maintenance is active.

## Features

- Checks maintenance status from a configurable API endpoint
- Caches the maintenance status for a configurable duration
- Allows specific IP addresses to bypass maintenance mode
- Supports a wildcard (`*`) for allowing all traffic during maintenance
- Allows specific URL prefixes to bypass maintenance checks
- Configurable request timeout for maintenance status API 
- Thread-safe implementation to handle concurrent requests
- Customizable maintenance status code
- Automatic cache warmup

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
```

### API Format

The maintenance API should return a JSON response in the following format:

```json
{
  "system_config": {
    "maintenance": {
      "is_active": false,
      "whitelist": ["192.168.1.1", "10.0.0.5"]
    }
  }
}
```

When `is_active` is `true`, only IPs in the whitelist will be allowed to access the service. If the whitelist contains `"*"`, all IPs will be allowed.

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
          maintenanceStatusCode: 512
          debug: false
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
        "10.0.0.5"
      ]
    }
  }
}
```

When `maintenance.is_active` is `true`, the middleware will check the whitelist:

1. If the whitelist contains `"*"`, all users will be allowed to access the service.
2. If the client's IP address matches any entry in the whitelist, they will be allowed through.
3. Otherwise, the configured maintenance status code (default: 512) with the message "Service is in maintenance mode" will be returned.

The plugin extracts client IPs by checking headers in the following order:
1. X-Forwarded-For (first IP if multiple are present)
2. X-Real-IP
3. Request's RemoteAddr

## Parameters

- `endpoint` (required): URL to check maintenance status
- `cacheDurationInSeconds` (optional): How long to cache maintenance status, specified in seconds. Default is 10.
- `requestTimeoutInSeconds` (optional): Timeout for API requests in seconds. Default is 5.
- `skipPrefixes` (optional): List of URL path prefixes that should bypass maintenance checks, useful for admin interfaces. Default is empty list.
- `maintenanceStatusCode` (optional): HTTP status code to return when in maintenance mode. Default is 512 (Service Unavailable).
- `skipHosts` (optional): List of hostnames that should bypass maintenance checks, useful for admin interfaces, monitoring tools, etc. Supports wildcard patterns like "*.example.com". Default is empty list.
- `debug` (optional): Enable debug logging for troubleshooting. Default is false. When enabled, detailed information about decision making will be logged to stdout.

## Production Readiness Features

- **Concurrency handling**: The plugin correctly handles simultaneous requests using atomic operations and locks to prevent thundering herd problems
- **Error resilience**: If the maintenance API is unavailable or returns errors, the plugin gracefully falls back to cached values
- **Timeout handling**: Configurable timeout for maintenance API requests prevents service degradation due to slow APIs
- **Cache invalidation**: Maintenance status is automatically refreshed after the configured duration has elapsed
- **Performance optimization**: The plugin uses read locks when possible to maximize throughput
- **Cache warmup**: Makes an initial request at startup to populate the cache and avoid a request spike on first use
- **HTTP transport tuning**: Optimized HTTP client with connection pooling and proper timeouts
- **Standard HTTP status code**: Uses the proper 512 Service Unavailable status code by default (customizable)

## Installation

Add the plugin to `traefik_values.yaml`:
```yaml
experimental:
  plugins:
    maintenanceCheck:
      moduleName: github.com/CitronusAcademy/traefik-maintenance-plugin
      version: "v0.1.0"
```
