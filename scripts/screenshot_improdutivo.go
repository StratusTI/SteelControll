// screenshot_improdutivo.go
package scripts

import (
	"bufio"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kbinani/screenshot"
	"golang.org/x/image/draw"
)

// -----------------------------
// Tipos
// -----------------------------

// ScreenshotImprodutivoData representa o resultado da captura de tela improdutiva
type ScreenshotImprodutivoData struct {
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

// ScreenshotImprodutivoScript implementa a interface Script
type ScreenshotImprodutivoScript struct {
	ftpServer   string
	ftpUser     string
	ftpPass     string
	localTemp   string
	jpegQuality int
	thumbScale  float64
}

// NewScreenshotImprodutivoScript cria uma nova instância com configurações
func NewScreenshotImprodutivoScript(ftpServer, ftpUser, ftpPass string) *ScreenshotImprodutivoScript {
	if ftpServer == "" {
		ftpServer = "172.20.29.194"
	}
	if ftpUser == "" {
		ftpUser = "steel"
	}
	if ftpPass == "" {
		ftpPass = "M7a64tl0"
	}

	return &ScreenshotImprodutivoScript{
		ftpServer:   ftpServer,
		ftpUser:     ftpUser,
		ftpPass:     ftpPass,
		localTemp:   `C:\temp`,
		jpegQuality: 85,
		thumbScale:  0.3,
	}
}

func (s *ScreenshotImprodutivoScript) Name() string {
	return "screenshot_improdutivo"
}

func (s *ScreenshotImprodutivoScript) Execute(args ...string) ([]map[string]interface{}, error) {
	// Executar captura de tela improdutiva
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

func (s *ScreenshotImprodutivoScript) captureScreen() (*ScreenshotImprodutivoData, error) {
	// Obter informações do sistema
	hostname, _ := os.Hostname()
	username := os.Getenv("USERNAME")
	if username == "" {
		username = os.Getenv("USER")
	}

	// Preparar caminhos
	if err := os.MkdirAll(s.localTemp, 0755); err != nil {
		return nil, fmt.Errorf("erro criando temp: %w", err)
	}

	timestamp := time.Now().Format("2006-01-02_15-04-05")
	screenshotPath := filepath.Join(s.localTemp, fmt.Sprintf("screenshot_improdutivo_%s.jpg", timestamp))
	screenshotThumbPath := filepath.Join(s.localTemp, fmt.Sprintf("screenshot_improdutivo_%s_thumb.jpg", timestamp))

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
	if err := saveJpegWithQualityImprodutivo(screenshotPath, img, s.jpegQuality); err != nil {
		return nil, fmt.Errorf("erro salvando screenshot: %w", err)
	}

	// Criar thumbnail
	thumb := scaleImageImprodutivo(img, s.thumbScale)
	_ = saveJpegWithQualityImprodutivo(screenshotThumbPath, thumb, s.jpegQuality)

	// FTP: criar diretórios
	ftpMainDir := "/ftp/"
	ftpHostDir := fmt.Sprintf("/ftp/%s/", hostname)

	_ = ftpMakeDirIfNotExistsImprodutivo(s.ftpServer, s.ftpUser, s.ftpPass, ftpMainDir)
	_ = ftpMakeDirIfNotExistsImprodutivo(s.ftpServer, s.ftpUser, s.ftpPass, ftpHostDir)

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
	err1 := ftpUploadFileImprodutivo(s.ftpServer, s.ftpUser, s.ftpPass, ftpScreenshotRemote, screenshotBytes)

	err2 := error(nil)
	if len(thumbBytes) > 0 {
		err2 = ftpUploadFileImprodutivo(s.ftpServer, s.ftpUser, s.ftpPass, ftpThumbRemote, thumbBytes)
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
	data := &ScreenshotImprodutivoData{
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
// FTP minimal client (PASV)
// -----------------------------
type ftpClientImprodutivo struct {
	conn   net.Conn
	reader *bufio.Reader
	user   string
	pass   string
}

func newFtpClientImprodutivo(host string, user, pass string) (*ftpClientImprodutivo, error) {
	var conn net.Conn
	var err error
	maxRetries := 3
	timeout := 30 * time.Second

	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			time.Sleep(2 * time.Second)
		}
		conn, err = net.DialTimeout("tcp", host+":21", timeout)
		if err == nil {
			break
		}
	}

	if err != nil {
		return nil, fmt.Errorf("falha após %d tentativas: %w", maxRetries, err)
	}

	c := &ftpClientImprodutivo{
		conn:   conn,
		reader: bufio.NewReader(conn),
		user:   user,
		pass:   pass,
	}

	if _, err := c.readLine(); err != nil {
		c.conn.Close()
		return nil, err
	}

	if err := c.sendExpect("USER "+user, "331"); err != nil {
		if !strings.HasPrefix(err.Error(), "230") {
			c.conn.Close()
			return nil, fmt.Errorf("USER failed: %w", err)
		}
	}

	if err := c.sendExpect("PASS "+pass, "230"); err != nil {
		if !strings.HasPrefix(err.Error(), "230") {
			c.conn.Close()
			return nil, fmt.Errorf("PASS failed: %w", err)
		}
	}

	return c, nil
}

func (c *ftpClientImprodutivo) readLine() (string, error) {
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (c *ftpClientImprodutivo) writeLine(cmd string) error {
	_, err := c.conn.Write([]byte(cmd + "\r\n"))
	return err
}

func (c *ftpClientImprodutivo) sendExpect(cmd, expectPrefix string) error {
	if err := c.writeLine(cmd); err != nil {
		return err
	}
	line, err := c.readLine()
	if err != nil {
		return err
	}
	if expectPrefix != "" && !strings.HasPrefix(line, expectPrefix) {
		return errors.New(line)
	}
	return nil
}

func (c *ftpClientImprodutivo) makeDir(path string) error {
	if err := c.writeLine("MKD " + path); err != nil {
		return err
	}
	line, err := c.readLine()
	if err != nil {
		return err
	}
	if strings.HasPrefix(line, "257") || strings.HasPrefix(line, "250") || strings.HasPrefix(line, "550") {
		return nil
	}
	return errors.New(line)
}

func (c *ftpClientImprodutivo) size(path string) (int64, error) {
	if err := c.writeLine("SIZE " + path); err != nil {
		return 0, err
	}
	line, err := c.readLine()
	if err != nil {
		return 0, err
	}
	if strings.HasPrefix(line, "213") {
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			return n, nil
		}
	}
	if strings.HasPrefix(line, "550") {
		return 0, errors.New("file not found")
	}
	return 0, errors.New(line)
}

func (c *ftpClientImprodutivo) enterPassive() (string, int, error) {
	if err := c.writeLine("PASV"); err != nil {
		return "", 0, err
	}
	line, err := c.readLine()
	if err != nil {
		return "", 0, err
	}
	start := strings.Index(line, "(")
	end := strings.Index(line, ")")
	if start < 0 || end < 0 {
		return "", 0, fmt.Errorf("unexpected PASV response: %s", line)
	}
	parts := strings.Split(line[start+1:end], ",")
	if len(parts) < 6 {
		return "", 0, fmt.Errorf("unexpected PASV parts: %v", parts)
	}
	host := parts[0] + "." + parts[1] + "." + parts[2] + "." + parts[3]
	p1, _ := strconv.Atoi(parts[4])
	p2, _ := strconv.Atoi(parts[5])
	port := p1*256 + p2
	return host, port, nil
}

func (c *ftpClientImprodutivo) stor(path string, data []byte) error {
	if err := c.writeLine("TYPE I"); err != nil {
		return err
	}
	if _, err := c.readLine(); err != nil {
		return err
	}

	host, port, err := c.enterPassive()
	if err != nil {
		return err
	}

	dataConn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 30*time.Second)
	if err != nil {
		return err
	}
	defer dataConn.Close()

	if err := c.writeLine("STOR " + path); err != nil {
		return err
	}
	if _, err := c.readLine(); err != nil {
		return err
	}

	_, err = dataConn.Write(data)
	if err != nil {
		return err
	}
	dataConn.Close()
	if _, err := c.readLine(); err != nil {
		return err
	}
	return nil
}

func (c *ftpClientImprodutivo) quit() {
	_ = c.writeLine("QUIT")
	c.conn.Close()
}

// High level helpers
func ftpMakeDirIfNotExistsImprodutivo(server, user, pass, path string) error {
	u, err := url.Parse("ftp://" + server + path)
	if err != nil {
		return err
	}
	client, err := newFtpClientImprodutivo(u.Host, user, pass)
	if err != nil {
		return err
	}
	defer client.quit()
	return client.makeDir(u.Path)
}

func ftpFileExistsImprodutivo(server, user, pass, path string) (bool, error) {
	u, err := url.Parse("ftp://" + server + path)
	if err != nil {
		return false, err
	}
	client, err := newFtpClientImprodutivo(u.Host, user, pass)
	if err != nil {
		return false, err
	}
	defer client.quit()
	_, err = client.size(u.Path)
	if err != nil {
		return false, nil
	}
	return true, nil
}

func ftpUploadFileImprodutivo(server, user, pass, remotePath string, data []byte) error {
	u, err := url.Parse("ftp://" + server + remotePath)
	if err != nil {
		return err
	}
	client, err := newFtpClientImprodutivo(u.Host, user, pass)
	if err != nil {
		return err
	}
	defer client.quit()
	return client.stor(u.Path, data)
}

// -----------------------------
// Utilidades imagem
// -----------------------------
func saveJpegWithQualityImprodutivo(path string, img image.Image, quality int) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	opts := &jpeg.Options{Quality: quality}
	return jpeg.Encode(out, img, opts)
}

func scaleImageImprodutivo(src image.Image, scale float64) image.Image {
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
