package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderGitOpsSourceRendersACMEIssuerTemplate(t *testing.T) {
	t.Parallel()

	cfg := workflowTestConfig(3)
	sourceRoot := t.TempDir()

	templatePath := filepath.Join(sourceRoot, "core", "cert-manager-issuer", "clusterissuer.yaml.tmpl")
	if err := os.MkdirAll(filepath.Dir(templatePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(templatePath, []byte("email: {{ .ACMEEmail }}\n"), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "kustomization.yaml"), []byte("kind: Kustomization\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	renderedRoot, cleanup, err := renderGitOpsSource(sourceRoot, cfg)
	if err != nil {
		t.Fatalf("render gitops source: %v", err)
	}
	defer cleanup()

	renderedPath := filepath.Join(renderedRoot, "core", "cert-manager-issuer", "clusterissuer.yaml")
	rendered, err := os.ReadFile(renderedPath)
	if err != nil {
		t.Fatalf("read rendered file: %v", err)
	}
	if strings.Contains(string(rendered), "{{") {
		t.Fatalf("expected template markers to be rendered, got %s", rendered)
	}
	if !strings.Contains(string(rendered), cfg.Cluster.ACMEEmail) {
		t.Fatalf("expected ACME email in rendered manifest, got %s", rendered)
	}
}
