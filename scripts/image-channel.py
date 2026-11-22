#!/usr/bin/env python3
"""Discover pinned upstream recipes for the stable image channel."""
import base64
import datetime
import hashlib
import json
import os
import re
import subprocess
import tempfile
import urllib.error
import urllib.request
from pathlib import Path

STABLE = "https://api.github.com/repos/illusions-lab/Ephemera/contents/stable.json?ref=image-channel"
UBUNTU = "https://cloud-images.ubuntu.com/releases/noble"
API = "https://api.github.com"
OCI = "ghcr.io/cirruslabs/macos-tahoe-base"


def fetch(url, headers=None):
    with urllib.request.urlopen(urllib.request.Request(url, headers=headers or {}), timeout=30) as response:
        return response.read(), response.headers


def main():
    try:
        file = json.loads(fetch(STABLE, {"Authorization": "Bearer " + os.environ["GH_TOKEN"]})[0])
        previous = json.loads(base64.b64decode(file["content"]))["images"]
    except urllib.error.HTTPError as error:
        if error.code != 404:
            raise
        previous = {}
    release = json.loads(fetch(f"{API}/repos/actions/runner/releases/latest", {
        "Authorization": "Bearer " + os.environ["GH_TOKEN"]
    })[0])
    runner_version = release["tag_name"].removeprefix("v")
    if not re.fullmatch(r"\d+\.\d+\.\d+", runner_version):
        raise ValueError("unexpected upstream Runner version")
    digests = {asset["name"]: asset.get("digest") for asset in release["assets"]}

    def runner_sha(platform):
        digest = digests[f"actions-runner-{platform}-{runner_version}.tar.gz"]
        if not digest or not re.fullmatch(r"sha256:[a-f0-9]{64}", digest):
            raise ValueError(f"missing Runner asset digest for {platform}")
        return digest.removeprefix("sha256:")

    page = fetch(UBUNTU + "/")[0].decode()
    latest = max(re.findall(r'href="(release-\d{8})/"', page))
    base = f"{UBUNTU}/{latest}"
    with tempfile.TemporaryDirectory() as temporary:
        directory = Path(temporary)
        sums = directory / "SHA256SUMS"
        signature = directory / "SHA256SUMS.gpg"
        sums.write_bytes(fetch(base + "/SHA256SUMS")[0])
        signature.write_bytes(fetch(base + "/SHA256SUMS.gpg")[0])
        subprocess.run(["gpgv", "--keyring", "/usr/share/keyrings/ubuntu-cloudimage-keyring.gpg",
                        str(signature), str(sums)], check=True)
        sha = dict((name.lstrip("*"), digest) for digest, name in
                   (line.split() for line in sums.read_text().splitlines() if line.strip()))

    minimum = os.environ["IMAGE_MIN_VERSION"]
    images = {}
    now = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT000000Z")
    for arch, runner_arch in (("amd64", "x64"), ("arm64", "arm64")):
        filename = f"ubuntu-24.04-server-cloudimg-{arch}.img"
        images[f"linux/{arch}"] = {"min_version": minimum, "ubuntu": {
            "BaseURL": f"{base}/{filename}", "BaseSHA256": sha[filename],
            "RunnerVersion": runner_version, "RunnerSHA256": runner_sha("linux-" + runner_arch),
        }}
    oci_path = OCI.removeprefix("ghcr.io/")
    auth = json.loads(fetch(f"https://ghcr.io/token?scope=repository:{oci_path}:pull")[0])
    manifest_bytes, headers = fetch(f"https://ghcr.io/v2/{oci_path}/manifests/latest", {
        "Authorization": "Bearer " + auth["token"],
        "Accept": "application/vnd.oci.image.manifest.v1+json",
    })
    oci_digest = "sha256:" + hashlib.sha256(manifest_bytes).hexdigest()
    if headers.get("Docker-Content-Digest") != oci_digest:
        raise ValueError("Tart base manifest digest mismatch")
    images["darwin/arm64"] = {"min_version": minimum, "tart_ref": OCI + "@" + oci_digest,
                            "runner_version": runner_version, "runner_sha256": runner_sha("osx-arm64")}
    for platform, recipe in images.items():
        old = previous.get(platform, {})
        comparable = {key: value for key, value in old.items() if key != "revision"}
        if "ubuntu" in recipe:
            old_source = dict(comparable.get("ubuntu", {}))
            old_snapshot = old_source.pop("snapshot", None)
            if old_source:
                comparable["ubuntu"] = old_source
            recipe["ubuntu"]["snapshot"] = old_snapshot if comparable == recipe and old_snapshot else now
            if old_snapshot:
                comparable["ubuntu"]["snapshot"] = old_snapshot
        recipe["revision"] = old.get("revision", 0) + (comparable != recipe)
    Path("stable.json").write_text(json.dumps({"schema": 1, "images": images}, indent=2, sort_keys=True) + "\n")
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        output.write(f"changed={str(previous != images).lower()}\n")


if __name__ == "__main__":
    main()
