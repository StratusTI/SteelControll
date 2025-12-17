// user_activity.go
package scripts

import (
	"os/user"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	MonitoringSeconds        = 60
	SampleInterval           = 5
	WH_KEYBOARD_LL           = 13
	WH_MOUSE_LL              = 14
	WM_KEYDOWN_USER_ACTIVITY = 0x0100 // Renamed to avoid conflict
	WM_MOUSEMOVE             = 0x0200
)

type LASTINPUTINFO struct {
	cbSize uint32
	dwTime uint32
}

type Session struct {
	Username        string  `json:"username"`
	ActivityType    string  `json:"activity_type"`
	StartTime       string  `json:"start_time"`
	EndTime         *string `json:"end_time"`
	DurationSeconds float64 `json:"duration_seconds"`
	MouseEvents     int64   `json:"mouse_events"`
	KeyboardEvents  int64   `json:"keyboard_events"`
	CreatedAt       string  `json:"created_at"`
}

// UserActivityScript implementa a interface Script
type UserActivityScript struct {
	user32                  *syscall.LazyDLL
	kernel32                *syscall.LazyDLL
	procSetWindowsHookEx    *syscall.LazyProc
	procUnhookWindowsHookEx *syscall.LazyProc
	procCallNextHookEx      *syscall.LazyProc
	procGetMessage          *syscall.LazyProc
	procGetLastInputInfo    *syscall.LazyProc
	procGetModuleHandle     *syscall.LazyProc
	procGetTickCount        *syscall.LazyProc
	globalMouseEvents       int64
	globalKeyboardEvents    int64
}

func (ua *UserActivityScript) Name() string {
	return "user_activity"
}

func (ua *UserActivityScript) Execute(args ...string) ([]map[string]interface{}, error) {
	defer func() {
		if r := recover(); r != nil {
			// Panic recuperado
		}
	}()

	ua.initDLLs()

	currentUser, err := user.Current()
	if err != nil {
		return []map[string]interface{}{
			{
				"error":  "não foi possível obter usuário atual",
				"status": "error",
			},
		}, err
	}

	username := currentUser.Username

	// Inicia hooks em background
	go ua.startHooks()

	var userActivityLog []Session
	startTime := time.Now()
	endTime := startTime.Add(time.Duration(MonitoringSeconds) * time.Second)

	var previousMouseCount int64 = 0
	var previousKeyboardCount int64 = 0
	var currentSession *Session = nil

	timeLayout := "2006-01-02 15:04:05"

	for time.Now().Before(endTime) {
		currentTime := time.Now()
		idleTimeSeconds := ua.getIdleTime()

		currMouse := atomic.LoadInt64(&ua.globalMouseEvents)
		currKbd := atomic.LoadInt64(&ua.globalKeyboardEvents)

		mouseEventsDelta := currMouse - previousMouseCount
		keyboardEventsDelta := currKbd - previousKeyboardCount

		previousMouseCount = currMouse
		previousKeyboardCount = currKbd

		activityType := "active"
		if idleTimeSeconds > 900 && mouseEventsDelta == 0 && keyboardEventsDelta == 0 {
			activityType = "away"
		} else if idleTimeSeconds > 300 && mouseEventsDelta == 0 && keyboardEventsDelta == 0 {
			activityType = "idle"
		}

		if currentSession != nil && currentSession.ActivityType != activityType {
			endStr := currentTime.Format(timeLayout)
			currentSession.EndTime = &endStr

			startT, _ := time.Parse(timeLayout, currentSession.StartTime)
			currentSession.DurationSeconds = currentTime.Sub(startT).Seconds()

			userActivityLog = append(userActivityLog, *currentSession)
			currentSession = nil
		}

		if currentSession == nil {
			currentSession = &Session{
				Username:        username,
				ActivityType:    activityType,
				StartTime:       currentTime.Format(timeLayout),
				EndTime:         nil,
				DurationSeconds: 0,
				MouseEvents:     mouseEventsDelta,
				KeyboardEvents:  keyboardEventsDelta,
				CreatedAt:       currentTime.Format(timeLayout),
			}
		} else {
			currentSession.MouseEvents += mouseEventsDelta
			currentSession.KeyboardEvents += keyboardEventsDelta
		}

		time.Sleep(time.Duration(SampleInterval) * time.Second)
	}

	if currentSession != nil {
		currentTime := time.Now()
		endStr := currentTime.Format(timeLayout)
		currentSession.EndTime = &endStr

		startT, _ := time.Parse(timeLayout, currentSession.StartTime)
		currentSession.DurationSeconds = currentTime.Sub(startT).Seconds()

		userActivityLog = append(userActivityLog, *currentSession)
	}

	// Converte para map
	result := make([]map[string]interface{}, 0, len(userActivityLog))
	for _, session := range userActivityLog {
		result = append(result, map[string]interface{}{
			"username":         session.Username,
			"activity_type":    session.ActivityType,
			"start_time":       session.StartTime,
			"end_time":         session.EndTime,
			"duration_seconds": session.DurationSeconds,
			"mouse_events":     session.MouseEvents,
			"keyboard_events":  session.KeyboardEvents,
			"created_at":       session.CreatedAt,
		})
	}

	return result, nil
}

func (ua *UserActivityScript) initDLLs() {
	if ua.user32 == nil {
		ua.user32 = syscall.NewLazyDLL("user32.dll")
		ua.kernel32 = syscall.NewLazyDLL("kernel32.dll")
		ua.procSetWindowsHookEx = ua.user32.NewProc("SetWindowsHookExW")
		ua.procUnhookWindowsHookEx = ua.user32.NewProc("UnhookWindowsHookEx")
		ua.procCallNextHookEx = ua.user32.NewProc("CallNextHookEx")
		ua.procGetMessage = ua.user32.NewProc("GetMessageW")
		ua.procGetLastInputInfo = ua.user32.NewProc("GetLastInputInfo")
		ua.procGetModuleHandle = ua.kernel32.NewProc("GetModuleHandleW")
		ua.procGetTickCount = ua.kernel32.NewProc("GetTickCount")
	}
}

func (ua *UserActivityScript) mouseHook(nCode int, wParam uintptr, lParam uintptr) uintptr {
	if nCode >= 0 {
		atomic.AddInt64(&ua.globalMouseEvents, 1)
	}
	ret, _, _ := ua.procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
	return ret
}

func (ua *UserActivityScript) keyboardHook(nCode int, wParam uintptr, lParam uintptr) uintptr {
	if nCode >= 0 {
		atomic.AddInt64(&ua.globalKeyboardEvents, 1)
	}
	ret, _, _ := ua.procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
	return ret
}

func (ua *UserActivityScript) startHooks() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	mod, _, _ := ua.procGetModuleHandle.Call(0)

	mouseCallback := syscall.NewCallback(ua.mouseHook)
	kbdCallback := syscall.NewCallback(ua.keyboardHook)

	hMouseHook, _, _ := ua.procSetWindowsHookEx.Call(uintptr(WH_MOUSE_LL), mouseCallback, mod, 0)
	hKbdHook, _, _ := ua.procSetWindowsHookEx.Call(uintptr(WH_KEYBOARD_LL), kbdCallback, mod, 0)

	var msg struct {
		hwnd    syscall.Handle
		message uint32
		wParam  uintptr
		lParam  uintptr
		time    uint32
		pt      struct{ x, y int32 }
	}

	for {
		ret, _, _ := ua.procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if ret == 0 || ret == ^uintptr(0) {
			break
		}
	}

	ua.procUnhookWindowsHookEx.Call(hMouseHook)
	ua.procUnhookWindowsHookEx.Call(hKbdHook)
}

func (ua *UserActivityScript) getIdleTime() float64 {
	var lastInput LASTINPUTINFO
	lastInput.cbSize = uint32(unsafe.Sizeof(lastInput))

	ret, _, _ := ua.procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&lastInput)))
	if ret == 0 {
		return 0
	}

	tickCount, _, _ := ua.procGetTickCount.Call()
	elapsedMillis := uint32(tickCount) - lastInput.dwTime
	return float64(elapsedMillis) / 1000.0
}
