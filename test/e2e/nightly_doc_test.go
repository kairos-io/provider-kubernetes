//go:build e2e && nightly

package e2e

// nightly_doc_test.go documents and load-bears the Tier-2 (ADR-13 E4) gating
// contract for the heavier nightly e2e scenarios (kairos-io/kairos#4202).
//
// GATING MECHANISM (build tags):
//   - Per-PR Tier-1 (ci.yml `e2e` job) runs `go test -tags e2e ...`. The nightly
//     files carry `//go:build e2e && nightly`, so they are EXCLUDED from the per-PR
//     run -- the per-PR budget never pays for the heavy HA/upgrade scenarios.
//   - Nightly Tier-2 (nightly.yml) runs `go test -tags "e2e nightly" ...`, which
//     satisfies `e2e && nightly`, so it compiles AND runs everything: the Tier-1
//     scenarios (still tagged `e2e`) PLUS the nightly-only files.
//   - `make e2e` -> per-PR set; `make e2e-nightly` -> the full set with a longer
//     timeout.
//
// The nightly-only scenarios (each in its own file, all `//go:build e2e && nightly`):
//   - nightly_failure_test.go  -- pre-membership FAILURE-STATUS path (#4099-1 never
//     hang): a worker against an unreachable endpoint fails loud within the budget
//     ceiling and writes a structured failure status.
//   - nightly_ha_test.go       -- multi-control-plane stacked-etcd HA bring-up
//     (ADR-11 keystone): a second control plane joins, 2 control-plane nodes, 2 etcd
//     members.
//   - nightly_upgrade_test.go  -- kubeadm-layer in-place minor upgrade (ADR-12): a
//     real `kubeadm upgrade apply` flips a running cluster N -> N+1 in place (no
//     A/B reboot; that hazard stays VM-only).
//
// This file contains no test functions; its presence under the nightly tag makes
// the gating self-documenting and gives `go vet -tags "e2e nightly"` a stable
// anchor. Keep it tagged identically to the scenarios it describes.
