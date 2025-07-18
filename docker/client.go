package docker

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/fosrl/newt/logger"
)

// Container represents a Docker container
type Container struct {
	ID       string             `json:"id"`
	Name     string             `json:"name"`
	Image    string             `json:"image"`
	State    string             `json:"state"`
	Status   string             `json:"status"`
	Ports    []Port             `json:"ports"`
	Labels   map[string]string  `json:"labels"`
	Created  int64              `json:"created"`
	Networks map[string]Network `json:"networks"`
	Hostname string             `json:"hostname"` // added to use hostname if available instead of network address

}

// Port represents a port mapping for a Docker container
type Port struct {
	PrivatePort int    `json:"privatePort"`
	PublicPort  int    `json:"publicPort,omitempty"`
	Type        string `json:"type"`
	IP          string `json:"ip,omitempty"`
}

// Network represents network information for a Docker container
type Network struct {
	NetworkID           string   `json:"networkId"`
	EndpointID          string   `json:"endpointId"`
	Gateway             string   `json:"gateway,omitempty"`
	IPAddress           string   `json:"ipAddress,omitempty"`
	IPPrefixLen         int      `json:"ipPrefixLen,omitempty"`
	IPv6Gateway         string   `json:"ipv6Gateway,omitempty"`
	GlobalIPv6Address   string   `json:"globalIPv6Address,omitempty"`
	GlobalIPv6PrefixLen int      `json:"globalIPv6PrefixLen,omitempty"`
	MacAddress          string   `json:"macAddress,omitempty"`
	Aliases             []string `json:"aliases,omitempty"`
	DNSNames            []string `json:"dnsNames,omitempty"`
}

// CheckSocket checks if Docker socket is available
func CheckSocket(socketPath string) bool {
	// Use the provided socket path or default to standard location
	if socketPath == "" {
		socketPath = "/var/run/docker.sock"
	}

	// Try to create a connection to the Docker socket
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		logger.Debug("Docker socket not available at %s: %v", socketPath, err)
		return false
	}
	defer conn.Close()

	logger.Debug("Docker socket is available at %s", socketPath)
	return true
}

// IsWithinHostNetwork checks if a provided target is within the host container network
func IsWithinHostNetwork(socketPath string, targetAddress string, targetPort int) (bool, error) {
	// Always enforce network validation
	containers, err := ListContainers(socketPath, true)
	if err != nil {
		return false, err
	}

	// Determine if given an IP address
	var parsedTargetAddressIp = net.ParseIP(targetAddress)

	// If we can find the passed hostname/IP address in the networks or as the container name, it is valid and can add it
	for _, c := range containers {
		for _, network := range c.Networks {
			// If the target address is not an IP address, use the container name
			if parsedTargetAddressIp == nil {
				if c.Name == targetAddress {
					for _, port := range c.Ports {
						if port.PublicPort == targetPort || port.PrivatePort == targetPort {
							return true, nil
						}
					}
				}
			} else {
				//If the IP address matches, check the ports being mapped too
				if network.IPAddress == targetAddress {
					for _, port := range c.Ports {
						if port.PublicPort == targetPort || port.PrivatePort == targetPort {
							return true, nil
						}
					}
				}
			}
		}
	}

	combinedTargetAddress := targetAddress + ":" + strconv.Itoa(targetPort)
	return false, fmt.Errorf("target address not within host container network: %s", combinedTargetAddress)
}

// ListContainers lists all Docker containers with their network information
func ListContainers(socketPath string, enforceNetworkValidation bool) ([]Container, error) {
	// Use the provided socket path or default to standard location
	if socketPath == "" {
		socketPath = "/var/run/docker.sock"
	}

	// Used to filter down containers returned to Pangolin
	containerFilters := filters.NewArgs()

	// Used to determine if we will send IP addresses or hostnames to Pangolin
	useContainerIpAddresses := true
	hostContainerId := ""

	// Create a new Docker client
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create client with custom socket path
	cli, err := client.NewClientWithOpts(
		client.WithHost("unix://"+socketPath),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %v", err)
	}

	defer cli.Close()

	hostContainer, err := getHostContainer(ctx, cli)
	if enforceNetworkValidation && err != nil {
		return nil, fmt.Errorf("network validation enforced, cannot validate due to: %w", err)
	}

	// We may not be able to get back host container in scenarios like running the container in network mode 'host'
	if hostContainer != nil {
		// We can use the host container to filter out the list of returned containers
		hostContainerId = hostContainer.ID

		for hostContainerNetworkName := range hostContainer.NetworkSettings.Networks {
			// If we're enforcing network validation, we'll filter on the host containers networks
			if enforceNetworkValidation {
				containerFilters.Add("network", hostContainerNetworkName)
			}

			// If the container is on the docker bridge network, we will use IP addresses over hostnames
			if useContainerIpAddresses && hostContainerNetworkName != "bridge" {
				useContainerIpAddresses = false
			}
		}
	}

	// List containers
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true, Filters: containerFilters})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %v", err)
	}

	var dockerContainers []Container
	for _, c := range containers {
		// Short ID like docker ps
		shortId := c.ID[:12]

		// Inspect container to get hostname
		hostname := ""
		containerInfo, err := cli.ContainerInspect(ctx, c.ID)
		if err == nil && containerInfo.Config != nil {
			hostname = containerInfo.Config.Hostname
		}


		// Skip host container if set
		if hostContainerId != "" && c.ID == hostContainerId {
			continue
		}

		// Get container name (remove leading slash)
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}

		// Convert ports
		var ports []Port
		for _, port := range c.Ports {
			dockerPort := Port{
				PrivatePort: int(port.PrivatePort),
				Type:        port.Type,
			}
			if port.PublicPort != 0 {
				dockerPort.PublicPort = int(port.PublicPort)
			}
			if port.IP != "" {
				dockerPort.IP = port.IP
			}
			ports = append(ports, dockerPort)
		}

		// Get network information by inspecting the container
		networks := make(map[string]Network)

		// Extract network information from inspection
		if c.NetworkSettings != nil && c.NetworkSettings.Networks != nil {
			for networkName, endpoint := range c.NetworkSettings.Networks {
				dockerNetwork := Network{
					NetworkID:           endpoint.NetworkID,
					EndpointID:          endpoint.EndpointID,
					Gateway:             endpoint.Gateway,
					IPPrefixLen:         endpoint.IPPrefixLen,
					IPv6Gateway:         endpoint.IPv6Gateway,
					GlobalIPv6Address:   endpoint.GlobalIPv6Address,
					GlobalIPv6PrefixLen: endpoint.GlobalIPv6PrefixLen,
					MacAddress:          endpoint.MacAddress,
					Aliases:             endpoint.Aliases,
					DNSNames:            endpoint.DNSNames,
				}

				// Use IPs over hostnames/containers as we're on the bridge network
				if useContainerIpAddresses {
					dockerNetwork.IPAddress = endpoint.IPAddress
				}

				networks[networkName] = dockerNetwork
			}
		}

		dockerContainer := Container{
			ID:       shortId,
			Name:     name,
			Image:    c.Image,
			State:    c.State,
			Status:   c.Status,
			Ports:    ports,
			Labels:   c.Labels,
			Created:  c.Created,
			Networks: networks,
			Hostname: hostname, // added
		}

		dockerContainers = append(dockerContainers, dockerContainer)
	}

	return dockerContainers, nil
}

// getHostContainer gets the current container for the current host if possible
func getHostContainer(dockerContext context.Context, dockerClient *client.Client) (*container.InspectResponse, error) {
	// Get hostname from the os
	hostContainerName, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("failed to find hostname for container")
	}

	// Get host container from the docker socket
	hostContainer, err := dockerClient.ContainerInspect(dockerContext, hostContainerName)
	if err != nil {
		return nil, fmt.Errorf("failed to find host container")
	}

	return &hostContainer, nil
}
