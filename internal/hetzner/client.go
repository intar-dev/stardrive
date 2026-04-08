package hetzner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/apricote/hcloud-upload-image/hcloudimages"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

const (
	cloudAPIBaseURL      = "https://api.hetzner.cloud/v1"
	storageAPIBaseURL    = "https://api.hetzner.com/v1"
	defaultPollingPeriod = 5 * time.Second
)

type Credentials struct {
	Token string
}

type Client struct {
	token       string
	cloud       *hcloud.Client
	httpClient  *http.Client
	imageUpload *hcloudimages.Client
}

type ServerType struct {
	Name                 string
	Description          string
	Architecture         string
	Cores                int
	MemoryGB             float64
	DiskGB               int
	AvailableAtLocations []string
	RecommendedLocations []string
}

type Location struct {
	ID          int64
	Name        string
	Description string
	NetworkZone string
}

type Server struct {
	ID          int64
	Name        string
	Status      string
	ServerType  string
	Location    string
	PublicIPv4  string
	PublicIPv6  string
	PrivateIPv4 string
	Labels      map[string]string
}

type Network struct {
	ID   int64
	Name string
	CIDR string
}

type PlacementGroup struct {
	ID   int64
	Name string
}

type Firewall struct {
	ID   int64
	Name string
}

type LoadBalancer struct {
	ID         int64
	Name       string
	PublicIPv4 string
}

type LoadBalancerTargetHealth struct {
	ServerID int64
	Status   string
}

type Image struct {
	ID           int64
	Name         string
	Description  string
	Architecture string
	Labels       map[string]string
}

type ServerCreateRequest struct {
	Name           string
	ServerType     string
	Location       string
	ImageID        int64
	UserData       string
	PrivateIPv4    string
	SSHKey         *hcloud.SSHKey
	Network        *hcloud.Network
	PlacementGroup *hcloud.PlacementGroup
	Firewall       *hcloud.Firewall
	Labels         map[string]string
	PublicIPv6     bool
}

type StorageBox struct {
	ID       int64
	Name     string
	Username string
	Location string
	Type     string
	Status   string
}

func NewClient(creds Credentials) (*Client, error) {
	token := strings.TrimSpace(creds.Token)
	if token == "" {
		return nil, fmt.Errorf("Hetzner token is required")
	}

	cloud := hcloud.NewClient(hcloud.WithToken(token))
	return &Client{
		token:       token,
		cloud:       cloud,
		httpClient:  &http.Client{Timeout: 2 * time.Minute},
		imageUpload: hcloudimages.NewClient(cloud),
	}, nil
}

func (c *Client) ListServerTypes(ctx context.Context) ([]ServerType, error) {
	serverTypes, err := c.cloud.ServerType.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list server types: %w", err)
	}

	result := make([]ServerType, 0, len(serverTypes))
	for _, item := range serverTypes {
		if item == nil {
			continue
		}
		available := make([]string, 0, len(item.Locations))
		for _, location := range item.Locations {
			if location.Location == nil {
				continue
			}
			available = append(available, strings.TrimSpace(location.Location.Name))
		}
		slices.Sort(available)
		result = append(result, ServerType{
			Name:                 item.Name,
			Description:          item.Description,
			Architecture:         string(item.Architecture),
			Cores:                item.Cores,
			MemoryGB:             float64(item.Memory),
			DiskGB:               item.Disk,
			AvailableAtLocations: uniqueNonEmpty(available),
			RecommendedLocations: nil,
		})
	}

	slices.SortFunc(result, func(a, b ServerType) int {
		return strings.Compare(a.Name, b.Name)
	})
	return result, nil
}

func (c *Client) ListLocations(ctx context.Context) ([]Location, error) {
	locations, err := c.cloud.Location.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list locations: %w", err)
	}

	result := make([]Location, 0, len(locations))
	for _, item := range locations {
		if item == nil {
			continue
		}
		result = append(result, Location{
			ID:          item.ID,
			Name:        item.Name,
			Description: item.Description,
			NetworkZone: string(item.NetworkZone),
		})
	}

	slices.SortFunc(result, func(a, b Location) int {
		return strings.Compare(a.Name, b.Name)
	})
	return result, nil
}

func (c *Client) ValidateServerTypeAtLocation(ctx context.Context, serverTypeName, locationName string) (*ServerType, *Location, error) {
	serverTypes, err := c.ListServerTypes(ctx)
	if err != nil {
		return nil, nil, err
	}
	locations, err := c.ListLocations(ctx)
	if err != nil {
		return nil, nil, err
	}

	serverTypeName = strings.TrimSpace(serverTypeName)
	locationName = strings.TrimSpace(locationName)

	var serverType *ServerType
	for i := range serverTypes {
		if serverTypes[i].Name == serverTypeName {
			serverType = &serverTypes[i]
			break
		}
	}
	if serverType == nil {
		return nil, nil, fmt.Errorf("server type %q not found", serverTypeName)
	}

	var location *Location
	for i := range locations {
		if locations[i].Name == locationName {
			location = &locations[i]
			break
		}
	}
	if location == nil {
		return nil, nil, fmt.Errorf("location %q not found", locationName)
	}

	if !slices.Contains(serverType.AvailableAtLocations, location.Name) {
		return nil, nil, fmt.Errorf("server type %s is not available at location %s", serverType.Name, location.Name)
	}

	return serverType, location, nil
}

func (c *Client) EnsureSSHKey(ctx context.Context, name, publicKey string) (*hcloud.SSHKey, error) {
	key, _, err := c.cloud.SSHKey.GetByName(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, fmt.Errorf("lookup ssh key %s: %w", name, err)
	}
	if key != nil {
		return key, nil
	}

	created, _, err := c.cloud.SSHKey.Create(ctx, hcloud.SSHKeyCreateOpts{
		Name:      strings.TrimSpace(name),
		PublicKey: strings.TrimSpace(publicKey),
	})
	if err != nil {
		return nil, fmt.Errorf("create ssh key %s: %w", name, err)
	}
	return created, nil
}

func (c *Client) EnsureNetwork(ctx context.Context, name, cidr string) (*hcloud.Network, error) {
	network, _, err := c.cloud.Network.GetByName(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, fmt.Errorf("lookup network %s: %w", name, err)
	}
	if network != nil {
		return network, nil
	}

	ipRange, err := parseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse network CIDR %q: %w", cidr, err)
	}
	created, _, err := c.cloud.Network.Create(ctx, hcloud.NetworkCreateOpts{
		Name:    strings.TrimSpace(name),
		IPRange: ipRange,
	})
	if err != nil {
		return nil, fmt.Errorf("create network %s: %w", name, err)
	}
	return created, nil
}

func (c *Client) EnsureSubnet(ctx context.Context, network *hcloud.Network, networkZone, cidr string) error {
	if network == nil {
		return fmt.Errorf("network is required")
	}
	want, err := parseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("parse subnet CIDR %q: %w", cidr, err)
	}

	for _, subnet := range network.Subnets {
		if subnet.IPRange != nil && subnet.IPRange.String() == want.String() {
			return nil
		}
	}

	action, _, err := c.cloud.Network.AddSubnet(ctx, network, hcloud.NetworkAddSubnetOpts{
		Subnet: hcloud.NetworkSubnet{
			Type:        hcloud.NetworkSubnetTypeCloud,
			IPRange:     want,
			NetworkZone: hcloud.NetworkZone(strings.TrimSpace(networkZone)),
		},
	})
	if err != nil {
		return fmt.Errorf("add subnet to network %s: %w", network.Name, err)
	}
	return c.waitForAction(ctx, action)
}

func (c *Client) EnsurePlacementGroup(ctx context.Context, name string) (*hcloud.PlacementGroup, error) {
	group, _, err := c.cloud.PlacementGroup.GetByName(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, fmt.Errorf("lookup placement group %s: %w", name, err)
	}
	if group != nil {
		return group, nil
	}

	result, _, err := c.cloud.PlacementGroup.Create(ctx, hcloud.PlacementGroupCreateOpts{
		Name: strings.TrimSpace(name),
		Type: hcloud.PlacementGroupTypeSpread,
	})
	if err != nil {
		return nil, fmt.Errorf("create placement group %s: %w", name, err)
	}
	if err := c.waitForAction(ctx, result.Action); err != nil {
		return nil, err
	}
	return result.PlacementGroup, nil
}

func (c *Client) EnsureFirewall(ctx context.Context, name string, sshCIDRs []string) (*hcloud.Firewall, error) {
	firewall, _, err := c.cloud.Firewall.GetByName(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, fmt.Errorf("lookup firewall %s: %w", name, err)
	}
	if firewall != nil {
		return firewall, nil
	}

	talosPort := "50000"
	apiPort := "6443"
	rules := []hcloud.FirewallRule{
		{
			Direction: hcloud.FirewallRuleDirectionIn,
			Protocol:  hcloud.FirewallRuleProtocolTCP,
			Port:      &talosPort,
			SourceIPs: []net.IPNet{mustIPNet("0.0.0.0/0")},
		},
		{
			Direction: hcloud.FirewallRuleDirectionIn,
			Protocol:  hcloud.FirewallRuleProtocolTCP,
			Port:      &apiPort,
			SourceIPs: []net.IPNet{mustIPNet("0.0.0.0/0")},
		},
	}
	if len(sshCIDRs) > 0 {
		sshPort := "22"
		sourceIPs := make([]net.IPNet, 0, len(sshCIDRs))
		for _, cidr := range sshCIDRs {
			sourceIPs = append(sourceIPs, mustIPNet(cidr))
		}
		rules = append(rules, hcloud.FirewallRule{
			Direction: hcloud.FirewallRuleDirectionIn,
			Protocol:  hcloud.FirewallRuleProtocolTCP,
			Port:      &sshPort,
			SourceIPs: sourceIPs,
		})
	}

	result, _, err := c.cloud.Firewall.Create(ctx, hcloud.FirewallCreateOpts{
		Name:  strings.TrimSpace(name),
		Rules: rules,
	})
	if err != nil {
		return nil, fmt.Errorf("create firewall %s: %w", name, err)
	}
	for _, action := range result.Actions {
		if err := c.waitForAction(ctx, action); err != nil {
			return nil, err
		}
	}
	return result.Firewall, nil
}

func (c *Client) EnsureLoadBalancer(ctx context.Context, name, loadBalancerType, location string, network *hcloud.Network) (*hcloud.LoadBalancer, error) {
	loadBalancer, _, err := c.cloud.LoadBalancer.GetByName(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, fmt.Errorf("lookup load balancer %s: %w", name, err)
	}
	if loadBalancer != nil {
		return loadBalancer, nil
	}

	listenPort := 6443
	destPort := 6443
	publicInterface := true
	result, _, err := c.cloud.LoadBalancer.Create(ctx, hcloud.LoadBalancerCreateOpts{
		Name:             strings.TrimSpace(name),
		LoadBalancerType: &hcloud.LoadBalancerType{Name: strings.TrimSpace(loadBalancerType)},
		Location:         &hcloud.Location{Name: strings.TrimSpace(location)},
		PublicInterface:  &publicInterface,
		Network:          network,
		Services: []hcloud.LoadBalancerCreateOptsService{
			{
				Protocol:        hcloud.LoadBalancerServiceProtocolTCP,
				ListenPort:      &listenPort,
				DestinationPort: &destPort,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create load balancer %s: %w", name, err)
	}
	if err := c.waitForAction(ctx, result.Action); err != nil {
		return nil, err
	}
	return result.LoadBalancer, nil
}

func (c *Client) AddLoadBalancerTargets(ctx context.Context, loadBalancer *hcloud.LoadBalancer, servers []*hcloud.Server) error {
	if loadBalancer == nil {
		return fmt.Errorf("load balancer is required")
	}
	if len(servers) == 0 {
		return nil
	}

	existing := map[int64]struct{}{}
	for _, target := range loadBalancer.Targets {
		if target.Server == nil || target.Server.Server == nil {
			continue
		}
		existing[target.Server.Server.ID] = struct{}{}
	}

	usePrivateIP := true
	for _, server := range servers {
		if server == nil {
			continue
		}
		if _, ok := existing[server.ID]; ok {
			continue
		}
		action, _, err := c.cloud.LoadBalancer.AddServerTarget(ctx, loadBalancer, hcloud.LoadBalancerAddServerTargetOpts{
			Server:       server,
			UsePrivateIP: &usePrivateIP,
		})
		if err != nil {
			return fmt.Errorf("add load balancer target %s -> %s: %w", loadBalancer.Name, server.Name, err)
		}
		if err := c.waitForAction(ctx, action); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) SyncLoadBalancerTargetsByID(ctx context.Context, loadBalancerID int64, serverIDs []int64) error {
	if loadBalancerID == 0 {
		return fmt.Errorf("load balancer id is required")
	}

	loadBalancer, _, err := c.cloud.LoadBalancer.GetByID(ctx, loadBalancerID)
	if err != nil {
		return fmt.Errorf("lookup load balancer %d: %w", loadBalancerID, err)
	}
	if loadBalancer == nil {
		return fmt.Errorf("load balancer %d not found", loadBalancerID)
	}

	desired := map[int64]struct{}{}
	for _, serverID := range serverIDs {
		if serverID > 0 {
			desired[serverID] = struct{}{}
		}
	}

	for _, target := range loadBalancer.Targets {
		if target.LabelSelector == nil || strings.TrimSpace(target.LabelSelector.Selector) == "" {
			continue
		}
		action, _, err := c.cloud.LoadBalancer.RemoveLabelSelectorTarget(ctx, loadBalancer, target.LabelSelector.Selector)
		if err != nil {
			return fmt.Errorf("remove load balancer label target %s -> %s: %w", loadBalancer.Name, target.LabelSelector.Selector, err)
		}
		if err := c.waitForAction(ctx, action); err != nil {
			return err
		}
	}

	existing := map[int64]struct{}{}
	for _, target := range loadBalancer.Targets {
		if target.Server == nil || target.Server.Server == nil {
			continue
		}
		existing[target.Server.Server.ID] = struct{}{}
	}

	for serverID := range existing {
		if _, ok := desired[serverID]; ok {
			continue
		}
		action, _, err := c.cloud.LoadBalancer.RemoveServerTarget(ctx, loadBalancer, &hcloud.Server{ID: serverID})
		if err != nil {
			return fmt.Errorf("remove load balancer target %s -> %d: %w", loadBalancer.Name, serverID, err)
		}
		if err := c.waitForAction(ctx, action); err != nil {
			return err
		}
	}

	usePrivateIP := true
	for serverID := range desired {
		if _, ok := existing[serverID]; ok {
			continue
		}
		server, _, err := c.cloud.Server.GetByID(ctx, serverID)
		if err != nil {
			return fmt.Errorf("lookup server %d for load balancer target: %w", serverID, err)
		}
		if server == nil {
			return fmt.Errorf("server %d not found for load balancer target", serverID)
		}
		action, _, err := c.cloud.LoadBalancer.AddServerTarget(ctx, loadBalancer, hcloud.LoadBalancerAddServerTargetOpts{
			Server:       server,
			UsePrivateIP: &usePrivateIP,
		})
		if err != nil {
			return fmt.Errorf("add load balancer target %s -> %s: %w", loadBalancer.Name, server.Name, err)
		}
		if err := c.waitForAction(ctx, action); err != nil {
			return err
		}
	}

	return nil
}

func (c *Client) EnsureLoadBalancerLabelTarget(ctx context.Context, loadBalancer *hcloud.LoadBalancer, selector string) error {
	if loadBalancer == nil {
		return fmt.Errorf("load balancer is required")
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return fmt.Errorf("label selector is required")
	}
	for _, target := range loadBalancer.Targets {
		if target.LabelSelector != nil && target.LabelSelector.Selector == selector {
			return nil
		}
	}
	usePrivateIP := true
	action, _, err := c.cloud.LoadBalancer.AddLabelSelectorTarget(ctx, loadBalancer, hcloud.LoadBalancerAddLabelSelectorTargetOpts{
		Selector:     selector,
		UsePrivateIP: &usePrivateIP,
	})
	if err != nil {
		return fmt.Errorf("add load balancer label target %s -> %s: %w", loadBalancer.Name, selector, err)
	}
	return c.waitForAction(ctx, action)
}

func (c *Client) CreateServer(ctx context.Context, req ServerCreateRequest) (*Server, error) {
	startAfterCreate := true
	opts := hcloud.ServerCreateOpts{
		Name:             strings.TrimSpace(req.Name),
		ServerType:       &hcloud.ServerType{Name: strings.TrimSpace(req.ServerType)},
		Image:            &hcloud.Image{ID: req.ImageID},
		Location:         &hcloud.Location{Name: strings.TrimSpace(req.Location)},
		UserData:         req.UserData,
		StartAfterCreate: &startAfterCreate,
		Labels:           req.Labels,
		PublicNet: &hcloud.ServerCreatePublicNet{
			EnableIPv4: true,
			EnableIPv6: req.PublicIPv6,
		},
	}
	if req.SSHKey != nil {
		opts.SSHKeys = []*hcloud.SSHKey{req.SSHKey}
	}
	if req.PlacementGroup != nil {
		opts.PlacementGroup = req.PlacementGroup
	}
	if req.Firewall != nil {
		opts.Firewalls = []*hcloud.ServerCreateFirewall{{Firewall: *req.Firewall}}
	}
	if req.Network != nil && strings.TrimSpace(req.PrivateIPv4) == "" {
		opts.Networks = []*hcloud.Network{req.Network}
	}

	result, _, err := c.cloud.Server.Create(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("create server %s: %w", req.Name, err)
	}
	if err := c.waitForAction(ctx, result.Action); err != nil {
		return nil, err
	}
	for _, action := range result.NextActions {
		if err := c.waitForAction(ctx, action); err != nil {
			return nil, err
		}
	}

	server := result.Server
	if server == nil {
		return nil, fmt.Errorf("create server %s returned no server", req.Name)
	}
	if req.Network != nil && strings.TrimSpace(req.PrivateIPv4) != "" {
		action, _, err := c.cloud.Server.AttachToNetwork(ctx, server, hcloud.ServerAttachToNetworkOpts{
			Network: req.Network,
			IP:      net.ParseIP(strings.TrimSpace(req.PrivateIPv4)),
		})
		if err != nil {
			return nil, fmt.Errorf("attach server %s to private network: %w", req.Name, err)
		}
		if err := c.waitForAction(ctx, action); err != nil {
			return nil, err
		}
	}

	updated, _, err := c.cloud.Server.GetByID(ctx, server.ID)
	if err != nil {
		return nil, fmt.Errorf("refresh server %s: %w", req.Name, err)
	}
	return fromHCloudServer(updated), nil
}

func (c *Client) GetServerByID(ctx context.Context, id int64) (*Server, error) {
	server, _, err := c.cloud.Server.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("lookup server %d: %w", id, err)
	}
	if server == nil {
		return nil, fmt.Errorf("server %d not found", id)
	}
	return fromHCloudServer(server), nil
}

func (c *Client) ListServers(ctx context.Context, labels map[string]string) ([]Server, error) {
	servers, err := c.cloud.Server.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}

	result := make([]Server, 0, len(servers))
	for _, server := range servers {
		if server == nil {
			continue
		}
		if !matchesLabels(server.Labels, labels) {
			continue
		}
		result = append(result, *fromHCloudServer(server))
	}
	slices.SortFunc(result, func(a, b Server) int {
		return strings.Compare(a.Name, b.Name)
	})
	return result, nil
}

func (c *Client) ListImages(ctx context.Context, labels map[string]string) ([]Image, error) {
	opts := hcloud.ImageListOpts{}
	opts.Type = []hcloud.ImageType{hcloud.ImageTypeSnapshot}
	images, err := c.cloud.Image.AllWithOpts(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}

	result := make([]Image, 0, len(images))
	for _, image := range images {
		if image == nil {
			continue
		}
		if !matchesLabels(image.Labels, labels) {
			continue
		}
		result = append(result, Image{
			ID:           image.ID,
			Name:         image.Name,
			Description:  image.Description,
			Architecture: string(image.Architecture),
			Labels:       cloneLabels(image.Labels),
		})
	}
	slices.SortFunc(result, func(a, b Image) int {
		return strings.Compare(a.Description, b.Description)
	})
	return result, nil
}

func (c *Client) UploadImageFromReader(ctx context.Context, reader io.Reader, imageName, description string, labels map[string]string, compression hcloudimages.Compression, architecture, location string) (*Image, error) {
	var arch hcloud.Architecture
	switch strings.ToLower(strings.TrimSpace(architecture)) {
	case "", "amd64", "x86", "x86_64":
		arch = hcloud.ArchitectureX86
	case "arm64", "aarch64":
		arch = hcloud.ArchitectureARM
	default:
		return nil, fmt.Errorf("unsupported architecture %q", architecture)
	}

	description = strings.TrimSpace(description)
	uploadLabels := map[string]string{}
	for key, value := range labels {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "" && value != "" {
			uploadLabels[key] = value
		}
	}
	if imageName = strings.TrimSpace(imageName); imageName != "" {
		uploadLabels["stardrive.dev/image-name"] = imageName
	}

	opts := hcloudimages.UploadOptions{
		ImageReader:      reader,
		ImageCompression: compression,
		ImageFormat:      hcloudimages.FormatRaw,
		Architecture:     arch,
		Labels:           uploadLabels,
	}
	if description != "" {
		opts.Description = &description
	}
	if strings.TrimSpace(location) != "" {
		opts.Location = &hcloud.Location{Name: strings.TrimSpace(location)}
	}

	image, err := c.imageUpload.Upload(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("upload image %s: %w", imageName, err)
	}
	return &Image{
		ID:           image.ID,
		Name:         image.Name,
		Description:  image.Description,
		Architecture: string(image.Architecture),
	}, nil
}

func (c *Client) WaitForImageAvailable(ctx context.Context, id int64, timeout time.Duration) (*Image, error) {
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		image, _, err := c.cloud.Image.GetByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("lookup image %d: %w", id, err)
		}
		if image == nil {
			return nil, fmt.Errorf("image %d not found", id)
		}
		if image.Status == hcloud.ImageStatusAvailable {
			return &Image{
				ID:           image.ID,
				Name:         image.Name,
				Description:  image.Description,
				Architecture: string(image.Architecture),
			}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(defaultPollingPeriod):
		}
	}
}

func (c *Client) WaitForServerPublicIPv4(ctx context.Context, id int64, timeout time.Duration) (*Server, error) {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		server, _, err := c.cloud.Server.GetByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("lookup server %d: %w", id, err)
		}
		if server == nil {
			return nil, fmt.Errorf("server %d not found", id)
		}
		converted := fromHCloudServer(server)
		if converted.PublicIPv4 != "" {
			return converted, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(defaultPollingPeriod):
		}
	}
}

func (c *Client) GetNetworkByName(ctx context.Context, name string) (*Network, error) {
	network, _, err := c.cloud.Network.GetByName(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, fmt.Errorf("lookup network %s: %w", name, err)
	}
	if network == nil {
		return nil, nil
	}
	cidr := ""
	if network.IPRange != nil {
		cidr = network.IPRange.String()
	}
	return &Network{
		ID:   network.ID,
		Name: network.Name,
		CIDR: cidr,
	}, nil
}

func (c *Client) GetPlacementGroupByName(ctx context.Context, name string) (*PlacementGroup, error) {
	group, _, err := c.cloud.PlacementGroup.GetByName(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, fmt.Errorf("lookup placement group %s: %w", name, err)
	}
	if group == nil {
		return nil, nil
	}
	return &PlacementGroup{ID: group.ID, Name: group.Name}, nil
}

func (c *Client) GetFirewallByName(ctx context.Context, name string) (*Firewall, error) {
	firewall, _, err := c.cloud.Firewall.GetByName(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, fmt.Errorf("lookup firewall %s: %w", name, err)
	}
	if firewall == nil {
		return nil, nil
	}
	return &Firewall{ID: firewall.ID, Name: firewall.Name}, nil
}

func (c *Client) GetLoadBalancerByName(ctx context.Context, name string) (*LoadBalancer, error) {
	loadBalancer, _, err := c.cloud.LoadBalancer.GetByName(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, fmt.Errorf("lookup load balancer %s: %w", name, err)
	}
	if loadBalancer == nil {
		return nil, nil
	}
	ipv4 := ""
	if !loadBalancer.PublicNet.IPv4.IP.IsUnspecified() {
		ipv4 = loadBalancer.PublicNet.IPv4.IP.String()
	}
	return &LoadBalancer{
		ID:         loadBalancer.ID,
		Name:       loadBalancer.Name,
		PublicIPv4: ipv4,
	}, nil
}

func (c *Client) LoadBalancerTargetsHealthy(ctx context.Context, loadBalancerID int64, serverIDs []int64, listenPort int) (bool, string, error) {
	if loadBalancerID == 0 {
		return false, "", fmt.Errorf("load balancer id is required")
	}
	loadBalancer, _, err := c.cloud.LoadBalancer.GetByID(ctx, loadBalancerID)
	if err != nil {
		return false, "", fmt.Errorf("lookup load balancer %d: %w", loadBalancerID, err)
	}
	if loadBalancer == nil {
		return false, "", fmt.Errorf("load balancer %d not found", loadBalancerID)
	}
	ready, summary := evaluateLoadBalancerTargets(loadBalancer.Targets, serverIDs, listenPort)
	return ready, summary, nil
}

func evaluateLoadBalancerTargets(targets []hcloud.LoadBalancerTarget, serverIDs []int64, listenPort int) (bool, string) {
	desired := make([]int64, 0, len(serverIDs))
	seenDesired := map[int64]struct{}{}
	for _, serverID := range serverIDs {
		if serverID <= 0 {
			continue
		}
		if _, ok := seenDesired[serverID]; ok {
			continue
		}
		seenDesired[serverID] = struct{}{}
		desired = append(desired, serverID)
	}
	slices.Sort(desired)

	byServerID := map[int64]hcloud.LoadBalancerTarget{}
	for _, target := range targets {
		if target.Server == nil || target.Server.Server == nil {
			continue
		}
		byServerID[target.Server.Server.ID] = target
	}

	if len(desired) == 0 {
		return true, "no targets required"
	}

	attached := 0
	healthy := 0
	statuses := make([]string, 0, len(desired))
	for _, serverID := range desired {
		target, ok := byServerID[serverID]
		if !ok {
			statuses = append(statuses, fmt.Sprintf("%d:missing", serverID))
			continue
		}
		attached++
		status := "unknown"
		for _, health := range target.HealthStatus {
			if health.ListenPort == listenPort {
				status = string(health.Status)
				break
			}
		}
		if status == string(hcloud.LoadBalancerTargetHealthStatusStatusHealthy) {
			healthy++
		}
		statuses = append(statuses, fmt.Sprintf("%d:%s", serverID, status))
	}

	ready := attached == len(desired) && healthy == len(desired)
	return ready, fmt.Sprintf("attached=%d/%d healthy=%d/%d [%s]", attached, len(desired), healthy, len(desired), strings.Join(statuses, ", "))
}

func (c *Client) DeleteServer(ctx context.Context, id int64) error {
	result, _, err := c.cloud.Server.DeleteWithResult(ctx, &hcloud.Server{ID: id})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete server %d: %w", id, err)
	}
	if result == nil || result.Action == nil {
		return nil
	}
	return c.waitForAction(ctx, result.Action)
}

func (c *Client) DeleteLoadBalancer(ctx context.Context, id int64) error {
	_, err := c.cloud.LoadBalancer.Delete(ctx, &hcloud.LoadBalancer{ID: id})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete load balancer %d: %w", id, err)
	}
	return nil
}

func (c *Client) DeleteNetwork(ctx context.Context, id int64) error {
	_, err := c.cloud.Network.Delete(ctx, &hcloud.Network{ID: id})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete network %d: %w", id, err)
	}
	return nil
}

func (c *Client) DeletePlacementGroup(ctx context.Context, id int64) error {
	_, err := c.cloud.PlacementGroup.Delete(ctx, &hcloud.PlacementGroup{ID: id})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete placement group %d: %w", id, err)
	}
	return nil
}

func (c *Client) DeleteFirewall(ctx context.Context, id int64) error {
	_, err := c.cloud.Firewall.Delete(ctx, &hcloud.Firewall{ID: id})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete firewall %d: %w", id, err)
	}
	return nil
}

func (c *Client) DeleteImage(ctx context.Context, id int64) error {
	_, err := c.cloud.Image.Delete(ctx, &hcloud.Image{ID: id})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete image %d: %w", id, err)
	}
	return nil
}

func (c *Client) ListStorageBoxes(ctx context.Context) ([]StorageBox, error) {
	resp, err := c.doStorageRequest(ctx, http.MethodGet, "/storage_boxes", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var payload struct {
		StorageBoxes []map[string]any `json:"storage_boxes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode storage boxes: %w", err)
	}

	result := make([]StorageBox, 0, len(payload.StorageBoxes))
	for _, item := range payload.StorageBoxes {
		result = append(result, parseStorageBox(item))
	}
	slices.SortFunc(result, func(a, b StorageBox) int {
		return strings.Compare(a.Name, b.Name)
	})
	return result, nil
}

func (c *Client) CreateStorageBox(ctx context.Context, name, plan, location, password string) (*StorageBox, error) {
	plan = strings.TrimSpace(plan)
	password = strings.TrimSpace(password)
	if plan == "" {
		return nil, fmt.Errorf("storage box plan is required")
	}
	if password == "" {
		return nil, fmt.Errorf("storage box password is required")
	}
	body := map[string]any{
		"name":             strings.TrimSpace(name),
		"storage_box_type": plan,
		"location":         strings.TrimSpace(location),
		"password":         password,
	}
	resp, err := c.doStorageRequest(ctx, http.MethodPost, "/storage_boxes", body, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var payload struct {
		StorageBox map[string]any `json:"storage_box"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode created storage box: %w", err)
	}
	box := parseStorageBox(payload.StorageBox)
	return &box, nil
}

func (c *Client) WaitForStorageBoxReady(ctx context.Context, id int64, timeout time.Duration) (*StorageBox, error) {
	if id == 0 {
		return nil, fmt.Errorf("storage box id is required")
	}

	deadline := time.Now().Add(timeout)
	for {
		boxes, err := c.ListStorageBoxes(ctx)
		if err != nil {
			return nil, err
		}
		for _, box := range boxes {
			if box.ID != id {
				continue
			}
			if strings.TrimSpace(box.Username) == "" {
				break
			}
			if strings.EqualFold(strings.TrimSpace(box.Status), "initializing") {
				break
			}
			return &box, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("wait for storage box %d to become ready: timeout after %s", id, timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(defaultPollingPeriod):
		}
	}
}

func (c *Client) ResetStorageBoxPassword(ctx context.Context, id int64, password string) error {
	if id == 0 {
		return fmt.Errorf("storage box id is required")
	}
	password = strings.TrimSpace(password)
	if password == "" {
		return fmt.Errorf("storage box password is required")
	}
	resp, err := c.doStorageRequest(ctx, http.MethodPost, "/storage_boxes/"+strconv.FormatInt(id, 10)+"/actions/reset_password", map[string]any{
		"password": password,
	}, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (c *Client) UpdateStorageBoxAccessSettings(ctx context.Context, id int64, sambaEnabled bool) error {
	if id == 0 {
		return fmt.Errorf("storage box id is required")
	}
	resp, err := c.doStorageRequest(ctx, http.MethodPost, "/storage_boxes/"+strconv.FormatInt(id, 10)+"/actions/update_access_settings", map[string]any{
		"samba_enabled": sambaEnabled,
	}, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (c *Client) DeleteStorageBox(ctx context.Context, id int64) error {
	resp, err := c.doStorageRequest(ctx, http.MethodDelete, "/storage_boxes/"+strconv.FormatInt(id, 10), nil, nil)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (c *Client) EnsureWebDAVDirectory(ctx context.Context, username, password, dir string) error {
	dir = strings.Trim(strings.TrimSpace(dir), "/")
	if dir == "" {
		return nil
	}

	baseURL := fmt.Sprintf("https://%s.your-storagebox.de", strings.TrimSpace(username))
	current := baseURL
	for _, segment := range strings.Split(dir, "/") {
		current += "/" + url.PathEscape(segment)
		req, err := http.NewRequestWithContext(ctx, "MKCOL", current, nil)
		if err != nil {
			return err
		}
		req.SetBasicAuth(username, password)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("create WebDAV directory %s: %w", current, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusConflict {
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("create WebDAV directory %s returned %s", current, resp.Status)
		}
	}
	return nil
}

func (c *Client) UploadWebDAVFile(ctx context.Context, username, password, remotePath string, body io.Reader) (string, error) {
	remotePath = "/" + strings.Trim(strings.TrimSpace(remotePath), "/")
	baseURL := fmt.Sprintf("https://%s.your-storagebox.de%s", strings.TrimSpace(username), remotePath)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, baseURL, body)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(username, password)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload WebDAV file %s: %w", remotePath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("upload WebDAV file %s returned %s: %s", remotePath, resp.Status, strings.TrimSpace(string(out)))
	}
	u := &url.URL{
		Scheme: "https",
		User:   url.UserPassword(username, password),
		Host:   fmt.Sprintf("%s.your-storagebox.de", username),
		Path:   remotePath,
	}
	return u.String(), nil
}

func (c *Client) StorageBoxSMBSource(username string) string {
	return fmt.Sprintf("//%s.your-storagebox.de/backup", strings.TrimSpace(username))
}

func (c *Client) waitForAction(ctx context.Context, action *hcloud.Action) error {
	if action == nil {
		return nil
	}
	for {
		refreshed, _, err := c.cloud.Action.GetByID(ctx, action.ID)
		if err != nil {
			return fmt.Errorf("lookup action %d: %w", action.ID, err)
		}
		if refreshed == nil {
			return fmt.Errorf("action %d not found", action.ID)
		}
		switch refreshed.Status {
		case hcloud.ActionStatusSuccess:
			return nil
		case hcloud.ActionStatusError:
			return fmt.Errorf("action %d failed: %s", action.ID, refreshed.ErrorMessage)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(defaultPollingPeriod):
		}
	}
}

func (c *Client) doStorageRequest(ctx context.Context, method, requestPath string, body any, query url.Values) (*http.Response, error) {
	fullURL := storageAPIBaseURL + requestPath
	if len(query) > 0 {
		fullURL += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode storage box request: %w", err)
		}
		reader = strings.NewReader(string(data))
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("storage box API %s %s: %w", method, requestPath, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: storage box API %s %s returned %s: %s", os.ErrNotExist, method, requestPath, resp.Status, strings.TrimSpace(string(payload)))
	}
	return nil, fmt.Errorf("storage box API %s %s returned %s: %s", method, requestPath, resp.Status, strings.TrimSpace(string(payload)))
}

func parseStorageBox(item map[string]any) StorageBox {
	box := StorageBox{
		ID:       readInt64(item["id"]),
		Name:     readString(item["name"]),
		Username: readString(item["username"]),
		Type:     readString(item["type"]),
		Status:   readString(item["status"]),
	}
	if box.Type == "" {
		box.Type = readString(item["storage_box_type"])
	}
	if location, ok := item["location"].(map[string]any); ok {
		box.Location = readString(location["name"])
	}
	if box.Location == "" {
		box.Location = readString(item["location"])
	}
	if box.Name == "" {
		box.Name = box.Username
	}
	return box
}

func fromHCloudServer(server *hcloud.Server) *Server {
	if server == nil {
		return nil
	}
	out := &Server{
		ID:     server.ID,
		Name:   server.Name,
		Status: string(server.Status),
		Labels: cloneLabels(server.Labels),
	}
	if server.ServerType != nil {
		out.ServerType = server.ServerType.Name
	}
	if server.Location != nil {
		out.Location = server.Location.Name
	} else if server.Datacenter != nil && server.Datacenter.Location != nil {
		out.Location = server.Datacenter.Location.Name
	}
	if !server.PublicNet.IPv4.IsUnspecified() {
		out.PublicIPv4 = server.PublicNet.IPv4.IP.String()
	}
	if !server.PublicNet.IPv6.IsUnspecified() {
		out.PublicIPv6 = server.PublicNet.IPv6.IP.String()
	}
	for _, privateNet := range server.PrivateNet {
		if privateNet.IP == nil {
			continue
		}
		out.PrivateIPv4 = privateNet.IP.String()
		break
	}
	return out
}

func cloneLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}

func matchesLabels(actual, expected map[string]string) bool {
	if len(expected) == 0 {
		return true
	}
	for key, want := range expected {
		if strings.TrimSpace(actual[key]) != strings.TrimSpace(want) {
			return false
		}
	}
	return true
}

func buildLabelSelector(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	selectors := make([]string, 0, len(labels))
	for key, value := range labels {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		selectors = append(selectors, key+"="+value)
	}
	slices.Sort(selectors)
	return strings.Join(selectors, ",")
}

func uniqueNonEmpty(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func parseCIDR(value string) (*net.IPNet, error) {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
	if err != nil {
		return nil, err
	}
	return &net.IPNet{
		IP:   net.IP(prefix.Addr().AsSlice()),
		Mask: net.CIDRMask(prefix.Bits(), prefix.Addr().BitLen()),
	}, nil
}

func readString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	default:
		return ""
	}
}

func readInt64(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	case json.Number:
		v, _ := typed.Int64()
		return v
	default:
		return 0
	}
}

func mustIPNet(cidr string) net.IPNet {
	prefix := netip.MustParsePrefix(strings.TrimSpace(cidr))
	return net.IPNet{
		IP:   net.IP(prefix.Addr().AsSlice()),
		Mask: net.CIDRMask(prefix.Bits(), prefix.Addr().BitLen()),
	}
}

func isNotFound(err error) bool {
	var apiErr hcloud.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code == hcloud.ErrorCodeNotFound
	}
	return false
}

func JoinStorageBoxPath(parts ...string) string {
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.Trim(strings.TrimSpace(part), "/")
		if part != "" {
			clean = append(clean, part)
		}
	}
	return path.Join(clean...)
}
