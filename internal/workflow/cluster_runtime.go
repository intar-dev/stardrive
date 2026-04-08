package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apricote/hcloud-upload-image/hcloudimages"
	"github.com/intar-dev/stardrive/internal/config"
	"github.com/intar-dev/stardrive/internal/hetzner"
	"github.com/intar-dev/stardrive/internal/infisical"
	"github.com/intar-dev/stardrive/internal/names"
	"github.com/intar-dev/stardrive/internal/talos"
	"gopkg.in/yaml.v3"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	registryPVCName          = "registry-data"
	registryStorageSecret    = "storagebox-credentials"
	registryTLSSecret        = "stardrive-registry-tls"
	fluxRegistryCASecret     = "stardrive-registry-ca"
	hetznerCloudSecretName   = "hcloud"
	registryBootstrapPVName  = "storagebox-registry"
	resourceManagedByKey     = "stardrive.dev-managed-by"
	resourceManagedByValue   = "stardrive"
	resourceClusterKey       = "stardrive.dev-cluster"
	resourceNodeKey          = "stardrive.dev-node"
	resourceImageIDKey       = "stardrive.dev-image-id"
	resourceRepositoryPrefix = "gitops"
	hetznerUserDataByteLimit = 32 * 1024
)

type inlineManifest struct {
	Name     string
	Contents string
}

func (a *App) regenerateTalosAssets(ctx context.Context, cfg *config.Config, infClient *infisical.Client, existingSecrets []byte) (clusterAccessSecrets, error) {
	secretsYAML := bytes.TrimSpace(existingSecrets)
	if len(secretsYAML) == 0 {
		generated, err := talos.GenerateSecretsYAML(ctx)
		if err != nil {
			return clusterAccessSecrets{}, err
		}
		secretsYAML = generated
	}

	sans := []string{cfg.DNS.APIHostname}
	for _, node := range cfg.Nodes {
		sans = append(sans, node.Name)
		if cfg.DNS.ManageNodeRecords {
			sans = append(sans, nodeDNSName(cfg, node))
		}
		if node.PublicIPv4 != "" {
			sans = append(sans, node.PublicIPv4)
		}
	}

	generated, err := talos.GenerateConfig(ctx, talos.GenConfigParams{
		ClusterName:                 cfg.Cluster.Name,
		Endpoint:                    cfg.DNS.APIHostname,
		TalosEndpoints:              controlPlaneIPs(cfg),
		TalosVersion:                cfg.Cluster.TalosVersion,
		TalosSchematic:              cfg.Cluster.TalosSchematic,
		KubernetesVersion:           cfg.Cluster.KubernetesVersion,
		ControlPlaneTaints:          cfg.Cluster.ControlPlaneTaints,
		KubernetesAPIServerCertSANs: uniqueNonEmpty(sans),
		SecretsYAML:                 secretsYAML,
	})
	if err != nil {
		return clusterAccessSecrets{}, err
	}

	secrets := clusterAccessSecrets{
		TalosSecretsYAML:       slicesClone(secretsYAML),
		ControlPlaneConfigYAML: slicesClone(generated.ControlPlane),
		WorkerConfigYAML:       slicesClone(generated.Worker),
		TalosconfigYAML:        slicesClone(generated.Talosconfig),
	}

	if infClient != nil {
		if err := infClient.SetSecrets(ctx, cfg.Infisical.ProjectID, cfg.Infisical.Environment, cfg.Secrets().ClusterAccess, map[string]string{
			secretTalosSecretsYAML:      string(secrets.TalosSecretsYAML),
			secretTalosControlPlaneYAML: string(secrets.ControlPlaneConfigYAML),
			secretTalosWorkerYAML:       string(secrets.WorkerConfigYAML),
			secretTalosconfigYAML:       string(secrets.TalosconfigYAML),
		}); err != nil {
			return clusterAccessSecrets{}, err
		}
	}

	if err := a.writeLocalStateFile(cfg.Cluster.Name, "talos-secrets.yaml", secrets.TalosSecretsYAML, 0o600); err != nil {
		return clusterAccessSecrets{}, err
	}
	if err := a.writeLocalStateFile(cfg.Cluster.Name, "controlplane.yaml", secrets.ControlPlaneConfigYAML, 0o600); err != nil {
		return clusterAccessSecrets{}, err
	}
	if err := a.writeLocalStateFile(cfg.Cluster.Name, "worker.yaml", secrets.WorkerConfigYAML, 0o600); err != nil {
		return clusterAccessSecrets{}, err
	}
	if err := a.writeLocalStateFile(cfg.Cluster.Name, "talosconfig", secrets.TalosconfigYAML, 0o600); err != nil {
		return clusterAccessSecrets{}, err
	}

	return secrets, nil
}

func slicesClone(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	return append([]byte(nil), data...)
}

func (a *App) ensureHetznerTalosImage(ctx context.Context, cfg *config.Config, hzClient *hetzner.Client, infClient *infisical.Client, _ storageBoxSecrets) (runtimeSecrets, error) {
	current, _ := a.loadRuntimeSecrets(ctx, cfg)
	expectedBootImageURL, err := talos.BuildFactoryDiskImageURL(cfg.Cluster.TalosVersion, cfg.Cluster.TalosSchematic, "hcloud-amd64.raw.xz")
	if err != nil {
		return runtimeSecrets{}, err
	}
	expectedInstallerImage := installerImageForVersion(cfg.Cluster.TalosVersion, cfg.Cluster.TalosSchematic)

	if current.BootImageID > 0 && strings.TrimSpace(current.InstallerImage) != "" &&
		strings.TrimSpace(current.BootImageURL) == expectedBootImageURL &&
		strings.TrimSpace(current.InstallerImage) == expectedInstallerImage {
		return current, nil
	}

	imageID := talosImageIdentity(cfg)
	labels := map[string]string{
		resourceManagedByKey: resourceManagedByValue,
		resourceClusterKey:   hetznerLabelValue(names.Slugify(cfg.Cluster.Name)),
		resourceImageIDKey:   hetznerLabelValue(imageID),
	}
	images, err := hzClient.ListImages(ctx, labels)
	if err != nil {
		return runtimeSecrets{}, err
	}
	for _, image := range images {
		if image.ID <= 0 {
			continue
		}
		current.BootImageID = image.ID
		current.BootImageURL = expectedBootImageURL
		current.InstallerImage = expectedInstallerImage
		current.Repository = gitOpsRepository(cfg)
		current.RegistryAddress = cfg.EffectiveRegistryAddress()
		if err := infClient.SetSecrets(ctx, cfg.Infisical.ProjectID, cfg.Infisical.Environment, cfg.Secrets().ClusterRuntime, map[string]string{
			secretTalosBootImageID:    fmt.Sprintf("%d", current.BootImageID),
			secretTalosBootImageURL:   current.BootImageURL,
			secretTalosInstallerImage: current.InstallerImage,
			secretRegistryAddress:     current.RegistryAddress,
			secretRegistryRepository:  current.Repository,
		}); err != nil {
			return runtimeSecrets{}, err
		}
		return current, nil
	}

	artifactReader, compression, cleanup, err := a.openRemoteTalosDiskArtifact(ctx, expectedBootImageURL)
	if err != nil {
		return runtimeSecrets{}, err
	}
	defer cleanup()

	description := fmt.Sprintf("Stardrive Talos %s for %s (%s)", cfg.Cluster.TalosVersion, cfg.Cluster.Name, imageID[:12])
	image, err := hzClient.UploadImageFromReader(ctx, artifactReader, clusterResourceName(cfg, "talos"), description, labels, compression, "amd64", cfg.Hetzner.Location)
	if err != nil {
		return runtimeSecrets{}, err
	}
	available, err := hzClient.WaitForImageAvailable(ctx, image.ID, 45*time.Minute)
	if err != nil {
		return runtimeSecrets{}, err
	}

	current.BootImageID = available.ID
	current.BootImageURL = expectedBootImageURL
	current.InstallerImage = expectedInstallerImage
	current.RegistryAddress = cfg.EffectiveRegistryAddress()
	current.Repository = gitOpsRepository(cfg)
	if err := infClient.SetSecrets(ctx, cfg.Infisical.ProjectID, cfg.Infisical.Environment, cfg.Secrets().ClusterRuntime, map[string]string{
		secretTalosBootImageID:    fmt.Sprintf("%d", current.BootImageID),
		secretTalosBootImageURL:   current.BootImageURL,
		secretTalosInstallerImage: current.InstallerImage,
		secretRegistryAddress:     current.RegistryAddress,
		secretRegistryRepository:  current.Repository,
	}); err != nil {
		return runtimeSecrets{}, err
	}
	return current, nil
}

func (a *App) openRemoteTalosDiskArtifact(ctx context.Context, artifactURL string) (io.ReadCloser, hcloudimages.Compression, func(), error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(artifactURL), nil)
	if err != nil {
		return nil, hcloudimages.CompressionNone, nil, fmt.Errorf("build Talos image request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, hcloudimages.CompressionNone, nil, fmt.Errorf("download Talos disk artifact %s: %w", artifactURL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, hcloudimages.CompressionNone, nil, fmt.Errorf("download Talos disk artifact %s: unexpected status %s", artifactURL, resp.Status)
	}

	compression, err := imageCompressionForPath(artifactURL)
	if err != nil {
		resp.Body.Close()
		return nil, hcloudimages.CompressionNone, nil, err
	}
	a.logInfo("streaming Talos boot image from Image Factory", "url", artifactURL, "compression", compression)
	return resp.Body, compression, func() { _ = resp.Body.Close() }, nil
}

func imageCompressionForPath(path string) (hcloudimages.Compression, error) {
	switch {
	case strings.HasSuffix(path, ".xz"):
		return hcloudimages.CompressionXZ, nil
	case strings.HasSuffix(path, ".zst"):
		return hcloudimages.CompressionZSTD, nil
	case strings.HasSuffix(path, ".raw"):
		return hcloudimages.CompressionNone, nil
	default:
		return hcloudimages.CompressionNone, fmt.Errorf("unsupported Talos disk artifact format: %s", path)
	}
}

func talosImageIdentity(cfg *config.Config) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"hcloud-amd64.raw.xz",
		strings.TrimSpace(cfg.Cluster.TalosVersion),
		strings.TrimSpace(cfg.Cluster.TalosSchematic),
	}, "|")))
	return hex.EncodeToString(sum[:])
}

func (a *App) ensureHetznerNetworking(ctx context.Context, cfg *config.Config, hzClient *hetzner.Client, infClient *infisical.Client) (runtimeSecrets, error) {
	network, err := hzClient.EnsureNetwork(ctx, clusterResourceName(cfg, "network"), cfg.Hetzner.PrivateNetworkCIDR)
	if err != nil {
		return runtimeSecrets{}, err
	}
	if err := hzClient.EnsureSubnet(ctx, network, cfg.Hetzner.NetworkZone, cfg.Hetzner.PrivateNetworkCIDR); err != nil {
		return runtimeSecrets{}, err
	}
	placementGroup, err := hzClient.EnsurePlacementGroup(ctx, clusterResourceName(cfg, "placement-group"))
	if err != nil {
		return runtimeSecrets{}, err
	}
	firewall, err := hzClient.EnsureFirewall(ctx, clusterResourceName(cfg, "firewall"), nil)
	if err != nil {
		return runtimeSecrets{}, err
	}
	loadBalancer, err := hzClient.EnsureLoadBalancer(ctx, clusterResourceName(cfg, "api-lb"), cfg.Hetzner.LoadBalancerType, cfg.Hetzner.Location, network)
	if err != nil {
		return runtimeSecrets{}, err
	}

	runtime := runtimeSecrets{
		NetworkID:        network.ID,
		PlacementGroupID: placementGroup.ID,
		FirewallID:       firewall.ID,
		LoadBalancerID:   loadBalancer.ID,
		LoadBalancerIPv4: strings.TrimSpace(loadBalancer.PublicNet.IPv4.IP.String()),
	}
	if err := infClient.SetSecrets(ctx, cfg.Infisical.ProjectID, cfg.Infisical.Environment, cfg.Secrets().ClusterRuntime, map[string]string{
		secretHetznerNetworkID:        fmt.Sprintf("%d", runtime.NetworkID),
		secretHetznerPlacementGroupID: fmt.Sprintf("%d", runtime.PlacementGroupID),
		secretHetznerFirewallID:       fmt.Sprintf("%d", runtime.FirewallID),
		secretHetznerLoadBalancerID:   fmt.Sprintf("%d", runtime.LoadBalancerID),
		secretHetznerLoadBalancerIPv4: runtime.LoadBalancerIPv4,
	}); err != nil {
		return runtimeSecrets{}, err
	}
	return runtime, nil
}

func (a *App) ensureClusterServers(ctx context.Context, cfg *config.Config, hzClient *hetzner.Client, _ *infisical.Client, runtime runtimeSecrets, renderedConfigs map[string][]byte) error {
	network, err := hzClient.EnsureNetwork(ctx, clusterResourceName(cfg, "network"), cfg.Hetzner.PrivateNetworkCIDR)
	if err != nil {
		return err
	}
	placementGroup, err := hzClient.EnsurePlacementGroup(ctx, clusterResourceName(cfg, "placement-group"))
	if err != nil {
		return err
	}
	firewall, err := hzClient.EnsureFirewall(ctx, clusterResourceName(cfg, "firewall"), nil)
	if err != nil {
		return err
	}
	loadBalancer, err := hzClient.EnsureLoadBalancer(ctx, clusterResourceName(cfg, "api-lb"), cfg.Hetzner.LoadBalancerType, cfg.Hetzner.Location, network)
	if err != nil {
		return err
	}

	existing, err := hzClient.ListServers(ctx, clusterResourceLabels(cfg))
	if err != nil {
		return err
	}
	byNode := map[string]hetzner.Server{}
	for _, server := range existing {
		if value := strings.TrimSpace(server.Name); value != "" {
			byNode[value] = server
		}
	}

	for i := range cfg.Nodes {
		node := &cfg.Nodes[i]
		if existingServer, ok := byNode[node.Name]; ok {
			node.ServerID = existingServer.ID
			node.InstanceID = existingServer.ID
			node.PublicIPv4 = existingServer.PublicIPv4
			if strings.TrimSpace(existingServer.PrivateIPv4) != "" {
				node.PrivateIPv4 = existingServer.PrivateIPv4
			}
			node.PublicIPv6 = existingServer.PublicIPv6
			continue
		}

		userData, ok := renderedConfigs[node.Name]
		if !ok {
			return fmt.Errorf("rendered Talos config for node %s is missing", node.Name)
		}
		created, err := hzClient.CreateServer(ctx, hetzner.ServerCreateRequest{
			Name:           node.Name,
			ServerType:     cfg.Hetzner.ServerType,
			Location:       cfg.Hetzner.Location,
			ImageID:        runtime.BootImageID,
			UserData:       string(userData),
			PrivateIPv4:    node.PrivateIPv4,
			Network:        network,
			PlacementGroup: placementGroup,
			Firewall:       firewall,
			Labels:         nodeResourceLabels(cfg, *node),
			PublicIPv6:     cfg.Hetzner.PublicIPv6,
		})
		if err != nil {
			return err
		}
		node.ServerID = created.ID
		node.InstanceID = created.ID
		node.PublicIPv4 = created.PublicIPv4
		if strings.TrimSpace(created.PrivateIPv4) != "" {
			node.PrivateIPv4 = created.PrivateIPv4
		}
		node.PublicIPv6 = created.PublicIPv6
	}

	serverIDs := make([]int64, 0, len(cfg.Nodes))
	for _, node := range cfg.Nodes {
		if node.ProviderID() > 0 {
			serverIDs = append(serverIDs, node.ProviderID())
		}
	}
	if err := hzClient.SyncLoadBalancerTargetsByID(ctx, loadBalancer.ID, serverIDs); err != nil {
		return err
	}
	return nil
}

func (a *App) waitForTalosSecure(ctx context.Context, nodeAddress string, talosconfig []byte) error {
	a.logInfo("waiting for Talos secure API", "node", nodeAddress, "timeout", time.Until(deadlineOrNow(ctx)).String())
	return waitFor(ctx, 5*time.Second, func(ctx context.Context) error {
		client, err := talos.NewClient(nodeAddress, talosconfig)
		if err != nil {
			return err
		}
		defer client.Close()
		_, err = client.Version(ctx)
		return err
	})
}

func (a *App) fetchKubeconfig(ctx context.Context, nodeAddress string, talosconfig []byte) ([]byte, error) {
	client, err := talos.NewClient(nodeAddress, talosconfig)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.Kubeconfig(ctx)
}

func (a *App) waitForLoadBalancerTargets(ctx context.Context, cfg *config.Config, hzClient *hetzner.Client, runtime runtimeSecrets) error {
	if hzClient == nil || runtime.LoadBalancerID == 0 {
		return nil
	}

	serverIDs := make([]int64, 0, len(cfg.Nodes))
	for _, node := range cfg.Nodes {
		if id := node.ProviderID(); id > 0 {
			serverIDs = append(serverIDs, id)
		}
	}
	if len(serverIDs) == 0 {
		return nil
	}

	a.logInfo("waiting for Hetzner load balancer targets",
		"cluster", cfg.Cluster.Name,
		"load_balancer_id", runtime.LoadBalancerID,
		"targets", len(serverIDs),
		"port", 6443,
		"timeout", time.Until(deadlineOrNow(ctx)).String(),
	)
	lastSummary := ""
	return waitFor(ctx, 5*time.Second, func(ctx context.Context) error {
		ready, summary, err := hzClient.LoadBalancerTargetsHealthy(ctx, runtime.LoadBalancerID, serverIDs, 6443)
		if err != nil {
			return err
		}
		if summary != "" && summary != lastSummary {
			a.logInfo("load balancer target status",
				"cluster", cfg.Cluster.Name,
				"load_balancer_id", runtime.LoadBalancerID,
				"status", summary,
			)
			lastSummary = summary
		}
		if !ready {
			return fmt.Errorf("load balancer targets are not ready")
		}
		return nil
	})
}

func (a *App) waitForKubernetesAPI(ctx context.Context, cfg *config.Config, kubeconfigPath string) error {
	a.logInfo("waiting for Kubernetes API endpoint",
		"cluster", cfg.Cluster.Name,
		"endpoint", cfg.DNS.APIHostname,
		"timeout", time.Until(deadlineOrNow(ctx)).String(),
	)
	restConfig, err := clientcmd.BuildConfigFromFlags("", strings.TrimSpace(kubeconfigPath))
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	restConfig.Timeout = 10 * time.Second
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create kubernetes discovery client: %w", err)
	}
	lastError := ""
	return waitFor(ctx, 5*time.Second, func(ctx context.Context) error {
		_, err := discoveryClient.ServerVersion()
		if err != nil {
			if err.Error() != lastError {
				a.logInfo("Kubernetes API not ready yet",
					"cluster", cfg.Cluster.Name,
					"endpoint", cfg.DNS.APIHostname,
					"error", err,
				)
				lastError = err.Error()
			}
			return err
		}
		return nil
	})
}

func (a *App) persistKubeconfig(ctx context.Context, cfg *config.Config, infClient *infisical.Client, kubeconfig []byte) error {
	if err := infClient.SetSecrets(ctx, cfg.Infisical.ProjectID, cfg.Infisical.Environment, cfg.Secrets().ClusterAccess, map[string]string{
		secretKubeconfigYAML: string(kubeconfig),
	}); err != nil {
		return err
	}
	return a.writeLocalStateFile(cfg.Cluster.Name, "kubeconfig", kubeconfig, 0o600)
}

func (a *App) verifyTalosHealth(ctx context.Context, cfg *config.Config, talosconfig []byte) error {
	endpoint := firstControlPlaneIP(cfg)
	if endpoint == "" {
		return fmt.Errorf("no control-plane public IPv4 addresses are available")
	}
	controlPlaneNodes := controlPlaneHealthIPs(cfg)
	if len(controlPlaneNodes) == 0 {
		return fmt.Errorf("no control-plane node IPs are available for health checks")
	}
	kubernetesEndpoint := endpoint
	client, err := talos.NewClient(endpoint, talosconfig)
	if err != nil {
		return err
	}
	defer client.Close()
	a.logInfo("verifying Talos cluster health", "endpoint", endpoint, "kubernetes_endpoint", kubernetesEndpoint, "control_plane_nodes", controlPlaneNodes)
	return client.HealthCheck(ctx, 15*time.Minute, controlPlaneNodes, nil, kubernetesEndpoint)
}

func (a *App) installCilium(ctx context.Context, cfg *config.Config, kubeconfigPath string) error {
	a.logInfo("installing Cilium", "cluster", cfg.Cluster.Name, "version", cfg.EffectiveCiliumVersion())
	ciliumBinary, err := a.ensureCiliumCLI(ctx)
	if err != nil {
		return err
	}
	env := a.kubectlEnv(kubeconfigPath)
	statusCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := a.probeCommand(statusCtx, env, nil, ciliumBinary, "status", "--wait-duration", "30s"); err == nil {
		a.logInfo("Cilium is already installed", "cluster", cfg.Cluster.Name)
		return nil
	}
	a.logInfo("Cilium is not installed yet; proceeding with install", "cluster", cfg.Cluster.Name)
	args := append([]string{"install", "--version", trimVersionPrefix(cfg.EffectiveCiliumVersion())}, talosCiliumInstallFlags()...)
	args = append(args, "--wait")
	if err := a.runCommand(ctx, env, nil, ciliumBinary, args...); err != nil {
		return fmt.Errorf("install Cilium: %w", err)
	}
	return a.runCommand(ctx, env, nil, ciliumBinary, "status", "--wait")
}

func talosCiliumInstallFlags() []string {
	return []string{
		"--set", "ipam.mode=kubernetes",
		"--set", "kubeProxyReplacement=false",
		"--set", `securityContext.capabilities.ciliumAgent={CHOWN,KILL,NET_ADMIN,NET_RAW,IPC_LOCK,SYS_ADMIN,SYS_RESOURCE,DAC_OVERRIDE,FOWNER,SETGID,SETUID}`,
		"--set", `securityContext.capabilities.cleanCiliumState={NET_ADMIN,SYS_ADMIN,SYS_RESOURCE}`,
		"--set", "cgroup.autoMount.enabled=false",
		"--set", "cgroup.hostRoot=/sys/fs/cgroup",
	}
}

func (a *App) waitForBootstrapRegistry(ctx context.Context, cfg *config.Config, kubeconfigPath string, storage storageBoxSecrets, infra infraSecrets, runtime runtimeSecrets) error {
	env := a.kubectlEnv(kubeconfigPath)
	if err := a.applyBootstrapClusterManifests(ctx, cfg, kubeconfigPath, storage, infra, runtime); err != nil {
		return err
	}
	if err := a.runCommand(ctx, env, nil, "kubectl", "wait", "--namespace", "kube-system", "--for=condition=Available", "deployment/hcloud-cloud-controller-manager", "--timeout=15m"); err != nil {
		return err
	}
	if err := a.runCommand(ctx, env, nil, "kubectl", "wait", "--namespace", "kube-system", "--for=condition=Available", "deployment/csi-smb-controller", "--timeout=15m"); err != nil {
		return err
	}
	if err := a.runCommand(ctx, env, nil, "kubectl", "rollout", "status", "--namespace", "kube-system", "daemonset/csi-smb-node", "--timeout=15m"); err != nil {
		return err
	}
	if err := a.runCommand(ctx, env, nil, "kubectl", "wait", "--namespace", cfg.RegistryNamespace(), "--for=jsonpath={.status.phase}=Bound", "pvc/"+registryPVCName, "--timeout=15m"); err != nil {
		return err
	}
	return a.runCommand(ctx, env, nil, "kubectl", "wait", "--namespace", cfg.RegistryNamespace(), "--for=condition=Available", "deployment/"+cfg.RegistryServiceName(), "--timeout=15m")
}

func (a *App) applyBootstrapClusterManifests(ctx context.Context, cfg *config.Config, kubeconfigPath string, storage storageBoxSecrets, infra infraSecrets, runtime runtimeSecrets) error {
	env := a.kubectlEnv(kubeconfigPath)
	for _, manifestURL := range smbDriverManifestURLs(cfg) {
		if err := a.runCommand(ctx, env, nil, "kubectl", "apply", "-f", manifestURL); err != nil {
			return fmt.Errorf("apply SMB CSI manifest %s: %w", manifestURL, err)
		}
	}

	manifests, err := a.bootstrapInlineManifests(cfg, storage, infra, runtime)
	if err != nil {
		return err
	}
	return a.runCommand(ctx, env, renderInlineManifestBundle(manifests), "kubectl", "apply", "-f", "-")
}

func (a *App) installFlux(ctx context.Context, cfg *config.Config, kubeconfigPath string) error {
	a.logInfo("installing Flux", "cluster", cfg.Cluster.Name, "version", cfg.EffectiveFluxVersion())
	env := a.kubectlEnv(kubeconfigPath)
	if err := a.runCommand(ctx, env, nil, "kubectl", "apply", "-f", fluxInstallURL(cfg.EffectiveFluxVersion())); err != nil {
		return fmt.Errorf("install Flux: %w", err)
	}
	for _, deployment := range []string{"source-controller", "kustomize-controller", "helm-controller", "notification-controller"} {
		if err := a.runCommand(ctx, env, nil, "kubectl", "wait", "--namespace", fluxNamespace, "--for=condition=Available", "deployment/"+deployment, "--timeout=10m"); err != nil {
			return fmt.Errorf("wait for Flux deployment %s: %w", deployment, err)
		}
	}
	return nil
}

func (a *App) applyFluxBootstrapOCI(ctx context.Context, cfg *config.Config, kubeconfigPath string, runtime runtimeSecrets) error {
	return a.runCommand(ctx, a.kubectlEnv(kubeconfigPath), a.fluxOCIBootstrapManifestWithCA(cfg, runtime.RegistryCACertPEM, runtime.GitOpsArtifactTag), "kubectl", "apply", "-f", "-")
}

func (a *App) waitForFluxBootstrapOCI(ctx context.Context, cfg *config.Config, kubeconfigPath string) error {
	env := a.kubectlEnv(kubeconfigPath)
	if err := a.runCommand(ctx, env, nil, "kubectl", "wait", "--namespace", fluxNamespace, "--for=condition=Ready", "ocirepository/"+fluxOCIRepositoryName, "--timeout=10m"); err != nil {
		return err
	}
	for _, name := range []string{fluxKustomizationName, fluxCertManagerKustomizationName} {
		if err := a.runCommand(ctx, env, nil, "kubectl", "wait", "--namespace", fluxNamespace, "--for=condition=Ready", "kustomization/"+name, "--timeout=15m"); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) waitForDeferredFluxBootstrapOCI(ctx context.Context, cfg *config.Config, kubeconfigPath string) error {
	env := a.kubectlEnv(kubeconfigPath)
	if err := a.runCommand(ctx, env, nil, "kubectl", "wait", "--namespace", fluxNamespace, "--for=condition=Ready", "kustomization/"+fluxAppsKustomizationName, "--timeout=15m"); err != nil {
		return err
	}
	for _, name := range []string{fluxIssuerKustomizationName, fluxClusterSecretsKustomizationName} {
		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := a.probeCommand(waitCtx, env, nil, "kubectl", "wait", "--namespace", fluxNamespace, "--for=condition=Ready", "kustomization/"+name, "--timeout=5s")
		cancel()
		if err != nil {
			a.logWarn("deferred Flux kustomization is not ready yet", "name", name)
		}
	}
	return nil
}

func (a *App) deleteFluxBootstrapResources(ctx context.Context, cfg *config.Config, kubeconfigPath string) error {
	_ = a.runCommand(ctx, a.kubectlEnv(kubeconfigPath), a.fluxOCIBootstrapManifest(cfg), "kubectl", "delete", "--ignore-not-found=true", "-f", "-")
	return nil
}

func (a *App) waitForKubernetesNodes(ctx context.Context, cfg *config.Config, kubeconfigPath string) error {
	a.logInfo("waiting for Kubernetes nodes to become Ready", "cluster", cfg.Cluster.Name, "nodes", sortedNodeNames(cfg))
	env := a.kubectlEnv(kubeconfigPath)
	for _, node := range sortedNodeNames(cfg) {
		if err := a.runCommand(ctx, env, nil, "kubectl", "wait", "--for=condition=Ready", "node/"+node, "--timeout=15m"); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) loadKubeconfigFromStateOrSecrets(ctx context.Context, cfg *config.Config) ([]byte, error) {
	statePath := filepath.Join(a.clusterStateDir(cfg.Cluster.Name), "kubeconfig")
	if data, err := os.ReadFile(statePath); err == nil && len(bytes.TrimSpace(data)) > 0 {
		return data, nil
	}
	access, err := a.loadClusterAccessSecrets(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(access.KubeconfigYAML)) == 0 {
		return nil, fmt.Errorf("kubeconfig is not available")
	}
	return access.KubeconfigYAML, nil
}

func (a *App) bootstrapInlineManifests(cfg *config.Config, storage storageBoxSecrets, infra infraSecrets, runtime runtimeSecrets) ([]inlineManifest, error) {
	if strings.TrimSpace(runtime.RegistryCACertPEM) == "" || strings.TrimSpace(runtime.RegistryTLSCertPEM) == "" || strings.TrimSpace(runtime.RegistryTLSKeyPEM) == "" {
		return nil, fmt.Errorf("registry TLS material is missing")
	}
	sharePath := strings.Trim(strings.TrimSpace(cfg.Storage.ShareName), "/")
	if sharePath == "" {
		sharePath = config.DefaultStorageShareName
	}
	clusterSlug := names.Slugify(cfg.Cluster.Name)
	smbSource := storage.SMBSource
	registryRootDirectory := "/var/lib/registry/" + sharePath + "/" + clusterSlug + "/registry"

	providerSecretManifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: kube-system
type: Opaque
stringData:
  token: %q
  network: %q
`, hetznerCloudSecretName, infra.Hetzner.Token, fmt.Sprintf("%d", runtime.NetworkID))

	secretManifest := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
---
apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
stringData:
  username: %q
  password: %q
---
apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: kubernetes.io/tls
stringData:
  tls.crt: |
%s
  tls.key: |
%s
`, cfg.RegistryNamespace(), registryStorageSecret, cfg.RegistryNamespace(), storage.Username, storage.Password, registryTLSSecret, cfg.RegistryNamespace(), indentBlock(runtime.RegistryTLSCertPEM, 4), indentBlock(runtime.RegistryTLSKeyPEM, 4))

	storageManifest := fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
provisioner: smb.csi.k8s.io
reclaimPolicy: Retain
volumeBindingMode: Immediate
allowVolumeExpansion: true
parameters:
  source: %s
  csi.storage.k8s.io/node-stage-secret-name: %s
  csi.storage.k8s.io/node-stage-secret-namespace: %s
mountOptions:
  - dir_mode=0770
  - file_mode=0660
  - vers=3.0
  - noperm
  - mfsymlinks
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: %s
spec:
  capacity:
    storage: %s
  accessModes:
    - ReadWriteMany
  persistentVolumeReclaimPolicy: Retain
  storageClassName: %s
  csi:
    driver: smb.csi.k8s.io
    volumeHandle: %s
    volumeAttributes:
      source: %s
    nodeStageSecretRef:
      name: %s
      namespace: %s
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes:
    - ReadWriteMany
  resources:
    requests:
      storage: %s
  storageClassName: %s
  volumeName: %s
`, cfg.Storage.StorageClassName, smbSource, registryStorageSecret, cfg.RegistryNamespace(), registryBootstrapPVName, cfg.Storage.BootstrapPVCSize, cfg.Storage.StorageClassName, clusterSlug+"-registry", smbSource, registryStorageSecret, cfg.RegistryNamespace(), registryPVCName, cfg.RegistryNamespace(), cfg.Storage.BootstrapPVCSize, cfg.Storage.StorageClassName, registryBootstrapPVName)

	registryManifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: %s
  template:
    metadata:
      labels:
        app.kubernetes.io/name: %s
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        runAsGroup: 1000
        fsGroup: 1000
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: registry
          image: registry:2.8.3
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
          env:
            - name: REGISTRY_STORAGE_DELETE_ENABLED
              value: "true"
            - name: REGISTRY_STORAGE_FILESYSTEM_ROOTDIRECTORY
              value: %q
            - name: REGISTRY_HTTP_TLS_CERTIFICATE
              value: /certs/tls.crt
            - name: REGISTRY_HTTP_TLS_KEY
              value: /certs/tls.key
          ports:
            - containerPort: %d
              name: registry
          livenessProbe:
            httpGet:
              path: /v2/
              port: registry
              scheme: HTTPS
          readinessProbe:
            httpGet:
              path: /v2/
              port: registry
              scheme: HTTPS
          volumeMounts:
            - name: data
              mountPath: /var/lib/registry
            - name: tls
              mountPath: /certs
              readOnly: true
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: %s
        - name: tls
          secret:
            secretName: %s
---
apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
spec:
  selector:
    app.kubernetes.io/name: %s
  ports:
    - name: registry
      port: %d
      targetPort: registry
`, cfg.RegistryServiceName(), cfg.RegistryNamespace(), cfg.RegistryServiceName(), cfg.RegistryServiceName(), registryRootDirectory, cfg.Storage.RegistryPort, registryPVCName, registryTLSSecret, cfg.RegistryServiceName(), cfg.RegistryNamespace(), cfg.RegistryServiceName(), cfg.Storage.RegistryPort)

	return []inlineManifest{
		{Name: "hcloud-secret", Contents: providerSecretManifest},
		{Name: "registry-bootstrap", Contents: secretManifest},
		{Name: "storagebox-registry", Contents: storageManifest},
		{Name: "registry-deployment", Contents: registryManifest},
	}, nil
}

func injectBootstrapManifests(base []byte, cfg *config.Config, installerImage string) ([]byte, error) {
	var document map[string]any
	if err := yaml.Unmarshal(base, &document); err != nil {
		return nil, fmt.Errorf("parse Talos config: %w", err)
	}

	clusterSection := ensureConfigMap(document, "cluster")
	clusterSection["externalCloudProvider"] = map[string]any{
		"enabled":   true,
		"manifests": []string{hetznerCCMManifestURL()},
	}

	etcdSection := ensureConfigMap(clusterSection, "etcd")
	etcdSection["advertisedSubnets"] = []string{cfg.Hetzner.PrivateNetworkCIDR}

	machineSection := ensureConfigMap(document, "machine")
	kubeletSection := ensureConfigMap(machineSection, "kubelet")
	kubeletSection["nodeIP"] = map[string]any{"validSubnets": []string{cfg.Hetzner.PrivateNetworkCIDR}}
	if installerImage != "" {
		installSection := ensureConfigMap(machineSection, "install")
		installSection["image"] = installerImage
	}

	rendered, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("render Talos bootstrap manifests: %w", err)
	}
	return rendered, nil
}

func renderInlineManifestBundle(manifests []inlineManifest) []byte {
	var builder strings.Builder
	for _, manifest := range manifests {
		contents := strings.TrimSpace(manifest.Contents)
		if contents == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString("\n---\n")
		}
		builder.WriteString(contents)
		builder.WriteByte('\n')
	}
	return []byte(builder.String())
}

func manifestList(manifests []inlineManifest) []map[string]any {
	out := make([]map[string]any, 0, len(manifests))
	for _, manifest := range manifests {
		if strings.TrimSpace(manifest.Name) == "" || strings.TrimSpace(manifest.Contents) == "" {
			continue
		}
		out = append(out, map[string]any{
			"name":     manifest.Name,
			"contents": strings.TrimSpace(manifest.Contents),
		})
	}
	return out
}

func smbDriverManifestURLs(cfg *config.Config) []string {
	version := strings.TrimSpace(cfg.Storage.SMBDriverVersion)
	if version == "" {
		version = config.DefaultSMBDriverVersion
	}
	base := fmt.Sprintf("https://raw.githubusercontent.com/kubernetes-csi/csi-driver-smb/%s/deploy/%s", version, version)
	return []string{
		base + "/rbac-csi-smb.yaml",
		base + "/csi-smb-driver.yaml",
		base + "/csi-smb-controller.yaml",
		base + "/csi-smb-node.yaml",
	}
}

func hetznerCCMManifestURL() string {
	return fmt.Sprintf("https://github.com/hetznercloud/hcloud-cloud-controller-manager/releases/download/%s/ccm-networks.yaml", config.DefaultHetznerCCMVersion)
}

func (a *App) fluxOCIBootstrapManifest(cfg *config.Config) []byte {
	return a.fluxOCIBootstrapManifestWithCA(cfg, "", "")
}

func (a *App) fluxOCIBootstrapManifestWithCA(cfg *config.Config, registryCACertPEM, gitOpsTag string) []byte {
	gitOpsTag = strings.TrimSpace(gitOpsTag)
	if gitOpsTag == "" {
		gitOpsTag = "latest"
	}
	caSecretManifest := ""
	if strings.TrimSpace(registryCACertPEM) != "" {
		caSecretManifest = fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
stringData:
  ca.crt: |
%s
---
`, fluxRegistryCASecret, fluxNamespace, indentBlock(registryCACertPEM, 4))
	}
	return []byte(fmt.Sprintf(`%sapiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: %s
  namespace: %s
spec:
  interval: 1m0s
  url: oci://%s/%s
  ref:
    tag: %s
  certSecretRef:
    name: %s
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: %s
  namespace: %s
spec:
  interval: 5m0s
  prune: true
  wait: true
  path: "./core/external-secrets"
  sourceRef:
    kind: OCIRepository
    name: %s
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: %s
  namespace: %s
spec:
  interval: 5m0s
  prune: true
  wait: true
  path: "./core/cert-manager"
  sourceRef:
    kind: OCIRepository
    name: %s
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: %s
  namespace: %s
spec:
  interval: 5m0s
  prune: true
  wait: true
  path: "./core/cert-manager-issuer"
  dependsOn:
    - name: %s
    - name: %s
  sourceRef:
    kind: OCIRepository
    name: %s
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: %s
  namespace: %s
spec:
  interval: 5m0s
  prune: true
  wait: true
  path: "./core/cluster-secrets"
  dependsOn:
    - name: %s
  sourceRef:
    kind: OCIRepository
    name: %s
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: %s
  namespace: %s
spec:
  interval: 5m0s
  prune: true
  wait: true
  path: "./apps"
  sourceRef:
    kind: OCIRepository
    name: %s
`,
		caSecretManifest,
		fluxOCIRepositoryName,
		fluxNamespace,
		cfg.EffectiveRegistryAddress(),
		gitOpsRepository(cfg),
		gitOpsTag,
		fluxRegistryCASecret,
		fluxKustomizationName,
		fluxNamespace,
		fluxOCIRepositoryName,
		fluxCertManagerKustomizationName,
		fluxNamespace,
		fluxOCIRepositoryName,
		fluxIssuerKustomizationName,
		fluxNamespace,
		fluxKustomizationName,
		fluxCertManagerKustomizationName,
		fluxOCIRepositoryName,
		fluxClusterSecretsKustomizationName,
		fluxNamespace,
		fluxKustomizationName,
		fluxOCIRepositoryName,
		fluxAppsKustomizationName,
		fluxNamespace,
		fluxOCIRepositoryName,
	))
}

func indentBlock(value string, spaces int) string {
	value = strings.TrimRight(value, "\n")
	if value == "" {
		return strings.Repeat(" ", spaces)
	}
	prefix := strings.Repeat(" ", spaces)
	lines := strings.Split(value, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func clusterResourceName(cfg *config.Config, suffix string) string {
	return fmt.Sprintf("stardrive-%s-%s", names.Slugify(cfg.Cluster.Name), names.Slugify(suffix))
}

func clusterResourceLabels(cfg *config.Config) map[string]string {
	return map[string]string{
		resourceManagedByKey: resourceManagedByValue,
		resourceClusterKey:   hetznerLabelValue(names.Slugify(cfg.Cluster.Name)),
	}
}

func nodeResourceLabels(cfg *config.Config, node config.NodeConfig) map[string]string {
	labels := clusterResourceLabels(cfg)
	labels[resourceNodeKey] = hetznerLabelValue(names.Slugify(node.Name))
	return labels
}

func hetznerLabelValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 63 {
		return value
	}
	return names.HashSuffix(value, 32)
}

func gitOpsRepository(cfg *config.Config) string {
	return filepath.ToSlash(filepath.Join(resourceRepositoryPrefix, names.Slugify(cfg.Cluster.Name)))
}

func nodeDNSName(cfg *config.Config, node config.NodeConfig) string {
	if strings.Contains(node.Name, ".") {
		return node.Name
	}
	return node.Name + "." + cfg.DNS.Zone
}

func deadlineOrNow(ctx context.Context) time.Time {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline
	}
	return time.Now()
}
