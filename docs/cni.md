# CNI

The provider deliberately installs **no CNI**. It stays out of CNI policy so you
can choose Flannel, Calico, Cilium, or whatever fits. Until a CNI is installed,
nodes report `NotReady` and pods stay `Pending` - this is expected.

Install a CNI **after** the control plane is up.

## Calico (worked example)

[`samples/cni-calico/`](../samples/cni-calico/) has two validated approaches.

### A. Apply after the cluster is up

```sh
# operator
kubectl create -f https://raw.githubusercontent.com/projectcalico/calico/v3.32.0/manifests/tigera-operator.yaml
# wait for the operator CRDs, then the Installation CR (pool must match podSubnet):
kubectl wait --for=condition=Established crd/installations.operator.tigera.io --timeout=60s
kubectl apply -f samples/cni-calico/installation.yaml
```

The sample `Installation` uses pool CIDR `10.244.0.0/16` (matching the sample
`podSubnet`) and `VXLANCrossSubnet` encapsulation, which needs no BGP/fabric and
works on a plain bridged or NAT LAN. If you change `podSubnet`, change the pool
CIDR to match.

Gotcha: apply the `Installation` CR only **after** the operator CRDs are
`Established`, or you get "no matches for kind Installation".

### B. Bundle it in the control-plane cloud-config

[`samples/cni-calico/cluster-with-calico.yaml`](../samples/cni-calico/cluster-with-calico.yaml)
installs Calico from a self-waiting, idempotent systemd one-shot that waits for
`admin.conf` and a ready API, then server-side-applies the operator + Installation
(sentinel-guarded). Because it waits on its own, it is independent of yip stage
ordering relative to the provider's reconcile, never blocks a boot stage, and is
idempotent on reboot.

## Picking a pod subnet

Whatever CNI you use, its pod network must match the `podSubnet` you set in
`clusterConfiguration.networking`. The default in the samples is `10.244.0.0/16`.

## Other CNIs

Any standard CNI works - install it with its normal manifests/Helm chart after
bootstrap. The provider does not interfere with `/opt/cni/bin` beyond shipping the
upstream reference plugins the kubelet needs.
