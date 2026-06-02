# Configuration reference

The provider is driven entirely by the `cluster` block of a Kairos cloud-config.
Kairos parses the cloud-config and hands the provider a `Cluster` payload; the
provider validates it, merges defaults, and converges the node.

## The `cluster` block

```yaml
cluster:
  cluster_token: "<high-entropy correlation string>"
  control_plane_host: "<ip-or-host[:port]>"
  role: init | controlplane | worker
  providerConfig:
    cluster_root_path: "/"
  config: |
    # kubeadm v1beta4 documents (see "The config: block" below)
```

### `cluster_token` (required)

A cluster-wide **correlation value**, not key material. The provider never derives
any credential from it (kubeadm's own CSPRNG generators produce tokens and keys).

- Validated at ingest: **rejected** if empty/whitespace or shorter than 16
  characters; a **loud one-time warning** is logged below roughly 128 bits of
  estimated entropy. No character-set restriction.
- It is never logged. Treat it as confidential anyway.

See [Security model](./security.md).

### `control_plane_host` (required)

The API server address this node uses. A bare host gets `:6443` appended. Its
role differs by node role:

- `role: init` - this node's own advertise address (or the stable endpoint; see
  the HA note below).
- `role: controlplane` / `role: worker` - the address the joiner reaches the
  control plane at (for HA this is the stable VIP/LB/DNS endpoint).

### `role` (required)

| Role | Behavior |
|------|----------|
| `init` | Runs `kubeadm init` to bootstrap the first control plane. Refuses to run if a control plane already answers at the endpoint (never clobbers an existing cluster). |
| `controlplane` | Joins an existing cluster as an additional, stacked-etcd control plane (requires a certificate key + stable endpoint). |
| `worker` | Joins an existing cluster as a worker. |

### `providerConfig.cluster_root_path`

Filesystem root the provider operates under (default `/`). It locates
`etc/kubernetes/...`, `var/lib/kubelet`, etc. Honored everywhere, including the
reset path, which validates it (absolute, no traversal) before removing artifacts.

### `config:` - the kubeadm v1beta4 surface

A raw YAML string containing the kubeadm documents the provider should emit. The
provider models the **v1beta4** schema directly (it does not import
`k8s.io/kubernetes`), so the keys match upstream kubeadm.

Common fields:

```yaml
config: |
  clusterConfiguration:
    kubernetesVersion: v1.34.0          # must be within the supported window
    controlPlaneEndpoint: "vip:6443"    # stable endpoint; REQUIRED for HA
    imageRepository: registry.k8s.io
    networking:
      podSubnet: 10.244.0.0/16
      serviceSubnet: 10.96.0.0/12
      dnsDomain: cluster.local
    apiServer:
      certSANs:
        - vip.example.test
        - 10.0.0.10
  initConfiguration:                    # role: init
    localAPIEndpoint:
      advertiseAddress: 10.0.0.10        # this node's own IP
      bindPort: 6443
    nodeRegistration:
      criSocket: unix:///run/containerd/containerd.sock
      kubeletExtraArgs: []
      taints: []
  joinConfiguration:                    # role: controlplane / worker
    discovery:
      bootstrapToken:
        token: "<bootstrap token>"
        apiServerEndpoint: "vip:6443"
        caCertHashes:
          - "sha256:<CA SPKI pin>"      # MANDATORY (CA pinning)
    controlPlane:                       # role: controlplane only
      certificateKey: "<fresh cert key>"
      localAPIEndpoint:
        advertiseAddress: 10.0.0.11      # the joining node's own IP
        bindPort: 6443
```

Notes:

- The provider auto-adds the `controlPlaneEndpoint` host (and the
  `control_plane_host`) to `certSANs` so TLS to the endpoint validates.
- For a control-plane join, set `controlPlane.localAPIEndpoint.advertiseAddress`
  to the joining node's own routable IP, especially on multi-homed nodes. The
  minting control plane cannot know it; `mint-join` leaves a placeholder you fill
  in (or pass `--advertise-address`).
- Token discovery **requires** `caCertHashes`; the provider refuses to emit a
  join config without a CA anchor and never sets `unsafeSkipCAVerification`.

## Externally-managed control planes

You can join a control plane the provider did not bootstrap. Supply the trust
anchor explicitly: a `discovery.bootstrapToken` with `caCertHashes`, or a
CA-embedded discovery file. If you supply both `CACerts` (via the cloud-config)
and explicit `caCertHashes`, they are cross-validated and a mismatch fails loud.

## Proxy environment

HTTP proxy variables (`HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`) supplied via the
Kairos `env` are honored for the provider's actions.

## What the provider emits

The `Provider` function is a side-effect-free emitter: it produces a single yip
stage at **`network.after`** that writes the serialized `Cluster` to a `0600`
tmpfs file (`/run/provider-kubernetes/cluster.json`) and invokes the provider's
`reconcile` subcommand against it. All actual work happens in that bounded
reconcile pass.
