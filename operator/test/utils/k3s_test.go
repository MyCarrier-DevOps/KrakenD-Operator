package utils

import (
	"slices"
	"strings"
	"testing"
)

const rootlessPodmanHost = "unix:///run/user/1000/podman/podman.sock"

func TestK3sArgsFor_NoRootlessArgsOutsideRootlessPodman(t *testing.T) {
	for _, host := range []string{"", "unix:///var/run/docker.sock", "unix:///run/podman/podman.sock"} {
		for _, arg := range k3sArgsFor(host) {
			if strings.HasPrefix(arg, "--kubelet-arg=") {
				t.Errorf("DOCKER_HOST=%q: unexpected %s", host, arg)
			}
		}
	}
}

func TestK3sArgsFor_RootlessPodmanGetsKubeletArgs(t *testing.T) {
	args := k3sArgsFor(rootlessPodmanHost)

	for _, want := range []string{
		"--kubelet-arg=feature-gates=KubeletInUserNamespace=true",
		"--kubelet-arg=cgroups-per-qos=false",
		"--kubelet-arg=enforce-node-allocatable=",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("rootless podman: missing %s in %v", want, args)
		}
	}
}

func TestK3sArgsFor_AlwaysDisablesAddonsAndEnablesOwnerRefEnforcement(t *testing.T) {
	for _, host := range []string{"", rootlessPodmanHost} {
		args := k3sArgsFor(host)
		for _, want := range []string{
			"--disable=traefik",
			"--disable=metrics-server",
			"--kube-apiserver-arg=enable-admission-plugins=OwnerReferencesPermissionEnforcement",
		} {
			if !slices.Contains(args, want) {
				t.Errorf("DOCKER_HOST=%q: missing %s", host, want)
			}
		}
	}
}
