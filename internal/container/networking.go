// internal/container/networking.go
package container

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net"
)

// NetworkDriver represents the network driver type
type NetworkDriver string

const (
	NetworkDriverBridge  NetworkDriver = "bridge"
	NetworkDriverHost    NetworkDriver = "host"
	NetworkDriverOverlay NetworkDriver = "overlay"
	NetworkDriverNone    NetworkDriver = "none"
)

// NetworkConfig represents a container network configuration
type NetworkConfig struct {
	Name       string            `json:"name"`
	Driver     NetworkDriver     `json:"driver"`
	Subnet     string            `json:"subnet"`
	Gateway    string            `json:"gateway"`
	IPRange    string            `json:"ip_range"`
	Internal   bool              `json:"internal"`
	Attachable bool              `json:"attachable"`
	Labels     map[string]string `json:"labels"`
	Options    map[string]string `json:"options"`
	IPAM       *IPAMConfig       `json:"ipam,omitempty"`
}

// Validate checks the network configuration
func (c *NetworkConfig) Validate() error {
	if c.Name == "" {
		return errors.New("networking: network name is required")
	}
	if c.Subnet != "" {
		if _, _, err := net.ParseCIDR(c.Subnet); err != nil {
			return fmt.Errorf("networking: invalid subnet %q: %w", c.Subnet, err)
		}
	}
	return nil
}

// IPAMConfig represents IP Address Management configuration
type IPAMConfig struct {
	Driver  string            `json:"driver"`
	Config  []IPAMPool        `json:"config"`
	Options map[string]string `json:"options"`
}

// IPAMPool represents an IPAM address pool
type IPAMPool struct {
	Subnet     string            `json:"subnet"`
	Gateway    string            `json:"gateway"`
	IPRange    string            `json:"ip_range"`
	AuxAddress map[string]string `json:"aux_address"`
}

// NetworkEndpoint represents a container's connection to a network
type NetworkEndpoint struct {
	ContainerID string   `json:"container_id"`
	NetworkID   string   `json:"network_id"`
	EndpointID  string   `json:"endpoint_id"`
	IPAddress   string   `json:"ip_address"`
	IPv6Address string   `json:"ipv6_address"`
	MacAddress  string   `json:"mac_address"`
	Gateway     string   `json:"gateway"`
	Aliases     []string `json:"aliases"`
}

// DNSConfig represents DNS configuration for a container
type DNSConfig struct {
	Servers []string `json:"servers"`
	Search  []string `json:"search"`
	Options []string `json:"options"`
}

// PortForward represents a port forwarding rule
type PortForward struct {
	Protocol      string `json:"protocol"`
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
}

// String returns the port forward as a string
func (p *PortForward) String() string {
	return fmt.Sprintf("%s:%d->%d", p.HostIP, p.HostPort, p.ContainerPort)
}

// NetworkPolicy represents a Kubernetes-style network policy
type NetworkPolicy struct {
	Name        string              `json:"name"`
	Namespace   string              `json:"namespace"`
	PodSelector map[string]string   `json:"pod_selector"`
	Ingress     []NetworkPolicyRule `json:"ingress,omitempty"`
	Egress      []NetworkPolicyRule `json:"egress,omitempty"`
	PolicyTypes []string            `json:"policy_types"`
}

// NetworkPolicyRule represents a network policy rule
type NetworkPolicyRule struct {
	Ports []PolicyPort `json:"ports,omitempty"`
	From  []PolicyPeer `json:"from,omitempty"`
	To    []PolicyPeer `json:"to,omitempty"`
}

// PolicyPort represents a port in a network policy
type PolicyPort struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}

// PolicyPeer represents a peer in a network policy
type PolicyPeer struct {
	PodSelector       map[string]string `json:"pod_selector,omitempty"`
	NamespaceSelector map[string]string `json:"namespace_selector,omitempty"`
	IPBlock           *IPBlock          `json:"ip_block,omitempty"`
}

// IPBlock represents an IP block for network policies
type IPBlock struct {
	CIDR   string   `json:"cidr"`
	Except []string `json:"except,omitempty"`
}

// NetworkManagerConfig configures the network manager
type NetworkManagerConfig struct {
	Provider string `json:"provider"`
}

// NetworkManager manages container networks
type NetworkManager struct {
	config *NetworkManagerConfig
}

// NewNetworkManager creates a new network manager
func NewNetworkManager(config *NetworkManagerConfig) *NetworkManager {
	return &NetworkManager{config: config}
}

// CreateNetwork creates a new network
func (m *NetworkManager) CreateNetwork(config *NetworkConfig) (string, error) {
	if err := config.Validate(); err != nil {
		return "", err
	}
	if m.config.Provider == "mock" {
		return "net-" + config.Name, nil
	}
	return "", errors.New("networking: not implemented")
}

// DeleteNetwork deletes a network
func (m *NetworkManager) DeleteNetwork(name string) error {
	if m.config.Provider == "mock" {
		return nil
	}
	return errors.New("networking: not implemented")
}

// ConnectContainer connects a container to a network
func (m *NetworkManager) ConnectContainer(network, containerID string, endpoint *NetworkEndpoint) error {
	if m.config.Provider == "mock" {
		return nil
	}
	return errors.New("networking: not implemented")
}

// DisconnectContainer disconnects a container from a network
func (m *NetworkManager) DisconnectContainer(network, containerID string) error {
	if m.config.Provider == "mock" {
		return nil
	}
	return errors.New("networking: not implemented")
}

// ListNetworks lists all networks
func (m *NetworkManager) ListNetworks() ([]*NetworkConfig, error) {
	if m.config.Provider == "mock" {
		return []*NetworkConfig{
			{Name: "bridge", Driver: NetworkDriverBridge},
			{Name: "host", Driver: NetworkDriverHost},
		}, nil
	}
	return nil, errors.New("networking: not implemented")
}

// ParseSubnet parses a CIDR subnet string
func ParseSubnet(cidr string) (net.IP, *net.IPNet, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, nil, fmt.Errorf("networking: invalid CIDR %q: %w", cidr, err)
	}
	return ip, ipnet, nil
}

// IsPrivateIP checks if an IP address is private
func IsPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	private := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
	}
	for _, cidr := range private {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// GenerateMAC generates a random MAC address
func GenerateMAC() string {
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	// Set local bit, clear multicast bit
	buf[0] = (buf[0] | 0x02) & 0xfe
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		buf[0], buf[1], buf[2], buf[3], buf[4], buf[5])
}
