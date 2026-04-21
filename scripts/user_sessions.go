// user_sessions.go
package scripts

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"
)

type SessionEvent struct {
	Username        string  `json:"username"`
	SessionType     string  `json:"session_type"`
	EventTime       string  `json:"event_time"`
	SessionID       *string `json:"session_id"`
	DurationSeconds *int    `json:"duration_seconds"`
	LogonType       *string `json:"logon_type"`
	SourceIp        *string `json:"source_ip"`
	CollectedAt     string  `json:"collected_at"`
}

// UserSessionsScript implementa a interface Script
type UserSessionsScript struct{}

func (us *UserSessionsScript) Name() string {
	return "user_sessions"
}

func (us *UserSessionsScript) Execute(args ...string) ([]map[string]interface{}, error) {
	daysBack := 60

	// Parse args se fornecido
	if len(args) > 0 {
		fmt.Sscanf(args[0], "%d", &daysBack)
	}

	sessions, err := us.run(daysBack)

	if err != nil {
		return []map[string]interface{}{
			{
				"username":     "unknown",
				"session_type": "error",
				"error":        err.Error(),
				"event_time":   time.Now().Format("2006-01-02 15:04:05"),
				"collected_at": time.Now().Format("2006-01-02 15:04:05"),
			},
		}, err
	}

	// Converte para map
	result := make([]map[string]interface{}, 0, len(sessions))
	for _, session := range sessions {
		result = append(result, map[string]interface{}{
			"username":         session.Username,
			"session_type":     session.SessionType,
			"event_time":       session.EventTime,
			"session_id":       session.SessionID,
			"duration_seconds": session.DurationSeconds,
			"logon_type":       session.LogonType,
			"source_ip":        session.SourceIp,
			"collected_at":     session.CollectedAt,
		})
	}

	return result, nil
}

func (us *UserSessionsScript) run(daysBack int) ([]SessionEvent, error) {
	activeSessions, err := us.getActiveSessions()
	if err != nil {
		return nil, err
	}

	eventSessions := us.getLogEvents(daysBack)

	userSessions := append(activeSessions, eventSessions...)

	sort.Slice(userSessions, func(i, j int) bool {
		t1, err1 := time.Parse("2006-01-02 15:04:05", userSessions[i].EventTime)
		t2, err2 := time.Parse("2006-01-02 15:04:05", userSessions[j].EventTime)

		if err1 != nil || err2 != nil {
			return userSessions[i].EventTime > userSessions[j].EventTime
		}

		return t1.After(t2)
	})

	return userSessions, nil
}

func (us *UserSessionsScript) getActiveSessions() ([]SessionEvent, error) {
	now := time.Now()
	simulatedDuration := rand.Intn(11)*3600 + 3600
	startTime := now.Add(-time.Duration(simulatedDuration) * time.Second)

	durationSeconds := int(math.Round(now.Sub(startTime).Seconds()))
	sessionIDActive := "1"

	currentUserName := GetFullUsername()
	if currentUserName == "" {
		currentUserName = "simulated_user"
	}

	return []SessionEvent{
		{
			Username:        currentUserName,
			SessionType:     "active",
			EventTime:       startTime.Format("2006-01-02 15:04:05"),
			SessionID:       &sessionIDActive,
			DurationSeconds: &durationSeconds,
			LogonType:       nil,
			SourceIp:        nil,
			CollectedAt:     time.Now().Format("2006-01-02 15:04:05"),
		},
	}, nil
}

func (us *UserSessionsScript) getLogEvents(daysBack int) []SessionEvent {
	events := []SessionEvent{}
	endTime := time.Now()
	startTime := endTime.AddDate(0, 0, -daysBack)
	collectedAt := time.Now().Format("2006-01-02 15:04:05")

	numEvents := 200 + rand.Intn(201)
	users := []string{"UserA", "UserB", "UserC", "AdminX"}
	logonTypes := []string{"2", "3", "10"}

	for i := 0; i < numEvents; i++ {
		timeRange := endTime.Sub(startTime)
		randomOffset := time.Duration(rand.Int63n(int64(timeRange)))
		eventTime := startTime.Add(randomOffset)

		eventTypes := []string{"login", "logout", "screen_locked", "screen_unlocked", "shutdown", "startup"}
		sessionType := eventTypes[rand.Intn(len(eventTypes))]

		username := users[rand.Intn(len(users))]
		logonType := logonTypes[rand.Intn(len(logonTypes))]
		logonID := fmt.Sprintf("0x%x", rand.Intn(999999))
		ipAddress := fmt.Sprintf("192.168.1.%d", 10+rand.Intn(100))

		event := SessionEvent{
			Username:    username,
			SessionType: sessionType,
			EventTime:   eventTime.Format("2006-01-02 15:04:05"),
			CollectedAt: collectedAt,
		}

		switch sessionType {
		case "login":
			event.SessionID = &logonID
			event.LogonType = &logonType
			event.SourceIp = &ipAddress
		case "logout", "screen_locked", "screen_unlocked":
			event.SessionID = &logonID
		}

		events = append(events, event)
	}

	return events
}