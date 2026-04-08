package hetzner

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestCreateStorageBoxUsesCurrentAPIFields(t *testing.T) {
	client := &Client{
		token: "test-token",
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodPost {
					t.Fatalf("unexpected method: %s", req.Method)
				}
				if req.URL.Path != "/v1/storage_boxes" {
					t.Fatalf("unexpected path: %s", req.URL.Path)
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}
				payload := string(body)
				if !strings.Contains(payload, `"storage_box_type":"BX11"`) {
					t.Fatalf("missing storage_box_type in payload: %s", payload)
				}
				if strings.Contains(payload, `"type":"BX11"`) {
					t.Fatalf("legacy type field should not be sent: %s", payload)
				}
				if !strings.Contains(payload, `"password":"Sd1!test-password"`) {
					t.Fatalf("missing password in payload: %s", payload)
				}
				return &http.Response{
					StatusCode: http.StatusCreated,
					Body: io.NopCloser(strings.NewReader(`{
						"storage_box": {
							"id": 7,
							"name": "stardrive-intar-storage-box",
							"username": "u12345",
							"location": "fsn1",
							"storage_box_type": "BX11",
							"status": "initializing"
						}
					}`)),
					Header: make(http.Header),
				}, nil
			}),
		},
	}

	box, err := client.CreateStorageBox(context.Background(), "stardrive-intar-storage-box", "BX11", "fsn1", "Sd1!test-password")
	if err != nil {
		t.Fatalf("CreateStorageBox returned error: %v", err)
	}
	if box.Type != "BX11" {
		t.Fatalf("unexpected storage box type: %q", box.Type)
	}
	if box.Location != "fsn1" {
		t.Fatalf("unexpected storage box location: %q", box.Location)
	}
}

func TestWaitForStorageBoxReadyPollsUntilUsernameAndStatusAreReady(t *testing.T) {
	requests := 0
	client := &Client{
		token: "test-token",
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				if req.Method != http.MethodGet {
					t.Fatalf("unexpected method: %s", req.Method)
				}
				if req.URL.Path != "/v1/storage_boxes" {
					t.Fatalf("unexpected path: %s", req.URL.Path)
				}

				body := `{"storage_boxes":[{"id":7,"name":"stardrive-intar-storage-box","location":"fsn1","storage_box_type":"BX11","status":"initializing"}]}`
				if requests > 1 {
					body = `{"storage_boxes":[{"id":7,"name":"stardrive-intar-storage-box","username":"u12345","location":"fsn1","storage_box_type":"BX11","status":"active"}]}`
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(body)),
					Header:     make(http.Header),
				}, nil
			}),
		},
	}

	box, err := client.WaitForStorageBoxReady(context.Background(), 7, 6*time.Second)
	if err != nil {
		t.Fatalf("WaitForStorageBoxReady returned error: %v", err)
	}
	if requests < 2 {
		t.Fatalf("expected multiple polling requests, got %d", requests)
	}
	if box.Username != "u12345" {
		t.Fatalf("unexpected username: %q", box.Username)
	}
	if box.Status != "active" {
		t.Fatalf("unexpected status: %q", box.Status)
	}
}

func TestResetStorageBoxPasswordUsesExpectedEndpoint(t *testing.T) {
	client := &Client{
		token: "test-token",
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodPost {
					t.Fatalf("unexpected method: %s", req.Method)
				}
				if req.URL.Path != "/v1/storage_boxes/7/actions/reset_password" {
					t.Fatalf("unexpected path: %s", req.URL.Path)
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}
				payload := string(body)
				if !strings.Contains(payload, `"password":"Sd1!replacement-password"`) {
					t.Fatalf("missing password in payload: %s", payload)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{}`)),
					Header:     make(http.Header),
				}, nil
			}),
		},
	}

	if err := client.ResetStorageBoxPassword(context.Background(), 7, "Sd1!replacement-password"); err != nil {
		t.Fatalf("ResetStorageBoxPassword returned error: %v", err)
	}
}

func TestUpdateStorageBoxAccessSettingsUsesExpectedEndpoint(t *testing.T) {
	client := &Client{
		token: "test-token",
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodPost {
					t.Fatalf("unexpected method: %s", req.Method)
				}
				if req.URL.Path != "/v1/storage_boxes/7/actions/update_access_settings" {
					t.Fatalf("unexpected path: %s", req.URL.Path)
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}
				payload := string(body)
				if !strings.Contains(payload, `"samba_enabled":true`) {
					t.Fatalf("missing samba_enabled in payload: %s", payload)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{}`)),
					Header:     make(http.Header),
				}, nil
			}),
		},
	}

	if err := client.UpdateStorageBoxAccessSettings(context.Background(), 7, true); err != nil {
		t.Fatalf("UpdateStorageBoxAccessSettings returned error: %v", err)
	}
}

func TestBuildLabelSelectorUsesHetznerSafeKeys(t *testing.T) {
	selector := buildLabelSelector(map[string]string{
		"stardrive.dev-managed-by": "stardrive",
		"stardrive.dev-cluster":    "intar",
		"stardrive.dev-image-id":   "abc123",
	})
	if strings.Contains(selector, "/") {
		t.Fatalf("label selector should not contain slash-delimited keys: %s", selector)
	}
	if !strings.Contains(selector, "stardrive.dev-managed-by=stardrive") {
		t.Fatalf("missing managed-by selector: %s", selector)
	}
	if !strings.Contains(selector, "stardrive.dev-cluster=intar") {
		t.Fatalf("missing cluster selector: %s", selector)
	}
	if !strings.Contains(selector, "stardrive.dev-image-id=abc123") {
		t.Fatalf("missing image-id selector: %s", selector)
	}
}

func TestEvaluateLoadBalancerTargetsRequiresAttachmentAndHealthyStatus(t *testing.T) {
	ready, summary := evaluateLoadBalancerTargets([]hcloud.LoadBalancerTarget{
		{
			Server: &hcloud.LoadBalancerTargetServer{
				Server: &hcloud.Server{ID: 101},
			},
			HealthStatus: []hcloud.LoadBalancerTargetHealthStatus{
				{ListenPort: 6443, Status: hcloud.LoadBalancerTargetHealthStatusStatusHealthy},
			},
		},
		{
			Server: &hcloud.LoadBalancerTargetServer{
				Server: &hcloud.Server{ID: 202},
			},
			HealthStatus: []hcloud.LoadBalancerTargetHealthStatus{
				{ListenPort: 6443, Status: hcloud.LoadBalancerTargetHealthStatusStatusUnknown},
			},
		},
	}, []int64{101, 202, 303}, 6443)

	if ready {
		t.Fatal("expected targets to be not ready")
	}
	if !strings.Contains(summary, "101:healthy") {
		t.Fatalf("expected healthy target in summary, got %q", summary)
	}
	if !strings.Contains(summary, "202:unknown") {
		t.Fatalf("expected unknown target in summary, got %q", summary)
	}
	if !strings.Contains(summary, "303:missing") {
		t.Fatalf("expected missing target in summary, got %q", summary)
	}
}
