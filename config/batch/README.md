# Local Batch Stack

`config/batch` rebuilds the local batch debugging overlay discussed earlier:

- metadata service with Prometheus metrics enabled
- Redis from the existing metadata base
- MinIO as a local S3-compatible object store
- the AIBrix console
- a standalone Prometheus
- Grafana with the Prometheus datasource preconfigured

This overlay intentionally uses a standalone Prometheus deployment. The repo's
`config/prometheus/monitor.yaml` only creates a `ServiceMonitor` for clusters
that already run Prometheus Operator.

## Prerequisites

Build the images used by the local stack:

```bash
make docker-build-metadata-service
(cd apps/console/web && npm install && npm run build)
docker build -f apps/console/Dockerfile -t aibrix/console:latest .
```

## Deploy

```bash
kubectl apply -k config/batch
```

## Access

Grafana:

```bash
kubectl -n aibrix-system port-forward svc/aibrix-grafana 3000:3000
```

Prometheus:

```bash
kubectl -n aibrix-system port-forward svc/aibrix-prometheus 9090:9090
```

Console:

```bash
kubectl -n aibrix-system port-forward svc/aibrix-console 8080:8080
```

MinIO API and console:

```bash
kubectl -n aibrix-system port-forward svc/aibrix-minio 9000:9000 9001:9001
```

## Notes

- Grafana only provisions the Prometheus datasource. Import dashboards manually
  from their original location when needed.
- The local object store credentials are `minioadmin` / `minioadmin`.
- The bootstrap job creates the `aibrix-batch` bucket automatically.
