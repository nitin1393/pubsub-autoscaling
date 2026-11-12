"""Render tests for the pubsub-hpa chart: python3 -m pytest charts/pubsub-hpa/tests
Needs helm on PATH (or HELM=/path/to/helm)."""

import os
import pathlib
import subprocess

import pytest
import yaml

CHART = pathlib.Path(__file__).resolve().parents[1]
HELM = os.environ.get("HELM", "helm")
GCP = ["--set", "gcpPubsub.subscription=projects/p/subscriptions/s"]


def render(*args, ok=True):
    r = subprocess.run([HELM, "template", "t", str(CHART), "-n", "my-app", *args],
                       capture_output=True, text=True)
    if not ok:
        assert r.returncode != 0, "expected render to fail"
        return r.stderr
    assert r.returncode == 0, r.stderr
    return [d for d in yaml.safe_load_all(r.stdout) if d]


def kinds(docs):
    return sorted(d["kind"] for d in docs)


def one(docs, kind):
    found = [d for d in docs if d["kind"] == kind]
    assert len(found) == 1, f"{len(found)} {kind}"
    return found[0]


def triggers(docs):
    return {t["name"]: t for t in one(docs, "ScaledObject")["spec"]["triggers"]}


def test_default_gcp_source_reads_pubsub_directly():
    docs = render(*GCP)
    assert kinds(docs) == ["ScaledObject", "TriggerAuthentication"]
    t = triggers(docs)
    assert set(t) == {"backlog", "oldest-age"}
    assert t["backlog"]["type"] == "gcp-pubsub"
    assert t["backlog"]["metadata"] == {"subscriptionName": "projects/p/subscriptions/s",
                                        "mode": "SubscriptionSize", "value": "20", "activationValue": "0"}
    assert t["oldest-age"]["metadata"]["mode"] == "OldestUnackedMessageAge"
    assert t["oldest-age"]["metadata"]["value"] == "60"
    ta = one(docs, "TriggerAuthentication")
    assert ta["metadata"]["name"] == "worker-scaler-gcppubsub"
    assert ta["spec"]["secretTargetRef"][0] == {"parameter": "GoogleApplicationCredentials",
                                                 "name": "pubsub-scaler-gcp",
                                                 "key": "GOOGLE_APPLICATION_CREDENTIALS_JSON"}
    assert all(x["authenticationRef"]["name"] == ta["metadata"]["name"] for x in t.values())


def test_scaledobject_defaults_match_design():
    so = one(render(*GCP), "ScaledObject")["spec"]
    assert so["scaleTargetRef"] == {"kind": "Deployment", "name": "my-worker"}
    assert (so["minReplicaCount"], so["maxReplicaCount"], so["pollingInterval"]) == (2, 25, 15)
    assert so["fallback"] == {"failureThreshold": 3, "replicas": 10}
    b = so["advanced"]["horizontalPodAutoscalerConfig"]["behavior"]
    assert b["scaleUp"]["stabilizationWindowSeconds"] == 0 and b["scaleUp"]["selectPolicy"] == "Max"
    assert {(p["type"], p["value"], p["periodSeconds"]) for p in b["scaleUp"]["policies"]} == {("Percent", 100, 15), ("Pods", 10, 15)}
    assert b["scaleDown"]["stabilizationWindowSeconds"] == 300
    assert b["scaleDown"]["policies"] == [{"type": "Percent", "value": 20, "periodSeconds": 60}]


def test_hpa_name_matches_vpa_gate_annotation():
    # 06-vpa.yaml expects keda-hpa-worker-scaler
    assert one(render(*GCP), "ScaledObject")["metadata"]["name"] == "worker-scaler"


def test_groundcover_source_uses_bearer_and_backend_header():
    docs = render("--set", "metricsSource=groundcover", "--set", "groundcover.backendId=b1")
    t = triggers(docs)
    assert {x["type"] for x in t.values()} == {"prometheus"}
    md = t["backlog"]["metadata"]
    assert md["serverAddress"] == "https://app.groundcover.com/api/prometheus"
    assert md["authModes"] == "bearer" and md["customHeaders"] == "X-Backend-Id=b1"
    assert md["threshold"] == "20" and md["activationThreshold"] == "0"
    assert "activationThreshold" not in t["oldest-age"]["metadata"]
    ta = one(docs, "TriggerAuthentication")
    assert ta["spec"]["secretTargetRef"] == [{"parameter": "bearerToken", "name": "groundcover-api-key", "key": "bearerToken"}]


def test_in_cluster_prometheus_needs_no_auth():
    docs = render("--set", "metricsSource=prometheus")
    assert kinds(docs) == ["ScaledObject"]
    for t in triggers(docs).values():
        assert "authenticationRef" not in t
        assert t["metadata"]["serverAddress"] == "http://prometheus-operated.monitoring.svc:9090"


def test_grafana_cloud_basic_auth():
    docs = render("--set", "metricsSource=prometheus", "--set", "prometheus.authModes=basic",
                  "--set", "prometheus.existingSecret=gc")
    ta = one(docs, "TriggerAuthentication")
    assert {r["parameter"] for r in ta["spec"]["secretTargetRef"]} == {"username", "password"}
    assert all(t["metadata"]["authModes"] == "basic" for t in triggers(docs).values())


def test_inflight_from_groundcover_alongside_gcp_backlog():
    docs = render(*GCP, "--set", "triggers.inFlight.enabled=true")
    t = triggers(docs)
    assert set(t) == {"backlog", "oldest-age", "in-flight"}
    assert t["in-flight"]["type"] == "prometheus" and t["in-flight"]["metadata"]["threshold"] == "16"
    names = {d["metadata"]["name"] for d in docs if d["kind"] == "TriggerAuthentication"}
    assert names == {"worker-scaler-gcppubsub", "worker-scaler-groundcover"}


def test_never_a_cpu_or_memory_trigger():
    for args in (GCP, ["--set", "metricsSource=groundcover", "--set", "triggers.inFlight.enabled=true"]):
        assert not {t["type"] for t in triggers(render(*args)).values()} & {"cpu", "memory"}


def test_nodepool_and_headroom_optional():
    assert kinds(render(*GCP)) == ["ScaledObject", "TriggerAuthentication"]
    docs = render(*GCP, "--set", "nodePool.enabled=true", "--set", "headroom.enabled=true",
                  "--set", "nodePool.zones={a,b,c}")
    assert kinds(docs) == ["Deployment", "NodePool", "PriorityClass", "PriorityClass", "ScaledObject", "TriggerAuthentication"]
    np = one(docs, "NodePool")["spec"]
    reqs = {r["key"]: r for r in np["template"]["spec"]["requirements"]}
    assert reqs["karpenter.sh/capacity-type"]["values"] == ["spot", "on-demand"]
    assert reqs["topology.kubernetes.io/zone"]["values"] == ["a", "b", "c"]
    assert np["limits"]["cpu"] == "400" and np["disruption"]["budgets"] == [{"nodes": "10%"}]
    pcs = {d["metadata"]["name"]: d["value"] for d in docs if d["kind"] == "PriorityClass"}
    assert pcs == {"pubsub-overprovisioning": -10, "pubsub-worker": 1000}
    dep = one(docs, "Deployment")["spec"]["template"]["spec"]
    assert dep["priorityClassName"] == "pubsub-overprovisioning"
    assert dep["tolerations"][0]["value"] == "pubsub-worker"


@pytest.mark.parametrize("args,msg", [
    ([], "set gcpPubsub.subscription"),
    (GCP + ["--set", "metricsSource=nope"], "metricsSource must be"),
    (GCP + ["--set", "replicas.min=5", "--set", "replicas.max=2"], "replicas.max must be >= replicas.min"),
    (GCP + ["--set", "triggers.backlog.enabled=false", "--set", "triggers.oldestAge.enabled=false"], "enable at least one trigger"),
    (GCP + ["--set", "triggers.inFlight.enabled=true", "--set", "triggers.inFlight.source=gcpPubsub"], "triggers.inFlight.source must be"),
])
def test_bad_config_fails_clearly(args, msg):
    assert msg in render(*args, ok=False)
