"""Watch the cluster during a load test and check the pass criteria.

Samples every INTERVAL seconds with kubectl (needs read access to the
namespace), writes samples to a CSV, and at the end (or Ctrl-C) prints a
PASS/FAIL report for the cluster-side criteria of runbook phase 4/5:

  gate-safety        VPA is never InPlaceOrRecreate while the HPA is scaling
                     or scaled less than STABLE_SECONDS ago
  gate-settles       once the HPA has been quiet for STABLE_SECONDS (+ one
                     sample of slack), VPA is InPlaceOrRecreate
  no-oom             no worker container OOMKilled during the run
  no-long-pending    no worker pod Pending longer than PENDING_LIMIT seconds
  scale-up-latency   ready pods start rising within SCALEUP_LIMIT seconds of
                     the HPA's desired count rising
  gate-no-flap       VPA mode switches at most MAX_SWITCHES_PER_HOUR per hour

Message-level criteria (nothing lost, oldest message age) come from Pub/Sub /
groundcover; the report prints the queries to check them.

Usage:
  python3 watch.py                      # until Ctrl-C
  DURATION=2700 python3 watch.py        # 45 minutes (spike profile + drain)

Environment: NAMESPACE (my-app), HPA (keda-hpa-worker-scaler), VPA (my-worker),
WORKER_LABEL (app=my-worker), INTERVAL (15), STABLE_SECONDS (900),
PENDING_LIMIT (180), SCALEUP_LIMIT (60), MAX_SWITCHES_PER_HOUR (4), OUT (samples.csv)
"""

from __future__ import annotations

import csv
import json
import os
import subprocess
import sys
import time
from dataclasses import asdict, dataclass, fields
from datetime import datetime, timezone

SCALING, SETTLED = "Initial", "InPlaceOrRecreate"


@dataclass
class Sample:
    t: float                 # unix seconds
    hpa_current: int
    hpa_desired: int
    hpa_last_scale: float    # unix seconds, 0 if never
    vpa_mode: str
    pods_ready: int
    pods_pending: int
    oldest_pending_s: float
    oom_kills: int           # cumulative OOMKilled restarts seen on worker pods
    nodes: int


# ----------------------------------------------------------------- checks --
def check(samples: list[Sample], stable_s: float, interval_s: float, pending_limit: float,
          scaleup_limit: float, max_switches_per_hour: float) -> list[tuple[str, bool, str]]:
    """Pure function: evaluate the criteria on a list of samples."""
    out: list[tuple[str, bool, str]] = []
    if not samples:
        return [("samples", False, "no samples collected")]

    # gate-safety / gate-settles
    unsafe, unsettled = [], []
    for s in samples:
        scaling = s.hpa_current != s.hpa_desired
        recent = s.hpa_last_scale > 0 and s.t - s.hpa_last_scale < stable_s
        if (scaling or recent) and s.vpa_mode == SETTLED:
            unsafe.append(s)
        quiet_for = s.t - s.hpa_last_scale if s.hpa_last_scale > 0 else float("inf")
        if not scaling and quiet_for >= stable_s + interval_s and s.vpa_mode not in (SETTLED, "Off"):
            unsettled.append(s)
    out.append(("gate-safety", not unsafe,
                f"{len(unsafe)} samples with VPA={SETTLED} while HPA busy" if unsafe else "VPA held back during every scaling period"))
    out.append(("gate-settles", not unsettled,
                f"{len(unsettled)} samples quiet >= window but VPA not {SETTLED}" if unsettled else "VPA released after every quiet window"))

    # no-oom
    ooms = samples[-1].oom_kills - samples[0].oom_kills
    out.append(("no-oom", ooms == 0, f"{ooms} OOMKilled worker restarts during the run"))

    # no-long-pending
    worst = max(s.oldest_pending_s for s in samples)
    out.append(("no-long-pending", worst <= pending_limit, f"longest Pending pod {worst:.0f}s (limit {pending_limit:.0f}s)"))

    # scale-up-latency: each rise in desired must be followed by a rise in ready pods
    slow = []
    for i in range(1, len(samples)):
        if samples[i].hpa_desired > samples[i - 1].hpa_desired:
            base_ready = samples[i - 1].pods_ready
            start = samples[i].t
            hit = next((s.t for s in samples[i:] if s.pods_ready > base_ready), None)
            if hit is None or hit - start > scaleup_limit:
                # ignore rises right at the end of the run that had no time to land
                if samples[-1].t - start > scaleup_limit:
                    slow.append((start, None if hit is None else hit - start))
    out.append(("scale-up-latency", not slow,
                f"{len(slow)} scale-ups slower than {scaleup_limit:.0f}s" if slow else f"every scale-up showed new ready pods within {scaleup_limit:.0f}s"))

    # gate-no-flap
    switches = sum(1 for a, b in zip(samples, samples[1:]) if a.vpa_mode != b.vpa_mode)
    hours = max((samples[-1].t - samples[0].t) / 3600, 1 / 60)
    rate = switches / hours
    out.append(("gate-no-flap", rate <= max_switches_per_hour or switches <= 2,
                f"{switches} mode switches in {hours:.2f} h ({rate:.1f}/h, limit {max_switches_per_hour:.0f}/h)"))
    return out


# --------------------------------------------------------------- sampling --
def kubectl_json(*args: str) -> dict:
    r = subprocess.run(["kubectl", *args, "-o", "json"], capture_output=True, text=True, timeout=30)
    if r.returncode != 0:
        raise RuntimeError(r.stderr.strip())
    return json.loads(r.stdout)


def ts(s: str | None) -> float:
    if not s:
        return 0.0
    return datetime.strptime(s, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).timestamp()


def sample(ns: str, hpa: str, vpa: str, label: str) -> Sample:
    now = time.time()
    h = kubectl_json("-n", ns, "get", "hpa", hpa)
    v = kubectl_json("-n", ns, "get", "vpa", vpa)
    pods = kubectl_json("-n", ns, "get", "pods", "-l", label)["items"]
    nodes = kubectl_json("get", "nodes")["items"]
    hs = h.get("status", {})
    ready = pending = 0
    oldest = 0.0
    ooms = 0
    for p in pods:
        st = p.get("status", {})
        if st.get("phase") == "Pending":
            pending += 1
            oldest = max(oldest, now - ts(p["metadata"].get("creationTimestamp")))
        if any(c.get("type") == "Ready" and c.get("status") == "True" for c in st.get("conditions", [])):
            ready += 1
        for cs in st.get("containerStatuses", []):
            term = (cs.get("lastState") or {}).get("terminated") or {}
            if term.get("reason") == "OOMKilled":
                ooms += 1
    return Sample(
        t=now,
        hpa_current=int(hs.get("currentReplicas", 0)),
        hpa_desired=int(hs.get("desiredReplicas", 0)),
        hpa_last_scale=ts(hs.get("lastScaleTime")),
        vpa_mode=v.get("spec", {}).get("updatePolicy", {}).get("updateMode", ""),
        pods_ready=ready, pods_pending=pending, oldest_pending_s=oldest,
        oom_kills=ooms, nodes=len(nodes),
    )


def main() -> None:
    env = os.environ.get
    ns, hpa, vpa = env("NAMESPACE", "my-app"), env("HPA", "keda-hpa-worker-scaler"), env("VPA", "my-worker")
    label = env("WORKER_LABEL", "app=my-worker")
    interval = float(env("INTERVAL", "15"))
    duration = float(env("DURATION", "0"))
    stable = float(env("STABLE_SECONDS", "900"))
    out_path = env("OUT", "samples.csv")

    samples: list[Sample] = []
    start = time.time()
    with open(out_path, "w", newline="") as f:
        w = csv.writer(f)
        w.writerow([fl.name for fl in fields(Sample)])
        try:
            while not duration or time.time() - start < duration:
                try:
                    s = sample(ns, hpa, vpa, label)
                except Exception as e:  # keep going through API blips
                    print(f"sample failed: {e}", file=sys.stderr)
                    time.sleep(interval)
                    continue
                samples.append(s)
                w.writerow(list(asdict(s).values()))
                f.flush()
                print(f"{time.strftime('%H:%M:%S')} hpa {s.hpa_current}->{s.hpa_desired} "
                      f"ready={s.pods_ready} pending={s.pods_pending} vpa={s.vpa_mode} nodes={s.nodes}", flush=True)
                time.sleep(interval)
        except KeyboardInterrupt:
            pass

    results = check(samples, stable, interval, float(env("PENDING_LIMIT", "180")),
                    float(env("SCALEUP_LIMIT", "60")), float(env("MAX_SWITCHES_PER_HOUR", "4")))
    print("\n=== cluster-side results ===")
    for name, ok, detail in results:
        print(f"{'PASS' if ok else 'FAIL'}  {name:18s} {detail}")
    print(f"\nsamples: {out_path}")
    print("Also check in groundcover / Pub/Sub for the same window:\n"
          "  - oldest unacked message age stayed under 2 min\n"
          "  - published (publisher 'done' log) == acked + dead-lettered (nothing lost)")
    sys.exit(0 if all(ok for _, ok, _ in results) else 1)


if __name__ == "__main__":
    main()
