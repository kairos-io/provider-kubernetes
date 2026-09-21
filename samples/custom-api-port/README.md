# API server on a non-default port

kubeadm's `localAPIEndpoint.bindPort` defaults to 6443. You can move it, for
example to keep 6443 free for a load balancer or an ingress on the same host.
The provider reads the field, renders it into the kubeadm config, and probes
the local API server on that port.

| File | Role |
|------|------|
| [`init-custom-port.yaml`](./init-custom-port.yaml) | first control plane on 7443 |
| [`worker-custom-port.yaml`](./worker-custom-port.yaml) | worker joining it |

## The port appears in four places

They must all agree, or the node converges slowly or not at all.

| Where | Why |
|-------|-----|
| `initConfiguration.localAPIEndpoint.bindPort` | kubeadm renders it as the API server's `--secure-port`. |
| `clusterConfiguration.controlPlaneEndpoint` | Baked into the serving certificate and dialed by every node. |
| `cluster.control_plane_host` | The provider's reachability probe dials this literally. A bare host with no port gets `:6443` appended. |
| each joiner's `discovery.bootstrapToken.apiServerEndpoint` | Where `kubeadm join` talks to the control plane. |

For an additional control plane, the joining node's own port lives in
`joinConfiguration.controlPlane.localAPIEndpoint.bindPort`, which takes
precedence over `initConfiguration.localAPIEndpoint.bindPort` for that role.

## What this fixed

Before v0.4.0 both probes of the *local* API server polled a literal
`https://127.0.0.1:6443/healthz`, ignoring `bindPort`. On a cluster that had
moved the API server, the node looked unreachable on every reconcile pass. The
upgrade planner then prepended a kubelet-config repair to a control plane whose
API server was perfectly healthy, and that repair's own wait polled the wrong
port until the reconcile budget ran out: the upgrade never ran, and the kubelet
had been restarted for nothing. Both call sites now derive the probe URL from
the configured port (upstream issue
[kairos-io/kairos#4826](https://github.com/kairos-io/kairos/issues/4826)).

## Checks

```sh
# the port kubeadm gave the apiserver
sudo grep -- --secure-port /etc/kubernetes/manifests/kube-apiserver.yaml
# what the node dials
sudo grep server: /etc/kubernetes/admin.conf
```

Both must show the port you set. If a reconcile reports
`reason: ControlPlaneUnreachable` while the API server answers, check
`control_plane_host` first: that is the value the probe dials.
