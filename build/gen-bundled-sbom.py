#!/usr/bin/env python3
"""Convert the pre-bundled control-plane image lockfile (images.lock, ADR-16 P5)
into a CycloneDX SBOM so the release pipeline can attest -- by digest -- exactly
which control-plane images the published image baked, and whether each was
upstream-signature-verified at build (verified) or digest-pinned only.

Usage: gen-bundled-sbom.py <images.lock> <out.cdx.json>

This is a build-time helper run on the CI runner (not inside the image); it has no
third-party deps so it runs on a stock runner. Input is the trusted, build-emitted
lockfile, so parsing is intentionally strict and fails loud on malformed input.
"""
import json
import sys


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: gen-bundled-sbom.py <images.lock> <out.cdx.json>", file=sys.stderr)
        return 2
    lock = json.load(open(sys.argv[1]))

    components = []
    for img in lock["images"]:
        ref = img["ref"]
        digest = img["digest"]  # "sha256:<hex>"
        name = ref.rsplit(":", 1)[0]  # strip the tag, keep the repo path
        version = ref.rsplit(":", 1)[-1]
        short = name.rsplit("/", 1)[-1]
        components.append({
            "type": "container",
            "name": name,
            "version": version,
            "purl": "pkg:oci/%s@%s" % (short, digest),
            "hashes": [{"alg": "SHA-256", "content": digest.split(":", 1)[1]}],
            "properties": [
                {"name": "kairos:bundled.ref", "value": ref},
                {"name": "kairos:bundled.verified", "value": str(bool(img["verified"])).lower()},
                {"name": "kairos:bundled.verifyReason", "value": img.get("verifyReason", "")},
            ],
        })

    bom = {
        "bomFormat": "CycloneDX",
        "specVersion": "1.5",
        "version": 1,
        "metadata": {
            "component": {
                "type": "application",
                "name": "provider-kubernetes-bundled-control-plane-images",
                "version": lock["kubernetesVersion"],
            },
            "properties": [
                {"name": "kairos:bundled.verifiedBy.identity", "value": lock["verifiedBy"]["identity"]},
                {"name": "kairos:bundled.verifiedBy.issuer", "value": lock["verifiedBy"]["issuer"]},
                {"name": "kairos:bundled.imageRepository", "value": lock["imageRepository"]},
            ],
        },
        "components": components,
    }

    json.dump(bom, open(sys.argv[2], "w"), indent=2)
    verified = sum(1 for c in lock["images"] if c["verified"])
    print("wrote %s: %d components, %d signature-verified" %
          (sys.argv[2], len(components), verified))
    return 0


if __name__ == "__main__":
    sys.exit(main())
