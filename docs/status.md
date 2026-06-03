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
doc) with mode **0640, owner root, group adm** (group-readable by an `adm`-group
monitoring agent; world-unreadable). This is the only channel that works when a
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
version: v0.2.0
```

### Fields

| Field | Meaning |
|-------|---------|
| `phase` | `Reconciling` (in progress), `Converged` (success), `Failed`, or `Reset`. |
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
| `ResetFailed` / `ResetOK` | The outcome of an `EventClusterReset`. |

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
  layer is silently skipped - Layer 1 still has the truth.

## How to use it

- A node didn't come up? Read `/run/provider-kubernetes/status.yaml` (current boot)
  or, after a reboot, the `/var/log` mirror. `phase: Failed` + `reason` + `message`
  tells you what to fix; `terminal: true` means the next boot won't retry on its
  own.
- Watching a fleet? Scrape the Node annotations with `kubectl`.

See also [Lifecycle and reset](./lifecycle.md) and
[Troubleshooting](./troubleshooting.md).
