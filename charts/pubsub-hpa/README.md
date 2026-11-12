# pubsub-hpa chart: HPA first

Queue-driven horizontal autoscaling for a Pub/Sub worker with KEDA. Install
this alone to test HPA before adding VPA and the vpa-gate.

What it creates: a KEDA `ScaledObject` (KEDA then creates the HPA
`keda-hpa-worker-scaler`), the `TriggerAuthentication` for the metrics source,
and optionally an EKS Auto Mode `NodePool` and placeholder pods for headroom.

> Use this chart **or** the plain `01-keda-auth.yaml` + `02-scaledobject.yaml`
> files, never both: two ScaledObjects on one Deployment fight each other.

## What you need

| Need | Why |
| --- | --- |
| KEDA installed | Reads the queue and drives the HPA |
| Your worker Deployment running | The thing being scaled (`target.name`) |
| One Secret with credentials for the metrics source | Default source `gcpPubsub`: a GCP service account key with `roles/monitoring.viewer` |

## Step by step (HPA test with `metricsSource: gcpPubsub`)

### 1. Install KEDA
```bash
helm repo add kedacore https://kedacore.github.io/charts && helm repo update
helm install keda kedacore/keda -n keda --create-namespace --version 2.21.0
kubectl -n keda get pods            # keda-operator, keda-operator-metrics-apiserver Running
```

### 2. Give KEDA read access to Pub/Sub metrics
```bash
PROJECT=my-gcp-project
gcloud iam service-accounts create keda-pubsub-scaler --project "$PROJECT"
gcloud projects add-iam-policy-binding "$PROJECT" \
  --member "serviceAccount:keda-pubsub-scaler@$PROJECT.iam.gserviceaccount.com" \
  --role roles/monitoring.viewer
gcloud iam service-accounts keys create key.json \
  --iam-account "keda-pubsub-scaler@$PROJECT.iam.gserviceaccount.com"

kubectl -n my-app create secret generic pubsub-scaler-gcp \
  --from-file=GOOGLE_APPLICATION_CREDENTIALS_JSON=key.json
rm key.json
```
(If your organization blocks service account keys, use the `groundcover` or
`prometheus` source instead.)

### 3. Write your values
```yaml
# my-hpa-values.yaml
target:
  name: my-worker                     # your worker Deployment
gcpPubsub:
  subscription: projects/my-gcp-project/subscriptions/my-sub
replicas:
  min: 2
  max: 25                             # staging limit
triggers:
  backlog:
    target: 20                        # messages per pod; re-measure later
```

### 4. Install
From the repo:
```bash
helm upgrade --install pubsub-hpa charts/pubsub-hpa -n my-app -f my-hpa-values.yaml
```
Or from GHCR without cloning:
```bash
helm upgrade --install pubsub-hpa oci://ghcr.io/nitin1393/charts/pubsub-hpa \
  --version 0.1.0 -n my-app -f my-hpa-values.yaml
```

### 5. Check it works
```bash
kubectl -n my-app get scaledobject worker-scaler       # READY=True, ACTIVE=True under load
kubectl -n my-app get hpa keda-hpa-worker-scaler       # TARGETS show numbers, not <unknown>
kubectl -n my-app describe scaledobject worker-scaler  # events show metric errors, if any
kubectl -n keda logs deploy/keda-operator | grep -i error
```
`<unknown>` targets or READY=False almost always mean a wrong subscription
name or a missing/incorrect Secret.

### 6. Load test (HPA only)
See [../../loadtest/README.md](../../loadtest/README.md). Run the watcher with VPA checks off:
```bash
cd loadtest
VPA=none DURATION=2700 python3 watch.py
```
and start the `spike` profile. Pass when `scale-up-latency`, `no-long-pending`,
`no-oom` and `hpa-caught-up` all PASS, the oldest message stays under 2 minutes,
and nothing is lost.

### 7. Next
- Switch `metricsSource` to `groundcover` once the exporter feeds it (same
  numbers in your dashboards), or `prometheus` for in-cluster scaling.
- Turn on `nodePool.enabled` and `headroom.enabled` for dedicated Spot nodes and
  instant starts (the worker then needs the NodePool toleration and
  `priorityClassName: pubsub-worker`).
- Then add VPA and the vpa-gate (runbook phase 5).

## Values

| Value | Default | Meaning |
| --- | --- | --- |
| `target.name` | `my-worker` | Deployment to scale |
| `scaledObjectName` | `worker-scaler` | HPA becomes `keda-hpa-<this>` (the VPA gate expects `keda-hpa-worker-scaler`) |
| `replicas.min` / `max` | 2 / 25 | Pod range |
| `pollingInterval` | 15 | Seconds between metric checks |
| `fallback` | 10 replicas after 3 failed polls | Safe count if metrics fail |
| `behavior.scaleUp` | +100% or +10 pods per 15 s, no wait | Fast up |
| `behavior.scaleDown` | -20% per 60 s after 300 s | Slow down |
| `metricsSource` | `gcpPubsub` | `gcpPubsub`, `groundcover` or `prometheus` |
| `gcpPubsub.subscription` | placeholder (must set) | `projects/<p>/subscriptions/<s>` |
| `gcpPubsub.existingSecret` / `secretKey` | `pubsub-scaler-gcp` / `GOOGLE_APPLICATION_CREDENTIALS_JSON` | GCP key Secret |
| `groundcover.*` | API URL, Secret `groundcover-api-key`, `backendId` | groundcover source |
| `prometheus.*` | in-cluster URL; `authModes` `bearer`/`basic` + Secret for Grafana Cloud | Prometheus source |
| `triggers.backlog` | on, 20 per pod, activation 0 | Pods = undelivered / 20 |
| `triggers.oldestAge` | on, 60 s | Pods = oldest age / 60 s |
| `triggers.inFlight` | off, 16 per pod, source `groundcover` | Needs the worker's in-flight metric |
| `nodePool.*` | off | Auto Mode NodePool (Spot first, 3 AZs, 400 vCPU cap) |
| `headroom.*` | off; 2 x (2 CPU / 4Gi) | Placeholder pods |

The chart refuses bad settings with a clear message (placeholder subscription,
unknown source, max < min, no trigger enabled).

## Tests
`helm lint` on every `ci/*.yaml` value set and 14 render tests run in CI:
`HELM=helm python3 -m pytest charts/pubsub-hpa/tests`
