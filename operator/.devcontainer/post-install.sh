#!/bin/bash
set -euxo pipefail

# Pinned tool versions and checksums for supply-chain integrity.
KIND_VERSION="v0.27.0"
KIND_SHA256="a6875aaea358acf0ac07786b1a6755d08fd640f4c79b7a2e46681cc13f49a04b"
KUBEBUILDER_VERSION="v4.13.1"
KUBEBUILDER_SHA256="c91a214f3cada120cd7468ee4633621da963e997cf19a1cf320ebd36e6324fd6"
KUBECTL_VERSION="v1.33.0"
KUBECTL_SHA256="9efe8d3facb23e1618cba36fb1c4e15ac9dc3ed5a2c2e18109e4a66b2bac12dc"
HELM_VERSION="v4.3.0"
HELM_SHA256="86584a54def73570558f66f5111cc53dfed56689637ae32c1201205d494f54fb"

download_and_verify() {
    local name="$1"
    local url="$2"
    local expected_sha256="$3"
    local tmpfile
    tmpfile="$(mktemp)"

    curl -fsSL -o "$tmpfile" "$url"
    echo "${expected_sha256}  ${tmpfile}" | sha256sum -c -
    chmod +x "$tmpfile"
    mv "$tmpfile" "/usr/local/bin/${name}"
}

download_and_verify "kind" \
    "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-amd64" \
    "${KIND_SHA256}"

download_and_verify "kubebuilder" \
    "https://github.com/kubernetes-sigs/kubebuilder/releases/download/${KUBEBUILDER_VERSION}/kubebuilder_linux_amd64" \
    "${KUBEBUILDER_SHA256}"

download_and_verify "kubectl" \
    "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    "${KUBECTL_SHA256}"

# download_tarball_binary NAME URL SHA256 PATH_IN_TARBALL: like download_and_verify, for a
# binary that ships inside a .tar.gz.
download_tarball_binary() {
    local name="$1"
    local url="$2"
    local expected_sha256="$3"
    local path_in_tarball="$4"
    local tmpdir
    tmpdir="$(mktemp -d)"

    curl -fsSL -o "${tmpdir}/archive.tar.gz" "$url"
    echo "${expected_sha256}  ${tmpdir}/archive.tar.gz" | sha256sum -c -
    tar -xzf "${tmpdir}/archive.tar.gz" -C "$tmpdir" "$path_in_tarball"
    chmod +x "${tmpdir}/${path_in_tarball}"
    mv "${tmpdir}/${path_in_tarball}" "/usr/local/bin/${name}"
    rm -rf "$tmpdir"
}

download_tarball_binary "helm" \
    "https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz" \
    "${HELM_SHA256}" \
    "linux-amd64/helm"

docker network create -d=bridge --subnet=172.19.0.0/24 kind || true

kind version
kubebuilder version
docker --version
go version
kubectl version --client
helm version
