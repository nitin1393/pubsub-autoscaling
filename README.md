# Pub/Sub worker autoscaling: EKS Auto Mode + KEDA + VPA + groundcover

Queue-driven autoscaling for Pub/Sub consumers, deployed with Argo CD.

**Start here: [docs/RUNBOOK.md](docs/RUNBOOK.md)**, the best-practice rollout in
7 phases, each with an exit check (prerequisites, sizing from groundcover
history, proving metrics, staging, load test, enabling VPA, production, operating).

```
Pub/Sub backlog -> groundcover -> KEDA/HPA -> pod count -> EKS Auto Mode adds nodes
                                   VPA      -> pod size  (gated by vpa-gate)
```

| Piece | What it does | Where |
| --- | --- | --- |
| KEDA ScaledObject | Pod **count** from queue backlog (via groundcover PromQL) | `01-keda-auth.yaml`, `02-scaledobject.yaml` |
| Auto Mode NodePool | Nodes for the workers, Spot first | `03-nodepool.yaml` |
| Placeholder pods | Spare capacity so new workers start instantly | `04-overprovisioning.yaml` |
| Worker Deployment + PDB | The consumer itself | `05-worker-deployment.yaml` |
| VPA | Pod **size** (CPU/memory requests) | `06-vpa.yaml` |
| vpa-gate controller | Lets VPA resize running pods only after HPA settles | `controller/` (code), `charts/vpa-gate/` (Helm chart) |
| Argo CD apps | KEDA, VPA, this repo and vpa-gate, as an app of apps | `argocd/` |

## vpa-gate: resize only after HPA settles

| HPA state | VPA mode | What happens |
| --- | --- | --- |
| Scaled in the last 15 minutes, or still scaling | `Initial` | New pods get the right size; running pods aren't touched |
| No scaling for 15 minutes | `InPlaceOrRecreate` | Running pods are resized; on Kubernetes 1.33 or later without a restart |

A small Go controller (controller-runtime) in `controller/`:
- watches HPAs, so it switches to `Initial` within seconds of scaling
- re-checks exactly when the stable window ends (no polling)
- only touches VPAs that opt in with annotations, kept in Git:
  ```yaml
  pubsub-autoscaling/vpa-gate: "enabled"            # "disabled" = leave this VPA alone
  pubsub-autoscaling/hpa-name: "keda-hpa-worker-scaler"
  pubsub-autoscaling/stable-seconds: "900"          # optional
  ```
- records a Kubernetes Event (`UpdateModeChanged`) on the VPA for every switch,
  logs JSON, and exposes `vpa_gate_settled` and `vpa_gate_mode_switches_total`
  on `:8080/metrics`
- supports dry run to log decisions without changing anything

---

## Deploy the vpa-gate controller

### Before you start
| Need | Check |
| --- | --- |
| VPA installed (recommender, updater, admission controller) | `kubectl get crd verticalpodautoscalers.autoscaling.k8s.io` |
| The HPA it watches exists (KEDA creates `keda-hpa-<ScaledObject>`) | `kubectl -n my-app get hpa` |
| Kubernetes 1.33+ for in-place resize (older: pods are recreated) | `kubectl version` |
| Cluster can pull `ghcr.io/nitin1393/vpa-gate` | see step 1 |

### Step 1: image access (once)
The GHCR package is private because this repo is private. Pick one:
- **Make it public:** GitHub -> your profile -> Packages -> `vpa-gate` ->
  Package settings -> Change visibility -> Public. Do the same for `charts/vpa-gate`
  if you install the chart from GHCR.
- **Or use a pull secret** (a GitHub token with `read:packages`):
  ```bash
  kubectl -n my-app create secret docker-registry ghcr-pull \
    --docker-server=ghcr.io --docker-username=nitin1393 \
    --docker-password=<GITHUB_TOKEN_WITH_read:packages>
  ```
  and set `imagePullSecrets: [{name: ghcr-pull}]` in the chart values.

### Option A: Argo CD (recommended)
`argocd/apps/vpa-gate.yaml` deploys the chart from this repo. It is part of the
app of apps, so it is installed with everything else:
```bash
kubectl apply -n argocd -f argocd/root.yaml
```
To change settings, edit `valuesObject` in `argocd/apps/vpa-gate.yaml` and push.

### Option B: Helm CLI, from this repo
```bash
git clone https://github.com/nitin1393/pubsub-autoscaling.git
cd pubsub-autoscaling

# 1. Dry run first: logs what it would do, changes nothing
helm upgrade --install vpa-gate charts/vpa-gate -n my-app \
  --set watchNamespace=my-app --set gate.dryRun=true

kubectl -n my-app rollout status deploy/vpa-gate
kubectl -n my-app logs deploy/vpa-gate          # look for "would switch vpa mode"

# 2. Go live
helm upgrade vpa-gate charts/vpa-gate -n my-app --reuse-values --set gate.dryRun=false
```

### Option C: Helm CLI, from GHCR (no clone)
CI publishes the chart as an OCI artifact on every push to `main`:
```bash
helm registry login ghcr.io -u nitin1393      # only while the package is private
helm upgrade --install vpa-gate oci://ghcr.io/nitin1393/charts/vpa-gate \
  --version 0.1.1 -n my-app --set watchNamespace=my-app
```

### Step 2: turn the gate on for your VPA
The controller ignores every VPA until it opts in. After the first week of
`updateMode: Off` recommendations, in `06-vpa.yaml` change:
```yaml
pubsub-autoscaling/vpa-gate: "disabled"   ->   "enabled"
```
and push (Argo CD), or `kubectl annotate vpa my-worker -n my-app pubsub-autoscaling/vpa-gate=enabled --overwrite`
if you're not using Argo CD.

### Step 3: verify
```bash
kubectl -n my-app get deploy vpa-gate                                  # 1/1 ready
kubectl -n my-app logs deploy/vpa-gate                                 # "switched vpa mode" lines
kubectl -n my-app get events --field-selector reason=UpdateModeChanged
kubectl -n my-app get vpa my-worker -o jsonpath='{.spec.updatePolicy.updateMode}'; echo
kubectl -n my-app port-forward deploy/vpa-gate 8080 &  curl -s localhost:8080/metrics | grep vpa_gate_
```

### Chart values you're most likely to change
| Value | Default | Meaning |
| --- | --- | --- |
| `watchNamespace` | release namespace | Namespace of the HPA and VPA |
| `watchAllNamespaces` | `false` | Manage gated VPAs everywhere (ClusterRole) |
| `gate.stableWindow` | `15m` | Quiet time before running pods may be resized |
| `gate.scalingMode` / `gate.settledMode` | `Initial` / `InPlaceOrRecreate` | The two VPA modes |
| `gate.dryRun` | `false` | Log decisions only |
| `replicaCount` | `1` | `2` is safe (leader election) |
| `metrics.podAnnotations` | `true` | groundcover scrapes the pod |
| `metrics.serviceMonitor.enabled` | `false` | Prometheus Operator scraping (Grafana option) |
| `imagePullSecrets` | `[]` | While GHCR is private |

### Argo CD and the VPA's updateMode
The workload app (`argocd/apps/pubsub-autoscaling.yaml`) ignores the VPA's
`updateMode` (`ignoreDifferences` + `RespectIgnoreDifferences=true`). Without
this, Argo CD self-heal would reset it to `Off` and fight the controller.

### Build and release
GitHub Actions (`.github/workflows/vpa-gate.yaml`) on changes to `controller/` or `charts/`:
- Go: gofmt, vet, `go test -race`; Chart: `helm lint --strict` + render 3 configurations
- Image: `ghcr.io/nitin1393/vpa-gate:sha-<commit>` and `:main` (amd64 + arm64)
- Chart: `oci://ghcr.io/nitin1393/charts/vpa-gate:<Chart.yaml version>`

To release: set `appVersion` in `charts/vpa-gate/Chart.yaml` to the new
`sha-<commit>` image tag, bump `version`, push. Argo CD rolls it out.

Local: `cd controller && go test ./... && go build ./cmd`

---

## Deploy the rest with Argo CD

1. **Repo access.** This repo is private: add it in Argo CD
   (Settings -> Repositories) with a GitHub token or deploy key.
2. **groundcover API key** (kept out of Git):
   ```bash
   kubectl create namespace my-app
   kubectl -n my-app create secret generic groundcover-api-key \
     --from-literal=bearerToken=<YOUR_GROUNDCOVER_API_KEY>
   ```
3. **Edit before syncing:** metric queries and `threshold` in `02-scaledobject.yaml`
   (test them first, below), AZs in `03-nodepool.yaml`, image and requests in
   `05-worker-deployment.yaml`.
4. **Apply the root app once:**
   ```bash
   kubectl apply -n argocd -f argocd/root.yaml
   ```
   Argo CD installs KEDA and VPA (sync wave -1), then the workload and vpa-gate (wave 0).

### Test a groundcover query
```bash
curl -sG 'https://app.groundcover.com/api/prometheus/api/v1/query' \
  --data-urlencode 'query=sum(pubsub_subscription_num_undelivered_messages{subscription="my-sub"})' \
  -H "Authorization: Bearer $GC_API_KEY"
```

## Rollout of VPA
See [docs/RUNBOOK.md](docs/RUNBOOK.md), phases 1 and 5. In short: size requests
from groundcover history, set them as the VPA floor (`minAllowed`), enable the
gate in dry run, then live. VPA uses `RequestsAndLimits` so it can actually
prevent OOMs. Lower the floor after about 2 weeks.

KEDA and VPA don't conflict: KEDA scales pod count on queue metrics, VPA sets
pod size from CPU/memory. Never add a CPU/memory trigger to KEDA.

## Verify everything
```bash
kubectl get scaledobject,hpa,vpa -n my-app
kubectl logs -n my-app deploy/vpa-gate
kubectl get nodeclaims -w
```
