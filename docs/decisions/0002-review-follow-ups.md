# 0002: Review follow-ups: NetworkPolicy, node pinning (ADR 0090), git history

Status: **implemented** (2026-09-29). Three items from the coordinator's review of the first pass
(ADR 0089/0090). All three were specified concretely. The calls made while building them are listed
below.

## 1. NetworkPolicy on the bundled server

`templates/networkpolicy.yaml`, on by default in bundled mode (`networkPolicy.enabled`). It is
booth-core's pattern (`charts/booth-core/templates/networkpolicy-postgresql.yaml`) applied to this
module's own server. Ingress to 5432 is allowed only from:

- this module's own pod (`component: api`, its admin connection),
- the backup Job (`component: backup`, which dumps over the network),
- namespaces labelled `booth.projectbooth.io/database-client=true`.

**Call: the same label as core's, not a new one.** booth-core already applies it to the namespace of
every module that declares `database`, and booth-notebooks and booth-pipeline, whose workloads are
this database's real clients, both do. So their namespaces are admitted with no new mechanism. The
cost is that the label now means "may reach either bundled Postgres", not just core's. Credential
auth is still the real boundary on both servers, so this widens network reachability only, and only
for namespaces core already trusts with database credentials. The label key/value is configurable
(`networkPolicy.clientNamespaceLabel`) should a distinct one be ruled.

**Found while doing this, not fixable here: the clients' own egress policies block this database.**
booth-notebooks' singleuser NetworkPolicy and booth-pipeline's runner NetworkPolicy both allow
egress only to DNS, core, and (opt-in) the internet *with every private range excepted*. On an
enforcing CNI, a notebook kernel or pipeline task therefore cannot open a connection to this
module's Postgres at all, whatever ingress policy this chart ships. Both charts expose an
`egress.extra` hook an operator can use today. The real fix is a default egress rule in each of
those charts (to pods labelled `app.kubernetes.io/name: booth-database,
app.kubernetes.io/component: postgres` on 5432), which is theirs to make, and pipeline's
"closed by default" stance (ADR 0070) may want a ruling first. Routed to the coordinator.

## 2. Node pinning (ADR 0090)

`internal/nodepin` is a deliberate copy of booth-core's `dbprov.EnsureNodeAffinity` and its
`pinBundledPostgresNode` loop:

- It reads pod `<sts>-0`'s `spec.nodeName` and, if the StatefulSet's
  `nodeSelector["kubernetes.io/hostname"]` doesn't match, merge-patches just that key. An
  operator's own `bundled.nodeSelector` entries are kept.
- It is idempotent: an already-correct pin is never re-patched (tested). A stale pin is corrected
  (tested). "No pod yet" and "no node yet" are "too early", not errors.
- It uses core's loop shape: exponential backoff from 1s to 30s until pinned, then a steady 5-minute
  recheck for the life of the process, resetting the backoff after success.

Calls:

- **RBAC is minimal, and slightly wider than the review's literal wording.** The review said
  "get/patch on your own StatefulSet". The pod's node can only be read from the pod, so the Role is
  get+patch on exactly one StatefulSet plus get on exactly one pod (`<sts>-0`), both pinned by
  `resourceNames`, namespaced, with no ClusterRole. A contract test enforces exactly that.
- **The service-account token is mounted only when pinning is on** (bundled + `bundled.pinToNode`,
  default true). External mode or `pinToNode=false` keeps the first pass's no-token posture.
- **client-go's typed clientset, not controller-runtime.** This module has no other use for
  controller-runtime. The patch carries field manager `booth-database-nodepin`.
- **`bundled.pinToNode` is a switch** (default on) for network-attached storage, where pinning would
  only stop the pod from failing over.
- Like core's, the first pin changes the pod template, so the StatefulSet rolls its pod once, right
  after first install.

## 3. Git history

The repo is initialised and committed, with CI workflows in place. See the report for push status.

## How this was verified

- `internal/nodepin` unit tests against client-go's fake clientset: nothing to pin yet, pin plus
  keeping other selectors, idempotence (no patch), stale re-pin, patch error surfaced, and the
  `Run` loop pinning once a previously-unscheduled pod gets a node.
- Contract tests: Role rules (exact resources, names and verbs; no Cluster* objects), token mounted
  iff pinning, NetworkPolicy peers and port, no policy or RBAC in external mode.
- **Two-node kind cluster** (`hack/kind-integration.sh`, now control plane + worker, matching
  ADR 0082's `node_count=2`; kindnet enforces NetworkPolicy):
  - the StatefulSet was pinned to the node its pod landed on;
  - a forced pod deletion brought it back on the same node with workspace data intact;
  - a `helm upgrade` that changed the pod template kept the pin (Helm's server-side apply leaves
    the field owned by `booth-database-nodepin` alone);
  - a TCP probe from a labelled namespace reached 5432, and one from an unlabelled namespace did
    not;
  - the client checks (4/4) passed from the labelled namespace;
  - the backup Job still works under the policy.
