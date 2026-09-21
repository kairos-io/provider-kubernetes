# Trusted boot (UKI)

[`init-uki.yaml`](./init-uki.yaml) is a `role: init` cloud-config for a
trusted-boot (UKI) Kairos node. The `cluster:` block is the same one a GRUB node
uses. This directory exists because two operational things differ on UKI, and
both have bitten people.

## 1. First boot after install now bootstraps

kairos-sdk writes `/usr/local/cloud-config/cluster.kairos.yaml` with
`O_CREATE|O_WRONLY|O_TRUNC` and never creates the parent directory. A GRUB
install creates it as a side effect of the install's chroot hook. The UKI
install and state-reset paths have no equivalent hook, so on the first boot
after install, and on the boot right after a state reset, that write failed and
the node never bootstrapped. A plain reboot made it converge; a state reset
reproduced it.

The provider now creates that one directory itself, `/usr/local/cloud-config`
at mode `0700`, never its ancestors, using an openat/mkdirat walk that refuses
to follow a symlink and refuses an entry that already exists unless it is a
plain, root-owned directory. A fresh UKI install or state reset converges with
no manual step.

Read the `0700` narrowly: it is not a boundary against root or against a
hostPath-capable workload, and the platform widens the directory to `0770
root:admin` from the second boot onward.
[Security model](../../docs/security.md#cluster-config-directory-creation-is-not-a-security-boundary-d-3--f-ukiboot)
states exactly what it does and does not buy.

## 2. The status file is your only channel

There is no log channel reachable after boot on UKI. The provider does log a
`provider-kubernetes: cluster-config-dir: ...` error line, but it lands in a
pre-pivot, tmpfs-backed log that does not survive the switch-root. A VM run on
2026-09-21 confirmed it appears in neither `immucore.log`, `agent.log`, nor the
journal afterwards.

What does survive is the status document:

```sh
sudo cat /run/provider-kubernetes/status.yaml          # this boot
sudo cat /var/log/provider-kubernetes/status.yaml      # persistent mirror
```

On UKI the `/var/log` mirror is load-bearing rather than a convenience: it is
the only copy you can rely on if you cannot read `/run` before the node reboots.

If the node does not converge, the `reason:` field names the cause. Three
reasons (`ClusterConfigAncestorUnsafe`, `ClusterConfigDirUnsafe`,
`ClusterConfigTokenFileUnsafe`) also make the provider withhold `cluster_token`
and emit no bootstrap commands for that boot, rather than let the SDK write the
real secret somewhere it could not verify.
[Troubleshooting](../../docs/troubleshooting.md#first-boot-after-install-or-a-state-reset-never-bootstraps-d-3--f-ukiboot)
has the full table and what to do about each.

## Bundled images work on UKI too

Importing the bundled control-plane images works on a UKI node, so an
air-gapped trusted-boot install is supported. See [`../air-gapped/`](../air-gapped/).

## Overriding the config path

`cluster_config_path` is honored, but its directory part must be exactly
`/usr/local/cloud-config`. Anything else, including a relative path or one
containing `..`, is rejected with `reason: ClusterConfigOverrideRejected` and
nothing is created; pre-creating your own directory safely is then your job.
