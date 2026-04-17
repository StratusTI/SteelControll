// software_inventory.go
package scripts

import (
	"os"
	"regexp"
	"sort"
	"time"

	"golang.org/x/sys/windows/registry"
)

type Software struct {
	SoftwareName string   `json:"software_name"`
	Version      string   `json:"version,omitempty"`
	Publisher    string   `json:"publisher,omitempty"`
	InstallDate  *string  `json:"install_date,omitempty"`
	InstallPath  string   `json:"install_path,omitempty"`
	SizeMB       *float64 `json:"size_mb,omitempty"`
}

// SoftwareInventoryScript implementa a interface Script
type SoftwareInventoryScript struct{}

func (s *SoftwareInventoryScript) Name() string {
	return "software_inventory"
}

func (s *SoftwareInventoryScript) Execute(args ...string) ([]map[string]interface{}, error) {
	softwareList, err := s.getSoftwareInventory()

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

	// Converte []Software para []map[string]interface{}
	result := make([]map[string]interface{}, 0, len(softwareList))
	for _, sw := range softwareList {
		result = append(result, map[string]interface{}{
			"software_name": sw.SoftwareName,
			"version":       sw.Version,
			"publisher":     sw.Publisher,
			"install_date":  sw.InstallDate,
			"install_path":  sw.InstallPath,
			"size_mb":       sw.SizeMB,
		})
	}

	return result, nil
}

func (s *SoftwareInventoryScript) getSoftwareInventory() ([]Software, error) {
	var softwareInventory []Software

	softwarePaths := []struct {
		root registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `Software\Microsoft\Windows\CurrentVersion\Uninstall`},
		{registry.LOCAL_MACHINE, `Software\Wow6432Node\Microsoft\Windows\CurrentVersion\Uninstall`},
	}

	for _, sp := range softwarePaths {
		key, err := registry.OpenKey(sp.root, sp.path, registry.ENUMERATE_SUB_KEYS)
		if err != nil {
			continue
		}
		defer key.Close()

		subkeys, err := key.ReadSubKeyNames(-1)
		if err != nil {
			continue
		}

		for _, subkey := range subkeys {
			software, err := s.readSoftwareInfo(sp.root, sp.path+`\`+subkey)
			if err != nil {
				continue
			}

			if software.SoftwareName == "" {
				continue
			}

			matched, _ := regexp.MatchString(`^KB`, software.SoftwareName)
			if matched {
				continue
			}

			softwareInventory = append(softwareInventory, software)
		}
	}

	uniqueSoftware := s.removeDuplicates(softwareInventory)
	return uniqueSoftware, nil
}

func (s *SoftwareInventoryScript) readSoftwareInfo(root registry.Key, path string) (Software, error) {
	key, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return Software{}, err
	}
	defer key.Close()

	software := Software{}

	if name, _, err := key.GetStringValue("DisplayName"); err == nil {
		software.SoftwareName = name
	}

	if version, _, err := key.GetStringValue("DisplayVersion"); err == nil {
		software.Version = version
	}

	if publisher, _, err := key.GetStringValue("Publisher"); err == nil {
		software.Publisher = publisher
	}

	if installDate, _, err := key.GetStringValue("InstallDate"); err == nil {
		normalized := s.normalizeDate(installDate)
		if normalized != "" {
			software.InstallDate = &normalized
		}
	}

	if installPath, _, err := key.GetStringValue("InstallLocation"); err == nil {
		software.InstallPath = installPath
	}

	if size, _, err := key.GetIntegerValue("EstimatedSize"); err == nil {
		sizeMB := float64(size) / 1024.0
		roundedSize := float64(int(sizeMB*100)) / 100.0
		software.SizeMB = &roundedSize
	}

	return software, nil
}

func (s *SoftwareInventoryScript) normalizeDate(dateStr string) string {
	matched, _ := regexp.MatchString(`^\d{8}$`, dateStr)
	if matched {
		t, err := time.Parse("20060102", dateStr)
		if err == nil {
			return t.Format("2006-01-02")
		}
	}

	matched, _ = regexp.MatchString(`^\d{4}-\d{2}-\d{2}$`, dateStr)
	if matched {
		return dateStr
	}

	return ""
}

func (s *SoftwareInventoryScript) removeDuplicates(software []Software) []Software {
	sort.Slice(software, func(i, j int) bool {
		if software[i].SoftwareName != software[j].SoftwareName {
			return software[i].SoftwareName < software[j].SoftwareName
		}
		return software[i].Version < software[j].Version
	})

	unique := []Software{}
	seen := make(map[string]bool)

	for _, s := range software {
		key := s.SoftwareName + "|" + s.Version
		if !seen[key] {
			seen[key] = true
			unique = append(unique, s)
		}
	}

	return unique
}
