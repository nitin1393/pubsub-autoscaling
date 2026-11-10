"""Pub/Sub load generator for autoscaling stress tests.

Publishes messages to a topic following a traffic profile, so you can watch
KEDA/HPA, VPA, the vpa-gate controller and EKS Auto Mode react.

Profiles (rates are messages per second; BASE_RATE scales all of them):
  spike      base for 5 min, 10x for 15 min, base for 20 min   (runbook phase 4)
  sustained  5x for 60 min                                     (memory growth, throttling)
  sawtooth   ramps 1x -> 10x every 10 min, 6 times              (rapid scale up/down)
  flapping   alternates 1x / 8x every 60 s for 30 min           (vpa-gate must hold Initial)
  soak       2x for 6 h                                         (leaks, slow drift)

Environment:
  PROJECT_ID, TOPIC_ID            required
  PROFILE                         spike (default) | sustained | sawtooth | flapping | soak
  BASE_RATE                       messages/s at 1x (default 20)
  PAYLOAD_BYTES                   message size (default 1024)
  GOOGLE_APPLICATION_CREDENTIALS  service account key with roles/pubsub.publisher

Every message carries attributes loadtest_run and seq, so the worker or a
check can count what was published vs acked.
"""

from __future__ import annotations

import json
import os
import signal
import sys
import time
import uuid
from concurrent import futures

PROFILES: dict[str, list[tuple[float, int]]] = {
    # (multiplier, seconds)
    "spike": [(1, 300), (10, 900), (1, 1200)],
    "sustained": [(5, 3600)],
    "sawtooth": [(m, 60) for _ in range(6) for m in (1, 2, 4, 6, 8, 10, 8, 4, 2, 1)],
    "flapping": [(m, 60) for _ in range(15) for m in (1, 8)],
    "soak": [(2, 6 * 3600)],
}


def log(**fields) -> None:
    fields["ts"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    print(json.dumps(fields), flush=True)


def plan(profile: str, base: float) -> list[tuple[float, int]]:
    if profile not in PROFILES:
        raise SystemExit(f"unknown PROFILE {profile!r}; choose from {', '.join(PROFILES)}")
    return [(mult * base, secs) for mult, secs in PROFILES[profile]]


def run() -> None:
    from google.cloud import pubsub_v1

    project = os.environ["PROJECT_ID"]
    topic_id = os.environ["TOPIC_ID"]
    profile = os.environ.get("PROFILE", "spike")
    base = float(os.environ.get("BASE_RATE", "20"))
    payload = b"x" * int(os.environ.get("PAYLOAD_BYTES", "1024"))
    run_id = os.environ.get("RUN_ID", uuid.uuid4().hex[:8])

    publisher = pubsub_v1.PublisherClient(
        batch_settings=pubsub_v1.types.BatchSettings(max_messages=500, max_latency=0.05)
    )
    topic = publisher.topic_path(project, topic_id)
    stop = False

    def _stop(*_):
        nonlocal stop
        stop = True

    signal.signal(signal.SIGTERM, _stop)
    signal.signal(signal.SIGINT, _stop)

    steps = plan(profile, base)
    total_s = sum(s for _, s in steps)
    log(msg="start", run_id=run_id, profile=profile, base_rate=base,
        duration_s=total_s, expected_messages=int(sum(r * s for r, s in steps)))

    seq, failed, pending = 0, 0, []
    for rate, secs in steps:
        if stop:
            break
        log(msg="phase", rate_per_s=rate, seconds=secs)
        phase_end = time.monotonic() + secs
        next_report = time.monotonic() + 30
        while time.monotonic() < phase_end and not stop:
            tick = time.monotonic()
            for _ in range(max(1, int(rate))):  # one batch per second
                pending.append(publisher.publish(topic, payload, loadtest_run=run_id, seq=str(seq)))
                seq += 1
            # drop finished futures, count failures
            still = []
            for f in pending:
                if f.done():
                    if f.exception() is not None:
                        failed += 1
                else:
                    still.append(f)
            pending = still
            if time.monotonic() >= next_report:
                log(msg="progress", published=seq, failed=failed, in_flight=len(pending))
                next_report += 30
            time.sleep(max(0.0, 1.0 - (time.monotonic() - tick)))

    for f in futures.as_completed(pending, timeout=120):
        if f.exception() is not None:
            failed += 1
    log(msg="done", run_id=run_id, published=seq, failed=failed)
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    run()
