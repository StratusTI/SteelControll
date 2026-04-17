// hardware_inventory.go
package scripts

import (
	"fmt"
	"strings"

	"github.com/StackExchange/wmi"
)

// Estruturas WMI
type Win32_Processor struct {
	Manufacturer              string
	Name                      string
	ProcessorId               string
	NumberOfCores             uint32
	NumberOfLogicalProcessors uint32
	Architecture              uint16
	MaxClockSpeed             uint32
}

type Win32_PhysicalMemory struct {
	Manufacturer string
	PartNumber   string
	SerialNumber string
	Capacity     uint64
	Speed        uint32
	MemoryType   uint16
	FormFactor   uint16
	BankLabel    string
}

type Win32_DiskDrive struct {
	Manufacturer   string
	Model          string
	SerialNumber   string
	Size           uint64
	InterfaceType  string
	MediaType      string
	Partitions     uint32
	BytesPerSector uint32
}

type Win32_VideoController struct {
	Name           string
	DriverVersion  string
	DriverDate     string
	AdapterRAM     uint32
	VideoProcessor string
	PNPDeviceID    string
}

type Win32_BaseBoard struct {
	Manufacturer string
	Product      string
	SerialNumber string
}

type Win32_BIOS struct {
	SMBIOSBIOSVersion string
	ReleaseDate       string
}

type Win32_NetworkAdapter struct {
	Name            string
	Manufacturer    string
	MACAddress      string
	PhysicalAdapter bool
	AdapterType     string
	NetConnectionID string
	PNPDeviceID     string
	Speed           uint64
}

// Estruturas de dados de saída
type HardwareComponent struct {
	ComponentType  string                 `json:"component_type"`
	Manufacturer   string                 `json:"manufacturer"`
	Model          string                 `json:"model"`
	SerialNumber   string                 `json:"serial_number"`
	Capacity       string                 `json:"capacity"`
	AdditionalInfo map[string]interface{} `json:"additional_info"`
}

// HardwareInventoryScript implementa a interface Script
type HardwareInventoryScript struct{}

func (h *HardwareInventoryScript) Name() string {
	return "hardware_inventory"
}

func (h *HardwareInventoryScript) Execute(args ...string) ([]map[string]interface{}, error) {
	defer func() {
		if r := recover(); r != nil {
			// Panic recuperado, retorna erro
		}
	}()

	inventory, err := h.collectHardwareInventory()
	if err != nil {
		return []map[string]interface{}{
			{
				"error":     err.Error(),
				"status":    "error",
				"component": "hardware_inventory",
			},
		}, err
	}

	// Converte []HardwareComponent para []map[string]interface{}
	result := make([]map[string]interface{}, 0, len(inventory))
	for _, hw := range inventory {
		result = append(result, map[string]interface{}{
			"component_type":  hw.ComponentType,
			"manufacturer":    hw.Manufacturer,
			"model":           hw.Model,
			"serial_number":   hw.SerialNumber,
			"capacity":        hw.Capacity,
			"additional_info": hw.AdditionalInfo,
		})
	}

	return result, nil
}

func (h *HardwareInventoryScript) collectHardwareInventory() ([]HardwareComponent, error) {
	var inventory []HardwareComponent

	// CPU
	var cpuInfo []Win32_Processor
	if err := wmi.Query("SELECT * FROM Win32_Processor", &cpuInfo); err == nil {
		for _, cpu := range cpuInfo {
			serialNumber := cpu.ProcessorId
			if serialNumber == "" {
				serialNumber = "N/A"
			}

			additionalInfo := map[string]interface{}{
				"cores":           cpu.NumberOfCores,
				"threads":         cpu.NumberOfLogicalProcessors,
				"architecture":    cpu.Architecture,
				"max_clock_speed": cpu.MaxClockSpeed,
			}

			component := HardwareComponent{
				ComponentType:  "cpu",
				Manufacturer:   cpu.Manufacturer,
				Model:          cpu.Name,
				SerialNumber:   serialNumber,
				Capacity:       fmt.Sprintf("%d cores / %d threads", cpu.NumberOfCores, cpu.NumberOfLogicalProcessors),
				AdditionalInfo: additionalInfo,
			}
			inventory = append(inventory, component)
		}
	}

	// Memória
	var memInfo []Win32_PhysicalMemory
	if err := wmi.Query("SELECT * FROM Win32_PhysicalMemory", &memInfo); err == nil {
		for _, mem := range memInfo {
			capacityGB := float64(mem.Capacity) / (1024 * 1024 * 1024)

			manufacturer := mem.Manufacturer
			if manufacturer == "" {
				manufacturer = "Unknown"
			}

			model := strings.TrimSpace(mem.PartNumber)
			if model == "" {
				model = "Unknown"
			}

			serialNumber := strings.TrimSpace(mem.SerialNumber)
			if serialNumber == "" {
				serialNumber = "N/A"
			}

			additionalInfo := map[string]interface{}{
				"speed_mhz":   mem.Speed,
				"memory_type": mem.MemoryType,
				"form_factor": mem.FormFactor,
				"bank_label":  mem.BankLabel,
			}

			component := HardwareComponent{
				ComponentType:  "memory",
				Manufacturer:   manufacturer,
				Model:          model,
				SerialNumber:   serialNumber,
				Capacity:       fmt.Sprintf("%.2fGB", capacityGB),
				AdditionalInfo: additionalInfo,
			}
			inventory = append(inventory, component)
		}
	}

	// Disco
	var diskInfo []Win32_DiskDrive
	if err := wmi.Query("SELECT * FROM Win32_DiskDrive", &diskInfo); err == nil {
		for _, disk := range diskInfo {
			sizeGB := float64(disk.Size) / (1024 * 1024 * 1024)

			manufacturer := disk.Manufacturer
			if manufacturer == "" {
				manufacturer = "Unknown"
			}

			serialNumber := strings.TrimSpace(disk.SerialNumber)
			if serialNumber == "" {
				serialNumber = "N/A"
			}

			additionalInfo := map[string]interface{}{
				"interface_type":   disk.InterfaceType,
				"media_type":       disk.MediaType,
				"partitions":       disk.Partitions,
				"bytes_per_sector": disk.BytesPerSector,
			}

			component := HardwareComponent{
				ComponentType:  "disk",
				Manufacturer:   manufacturer,
				Model:          disk.Model,
				SerialNumber:   serialNumber,
				Capacity:       fmt.Sprintf("%.2fGB", sizeGB),
				AdditionalInfo: additionalInfo,
			}
			inventory = append(inventory, component)
		}
	}

	// GPU
	var gpuInfo []Win32_VideoController
	if err := wmi.Query("SELECT * FROM Win32_VideoController", &gpuInfo); err == nil {
		for _, gpu := range gpuInfo {
			// Filtra adaptadores básicos e VGA
			if strings.Contains(strings.ToLower(gpu.Name), "basic") ||
				strings.Contains(strings.ToLower(gpu.Name), "vga") {
				continue
			}

			memoryMB := gpu.AdapterRAM / (1024 * 1024)

			manufacturer := "Unknown"
			if strings.Contains(gpu.Name, "NVIDIA") {
				manufacturer = "NVIDIA"
			} else if strings.Contains(gpu.Name, "AMD") || strings.Contains(gpu.Name, "ATI") {
				manufacturer = "AMD"
			} else if strings.Contains(gpu.Name, "Intel") {
				manufacturer = "Intel"
			}

			serialNumber := gpu.PNPDeviceID
			if serialNumber == "" {
				serialNumber = "N/A"
			}

			capacity := "Unknown"
			if memoryMB > 0 {
				capacity = fmt.Sprintf("%dMB", memoryMB)
			}

			additionalInfo := map[string]interface{}{
				"driver_version":  gpu.DriverVersion,
				"driver_date":     gpu.DriverDate,
				"video_memory_mb": memoryMB,
				"video_processor": gpu.VideoProcessor,
			}

			component := HardwareComponent{
				ComponentType:  "gpu",
				Manufacturer:   manufacturer,
				Model:          gpu.Name,
				SerialNumber:   serialNumber,
				Capacity:       capacity,
				AdditionalInfo: additionalInfo,
			}
			inventory = append(inventory, component)
		}
	}

	// Motherboard
	var motherboard []Win32_BaseBoard
	if err := wmi.Query("SELECT * FROM Win32_BaseBoard", &motherboard); err == nil && len(motherboard) > 0 {
		mb := motherboard[0]

		var bios []Win32_BIOS
		additionalInfo := map[string]interface{}{
			"bios_version": nil,
			"bios_date":    nil,
		}

		if err := wmi.Query("SELECT * FROM Win32_BIOS", &bios); err == nil && len(bios) > 0 {
			additionalInfo["bios_version"] = bios[0].SMBIOSBIOSVersion
			additionalInfo["bios_date"] = bios[0].ReleaseDate
		}

		serialNumber := mb.SerialNumber
		if serialNumber == "" {
			serialNumber = "N/A"
		}

		component := HardwareComponent{
			ComponentType:  "motherboard",
			Manufacturer:   mb.Manufacturer,
			Model:          mb.Product,
			SerialNumber:   serialNumber,
			Capacity:       "N/A",
			AdditionalInfo: additionalInfo,
		}
		inventory = append(inventory, component)
	}

	// Network Adapters
	var networkAdapters []Win32_NetworkAdapter
	if err := wmi.Query("SELECT * FROM Win32_NetworkAdapter WHERE PhysicalAdapter=TRUE", &networkAdapters); err == nil {
		for _, adapter := range networkAdapters {
			if adapter.MACAddress == "" {
				continue
			}

			manufacturer := adapter.Manufacturer
			if manufacturer == "" {
				manufacturer = "Unknown"
			}

			serialNumber := adapter.MACAddress
			if serialNumber == "" {
				serialNumber = "N/A"
			}

			capacity := "Unknown"
			if adapter.Speed > 0 {
				speedMbps := adapter.Speed / (1024 * 1024)
				capacity = fmt.Sprintf("%dMbps", speedMbps)
			}

			additionalInfo := map[string]interface{}{
				"mac_address":       adapter.MACAddress,
				"adapter_type":      adapter.AdapterType,
				"net_connection_id": adapter.NetConnectionID,
				"pnp_device_id":     adapter.PNPDeviceID,
			}

			component := HardwareComponent{
				ComponentType:  "network",
				Manufacturer:   manufacturer,
				Model:          adapter.Name,
				SerialNumber:   serialNumber,
				Capacity:       capacity,
				AdditionalInfo: additionalInfo,
			}
			inventory = append(inventory, component)
		}
	}

	return inventory, nil
}
