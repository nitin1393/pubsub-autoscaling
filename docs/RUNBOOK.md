# Rollout runbook: best practices, step by step

Order matters: each phase has an exit check. Don't start the next phase until
the current one passes.

| Phase | Goal | Exit check |
| --- | --- | --- |
| 0 | Prerequisites | Every box in the table ticked |
| 1 | Size workers from groundcover history | Requests, limits, VPA floor/ceiling set from real numbers |
| 2 | Scaling metrics proven | Each KEDA query returns one correct number |
| 3 | Staging deploy | All Argo CD apps Healthy, ScaledObject READY |
| 4 | Load test | All pass criteria met |
| 5 | Enable VPA + gate | Dry run sane, then live with no OOMs |
| 6 | Production | 1 week clean at max 25, then raise to 100 |
| 7 | Operate | Weekly review, scheduled tuning |

---

## Phase 0: prerequisites

| Check | Command / where | Why |
| --- | --- | --- |
| Kubernetes 1.33+ | `kubectl version` | In-place pod resize; older versions recreate pods |
| metrics-server running | `kubectl top pods -n my-app` | VPA's live CPU/memory samples |
| Argo CD can read the repo | Argo CD > Settings > Repositories | Repo is private |
| Cluster can pull `ghcr.io/nitin1393/vpa-gate` | README, Step 1 | Package is private |
| `groundcover-api-key` Secret created (not in Git) | README | KEDA auth |
| Worker acks **after** processing; idempotent | Code review | Spot reclaims and scale-down redeliver messages |
| Worker flow control ~= KEDA target (20) | Worker config | Otherwise one pod hoards the backlog |
| Worker handles SIGTERM: stop pulling, finish, exit < 90 s | Code review | Clean scale-down |
| Dead-letter topic on the subscription (e.g. 5 attempts) | GCP console | Poison / OOM-causing messages don't loop forever |
| Worker exposes an in-flight gauge on `/metrics` | Worker code | Fast KEDA trigger |

---

## Phase 1: size the workers from groundcover history

Use 30 days (more if you have month-end peaks). Replace `my-worker`.

```promql
# CPU request: p90 of the busiest worker pod (mCPU)
quantile_over_time(0.90, max(groundcover_container_cpu_usage_rate_millis{namespace="my-app",workload_name="my-worker",container_name="worker"})[30d:5m])
# CPU peak (mCPU)
max_over_time(max(groundcover_container_cpu_usage_rate_millis{namespace="my-app",workload_name="my-worker",container_name="worker"})[30d:5m])
# Memory p95 (bytes)
quantile_over_time(0.95, max(groundcover_container_memory_working_set_bytes{namespace="my-app",workload_name="my-worker",container_name="worker"})[30d:5m])
# Memory peak (bytes)
max_over_time(max(groundcover_container_memory_working_set_bytes{namespace="my-app",workload_name="my-worker",container_name="worker"})[30d:5m])
# Throttling today (seconds throttled per second; > 0.1 means CPU-starved)
max(rate(groundcover_container_cpu_throttled_seconds_total{namespace="my-app",workload_name="my-worker"}[5m]))
```

| Setting | Value | File |
| --- | --- | --- |
| CPU request | CPU p90 | `05-worker-deployment.yaml` |
| Memory request | memory p95 x 1.15 | `05-worker-deployment.yaml` |
| Memory limit | memory peak x 1.3 | `05-worker-deployment.yaml` |
| CPU limit | none (avoid throttling) | `05-worker-deployment.yaml` |
| VPA `minAllowed` (floor) | = the two requests | `06-vpa.yaml` |
| VPA `maxAllowed` (ceiling) | >= 2 x the peaks | `06-vpa.yaml` |

Why: VPA keeps only 8 days of memory peaks; your month of history catches
weekly and month-end peaks VPA would miss. The floor means VPA can only make
pods bigger until it has learned.

**Exit:** the numbers are in Git.

---

## Phase 2: prove the scaling metrics

1. Find the real metric names in groundcover's metric explorer for:
   backlog (undelivered messages), oldest unacked message age, worker in-flight.
2. Backlog and age must come from `stackdriver-exporter` scraped every 60 s,
   **not** groundcover's GCP integration (it polls every 5 minutes).
3. Test every query KEDA will run, exactly as written in `02-scaledobject.yaml`:
   ```bash
   curl -sG 'https://app.groundcover.com/api/prometheus/api/v1/query' \
     --data-urlencode 'query=<the query>' -H "Authorization: Bearer $GC_API_KEY"
   ```
   Each must return **one** series with a sensible value.
4. Set the KEDA target from real throughput:
   `threshold = messages one pod processes per second x 30 s` (drain a pod's
   share in about 30 s). Measure per-pod throughput in groundcover from the
   ack rate divided by ready pods during a busy hour.

**Exit:** every query returns one correct number; threshold is measured, not guessed.

---

## Phase 3: staging deploy

1. Staging values: KEDA `maxReplicaCount: 25`, VPA `updateMode: Off`, gate `disabled`.
2. `kubectl apply -n argocd -f argocd/root.yaml`
3. Check:
   ```bash
   kubectl -n argocd get applications            # all Synced + Healthy
   kubectl -n my-app get scaledobject             # READY=True
   kubectl -n my-app get hpa                      # shows targets, not <unknown>
   kubectl -n my-app logs deploy/vpa-gate         # "starting vpa-gate"
   ```

**Exit:** all apps Healthy, ScaledObject READY, HPA shows real metric values.

---

## Phase 4: load test (staging)

Use the load-test kit in [`loadtest/`](../loadtest/README.md): run `watch.py`, then the
`spike` profile (10x normal peak for 15 minutes). Then `flapping`, `sawtooth` and
`sustained`; `soak` overnight. `watch.py` prints PASS/FAIL for the cluster-side checks.

| Pass criterion | How to check |
| --- | --- |
| First new pod Running within 30 s of backlog rising | `kubectl get pods -w`; placeholder pods get preempted |
| Oldest message age stays under 2 min at peak | groundcover |
| No messages lost (published = acked + dead-lettered) | Pub/Sub metrics |
| New nodes appear within ~2 min when headroom runs out | `kubectl get nodeclaims -w` |
| Scale-down is gradual (<= 20%/min after 5 min) and pods exit cleanly | Pod events: no SIGKILL after grace period |
| Fallback works: break the groundcover key -> HPA holds 10 replicas | `kubectl describe scaledobject` |
| Empty nodes removed within a few minutes after drain | `kubectl get nodes` |

**Exit:** every criterion passes. Fix and repeat otherwise.

---

## Phase 5: enable VPA and the gate (day 1 is fine with the floor)

1. Gate in dry run: `gate.dryRun: true` in `argocd/apps/vpa-gate.yaml`.
2. In `06-vpa.yaml`: `pubsub-autoscaling/vpa-gate: "enabled"`, push.
3. For a few hours, read the decisions:
   ```bash
   kubectl -n my-app logs deploy/vpa-gate | grep "would switch"
   ```
   Expect `Initial` during scaling and `InPlaceOrRecreate` only after 15 quiet minutes.
4. Go live: `gate.dryRun: false`, push.
5. Watch for 48 hours:
   ```bash
   kubectl -n my-app get events --field-selector reason=UpdateModeChanged
   kubectl describe vpa my-worker -n my-app        # target vs current requests
   ```
   and the groundcover OOM alert.

**Exit:** no OOMKilled workers, mode switches only around real scaling, no more than a few switches per hour.

---

## Phase 6: production

1. Same as staging with `maxReplicaCount: 25`.
2. Alerts on (groundcover monitors or Grafana/PrometheusRule):
   at max replicas > 10 min; oldest message > 5 min; ScaledObject not ready;
   NodePool CPU > 80% of limit; pods Pending > 3 min; OOMKilled; vpa-gate > 4 switches/hour.
3. After 1 clean week: raise `maxReplicaCount` to 100, within what downstream services can take.

**Exit:** one week at 100 with no alerts that needed action.

---

## Phase 7: operate

| When | Do |
| --- | --- |
| Weekly | Compare VPA target vs requests; check throttling and OOMs; check time at max replicas |
| After 2 weeks of VPA | Lower `minAllowed` (e.g. 250m / 256Mi) so VPA can shrink oversized pods |
| After VPA changes pod size noticeably | Re-measure per-pod throughput and re-tune the KEDA threshold |
| Monthly | Review Spot vs on-demand mix and NodePool CPU limit |
| Upgrades | Bump chart/image versions in Git only; let Argo CD roll them out; staging first |
| Avoid | Restarting the VPA recommender without need: it loses in-memory history |

## Rules that never change
- KEDA scales on queue metrics only. **Never** add a CPU or memory trigger.
- VPA mode is never `Recreate` or `Auto`; only the gate switches it.
- Keep Argo CD's `ignoreDifferences` on the VPA's `updateMode`.
- Workers ack after processing; the subscription has a dead-letter topic.
- No secrets in Git.
- Every change goes through Git and staging first.
