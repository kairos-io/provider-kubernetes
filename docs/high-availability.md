# High availability (multi-control-plane)

The provider supports **stacked-etcd HA**: three or more control-plane nodes, each
running etcd, behind a stable API endpoint. This page covers the concepts and
rules; [`samples/ha/`](../samples/ha/) has a full walkthrough
(init + two CP joins + the reset/orphan runbook).

## The one hard prerequisite: a stable endpoint

HA is only possible if the cluster is initialized with a `controlPlaneEndpoint`
that is **stable and outlives any single node** - a VIP, an external L4 load
balancer, or a health-checked DNS name fronting all control planes. It is baked
into the API server serving certificate and is how every node reaches the API.

- The provider ships **no load balancer** (no vendor lock-in). Provisioning the
  endpoint (kube-vip, keepalived, a cloud LB, DNS) is your responsibility.
- At `role: init` the provider **warns** if there is no stable endpoint (it
  compares the endpoint against the node's own advertise address).
- At `role: controlplane` an empty endpoint is a **hard failure**.
- You cannot retrofit an endpoint into a live cluster (it requires re-issuing all
  API server certs). Initialize with the stable endpoint from the start.

## Bring up ONE control plane at a time

etcd membership changes are sequential; adding two members at once can break
quorum, and concurrent joins race the shared certificate upload. The provider
does **not** build a distributed lock (that risks hangs). Instead:

1. Bring up the first control plane and wait until it is fully Ready.
2. Mint control-plane join material for the next node (fresh per node, near its
   boot time) and boot it. Wait until it joins and etcd shows the new member.
3. Repeat for the third (and any further) control plane.

The provider adds a bounded node-local `/readyz` health gate before a
control-plane join, so a new CP only joins a healthy quorum.

Aim for an **odd** number of healthy control planes (3 or 5).

## The control-plane certificate key

A control-plane join needs a `certificateKey` that decrypts the cluster PKI
uploaded to the `kubeadm-certs` Secret. `mint-join --role controlplane` mints a
fresh key **and** re-uploads the certs encrypted under it, so the key always
matches what is in etcd. This material is **root-equivalent** - handle it
accordingly (see [Security model](./security.md)).

## Removing a control plane

Removing a control plane is a two-part operation:

1. **Local reset** on the node (the [reset](./lifecycle.md) flow) wipes the local
   kubeadm artifacts, including its etcd data.
2. **etcd member deregistration**. If the cluster is reachable at reset time,
   `kubeadm reset` removes the local member for you. If the node is being reset
   *because* it is broken/unreachable, the member is left orphaned - a stale
   member erodes quorum. The provider does **not** run `etcdctl` from a dying
   node; it logs a loud, actionable warning and proceeds with local cleanup. You
   then deregister it from a surviving control plane:

   ```sh
   kubectl delete node <name>
   # on a surviving control plane:
   etcdctl member list
   etcdctl member remove <member-id>
   ```

## What the HA path covers

A three-control-plane cluster behind a VIP supports: control-plane joins with
per-node advertise addresses, etcd quorum growth, the init-clobber guard (a stray
`role: init` is refused, not allowed to clobber the cluster), and failover (losing
a control plane while the cluster survives on remaining quorum). See
[`samples/ha/README.md`](../samples/ha/README.md).
