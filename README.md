# Pub/Sub worker autoscaling: EKS Auto Mode + KEDA + groundcover

Flow: Pub/Sub backlog -> groundcover -> KEDA -> more pods -> EKS Auto Mode adds nodes.

## 1. Install KEDA
```bash
helm repo add kedacore https://kedacore.github.io/charts
helm repo update
helm install keda kedacore/keda -n keda --create-namespace
```

## 2. Find the metric names in groundcover and test them
Search groundcover's metric explorer for your subscription's backlog
(undelivered messages) and, if present, oldest-unacked-message age.
Then test the exact query KEDA will run:
```bash
export GC_API_KEY=...
curl -sG 'https://app.groundcover.com/api/prometheus/api/v1/query' \
  --data-urlencode 'query=sum(pubsub_subscription_num_undelivered_messages{subscription="my-sub"})' \
  -H "Authorization: Bearer $GC_API_KEY"
```
You need a single number back. Put the working queries into `02-scaledobject.yaml`.

## 3. Edit, then apply in order
- `01-keda-auth.yaml`: groundcover API key
- `02-scaledobject.yaml`: queries, `threshold` (messages per pod), min/max replicas
- `03-nodepool.yaml`: your region's AZs
- `04-overprovisioning.yaml`: size of the spare capacity
- `05-worker-deployment.yaml`: image, resource requests, probes
- `06-vpa.yaml`: VPA for pod right-sizing (needs VPA installed; starts in `Off` mode)

- `07-vpa-gate.yaml`: CronJob that lets VPA resize running pods only after HPA has settled

### VPA gate: resize only after HPA settles
| HPA state | VPA mode | Effect |
| --- | --- | --- |
| Scaled in the last 15 min | `Initial` | New pods get the right size; running pods untouched |
| Stable for 15 min | `InPlaceOrRecreate` | Running pods resized (in place on K8s 1.33+) |

It is off until you enable it, after reviewing a week of `Off`-mode recommendations:
```bash
kubectl annotate vpa my-worker -n my-app pubsub-autoscaling/vpa-gate=enabled --overwrite
kubectl get jobs -n my-app                 # find the latest vpa-gate-<id> job
kubectl logs -n my-app job/vpa-gate-<id>   # shows "Initial -> InPlaceOrRecreate (settled: ...)"
```

KEDA and VPA don't conflict: KEDA scales pod count on queue metrics, VPA sets
pod size from CPU/memory. Never add a CPU/memory trigger to KEDA.

```bash
kubectl apply -f .
```

## 4. Verify
```bash
kubectl get scaledobject -n my-app       # READY=True, ACTIVE=True under load
kubectl get hpa -n my-app                # KEDA-created HPA with current metric values
kubectl get nodeclaims -w                # Auto Mode adding / removing nodes
kubectl logs -n keda deploy/keda-operator | grep -i error
```
