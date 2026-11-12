# Stress testing

Two levels:

| Level | What | Where it runs |
| --- | --- | --- |
| Controller | Simulated traffic days, flapping, 200 VPAs in parallel, fuzzing, benchmarks | CI on every push (`controller/`) |
| Cluster | Real Pub/Sub traffic through KEDA, HPA, VPA, vpa-gate and Auto Mode | Your staging cluster, on demand (this folder) |

## Controller stress tests (CI, or locally)

```bash
cd controller
go test ./... -race -short                       # unit + 3 simulated days, race detector
go test ./internal/controller -run TestStress -v # 20 simulated 8-hour days, ~1 min
go test ./internal/gate -run '^$' -fuzz FuzzDecide -fuzztime 60s
go test ./internal/controller -run '^$' -bench . -benchmem
```

What they prove, at every simulated 30 s step:
- VPA is `Initial` whenever the HPA is scaling or scaled in the last 15 min
- VPA is `InPlaceOrRecreate` as soon as the HPA has been quiet for 15 min
- no flapping: few mode switches despite hundreds of HPA scale events per day
- 200 VPA/HPA pairs reconciled from 32 goroutines end in the right modes (race-free)
- the decision logic holds for any input (fuzzing, millions of cases)

## Cluster stress test (staging)

### 1. One-time setup
```bash
# service account key with roles/pubsub.publisher on the worker's topic
kubectl create namespace loadtest
kubectl -n loadtest create secret generic pubsub-loadtest-sa --from-file=key.json=./sa-key.json
```
Edit `job.yaml`: `PROJECT_ID`, `TOPIC_ID`, and `BASE_RATE` = your normal peak messages/s.

### 2. Run a profile
In one terminal, start the watcher (needs `kubectl` read access to the worker namespace).
Testing HPA only, before VPA is installed? Add `VPA=none` (gate checks are skipped):
```bash
cd loadtest
DURATION=2700 python3 watch.py        # 45 min: spike profile (40 min) + drain
```
In another, start the load:
```bash
kubectl apply -k loadtest/
kubectl -n loadtest logs -f job/pubsub-loadtest
```
To run another profile, delete the Job, change `PROFILE`, apply again:
`kubectl -n loadtest delete job pubsub-loadtest`

| Profile | Shape | Tests |
| --- | --- | --- |
| `spike` | 1x 5 min, **10x 15 min**, 1x 20 min | Runbook phase 4: scale-up speed, headroom, drain, gate safety |
| `flapping` | 1x / 8x every 60 s for 30 min | Gate holds `Initial` the whole time; no VPA churn |
| `sawtooth` | 1x to 10x ramps every 10 min, 6 times | Rapid scale up and down, node consolidation |
| `sustained` | 5x for 60 min | Memory growth, CPU throttling, VPA raising sizes after settling |
| `soak` | 2x for 6 h | Leaks and slow drift |

Recommended order: `spike`, `flapping`, `sawtooth`, `sustained`, then `soak` overnight.

### 3. Read the result
`watch.py` prints PASS/FAIL for the cluster-side criteria and saves `samples.csv`:

| Check | Pass when |
| --- | --- |
| `gate-safety` | VPA never `InPlaceOrRecreate` while the HPA is busy |
| `gate-settles` | VPA released after every 15-minute quiet window |
| `no-oom` | No worker OOMKilled |
| `no-long-pending` | No worker pod Pending > 3 min |
| `scale-up-latency` | New ready pods within 60 s of each scale-up |
| `gate-no-flap` | At most 4 VPA mode switches per hour |

Then check in groundcover / Pub/Sub for the same window:
- oldest unacked message age stayed under 2 minutes
- the publisher's `done` log `published` count == acked + dead-lettered (nothing lost)

Extra failure drills (one at a time, during a `sustained` run):
- delete the groundcover key Secret: HPA should hold the 10-replica fallback; restore it
- `kubectl -n my-app delete pod -l app.kubernetes.io/name=vpa-gate`: a new leader takes over, mode unchanged
- cordon a node with workers: pods reschedule within the PDB (max 20% at once)

Local tests for this kit: `python3 -m pytest loadtest`
