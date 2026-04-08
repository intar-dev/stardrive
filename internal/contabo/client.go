package contabo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	defaultAPIBase  = "https://api.contabo.com/v1"
	defaultAuthBase = "https://auth.contabo.com/auth/realms/contabo/protocol/openid-connect/token"
)

type Credentials struct {
	ClientID     string
	ClientSecret string
	APIUser      string
	APIPassword  string
}

type Client struct {
	credentials Credentials
	apiBaseURL  string
	authURL     string
	httpClient  *http.Client

	token       string
	tokenExpiry time.Time
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

type listResponse[T any] struct {
	Data []T `json:"data"`
}

type InstanceIP struct {
	IP          string `json:"ip"`
	NetmaskCIDR int    `json:"netmaskCidr"`
	Gateway     string `json:"gateway"`
}

type InstanceIPConfig struct {
	V4 *InstanceIP `json:"v4"`
}

type Instance struct {
	TenantID    string           `json:"tenantId"`
	CustomerID  string           `json:"customerId"`
	InstanceID  int64            `json:"instanceId"`
	Name        string           `json:"name"`
	DisplayName string           `json:"displayName"`
	Status      string           `json:"status"`
	Region      string           `json:"region"`
	IPConfig    InstanceIPConfig `json:"ipConfig"`
	IPAddress   string           `json:"ipAddress"`
	PublicIPv4  string           `json:"publicIpv4"`
	ImageID     string           `json:"imageId"`
}

func (i Instance) PrimaryIPv4() string {
	if i.IPConfig.V4 != nil && strings.TrimSpace(i.IPConfig.V4.IP) != "" {
		return strings.TrimSpace(i.IPConfig.V4.IP)
	}
	if strings.TrimSpace(i.PublicIPv4) != "" {
		return strings.TrimSpace(i.PublicIPv4)
	}
	return strings.TrimSpace(i.IPAddress)
}

func (i Instance) PrimaryIPv4AddressCIDR() string {
	if i.IPConfig.V4 == nil {
		return ""
	}
	ip := strings.TrimSpace(i.IPConfig.V4.IP)
	if ip == "" || i.IPConfig.V4.NetmaskCIDR <= 0 {
		return ""
	}
	return fmt.Sprintf("%s/%d", ip, i.IPConfig.V4.NetmaskCIDR)
}

func (i Instance) PrimaryIPv4Gateway() string {
	if i.IPConfig.V4 == nil {
		return ""
	}
	return strings.TrimSpace(i.IPConfig.V4.Gateway)
}

type Image struct {
	TenantID    string `json:"tenantId"`
	CustomerID  string `json:"customerId"`
	ImageID     string `json:"imageId"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Type        string `json:"type"`
	OSType      string `json:"osType"`
	URL         string `json:"url"`
	Status      string `json:"status"`
}

type ObjectStorage struct {
	TenantID            string    `json:"tenantId"`
	CustomerID          string    `json:"customerId"`
	ObjectStorageID     string    `json:"objectStorageId"`
	CreatedDate         time.Time `json:"createdDate"`
	DisplayName         string    `json:"displayName"`
	DataCenter          string    `json:"dataCenter"`
	Region              string    `json:"region"`
	Endpoint            string    `json:"s3Url"`
	LegacyEndpoint      string    `json:"s3ServerUrl,omitempty"`
	TotalPurchasedSpace float64   `json:"totalPurchasedSpaceTB"`
	Status              string    `json:"status"`
}

type User struct {
	TenantID   string `json:"tenantId"`
	CustomerID string `json:"customerId"`
	UserID     string `json:"userId"`
	Email      string `json:"email"`
	FirstName  string `json:"firstName"`
	LastName   string `json:"lastName"`
	Enabled    bool   `json:"enabled"`
	Owner      bool   `json:"owner"`
}

type UserClient struct {
	TenantID   string `json:"tenantId"`
	CustomerID string `json:"customerId"`
	ID         string `json:"id"`
	ClientID   string `json:"clientId"`
	Secret     string `json:"secret"`
}

type ObjectStorageCredential struct {
	TenantID        string `json:"tenantId"`
	CustomerID      string `json:"customerId"`
	AccessKey       string `json:"accessKey"`
	SecretKey       string `json:"secretKey"`
	ObjectStorageID string `json:"objectStorageId"`
	DisplayName     string `json:"displayName"`
	Region          string `json:"region"`
	CredentialID    int64  `json:"credentialId"`
}

type CreateCustomImageRequest struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url"`
	OSType      string `json:"osType"`
	Version     string `json:"version"`
}

type ReinstallInstanceRequest struct {
	ImageID     string `json:"imageId"`
	UserData    string `json:"userData,omitempty"`
	DefaultUser string `json:"defaultUser,omitempty"`
}

func NewClient(creds Credentials) *Client {
	return &Client{
		credentials: creds,
		apiBaseURL:  defaultAPIBase,
		authURL:     defaultAuthBase,
		httpClient:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) Authenticate(ctx context.Context) error {
	if c.token != "" && time.Until(c.tokenExpiry) > 30*time.Second {
		return nil
	}
	slog.Debug("authenticating with Contabo API", "api_user", strings.TrimSpace(c.credentials.APIUser))

	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("client_id", strings.TrimSpace(c.credentials.ClientID))
	form.Set("client_secret", strings.TrimSpace(c.credentials.ClientSecret))
	form.Set("username", strings.TrimSpace(c.credentials.APIUser))
	form.Set("password", strings.TrimSpace(c.credentials.APIPassword))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("create contabo auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("contabo auth request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("contabo auth failed: %s", strings.TrimSpace(string(body)))
	}

	var token tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return fmt.Errorf("decode contabo auth response: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return fmt.Errorf("contabo auth did not return an access token")
	}

	c.token = strings.TrimSpace(token.AccessToken)
	c.tokenExpiry = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	slog.Debug("authenticated with Contabo API", "expires_at", c.tokenExpiry.Format(time.RFC3339))
	return nil
}

func (c *Client) ListInstances(ctx context.Context) ([]Instance, error) {
	var response listResponse[Instance]
	if err := c.doJSON(ctx, http.MethodGet, "/compute/instances", nil, &response); err != nil {
		return nil, err
	}
	return response.Data, nil
}

func (c *Client) GetInstance(ctx context.Context, instanceID int64) (*Instance, error) {
	var response listResponse[Instance]
	path := "/compute/instances/" + strconv.FormatInt(instanceID, 10)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	if len(response.Data) == 0 {
		return nil, fmt.Errorf("instance %d not found", instanceID)
	}
	return &response.Data[0], nil
}

func (c *Client) ReinstallInstance(ctx context.Context, instanceID int64, imageID string) error {
	request := ReinstallInstanceRequest{ImageID: strings.TrimSpace(imageID)}
	path := "/compute/instances/" + strconv.FormatInt(instanceID, 10)
	return c.doJSON(ctx, http.MethodPut, path, request, nil)
}

func (c *Client) ReinstallInstanceWithOptions(ctx context.Context, instanceID int64, request ReinstallInstanceRequest) error {
	request.ImageID = strings.TrimSpace(request.ImageID)
	request.UserData = strings.TrimSpace(request.UserData)
	request.DefaultUser = strings.TrimSpace(request.DefaultUser)
	path := "/compute/instances/" + strconv.FormatInt(instanceID, 10)
	return c.doJSON(ctx, http.MethodPut, path, request, nil)
}

func (c *Client) ListImages(ctx context.Context) ([]Image, error) {
	var response listResponse[Image]
	if err := c.doJSON(ctx, http.MethodGet, "/compute/images", nil, &response); err != nil {
		return nil, err
	}
	return response.Data, nil
}

func (c *Client) GetImage(ctx context.Context, imageID string) (*Image, error) {
	var response listResponse[Image]
	if err := c.doJSON(ctx, http.MethodGet, "/compute/images/"+url.PathEscape(strings.TrimSpace(imageID)), nil, &response); err != nil {
		return nil, err
	}
	if len(response.Data) == 0 {
		return nil, fmt.Errorf("image %s not found", imageID)
	}
	return &response.Data[0], nil
}

func (c *Client) CreateCustomImage(ctx context.Context, req CreateCustomImageRequest) (*Image, error) {
	var response listResponse[Image]
	if err := c.doJSON(ctx, http.MethodPost, "/compute/images", req, &response); err != nil {
		return nil, err
	}
	if len(response.Data) == 0 {
		return nil, fmt.Errorf("contabo did not return created image data")
	}
	return &response.Data[0], nil
}

func (c *Client) DeleteImage(ctx context.Context, imageID string) error {
	return c.doJSON(ctx, http.MethodDelete, "/compute/images/"+url.PathEscape(strings.TrimSpace(imageID)), nil, nil)
}

func (c *Client) FindDebianImage(ctx context.Context) (*Image, error) {
	images, err := c.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	var candidates []Image
	for _, image := range images {
		name := strings.ToLower(strings.TrimSpace(image.Name + " " + image.DisplayName + " " + image.Description))
		if strings.Contains(name, "debian") && !strings.Contains(strings.ToLower(image.Type), "custom") {
			candidates = append(candidates, image)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no Debian image found in Contabo catalog")
	}
	slices.SortFunc(candidates, func(a, b Image) int {
		return compareImageVersions(a, b) * -1
	})
	return &candidates[0], nil
}

func (c *Client) ListObjectStorages(ctx context.Context) ([]ObjectStorage, error) {
	var response listResponse[ObjectStorage]
	if err := c.doJSON(ctx, http.MethodGet, "/object-storages", nil, &response); err != nil {
		return nil, err
	}
	return response.Data, nil
}

func FindOldestObjectStorageByRegion(storages []ObjectStorage, region string) *ObjectStorage {
	region = normalizeObjectStorageRegion(region)
	if region == "" {
		return nil
	}

	var matches []ObjectStorage
	for _, storage := range storages {
		if regionMatchesObjectStorage(region, storage) {
			matches = append(matches, storage)
		}
	}
	if len(matches) == 0 {
		return nil
	}

	slices.SortFunc(matches, func(a, b ObjectStorage) int {
		switch {
		case a.CreatedDate.IsZero() && !b.CreatedDate.IsZero():
			return 1
		case !a.CreatedDate.IsZero() && b.CreatedDate.IsZero():
			return -1
		case !a.CreatedDate.Equal(b.CreatedDate):
			if a.CreatedDate.Before(b.CreatedDate) {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.TrimSpace(a.ObjectStorageID), strings.TrimSpace(b.ObjectStorageID))
	})

	return &matches[0]
}

func (c *Client) EnsureObjectStorage(ctx context.Context, region, displayName string) (*ObjectStorage, bool, error) {
	storages, err := c.ListObjectStorages(ctx)
	if err != nil {
		return nil, false, err
	}
	region = strings.TrimSpace(region)
	displayName = strings.TrimSpace(displayName)
	if storage := FindOldestObjectStorageByRegion(storages, region); storage != nil {
		slog.Debug("reusing oldest Contabo object storage", "region", region, "object_storage_id", storage.ObjectStorageID, "created_date", storage.CreatedDate)
		return storage, false, nil
	}

	request := map[string]any{
		"region":                region,
		"displayName":           displayName,
		"totalPurchasedSpaceTB": 0.25,
	}
	var response listResponse[ObjectStorage]
	if err := c.doJSON(ctx, http.MethodPost, "/object-storages", request, &response); err != nil {
		return nil, false, err
	}
	if len(response.Data) == 0 {
		return nil, false, fmt.Errorf("contabo did not return created object storage")
	}
	slog.Info("created Contabo object storage", "region", region, "object_storage_id", response.Data[0].ObjectStorageID)
	return &response.Data[0], true, nil
}

func (c *Client) DeleteObjectStorage(ctx context.Context, objectStorageID string) error {
	path := "/object-storages/" + url.PathEscape(strings.TrimSpace(objectStorageID)) + "/cancel"
	return c.doJSON(ctx, http.MethodPost, path, map[string]any{}, nil)
}

func (c *Client) GetCurrentUserClient(ctx context.Context) (*UserClient, error) {
	var response listResponse[UserClient]
	if err := c.doJSON(ctx, http.MethodGet, "/users/client", nil, &response); err != nil {
		return nil, err
	}
	if len(response.Data) == 0 {
		return nil, fmt.Errorf("contabo did not return current user client")
	}
	return &response.Data[0], nil
}

func (c *Client) ListUsers(ctx context.Context, email string) ([]User, error) {
	query := url.Values{}
	if strings.TrimSpace(email) != "" {
		query.Set("email", strings.TrimSpace(email))
	}

	path := "/users"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var response listResponse[User]
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return response.Data, nil
}

func (c *Client) ResolveUser(ctx context.Context, email string) (*User, error) {
	users, err := c.ListUsers(ctx, email)
	if err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("no Contabo user found for email %q", strings.TrimSpace(email))
	}

	email = strings.ToLower(strings.TrimSpace(email))
	if email != "" {
		for _, user := range users {
			if strings.EqualFold(strings.TrimSpace(user.Email), email) {
				slog.Debug("resolved Contabo user", "email", user.Email, "user_id", user.UserID)
				return &user, nil
			}
		}
	}

	if len(users) == 1 {
		slog.Debug("resolved single Contabo user", "email", users[0].Email, "user_id", users[0].UserID)
		return &users[0], nil
	}

	return nil, fmt.Errorf("multiple Contabo users matched %q; unable to resolve a unique userId", strings.TrimSpace(email))
}

func (c *Client) ListObjectStorageCredentials(ctx context.Context, userID, objectStorageID string) ([]ObjectStorageCredential, error) {
	query := url.Values{}
	if strings.TrimSpace(objectStorageID) != "" {
		query.Set("objectStorageId", strings.TrimSpace(objectStorageID))
	}
	path := "/users/" + url.PathEscape(strings.TrimSpace(userID)) + "/object-storages/credentials"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var response listResponse[ObjectStorageCredential]
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return response.Data, nil
}

func (c *Client) ResolveObjectStorageCredential(ctx context.Context, userID, objectStorageID string) (*ObjectStorageCredential, error) {
	credentials, err := c.ListObjectStorageCredentials(ctx, userID, objectStorageID)
	if err != nil {
		return nil, err
	}
	for _, credential := range credentials {
		if strings.TrimSpace(credential.ObjectStorageID) == strings.TrimSpace(objectStorageID) {
			return &credential, nil
		}
	}
	if len(credentials) == 1 && strings.TrimSpace(objectStorageID) == "" {
		return &credentials[0], nil
	}
	return nil, fmt.Errorf("no object storage credentials found for user %s and object storage %s", strings.TrimSpace(userID), strings.TrimSpace(objectStorageID))
}

func (o ObjectStorage) S3Endpoint() string {
	if endpoint := strings.TrimSpace(o.Endpoint); endpoint != "" {
		return endpoint
	}
	return strings.TrimSpace(o.LegacyEndpoint)
}

func (c *Client) GetObjectStorageCredential(ctx context.Context, userID, objectStorageID string, credentialID int64) (*ObjectStorageCredential, error) {
	path := "/users/" + url.PathEscape(strings.TrimSpace(userID)) +
		"/object-storages/" + url.PathEscape(strings.TrimSpace(objectStorageID)) +
		"/credentials/" + strconv.FormatInt(credentialID, 10)
	var response listResponse[ObjectStorageCredential]
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	if len(response.Data) == 0 {
		return nil, fmt.Errorf("contabo did not return object storage credential")
	}
	return &response.Data[0], nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, requestBody any, out any) error {
	if err := c.Authenticate(ctx); err != nil {
		return err
	}
	startedAt := time.Now()
	slog.Debug("calling Contabo API", "method", method, "path", path)

	var body io.Reader
	if requestBody != nil {
		payload, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("marshal contabo request: %w", err)
		}
		body = strings.NewReader(string(payload))
	}

	req, err := http.NewRequestWithContext(ctx, method, c.apiBaseURL+path, body)
	if err != nil {
		return fmt.Errorf("create contabo request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("x-request-id", uuid.NewString())
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call contabo API %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	slog.Debug("Contabo API response", "method", method, "path", path, "status", resp.Status, "duration", time.Since(startedAt).String())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("contabo API %s %s returned %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode contabo response %s %s: %w", method, path, err)
	}
	return nil
}

func compareImageVersions(a, b Image) int {
	av := imageVersion(a)
	bv := imageVersion(b)
	switch {
	case av > bv:
		return 1
	case av < bv:
		return -1
	default:
		return strings.Compare(a.Name+a.DisplayName, b.Name+b.DisplayName)
	}
}

func regionMatchesObjectStorage(region string, storage ObjectStorage) bool {
	return region == normalizeObjectStorageRegion(storage.Region) || region == normalizeObjectStorageRegion(storage.DataCenter)
}

func normalizeObjectStorageRegion(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, " ", "")
	value = strings.ReplaceAll(value, "-", "")
	value = strings.ReplaceAll(value, "(", "")
	value = strings.ReplaceAll(value, ")", "")

	switch value {
	case "eu", "europeanunion":
		return "eu"
	case "uscentral", "unitedstatescentral":
		return "uscentral"
	case "sin", "asiasingapore":
		return "sin"
	default:
		return value
	}
}

func imageVersion(image Image) int {
	re := regexp.MustCompile(`\b(\d+)\b`)
	matches := re.FindAllString(strings.ToLower(image.Name+" "+image.DisplayName+" "+image.Description), -1)
	best := 0
	for _, match := range matches {
		if value, err := strconv.Atoi(match); err == nil && value > best {
			best = value
		}
	}
	return best
}
