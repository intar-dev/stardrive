package contabo

import (
	"testing"
	"time"
)

func TestFindOldestObjectStorageByRegion(t *testing.T) {
	storages := []ObjectStorage{
		{
			ObjectStorageID: "newer-eu",
			CreatedDate:     time.Date(2026, 4, 8, 10, 0, 0, 0, time.UTC),
			Region:          "European Union",
		},
		{
			ObjectStorageID: "older-eu",
			CreatedDate:     time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC),
			DataCenter:      "EU",
		},
		{
			ObjectStorageID: "sin",
			CreatedDate:     time.Date(2023, 1, 10, 12, 0, 0, 0, time.UTC),
			Region:          "Asia (Singapore)",
		},
	}

	got := FindOldestObjectStorageByRegion(storages, "EU")
	if got == nil {
		t.Fatalf("expected object storage match")
	}
	if got.ObjectStorageID != "older-eu" {
		t.Fatalf("expected oldest EU object storage, got %q", got.ObjectStorageID)
	}
}

func TestFindOldestObjectStorageByRegionReturnsNilWhenMissing(t *testing.T) {
	got := FindOldestObjectStorageByRegion([]ObjectStorage{
		{ObjectStorageID: "sin", Region: "Asia (Singapore)"},
	}, "EU")
	if got != nil {
		t.Fatalf("expected no match, got %q", got.ObjectStorageID)
	}
}

func TestInstancePrimaryIPv4(t *testing.T) {
	instance := Instance{
		IPConfig: InstanceIPConfig{
			V4: &InstanceIP{IP: "203.0.113.10", NetmaskCIDR: 24, Gateway: "203.0.113.1"},
		},
		PublicIPv4: "198.51.100.10",
		IPAddress:  "192.0.2.10",
	}

	if got := instance.PrimaryIPv4(); got != "203.0.113.10" {
		t.Fatalf("expected ipConfig.v4.ip to win, got %q", got)
	}
	if got := instance.PrimaryIPv4AddressCIDR(); got != "203.0.113.10/24" {
		t.Fatalf("expected address CIDR, got %q", got)
	}
	if got := instance.PrimaryIPv4Gateway(); got != "203.0.113.1" {
		t.Fatalf("expected gateway, got %q", got)
	}
}
