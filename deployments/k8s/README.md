# Kubernetes Deployment

This directory contains a runnable Kubernetes stack for local or single-node clusters.

Included resources:

- `Namespace` `payflow`
- `Secret` and `ConfigMap` for application configuration
- `Deployment` and `Service` for `payflow`
- `Deployment`, `Service`, and `PVC` for PostgreSQL
- `Deployment` and `Service` for Redis
- `Deployment`, `Service`, and `PVC` for Kafka in KRaft mode
- `Job` to create the `payment-events` topic

## Apply

Build the application image first:

```bash
docker build -t payflow:latest .
```

If your Kubernetes cluster does not use the same Docker daemon as your host, load or push the image before applying the manifests.

Examples:

```bash
# kind
kind load docker-image payflow:latest --name <cluster-name>

# minikube
minikube image load payflow:latest
```

Apply the manifests with:

```bash
kubectl apply -k deployments/k8s
```

Check rollout status:

```bash
kubectl -n payflow get pods
kubectl -n payflow get svc
kubectl -n payflow get jobs
```

Wait until:

- `postgres`, `redis`, `kafka`, and `payflow` pods are `Running`
- the `kafka-bootstrap-topics` job is `Completed`

## Access the API

Port-forward the service:

```bash
kubectl -n payflow port-forward svc/payflow 8080:80
```

Then call the API:

```bash
curl http://localhost:8080/health

curl -X POST http://localhost:8080/api/v1/transactions \
  -H "Authorization: Bearer sk_live_maple_001" \
  -H "Content-Type: application/json" \
  -H "X-Idempotency-Key: order_k8s_001" \
  -d '{"amount_cents":15000,"currency":"CAD"}'
```

## Notes

- These manifests are designed for local development and single-node environments, not a HA production cluster.
- The application image is `payflow:latest` with `imagePullPolicy: IfNotPresent`.
- PostgreSQL and Kafka use persistent volume claims and expect a default storage class in the cluster.
- Redis is deployed as a single instance with ephemeral storage.
