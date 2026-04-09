# Apps

Public apps attach to the shared `Gateway` in `gateway-system` with `HTTPRoute`.

Constraints:
- hostnames must be under your cluster base domain, for example `*.intar.app`
- routes should bind to `stardrive-public`
- HTTPS is terminated by the platform wildcard certificate

Example shape:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: example
  namespace: app
spec:
  parentRefs:
    - name: stardrive-public
      namespace: gateway-system
      sectionName: https
  hostnames:
    - app.intar.app
  rules:
    - backendRefs:
        - name: example
          port: 8080
```
