package utils

import (
	"os"
	"strings"
)

// K3sImage is the K3s image both the integration and the e2e suites start.
// K3s 1.32.x is used instead of 1.33.x because K8s 1.33 removed the
// KubeletInUserNamespace feature gate (graduated to GA), which rootless podman
// needs. client-go v0.33.0 supports +/-1 minor version skew per the K8s policy.
const K3sImage = "rancher/k3s:v1.32.13-k3s1"

// K3sArgs returns the K3s server arguments both suites use.
//
// When the container host is rootless podman the kubelet args work around its
// constraints:
//   - KubeletInUserNamespace: graceful fallback when /dev/kmsg is unavailable
//   - cgroups-per-qos=false + enforce-node-allocatable="": skip the cgroup
//     hierarchy creation that fails in rootless cgroupv2 containers
//
// Clusters that enable the OwnerReferencesPermissionEnforcement admission
// plugin (OpenShift by default) require delete on any object whose
// ownerReferences an update changes, so the suites enable it.
func K3sArgs() []string {
	return k3sArgsFor(os.Getenv("DOCKER_HOST"))
}

// k3sArgsFor builds the arguments for the given container host (DOCKER_HOST).
// The kubelet args are added only for rootless podman, whose socket lives under
// /run/user/<uid>/. Rootful hosts such as a CI runner keep the kubelet defaults.
func k3sArgsFor(dockerHost string) []string {
	args := []string{
		"--disable=traefik",
		"--disable=metrics-server",
		"--kube-apiserver-arg=enable-admission-plugins=OwnerReferencesPermissionEnforcement",
	}
	if strings.Contains(dockerHost, "/run/user/") && strings.Contains(dockerHost, "podman") {
		args = append(args,
			"--kubelet-arg=feature-gates=KubeletInUserNamespace=true",
			"--kubelet-arg=cgroups-per-qos=false",
			"--kubelet-arg=enforce-node-allocatable=",
		)
	}
	return args
}
