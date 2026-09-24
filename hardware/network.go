package hardware

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
)

// NetworkInfo holds network interface information
type NetworkInfo struct {
	InterfaceName string
	IPAddress     string
	SubnetMask    string
	Connected     bool
	LinkUp        bool
}

// NetworkDetector handles network interface detection and status
type NetworkDetector struct {
	interfaceName string
}

// NewNetworkDetector creates a new network detector for the specified interface
func NewNetworkDetector(interfaceName string) *NetworkDetector {
	return &NetworkDetector{
		interfaceName: interfaceName,
	}
}

// GetNetworkInfo returns current network information for eth0
func (nd *NetworkDetector) GetNetworkInfo() (*NetworkInfo, error) {
	info := &NetworkInfo{
		InterfaceName: nd.interfaceName,
		Connected:     false,
		LinkUp:        false,
	}

	// Check if interface exists and get IP information
	iface, err := net.InterfaceByName(nd.interfaceName)
	if err != nil {
		// Interface doesn't exist
		return info, nil
	}

	// Check link status
	info.LinkUp = nd.isLinkUp(iface)

	// Get IP address and subnet mask
	addrs, err := iface.Addrs()
	if err != nil {
		return info, nil
	}

	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				info.IPAddress = ipnet.IP.String()
				info.SubnetMask = nd.getSubnetMask(ipnet.Mask)
				info.Connected = true
				break
			}
		}
	}

	return info, nil
}

// isLinkUp checks if the network interface link is up
func (nd *NetworkDetector) isLinkUp(iface *net.Interface) bool {
	// Check interface flags
	if iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagRunning != 0 {
		// Also check carrier status from /sys/class/net
		carrierPath := fmt.Sprintf("/sys/class/net/%s/carrier", nd.interfaceName)
		if data, err := os.ReadFile(carrierPath); err == nil {
			carrier := strings.TrimSpace(string(data))
			return carrier == "1"
		}
		// Unreadable carrier must not read as link-up: flags can claim
		// Up+Running while the cable is out, which masked faults.
		return false
	}
	return false
}

// getSubnetMask converts net.IPMask to readable subnet mask
func (nd *NetworkDetector) getSubnetMask(mask net.IPMask) string {
	if len(mask) == 4 {
		return fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])
	}
	return ""
}

// GetNetworkStatus returns a simple status string for display
func (nd *NetworkDetector) GetNetworkStatus() (connected bool, status string) {
	info, err := nd.GetNetworkInfo()
	if err != nil || !info.Connected {
		return false, "No Network"
	}

	if info.IPAddress != "" {
		// Return short IP for status display
		parts := strings.Split(info.IPAddress, ".")
		if len(parts) >= 3 {
			return true, fmt.Sprintf("%s.%s.*", parts[0], parts[1])
		}
		return true, "Connected"
	}

	return false, "No IP"
}

// GetDetailedNetworkInfo returns formatted network information for menu display
func (nd *NetworkDetector) GetDetailedNetworkInfo() []string {
	info, err := nd.GetNetworkInfo()
	if err != nil {
		return []string{"Network Error", err.Error()}
	}

	var details []string
	details = append(details, fmt.Sprintf("Interface: %s", info.InterfaceName))

	if !info.LinkUp {
		details = append(details, "Status: Link Down")
		details = append(details, "Cable: Not Connected")
		return details
	}

	if !info.Connected || info.IPAddress == "" {
		details = append(details, "Status: Link Up")
		details = append(details, "IP Address: Not Assigned")
		details = append(details, "DHCP: Waiting...")
		return details
	}

	details = append(details, "Status: Connected")
	details = append(details, fmt.Sprintf("IP Address: %s", info.IPAddress))
	if info.SubnetMask != "" {
		details = append(details, fmt.Sprintf("Subnet Mask: %s", info.SubnetMask))
	}

	// Get additional network information
	gateway := nd.getGateway()
	if gateway != "" {
		details = append(details, fmt.Sprintf("Gateway: %s", gateway))
	}

	dns := nd.getDNSServers()
	if len(dns) > 0 {
		details = append(details, fmt.Sprintf("DNS: %s", strings.Join(dns, ", ")))
	}

	return details
}

// getGateway attempts to find the default gateway
func (nd *NetworkDetector) getGateway() string {
	// Try to read from /proc/net/route
	file, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	fallback := ""
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)

		// Look for default route (destination 00000000)
		if len(fields) >= 3 && fields[1] == "00000000" {
			// Gateway is in field 2, convert from hex
			gatewayHex := fields[2]
			if len(gatewayHex) == 8 {
				gateway := hexToIP(gatewayHex)
				if gateway == "" {
					continue
				}
				// Prefer the default route via our own interface: without
				// the interface match, Network Info could show wlan0's
				// gateway next to eth0's IP. Keep the first other-interface
				// route only as a fallback.
				if fields[0] == nd.interfaceName {
					return gateway
				}
				if fallback == "" {
					fallback = gateway
				}
			}
		}
	}
	return fallback
}

// hexToIP converts the little-endian hex gateway field from /proc/net/route
// into dotted-quad form.
func hexToIP(hexStr string) string {
	raw, err := hex.DecodeString(hexStr)
	if err != nil || len(raw) != 4 {
		return ""
	}
	// /proc/net/route stores the gateway little-endian; reverse to big-endian.
	return net.IP([]byte{raw[3], raw[2], raw[1], raw[0]}).String()
}

// getDNSServers attempts to get DNS server information
func (nd *NetworkDetector) getDNSServers() []string {
	var dnsServers []string

	// Try to read /etc/resolv.conf
	file, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return dnsServers
	}
	defer file.Close()

	nameserverRegex := regexp.MustCompile(`^nameserver\s+(\S+)`)
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if matches := nameserverRegex.FindStringSubmatch(line); matches != nil {
			dnsServer := matches[1]
			// Filter out IPv6 and localhost
			if net.ParseIP(dnsServer) != nil && !strings.Contains(dnsServer, ":") && dnsServer != "127.0.0.1" {
				dnsServers = append(dnsServers, dnsServer)
			}
		}
	}

	return dnsServers
}

// IsNetworkAvailable returns true if network is connected with IP
func (nd *NetworkDetector) IsNetworkAvailable() bool {
	info, err := nd.GetNetworkInfo()
	return err == nil && info.Connected && info.IPAddress != ""
}
