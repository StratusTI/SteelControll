// obs_record.go
package scripts

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
	"context"
	"sync"

	"github.com/gorilla/websocket"
)

// -----------------------------
// Config
// -----------------------------
const (
	obsDefaultPath     = `C:\ProgramData\Microsoft\Windows\Start Menu\Programs\OBS Studio.lnk`
	obsDefaultHost     = "ws://localhost:4455"
	obsDefaultPassword = "M7a64tl0"
	obsStartupWait     = 15 * time.Second
	obsShutdownWait    = 3 * time.Second
)

// -----------------------------
// OBS WebSocket Structures
// -----------------------------
type HelloMessage struct {
	Op int `json:"op"`
	D  struct {
		ObsWebSocketVersion string `json:"obsWebSocketVersion"`
		RpcVersion          int    `json:"rpcVersion"`
		Authentication      struct {
			Challenge string `json:"challenge"`
			Salt      string `json:"salt"`
		} `json:"authentication"`
	} `json:"d"`
}

type IdentifyMessage struct {
	Op int `json:"op"`
	D  struct {
		RpcVersion         int    `json:"rpcVersion"`
		Authentication     string `json:"authentication"`
		EventSubscriptions int    `json:"eventSubscriptions"`
	} `json:"d"`
}

type RequestMessage struct {
	Op int `json:"op"`
	D  struct {
		RequestType string                 `json:"requestType"`
		RequestId   string                 `json:"requestId"`
		RequestData map[string]interface{} `json:"requestData,omitempty"`
	} `json:"d"`
}

type ResponseMessage struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
}

// -----------------------------
// OBS Client
// -----------------------------
type OBSClient struct {
	conn       *websocket.Conn
	password   string
	host       string
	obsProcess *exec.Cmd
	ctx        context.Context
	cancel     context.CancelFunc
}

func NewOBSClient(host, password string) *OBSClient {
	ctx, cancel := context.WithCancel(context.Background())
	return &OBSClient{
		host:     host,
		password: password,
		ctx:      ctx,
		cancel:   cancel,
	}
}

func (c *OBSClient) CancelRecording() {
	if c.cancel != nil {
		c.cancel()
	}
}

func (c *OBSClient) StartOBS(obsPath string) error {
	fmt.Println("🚀 Iniciando OBS Studio...")

	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "windows":
		if len(obsPath) > 4 && obsPath[len(obsPath)-4:] == ".lnk" {
			cmd = exec.Command("cmd.exe", "/c", "start", "", obsPath)
		} else {
			cmd = exec.Command(obsPath, "--minimize", "--disable-shutdown-check", "--startminimized")
			cmd.SysProcAttr = &syscall.SysProcAttr{
				HideWindow: true,
			}
		}
	case "linux":
		cmd = exec.Command(obsPath, "--minimize", "--disable-shutdown-check")
	case "darwin":
		cmd = exec.Command("open", "-a", obsPath, "--args", "--minimize", "--disable-shutdown-check")
	default:
		return fmt.Errorf("sistema operacional não suportado: %s", runtime.GOOS)
	}

	err := cmd.Start()
	if err != nil {
		return fmt.Errorf("erro ao iniciar OBS: %w", err)
	}

	c.obsProcess = cmd
	fmt.Println("✓ OBS iniciado (minimizado)")
	fmt.Printf("⏳ Aguardando OBS inicializar (%v)...\n", obsStartupWait)
	time.Sleep(obsStartupWait)

	return nil
}

func (c *OBSClient) CloseOBS() error {
	if c.conn == nil && c.obsProcess == nil {
		return nil
	}

	fmt.Println("🔴 Fechando OBS Studio graciosamente...")

	if c.conn != nil {
		c.SendRequest("Sleep", "pre-exit-sleep", map[string]interface{}{
			"sleepMillis": 500,
		})
		time.Sleep(500 * time.Millisecond)
	}

	time.Sleep(2 * time.Second)

	if c.obsProcess != nil && c.obsProcess.Process != nil {
		if runtime.GOOS == "windows" {
			exec.Command("taskkill", "/IM", "obs64.exe", "/T").Run()
			time.Sleep(2 * time.Second)
			exec.Command("taskkill", "/F", "/IM", "obs64.exe").Run()
		} else {
			c.obsProcess.Process.Signal(syscall.SIGTERM)
			time.Sleep(2 * time.Second)
			c.obsProcess.Process.Kill()
		}
	}

	fmt.Println("✓ OBS fechado")
	return nil
}

func (c *OBSClient) Connect() error {
	maxRetries := 5
	retryDelay := 2 * time.Second

	for i := 0; i < maxRetries; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(c.host, nil)
		if err == nil {
			c.conn = conn
			fmt.Println("✓ Conectado ao OBS WebSocket")
			return nil
		}

		if i < maxRetries-1 {
			fmt.Printf("⚠ Tentativa %d/%d falhou, tentando novamente...\n", i+1, maxRetries)
			time.Sleep(retryDelay)
		}
	}

	return fmt.Errorf("falha ao conectar após %d tentativas", maxRetries)
}

func (c *OBSClient) Authenticate() error {
	var hello HelloMessage
	err := c.conn.ReadJSON(&hello)
	if err != nil {
		return fmt.Errorf("erro ao ler Hello: %w", err)
	}

	fmt.Printf("✓ Recebido Hello - OBS v%s, RPC v%d\n",
		hello.D.ObsWebSocketVersion, hello.D.RpcVersion)

	challenge := hello.D.Authentication.Challenge
	salt := hello.D.Authentication.Salt

	secretHash := sha256.Sum256([]byte(c.password + salt))
	secret := base64.StdEncoding.EncodeToString(secretHash[:])

	authHash := sha256.Sum256([]byte(secret + challenge))
	authString := base64.StdEncoding.EncodeToString(authHash[:])

	identify := IdentifyMessage{
		Op: 1,
	}
	identify.D.RpcVersion = 1
	identify.D.Authentication = authString
	identify.D.EventSubscriptions = 33

	err = c.conn.WriteJSON(identify)
	if err != nil {
		return fmt.Errorf("erro ao enviar Identify: %w", err)
	}

	var response ResponseMessage
	err = c.conn.ReadJSON(&response)
	if err != nil {
		return fmt.Errorf("erro ao ler resposta: %w", err)
	}

	if response.Op == 2 {
		fmt.Println("✓ Autenticação bem-sucedida!")
		return nil
	}

	return fmt.Errorf("falha na autenticação - op: %d", response.Op)
}

func (c *OBSClient) SendRequest(requestType, requestId string, data map[string]interface{}) (json.RawMessage, error) {
	req := RequestMessage{
		Op: 6,
	}
	req.D.RequestType = requestType
	req.D.RequestId = requestId
	req.D.RequestData = data

	err := c.conn.WriteJSON(req)
	if err != nil {
		return nil, fmt.Errorf("erro ao enviar request: %w", err)
	}

	var response ResponseMessage
	err = c.conn.ReadJSON(&response)
	if err != nil {
		return nil, fmt.Errorf("erro ao ler resposta: %w", err)
	}

	return response.D, nil
}

func (c *OBSClient) StartRecording() error {
	fmt.Println("▶ Iniciando gravação...")
	_, err := c.SendRequest("StartRecord", "start-rec", nil)
	if err != nil {
		return err
	}
	fmt.Println("✓ Gravação iniciada!")
	return nil
}

func (c *OBSClient) StopRecording() error {
	fmt.Println("■ Parando gravação...")
	_, err := c.SendRequest("StopRecord", "stop-rec", nil)
	if err != nil {
		return err
	}
	fmt.Println("✓ Gravação parada!")
	return nil
}

func (c *OBSClient) GetRecordingStatus() (bool, error) {
	resp, err := c.SendRequest("GetRecordStatus", "get-rec-status", nil)
	if err != nil {
		return false, err
	}

	var status struct {
		RequestStatus struct {
			Result bool `json:"result"`
		} `json:"requestStatus"`
		ResponseData struct {
			OutputActive bool `json:"outputActive"`
		} `json:"responseData"`
	}

	err = json.Unmarshal(resp, &status)
	if err != nil {
		return false, err
	}

	return status.ResponseData.OutputActive, nil
}

func (c *OBSClient) GetLastRecordingPath() (string, error) {
	resp, err := c.SendRequest("GetLastReplayBufferReplay", "get-last-recording", nil)
	if err != nil {
		// Tentar método alternativo
		resp, err = c.SendRequest("GetRecordDirectory", "get-rec-dir", nil)
		if err != nil {
			return "", err
		}
	}

	var result struct {
		ResponseData struct {
			SavedReplayPath string `json:"savedReplayPath"`
			RecordDirectory string `json:"recordDirectory"`
		} `json:"responseData"`
	}

	err = json.Unmarshal(resp, &result)
	if err != nil {
		return "", err
	}

	if result.ResponseData.SavedReplayPath != "" {
		return result.ResponseData.SavedReplayPath, nil
	}

	return result.ResponseData.RecordDirectory, nil
}

func (c *OBSClient) Close() {
	if c.conn != nil {
		c.conn.Close()
		fmt.Println("✓ Conexão WebSocket fechada")
	}
}

func (c *OBSClient) RecordForDuration(duration time.Duration) error {
	isRecording, err := c.GetRecordingStatus()
	if err != nil {
		return fmt.Errorf("erro ao verificar status: %w", err)
	}

	if isRecording {
		fmt.Println("⚠ Gravação já em andamento, parando primeiro...")
		c.StopRecording()
		time.Sleep(2 * time.Second)
	}

	err = c.StartRecording()
	if err != nil {
		return err
	}

	if duration == 0 {
		fmt.Println("♾️ Gravação contínua iniciada. Aguardando sinal de parada...")

		// ← CORREÇÃO CRÍTICA: Aguarda cancelamento via context
		<-c.ctx.Done()

		fmt.Println("🛑 Sinal de parada recebido, finalizando gravação...")
		err = c.StopRecording()
		if err != nil {
			return err
		}
	} else {
		fmt.Printf("⏱ Gravando por %v...\n", duration)

		// ← CORREÇÃO: Interruptível via context
		select {
		case <-time.After(duration):
			// Tempo normal esgotado
		case <-c.ctx.Done():
			// Cancelamento antecipado
			fmt.Println("⚠ Gravação cancelada antes do término")
		}

		err = c.StopRecording()
		if err != nil {
			return err
		}
	}

	fmt.Println("💾 Aguardando finalizar salvamento...")
	time.Sleep(3 * time.Second)
	return nil
}

// -----------------------------
// OBSRecordScript implementa a interface Script
// -----------------------------
type OBSRecordScript struct {
	ftpServer string
	ftpUser   string
	ftpPass   string
	obsHost   string
	obsPass   string
	obsPath   string
	activeClients map[string]*OBSClient
	clientsMutex sync.Mutex
}

func NewOBSRecordScript(ftpServer, ftpUser, ftpPass, obsHost, obsPass, obsPath string) *OBSRecordScript {
	if ftpServer == "" {
		ftpServer = "172.20.29.199"
	}
	if ftpUser == "" {
		ftpUser = "steel"
	}
	if ftpPass == "" {
		ftpPass = "M7a64tl0"
	}
	if obsHost == "" {
		obsHost = obsDefaultHost
	}
	if obsPass == "" {
		obsPass = obsDefaultPassword
	}
	if obsPath == "" {
		obsPath = obsDefaultPath
	}

	return &OBSRecordScript{
		ftpServer: ftpServer,
		ftpUser:   ftpUser,
		ftpPass:   ftpPass,
		obsHost:   obsHost,
		obsPass:   obsPass,
		obsPath:   obsPath,
		activeClients: make(map[string]*OBSClient),
	}
}

func (s *OBSRecordScript) Name() string {
	return "obs_record"
}

func (s *OBSRecordScript) StopActiveRecording(clientID string) error {
	s.clientsMutex.Lock()
	defer s.clientsMutex.Unlock()

	if client, exists := s.activeClients[clientID]; exists {
		client.CancelRecording()
		delete(s.activeClients, clientID)
		return nil
	}
	return errors.New("cliente não encontrado")
}

func (s *OBSRecordScript) Execute(args ...string) ([]map[string]interface{}, error) {
	if len(args) == 0 {
		return nil, errors.New("necessário informar a duração em segundos")
	}

	seconds, err := strconv.Atoi(args[0])
	if err != nil {
		return nil, fmt.Errorf("erro ao converter duração: %w", err)
	}

	if seconds < 0 {
		return nil, errors.New("duração deve ser >= 0 (0 = contínuo)")
	}

	// Executar gravação OBS
	data, err := s.recordWithOBS(seconds)
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

func (s *OBSRecordScript) recordWithOBS(seconds int) (*ScreenRecordData, error) {
	hostname, _ := os.Hostname()
	username := os.Getenv("USERNAME")
	if username == "" {
		username = os.Getenv("USER")
	}

	fmt.Println("\n╔══════════════════════════════════════╗")
	fmt.Println("║   GRAVAÇÃO COM OBS STUDIO            ║")
	fmt.Println("╚══════════════════════════════════════╝")

	// Criar cliente OBS
	client := NewOBSClient(s.obsHost, s.obsPass)

	clientID := fmt.Sprintf("%s_%d", username, time.Now().Unix())
	if seconds == 0 {
		s.clientsMutex.Lock()
		s.activeClients[clientID] = client
		s.clientsMutex.Unlock()
	}

	defer func() {
		client.Close()
		client.CloseOBS()

		s.clientsMutex.Lock()
		delete(s.activeClients, clientID)
		s.clientsMutex.Unlock()
	}()

	// Iniciar OBS
	err := client.StartOBS(s.obsPath)
	if err != nil {
		return nil, fmt.Errorf("erro ao iniciar OBS: %w", err)
	}

	// Conectar ao WebSocket
	err = client.Connect()
	if err != nil {
		return nil, fmt.Errorf("erro ao conectar WebSocket: %w", err)
	}

	// Autenticar
	err = client.Authenticate()
	if err != nil {
		return nil, fmt.Errorf("erro ao autenticar: %w", err)
	}

	// Gravar por duração especificada
	fmt.Printf("⏱️  Duração: %d segundos\n", seconds)
	fmt.Println("╚══════════════════════════════════════╝\n")

	err = client.RecordForDuration(time.Duration(seconds) * time.Second)
	if err != nil {
		return nil, fmt.Errorf("erro durante gravação: %w", err)
	}

	// Localizar arquivo de vídeo mais recente
	videoPath, err := s.findLatestRecording()
	if err != nil {
		return nil, fmt.Errorf("erro ao localizar gravação: %w", err)
	}

	fmt.Printf("📹 Vídeo encontrado: %s\n", videoPath)

	// Confirmar arquivo existe
	stat, err := os.Stat(videoPath)
	if err != nil {
		return nil, fmt.Errorf("arquivo de vídeo não encontrado: %w", err)
	}

	// Gerar timestamp e thumbnail
	timestamp := time.Now().Format("2006-01-02_15-04-05")
	thumbPath := filepath.Join(tempFolder, fmt.Sprintf("obs_record_%s_thumb.png", timestamp))

	// Gerar thumbnail usando FFmpeg (se disponível)
	if fileExists(ffmpegExePath) {
		fmt.Println("📸 Gerando miniatura...")
		thumbTime := 1
		if seconds <= 2 {
			thumbTime = seconds / 2
		}
		thumbArgs := []string{"-y", "-ss", fmt.Sprint(thumbTime), "-i", videoPath, "-frames:v", "1", "-vf", "scale=iw*0.3:ih*0.3", thumbPath}
		cmd := exec.Command(ffmpegExePath, thumbArgs...)
		cmd.Stdout = nil
		cmd.Stderr = nil
		_ = cmd.Run()
	}

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
		videoRemotePath = fmt.Sprintf("/ftp/%s/obs_record_%s.mp4", hostname, finalTimestamp)
		thumbRemotePath = fmt.Sprintf("/ftp/%s/obs_record_%s_thumb.png", hostname, finalTimestamp)
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
		FFmpegVersion:         "OBS-Studio",
		RecordedCamera:        false,
		RecordedMicro:         false,
		RecordedApp:           "OBS-Configured",
		AudioCapabilities:     "OBS-Managed",
	}

	// Remover arquivos locais se upload ok
	if uploadSuccess {
		_ = os.Remove(videoPath)
		_ = os.Remove(thumbPath)
	}

	return data, nil
}

func (s *OBSRecordScript) findLatestRecording() (string, error) {
	// Possíveis diretórios onde OBS salva gravações
	possibleDirs := []string{
		filepath.Join(os.Getenv("USERPROFILE"), "Videos"),
		filepath.Join(os.Getenv("HOME"), "Videos"),
		tempFolder,
		"C:\\Users\\Public\\Videos",
	}

	var latestFile string
	var latestTime time.Time

	for _, dir := range possibleDirs {
		if !fileExists(dir) {
			continue
		}

		files, err := ioutil.ReadDir(dir)
		if err != nil {
			continue
		}

		for _, file := range files {
			if file.IsDir() {
				continue
			}

			// Verificar extensões de vídeo
			ext := filepath.Ext(file.Name())
			if ext != ".mp4" && ext != ".mkv" && ext != ".flv" {
				continue
			}

			// Verificar se é mais recente
			if file.ModTime().After(latestTime) {
				latestTime = file.ModTime()
				latestFile = filepath.Join(dir, file.Name())
			}
		}
	}

	if latestFile == "" {
		return "", errors.New("nenhuma gravação encontrada")
	}

	// Verificar se arquivo foi modificado recentemente (últimos 60 segundos)
	if time.Since(latestTime) > 60*time.Second {
		return "", errors.New("nenhuma gravação recente encontrada")
	}

	return latestFile, nil
}
