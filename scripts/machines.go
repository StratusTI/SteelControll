// machines.go
package scripts

import (
	"fmt"
	"net"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

type MachineData struct {
	Hostname   string  `json:"hostname"`
	IPAddress  *string `json:"ip_address"`
	MACAddress *string `json:"mac_address"`
	DomainName *string `json:"domain_name"`
	Status     string  `json:"status"`
}

// MachinesScript implementa a interface Script
type MachinesScript struct{}

func (m *MachinesScript) Name() string {
	return "machines"
}

func (m *MachinesScript) Execute(args ...string) ([]map[string]interface{}, error) {
	machineData, err := m.collectMachineInfo()

	if err != nil {
		hostname, _ := os.Hostname()
		return []map[string]interface{}{
			{
				"hostname": hostname,
				"error":    err.Error(),
				"status":   "error",
			},
		}, err
	}

	// Converte MachineData para map
	result := map[string]interface{}{
		"hostname":    machineData.Hostname,
		"ip_address":  machineData.IPAddress,
		"mac_address": machineData.MACAddress,
		"domain_name": machineData.DomainName,
		"status":      machineData.Status,
	}

	return []map[string]interface{}{result}, nil
}

func (m *MachinesScript) collectMachineInfo() (*MachineData, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("failed to get hostname: %w", err)
	}

	ipAddress, macAddress := m.getNetworkInfo()
	domainName := m.getDomainName(hostname)

	machineData := &MachineData{
		Hostname:   hostname,
		IPAddress:  ipAddress,
		MACAddress: macAddress,
		DomainName: domainName,
		Status:     "active",
	}

	return machineData, nil
}

func (m *MachinesScript) getNetworkInfo() (*string, *string) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, nil
	}

	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip != nil && ip.To4() != nil && !ip.IsLoopback() {
				ipStr := ip.String()
				macStr := iface.HardwareAddr.String()

				var ipPtr *string
				var macPtr *string

				if ipStr != "" {
					ipPtr = &ipStr
				}
				if macStr != "" {
					macPtr = &macStr
				}

				return ipPtr, macPtr
			}
		}
	}

	return nil, nil
}

func (m *MachinesScript) getDomainName(hostname string) *string {
	var computerNameEx [windows.MAX_COMPUTERNAME_LENGTH + 1]uint16
	size := uint32(len(computerNameEx))

	err := windows.GetComputerNameEx(windows.ComputerNameDnsDomain, &computerNameEx[0], &size)
	if err == nil && size > 0 {
		domainName := windows.UTF16ToString(computerNameEx[:size])
		if domainName != "" && !strings.EqualFold(domainName, hostname) {
			return &domainName
		}
	}

	fqdn, err := net.LookupAddr("127.0.0.1")
	if err == nil && len(fqdn) > 0 {
		parts := strings.SplitN(fqdn[0], ".", 2)
		if len(parts) == 2 && parts[1] != "" {
			domainName := strings.TrimSuffix(parts[1], ".")
			if !strings.EqualFold(domainName, hostname) {
				return &domainName
			}
		}
	}

	return nil
}
