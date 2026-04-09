// windows_updates.go
package scripts

import (
	"os"
	"time"

	"github.com/yusufpapurcu/wmi"
)

type Win32_QuickFixEngineering struct {
	HotFixID    string
	Description string
	InstalledOn string
}

type UpdateResult struct {
	UpdateID    string  `json:"update_id"`
	Description string  `json:"description"`
	InstallDate *string `json:"install_date"`
	Status      string  `json:"status"`
}

// WindowsUpdatesScript implementa a interface Script
type WindowsUpdatesScript struct{}

func (wu *WindowsUpdatesScript) Name() string {
	return "windows_updates"
}

func (wu *WindowsUpdatesScript) Execute(args ...string) ([]map[string]interface{}, error) {
	updates, err := wu.getUpdates()

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

	// Converte para map
	result := make([]map[string]interface{}, 0, len(updates))
	for _, update := range updates {
		result = append(result, map[string]interface{}{
			"update_id":    update.UpdateID,
			"description":  update.Description,
			"install_date": update.InstallDate,
			"status":       update.Status,
		})
	}

	return result, nil
}

func (wu *WindowsUpdatesScript) getUpdates() ([]UpdateResult, error) {
	var dst []Win32_QuickFixEngineering

	query := "SELECT HotFixID, Description, InstalledOn FROM Win32_QuickFixEngineering"
	if err := wmi.Query(query, &dst); err != nil {
		return nil, err
	}

	var results []UpdateResult

	for _, item := range dst {
		var installDate *string
		if item.InstalledOn != "" {
			val := wu.parseWMIDate(item.InstalledOn)
			installDate = &val
		}

		results = append(results, UpdateResult{
			UpdateID:    item.HotFixID,
			Description: item.Description,
			InstallDate: installDate,
			Status:      "installed",
		})
	}

	if results == nil {
		results = []UpdateResult{}
	}

	return results, nil
}

func (wu *WindowsUpdatesScript) parseWMIDate(raw string) string {
	t, err := time.Parse("20060102", raw)
	if err == nil {
		return t.Format("2006-01-02 15:04:05")
	}
	return raw
}
