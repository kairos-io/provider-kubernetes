# Joining an externally-managed control plane

The provider does not assume it bootstrapped the control plane. A node can join
a cluster stood up by plain `kubeadm`, by another tool, or by a managed service,
as long as you give it a trust anchor and a bootstrap token.

[`worker-join-external.yaml`](./worker-join-external.yaml) is the most common
case: a worker joining with the external cluster's CA in PEM form.

## Anchoring trust

CA pinning is mandatory. The provider refuses to build a token-discovery join
config without an anchor, and never sets `unsafeSkipCAVerification`. Three ways
to supply one:

| Anchor | When |
|--------|------|
| `ca_certs` (PEM) plus a bootstrap token | You can read `ca.crt` off the control plane. The provider derives the `sha256:` SPKI pin for you. |
| `caCertHashes` plus a bootstrap token | You already have the pin, or policy hands you a pin rather than a certificate. |
| `discovery.file.kubeConfigPath` | Your tooling already produces a CA-embedded discovery kubeconfig on the node. |

Supply both `ca_certs` and `caCertHashes` and the provider cross-validates them:
if the derived set and the explicit set differ, the join fails with a CA trust
mismatch rather than picking one. A `ca_certs` entry that is not a parseable
certificate is also a hard error.

`ca_certs` is a key of the `cluster:` block itself, not of the inner `config:`
string. The inner `config:` uses kubeadm's own v1beta4 names.

## Getting the material

On the external control plane:

```sh
kubeadm token create --ttl 1h
cat /etc/kubernetes/pki/ca.crt
```

If you would rather pin by hash than paste a certificate:

```sh
openssl x509 -pubkey -in /etc/kubernetes/pki/ca.crt \
  | openssl rsa -pubin -outform der 2>/dev/null \
  | openssl dgst -sha256 -hex | awk '{print "sha256:"$2}'
```

Deliver the rendered config promptly: the token is short-lived by design, and
the provider persists none of the material.

## Control-plane joins

Joining an external cluster as an additional *control plane* additionally needs
a `certificateKey` matching the `kubeadm-certs` Secret on that cluster, which
only the external cluster can mint (`kubeadm init phase upload-certs
--upload-certs`). That material is root-equivalent; see
[Security model](../../docs/security.md#the-control-plane-certificate-key-is-root-equivalent)
and [`../controlplane.yaml`](../controlplane.yaml) for the shape.

## Coverage

This path runs in CI on every pull request. One end-to-end scenario stands up a
control plane with plain `kubeadm init`, which the provider never touches, then
joins a worker through the provider using only the operator-supplied CA PEM, and
asserts the provider derived the pin and the worker registered.
