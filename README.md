# Traefik Maintenance Plugin

A Traefik middleware plugin that checks if a service is in maintenance mode and returns a 512 status code if it is.

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
          cacheDuration: "10s"
```

## Endpoint Format

The plugin expects your maintenance status endpoint to return a JSON response in the following format:

```json
{
  "system_config": {
    "maintenance": {
      "is_active": false
    }
  }
}
```

When `maintenance.is_active` is `true`, the middleware will return a 512 status code with the message "Service is in maintenance mode".

## Parameters

- `endpoint` (required): URL to check maintenance status
- `cacheDuration` (optional): How long to cache maintenance status, default is 10 seconds

## Установка

Добавьте плагин в `traefik_values.yaml`:
```yaml
experimental:
  plugins:
    maintenanceCheck:
      moduleName: github.com/CitronusAcademy/traefik-maintenance-plugin
      version: "v0.1.0"
```
