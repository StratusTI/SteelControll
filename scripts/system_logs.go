// system_logs.go
package scripts

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/yusufpapurcu/wmi"
)

type LogEntry struct {
	LogName   string      `json:"log_name"`
	EventID   uint32      `json:"event_id"`
	Level     string      `json:"level"`
	Source    string      `json:"source"`
	Message   string      `json:"message"`
	EventTime string      `json:"event_time"`
	Username  interface{} `json:"username"`
}

type Win32_NTLogEvent struct {
	Logfile       string
	EventCode     uint32
	EventType     uint8
	SourceName    string
	Message       string
	TimeGenerated string
}

// SystemLogsScript implementa a interface Script
type SystemLogsScript struct{}

func (sl *SystemLogsScript) Name() string {
	return "system_logs"
}

func (sl *SystemLogsScript) Execute(args ...string) ([]map[string]interface{}, error) {
	defer func() {
		if r := recover(); r != nil {
			// Panic recuperado
		}
	}()

	logs, err := sl.collectSystemLogs()

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

	// Converte []LogEntry para []map[string]interface{}
	result := make([]map[string]interface{}, 0, len(logs))
	for _, log := range logs {
		result = append(result, map[string]interface{}{
			"log_name":   log.LogName,
			"event_id":   log.EventID,
			"level":      log.Level,
			"source":     log.Source,
			"message":    log.Message,
			"event_time": log.EventTime,
			"username":   log.Username,
		})
	}

	return result, nil
}

func (sl *SystemLogsScript) collectSystemLogs() ([]LogEntry, error) {
	logNames := []string{"System", "Application", "Security", "Setup"}
	startTime := time.Now().AddDate(0, 0, -60)
	wmiDateStr := startTime.Format("20060102150405.000000-070")

	var allSystemLogs []LogEntry
	reUser := regexp.MustCompile(`(?:User Name|Account Name):\s+([^\s\r\n]+)`)

	for _, logName := range logNames {
		var wmiEvents []Win32_NTLogEvent

		query := fmt.Sprintf("WHERE Logfile='%s' AND TimeGenerated >= '%s' AND (EventType=1 OR EventType=2)",
			logName, wmiDateStr)

		q := wmi.CreateQuery(&wmiEvents, query)

		err := wmi.Query(q, &wmiEvents)
		if err != nil {
			continue
		}

		for _, event := range wmiEvents {
			var username interface{} = nil
			matches := reUser.FindStringSubmatch(event.Message)
			if len(matches) > 1 {
				username = matches[1]
			}

			levelStr := sl.mapEventType(event.EventType)
			formattedTime := sl.parseWMIDate(event.TimeGenerated)

			entry := LogEntry{
				LogName:   event.Logfile,
				EventID:   event.EventCode,
				Level:     levelStr,
				Source:    event.SourceName,
				Message:   event.Message,
				EventTime: formattedTime,
				Username:  username,
			}

			allSystemLogs = append(allSystemLogs, entry)
		}
	}

	return allSystemLogs, nil
}

func (sl *SystemLogsScript) mapEventType(etype uint8) string {
	switch etype {
	case 1:
		return "Error"
	case 2:
		return "Warning"
	case 3:
		return "Information"
	case 4, 5:
		return "Audit"
	default:
		return "Verbose"
	}
}

func (sl *SystemLogsScript) parseWMIDate(wmiDate string) string {
	if len(wmiDate) < 14 {
		return wmiDate
	}

	layout := "20060102150405"
	t, err := time.Parse(layout, wmiDate[:14])
	if err != nil {
		return wmiDate
	}
	return t.Format("2006-01-02T15:04:05")
}
