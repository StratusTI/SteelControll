// login_attempts.go

package scripts

import (
	"encoding/xml"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	wevtapi                    = windows.NewLazySystemDLL("wevtapi.dll")
	procEvtQuery               = wevtapi.NewProc("EvtQuery")
	procEvtNext                = wevtapi.NewProc("EvtNext")
	procEvtClose               = wevtapi.NewProc("EvtClose")
	procEvtRender              = wevtapi.NewProc("EvtRender")
	procEvtFormatMessage       = wevtapi.NewProc("EvtFormatMessage")
	procEvtCreateRenderContext = wevtapi.NewProc("EvtCreateRenderContext")
)

const (
	EvtQueryChannelPath      = 0x1
	EvtQueryForwardDirection = 0x100
	EvtRenderEventXml        = 0x1
	EvtRenderContextValues   = 0x0
	EvtFormatMessageEvent    = 0x1
)

type LoginAttempt struct {
	Username      string  `json:"username"`
	LoginType     string  `json:"login_type"`
	Success       bool    `json:"success"`
	FailureReason *string `json:"failure_reason"`
	SourceIP      string  `json:"source_ip"`
	AttemptTime   string  `json:"attempt_time"`
}

type Event struct {
	XMLName xml.Name `xml:"Event"`
	System  struct {
		EventID struct {
			Text string `xml:",chardata"`
		} `xml:"EventID"`
		TimeCreated struct {
			SystemTime string `xml:"SystemTime,attr"`
		} `xml:"TimeCreated"`
	} `xml:"System"`
	EventData struct {
		Data []struct {
			Name string `xml:"Name,attr"`
			Text string `xml:",chardata"`
		} `xml:"Data"`
	} `xml:"EventData"`
}

// LoginAttemptsScript implementa a interface Script
type LoginAttemptsScript struct{}

func (la *LoginAttemptsScript) Name() string {
	return "login_attempts"
}

func (la *LoginAttemptsScript) Execute(args ...string) ([]map[string]interface{}, error) {
	loginAttempts, err := la.collectLoginEvents()

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

	// Converte []LoginAttempt para []map[string]interface{}
	result := make([]map[string]interface{}, 0, len(loginAttempts))
	for _, attempt := range loginAttempts {
		result = append(result, map[string]interface{}{
			"username":       attempt.Username,
			"login_type":     attempt.LoginType,
			"success":        attempt.Success,
			"failure_reason": attempt.FailureReason,
			"source_ip":      attempt.SourceIP,
			"attempt_time":   attempt.AttemptTime,
		})
	}

	return result, nil
}

func (la *LoginAttemptsScript) collectLoginEvents() ([]LoginAttempt, error) {
	loginAttempts := []LoginAttempt{}
	startTime := time.Now().AddDate(0, 0, -60)

	// Eventos de sucesso (EventID 4624)
	successEvents, err := la.readEventLog("Security", 4624, startTime)
	if err == nil {
		for _, event := range successEvents {
			attempt := la.processSuccessEvent(event)
			if attempt != nil {
				loginAttempts = append(loginAttempts, *attempt)
			}
		}
	}

	// Eventos de falha (EventID 4625)
	failedEvents, err := la.readEventLog("Security", 4625, startTime)
	if err == nil {
		for _, event := range failedEvents {
			attempt := la.processFailedEvent(event)
			if attempt != nil {
				loginAttempts = append(loginAttempts, *attempt)
			}
		}
	}

	return loginAttempts, nil
}

func (la *LoginAttemptsScript) readEventLog(logName string, eventID uint32, startTime time.Time) ([]map[string]string, error) {
	startTimeStr := startTime.UTC().Format("2006-01-02T15:04:05.000Z")

	query := fmt.Sprintf(
		`<QueryList><Query Id="0" Path="%s"><Select Path="%s">*[System[(EventID=%d) and TimeCreated[@SystemTime&gt;='%s']]]</Select></Query></QueryList>`,
		logName, logName, eventID, startTimeStr,
	)

	logNamePtr, err := syscall.UTF16PtrFromString(logName)
	if err != nil {
		return nil, err
	}

	queryPtr, err := syscall.UTF16PtrFromString(query)
	if err != nil {
		return nil, err
	}

	handle, _, err := procEvtQuery.Call(
		0,
		uintptr(unsafe.Pointer(logNamePtr)),
		uintptr(unsafe.Pointer(queryPtr)),
		EvtQueryChannelPath|EvtQueryForwardDirection,
	)

	if handle == 0 {
		return nil, fmt.Errorf("EvtQuery failed (run as Administrator): %v", err)
	}
	defer procEvtClose.Call(handle)

	events := []map[string]string{}
	eventHandles := make([]uintptr, 100)

	for {
		var returned uint32
		ret, _, _ := procEvtNext.Call(
			handle,
			uintptr(len(eventHandles)),
			uintptr(unsafe.Pointer(&eventHandles[0])),
			1000, 0,
			uintptr(unsafe.Pointer(&returned)),
		)

		if ret == 0 || returned == 0 {
			break
		}

		for i := uint32(0); i < returned; i++ {
			eventData := la.extractEventData(eventHandles[i])
			if eventData != nil {
				events = append(events, eventData)
			}
			procEvtClose.Call(eventHandles[i])
		}
	}

	return events, nil
}

func (la *LoginAttemptsScript) extractEventData(eventHandle uintptr) map[string]string {
	var bufferSize uint32 = 65536
	buffer := make([]uint16, bufferSize)
	var bufferUsed uint32
	var propertyCount uint32

	ret, _, _ := procEvtRender.Call(
		0, eventHandle,
		EvtRenderEventXml,
		uintptr(bufferSize*2),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(unsafe.Pointer(&bufferUsed)),
		uintptr(unsafe.Pointer(&propertyCount)),
	)

	if ret == 0 {
		return nil
	}

	xmlString := syscall.UTF16ToString(buffer[:bufferUsed/2])

	var event Event
	err := xml.Unmarshal([]byte(xmlString), &event)
	if err != nil {
		return nil
	}

	eventData := make(map[string]string)
	eventData["TimeCreated"] = event.System.TimeCreated.SystemTime

	for _, data := range event.EventData.Data {
		if data.Name != "" {
			eventData[data.Name] = data.Text
		}
	}

	return eventData
}

func (la *LoginAttemptsScript) processSuccessEvent(eventData map[string]string) *LoginAttempt {
	loginType := la.getLoginType(eventData["LogonType"])
	attemptTime := la.parseEventTime(eventData["TimeCreated"])

	return &LoginAttempt{
		Username:      eventData["TargetUserName"],
		LoginType:     loginType,
		Success:       true,
		FailureReason: nil,
		SourceIP:      eventData["IpAddress"],
		AttemptTime:   attemptTime,
	}
}

func (la *LoginAttemptsScript) processFailedEvent(eventData map[string]string) *LoginAttempt {
	loginType := la.getLoginType(eventData["LogonType"])
	failureReason := la.getFailureReason(eventData["Status"])
	attemptTime := la.parseEventTime(eventData["TimeCreated"])

	return &LoginAttempt{
		Username:      eventData["TargetUserName"],
		LoginType:     loginType,
		Success:       false,
		FailureReason: &failureReason,
		SourceIP:      eventData["IpAddress"],
		AttemptTime:   attemptTime,
	}
}

func (la *LoginAttemptsScript) getLoginType(logonType string) string {
	switch logonType {
	case "2":
		return "local"
	case "3":
		return "domain"
	case "4":
		return "service"
	case "10":
		return "remote"
	default:
		return "other"
	}
}

func (la *LoginAttemptsScript) getFailureReason(status string) string {
	switch status {
	case "0xC0000064":
		return "Username not found"
	case "0xC000006A":
		return "Wrong password"
	case "0xC0000234":
		return "Account locked"
	case "0xC0000072":
		return "Account disabled"
	case "0xC000006F":
		return "Outside logon hours"
	case "0xC0000070":
		return "Workstation restriction"
	case "0xC0000193":
		return "Account expired"
	case "0xC0000071":
		return "Password expired"
	default:
		return "Other failure"
	}
}

func (la *LoginAttemptsScript) parseEventTime(timeStr string) string {
	t, err := time.Parse(time.RFC3339, timeStr)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05.999999999Z07:00", timeStr)
		if err != nil {
			return time.Now().Format("2006-01-02T15:04:05")
		}
	}
	return t.Format("2006-01-02T15:04:05")
}
