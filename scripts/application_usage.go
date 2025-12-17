// application_usage.go
package scripts

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"github.com/shirou/gopsutil/v3/process"
)

type AppInfo struct {
	Username        string `json:"username"`
	ProcessName     string `json:"process_name"`
	ProcessID       int32  `json:"process_id"`
	ApplicationName string `json:"application_name"`
	IsForeground    bool   `json:"is_foreground"`
	WindowState     string `json:"window_state"`
	StartTime       string `json:"start_time"`
	CollectedAt     string `json:"collected_at"`
}

type WindowInfo struct {
	Hwnd        uintptr
	Pid         uint32
	Title       string
	IsMinimized bool
}

// ApplicationUsageScript implementa a interface Script
type ApplicationUsageScript struct {
	user32                       *syscall.LazyDLL
	procGetForegroundWindow      *syscall.LazyProc
	procGetWindowThreadProcessId *syscall.LazyProc
	procIsWindowVisible          *syscall.LazyProc
	procGetWindowTextW           *syscall.LazyProc
	procEnumWindows              *syscall.LazyProc
	procIsIconic                 *syscall.LazyProc
	windowsList                  []WindowInfo
}

func (a *ApplicationUsageScript) Name() string {
	return "application_usage"
}

func (a *ApplicationUsageScript) Execute(args ...string) ([]map[string]interface{}, error) {
	// Inicializa as DLLs se necessário
	a.initDLLs()

	apps, err := a.collectApplications()

	if err != nil {
		username := os.Getenv("USERNAME")
		if username == "" {
			username = "unknown"
		}

		return []map[string]interface{}{
			{
				"username":         username,
				"process_name":     "error",
				"application_name": "list_error",
				"error":            err.Error(),
				"status":           "error",
				"collected_at":     time.Now().Format("2006-01-02 15:04:05"),
			},
		}, err
	}

	// Converte []AppInfo para []map[string]interface{}
	result := make([]map[string]interface{}, 0, len(apps))
	for _, app := range apps {
		result = append(result, map[string]interface{}{
			"username":         app.Username,
			"process_name":     app.ProcessName,
			"process_id":       app.ProcessID,
			"application_name": app.ApplicationName,
			"is_foreground":    app.IsForeground,
			"window_state":     app.WindowState,
			"start_time":       app.StartTime,
			"collected_at":     app.CollectedAt,
		})
	}

	return result, nil
}

// initDLLs inicializa as DLLs do Windows
func (a *ApplicationUsageScript) initDLLs() {
	if a.user32 == nil {
		a.user32 = syscall.NewLazyDLL("user32.dll")
		a.procGetForegroundWindow = a.user32.NewProc("GetForegroundWindow")
		a.procGetWindowThreadProcessId = a.user32.NewProc("GetWindowThreadProcessId")
		a.procIsWindowVisible = a.user32.NewProc("IsWindowVisible")
		a.procGetWindowTextW = a.user32.NewProc("GetWindowTextW")
		a.procEnumWindows = a.user32.NewProc("EnumWindows")
		a.procIsIconic = a.user32.NewProc("IsIconic")
	}
}

// getForegroundWindow obtém o handle da janela em primeiro plano
func (a *ApplicationUsageScript) getForegroundWindow() uintptr {
	hwnd, _, _ := a.procGetForegroundWindow.Call()
	return hwnd
}

// getWindowThreadProcessId obtém o PID do processo da janela
func (a *ApplicationUsageScript) getWindowThreadProcessId(hwnd uintptr) uint32 {
	var pid uint32
	a.procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid
}

// isWindowVisible verifica se a janela está visível
func (a *ApplicationUsageScript) isWindowVisible(hwnd uintptr) bool {
	ret, _, _ := a.procIsWindowVisible.Call(hwnd)
	return ret != 0
}

// isIconic verifica se a janela está minimizada
func (a *ApplicationUsageScript) isIconic(hwnd uintptr) bool {
	ret, _, _ := a.procIsIconic.Call(hwnd)
	return ret != 0
}

// getWindowText obtém o título da janela
func (a *ApplicationUsageScript) getWindowText(hwnd uintptr) string {
	buf := make([]uint16, 256)
	a.procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
	return syscall.UTF16ToString(buf)
}

// enumWindowsCallback é o callback para EnumWindows
func (a *ApplicationUsageScript) enumWindowsCallback(hwnd uintptr, lparam uintptr) uintptr {
	if a.isWindowVisible(hwnd) {
		title := a.getWindowText(hwnd)
		if title != "" {
			pid := a.getWindowThreadProcessId(hwnd)
			isMin := a.isIconic(hwnd)

			a.windowsList = append(a.windowsList, WindowInfo{
				Hwnd:        hwnd,
				Pid:         pid,
				Title:       title,
				IsMinimized: isMin,
			})
		}
	}
	return 1 // Continua a enumeração
}

// getAllWindows enumera todas as janelas visíveis
func (a *ApplicationUsageScript) getAllWindows() []WindowInfo {
	a.windowsList = []WindowInfo{}

	callback := syscall.NewCallback(func(hwnd uintptr, lparam uintptr) uintptr {
		return a.enumWindowsCallback(hwnd, lparam)
	})

	a.procEnumWindows.Call(callback, 0)

	return a.windowsList
}

// collectApplications coleta informações sobre aplicações em execução
func (a *ApplicationUsageScript) collectApplications() ([]AppInfo, error) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "Panic recuperado: %v\n", r)
		}
	}()

	foregroundHwnd := a.getForegroundWindow()
	foregroundPid := a.getWindowThreadProcessId(foregroundHwnd)

	allWindows := a.getAllWindows()

	// Processos do sistema para ignorar
	systemProcesses := map[string]bool{
		"dwm":      true,
		"winlogon": true,
		"csrss":    true,
		"smss":     true,
		"wininit":  true,
		"lsass":    true,
		"services": true,
	}

	// Agrupa janelas por PID
	pidWindows := make(map[int32][]WindowInfo)
	for _, window := range allWindows {
		pid := int32(window.Pid)
		pidWindows[pid] = append(pidWindows[pid], window)
	}

	apps := []AppInfo{}
	username := os.Getenv("USERNAME")
	if username == "" {
		username = "unknown"
	}

	for pid, windows := range pidWindows {
		proc, err := process.NewProcess(pid)
		if err != nil {
			continue
		}

		name, err := proc.Name()
		if err != nil {
			continue
		}

		// Remove extensão .exe
		if len(name) > 4 && name[len(name)-4:] == ".exe" {
			name = name[:len(name)-4]
		}

		// Ignora processos do sistema
		if systemProcesses[name] {
			continue
		}

		// Pega a primeira janela (principal)
		mainWindow := windows[0]

		// Verifica se está em primeiro plano
		isForeground := uint32(pid) == foregroundPid

		// Determina o estado da janela
		var windowState string
		if isForeground {
			windowState = "foreground"
		} else if mainWindow.IsMinimized {
			windowState = "minimized"
		} else {
			windowState = "background"
		}

		// Obtém o tempo de início
		createTime, err := proc.CreateTime()
		var startTime string
		if err == nil {
			t := time.Unix(0, createTime*int64(time.Millisecond))
			startTime = t.Format("2006-01-02 15:04:05")
		} else {
			startTime = "unknown"
		}

		appInfo := AppInfo{
			Username:        username,
			ProcessName:     name,
			ProcessID:       pid,
			ApplicationName: mainWindow.Title,
			IsForeground:    isForeground,
			WindowState:     windowState,
			StartTime:       startTime,
			CollectedAt:     time.Now().Format("2006-01-02 15:04:05"),
		}

		apps = append(apps, appInfo)
	}

	return apps, nil
}
