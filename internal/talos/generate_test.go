package talos

import (
	"context"
	"strings"
	"testing"
)

func TestGenerateConfigUsesSeparateTalosEndpoints(t *testing.T) {
	secretsYAML, err := GenerateSecretsYAML(context.Background())
	if err != nil {
		t.Fatalf("GenerateSecretsYAML() error = %v", err)
	}

	result, err := GenerateConfig(context.Background(), GenConfigParams{
		ClusterName:        "intar",
		Endpoint:           "cluster.intar.app",
		TalosEndpoints:     []string{"49.13.128.157", "49.13.142.173"},
		TalosVersion:       "v1.12.6",
		KubernetesVersion:  "1.35.3",
		SecretsYAML:        secretsYAML,
		ControlPlaneTaints: false,
	})
	if err != nil {
		t.Fatalf("GenerateConfig() error = %v", err)
	}

	configText := string(result.Talosconfig)
	if !strings.Contains(configText, "- 49.13.128.157") || !strings.Contains(configText, "- 49.13.142.173") {
		t.Fatalf("talosconfig endpoints not rendered as expected:\n%s", configText)
	}
	if strings.Contains(configText, "- cluster.intar.app") {
		t.Fatalf("talosconfig should not use Kubernetes endpoint as Talos endpoint:\n%s", configText)
	}
}
