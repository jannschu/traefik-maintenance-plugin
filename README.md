# Traefik Maintenance Plugin

Этот плагин для Traefik блокирует запросы, если сервис находится в режиме обслуживания.

## Установка

Добавьте плагин в `traefik_values.yaml`:
```yaml
experimental:
  plugins:
    maintenanceCheck:
      moduleName: github.com/CitronusAcademy/traefik-maintenance-plugin
