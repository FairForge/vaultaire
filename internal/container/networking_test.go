// internal/container/networking_test.go
package container

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNetworkConfig(t *testing.T) {
	t.Run("creates network config", func(t *testing.T) {
		config := &NetworkConfig{
			Name:    "vaultaire-network",
			Driver:  NetworkDriverBridge,
			Subnet:  "172.20.0.0/16",
			Gateway: "172.20.0.1",
		}
		assert.Equal(t, "vaultaire-network", config.Name)
		assert.Equal(t, NetworkDriverBridge, config.Driver)
	})
}

func TestNetworkConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &NetworkConfig{
			Name:   "test-net",
			Driver: NetworkDriverBridge,
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &NetworkConfig{Driver: NetworkDriverBridge}
		err := config.Validate()
		assert.Error(t, err)
	})

	t.Run("rejects invalid subnet", func(t *testing.T) {
		config := &NetworkConfig{
			Name:   "test",
			Driver: NetworkDriverBridge,
			Subnet: "invalid",
		}
		err := config.Validate()
		assert.Error(t, err)
	})
}

func TestNetworkDriver(t *testing.T) {
	t.Run("network drivers", func(t *testing.T) {
		assert.Equal(t, NetworkDriver("bridge"), NetworkDriverBridge)
		assert.Equal(t, NetworkDriver("host"), NetworkDriverHost)
		assert.Equal(t, NetworkDriver("overlay"), NetworkDriverOverlay)
		assert.Equal(t, NetworkDriver("none"), NetworkDriverNone)
	})
}

func TestIPAMConfig(t *testing.T) {
	t.Run("creates IPAM config", func(t *testing.T) {
		ipam := &IPAMConfig{
			Driver: "default",
			Config: []IPAMPool{
				{Subnet: "172.20.0.0/16", Gateway: "172.20.0.1"},
			},
		}
		assert.Equal(t, "default", ipam.Driver)
		assert.Len(t, ipam.Config, 1)
	})
}

func TestIPAMPool(t *testing.T) {
	t.Run("creates IPAM pool", func(t *testing.T) {
		pool := &IPAMPool{
			Subnet:  "10.0.0.0/8",
			Gateway: "10.0.0.1",
			IPRange: "10.0.1.0/24",
		}
		assert.Equal(t, "10.0.0.0/8", pool.Subnet)
	})
}

func TestNetworkEndpoint(t *testing.T) {
	t.Run("creates endpoint", func(t *testing.T) {
		endpoint := &NetworkEndpoint{
			ContainerID: "abc123",
			NetworkID:   "net456",
			IPAddress:   "172.20.0.5",
			MacAddress:  "02:42:ac:14:00:05",
			Gateway:     "172.20.0.1",
		}
		assert.Equal(t, "172.20.0.5", endpoint.IPAddress)
	})
}

func TestDNSConfig(t *testing.T) {
	t.Run("creates DNS config", func(t *testing.T) {
		dns := &DNSConfig{
			Servers: []string{"8.8.8.8", "8.8.4.4"},
			Search:  []string{"local", "cluster.local"},
			Options: []string{"ndots:5"},
		}
		assert.Len(t, dns.Servers, 2)
		assert.Contains(t, dns.Search, "cluster.local")
	})
}

func TestPortForward(t *testing.T) {
	t.Run("creates port forward", func(t *testing.T) {
		pf := &PortForward{
			Protocol:      "tcp",
			HostIP:        "0.0.0.0",
			HostPort:      8080,
			ContainerPort: 80,
		}
		assert.Equal(t, 8080, pf.HostPort)
	})

	t.Run("formats port string", func(t *testing.T) {
		pf := &PortForward{
			HostIP:        "0.0.0.0",
			HostPort:      8080,
			ContainerPort: 80,
		}
		assert.Equal(t, "0.0.0.0:8080->80", pf.String())
	})
}

func TestNetworkPolicy(t *testing.T) {
	t.Run("creates network policy", func(t *testing.T) {
		policy := &NetworkPolicy{
			Name:      "allow-web",
			Namespace: "default",
			PodSelector: map[string]string{
				"app": "web",
			},
			Ingress: []NetworkPolicyRule{
				{
					Ports: []PolicyPort{{Port: 80, Protocol: "TCP"}},
					From:  []PolicyPeer{{PodSelector: map[string]string{"app": "frontend"}}},
				},
			},
		}
		assert.Equal(t, "allow-web", policy.Name)
		assert.Len(t, policy.Ingress, 1)
	})
}

func TestNetworkPolicyRule(t *testing.T) {
	t.Run("creates ingress rule", func(t *testing.T) {
		rule := &NetworkPolicyRule{
			Ports: []PolicyPort{
				{Port: 443, Protocol: "TCP"},
			},
			From: []PolicyPeer{
				{NamespaceSelector: map[string]string{"env": "prod"}},
			},
		}
		assert.Len(t, rule.Ports, 1)
		assert.Len(t, rule.From, 1)
	})

	t.Run("creates egress rule", func(t *testing.T) {
		rule := &NetworkPolicyRule{
			Ports: []PolicyPort{
				{Port: 5432, Protocol: "TCP"},
			},
			To: []PolicyPeer{
				{IPBlock: &IPBlock{CIDR: "10.0.0.0/8"}},
			},
		}
		assert.Len(t, rule.To, 1)
	})
}

func TestIPBlock(t *testing.T) {
	t.Run("creates IP block", func(t *testing.T) {
		block := &IPBlock{
			CIDR:   "10.0.0.0/8",
			Except: []string{"10.0.0.0/24"},
		}
		assert.Equal(t, "10.0.0.0/8", block.CIDR)
		assert.Len(t, block.Except, 1)
	})
}

func TestNewNetworkManager(t *testing.T) {
	t.Run("creates network manager", func(t *testing.T) {
		mgr := NewNetworkManager(&NetworkManagerConfig{
			Provider: "mock",
		})
		assert.NotNil(t, mgr)
	})
}

func TestNetworkManager_CreateNetwork(t *testing.T) {
	mgr := NewNetworkManager(&NetworkManagerConfig{Provider: "mock"})

	t.Run("creates network", func(t *testing.T) {
		config := &NetworkConfig{
			Name:   "test-network",
			Driver: NetworkDriverBridge,
		}
		id, err := mgr.CreateNetwork(config)
		require.NoError(t, err)
		assert.NotEmpty(t, id)
	})
}

func TestNetworkManager_DeleteNetwork(t *testing.T) {
	mgr := NewNetworkManager(&NetworkManagerConfig{Provider: "mock"})

	t.Run("deletes network", func(t *testing.T) {
		err := mgr.DeleteNetwork("test-network")
		assert.NoError(t, err)
	})
}

func TestNetworkManager_ConnectContainer(t *testing.T) {
	mgr := NewNetworkManager(&NetworkManagerConfig{Provider: "mock"})

	t.Run("connects container to network", func(t *testing.T) {
		err := mgr.ConnectContainer("test-network", "container-123", nil)
		assert.NoError(t, err)
	})
}

func TestNetworkManager_DisconnectContainer(t *testing.T) {
	mgr := NewNetworkManager(&NetworkManagerConfig{Provider: "mock"})

	t.Run("disconnects container from network", func(t *testing.T) {
		err := mgr.DisconnectContainer("test-network", "container-123")
		assert.NoError(t, err)
	})
}

func TestNetworkManager_ListNetworks(t *testing.T) {
	mgr := NewNetworkManager(&NetworkManagerConfig{Provider: "mock"})

	t.Run("lists networks", func(t *testing.T) {
		networks, err := mgr.ListNetworks()
		require.NoError(t, err)
		assert.NotNil(t, networks)
	})
}

func TestParseSubnet(t *testing.T) {
	t.Run("parses valid subnet", func(t *testing.T) {
		_, ipnet, err := ParseSubnet("192.168.1.0/24")
		require.NoError(t, err)
		assert.NotNil(t, ipnet)
	})

	t.Run("rejects invalid subnet", func(t *testing.T) {
		_, _, err := ParseSubnet("invalid")
		assert.Error(t, err)
	})
}

func TestIsPrivateIP(t *testing.T) {
	t.Run("identifies private IPs", func(t *testing.T) {
		assert.True(t, IsPrivateIP(net.ParseIP("192.168.1.1")))
		assert.True(t, IsPrivateIP(net.ParseIP("10.0.0.1")))
		assert.True(t, IsPrivateIP(net.ParseIP("172.16.0.1")))
		assert.False(t, IsPrivateIP(net.ParseIP("8.8.8.8")))
	})
}

func TestGenerateMAC(t *testing.T) {
	t.Run("generates valid MAC", func(t *testing.T) {
		mac := GenerateMAC()
		assert.Len(t, mac, 17) // XX:XX:XX:XX:XX:XX
	})
}
