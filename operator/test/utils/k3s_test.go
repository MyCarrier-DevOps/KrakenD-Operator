package utils

import (
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
