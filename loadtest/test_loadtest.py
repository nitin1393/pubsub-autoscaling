"""Unit tests for the load-test kit (no cluster or GCP needed): python3 -m pytest loadtest"""

import pytest

from publisher import PROFILES, plan
from watch import SCALING, SETTLED, Sample, check

STABLE, STEP = 900.0, 15.0


def series(spec):
    """spec: list of (seconds, current, desired, vpa_mode, ready, pending_age, ooms); HPA lastScale
    moves whenever current changes."""
    out, t, last_cur, last_scale = [], 1_000_000.0, None, 0.0
    for secs, cur, des, mode, ready, pend, ooms in spec:
        for _ in range(int(secs // STEP)):
            if last_cur is not None and cur != last_cur:
                last_scale = t
            last_cur = cur
            out.append(Sample(t, cur, des, last_scale, mode, ready, 0, pend, ooms, 3))
            t += STEP
    return out


def results(samples):
    return {n: (ok, d) for n, ok, d in check(samples, STABLE, STEP, 180, 60, 4)}


def good_spike():
    return series([
        (300, 2, 2, SETTLED, 2, 0, 0),          # quiet, settled
        (15, 2, 20, SCALING, 2, 0, 0),          # spike detected: gate flips at once
        (600, 20, 20, SCALING, 20, 0, 0),       # running hot, within window
        (15, 20, 4, SCALING, 20, 0, 0),         # drain
        (900, 4, 4, SCALING, 4, 0, 0),          # waiting out the window
        (300, 4, 4, SETTLED, 4, 0, 0),          # released
    ])


def test_good_run_passes():
    r = results(good_spike())
    assert all(ok for ok, _ in r.values()), r


def test_settled_during_scaling_fails_gate_safety():
    s = good_spike()
    s[25].vpa_mode = SETTLED            # inside the hot period
    assert results(s)["gate-safety"][0] is False


def test_never_released_fails_gate_settles():
    s = good_spike()
    for x in s[-15:]:
        x.vpa_mode = SCALING
    assert results(s)["gate-settles"][0] is False


def test_oom_fails():
    s = good_spike()
    for x in s[40:]:
        x.oom_kills = 1
    assert results(s)["no-oom"][0] is False


def test_long_pending_fails():
    s = good_spike()
    s[30].oldest_pending_s = 400
    assert results(s)["no-long-pending"][0] is False


def test_slow_scaleup_fails():
    s = series([
        (300, 2, 2, SETTLED, 2, 0, 0),
        (15, 2, 20, SCALING, 2, 0, 0),
        (120, 2, 20, SCALING, 2, 0, 0),        # nothing becomes ready for 2 min
        (600, 20, 20, SCALING, 20, 0, 0),
    ])
    assert results(s)["scale-up-latency"][0] is False


def test_flapping_gate_fails():
    spec = []
    for _ in range(10):                       # mode flips every 30 s for 5 min
        spec += [(15, 4, 4, SETTLED, 4, 0, 0), (15, 4, 4, SCALING, 4, 0, 0)]
    assert results(series(spec))["gate-no-flap"][0] is False


def test_empty_samples_fail():
    assert check([], STABLE, STEP, 180, 60, 4)[0][1] is False


@pytest.mark.parametrize("name", sorted(PROFILES))
def test_profiles_are_sane(name):
    p = plan(name, 20)
    assert p and all(rate > 0 and secs > 0 for rate, secs in p)
    assert sum(s for _, s in p) <= 6 * 3600


def test_unknown_profile_rejected():
    with pytest.raises(SystemExit):
        plan("nope", 1)
