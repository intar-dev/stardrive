package workflow

import (
	"slices"
	"strings"
	"testing"
)

func TestTalosCiliumInstallFlagsMatchTalosRequirements(t *testing.T) {
	t.Parallel()

	flags := talosCiliumInstallFlags()
	expected := []string{
		"ipam.mode=kubernetes",
		"kubeProxyReplacement=false",
		"securityContext.capabilities.ciliumAgent={CHOWN,KILL,NET_ADMIN,NET_RAW,IPC_LOCK,SYS_ADMIN,SYS_RESOURCE,DAC_OVERRIDE,FOWNER,SETGID,SETUID}",
		"securityContext.capabilities.cleanCiliumState={NET_ADMIN,SYS_ADMIN,SYS_RESOURCE}",
		"cgroup.autoMount.enabled=false",
		"cgroup.hostRoot=/sys/fs/cgroup",
	}
	for _, want := range expected {
		if !slices.Contains(flags, want) {
			t.Fatalf("expected Cilium install flags to contain %q, got %v", want, flags)
		}
	}
}

func TestFluxOCIBootstrapManifestStagesDependentKustomizations(t *testing.T) {
	t.Parallel()

	app := &App{}
	cfg := workflowTestConfig(3)

	manifest := string(app.fluxOCIBootstrapManifest(cfg))
	for _, needle := range []string{
		`path: "./core/external-secrets"`,
		`path: "./core/cert-manager"`,
		`path: "./core/cert-manager-issuer"`,
		`path: "./core/cluster-secrets"`,
		`path: "./apps"`,
		`- name: stardrive`,
		`- name: stardrive-cert-manager`,
	} {
		if !strings.Contains(manifest, needle) {
			t.Fatalf("expected flux bootstrap manifest to contain %q, got:\n%s", needle, manifest)
		}
	}
}
