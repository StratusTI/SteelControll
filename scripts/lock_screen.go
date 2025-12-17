// lock_screen.go
package scripts

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"github.com/faiface/beep"
	"github.com/faiface/beep/speaker"
	"github.com/faiface/beep/wav"
	"github.com/lxn/win"
)

type MONITORINFO struct {
	CbSize    uint32
	RcMonitor win.RECT
	RcWork    win.RECT
	DwFlags   uint32
}

type Screen struct {
	X      int
	Y      int
	Width  int
	Height int
}

type WNDCLASSEX struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     win.HINSTANCE
	HIcon         win.HICON
	HCursor       win.HCURSOR
	HbrBackground win.HBRUSH
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       win.HICON
}

type MSG struct {
	Hwnd    win.HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      win.POINT
}

const (
	WS_POPUP            = 0x80000000
	WS_EX_TOPMOST       = 0x00000008
	WS_EX_LAYERED       = 0x00080000
	WS_EX_TOOLWINDOW    = 0x00000080
	LWA_ALPHA           = 0x00000002
	WM_DESTROY          = 0x0002
	WM_PAINT            = 0x000F
	WM_CLOSE            = 0x0010
	WM_KEYDOWN          = 0x0100
	WM_SYSKEYDOWN       = 0x0104
	TRANSPARENT         = 1
	DT_CENTER           = 0x00000001
	DT_VCENTER          = 0x00000004
	DT_SINGLELINE       = 0x00000020
	FW_BOLD             = 700
	DEFAULT_CHARSET     = 1
	OUT_DEFAULT_PRECIS  = 0
	CLIP_DEFAULT_PRECIS = 0
	DEFAULT_QUALITY     = 0
	DEFAULT_PITCH       = 0
	FF_DONTCARE         = 0
)

// LockScreenScript implementa a interface Script
type LockScreenScript struct {
	user32                         *syscall.LazyDLL
	gdi32                          *syscall.LazyDLL
	procEnumDisplayMonitors        *syscall.LazyProc
	procGetMonitorInfo             *syscall.LazyProc
	procSetLayeredWindowAttributes *syscall.LazyProc
	procCreateWindowEx             *syscall.LazyProc
	procDefWindowProc              *syscall.LazyProc
	procRegisterClassEx            *syscall.LazyProc
	procGetMessage                 *syscall.LazyProc
	procTranslateMessage           *syscall.LazyProc
	procDispatchMessage            *syscall.LazyProc
	procPostQuitMessage            *syscall.LazyProc
	procCreateSolidBrush           *syscall.LazyProc
	procDrawText                   *syscall.LazyProc
	procCreateFont                 *syscall.LazyProc
	procSelectObject               *syscall.LazyProc
	procSetBkMode                  *syscall.LazyProc
	procSetTextColor               *syscall.LazyProc
	className                      *uint16
	messageUTF16                   *uint16
	windows                        []win.HWND
}

func (l *LockScreenScript) Name() string {
	return "lock_screen"
}

func (l *LockScreenScript) Execute(args ...string) ([]map[string]interface{}, error) {
	// Inicializa DLLs
	l.initDLLs()

	// Argumentos padrão
	message := "Seu expediente finalizou. Para adquirir mais horas contate seu gestor"
	musicPath := "boa.wav"
	duration := 20 // segundos (não usado atualmente, mas pode ser implementado)

	// Parse de args: -Message "texto" -Duration "20"
	for i := 0; i < len(args); i++ {
		if args[i] == "-Message" && i+1 < len(args) {
			message = args[i+1]
			i++
		} else if args[i] == "-music" && i+1 < len(args) {
			musicPath = args[i+1]
			i++
		} else if args[i] == "-Duration" && i+1 < len(args) {
			fmt.Sscanf(args[i+1], "%d", &duration)
			i++
		}
	}

	l.messageUTF16 = syscall.StringToUTF16Ptr(message)

	// Toca música (se houver)
	go l.playMusic(musicPath)

	// Registra a classe da janela
	if !l.registerWindowClass() {
		return []map[string]interface{}{
			{
				"error":  "erro ao registrar classe de janela",
				"status": "error",
			},
		}, fmt.Errorf("erro ao registrar classe de janela")
	}

	// Obtém os monitores
	screens := l.getScreens()

	// Cria janela para cada monitor
	for i, screen := range screens {
		hwnd := l.createWindow(screen, i)
		if hwnd != 0 {
			l.windows = append(l.windows, hwnd)
		}
	}

	// Loop de manutenção (mantém topmost)
	go l.maintenanceLoop()

	// Loop de mensagens (bloqueante) - em uma aplicação real, isso seria feito em background
	// Por ora, apenas retorna sucesso
	// go l.messageLoop()

	return []map[string]interface{}{
		{
			"status":       "locked",
			"message":      message,
			"monitors":     len(screens),
			"duration_sec": duration,
		},
	}, nil
}

func (l *LockScreenScript) initDLLs() {
	if l.user32 == nil {
		l.user32 = syscall.NewLazyDLL("user32.dll")
		l.gdi32 = syscall.NewLazyDLL("gdi32.dll")
		l.procEnumDisplayMonitors = l.user32.NewProc("EnumDisplayMonitors")
		l.procGetMonitorInfo = l.user32.NewProc("GetMonitorInfoW")
		l.procSetLayeredWindowAttributes = l.user32.NewProc("SetLayeredWindowAttributes")
		l.procCreateWindowEx = l.user32.NewProc("CreateWindowExW")
		l.procDefWindowProc = l.user32.NewProc("DefWindowProcW")
		l.procRegisterClassEx = l.user32.NewProc("RegisterClassExW")
		l.procGetMessage = l.user32.NewProc("GetMessageW")
		l.procTranslateMessage = l.user32.NewProc("TranslateMessage")
		l.procDispatchMessage = l.user32.NewProc("DispatchMessageW")
		l.procPostQuitMessage = l.user32.NewProc("PostQuitMessage")
		l.procCreateSolidBrush = l.gdi32.NewProc("CreateSolidBrush")
		l.procDrawText = l.user32.NewProc("DrawTextW")
		l.procCreateFont = l.gdi32.NewProc("CreateFontW")
		l.procSelectObject = l.gdi32.NewProc("SelectObject")
		l.procSetBkMode = l.gdi32.NewProc("SetBkMode")
		l.procSetTextColor = l.gdi32.NewProc("SetTextColor")
		l.className = syscall.StringToUTF16Ptr("LockScreenWindow")
	}
}

func (l *LockScreenScript) registerWindowClass() bool {
	hInstance := win.GetModuleHandle(nil)
	brushHandle, _, _ := l.procCreateSolidBrush.Call(uintptr(win.RGB(30, 30, 30)))
	brush := win.HBRUSH(brushHandle)

	wc := WNDCLASSEX{
		CbSize:        uint32(unsafe.Sizeof(WNDCLASSEX{})),
		LpfnWndProc:   syscall.NewCallback(l.wndProc),
		HInstance:     hInstance,
		HbrBackground: brush,
		LpszClassName: l.className,
		HCursor:       win.LoadCursor(0, win.MAKEINTRESOURCE(win.IDC_ARROW)),
	}

	ret, _, _ := l.procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	return ret != 0
}

func (l *LockScreenScript) createWindow(screen Screen, index int) win.HWND {
	hInstance := win.GetModuleHandle(nil)

	hwnd, _, _ := l.procCreateWindowEx.Call(
		uintptr(WS_EX_TOPMOST|WS_EX_LAYERED|WS_EX_TOOLWINDOW),
		uintptr(unsafe.Pointer(l.className)),
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(""))),
		uintptr(WS_POPUP),
		uintptr(screen.X),
		uintptr(screen.Y),
		uintptr(screen.Width),
		uintptr(screen.Height),
		0, 0,
		uintptr(hInstance),
		0,
	)

	if hwnd == 0 {
		return 0
	}

	alpha := uint8(242)
	l.procSetLayeredWindowAttributes.Call(hwnd, 0, uintptr(alpha), uintptr(LWA_ALPHA))
	win.ShowWindow(win.HWND(hwnd), win.SW_SHOW)
	win.UpdateWindow(win.HWND(hwnd))

	return win.HWND(hwnd)
}

func (l *LockScreenScript) wndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_PAINT:
		var ps win.PAINTSTRUCT
		hdc := win.BeginPaint(hwnd, &ps)

		hFont, _, _ := l.procCreateFont.Call(
			uintptr(36), 0, 0, 0,
			uintptr(FW_BOLD), 0, 0, 0,
			uintptr(DEFAULT_CHARSET),
			uintptr(OUT_DEFAULT_PRECIS),
			uintptr(CLIP_DEFAULT_PRECIS),
			uintptr(DEFAULT_QUALITY),
			uintptr(DEFAULT_PITCH|FF_DONTCARE),
			uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("Segoe UI"))),
		)

		l.procSelectObject.Call(uintptr(hdc), hFont)
		l.procSetBkMode.Call(uintptr(hdc), uintptr(TRANSPARENT))
		l.procSetTextColor.Call(uintptr(hdc), uintptr(win.RGB(255, 255, 255)))

		var rect win.RECT
		win.GetClientRect(hwnd, &rect)

		l.procDrawText.Call(
			uintptr(hdc),
			uintptr(unsafe.Pointer(l.messageUTF16)),
			uintptr(^uint32(0)),
			uintptr(unsafe.Pointer(&rect)),
			uintptr(DT_CENTER|DT_VCENTER|DT_SINGLELINE),
		)

		win.EndPaint(hwnd, &ps)
		return 0

	case WM_CLOSE, WM_KEYDOWN, WM_SYSKEYDOWN:
		return 0

	case WM_DESTROY:
		return 0
	}

	ret, _, _ := l.procDefWindowProc.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return ret
}

func (l *LockScreenScript) maintenanceLoop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		for _, hwnd := range l.windows {
			if hwnd != 0 {
				win.SetWindowPos(hwnd, win.HWND_TOPMOST, 0, 0, 0, 0,
					win.SWP_NOMOVE|win.SWP_NOSIZE|win.SWP_SHOWWINDOW)
			}
		}
	}
}

func (l *LockScreenScript) playMusic(path string) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return
	}

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	streamer, format, err := wav.Decode(f)
	if err != nil {
		return
	}
	defer streamer.Close()

	err = speaker.Init(format.SampleRate, format.SampleRate.N(time.Second/10))
	if err != nil {
		return
	}

	done := make(chan bool)
	speaker.Play(beep.Seq(streamer, beep.Callback(func() {
		done <- true
	})))

	<-done
}

func (l *LockScreenScript) getScreens() []Screen {
	var screens []Screen

	callback := syscall.NewCallback(func(hMonitor, hdcMonitor, lprcMonitor, dwData uintptr) uintptr {
		var mi MONITORINFO
		mi.CbSize = uint32(unsafe.Sizeof(mi))

		ret, _, _ := l.procGetMonitorInfo.Call(hMonitor, uintptr(unsafe.Pointer(&mi)))
		if ret != 0 {
			screens = append(screens, Screen{
				X:      int(mi.RcMonitor.Left),
				Y:      int(mi.RcMonitor.Top),
				Width:  int(mi.RcMonitor.Right - mi.RcMonitor.Left),
				Height: int(mi.RcMonitor.Bottom - mi.RcMonitor.Top),
			})
		}
		return 1
	})

	l.procEnumDisplayMonitors.Call(0, 0, callback, 0)
	return screens
}
