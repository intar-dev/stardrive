package talos

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"unicode/utf16"

	talosmeta "github.com/siderolabs/talos/pkg/machinery/meta"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
	networkresource "github.com/siderolabs/talos/pkg/machinery/resources/network"
	"gopkg.in/yaml.v3"
)

const (
	defaultNetworkInterface = "eth0"
	talosADVBlockLength     = 256 * 1024
	talosADVTotalLength     = 2 * talosADVBlockLength
	talosADVDataLength      = talosADVBlockLength - 40
	talosADVMagic1          = 0x5a4b3c2d
	talosADVMagic2          = 0xa5b4c3d2
	gptHeaderOffset         = 512
	gptHeaderSize           = 92
	gptEntrySizeMinimum     = 128
	logicalBlockSize        = 512
	metaPartitionName       = "META"
)

type StaticNetworkConfig struct {
	Interface   string
	AddressCIDR string
	Gateway     string
	Nameservers []string
}

func (n StaticNetworkConfig) Validate() error {
	switch {
	case strings.TrimSpace(n.AddressCIDR) == "":
		return fmt.Errorf("static network addressCIDR is required")
	case strings.TrimSpace(n.Gateway) == "":
		return fmt.Errorf("static network gateway is required")
	}
	if _, err := netip.ParsePrefix(strings.TrimSpace(n.AddressCIDR)); err != nil {
		return fmt.Errorf("parse static network addressCIDR %q: %w", n.AddressCIDR, err)
	}
	if _, err := netip.ParseAddr(strings.TrimSpace(n.Gateway)); err != nil {
		return fmt.Errorf("parse static network gateway %q: %w", n.Gateway, err)
	}
	for _, server := range n.EffectiveNameservers() {
		if _, err := netip.ParseAddr(server); err != nil {
			return fmt.Errorf("parse static network nameserver %q: %w", server, err)
		}
	}
	return nil
}

func (n StaticNetworkConfig) EffectiveInterface() string {
	if iface := strings.TrimSpace(n.Interface); iface != "" {
		return iface
	}
	return defaultNetworkInterface
}

func (n StaticNetworkConfig) EffectiveNameservers() []string {
	values := make([]string, 0, len(n.Nameservers))
	for _, value := range n.Nameservers {
		value = strings.TrimSpace(value)
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

func RenderMachineConfig(base []byte, hostname string, network StaticNetworkConfig) ([]byte, error) {
	if err := network.Validate(); err != nil {
		return nil, err
	}

	var document map[string]any
	if err := yaml.Unmarshal(base, &document); err != nil {
		return nil, fmt.Errorf("parse Talos config: %w", err)
	}

	machine := ensureMap(document, "machine")
	networkMap := ensureMap(machine, "network")
	networkMap["hostname"] = strings.TrimSpace(hostname)
	networkMap["nameservers"] = network.EffectiveNameservers()
	networkMap["interfaces"] = []map[string]any{
		{
			"interface": network.EffectiveInterface(),
			"dhcp":      false,
			"addresses": []string{strings.TrimSpace(network.AddressCIDR)},
			"routes": []map[string]any{
				{
					"network": "0.0.0.0/0",
					"gateway": strings.TrimSpace(network.Gateway),
				},
			},
		},
	}

	rendered, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("render Talos node config: %w", err)
	}
	return rendered, nil
}

func BuildMetalPlatformNetworkConfigYAML(network StaticNetworkConfig) ([]byte, error) {
	if err := network.Validate(); err != nil {
		return nil, err
	}

	addressPrefix, err := netip.ParsePrefix(strings.TrimSpace(network.AddressCIDR))
	if err != nil {
		return nil, fmt.Errorf("parse platform network address %q: %w", network.AddressCIDR, err)
	}
	gateway, err := netip.ParseAddr(strings.TrimSpace(network.Gateway))
	if err != nil {
		return nil, fmt.Errorf("parse platform network gateway %q: %w", network.Gateway, err)
	}

	resolvers := make([]netip.Addr, 0, len(network.EffectiveNameservers()))
	for _, server := range network.EffectiveNameservers() {
		addr, err := netip.ParseAddr(server)
		if err != nil {
			return nil, fmt.Errorf("parse platform network nameserver %q: %w", server, err)
		}
		resolvers = append(resolvers, addr)
	}

	spec := networkresource.PlatformConfigSpec{
		Links: []networkresource.LinkSpecSpec{
			{
				Name:        network.EffectiveInterface(),
				Up:          true,
				Type:        nethelpers.LinkEther,
				ConfigLayer: networkresource.ConfigPlatform,
			},
		},
		Addresses: []networkresource.AddressSpecSpec{
			{
				Address:     addressPrefix,
				LinkName:    network.EffectiveInterface(),
				Family:      nethelpers.FamilyInet4,
				Scope:       nethelpers.ScopeGlobal,
				ConfigLayer: networkresource.ConfigPlatform,
			},
		},
		Routes: []networkresource.RouteSpecSpec{
			{
				Family:      nethelpers.FamilyInet4,
				Destination: netip.MustParsePrefix("0.0.0.0/0"),
				Gateway:     gateway,
				OutLinkName: network.EffectiveInterface(),
				Table:       nethelpers.TableMain,
				ConfigLayer: networkresource.ConfigPlatform,
			},
		},
		Resolvers: []networkresource.ResolverSpecSpec{
			{
				DNSServers:  resolvers,
				ConfigLayer: networkresource.ConfigPlatform,
			},
		},
	}
	spec.Routes[0].Normalize()

	data, err := yaml.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("marshal Talos metal platform network config: %w", err)
	}
	return data, nil
}

func InjectMetalPlatformNetwork(rawImagePath string, network StaticNetworkConfig) error {
	platformConfigYAML, err := BuildMetalPlatformNetworkConfigYAML(network)
	if err != nil {
		return err
	}

	file, err := os.OpenFile(rawImagePath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open Talos raw image %s: %w", rawImagePath, err)
	}
	defer file.Close()

	metaOffset, metaSize, err := locateGPTPartition(file, metaPartitionName)
	if err != nil {
		return err
	}
	if metaSize < talosADVTotalLength {
		return fmt.Errorf("META partition in %s is only %d bytes, expected at least %d", rawImagePath, metaSize, talosADVTotalLength)
	}

	adv, err := newTalosADVFromReader(file, metaOffset)
	if err != nil {
		return fmt.Errorf("read Talos META from %s: %w", rawImagePath, err)
	}
	if ok := adv.SetTag(uint8(talosmeta.MetalNetworkPlatformConfig), platformConfigYAML); !ok {
		return fmt.Errorf("Talos META overflow while setting metal platform network config")
	}
	data, err := adv.Bytes()
	if err != nil {
		return fmt.Errorf("encode Talos META: %w", err)
	}
	if _, err := file.WriteAt(data, metaOffset); err != nil {
		return fmt.Errorf("write Talos META to %s: %w", rawImagePath, err)
	}
	return nil
}

func ensureMap(root map[string]any, key string) map[string]any {
	if existing, ok := root[key]; ok {
		if typed, ok := existing.(map[string]any); ok {
			return typed
		}
		if typed, ok := existing.(map[any]any); ok {
			converted := make(map[string]any, len(typed))
			for k, v := range typed {
				converted[fmt.Sprint(k)] = v
			}
			root[key] = converted
			return converted
		}
	}
	child := map[string]any{}
	root[key] = child
	return child
}

type talosADV struct {
	tags map[uint8][]byte
}

func newTalosADVFromReader(file *os.File, offset int64) (*talosADV, error) {
	buffer := make([]byte, talosADVBlockLength)
	if _, err := file.ReadAt(buffer, offset); err != nil {
		return nil, fmt.Errorf("read primary Talos ADV: %w", err)
	}
	adv := &talosADV{tags: map[uint8][]byte{}}
	if err := adv.Unmarshal(buffer); err == nil {
		return adv, nil
	} else if isZeroBytes(buffer) {
		return adv, nil
	} else {
		primaryErr := err
		if _, err := file.ReadAt(buffer, offset+talosADVBlockLength); err != nil {
			return nil, fmt.Errorf("read secondary Talos ADV: %w", err)
		}
		adv = &talosADV{tags: map[uint8][]byte{}}
		if err := adv.Unmarshal(buffer); err == nil || isZeroBytes(buffer) {
			return adv, nil
		} else {
			return nil, fmt.Errorf("parse Talos ADV: %w (primary copy: %v)", err, primaryErr)
		}
	}
}

func (a *talosADV) SetTag(tag uint8, value []byte) bool {
	if a.tags == nil {
		a.tags = map[uint8][]byte{}
	}
	size := 20
	for _, existing := range a.tags {
		size += len(existing) + 8
	}
	size += len(value) - len(a.tags[tag])
	if size > talosADVDataLength {
		return false
	}
	a.tags[tag] = append([]byte(nil), value...)
	return true
}

func (a *talosADV) Bytes() ([]byte, error) {
	block, err := a.Marshal()
	if err != nil {
		return nil, err
	}
	return append(block, block...), nil
}

func (a *talosADV) Marshal() ([]byte, error) {
	buffer := make([]byte, talosADVBlockLength)
	binary.BigEndian.PutUint32(buffer[0:4], talosADVMagic1)
	binary.BigEndian.PutUint32(buffer[len(buffer)-4:], talosADVMagic2)

	cursor := buffer[4 : len(buffer)-36]
	for tag, value := range a.tags {
		if len(value)+8 > len(cursor) {
			return nil, fmt.Errorf("Talos ADV overflow for tag 0x%02x", tag)
		}
		binary.BigEndian.PutUint32(cursor[0:4], uint32(tag))
		binary.BigEndian.PutUint32(cursor[4:8], uint32(len(value)))
		copy(cursor[8:8+len(value)], value)
		cursor = cursor[8+len(value):]
	}

	checksum := sha256.Sum256(buffer)
	copy(buffer[len(buffer)-36:len(buffer)-4], checksum[:])
	return buffer, nil
}

func (a *talosADV) Unmarshal(buffer []byte) error {
	if len(buffer) < talosADVBlockLength {
		return fmt.Errorf("Talos ADV block too short: %d", len(buffer))
	}
	if magic := binary.BigEndian.Uint32(buffer[0:4]); magic != talosADVMagic1 {
		return fmt.Errorf("unexpected Talos ADV magic 0x%x", magic)
	}
	if magic := binary.BigEndian.Uint32(buffer[len(buffer)-4:]); magic != talosADVMagic2 {
		return fmt.Errorf("unexpected Talos ADV trailer magic 0x%x", magic)
	}

	checksum := append([]byte(nil), buffer[len(buffer)-36:len(buffer)-4]...)
	copy(buffer[len(buffer)-36:len(buffer)-4], make([]byte, 32))
	actual := sha256.Sum256(buffer)
	copy(buffer[len(buffer)-36:len(buffer)-4], checksum)
	if !bytes.Equal(checksum, actual[:]) {
		return fmt.Errorf("Talos ADV checksum mismatch")
	}

	if a.tags == nil {
		a.tags = map[uint8][]byte{}
	}
	for key := range a.tags {
		delete(a.tags, key)
	}

	data := buffer[4 : len(buffer)-36]
	for len(data) >= 8 {
		tag := binary.BigEndian.Uint32(data[0:4])
		if tag == 0 {
			break
		}
		size := binary.BigEndian.Uint32(data[4:8])
		if len(data) < int(8+size) {
			return fmt.Errorf("Talos ADV tag 0x%02x overruns buffer", tag)
		}
		a.tags[uint8(tag)] = append([]byte(nil), data[8:8+size]...)
		data = data[8+size:]
	}
	return nil
}

func locateGPTPartition(file *os.File, name string) (int64, int64, error) {
	headerBuffer := make([]byte, gptHeaderSize)
	if _, err := file.ReadAt(headerBuffer, gptHeaderOffset); err != nil {
		return 0, 0, fmt.Errorf("read GPT header: %w", err)
	}
	if string(headerBuffer[:8]) != "EFI PART" {
		return 0, 0, errors.New("raw image is missing a GPT header")
	}

	partitionEntriesLBA := binary.LittleEndian.Uint64(headerBuffer[72:80])
	partitionEntryCount := binary.LittleEndian.Uint32(headerBuffer[80:84])
	partitionEntrySize := binary.LittleEndian.Uint32(headerBuffer[84:88])
	if partitionEntrySize < gptEntrySizeMinimum {
		return 0, 0, fmt.Errorf("invalid GPT partition entry size %d", partitionEntrySize)
	}

	entriesSize := int64(partitionEntryCount) * int64(partitionEntrySize)
	entryBuffer := make([]byte, entriesSize)
	if _, err := file.ReadAt(entryBuffer, int64(partitionEntriesLBA)*logicalBlockSize); err != nil {
		return 0, 0, fmt.Errorf("read GPT partition entries: %w", err)
	}

	for index := uint32(0); index < partitionEntryCount; index++ {
		entry := entryBuffer[int(index*partitionEntrySize):int((index+1)*partitionEntrySize)]
		if isZeroBytes(entry[:16]) {
			continue
		}
		entryName := decodeGPTPartitionName(entry[56:128])
		if entryName != name {
			continue
		}
		firstLBA := binary.LittleEndian.Uint64(entry[32:40])
		lastLBA := binary.LittleEndian.Uint64(entry[40:48])
		size := int64(lastLBA-firstLBA+1) * logicalBlockSize
		return int64(firstLBA) * logicalBlockSize, size, nil
	}

	return 0, 0, fmt.Errorf("GPT partition %q not found", name)
}

func decodeGPTPartitionName(raw []byte) string {
	values := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		value := binary.LittleEndian.Uint16(raw[i : i+2])
		if value == 0 {
			break
		}
		values = append(values, value)
	}
	return string(utf16.Decode(values))
}

func isZeroBytes(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}
