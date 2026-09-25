# Node status (why a node did or did not converge)

The provider runs one bounded reconcile pass on each boot. Because that pass runs
as a fire-and-forget boot-time stage, its exit code does not flow back to
`kairos-agent` - so the provider publishes its outcome through two channels you can
inspect directly, instead of making you read journald. Both are **best-effort**: a
status write never blocks the boot, never retries forever, and never changes or
masks the real reconcile result.

## Layer 1 - the local status file (always written)

Every reconcile pass and every reset writes a small YAML status document to:

- `/run/provider-kubernetes/status.yaml` - current boot, on tmpfs (authoritative).
- `/var/log/provider-kubernetes/status.yaml` - a persistent mirror that survives
  reboot, for post-mortem after a failed boot.

Both are written atomically (temp file + rename, so a reader never sees a partial
doc) with mode **0640, owner root**. The group is set to `adm` when that group
resolves on the node, so an `adm`-group monitoring agent can read it; if the
lookup fails the file stays root:root. Either way it is world-unreadable. This
is the only channel that works when a
node never joined the cluster - the very failure you most need to debug.

The document carries **no secrets by construction**: every field except `message`
is a closed enum, and `message` is sanitized (bootstrap tokens, PEM blocks, and
long secret-like runs are redacted; it is truncated to one short line).

### Example

```yaml
apiVersion: provider-kubernetes.kairos.io/v1
phase: Failed
role: worker
membership: uninitialized
outcome: failure
reason: ControlPlaneUnreachable
terminal: false
lastAction: wait-for-control-plane
message: "control-plane endpoint not reachable within budget"
budget:
  attempts: 3
  maxAttempts: 3
updatedAt: "2026-06-03T12:00:00Z"
bootID: 7c9e6679-7425-40de-944b-e07fc1f90ae7
version: v0.4.0
```

### Fields

| Field | Meaning |
|-------|---------|
| `phase` | `Reconciling` (in progress), `Converged` (success), `Degraded`, `Failed`, or `Reset`. |
| `role` | The node's declared role: `init`, `controlplane`, or `worker`. |
| `membership` | `uninitialized`, `initialized`, or `joined` (probed actual state). |
| `outcome` | `success` or `failure`. |
| `reason` | Closed enum, empty on success. See table below. |
| `terminal` | `true` if the failure will not be retried on the next boot (fail-fast); `false` if a later boot may converge. |
| `lastAction` | The last kubeadm action attempted (e.g. `run-join`, `upgrade-apply`). |
| `message` | One short, sanitized, operator-facing line. |
| `budget` | `attempts` used out of `maxAttempts`. |
| `updatedAt` | RFC3339 timestamp of this status. |
| `bootID` | The kernel boot ID, so you can tell which boot produced it. |
| `version` | The provider build version. |

### Reason codes

| `reason` | What happened |
|----------|---------------|
| `ControlPlaneUnreachable` | The control-plane endpoint was not reachable within the budget. |
| `JoinTimeout` | A join did not complete within the budget. |
| `InitRefused` | A second `role: init` was refused to avoid clobbering an existing cluster (#4099-5). |
| `UpgradeRefused` | A downgrade, skip-level, or out-of-window upgrade pin was refused. |
| `BudgetExhausted` | The bounded retry budget ran out. |
| `KubeadmError` | A kubeadm action failed. |
| `ConfigInvalid` | The supplied cluster config was invalid (e.g. empty/short `cluster_token`). |
| `KubeletUnhealthy` | See `phase: Degraded` below. |
| `InitIncomplete` | See `phase: Degraded` below. |
| `ControlPlaneUnhealthy` | See `phase: Degraded` below. |
| `ResetFailed` / `ResetOK` | The outcome of an `EventClusterReset`. |

### `phase: Degraded`

The node is already a cluster member (`membership: initialized` or `joined`),
but it is not healthy. Three checks run, in this order, and the first one that
fails names the `reason`:

1. **`KubeletUnhealthy`**, on every member. A plain HTTP GET of the kubelet's
   own loopback healthz endpoint, `http://127.0.0.1:10248/healthz`, the same
   endpoint kubeadm itself waits on before considering a kubelet up. Healthy is
   exactly an HTTP 200 response within 2 seconds; a dial error (including
   "connection refused", which is what a masked or stopped kubelet looks
   like), a timeout, a non-200 status, or an unreadable body are all "not
   healthy". A stopped kubelet also explains the other two checks failing, so
   it is reported first.
2. **`InitIncomplete`**, on a control plane (`membership: initialized`).
   `kubeadm init` did not finish on this node. kubeadm writes `admin.conf`
   before it waits for the control plane, so a failed init leaves a node that
   looks initialized. This check reads two files and nothing else: the
   `kubelet-finalize` phase of `kubeadm init` rewrites
   `/etc/kubernetes/kubelet.conf` to reference the kubelet's rotated client
   certificate instead of embedding one, and it does so whenever
   `/var/lib/kubelet/pki/kubelet-client-current.pem` exists. An embedded
   certificate next to that file means the phase never ran, so the phases
   before it, which create the `kubeadm-config` ConfigMap, the bootstrap-token
   RBAC and `cluster-info` that every join needs, may not have run either.
   Without the rotated certificate (client certificate rotation disabled, or
   a custom kubelet `--cert-dir`) the check cannot decide and reports nothing.
3. **`ControlPlaneUnhealthy`**, on a control plane. The local kube-apiserver
   does not answer `https://127.0.0.1:<bindPort>/healthz` with HTTP 200, where
   `<bindPort>` is `localAPIEndpoint.bindPort` (6443 by default). After a
   reboot the kubelet starts the static pods only once it is up itself, so the
   apiserver is often still starting when reconcile runs. The provider
   therefore polls it every 3 seconds for up to **3 minutes** before reporting
   this; a healthy node answers well inside that, and one that already answers
   is not polled at all.

A node that joined as a worker (`membership: joined`) is judged on its kubelet
alone.

The provider deliberately does **not** re-run `kubeadm init`/`join` on its own:
recovering an established member is an explicit operator action (see
[Lifecycle and reset](./lifecycle.md)), never an automatic re-bootstrap. Before
this phase existed, this situation was silently reported as `phase: Converged`
- masking a real outage. `outcome` is `failure` (something is genuinely wrong)
but `terminal` is `false`: a later boot, or an explicit reset, may still
converge cleanly, so this is not treated as fatal anywhere else in the
provider or its tests.

The status is written by the reconcile pass, which runs once per boot. To
re-evaluate it without a reboot, run the same pass again, but only once the
boot's own pass has finished: nothing prevents two passes from running at the
same time, and the boot's pass may still be initializing, joining or upgrading.
The pass runs inside the boot stage `cos-setup-network.service`, which becomes
active the moment the stage is over:

```sh
systemctl is-active cos-setup-network.service   # "active": finished; "activating": still running
```

Then:

```sh
sudo /system/providers/agent-provider-kubernetes reconcile --cluster-file=/run/provider-kubernetes/cluster.json
```

It is the same pass the boot runs. On a member it takes no action and only
repeats the checks above, unless an upgrade is due: when
`clusterConfiguration.kubernetesVersion` is pinned to a newer minor than this
node runs, the pass performs the upgrade, exactly as a boot would.

## Layer 2 - Node annotations (when the node is a cluster member)

When - and only when - the node is already a member and a local kubeconfig exists,
the provider **also** records the same outcome as annotations on its own Node
object, so you can see it cluster-wide without SSH:

```sh
kubectl get node <name> -o jsonpath='{.metadata.annotations}' | tr ',' '\n' | grep provider-kubernetes.kairos.io
```

Keys published (all under `provider-kubernetes.kairos.io/`):
`phase`, `outcome`, `reason`, `terminal`, `last-action`, `updated-at`, `version`.

Notes:

- The free-text `message` is **not** published as an annotation - only the closed
  enum fields are, so an annotation can never carry a secret.
- These annotations are readable by any principal with `get node`. They are coarse
  lifecycle metadata and intentionally carry no sensitive data.
- The write uses the **least-privilege** credential already on the node
  (`kubelet.conf`, the `system:node:<name>` identity, preferred over `admin.conf`).
  The provider creates **no** new RBAC, ServiceAccount, or token.
- If the node is not a member, has no kubeconfig, or the API is unreachable, this
  layer is silently skipped and the annotations keep whatever an earlier pass
  wrote. That happens whenever a pass finishes before the apiserver is up, for
  example on a control plane that reports `Degraded`, so an annotation can say
  `Converged` from an earlier boot. Layer 1 always has the truth.

## How to use it

- A node didn't come up? Read `/run/provider-kubernetes/status.yaml` (current boot)
  or, after a reboot, the `/var/log` mirror. `phase: Failed` + `reason` + `message`
  tells you what to fix; `terminal: true` means the next boot won't retry on its
  own.
- A node reports `phase: Degraded`? It is already a cluster member but not
  healthy; `reason` names the check that failed. See
  [Troubleshooting](./troubleshooting.md#status-reports-phase-degraded) for
  what to look at for each reason. A reset is the supported recovery path when
  the node cannot be repaired in place, and the only one for `InitIncomplete`.
- Watching a fleet? Scrape the Node annotations with `kubectl`.

See also [Lifecycle and reset](./lifecycle.md) and
[Troubleshooting](./troubleshooting.md).
