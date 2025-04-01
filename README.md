# Traefik Maintenance Plugin

A simple middleware plugin for Traefik that checks for maintenance status from an API and blocks requests if maintenance is active.

## Features

- Checks maintenance status from a configurable API endpoint
- Caches the maintenance status for a configurable duration
- Allows specific IP addresses to bypass maintenance mode
- Supports a wildcard (`*`) for allowing all traffic during maintenance

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
      cacheDuration: 60  # in seconds
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
          cacheDuration: 10
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
3. Otherwise, a 512 status code with the message "Service is in maintenance mode" will be returned.

The plugin extracts client IPs by checking headers in the following order:
1. X-Forwarded-For (first IP if multiple are present)
2. X-Real-IP
3. Request's RemoteAddr

## Parameters

- `endpoint` (required): URL to check maintenance status
- `cacheDuration` (optional): How long to cache maintenance status, specified in seconds as an integer value. Default is 10.

## Установка

Добавьте плагин в `traefik_values.yaml`:
```yaml
experimental:
  plugins:
    maintenanceCheck:
      moduleName: github.com/CitronusAcademy/traefik-maintenance-plugin
      version: "v0.1.0"
```
