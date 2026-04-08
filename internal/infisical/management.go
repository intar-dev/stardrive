package infisical

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const identityRoleNoAccess = "no-access"

type APIError struct {
	ReqID      string `json:"reqId"`
	StatusCode int    `json:"statusCode"`
	Message    string `json:"message"`
	Err        string `json:"error"`
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	switch {
	case strings.TrimSpace(e.Message) != "" && strings.TrimSpace(e.Err) != "":
		return fmt.Sprintf("infisical API %d: %s (%s)", e.StatusCode, e.Message, e.Err)
	case strings.TrimSpace(e.Message) != "":
		return fmt.Sprintf("infisical API %d: %s", e.StatusCode, e.Message)
	case strings.TrimSpace(e.Err) != "":
		return fmt.Sprintf("infisical API %d: %s", e.StatusCode, e.Err)
	default:
		return fmt.Sprintf("infisical API %d", e.StatusCode)
	}
}

type MetadataEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Project struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	OrgID string `json:"orgId"`
}

type SecretTag struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type ProjectRole struct {
	ID          string           `json:"id"`
	Slug        string           `json:"slug"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Permissions []map[string]any `json:"permissions"`
}

type MachineIdentity struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	OrganizationID      string          `json:"orgId"`
	HasDeleteProtection bool            `json:"hasDeleteProtection"`
	AuthMethods         []string        `json:"authMethods"`
	Metadata            []MetadataEntry `json:"metadata"`
}

type IdentityMembershipRole struct {
	ID             string `json:"id"`
	Role           string `json:"role"`
	CustomRoleID   string `json:"customRoleId"`
	CustomRoleName string `json:"customRoleName"`
	CustomRoleSlug string `json:"customRoleSlug"`
}

type IdentityMembership struct {
	ID         string                   `json:"id"`
	IdentityID string                   `json:"identityId"`
	Roles      []IdentityMembershipRole `json:"roles"`
}

type UniversalAuthConfig struct {
	ID         string `json:"id"`
	ClientID   string `json:"clientId"`
	IdentityID string `json:"identityId"`
}

type UniversalAuthClientSecret struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Prefix      string `json:"clientSecretPrefix"`
	Revoked     bool   `json:"isClientSecretRevoked"`
}

type UniversalAuthClientSecretResult struct {
	ClientSecret string
	Data         UniversalAuthClientSecret
}

func (c *Client) GetProject(ctx context.Context, projectID string) (*Project, error) {
	var response struct {
		Project Project `json:"project"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/projects/"+url.PathEscape(strings.TrimSpace(projectID)), nil, &response); err != nil {
		return nil, err
	}
	return &response.Project, nil
}

func (c *Client) ListMachineIdentities(ctx context.Context, orgID string) ([]MachineIdentity, error) {
	var response struct {
		Identities []struct {
			OrganizationID string `json:"orgId"`
			IdentityID     string `json:"identityId"`
			Identity       struct {
				ID                  string   `json:"id"`
				Name                string   `json:"name"`
				HasDeleteProtection bool     `json:"hasDeleteProtection"`
				AuthMethods         []string `json:"authMethods"`
			} `json:"identity"`
		} `json:"identities"`
	}
	path := "/api/v1/identities?orgId=" + url.QueryEscape(strings.TrimSpace(orgID))
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}

	out := make([]MachineIdentity, 0, len(response.Identities))
	for _, item := range response.Identities {
		id := strings.TrimSpace(item.Identity.ID)
		if id == "" {
			id = strings.TrimSpace(item.IdentityID)
		}
		out = append(out, MachineIdentity{
			ID:                  id,
			Name:                strings.TrimSpace(item.Identity.Name),
			OrganizationID:      strings.TrimSpace(item.OrganizationID),
			HasDeleteProtection: item.Identity.HasDeleteProtection,
			AuthMethods:         append([]string(nil), item.Identity.AuthMethods...),
		})
	}
	return out, nil
}

func (c *Client) GetMachineIdentity(ctx context.Context, identityID string) (*MachineIdentity, error) {
	var response struct {
		Identity struct {
			OrganizationID string          `json:"orgId"`
			IdentityID     string          `json:"identityId"`
			Metadata       []MetadataEntry `json:"metadata"`
			Identity       struct {
				ID                  string   `json:"id"`
				Name                string   `json:"name"`
				OrganizationID      string   `json:"orgId"`
				HasDeleteProtection bool     `json:"hasDeleteProtection"`
				AuthMethods         []string `json:"authMethods"`
			} `json:"identity"`
		} `json:"identity"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/identities/"+url.PathEscape(strings.TrimSpace(identityID)), nil, &response); err != nil {
		return nil, err
	}

	out := &MachineIdentity{
		ID:                  strings.TrimSpace(response.Identity.Identity.ID),
		Name:                strings.TrimSpace(response.Identity.Identity.Name),
		OrganizationID:      strings.TrimSpace(response.Identity.Identity.OrganizationID),
		HasDeleteProtection: response.Identity.Identity.HasDeleteProtection,
		AuthMethods:         append([]string(nil), response.Identity.Identity.AuthMethods...),
		Metadata:            append([]MetadataEntry(nil), response.Identity.Metadata...),
	}
	if out.ID == "" {
		out.ID = strings.TrimSpace(response.Identity.IdentityID)
	}
	if out.OrganizationID == "" {
		out.OrganizationID = strings.TrimSpace(response.Identity.OrganizationID)
	}
	return out, nil
}

func (c *Client) CreateMachineIdentity(ctx context.Context, orgID, name string, deleteProtection bool, metadata []MetadataEntry) (*MachineIdentity, error) {
	request := struct {
		Name                string          `json:"name"`
		OrganizationID      string          `json:"organizationId"`
		Role                string          `json:"role"`
		HasDeleteProtection bool            `json:"hasDeleteProtection"`
		Metadata            []MetadataEntry `json:"metadata,omitempty"`
	}{
		Name:                strings.TrimSpace(name),
		OrganizationID:      strings.TrimSpace(orgID),
		Role:                identityRoleNoAccess,
		HasDeleteProtection: deleteProtection,
		Metadata:            metadata,
	}
	var response struct {
		Identity MachineIdentity `json:"identity"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/identities", request, &response); err != nil {
		return nil, err
	}
	return &response.Identity, nil
}

func (c *Client) UpdateMachineIdentity(ctx context.Context, identityID, name string, deleteProtection bool, metadata []MetadataEntry) (*MachineIdentity, error) {
	request := struct {
		Name                string          `json:"name,omitempty"`
		HasDeleteProtection bool            `json:"hasDeleteProtection"`
		Metadata            []MetadataEntry `json:"metadata,omitempty"`
	}{
		Name:                strings.TrimSpace(name),
		HasDeleteProtection: deleteProtection,
		Metadata:            metadata,
	}
	var response struct {
		Identity MachineIdentity `json:"identity"`
	}
	if err := c.doJSON(ctx, http.MethodPatch, "/api/v1/identities/"+url.PathEscape(strings.TrimSpace(identityID)), request, &response); err != nil {
		return nil, err
	}
	return &response.Identity, nil
}

func (c *Client) DeleteMachineIdentity(ctx context.Context, identityID string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/identities/"+url.PathEscape(strings.TrimSpace(identityID)), nil, nil)
}

func (c *Client) GetSecretTagBySlug(ctx context.Context, projectID, slug string) (*SecretTag, error) {
	var response struct {
		Tag SecretTag `json:"tag"`
	}
	path := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectID)) + "/tags/slug/" + url.PathEscape(strings.TrimSpace(slug))
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return &response.Tag, nil
}

func (c *Client) CreateSecretTag(ctx context.Context, projectID, slug, color string) (*SecretTag, error) {
	request := struct {
		Slug  string `json:"slug"`
		Color string `json:"color"`
	}{
		Slug:  strings.TrimSpace(slug),
		Color: strings.TrimSpace(color),
	}
	var response struct {
		Tag SecretTag `json:"tag"`
	}
	path := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectID)) + "/tags"
	if err := c.doJSON(ctx, http.MethodPost, path, request, &response); err != nil {
		return nil, err
	}
	return &response.Tag, nil
}

func (c *Client) GetProjectRoleBySlug(ctx context.Context, projectID, slug string) (*ProjectRole, error) {
	var response struct {
		Role ProjectRole `json:"role"`
	}
	path := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectID)) + "/roles/slug/" + url.PathEscape(strings.TrimSpace(slug))
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return &response.Role, nil
}

func (c *Client) CreateProjectRole(ctx context.Context, projectID, slug, name, description string, permissions []map[string]any) (*ProjectRole, error) {
	request := struct {
		Slug        string           `json:"slug"`
		Name        string           `json:"name"`
		Description string           `json:"description"`
		Permissions []map[string]any `json:"permissions"`
	}{
		Slug:        strings.TrimSpace(slug),
		Name:        strings.TrimSpace(name),
		Description: strings.TrimSpace(description),
		Permissions: permissions,
	}
	var response struct {
		Role ProjectRole `json:"role"`
	}
	path := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectID)) + "/roles"
	if err := c.doJSON(ctx, http.MethodPost, path, request, &response); err != nil {
		return nil, err
	}
	return &response.Role, nil
}

func (c *Client) UpdateProjectRole(ctx context.Context, projectID, roleID, slug, name, description string, permissions []map[string]any) (*ProjectRole, error) {
	request := struct {
		Slug        string           `json:"slug"`
		Name        string           `json:"name"`
		Description string           `json:"description"`
		Permissions []map[string]any `json:"permissions"`
	}{
		Slug:        strings.TrimSpace(slug),
		Name:        strings.TrimSpace(name),
		Description: strings.TrimSpace(description),
		Permissions: permissions,
	}
	var response struct {
		Role ProjectRole `json:"role"`
	}
	path := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectID)) + "/roles/" + url.PathEscape(strings.TrimSpace(roleID))
	if err := c.doJSON(ctx, http.MethodPatch, path, request, &response); err != nil {
		return nil, err
	}
	return &response.Role, nil
}

func (c *Client) DeleteProjectRole(ctx context.Context, projectID, roleID string) error {
	path := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectID)) + "/roles/" + url.PathEscape(strings.TrimSpace(roleID))
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

func (c *Client) GetProjectIdentityMembership(ctx context.Context, projectID, identityID string) (*IdentityMembership, error) {
	var response struct {
		IdentityMembership IdentityMembership `json:"identityMembership"`
	}
	path := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectID)) + "/identities/" + url.PathEscape(strings.TrimSpace(identityID))
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return &response.IdentityMembership, nil
}

func (c *Client) CreateProjectIdentityMembership(ctx context.Context, projectID, identityID, roleSlug string) (*IdentityMembership, error) {
	request := struct {
		IdentityID string   `json:"identityId"`
		Roles      []string `json:"roles"`
	}{
		IdentityID: strings.TrimSpace(identityID),
		Roles:      []string{strings.TrimSpace(roleSlug)},
	}
	var response struct {
		IdentityMembership IdentityMembership `json:"identityMembership"`
	}
	path := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectID)) + "/identities"
	if err := c.doJSON(ctx, http.MethodPost, path, request, &response); err != nil {
		return nil, err
	}
	return &response.IdentityMembership, nil
}

func (c *Client) UpdateProjectIdentityMembership(ctx context.Context, projectID, identityID, roleSlug string) ([]IdentityMembershipRole, error) {
	request := struct {
		Roles []string `json:"roles"`
	}{
		Roles: []string{strings.TrimSpace(roleSlug)},
	}
	var response struct {
		Roles []IdentityMembershipRole `json:"roles"`
	}
	path := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectID)) + "/identities/" + url.PathEscape(strings.TrimSpace(identityID))
	if err := c.doJSON(ctx, http.MethodPatch, path, request, &response); err != nil {
		return nil, err
	}
	return response.Roles, nil
}

func (c *Client) DeleteProjectIdentityMembership(ctx context.Context, projectID, identityID string) error {
	path := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectID)) + "/identities/" + url.PathEscape(strings.TrimSpace(identityID))
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

func (c *Client) GetUniversalAuth(ctx context.Context, identityID string) (*UniversalAuthConfig, error) {
	var response struct {
		IdentityUniversalAuth UniversalAuthConfig `json:"identityUniversalAuth"`
	}
	path := "/api/v1/auth/universal-auth/identities/" + url.PathEscape(strings.TrimSpace(identityID))
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return &response.IdentityUniversalAuth, nil
}

func (c *Client) AttachUniversalAuth(ctx context.Context, identityID string) (*UniversalAuthConfig, error) {
	var response struct {
		IdentityUniversalAuth UniversalAuthConfig `json:"identityUniversalAuth"`
	}
	path := "/api/v1/auth/universal-auth/identities/" + url.PathEscape(strings.TrimSpace(identityID))
	if err := c.doJSON(ctx, http.MethodPost, path, map[string]any{}, &response); err != nil {
		return nil, err
	}
	return &response.IdentityUniversalAuth, nil
}

func (c *Client) DeleteUniversalAuth(ctx context.Context, identityID string) error {
	path := "/api/v1/auth/universal-auth/identities/" + url.PathEscape(strings.TrimSpace(identityID))
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

func (c *Client) CreateUniversalAuthClientSecret(ctx context.Context, identityID, description string) (*UniversalAuthClientSecretResult, error) {
	request := struct {
		Description string `json:"description"`
	}{Description: strings.TrimSpace(description)}
	var response struct {
		ClientSecret     string                    `json:"clientSecret"`
		ClientSecretData UniversalAuthClientSecret `json:"clientSecretData"`
	}
	path := "/api/v1/auth/universal-auth/identities/" + url.PathEscape(strings.TrimSpace(identityID)) + "/client-secrets"
	if err := c.doJSON(ctx, http.MethodPost, path, request, &response); err != nil {
		return nil, err
	}
	return &UniversalAuthClientSecretResult{
		ClientSecret: strings.TrimSpace(response.ClientSecret),
		Data:         response.ClientSecretData,
	}, nil
}

func (c *Client) ListUniversalAuthClientSecrets(ctx context.Context, identityID string) ([]UniversalAuthClientSecret, error) {
	var response struct {
		ClientSecretData []UniversalAuthClientSecret `json:"clientSecretData"`
	}
	path := "/api/v1/auth/universal-auth/identities/" + url.PathEscape(strings.TrimSpace(identityID)) + "/client-secrets"
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return response.ClientSecretData, nil
}

func (c *Client) RevokeUniversalAuthClientSecret(ctx context.Context, identityID, clientSecretID string) error {
	path := "/api/v1/auth/universal-auth/identities/" + url.PathEscape(strings.TrimSpace(identityID)) + "/client-secrets/" + url.PathEscape(strings.TrimSpace(clientSecretID)) + "/revoke"
	return c.doJSON(ctx, http.MethodPost, path, nil, nil)
}

func (c *Client) doJSON(ctx context.Context, method, path string, requestBody any, out any) error {
	if c == nil {
		return fmt.Errorf("infisical client is required")
	}
	if c.siteURL == "" {
		return fmt.Errorf("infisical site URL is required")
	}
	token := c.AccessToken()
	if token == "" {
		return fmt.Errorf("infisical access token is required")
	}

	var body io.Reader
	if requestBody != nil {
		payload, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("marshal infisical request: %w", err)
		}
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.siteURL+ensureLeadingSlash(path), body)
	if err != nil {
		return fmt.Errorf("create infisical request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call infisical API %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeInfisicalAPIError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode infisical response %s %s: %w", method, path, err)
	}
	return nil
}

func decodeInfisicalAPIError(resp *http.Response) error {
	if resp == nil {
		return fmt.Errorf("empty infisical response")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read infisical error response: %w", err)
	}

	var apiErr APIError
	if len(body) > 0 && json.Unmarshal(body, &apiErr) == nil {
		if apiErr.StatusCode == 0 {
			apiErr.StatusCode = resp.StatusCode
		}
		return &apiErr
	}

	message := strings.TrimSpace(string(body))
	if message == "" {
		message = resp.Status
	}
	return &APIError{StatusCode: resp.StatusCode, Message: message}
}

func ensureLeadingSlash(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "/") {
		return path
	}
	return "/" + path
}
