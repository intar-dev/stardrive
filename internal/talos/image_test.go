package talos

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestRenderMachineConfig(t *testing.T) {
	rendered, err := RenderMachineConfig([]byte(`
machine:
  network:
    hostname: old
`), "cp-1", StaticNetworkConfig{
		Interface:   "eth0",
		AddressCIDR: "203.0.113.10/24",
		Gateway:     "203.0.113.1",
		Nameservers: []string{"1.1.1.1"},
	})
	if err != nil {
		t.Fatalf("RenderMachineConfig returned error: %v", err)
	}

	output := string(rendered)
	for _, expected := range []string{
		"hostname: cp-1",
		"interface: eth0",
		"dhcp: false",
		"203.0.113.10/24",
		"network: 0.0.0.0/0",
		"gateway: 203.0.113.1",
		"nameservers:",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("expected rendered config to contain %q, got:\n%s", expected, output)
		}
	}
}

func TestInjectMetalPlatformNetwork(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "talos.raw")
	if err := createSyntheticRawImage(rawPath); err != nil {
		t.Fatalf("createSyntheticRawImage returned error: %v", err)
	}

	cfg := StaticNetworkConfig{
		Interface:   "eth0",
		AddressCIDR: "203.0.113.10/24",
		Gateway:     "203.0.113.1",
		Nameservers: []string{"1.1.1.1", "1.0.0.1"},
	}
	if err := InjectMetalPlatformNetwork(rawPath, cfg); err != nil {
		t.Fatalf("InjectMetalPlatformNetwork returned error: %v", err)
	}

	file, err := os.Open(rawPath)
	if err != nil {
		t.Fatalf("open raw image: %v", err)
	}
	defer file.Close()

	metaOffset, _, err := locateGPTPartition(file, metaPartitionName)
	if err != nil {
		t.Fatalf("locateGPTPartition returned error: %v", err)
	}
	adv, err := newTalosADVFromReader(file, metaOffset)
	if err != nil {
		t.Fatalf("newTalosADVFromReader returned error: %v", err)
	}
	value, ok := adv.tags[uint8(10)]
	if !ok {
		t.Fatalf("expected META tag 0x0a to be present")
	}
	output := string(value)
	for _, expected := range []string{
		"linkName: eth0",
		"address: 203.0.113.10/24",
		"gateway: 203.0.113.1",
		"dnsServers:",
		"1.1.1.1",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("expected injected META YAML to contain %q, got:\n%s", expected, output)
		}
	}
}

func createSyntheticRawImage(path string) error {
	const (
		fileSize            = 4 * 1024 * 1024
		partitionEntryCount = 128
		partitionEntrySize  = 128
		metaFirstLBA        = 2048
		metaLastLBA         = 4095
	)

	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	if err := file.Truncate(fileSize); err != nil {
		return err
	}

	header := make([]byte, gptHeaderSize)
	copy(header[:8], []byte("EFI PART"))
	binary.LittleEndian.PutUint32(header[8:12], 0x00010000)
	binary.LittleEndian.PutUint32(header[12:16], gptHeaderSize)
	binary.LittleEndian.PutUint64(header[24:32], 1)
	binary.LittleEndian.PutUint64(header[32:40], uint64(fileSize/logicalBlockSize-1))
	binary.LittleEndian.PutUint64(header[40:48], 34)
	binary.LittleEndian.PutUint64(header[48:56], uint64(fileSize/logicalBlockSize-34))
	binary.LittleEndian.PutUint64(header[72:80], 2)
	binary.LittleEndian.PutUint32(header[80:84], partitionEntryCount)
	binary.LittleEndian.PutUint32(header[84:88], partitionEntrySize)
	if _, err := file.WriteAt(header, gptHeaderOffset); err != nil {
		return err
	}

	entries := make([]byte, partitionEntryCount*partitionEntrySize)
	copy(entries[0:16], []byte{1})
	copy(entries[16:32], []byte{2})
	binary.LittleEndian.PutUint64(entries[32:40], metaFirstLBA)
	binary.LittleEndian.PutUint64(entries[40:48], metaLastLBA)
	copy(entries[56:128], encodeGPTPartitionName(metaPartitionName))
	_, err = file.WriteAt(entries, 2*logicalBlockSize)
	return err
}

func encodeGPTPartitionName(value string) []byte {
	encoded := utf16.Encode([]rune(value))
	raw := make([]byte, 72)
	for i, r := range encoded {
		binary.LittleEndian.PutUint16(raw[i*2:i*2+2], r)
	}
	return raw
}
