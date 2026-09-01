# Security Policy

## Reporting a vulnerability

Report security issues privately through
[GitHub Security Advisories](https://github.com/breezycourses/presage/security/advisories/new).
Do not open a public issue for a suspected vulnerability.

Please include the presage version, the Kubernetes version, whether the
forecaster is deployed, and enough detail to reproduce.

Expect an acknowledgement within 5 working days and an assessment within 10.
presage is a small project without a paid on-call rotation; if an issue is
being actively exploited, say so in the report and it will be prioritised
accordingly.

## Supported versions

presage is `v1alpha1` and pre-1.0. Only the latest minor release receives
security fixes. There are no backports to older versions.

| Version | Supported |
| --- | --- |
| 0.1.x | ✅ |
| < 0.1 | ❌ |

## Threat model

Understanding what presage can do is more useful than a list of CVE classes.
The controller holds a ClusterRole that can **change the replica count of any
scalable workload in the cluster**. Anything that lets an attacker influence a
`PredictiveScaler` therefore has a cost-and-availability blast radius, even
though it grants no direct code execution.

### In scope

* **Replica manipulation.** Anything that causes presage to write a replica
  count an authorised operator did not intend — bypassing `maxReplicas`,
  bypassing the reactive floor, or scaling a workload the scaler does not
  reference.
* **Privilege escalation through the controller.** presage is deliberately
  restricted to `get/list/watch` on workloads and write access only on their
  `/scale` subresource. Any path that turns a `PredictiveScaler` into broader
  cluster access is a vulnerability.
* **Secret disclosure.** `spec.signal.prometheus.bearerTokenSecretRef` reads a
  Secret. That value must never appear in logs, events, status conditions, or
  metric labels.
* **Denial of service on the forecaster.** The model server is CPU- and
  memory-hungry by nature; a request that exhausts a node beyond the
  configured `max_batch` and `max_total_points` limits is in scope.
* **Agones webhook responses.** A response that causes Agones to scale a Fleet
  in a way presage did not compute.

### Known and accepted

These are documented design consequences, not vulnerabilities. They are listed
here so operators can make an informed decision rather than discover them.

* **`spec.signal.prometheus.address` is a server-side request.** Anyone who can
  create a `PredictiveScaler` in a namespace can make the controller issue
  HTTP POSTs to an arbitrary address reachable from the controller pod. The
  response must parse as a Prometheus API payload to be used, and parse
  failures surface in the object's status rather than the response body — but
  connection success or failure is observable, so this is a network probe
  primitive.

  Treat `create` on `predictivescalers` as a privileged grant. Do not hand it
  to untrusted tenants. Restrict the controller's egress with a
  NetworkPolicy limiting it to your metrics backends.

* **The Agones webhook is unauthenticated.** It serves cached recommendations
  over plain HTTP by default, because that is what Agones speaks to
  service-based webhooks. It exposes replica recommendations to anyone who can
  reach the Service, and accepts reviews from anyone. It cannot be made to
  return a number presage did not compute — the handler only reads a cache the
  reconciler writes — but the recommendation itself is readable. Keep the
  Service `ClusterIP`, and set `agonesWebhook.tls.enabled` if your threat model
  needs transport security.

* **The forecaster endpoint is unauthenticated.** It performs no cluster
  actions and holds no credentials, but it will spend CPU for anyone who can
  reach it. Keep it `ClusterIP`.

* **Model weights are third-party.** presage downloads a TimesFM checkpoint
  from Hugging Face. The revision is pinned by default precisely so the
  artefact cannot change under a running cluster. Supply-chain concerns about
  the weights themselves belong upstream, but a report that presage loads them
  unsafely is in scope.

### Out of scope

* Misconfiguration that under- or over-provisions a workload. Set
  `maxReplicas` deliberately; it is a hard bound and overrides the safety
  floor by design.
* Forecast inaccuracy. A wrong forecast is a modelling outcome, not a
  vulnerability — the reactive floor exists so that being wrong stays a cost
  problem rather than an outage.
* Vulnerabilities in Kubernetes, Agones, Prometheus, or TimesFM themselves.
  Report those to their projects.
