# Pub/Sub worker autoscaling: EKS Auto Mode + KEDA + VPA + groundcover

Queue-driven autoscaling for Pub/Sub consumers, deployed with Argo CD.

```
Pub/Sub backlog -> groundcover -> KEDA/HPA -> pod count -> EKS Auto Mode adds nodes
                                   VPA      -> pod size  (gated by vpa-gate)
```

| Piece | What it does | File |
| --- | --- | --- |
| KEDA ScaledObject | Pod **count** from queue backlog (via groundcover PromQL) | `01-keda-auth.yaml`, `02-scaledobject.yaml` |
| Auto Mode NodePool | Nodes for the workers, Spot first | `03-nodepool.yaml` |
| Placeholder pods | Spare capacity so new workers start instantly | `04-overprovisioning.yaml` |
| Worker Deployment + PDB | The consumer itself | `05-worker-deployment.yaml` |
| VPA | Pod **size** (CPU/memory requests) | `06-vpa.yaml` |
| vpa-gate controller | Lets VPA resize running pods only after HPA settles | `07-vpa-gate.yaml`, `controller/` |
| Argo CD apps | KEDA, VPA and this repo, as an app of apps | `argocd/` |

## vpa-gate: resize only after HPA settles

| HPA state | VPA mode | What happens |
| --- | --- | --- |
| Scaled in the last 15 minutes, or still scaling | `Initial` | New pods get the right size; running pods aren't touched |
| No scaling for 15 minutes | `InPlaceOrRecreate` | Running pods are resized; on Kubernetes 1.33 or later without a restart |

A small Go controller (controller-runtime) in `controller/`:
- watches HPAs, so it switches to `Initial` within seconds of scaling
- re-checks exactly when the stable window ends (no polling)
- is configured per VPA with annotations, kept in Git:
  ```yaml
  pubsub-autoscaling/vpa-gate: "enabled"            # "disabled" = leave this VPA alone
  pubsub-autoscaling/hpa-name: "keda-hpa-worker-scaler"
  pubsub-autoscaling/stable-seconds: "900"
  ```
- records a Kubernetes Event on the VPA for every switch, logs JSON, and exposes
  `vpa_gate_settled` and `vpa_gate_mode_switches_total` on `:8080/metrics`
  (annotated for groundcover scraping)
- supports `--dry-run=true` to log decisions without changing anything

Argo CD is told to ignore the VPA's `updateMode` (`ignoreDifferences` +
`RespectIgnoreDifferences`), otherwise self-heal would undo the controller.

### Build
GitHub Actions (`.github/workflows/vpa-gate.yaml`) runs gofmt, vet and tests on
every change under `controller/`, and pushes a multi-arch image to
`ghcr.io/nitin1393/vpa-gate`:
- push to `main` -> tags `main` and `sha-<commit>`
- git tag `vpa-gate-v0.1.0` -> tag `0.1.0`

`07-vpa-gate.yaml` pins an exact `sha-<commit>` build. To release a new version,
bump that tag in Git and Argo CD rolls it out.

Local: `cd controller && go test ./... && go build ./cmd`

## Deploy with Argo CD

1. **Repo access.** This repo is private: add it in Argo CD
   (Settings -> Repositories) with a GitHub token or deploy key.
2. **Image access.** GHCR packages from a private repo are private. Either make
   the `vpa-gate` package public (GitHub -> Packages -> vpa-gate -> Settings), or:
   ```bash
   kubectl -n my-app create secret docker-registry ghcr-pull \
     --docker-server=ghcr.io --docker-username=nitin1393 --docker-password=<PAT with read:packages>
   ```
   and uncomment `imagePullSecrets` in `07-vpa-gate.yaml`.
3. **groundcover API key** (kept out of Git):
   ```bash
   kubectl create namespace my-app
   kubectl -n my-app create secret generic groundcover-api-key \
     --from-literal=bearerToken=<YOUR_GROUNDCOVER_API_KEY>
   ```
4. **Edit before syncing:** metric queries and `threshold` in `02-scaledobject.yaml`
   (test them first, below), AZs in `03-nodepool.yaml`, image and requests in
   `05-worker-deployment.yaml`.
5. **Apply the root app once:**
   ```bash
   kubectl apply -n argocd -f argocd/root.yaml
   ```
   Argo CD installs KEDA and VPA (sync wave -1), then this repo (wave 0).

### Test a groundcover query
```bash
curl -sG 'https://app.groundcover.com/api/prometheus/api/v1/query' \
  --data-urlencode 'query=sum(pubsub_subscription_num_undelivered_messages{subscription="my-sub"})' \
  -H "Authorization: Bearer $GC_API_KEY"
```

## Rollout of VPA
1. Week 1: VPA `updateMode: Off`, gate `disabled`. Read recommendations:
   `kubectl describe vpa my-worker -n my-app`
2. Set worker requests near the recommendation; re-tune KEDA `threshold`.
3. In Git, set `pubsub-autoscaling/vpa-gate: "enabled"` in `06-vpa.yaml` and push.
   Watch: `kubectl get events -n my-app --field-selector reason=UpdateModeChanged`

KEDA and VPA don't conflict: KEDA scales pod count on queue metrics, VPA sets
pod size from CPU/memory. Never add a CPU/memory trigger to KEDA.

## Verify
```bash
kubectl get scaledobject,hpa,vpa -n my-app
kubectl logs -n my-app deploy/vpa-gate
kubectl get nodeclaims -w
```
