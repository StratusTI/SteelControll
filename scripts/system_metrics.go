// system_metrics.go
package scripts

import (
	"math"
	"os"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/yusufpapurcu/wmi"
)

type DiskMetric struct {
	Name        string  `json:"name"`
	SizeMB      uint64  `json:"size_mb"`
	FreeMB      uint64  `json:"free_mb"`
	UsedPercent float64 `json:"used_percent"`
}

type TempMetric struct {
	Instance string  `json:"instance"`
	Celsius  float64 `json:"celsius"`
}

type Metrics struct {
	Timestamp          string       `json:"timestamp"`
	CPUUsagePercent    float64      `json:"cpu_usage_percent"`
	MemoryUsagePercent float64      `json:"memory_usage_percent"`
	MemoryTotalMB      uint64       `json:"memory_total_mb"`
	MemoryAvailableMB  uint64       `json:"memory_available_mb"`
	DiskUsage          []DiskMetric `json:"disk_usage"`
	UptimeSeconds      uint64       `json:"uptime_seconds"`
	LoadAverage        float64      `json:"load_average"`
	Temperature        []TempMetric `json:"temperature"`
}

type MSAcpi_ThermalZoneTemperature struct {
	InstanceName       string
	CurrentTemperature uint32
}

type Win32_LogicalDisk struct {
	DeviceID  string
	Size      uint64
	FreeSpace uint64
}

// SystemMetricsScript implementa a interface Script
type SystemMetricsScript struct{}

func (sm *SystemMetricsScript) Name() string {
	return "system_metrics"
}

func (sm *SystemMetricsScript) Execute(args ...string) ([]map[string]interface{}, error) {
	defer func() {
		if r := recover(); r != nil {
			// Panic recuperado
		}
	}()

	metrics, err := sm.collectMetrics()

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

	// Converte Metrics para map
	diskUsage := make([]map[string]interface{}, 0, len(metrics.DiskUsage))
	for _, disk := range metrics.DiskUsage {
		diskUsage = append(diskUsage, map[string]interface{}{
			"name":         disk.Name,
			"size_mb":      disk.SizeMB,
			"free_mb":      disk.FreeMB,
			"used_percent": disk.UsedPercent,
		})
	}

	temperature := make([]map[string]interface{}, 0, len(metrics.Temperature))
	for _, temp := range metrics.Temperature {
		temperature = append(temperature, map[string]interface{}{
			"instance": temp.Instance,
			"celsius":  temp.Celsius,
		})
	}

	return []map[string]interface{}{
		{
			"timestamp":            metrics.Timestamp,
			"cpu_usage_percent":    metrics.CPUUsagePercent,
			"memory_usage_percent": metrics.MemoryUsagePercent,
			"memory_total_mb":      metrics.MemoryTotalMB,
			"memory_available_mb":  metrics.MemoryAvailableMB,
			"disk_usage":           diskUsage,
			"uptime_seconds":       metrics.UptimeSeconds,
			"load_average":         metrics.LoadAverage,
			"temperature":          temperature,
		},
	}, nil
}

func (sm *SystemMetricsScript) collectMetrics() (*Metrics, error) {
	var (
		cpuUsage           float64
		memoryUsagePercent float64
		memoryTotalMB      uint64
		memoryAvailableMB  uint64
		uptimeSeconds      uint64
		diskMetrics        []DiskMetric = []DiskMetric{}
		tempMetrics        []TempMetric = []TempMetric{}
	)

	// CPU
	if cpuPercentSlice, err := cpu.Percent(1*time.Second, false); err == nil && len(cpuPercentSlice) > 0 {
		cpuUsage = cpuPercentSlice[0]
	}

	// Memória
	if vMem, err := mem.VirtualMemory(); err == nil {
		memoryUsagePercent = vMem.UsedPercent
		memoryTotalMB = vMem.Total / 1024 / 1024
		memoryAvailableMB = vMem.Available / 1024 / 1024
	}

	// Uptime
	if hostInfo, err := host.Info(); err == nil {
		uptimeSeconds = hostInfo.Uptime
	}

	// Discos
	var wmiDisks []Win32_LogicalDisk
	queryDisks := "SELECT DeviceID, Size, FreeSpace FROM Win32_LogicalDisk WHERE DriveType=3"
	if err := wmi.Query(queryDisks, &wmiDisks); err == nil {
		for _, d := range wmiDisks {
			sizeMB := d.Size / 1024 / 1024
			freeMB := d.FreeSpace / 1024 / 1024

			usedPercent := 0.0
			if d.Size > 0 {
				usedRaw := float64(d.Size - d.FreeSpace)
				totalRaw := float64(d.Size)
				usedPercent = (usedRaw / totalRaw) * 100.0
			}

			diskMetrics = append(diskMetrics, DiskMetric{
				Name:        d.DeviceID,
				SizeMB:      sizeMB,
				FreeMB:      freeMB,
				UsedPercent: sm.round(usedPercent, 2),
			})
		}
	}

	// Temperatura
	var wmiTemps []MSAcpi_ThermalZoneTemperature
	queryTemp := "SELECT InstanceName, CurrentTemperature FROM MSAcpi_ThermalZoneTemperature"
	if err := wmi.QueryNamespace(queryTemp, &wmiTemps, "root/wmi"); err == nil {
		for _, t := range wmiTemps {
			celsius := (float64(t.CurrentTemperature) - 2732.0) / 10.0
			tempMetrics = append(tempMetrics, TempMetric{
				Instance: t.InstanceName,
				Celsius:  sm.round(celsius, 1),
			})
		}
	}

	metrics := &Metrics{
		Timestamp:          time.Now().Format("2006-01-02 15:04:05"),
		CPUUsagePercent:    sm.round(cpuUsage, 2),
		MemoryUsagePercent: sm.round(memoryUsagePercent, 2),
		MemoryTotalMB:      memoryTotalMB,
		MemoryAvailableMB:  memoryAvailableMB,
		DiskUsage:          diskMetrics,
		UptimeSeconds:      uptimeSeconds,
		LoadAverage:        sm.round(cpuUsage, 2),
		Temperature:        tempMetrics,
	}

	return metrics, nil
}

func (sm *SystemMetricsScript) round(val float64, precision int) float64 {
	ratio := math.Pow(10, float64(precision))
	return math.Round(val*ratio) / ratio
}
