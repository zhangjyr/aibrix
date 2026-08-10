# AIBrix Batch Local Config

This overlay is a local-oriented Kubernetes bundle for batch workflows. It
reuses the existing `config/metadata` base, the existing metadata S3 patch, and
the MinIO/template/profile wiring from `k8s-batch-bootstrap.yaml`.

Included components:

- metadata service
- Redis
- MinIO as the local S3-compatible backend
- AIBrix Console
- Prometheus
- Grafana

## Apply

```bash
kubectl apply -k config/batch
```

## Console Image

Build the console image before applying the overlay if your cluster cannot pull
`aibrix/console:latest` directly:

```bash
cd apps/console/web
npm install
npm run build

cd ../../..
docker build -f apps/console/Dockerfile -t aibrix/console:latest .
```

## Port Forward

```bash
kubectl -n aibrix-system port-forward svc/console 8080:8080 &
kubectl -n aibrix-system port-forward svc/grafana 3000:3000 &
kubectl -n aibrix-system port-forward svc/prometheus 9090:9090 &
kubectl -n aibrix-system port-forward svc/minio 9001:9001 &
```

## Defaults

- Console URL: `http://localhost:8080`
- Grafana URL: `http://localhost:3000` with `admin` / `admin`
- Prometheus URL: `http://localhost:9090`
- MinIO Console URL: `http://localhost:9001` with `minioadmin` / `minioadmin`

## Notes

- The console uses SQLite locally because the current console service supports
  `sqlite`, `memory`, and `mysql` stores, not Redis.
- Batch object storage uses MinIO through the same S3-style environment keys as
  the existing metadata S3 patch.
- Metadata Prometheus metrics are enabled, so Prometheus scrapes `/metrics`.
- Console Prometheus metrics are enabled, so Prometheus scrapes
  `/api/v1/metrics`.
- The bundled batch template and profile keep the `mock-vllm` example from
  `k8s-batch-bootstrap.yaml`.
- Import dashboards manually from the existing repo files:
  `apps/console/observability/aibrix-batch-grafana.json` and
  `python/aibrix/observability/aibrix-metadata-grafana.json`.
- The Console playground still expects an AIBrix gateway endpoint. This overlay
  focuses on batch, metadata, and observability only.
