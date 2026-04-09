package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"io"
	"io/ioutil"
	"net/http"

	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/debug"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"app-monitor/scripts"

	version "github.com/hashicorp/go-version"
)

const (
	serviceName = "PowerShellDataCollector"
	serviceDesc = "Serviço de coleta de dados através de scripts PowerShell"
)

// Controle de pool de conexões e concorrência
var (
	dbMutex sync.Mutex // Protege operações do banco
)

var communicationApps = []string{
	"teams",
	"zoom",
	"discord",
	"skype",
	"slack",
	"whatsapp",
	"telegram",
	"meet",        // Google Meet
	"chrome",      // Pode ser usado para chamadas web
	"firefox",
	"msedge",
	"webex",
	"gotomeeting",
	"bluejeans",
	"hangouts",
}

var ignoredMicProcesses = []string{
	"msedgewebview2",
	"msedgewebview1",
	"msedgewebview",
	"obs64",
	"obs32",
	"streamlabs",
}

// Semaphore para controlar execuções paralelas
type Semaphore struct {
	ch chan struct{}
}

func NewSemaphore(max int) *Semaphore {
	return &Semaphore{
		ch: make(chan struct{}, max),
	}
}

func (s *Semaphore) Acquire() {
	s.ch <- struct{}{}
}

func (s *Semaphore) Release() {
	<-s.ch
}

// Configuração do banco de dados
type DBConfig struct {
	Driver   string `json:"driver"` // "mysql" ou "postgres"
	Host     string `json:"host"`
	Port     string `json:"port"`
	Database string `json:"database"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// Configuração do serviço
type ServiceConfig struct {
	DB                             DBConfig `json:"database"`
	ScriptsPath                    string   `json:"scripts_path"`
	LogPath                        string   `json:"log_path"`
	CheckIntervalMinutes           int      `json:"check_interval_minutes"`
	PeriodicScriptsIntervalMinutes int      `json:"periodic_scripts_interval_minutes"` // NOVO
	FtpServer                      string   `json:"ftpServer"`
	FtpUsername                    string   `json:"ftpUsername"`
	FtpPassword                    string   `json:"ftpPassword"`
	OBSWebSocketHost               string   `json:"obs_websocket_host"`     // NOVO
	OBSWebSocketPassword           string   `json:"obs_websocket_password"` // NOVO
	OBSExecutablePath              string   `json:"obs_executable_path"`    // NOVO
}

type ScriptTracker struct {
	LastExecution map[string]time.Time
	mu            sync.RWMutex
}

type service struct {
	config        *ServiceConfig
	db            *sql.DB
	logger        *log.Logger
	machineID     string
	scripts       map[string]scripts.Script
	scriptTracker *ScriptTracker // NOVO
}

// PowerShell Script resultado
type ScriptResult struct {
	ScriptName string                   `json:"script_name"`
	Data       []map[string]interface{} `json:"data"`
	Error      string                   `json:"error,omitempty"`
	Timestamp  time.Time                `json:"timestamp"`
}

// Estrutura para dados da máquina
type MachineData struct {
	Hostname   string `json:"hostname"`
	IPAddress  string `json:"ip_address"`
	MACAddress string `json:"mac_address"`
	DomainName string `json:"domain_name"`
	Status     string `json:"status"`
}

// Estrutura para dados do hardware
type HardwareData struct {
	ComponentType  string                 `json:"component_type"`
	Manufacturer   string                 `json:"manufacturer"`
	Model          string                 `json:"model"`
	SerialNumber   string                 `json:"serial_number"`
	Capacity       string                 `json:"capacity"`
	AdditionalInfo map[string]interface{} `json:"additional_info"`
}

type SoftwareData struct {
	SoftwareName string `json:"software_name"`
	Version      string `json:"version"`
	Publisher    string `json:"publisher"`
	InstallDate  string `json:"install_date"`
	InstallPath  string `json:"install_path"`
	SizeMB       int    `json:"size_mb"`
	SoftwareType string `json:"software_type"`
}

type MachineConfig struct {
	Active                   bool   `json:"active"`
	Type                     string `json:"type"`
	PictureTime              int    `json:"picture_time"`
	RecordTime               int    `json:"record_time"`
	RecordingTiming          int    `json:"recording_timing"`
	RecordCamera             bool   `json:"record_camera"`              // NOVO
	RecordMicro              bool   `json:"record_micro"`               // NOVO
	RecordApp                string `json:"record_app"`                 // NOVO
	ActiveNetworkConnections int    `json:"active_network_connections"` // NOVO
}

type CargaHoraria struct {
	DiaSemana       string
	HorarioInicio   time.Time
	HorarioFim      time.Time
	IntervaloInicio *time.Time
	IntervaloFim    *time.Time
}

type HoraExtra struct {
	ID            string
	DataLiberacao time.Time
	HorarioInicio time.Time
	HorarioFim    time.Time
}

type AppImprodutivo struct {
	ApplicationName string
}

// generateUUID gera um novo UUID v4
func generateUUID() string {
	return uuid.New().String()
}

type HorasTrabalhadasDia struct {
	TotalSegundos  int64
	LimiteSegundos int64
}

type CaptureManager struct {
	service  *service
	stopChan chan struct{}

	// Controles de screenshot
	screenshotTicker *time.Ticker
	screenshotStop   chan struct{}
	screenshotActive bool

	// Controles de gravação
	recordingTicker *time.Ticker
	recordingStop   chan struct{}
	recordingActive bool

	// Última configuração conhecida
	lastConfig *MachineConfig
}

type MicrophoneMonitor struct {
	service           *service
	stopChan          chan struct{}
	isRecording       bool
	recordingStopChan chan struct{}
	mu                sync.Mutex
	activeClientID    string
	lastMicState      bool
}

func main() {
	// Verifica se está rodando como serviço
	isIntSess, err := svc.IsAnInteractiveSession()
	if err != nil {
		log.Fatalf("Erro ao verificar se é sessão interativa: %v", err)
	}

	if !isIntSess {
		runService(serviceName, false)
		return
	}

	// Comandos para instalação/remoção do serviço
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "install":
			err := installService()
			if err != nil {
				log.Fatalf("Erro ao instalar serviço: %v", err)
			}
			fmt.Println("Serviço instalado com sucesso!")
		case "remove":
			err := removeService()
			if err != nil {
				log.Fatalf("Erro ao remover serviço: %v", err)
			}
			fmt.Println("Serviço removido com sucesso!")
		case "start":
			err := startService()
			if err != nil {
				log.Fatalf("Erro ao iniciar serviço: %v", err)
			}
			fmt.Println("Serviço iniciado com sucesso!")
		case "stop":
			err := stopService()
			if err != nil {
				log.Fatalf("Erro ao parar serviço: %v", err)
			}
			fmt.Println("Serviço parado com sucesso!")
		case "run":
			runDirectly()
		default:
			fmt.Printf("Uso: %s [install|remove|start|stop|run]\n", os.Args[0])
		}
		return
	}

	fmt.Println("Nenhum argumento fornecido. Executando em modo debug...")
	fmt.Println("Pressione Ctrl+C para sair")
	runDirectly()
}

func loadConfig() (*ServiceConfig, error) {
	exePath, err := os.Executable()
	if err != nil {
		return nil, err
	}

	configPath := filepath.Join(filepath.Dir(exePath), "config.json")

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		defaultConfig := &ServiceConfig{
			DB: DBConfig{
				Driver:   "mysql",
				Host:     "localhost",
				Port:     "3306",
				Database: "produtividade",
				Username: "root",
				Password: "M7a64tl0",
			},
			ScriptsPath:                    filepath.Join(filepath.Dir(exePath), "scripts"),
			LogPath:                        filepath.Join(filepath.Dir(exePath), "service.log"),
			CheckIntervalMinutes:           60,
			PeriodicScriptsIntervalMinutes: 15, // Scripts periódicos a cada 15 min
			FtpServer:                      "localhost",
			FtpUsername:                    "steel",
			FtpPassword:                    "M7a64tl0",
		}

		configData, err := json.MarshalIndent(defaultConfig, "", "  ")
		if err != nil {
			return nil, err
		}

		if err := os.WriteFile(configPath, configData, 0644); err != nil {
			return nil, err
		}

		return defaultConfig, nil
	}

	configData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var config ServiceConfig
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, err
	}

	// Define valores padrão se não especificados
	if config.CheckIntervalMinutes == 0 {
		config.CheckIntervalMinutes = 60
	}
	if config.PeriodicScriptsIntervalMinutes == 0 {
		config.PeriodicScriptsIntervalMinutes = 15
	}
	if config.ScriptsPath == "" {
		config.ScriptsPath = filepath.Join(filepath.Dir(exePath), "scripts")
	}
	if config.LogPath == "" {
		config.LogPath = filepath.Join(filepath.Dir(exePath), "service.log")
	}
	if config.OBSWebSocketHost == "" {
		config.OBSWebSocketHost = "ws://localhost:4455"
	}
	if config.OBSWebSocketPassword == "" {
		config.OBSWebSocketPassword = "M7a64tl0"
	}
	if config.OBSExecutablePath == "" {
		config.OBSExecutablePath = `C:\ProgramData\Microsoft\Windows\Start Menu\Programs\OBS Studio.lnk`
	}

	return &config, nil
}

// getMachineIDFilePath retorna o caminho para o arquivo machine_id.txt
func getMachineIDFilePath() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exePath), "machine_id.txt"), nil
}

// loadMachineIDFromFile carrega o machine_id do arquivo local
func (s *service) loadMachineIDFromFile() (string, error) {
	path, err := getMachineIDFilePath()
	if err != nil {
		return "", err
	}

	data, err := ioutil.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // Arquivo não existe
		}
		return "", err
	}

	machineID := strings.TrimSpace(string(data))
	s.logger.Printf("Machine ID carregado do arquivo: %s", machineID)
	return machineID, nil
}

// saveMachineIDToFile salva o machine_id no arquivo local
func (s *service) saveMachineIDToFile(machineID string) error {
	path, err := getMachineIDFilePath()
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(path, []byte(machineID), 0644)
	if err != nil {
		return err
	}

	s.logger.Printf("Machine ID salvo no arquivo: %s", machineID)
	return nil
}

// initializeMachineID inicializa o machine_id (carrega do arquivo ou busca/cria no banco)
func (s *service) initializeMachineID() error {
	// Primeiro tenta carregar do arquivo local
	machineID, err := s.loadMachineIDFromFile()
	if err != nil {
		return fmt.Errorf("erro ao carregar machine_id do arquivo: %v", err)
	}

	if machineID != "" {
		// Verifica se o machine_id ainda existe no banco
		exists, err := s.machineIDExistsInDB(machineID)
		if err != nil {
			return fmt.Errorf("erro ao verificar machine_id no banco: %v", err)
		}

		if exists {
			s.machineID = machineID
			s.logger.Printf("Machine ID válido encontrado no arquivo: %s", machineID)
			return nil
		} else {
			s.logger.Printf("Machine ID do arquivo não existe mais no banco: %s. Buscando por hostname...", machineID)
		}
	}

	// Se não tem machine_id válido no arquivo, busca por hostname
	hostname := os.Getenv("COMPUTERNAME")
	if hostname == "" {
		return fmt.Errorf("não foi possível obter o hostname da máquina")
	}

	hostname = strings.ToUpper(strings.TrimSpace(hostname))
	s.logger.Printf("Buscando machine_id por hostname: %s", hostname)

	machineID, err = s.getMachineIDByHostname(hostname)
	if err != nil {
		return fmt.Errorf("erro ao buscar machine_id por hostname: %v", err)
	}

	if machineID == "" {
		s.logger.Printf("Máquina %s não encontrada no banco. Será criada quando os dados forem processados.", hostname)
		return nil
	}

	// Salva o machine_id encontrado no arquivo
	if err := s.saveMachineIDToFile(machineID); err != nil {
		s.logger.Printf("Erro ao salvar machine_id no arquivo: %v", err)
		// Não é erro fatal, continua com o machine_id em memória
	}

	s.machineID = machineID
	s.logger.Printf("Machine ID inicializado: %s", machineID)
	return nil
}

// machineIDExistsInDB verifica se o machine_id existe no banco de dados
func (s *service) machineIDExistsInDB(machineID string) (bool, error) {
	var count int
	query := "SELECT COUNT(*) FROM machines WHERE id = ?"

	if s.config.DB.Driver == "postgres" {
		query = "SELECT COUNT(*) FROM machines WHERE id = $1"
	}

	err := s.db.QueryRow(query, machineID).Scan(&count)
	if err != nil {
		return false, err
	}

	return count > 0, nil
}

// getCurrentMachineID retorna o machine_id atual (do cache ou busca/cria)
func (s *service) getCurrentMachineID() (string, error) {
	s.logger.Printf("DEBUG: getCurrentMachineID iniciado")

	// Se já tem em cache, retorna
	if s.machineID != "" {
		s.logger.Printf("DEBUG: Machine ID encontrado no cache: %s", s.machineID)
		return s.machineID, nil
	}

	s.logger.Printf("DEBUG: Machine ID não está em cache, buscando por hostname")

	// Busca por hostname
	hostname := os.Getenv("COMPUTERNAME")
	if hostname == "" {
		s.logger.Printf("DEBUG: ERRO - Não foi possível obter hostname da máquina")
		return "", fmt.Errorf("não foi possível obter o hostname da máquina")
	}

	hostname = strings.ToUpper(strings.TrimSpace(hostname))
	s.logger.Printf("DEBUG: Hostname obtido: '%s'", hostname)

	machineID, err := s.getMachineIDByHostname(hostname)
	if err != nil {
		s.logger.Printf("DEBUG: Erro ao buscar machine_id por hostname: %v", err)
		return "", fmt.Errorf("erro ao buscar machine_id por hostname: %v", err)
	}

	s.logger.Printf("DEBUG: MachineID retornado pela busca: '%s'", machineID)

	if machineID != "" {
		s.machineID = machineID
		s.logger.Printf("DEBUG: Machine ID definido no cache: %s", s.machineID)

		// Salva no arquivo para próximas execuções
		if err := s.saveMachineIDToFile(machineID); err != nil {
			s.logger.Printf("DEBUG: Erro ao salvar machine_id no arquivo: %v", err)
		} else {
			s.logger.Printf("DEBUG: Machine ID salvo no arquivo com sucesso")
		}
	} else {
		s.logger.Printf("DEBUG: Machine ID vazio - máquina não existe no banco ainda")
	}

	return machineID, nil
}

func runService(name string, isDebug bool) {
	var err error
	if isDebug {
		elog := debug.New(name)
		err = svc.Run(name, &service{})
		if err != nil {
			elog.Error(1, fmt.Sprintf("Erro ao executar serviço: %v", err))
		}
	} else {
		elog, err := eventlog.Open(name)
		if err != nil {
			return
		}
		defer elog.Close()

		elog.Info(1, fmt.Sprintf("Iniciando serviço %s", name))
		err = svc.Run(name, &service{})
		if err != nil {
			elog.Error(1, fmt.Sprintf("Erro ao executar serviço: %v", err))
		}
	}
}

func getCurrentVersion() string {
	exePath, err := os.Executable()
	if err != nil {
		return "0.0.0"
	}

	versionPath := filepath.Join(filepath.Dir(exePath), "version.txt")
	data, err := os.ReadFile(versionPath)
	if err != nil {
		return "0.0.0"
	}
	return strings.TrimSpace(string(data))
}

func saveCurrentVersion(v string) error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}

	versionPath := filepath.Join(filepath.Dir(exePath), "version.txt")
	return os.WriteFile(versionPath, []byte(v), 0644)
}

func (s *service) checkForUpdate() {
	s.logger.Println("=== INICIANDO VERIFICAÇÃO DE ATUALIZAÇÃO ===")

	// Teste de conectividade
	if resp, err := http.Get("https://www.google.com"); err == nil {
		resp.Body.Close()
		s.logger.Println("✓ Conectividade com internet OK")
	} else {
		s.logger.Printf("✗ Sem conectividade: %v", err)
		return
	}

	// Log do diretório atual para debug
	if wd, err := os.Getwd(); err == nil {
		s.logger.Printf("Diretório de trabalho: %s", wd)
	}

	// Log do caminho do executável
	if exePath, err := os.Executable(); err == nil {
		s.logger.Printf("Caminho do executável: %s", exePath)
	}

	url := "https://painel.stratustelecom.com.br/main/produtividade/update.json"
	s.logger.Printf("Acessando URL: %s", url)

	// Cliente HTTP com timeout
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		s.logger.Printf("ERRO ao acessar URL de atualização: %v", err)
		return
	}
	defer resp.Body.Close()

	s.logger.Printf("Status HTTP recebido: %d", resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		s.logger.Printf("ERRO: Status HTTP inválido: %d", resp.StatusCode)
		return
	}

	// Lê e faz log do corpo da resposta para debug
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		s.logger.Printf("ERRO ao ler corpo da resposta: %v", err)
		return
	}

	s.logger.Printf("Resposta recebida: %s", string(bodyBytes))

	var update struct {
		Version string `json:"version"`
		URL     string `json:"url"`
	}

	if err := json.Unmarshal(bodyBytes, &update); err != nil {
		s.logger.Printf("ERRO ao decodificar JSON: %v", err)
		s.logger.Printf("JSON recebido: %s", string(bodyBytes))
		return
	}

	s.logger.Printf("Versão remota encontrada: '%s'", update.Version)
	s.logger.Printf("URL de download: '%s'", update.URL)

	current := getCurrentVersion()
	s.logger.Printf("Versão atual: '%s'", current)

	// Validação das versões
	if update.Version == "" {
		s.logger.Println("ERRO: Versão remota está vazia")
		return
	}

	if update.URL == "" {
		s.logger.Println("ERRO: URL de download está vazia")
		return
	}

	// Comparação de versões com tratamento de erro
	vCurrent, err1 := version.NewVersion(current)
	vNew, err2 := version.NewVersion(update.Version)

	if err1 != nil {
		s.logger.Printf("ERRO ao parsear versão atual '%s': %v", current, err1)
		return
	}

	if err2 != nil {
		s.logger.Printf("ERRO ao parsear versão remota '%s': %v", update.Version, err2)
		return
	}

	s.logger.Printf("Comparando versões: atual=%s, remota=%s", vCurrent.String(), vNew.String())

	if vNew.GreaterThan(vCurrent) {
		s.logger.Printf("🔄 NOVA VERSÃO DISPONÍVEL! %s -> %s", current, update.Version)
		s.logger.Println("Iniciando processo de download e atualização...")
		go s.downloadAndUpdate(update.URL, update.Version)
	} else if vNew.Equal(vCurrent) {
		s.logger.Printf("✅ Sistema está na versão mais recente: %s", current)
	} else {
		s.logger.Printf("ℹ️ Versão local é mais nova que a remota: local=%s, remota=%s", current, update.Version)
	}
}

func (s *service) downloadAndUpdate(downloadURL, newVersion string) {
	s.logger.Printf("=== STARTING UPDATE PROCESS ===")
	s.logger.Printf("Download URL: %s", downloadURL)
	s.logger.Printf("New version: %s", newVersion)

	exePath, err := os.Executable()
	if err != nil {
		s.logger.Printf("ERROR getting executable path: %v", err)
		return
	}

	s.logger.Printf("Current executable: %s", exePath)

	// Verifica se o updater existe
	updaterPath := filepath.Join(filepath.Dir(exePath), "update.exe")
	if _, err := os.Stat(updaterPath); os.IsNotExist(err) {
		s.logger.Printf("ERROR: update.exe not found at %s", updaterPath)
		s.logger.Printf("Please ensure update.exe is in the same directory as the main executable")
		return
	}

	s.logger.Printf("Updater found at: %s", updaterPath)

	// CORREÇÃO: Passa o caminho do executável como argumento
	cmd := exec.Command(updaterPath, exePath)
	cmd.Dir = filepath.Dir(exePath)

	// Configura saída para um arquivo de log do updater
	logFile := filepath.Join(filepath.Dir(exePath), "updater.log")
	if logFileHandle, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
		cmd.Stdout = logFileHandle
		cmd.Stderr = logFileHandle
		defer logFileHandle.Close()
	}

	// IMPORTANTE: Usa Start() em vez de Run() para não bloquear
	if err := cmd.Start(); err != nil {
		s.logger.Printf("ERROR starting updater: %v", err)
		return
	}

	s.logger.Printf("✓ Updater started with PID: %d", cmd.Process.Pid)
	s.logger.Println("✓ Update process initiated! Service will be updated automatically.")
	s.logger.Printf("Check %s for update progress.", logFile)

	// Aguarda um pouco antes de parar o serviço
	time.Sleep(5 * time.Second)

	// CORREÇÃO: Para o serviço corretamente
	// Em vez de chamar stopService(), devemos encerrar o processo atual
	s.logger.Println("Exiting current process to allow update...")

	// Salva a versão atual antes de sair
	if err := saveCurrentVersion(newVersion); err != nil {
		s.logger.Printf("Warning: failed to save version: %v", err)
	}

	// Encerra o processo atual
	exec.Command("update.exe", "/run", newVersion, downloadURL).Start()
}

func (s *service) startBackgroundTasks(stopChan chan struct{}) {
	s.logger.Println("🚀 Iniciando tarefas em background...")

	// 1️⃣ Goroutine para verificação de tela (a cada 30s)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		s.logger.Println("🔍 Goroutine de verificação de tela INICIADA (intervalo: 30s)")

		for {
			select {
			case <-ticker.C:
				func() {
					defer func() {
						if r := recover(); r != nil {
							s.logger.Printf("⚠️ Panic em verificaEBloqueiaTela: %v", r)
						}
					}()
					s.verificaEBloqueiaTela()
				}()
			case <-stopChan:
				s.logger.Println("🛑 Parando goroutine de verificação de tela")
				return
			}
		}
	}()

	// 2️⃣ Goroutine para scripts DIÁRIOS (verifica a cada 1 hora)
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		s.logger.Println("📅 Goroutine de scripts diários INICIADA (intervalo: 1 hora)")

		for {
			select {
			case <-ticker.C:
				s.logger.Println("📅 ===== VERIFICANDO SCRIPTS DIÁRIOS =====")
				func() {
					defer func() {
						if r := recover(); r != nil {
							s.logger.Printf("⚠️ Panic em executeDailyScripts: %v", r)
						}
					}()
					s.executeDailyScripts()
				}()
			case <-stopChan:
				s.logger.Println("🛑 Parando goroutine de scripts diários")
				return
			}
		}
	}()

	// 3️⃣ Goroutine para scripts PERIÓDICOS (intervalo configurável)
	go func() {
		interval := time.Duration(s.config.PeriodicScriptsIntervalMinutes) * time.Minute
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		s.logger.Printf("⏰ Goroutine de scripts periódicos INICIADA (intervalo: %v)", interval)

		for {
			select {
			case <-ticker.C:
				s.logger.Printf("⏰ ===== EXECUTANDO SCRIPTS PERIÓDICOS (intervalo: %v) =====", interval)
				func() {
					defer func() {
						if r := recover(); r != nil {
							s.logger.Printf("⚠️ Panic em executePeriodicScripts: %v", r)
						}
					}()
					s.executePeriodicScripts()
				}()
			case <-stopChan:
				s.logger.Println("🛑 Parando goroutine de scripts periódicos")
				return
			}
		}
	}()

	// 4️⃣ Goroutine para GERENCIAR screenshot/gravação baseado em machine_config
	go func() {
		s.logger.Println("📸 Goroutine de gerenciamento de capturas INICIADA")

		s.logger.Println("⏳ Aguardando 10 segundos para garantir inicialização completa...")
		time.Sleep(10 * time.Second)

		testID, err := s.getCurrentMachineID()
		if err != nil || testID == "" {
			s.logger.Printf("⚠️ AVISO: Machine ID não disponível após inicialização: %v", err)
			s.logger.Println("⏳ Aguardando mais 15 segundos antes de tentar novamente...")
			time.Sleep(15 * time.Second)
		} else {
			s.logger.Printf("✅ Machine ID confirmado: %s", testID)
		}

		captureManager := &CaptureManager{
			service:  s,
			stopChan: make(chan struct{}),
		}

		go func() {
			<-stopChan
			s.logger.Println("🛑 Sinal de parada recebido no gerenciador de capturas")
			close(captureManager.stopChan)
		}()

		s.logger.Println("🎯 Iniciando CaptureManager.Run()...")
		captureManager.Run()
	}()

	// 5️⃣ **NOVO** Goroutine para MONITORAMENTO DE MICROFONE
	go func() {
		s.logger.Println("🎤 Goroutine de monitoramento de microfone INICIADA")

		s.logger.Println("⏳ Aguardando 5 segundos antes de iniciar monitoramento...")
		time.Sleep(5 * time.Second)

		micMonitor := &MicrophoneMonitor{
			service:  s,
			stopChan: make(chan struct{}),
		}

		go func() {
			<-stopChan
			s.logger.Println("🛑 Sinal de parada recebido no monitor de microfone")
			close(micMonitor.stopChan)
		}()

		s.logger.Println("🎤 Iniciando MicrophoneMonitor.Run()...")
		micMonitor.Run()
	}()

	// 6️⃣ Goroutine para verificação de updates (a cada 12 horas)
	go func() {
		ticker := time.NewTicker(12 * time.Hour)
		defer ticker.Stop()
		s.logger.Println("🔄 Goroutine de verificação de updates INICIADA (intervalo: 12 horas)")

		for {
			select {
			case <-ticker.C:
				// go s.checkForUpdate()
			case <-stopChan:
				s.logger.Println("🛑 Parando goroutine de verificação de updates")
				return
			}
		}
	}()

	s.logger.Println("✅ Todas as goroutinas foram iniciadas com sucesso!")
}

// Execute implementa a interface svc.Handler
func (s *service) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending}

	// Inicializa o serviço
	if err := s.initialize(); err != nil {
		s.logError("Erro na inicialização", err)
		changes <- svc.Status{State: svc.StopPending}
		return
	}

	s.resetScriptTracking()

	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	s.logger.Println("╔═══════════════════════════════════════════════╗")
	s.logger.Println("🚀 SERVIÇO INICIADO COM SUCESSO")
	s.logger.Println("╚═══════════════════════════════════════════════╝")

	// 📹 EXECUTA COLETA INICIAL (uma vez na inicialização)
	s.logger.Println("📊 Executando coleta inicial de dados...")
	s.executeDailyScripts()
	s.executePeriodicScripts()
	s.logger.Println("✅ Coleta inicial concluída")

	// Canal para parar todas as goroutines
	stopChan := make(chan struct{})

	// 🔹 INICIA TODAS AS GOROUTINES PERSISTENTES
	s.startBackgroundTasks(stopChan)

	s.logger.Println("📊 Sistema de coleta de dados operacional")

	// ============================================
	// LOOP PRINCIPAL - AGUARDA COMANDOS
	// ============================================
loop:
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s.logger.Println("🛑 Recebido comando de parada")
				close(stopChan)             // Para todas as goroutines
				time.Sleep(2 * time.Second) // Aguarda goroutines finalizarem
				break loop
			default:
				s.logError("Comando não reconhecido", fmt.Errorf("comando: %d", c.Cmd))
			}
		}
	}

	changes <- svc.Status{State: svc.StopPending}
	s.logger.Println("🧹 Limpando recursos...")
	s.cleanup()
	return
}

// ============================================
// FUNÇÃO MODIFICADA: initialize
// Adiciona inicialização dos scripts
// ============================================
func (s *service) initialize() error {
	// Carrega configuração
	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("erro ao carregar configuração: %v", err)
	}
	s.config = config

	// ⭐ INICIALIZAR SCRIPT TRACKER
	s.scriptTracker = &ScriptTracker{
		LastExecution: make(map[string]time.Time),
	}

	// Cria diretório de logs se não existir
	logDir := filepath.Dir(s.config.LogPath)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("erro ao criar diretório de logs: %v", err)
	}

	// Inicializa logger
	logFile, err := os.OpenFile(s.config.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return fmt.Errorf("erro ao abrir arquivo de log: %v", err)
	}
	s.logger = log.New(logFile, "[DataCollector] ", log.LstdFlags|log.Lshortfile)

	// 🔹 INICIALIZA OS SCRIPTS
	s.initializeScripts()

	// Conecta ao banco de dados
	if err := s.connectDB(); err != nil {
		return fmt.Errorf("erro ao conectar ao banco: %v", err)
	}

	// Cria as tabelas necessárias
	if err := s.createTables(); err != nil {
		return fmt.Errorf("erro ao criar tabelas: %v", err)
	}

	// Inicializa o machine_id
	if err := s.initializeMachineID(); err != nil {
		return fmt.Errorf("erro ao inicializar machine_id: %v", err)
	}

	// Sincroniza o funcionário atual
	if err := s.sincronizarFuncionarioAtual(); err != nil {
		s.logger.Printf("⚠️ Aviso ao sincronizar funcionário: %v", err)
	}

	s.logger.Println("Serviço inicializado com sucesso")
	return nil
}

func (s *service) connectDB() error {
	var dsn string
	switch s.config.DB.Driver {
	case "mysql":
		// 🔥 ADICIONADO: Parâmetros importantes para manter conexão viva
		dsn = fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&timeout=10s&readTimeout=30s&writeTimeout=30s&maxAllowedPacket=0",
			s.config.DB.Username,
			s.config.DB.Password,
			s.config.DB.Host,
			s.config.DB.Port,
			s.config.DB.Database)
	case "postgres":
		dsn = fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable connect_timeout=10",
			s.config.DB.Host,
			s.config.DB.Port,
			s.config.DB.Username,
			s.config.DB.Password,
			s.config.DB.Database)
	default:
		return fmt.Errorf("driver de banco não suportado: %s", s.config.DB.Driver)
	}

	var err error
	s.db, err = sql.Open(s.config.DB.Driver, dsn)
	if err != nil {
		return err
	}

	// 🔥 CONFIGURAÇÕES OTIMIZADAS DO POOL
	s.db.SetMaxOpenConns(15)                      // Reduzido para evitar sobrecarga
	s.db.SetMaxIdleConns(5)                       // Conexões idle prontas
	s.db.SetConnMaxLifetime(3 * time.Minute)      // Recria conexões a cada 3min
	s.db.SetConnMaxIdleTime(1 * time.Minute)      // Fecha idle após 1min

	// Testa a conexão
	if err := s.db.Ping(); err != nil {
		return err
	}

	// Log das estatísticas do pool
	stats := s.db.Stats()
	s.logger.Printf("📊 DB Pool configurado: MaxOpen=%d, MaxIdle=%d, OpenConns=%d",
		stats.MaxOpenConnections, stats.Idle, stats.InUse)

	// 🔥 NOVA: Goroutine para monitorar saúde do pool
	go s.monitorDBHealth()

	return nil
}

// ============================================
// NOVA FUNÇÃO: monitorDBHealth
// Monitora saúde do pool e reconecta se necessário
// ============================================
func (s *service) monitorDBHealth() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// Ping para verificar se a conexão está viva
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.db.PingContext(ctx)
		cancel()

		if err != nil {
			s.logger.Printf("⚠️ DB Health Check FALHOU: %v - Tentando reconectar...", err)

			// Tenta reconectar
			if err := s.reconnectDB(); err != nil {
				s.logger.Printf("❌ Falha ao reconectar: %v", err)
			} else {
				s.logger.Println("✅ Reconexão bem-sucedida")
			}
		}

		// Log das estatísticas a cada 5 minutos
		stats := s.db.Stats()
		if stats.OpenConnections > 10 || stats.WaitCount > 0 {
			s.logger.Printf("📊 DB Stats: Open=%d, InUse=%d, Idle=%d, Wait=%d, WaitDuration=%v",
				stats.OpenConnections, stats.InUse, stats.Idle, stats.WaitCount, stats.WaitDuration)
		}
	}
}

// ============================================
// NOVA FUNÇÃO: reconnectDB
// Tenta reconectar ao banco de dados
// ============================================
func (s *service) reconnectDB() error {
	// Fecha conexão atual
	if s.db != nil {
		s.db.Close()
	}

	// Aguarda um pouco antes de reconectar
	time.Sleep(2 * time.Second)

	// Reconecta usando a mesma configuração
	return s.connectDB()
}

// ============================================
// NOVA FUNÇÃO: execWithRetry
// Executa query com retry automático em caso de erro
// ============================================
func (s *service) execWithRetry(query string, args ...interface{}) (sql.Result, error) {
	maxRetries := 3
	var result sql.Result
	var err error

	for i := 0; i < maxRetries; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		result, err = s.db.ExecContext(ctx, query, args...)
		cancel()

		if err == nil {
			return result, nil
		}

		// Verifica se é erro de conexão
		if strings.Contains(err.Error(), "connection") ||
		   strings.Contains(err.Error(), "broken pipe") ||
		   strings.Contains(err.Error(), "forcibly closed") {
			s.logger.Printf("⚠️ Erro de conexão na tentativa %d/%d: %v", i+1, maxRetries, err)

			// Tenta reconectar
			if reconErr := s.reconnectDB(); reconErr != nil {
				s.logger.Printf("❌ Falha ao reconectar: %v", reconErr)
			}

			time.Sleep(time.Duration(i+1) * time.Second) // backoff exponencial
			continue
		}

		// Outro tipo de erro, não tenta novamente
		return result, err
	}

	return result, fmt.Errorf("falha após %d tentativas: %v", maxRetries, err)
}

// ============================================
// NOVA FUNÇÃO: queryWithRetry
// Executa query com retry automático em caso de erro
// ============================================
func (s *service) queryWithRetry(query string, args ...interface{}) (*sql.Rows, error) {
	maxRetries := 3
	var rows *sql.Rows
	var err error

	for i := 0; i < maxRetries; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		rows, err = s.db.QueryContext(ctx, query, args...)
		cancel()

		if err == nil {
			return rows, nil
		}

		// Verifica se é erro de conexão
		if strings.Contains(err.Error(), "connection") ||
		   strings.Contains(err.Error(), "broken pipe") ||
		   strings.Contains(err.Error(), "forcibly closed") {
			s.logger.Printf("⚠️ Erro de conexão na tentativa %d/%d: %v", i+1, maxRetries, err)

			// Tenta reconectar
			if reconErr := s.reconnectDB(); reconErr != nil {
				s.logger.Printf("❌ Falha ao reconectar: %v", reconErr)
			}

			time.Sleep(time.Duration(i+1) * time.Second)
			continue
		}

		// Outro tipo de erro
		return rows, err
	}

	return rows, fmt.Errorf("falha após %d tentativas: %v", maxRetries, err)
}

func (s *service) createTables() error {
	// Cria a tabela machines - SEM auto-geração de UUID
	var createMachinesSQL string
	if s.config.DB.Driver == "mysql" {
		createMachinesSQL = `
		CREATE TABLE IF NOT EXISTS machines (
			id CHAR(36) PRIMARY KEY,
			hostname VARCHAR(255) NOT NULL,
			ip_address VARCHAR(45),
			mac_address VARCHAR(17),
			domain_name VARCHAR(255),
			last_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			status ENUM('active', 'inactive', 'maintenance') DEFAULT 'active',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

			INDEX idx_machines_hostname (hostname),
			INDEX idx_machines_last_seen (last_seen),
			INDEX idx_machines_status (status),
			UNIQUE KEY unique_hostname (hostname)
		) ENGINE=INNODB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	} else {
		// PostgreSQL
		createMachinesSQL = `
		CREATE TABLE IF NOT EXISTS machines (
			id UUID PRIMARY KEY,
			hostname VARCHAR(255) NOT NULL,
			ip_address VARCHAR(45),
			mac_address VARCHAR(17),
			domain_name VARCHAR(255),
			last_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			status VARCHAR(20) DEFAULT 'active' CHECK (status IN ('active', 'inactive', 'maintenance')),
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,

			CONSTRAINT unique_hostname UNIQUE (hostname)
		);

		CREATE INDEX IF NOT EXISTS idx_machines_hostname ON machines (hostname);
		CREATE INDEX IF NOT EXISTS idx_machines_last_seen ON machines (last_seen);
		CREATE INDEX IF NOT EXISTS idx_machines_status ON machines (status);`
	}

	if _, err := s.db.Exec(createMachinesSQL); err != nil {
		return fmt.Errorf("erro ao criar tabela machines: %v", err)
	}

	// Cria a tabela hardware_inventory - SEM auto-geração de UUID
	var createHardwareSQL string
	if s.config.DB.Driver == "mysql" {
		createHardwareSQL = `
		CREATE TABLE IF NOT EXISTS hardware_inventory (
			id CHAR(36) PRIMARY KEY,
			machine_id CHAR(36) NOT NULL,
			component_type ENUM('cpu', 'memory', 'disk', 'gpu', 'motherboard', 'network', 'other') NOT NULL,
			manufacturer VARCHAR(255),
			model VARCHAR(500),
			serial_number VARCHAR(255),
			capacity VARCHAR(100),
			additional_info JSON,
			last_updated TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,

			FOREIGN KEY (machine_id) REFERENCES machines(id) ON DELETE CASCADE,
			UNIQUE KEY uk_hardware (machine_id, component_type, serial_number),
			INDEX idx_hardware_machine (machine_id),
			INDEX idx_hardware_type (component_type)
		) ENGINE=INNODB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	} else {
		// PostgreSQL
		createHardwareSQL = `
		CREATE TABLE IF NOT EXISTS hardware_inventory (
			id UUID PRIMARY KEY,
			machine_id UUID NOT NULL,
			component_type VARCHAR(20) NOT NULL CHECK (component_type IN ('cpu', 'memory', 'disk', 'gpu', 'motherboard', 'network', 'other')),
			manufacturer VARCHAR(255),
			model VARCHAR(500),
			serial_number VARCHAR(255),
			capacity VARCHAR(100),
			additional_info JSONB,
			last_updated TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,

			FOREIGN KEY (machine_id) REFERENCES machines(id) ON DELETE CASCADE,
			CONSTRAINT uk_hardware UNIQUE (machine_id, component_type, serial_number)
		);

		CREATE INDEX IF NOT EXISTS idx_hardware_machine ON hardware_inventory (machine_id);
		CREATE INDEX IF NOT EXISTS idx_hardware_type ON hardware_inventory (component_type);`
	}

	if _, err := s.db.Exec(createHardwareSQL); err != nil {
		return fmt.Errorf("erro ao criar tabela hardware_inventory: %v", err)
	}

	var createSoftwareSQL string
	if s.config.DB.Driver == "mysql" {
		createSoftwareSQL = `
		CREATE TABLE IF NOT EXISTS software_inventory (
			id CHAR(36) PRIMARY KEY,
			machine_id CHAR(36) NOT NULL,
			software_name VARCHAR(500) NOT NULL,
			version VARCHAR(255),
			publisher VARCHAR(255),
			install_date DATE,
			install_path VARCHAR(1000),
			size_mb INT UNSIGNED,
			software_type ENUM('application', 'driver', 'update', 'system') DEFAULT 'application',
			last_updated TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,

			FOREIGN KEY (machine_id) REFERENCES machines(id) ON DELETE CASCADE,
			UNIQUE KEY uk_software (machine_id, software_name, version),
			INDEX idx_software_machine (machine_id),
			INDEX idx_software_type (software_type),
			INDEX idx_software_name (software_name)
		) ENGINE=INNODB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	} else {
		// PostgreSQL
		createSoftwareSQL = `
		CREATE TABLE IF NOT EXISTS software_inventory (
			id UUID PRIMARY KEY,
			machine_id UUID NOT NULL,
			software_name VARCHAR(500) NOT NULL,
			version VARCHAR(255),
			publisher VARCHAR(255),
			install_date DATE,
			install_path VARCHAR(1000),
			size_mb INTEGER,
			software_type VARCHAR(20) DEFAULT 'application' CHECK (software_type IN ('application', 'driver', 'update', 'system')),
			last_updated TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,

			FOREIGN KEY (machine_id) REFERENCES machines(id) ON DELETE CASCADE,
			CONSTRAINT uk_software UNIQUE (machine_id, software_name, version)
		);

		CREATE INDEX IF NOT EXISTS idx_software_machine ON software_inventory (machine_id);
		CREATE INDEX IF NOT EXISTS idx_software_type ON software_inventory (software_type);
		CREATE INDEX IF NOT EXISTS idx_software_name ON software_inventory (software_name);`
	}

	if _, err := s.db.Exec(createSoftwareSQL); err != nil {
		return fmt.Errorf("erro ao criar tabela software_inventory: %v", err)
	}

	return nil
}

// Adicione esta função para buscar a configuração da máquina
func (s *service) getMachineConfig(machineID string) (*MachineConfig, error) {
	query := `SELECT active, type, picture_time, record_time, recording_timing,
	          record_camera, record_micro, active_network_connections
	          FROM machine_config
	          WHERE machine_id = ? AND active = 1
	          LIMIT 1`

	if s.config.DB.Driver == "postgres" {
		query = `SELECT active, type, picture_time, record_time, recording_timing,
		         record_camera, record_micro, record_app, active_network_connections
		         FROM machine_config
		         WHERE machine_id = $1 AND active = true
		         LIMIT 1`
	}

	var cfg MachineConfig
	err := s.db.QueryRow(query, machineID).Scan(
		&cfg.Active,
		&cfg.Type,
		&cfg.PictureTime,
		&cfg.RecordTime,
		&cfg.RecordingTiming,
		&cfg.RecordCamera,
		&cfg.RecordMicro,
		&cfg.ActiveNetworkConnections,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // sem configuração ativa
		}
		return nil, fmt.Errorf("erro ao buscar configuração: %v", err)
	}

	// printar
	s.logger.Printf("Configuração ativa: %v", cfg.Active)

	return &cfg, nil
}

// Função para verificar se a configuração ainda está ativa
func (s *service) isConfigActive(machineID string) bool {
	query := "SELECT COUNT(*) FROM machine_config WHERE machine_id = ? AND active = 1"
	if s.config.DB.Driver == "postgres" {
		query = "SELECT COUNT(*) FROM machine_config WHERE machine_id = $1 AND active = true"
	}

	var count int
	err := s.db.QueryRow(query, machineID).Scan(&count)
	if err != nil {
		s.logger.Printf("Erro ao verificar configuração ativa: %v", err)
		return false
	}

	// printar
	s.logger.Printf("Configuração ativa: %v", count > 0)

	return count > 0
}

// ============================================
// NOVA FUNÇÃO: initializeScripts
// Registra todos os scripts disponíveis
// ============================================
func (s *service) initializeScripts() {
	s.scripts = make(map[string]scripts.Script)

	s.scripts["machines"] = &scripts.MachinesScript{}
	s.scripts["user_activity"] = &scripts.UserActivityScript{}
	s.scripts["application_usage"] = &scripts.ApplicationUsageScript{}
	s.scripts["browser_history"] = &scripts.BrowserHistoryScript{}
	s.scripts["file_downloads"] = &scripts.FileDownloadsScript{}
	s.scripts["antivirus_status"] = &scripts.AntivirusStatusScript{}
	s.scripts["hardware_inventory"] = &scripts.HardwareInventoryScript{}
	s.scripts["login_attempts"] = &scripts.LoginAttemptsScript{}
	s.scripts["software_inventory"] = &scripts.SoftwareInventoryScript{}
	s.scripts["system_logs"] = &scripts.SystemLogsScript{}
	s.scripts["system_metrics"] = &scripts.SystemMetricsScript{}
	s.scripts["user_sessions"] = &scripts.UserSessionsScript{}
	s.scripts["windows_updates"] = &scripts.WindowsUpdatesScript{}

	// 📹 SCRIPTS QUE PRECISAM DE FTP
	s.scripts["screenshot"] = scripts.NewScreenshotScript(
		s.config.FtpServer,
		s.config.FtpUsername,
		s.config.FtpPassword,
	)

	s.scripts["screen_record"] = scripts.NewScreenRecordScript(
		s.config.FtpServer,
		s.config.FtpUsername,
		s.config.FtpPassword,
	)

	s.scripts["obs_record"] = scripts.NewOBSRecordScript(
		s.config.FtpServer,
		s.config.FtpUsername,
		s.config.FtpPassword,
		s.config.OBSWebSocketHost,
		s.config.OBSWebSocketPassword,
		s.config.OBSExecutablePath,
	)

	s.scripts["screenshot_improdutivo"] = scripts.NewScreenshotImprodutivoScript(
		s.config.FtpServer,
		s.config.FtpUsername,
		s.config.FtpPassword,
	)

	s.logger.Printf("✅ %d scripts registrados com configuração FTP (Server: %s, User: %s)",
		len(s.scripts), s.config.FtpServer, s.config.FtpUsername)
}

// ============================================
// NOVA FUNÇÃO: executeScript
// Executa um script internamente
// ============================================
func (s *service) executeScript(scriptName string, args ...string) *ScriptResult {
	result := &ScriptResult{
		ScriptName: scriptName,
		Timestamp:  time.Now(),
	}

	s.logger.Printf("📍 Executando script: %s", scriptName)

	// Busca o script no mapa
	script, exists := s.scripts[scriptName]
	if !exists {
		errMsg := fmt.Sprintf("script '%s' não encontrado", scriptName)
		s.logger.Printf("❌ %s", errMsg)
		result.Error = errMsg
		return result
	}

	// Executa o script
	data, err := script.Execute(args...)
	if err != nil {
		errMsg := fmt.Sprintf("erro ao executar script '%s': %v", scriptName, err)
		s.logger.Printf("❌ %s", errMsg)
		result.Error = errMsg
		return result
	}

	result.Data = data
	s.logger.Printf("✅ Script '%s' executado com sucesso: %d itens coletados", scriptName, len(data))

	return result
}

// ============================================
// FUNÇÃO MODIFICADA: executeScriptsParallel
// Agora executa scripts internos
// ============================================
func (s *service) executeScriptsParallel(scriptNames []string) {
	var wg sync.WaitGroup

	// 🔥 LIMITA A 3 SCRIPTS EXECUTANDO SIMULTANEAMENTE
	sem := NewSemaphore(3)

	for i, scriptName := range scriptNames {
		wg.Add(1)

		go func(index int, name string) {
			defer wg.Done()

			// Adquire permissão para executar
			sem.Acquire()
			defer sem.Release()

			// Remove a extensão .ps1 para obter o nome do script
			cleanName := strings.TrimSuffix(name, ".ps1")

			s.logger.Printf("DEBUG: Executando script %d/%d: %s", index+1, len(scriptNames), cleanName)

			// Executa o script
			result := s.executeScript(cleanName)

			s.logger.Printf("DEBUG: Script %s executado. Erro: %s, Dados: %d itens",
				cleanName, result.Error, len(result.Data))

			// 🔥 PROTEGE ESCRITA NO BANCO COM MUTEX
			err := s.storeData(result)

			if err != nil {
				s.logError(fmt.Sprintf("Erro ao salvar resultado de %s", cleanName), err)
			} else {
				s.logger.Printf("Script %s executado e salvo com sucesso", cleanName)
			}
		}(i, scriptName)
	}

	wg.Wait()
	s.logger.Println("DEBUG: Todos os scripts foram executados")
}

// shouldExecuteScript verifica se um script deve ser executado baseado em sua frequência
func (s *service) shouldExecuteScript(scriptName string, isDailyScript bool) bool {
	s.scriptTracker.mu.RLock()
	lastExec, exists := s.scriptTracker.LastExecution[scriptName]
	s.scriptTracker.mu.RUnlock()

	now := time.Now()

	if !exists {
		s.logger.Printf("✨ Script '%s' nunca foi executado - EXECUTANDO", scriptName)
		return true
	}

	if isDailyScript {
		lastExecDate := lastExec.Format("2006-01-02")
		todayDate := now.Format("2006-01-02")

		shouldExec := lastExecDate != todayDate
		if shouldExec {
			s.logger.Printf("📅 Script diário '%s' - última exec: %s, hoje: %s - EXECUTANDO",
				scriptName, lastExecDate, todayDate)
		} else {
			s.logger.Printf("⏭️ Script diário '%s' já executado hoje (%s) - PULANDO",
				scriptName, lastExecDate)
		}
		return shouldExec
	} else {
		interval := time.Duration(s.config.PeriodicScriptsIntervalMinutes) * time.Minute
		elapsed := now.Sub(lastExec)
		remaining := interval - elapsed

		shouldExec := elapsed >= interval
		if shouldExec {
			s.logger.Printf("⏰ Script periódico '%s' - intervalo atingido (última exec: %v atrás) - EXECUTANDO",
				scriptName, elapsed.Round(time.Second))
		} else {
			s.logger.Printf("⏭️ Script periódico '%s' - aguardando intervalo (faltam %v) - PULANDO",
				scriptName, remaining.Round(time.Second))
		}
		return shouldExec
	}
}

// markScriptExecuted marca um script como executado
func (s *service) markScriptExecuted(scriptName string) {
	s.scriptTracker.mu.Lock()
	s.scriptTracker.LastExecution[scriptName] = time.Now()
	s.scriptTracker.mu.Unlock()
}

// executeDailyScripts executa scripts que rodam uma vez por dia
func (s *service) executeDailyScripts() {
	dailyScripts := []string{
		"machines",
		"hardware_inventory",
		"software_inventory",
		"antivirus_status",
		"windows_updates",
		"login_attempts",
		"user_sessions",
		"system_logs",
	}

	s.logger.Printf("📅 Verificando scripts diários (%d scripts)", len(dailyScripts))

	sem := NewSemaphore(5)
	var wg sync.WaitGroup

	for _, scriptName := range dailyScripts {
		if s.shouldExecuteScript(scriptName, true) {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()

				sem.Acquire()
				defer sem.Release()

				s.logger.Printf("📄 Executando script diário: %s", name)
				result := s.executeScript(name)

				// 🔥 CRÍTICO: SÓ MARCA COMO EXECUTADO SE NÃO HOUVER ERRO
				if result.Error != "" {
					s.logger.Printf("❌ Script diário '%s' retornou erro: %s - NÃO será marcado como executado", name, result.Error)
					return
				}

				err := s.storeData(result)

				if err != nil {
					s.logError(fmt.Sprintf("Erro ao salvar resultado de %s", name), err)
					// 🔥 NÃO MARCA COMO EXECUTADO SE FALHOU AO SALVAR
					return
				}

				// ✅ SÓ MARCA COMO EXECUTADO SE TUDO DEU CERTO
				s.logger.Printf("✅ Script diário '%s' executado e salvo com sucesso", name)
				s.markScriptExecuted(name)
			}(scriptName)
		} else {
			s.logger.Printf("⏭️ Script diário '%s' já foi executado hoje, pulando...", scriptName)
		}
	}
	wg.Wait()
}

func (s *service) executePeriodicScripts() {
	periodicScripts := []string{
		"user_activity",
		"application_usage",
		"browser_history",
		"file_downloads",
		"system_metrics",
	}

	s.logger.Printf("⏰ Verificando scripts periódicos (%d scripts, intervalo: %d min)",
		len(periodicScripts), s.config.PeriodicScriptsIntervalMinutes)

	sem := NewSemaphore(5)
	var wg sync.WaitGroup

	for _, scriptName := range periodicScripts {
		if s.shouldExecuteScript(scriptName, false) {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()

				sem.Acquire()
				defer sem.Release()

				s.logger.Printf("📄 Executando script periódico: %s", name)
				result := s.executeScript(name)

				// 🔥 CRÍTICO: SÓ MARCA COMO EXECUTADO SE NÃO HOUVER ERRO
				if result.Error != "" {
					s.logger.Printf("❌ Script periódico '%s' retornou erro: %s - NÃO será marcado como executado", name, result.Error)
					return
				}

				err := s.storeData(result)

				if err != nil {
					s.logError(fmt.Sprintf("Erro ao salvar resultado de %s", name), err)
					// 🔥 NÃO MARCA COMO EXECUTADO SE FALHOU AO SALVAR
					return
				}

				// ✅ SÓ MARCA COMO EXECUTADO SE TUDO DEU CERTO
				s.logger.Printf("✅ Script periódico '%s' executado e salvo com sucesso", name)
				s.markScriptExecuted(name)
			}(scriptName)
		} else {
			s.logger.Printf("⏭️ Script periódico '%s' executado recentemente, pulando...", scriptName)
		}
	}
	wg.Wait()
}

func (s *service) resetScriptTracking() {
	s.scriptTracker.mu.Lock()
	s.scriptTracker.LastExecution = make(map[string]time.Time)
	s.scriptTracker.mu.Unlock()
	s.logger.Println("🔄 Tracking de scripts resetado - todos os scripts serão executados na próxima verificação")
}

// ============================================
// runDataCollection - SIMPLIFICADA
// Agora só executa a coleta inicial, sem criar goroutines
// ============================================
func (s *service) runDataCollection() {
	s.logger.Println("╔═══════════════════════════════════════════╗")
	s.logger.Println("🚀 INICIANDO CICLO DE COLETA DE DADOS")
	s.logger.Println("╚═══════════════════════════════════════════╝")

	// 🔹 EXECUTA SCRIPTS DIÁRIOS (uma vez por dia)
	s.executeDailyScripts()

	// 🔹 EXECUTA SCRIPTS PERIÓDICOS (a cada X minutos)
	s.executePeriodicScripts()

	s.logger.Println("╔═══════════════════════════════════════════╗")
	s.logger.Println("✅ COLETA DE DADOS CONCLUÍDA")
	s.logger.Println("╚═══════════════════════════════════════════╝")
}

// Run inicia o loop de gerenciamento
func (cm *CaptureManager) Run() {
	cm.service.logger.Println("🎯 CaptureManager: Iniciando gerenciamento")

	// Executa verificação imediata
	cm.checkAndUpdateCaptures()

	// Ticker para verificar configuração a cada 10 segundos (mais responsivo)
	configTicker := time.NewTicker(10 * time.Second)
	defer configTicker.Stop()

	for {
		select {
		case <-configTicker.C:
			cm.checkAndUpdateCaptures()

		case <-cm.stopChan:
			cm.service.logger.Println("🛑 CaptureManager: Recebido sinal de parada")
			cm.stopAllCaptures()
			return
		}
	}
}

// checkAndUpdateCaptures verifica configuração e ajusta capturas
func (cm *CaptureManager) checkAndUpdateCaptures() {
	// ⭐ Log de início da verificação com timestamp
	cm.service.logger.Printf("🔄 [%s] Verificando configuração de capturas...", time.Now().Format("15:04:05"))

	machineID, err := cm.service.getCurrentMachineID()
	if err != nil {
		cm.service.logger.Printf("❌ CaptureManager: Erro ao obter Machine ID: %v", err)
		cm.stopAllCaptures()
		return
	}

	if machineID == "" {
		cm.service.logger.Printf("⚠️ CaptureManager: Machine ID vazio (máquina pode não estar registrada ainda)")
		cm.stopAllCaptures()
		return
	}

	cm.service.logger.Printf("✅ Machine ID obtido: %s", machineID)

	cfg, err := cm.service.getMachineConfig(machineID)
	if err != nil {
		cm.service.logger.Printf("❌ CaptureManager: Erro ao buscar config do banco: %v", err)
		cm.stopAllCaptures()
		return
	}

	if cfg == nil {
		cm.service.logger.Println("⚠️ CaptureManager: Nenhuma configuração encontrada no banco (cfg == nil)")
		cm.stopAllCaptures()
		return
	}

	if !cfg.Active {
		cm.service.logger.Println("⚠️ CaptureManager: Configuração existe mas está INATIVA (active = false)")
		cm.stopAllCaptures()
		return
	}

	// ⭐ Log detalhado da configuração encontrada
	cm.service.logger.Printf("📋 CaptureManager: Configuração ATIVA encontrada:")
	cm.service.logger.Printf("   • Type: %s", cfg.Type)
	cm.service.logger.Printf("   • PictureTime: %d segundos", cfg.PictureTime)
	cm.service.logger.Printf("   • RecordTime: %d segundos", cfg.RecordTime)
	cm.service.logger.Printf("   • RecordingTiming: %d segundos", cfg.RecordingTiming)
	cm.service.logger.Printf("   • RecordCamera: %v", cfg.RecordCamera)
	cm.service.logger.Printf("   • RecordMicro: %v", cfg.RecordMicro)

	// Verifica se configuração mudou
	configChanged := cm.lastConfig == nil ||
		cm.lastConfig.Type != cfg.Type ||
		cm.lastConfig.PictureTime != cfg.PictureTime ||
		cm.lastConfig.RecordTime != cfg.RecordTime ||
		cm.lastConfig.RecordingTiming != cfg.RecordingTiming

	if configChanged {
		cm.service.logger.Println("🔄 CaptureManager: Configuração mudou, reiniciando capturas...")
		cm.stopAllCaptures()
		cm.lastConfig = cfg
	} else {
		cm.service.logger.Println("✅ Configuração não mudou, verificando estado atual...")
	}

	// ⭐ Gerencia screenshots com logs detalhados
	if cfg.Type == "picture" && cfg.PictureTime > 0 {
		if !cm.screenshotActive {
			cm.service.logger.Printf("🚀 Iniciando screenshots pela primeira vez (intervalo: %ds)...", cfg.PictureTime)
			cm.startScreenshots(cfg.PictureTime)
		} else {
			cm.service.logger.Printf("✅ Screenshots já ativos (intervalo: %ds)", cfg.PictureTime)
		}
	} else {
		if cm.screenshotActive {
			cm.service.logger.Println("🛑 Type não é 'picture' ou PictureTime <= 0, parando screenshots...")
			cm.stopScreenshots()
		} else {
			if cfg.Type != "picture" {
				cm.service.logger.Printf("ℹ️ Type atual: '%s' (não é 'picture'), screenshots não serão iniciados", cfg.Type)
			}
			if cfg.PictureTime <= 0 {
				cm.service.logger.Printf("ℹ️ PictureTime: %d (deve ser > 0), screenshots não serão iniciados", cfg.PictureTime)
			}
		}
	}

	// ⭐ Gerencia gravações com logs detalhados
	if cfg.Type == "record" && cfg.RecordTime > 0 && cfg.RecordingTiming > 0 {
		if !cm.recordingActive {
			cm.service.logger.Printf("🚀 Iniciando gravações (intervalo: %ds, duração: %ds)...", cfg.RecordTime, cfg.RecordingTiming)
			cm.startRecordings(cfg)
		} else {
			cm.service.logger.Printf("✅ Gravações já ativas (intervalo: %ds)", cfg.RecordTime)
		}
	} else {
		if cm.recordingActive {
			cm.service.logger.Println("🛑 Parando gravações (configuração não atende requisitos)...")
			cm.stopRecordings()
		}
	}

	cm.service.logger.Printf("📊 Estado atual: Screenshots=%v, Recordings=%v", cm.screenshotActive, cm.recordingActive)
}

// startScreenshots inicia captura de screenshots
func (cm *CaptureManager) startScreenshots(intervalSeconds int) {
	if cm.screenshotActive {
		return
	}

	interval := time.Duration(intervalSeconds) * time.Second
	cm.service.logger.Printf("📸 CaptureManager: Iniciando screenshots (intervalo: %v)", interval)

	cm.screenshotTicker = time.NewTicker(interval)
	cm.screenshotStop = make(chan struct{})
	cm.screenshotActive = true

	// Captura imediata
	go cm.captureScreenshot()

	// Loop de capturas
	go func() {
		for {
			select {
			case <-cm.screenshotTicker.C:
				go cm.captureScreenshot()

			case <-cm.screenshotStop:
				cm.service.logger.Println("🛑 Screenshot loop finalizado")
				return
			}
		}
	}()
}

// stopScreenshots para captura de screenshots
func (cm *CaptureManager) stopScreenshots() {
	if !cm.screenshotActive {
		return
	}

	cm.service.logger.Println("🛑 CaptureManager: Parando screenshots")

	if cm.screenshotTicker != nil {
		cm.screenshotTicker.Stop()
		cm.screenshotTicker = nil
	}

	if cm.screenshotStop != nil {
		close(cm.screenshotStop)
		cm.screenshotStop = nil
	}

	cm.screenshotActive = false
}

// startRecordings inicia gravações
func (cm *CaptureManager) startRecordings(cfg *MachineConfig) {
	if cm.recordingActive {
		return
	}

	interval := time.Duration(cfg.RecordTime) * time.Second
	cm.service.logger.Printf("🎬 CaptureManager: Iniciando gravações (intervalo: %v, duração: %ds)",
		interval, cfg.RecordingTiming)

	cm.recordingTicker = time.NewTicker(interval)
	cm.recordingStop = make(chan struct{})
	cm.recordingActive = true

	// Gravação imediata
	go cm.captureRecording(cfg)

	// Loop de gravações
	go func() {
		for {
			select {
			case <-cm.recordingTicker.C:
				go cm.captureRecording(cfg)

			case <-cm.recordingStop:
				cm.service.logger.Println("🛑 Recording loop finalizado")
				return
			}
		}
	}()
}

// stopRecordings para gravações
func (cm *CaptureManager) stopRecordings() {
	if !cm.recordingActive {
		return
	}

	cm.service.logger.Println("🛑 CaptureManager: Parando gravações")

	if cm.recordingTicker != nil {
		cm.recordingTicker.Stop()
		cm.recordingTicker = nil
	}

	if cm.recordingStop != nil {
		close(cm.recordingStop)
		cm.recordingStop = nil
	}

	cm.recordingActive = false
}

// stopAllCaptures para todas as capturas
func (cm *CaptureManager) stopAllCaptures() {
	cm.stopScreenshots()
	cm.stopRecordings()
}

// checkMicrophoneActive verifica se o microfone está em uso
func (mm *MicrophoneMonitor) checkMicrophoneActive() bool {
	// PowerShell script que usa API do Windows para detectar uso REAL do microfone
	script := `
# Verifica se o microfone está sendo usado AGORA (ícone na barra de tarefas)
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
using System.Collections.Generic;
namespace AudioMonitor {
    [ComImport]
    [Guid("BCDE0395-E52F-467C-8E3D-C4579291692E")]
    internal class MMDeviceEnumeratorClass { }
    [Guid("A95664D2-9614-4F35-A746-DE8DB63617E6")]
    [InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    internal interface IMMDeviceEnumerator {
        int NotImpl1();
        int GetDefaultAudioEndpoint(int dataFlow, int role, out IMMDevice ppDevice);
    }
    [Guid("D666063F-1587-4E43-81F1-B948E807363F")]
    [InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    internal interface IMMDevice {
        int Activate(ref Guid iid, int dwClsCtx, IntPtr pActivationParams, out IAudioMeterInformation ppInterface);
    }
    [Guid("C02216F6-8C67-4B5B-9D00-D008E73E0064")]
    [InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    internal interface IAudioMeterInformation {
        int GetPeakValue(out float pfPeak);
    }
    public class MicrophoneMonitor {
        public static bool IsMicrophoneActive() {
            try {
                var enumerator = new MMDeviceEnumeratorClass() as IMMDeviceEnumerator;
                IMMDevice device;
                // 1 = eCapture (microfone), 0 = eConsole (default device)
                enumerator.GetDefaultAudioEndpoint(1, 0, out device);
                Guid IID_IAudioMeterInformation = new Guid("C02216F6-8C67-4B5B-9D00-D008E73E0064");
                IAudioMeterInformation meter;
                device.Activate(ref IID_IAudioMeterInformation, 1, IntPtr.Zero, out meter);
                float peak;
                meter.GetPeakValue(out peak);
                // Se peak > 0, o microfone está captando áudio
                return peak > 0.001f; // threshold pequeno para evitar ruído
            } catch {
                return false;
            }
        }
    }
}
"@ -ErrorAction SilentlyContinue

try {
    $isMicActive = [AudioMonitor.MicrophoneMonitor]::IsMicrophoneActive()
    if ($isMicActive) {
        # Microfone está ativo, identifica qual app
        $possibleProcesses = @(
            "Teams", "ms-teams", "Zoom", "Discord", "Skype", "Slack",
            "WhatsApp", "Telegram", "chrome", "firefox", "msedge",
            "webex", "gotomeeting", "bluejeans"
        )
        $activeProcesses = Get-Process | Where-Object {
            $processName = $_.ProcessName
            $possibleProcesses | Where-Object { $processName -match $_ }
        } | Select-Object -ExpandProperty ProcessName -Unique
        if ($activeProcesses.Count -gt 0) {
            Write-Output ($activeProcesses -join ",")
        } else {
            Write-Output "MICROPHONE_ACTIVE_UNKNOWN"
        }
    } else {
        Write-Output "NO_MICROPHONE_USAGE"
    }
} catch {
    Write-Output "ERROR: $($_.Exception.Message)"
}
`

	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	output, err := cmd.Output()

	if err != nil {
		mm.service.logger.Printf("⚠️ Erro ao verificar microfone: %v", err)
		return false
	}

	result := strings.TrimSpace(string(output))

	// Casos especiais
	if result == "NO_MICROPHONE_USAGE" {
		if mm.lastMicState {
			mm.service.logger.Println("🎤 MICROFONE DESATIVADO")
		}
		mm.lastMicState = false
		return false
	}

	if result == "MICROPHONE_ACTIVE_UNKNOWN" {
		// Microfone está ativo mas não conseguimos identificar o app
		if !mm.lastMicState {
			mm.service.logger.Println("🎤 MICROFONE ATIVADO - App desconhecido (não está na lista)")
		}
		mm.lastMicState = true
		return true
	}

	if strings.HasPrefix(result, "ERROR:") {
		mm.service.logger.Printf("⚠️ Erro no script de detecção: %s", result)
		return false
	}

	processes := strings.Split(result, ",")

    // 🔥 NOVA LÓGICA: Verifica se há apps de comunicação
    hasCommunicationApp := false
    obsDetected := false

    for _, proc := range processes {
    		procLower := strings.ToLower(strings.TrimSpace(proc))

    		// Ignora processos da lista de ignorados
    		shouldIgnore := false
    		for _, ignoredProc := range ignoredMicProcesses {
    			if strings.Contains(procLower, strings.ToLower(ignoredProc)) {
    				shouldIgnore = true
    				break
    			}
    		}
    		if shouldIgnore {
    			continue
    		}

    		// Detecta OBS (já está na lista de ignorados, mas mantém por compatibilidade)
    		if strings.Contains(procLower, "obs64") ||
    			strings.Contains(procLower, "obs32") ||
    			strings.Contains(procLower, "streamlabs") {
    			obsDetected = true
    			continue
    		}

    		// Verifica se é app de comunicação
    		for _, commApp := range communicationApps {
    			if strings.Contains(procLower, commApp) {
    				hasCommunicationApp = true
    				break
    			}
    		}
    }

	// 🔥 REGRA PRINCIPAL:
	// - Se só tem OBS usando microfone → NÃO grava
	// - Se tem app de comunicação → GRAVA
	if obsDetected && !hasCommunicationApp {
		if mm.lastMicState {
			mm.service.logger.Println("🎤 MICROFONE: Apenas OBS detectado - IGNORANDO gravação")
		}
		mm.lastMicState = false
		return false
	}

	isActive := hasCommunicationApp

	// 🆕 Log apenas quando o estado muda
	if isActive != mm.lastMicState {
		if isActive {
			mm.service.logger.Printf("🎤 MICROFONE ATIVADO - Apps detectados: %s", strings.Join(processes, ", "))
		} else {
			mm.service.logger.Println("🎤 MICROFONE DESATIVADO")
		}
		mm.lastMicState = isActive
	}

	return isActive
}

func (mm *MicrophoneMonitor) getMicrophoneProcesses() []string {
	script := `
$audioDevices = Get-WmiObject -Class Win32_SoundDevice | Where-Object {
    $_.Status -eq "OK" -and
    ($_.Name -like "*Microphone*" -or $_.Name -like "*Mic*")
}

if ($audioDevices.Count -eq 0) {
    exit
}

$possibleMicProcesses = @(
    "Teams", "Zoom", "Discord", "Skype", "Slack", "WhatsApp", "Telegram",
    "chrome", "firefox", "msedge", "obs64", "obs32", "streamlabs",
    "webex", "gotomeeting", "bluejeans", "meet", "hangouts"
)

Get-Process | Where-Object {
    $processName = $_.ProcessName
    $possibleMicProcesses | Where-Object { $processName -match $_ }
} | Select-Object ProcessName, Id, @{N='CPU';E={$_.CPU}}, @{N='Memory';E={[math]::Round($_.WS/1MB,2)}} |
    Format-Table -AutoSize | Out-String
`

	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	output, _ := cmd.Output()

	return strings.Split(strings.TrimSpace(string(output)), "\n")
}

func (mm *MicrophoneMonitor) logStatus() {
	processes := mm.getMicrophoneProcesses()

	mm.service.logger.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	mm.service.logger.Println("📊 STATUS DO MONITOR DE MICROFONE")
	mm.service.logger.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	mm.service.logger.Printf("🎤 Microfone ativo: %v", mm.lastMicState)
	mm.service.logger.Printf("🎥 Gravação ativa: %v", mm.isRecording)

	if len(processes) > 0 && processes[0] != "" {
		mm.service.logger.Println("📌 Processos usando microfone:")
		for _, proc := range processes {
			if proc != "" {
				mm.service.logger.Printf("   %s", proc)
			}
		}
	} else {
		mm.service.logger.Println("📌 Nenhum processo usando microfone")
	}
	mm.service.logger.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
}

// Run inicia o monitoramento do microfone
func (mm *MicrophoneMonitor) Run() {
	mm.service.logger.Println("🎤 MicrophoneMonitor: Iniciando monitoramento")
	mm.service.logger.Println("⚙️  Configuração: Gravação automática para apps de comunicação")
	mm.service.logger.Println("🚫 OBS será ignorado (não dispara gravação)")

	// Log inicial de status
	mm.logStatus()

	// Ticker para verificações (1 segundo)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// Ticker para log de status (a cada 5 minutos)
	statusTicker := time.NewTicker(5 * time.Minute)
	defer statusTicker.Stop()

	for {
		select {
		case <-ticker.C:
			mm.checkAndManageRecording()

		case <-statusTicker.C:
			mm.logStatus()

		case <-mm.stopChan:
			mm.service.logger.Println("🛑 MicrophoneMonitor: Recebido sinal de parada")
			mm.stopRecording()
			return
		}
	}
}

// checkAndManageRecording verifica microfone e gerencia gravação
func (mm *MicrophoneMonitor) checkAndManageRecording() {
	// 🔥 CORREÇÃO: Chama checkMicrophoneActive SEM mutex travado
	micActive := mm.checkMicrophoneActive()

	mm.mu.Lock()
	defer mm.mu.Unlock()

	// 🆕 Iniciou uso do microfone (apps de comunicação)
	if micActive && !mm.isRecording {
		// Log detalhado dos processos
		processes := mm.getMicrophoneProcesses()
		mm.service.logger.Println("🎤 ▶️  Apps de comunicação usando microfone detectados:")
		for _, proc := range processes {
			if proc != "" {
				mm.service.logger.Printf("   📌 %s", proc)
			}
		}
		mm.service.logger.Println("🎥 Iniciando gravação OBS...")
		mm.startRecordingInternal()
	}

	// 🔥 NOVA LÓGICA: Verifica se gravação está ativa mas microfone foi desativado
	if !micActive && mm.isRecording {
		mm.service.logger.Println("🎤 ⏹️  Apps de comunicação pararam de usar microfone")
		mm.service.logger.Println("🎥 Parando gravação OBS...")
		mm.stopRecordingInternal()
	}
}

func (mm *MicrophoneMonitor) startRecordingInternal() {
	if mm.isRecording {
		return
	}

	mm.recordingStopChan = make(chan struct{})
	mm.isRecording = true

	// 🆕 Gera ID único para rastrear gravação
	mm.activeClientID = fmt.Sprintf("mic_%s_%d", getCurrentUsername(), time.Now().Unix())

	go func() {
		args := []string{
			"0", // ⭐ Duração 0 = gravação contínua
		}

		mm.service.logger.Println("🎥 Chamando script obs_record com duração=0 (contínua)")

		// ⭐ EXECUTA EM GOROUTINE SEPARADA
		done := make(chan *ScriptResult, 1)

		go func() {
			result := mm.service.executeScript("obs_record", args...)
			done <- result
		}()

		// 🔥 NOVO: Ticker para verificar continuamente se microfone ainda está ativo
		checkTicker := time.NewTicker(5 * time.Second) // Verifica a cada 5 segundos
		defer checkTicker.Stop()

		for {
			select {
			case result := <-done:
				// Gravação terminou naturalmente (não deveria acontecer com duration=0)
				mm.service.logger.Println("⚠️  Gravação OBS terminou inesperadamente")

				if err := mm.service.storeData(result); err != nil {
					mm.service.logger.Printf("❌ Erro ao salvar gravação OBS (microfone): %v", err)
				} else {
					mm.service.logger.Println("✅ Gravação OBS (microfone) salva com sucesso")
				}

				mm.mu.Lock()
				mm.isRecording = false
				mm.activeClientID = ""
				mm.mu.Unlock()

				mm.service.logger.Println("🎤 Monitor de microfone pronto para nova gravação")
				return

			case <-checkTicker.C:
				// 🔥 VERIFICA SE MICROFONE AINDA ESTÁ ATIVO
				mm.service.logger.Println("🔍 Verificando se microfone ainda está ativo...")

				// 🔥 CRÍTICO: Chama checkMicrophoneActive SEM mutex travado
				isStillActive := mm.checkMicrophoneActive()

				if !isStillActive {
					mm.service.logger.Println("⚠️  Microfone não está mais ativo durante gravação - encerrando...")

					// Para a gravação
					mm.service.logger.Println("🛑 Parando gravação OBS (microfone desativado)")

					// 🔥 TRAVA MUTEX APENAS PARA ACESSAR activeClientID
					mm.mu.Lock()
					clientID := mm.activeClientID
					mm.mu.Unlock()

					obsScript, ok := mm.service.scripts["obs_record"].(*scripts.OBSRecordScript)
					if ok {
						if err := obsScript.StopActiveRecording(clientID); err != nil {
							mm.service.logger.Printf("⚠️ Aviso ao parar gravação: %v", err)
						} else {
							mm.service.logger.Println("✅ Comando de parada enviado ao OBS")
						}
					} else {
						mm.service.logger.Println("❌ ERRO: Não foi possível acessar obs_record script")
					}

					// Aguarda conclusão do upload
					mm.service.logger.Println("⏳ Aguardando finalização da gravação...")
					result := <-done

					if result.Error != "" {
						mm.service.logger.Printf("⚠️  Gravação finalizada com aviso: %s", result.Error)
					}

					if err := mm.service.storeData(result); err != nil {
						mm.service.logger.Printf("❌ Erro ao salvar gravação interrompida: %v", err)
					} else {
						mm.service.logger.Println("✅ Gravação interrompida salva com sucesso")
					}

					mm.mu.Lock()
					mm.isRecording = false
					mm.activeClientID = ""
					mm.mu.Unlock()

					mm.service.logger.Println("🎤 Monitor de microfone pronto para nova gravação")
					return
				} else {
					mm.service.logger.Println("✅ Microfone ainda ativo, continuando gravação...")
				}

			case <-mm.recordingStopChan:
				// ⭐ SINAL DE PARADA RECEBIDO (shutdown do serviço)
				mm.service.logger.Println("🛑 Parando gravação OBS (shutdown do serviço)")

				// 🔥 TRAVA MUTEX APENAS PARA ACESSAR activeClientID
				mm.mu.Lock()
				clientID := mm.activeClientID
				mm.mu.Unlock()

				obsScript, ok := mm.service.scripts["obs_record"].(*scripts.OBSRecordScript)
				if ok {
					if err := obsScript.StopActiveRecording(clientID); err != nil {
						mm.service.logger.Printf("⚠️ Aviso ao parar gravação: %v", err)
					} else {
						mm.service.logger.Println("✅ Comando de parada enviado ao OBS")
					}
				} else {
					mm.service.logger.Println("❌ ERRO: Não foi possível acessar obs_record script")
				}

				// Aguarda conclusão do upload
				mm.service.logger.Println("⏳ Aguardando finalização da gravação...")
				result := <-done

				if result.Error != "" {
					mm.service.logger.Printf("⚠️  Gravação finalizada com aviso: %s", result.Error)
				}

				if err := mm.service.storeData(result); err != nil {
					mm.service.logger.Printf("❌ Erro ao salvar gravação interrompida: %v", err)
				} else {
					mm.service.logger.Println("✅ Gravação interrompida salva com sucesso")
				}

				mm.mu.Lock()
				mm.isRecording = false
				mm.activeClientID = ""
				mm.mu.Unlock()

				mm.service.logger.Println("🎤 Monitor de microfone pronto para nova gravação")
				return
			}
		}
	}()
}

// startRecording inicia gravação OBS
func (mm *MicrophoneMonitor) startRecording() {
	if mm.isRecording {
		return
	}

	mm.recordingStopChan = make(chan struct{})
	mm.isRecording = true

	// ← NOVO: Gera ID único para rastrear gravação
	mm.activeClientID = fmt.Sprintf("mic_%s_%d", getCurrentUsername(), time.Now().Unix())

	go func() {
		args := []string{
			"0", // ← Duração 0 = gravação contínua
			"-RecordMicro",
		}

		mm.service.logger.Println("🎥 Iniciando gravação OBS por atividade de microfone")

		// ← EXECUTA EM GOROUTINE SEPARADA
		done := make(chan *ScriptResult, 1)

		go func() {
			result := mm.service.executeScript("obs_record", args...)
			done <- result
		}()

		// ← AGUARDA FINALIZAÇÃO OU SINAL DE PARADA
		select {
		case result := <-done:
			// Gravação terminou naturalmente
			if err := mm.service.storeData(result); err != nil {
				mm.service.logger.Printf("❌ Erro ao salvar gravação OBS (microfone): %v", err)
			} else {
				mm.service.logger.Println("✅ Gravação OBS (microfone) salva com sucesso")
			}

		case <-mm.recordingStopChan:
			// ← SINAL DE PARADA RECEBIDO
			mm.service.logger.Println("🛑 Parando gravação OBS (microfone desativado)")

			// ← CORREÇÃO CRÍTICA: Para gravação usando script correto
			obsScript, ok := mm.service.scripts["obs_record"].(*scripts.OBSRecordScript)
			if ok {
				if err := obsScript.StopActiveRecording(mm.activeClientID); err != nil {
					mm.service.logger.Printf("⚠️ Aviso ao parar gravação: %v", err)
				}
			}

			// Aguarda conclusão do upload
			result := <-done
			if err := mm.service.storeData(result); err != nil {
				mm.service.logger.Printf("❌ Erro ao salvar gravação interrompida: %v", err)
			}
		}

		mm.mu.Lock()
		mm.isRecording = false
		mm.activeClientID = ""
		mm.mu.Unlock()
	}()
}

func (mm *MicrophoneMonitor) stopRecordingInternal() {
	if !mm.isRecording {
		return
	}

	if mm.recordingStopChan != nil {
		close(mm.recordingStopChan)
		mm.recordingStopChan = nil
	}

	mm.service.logger.Println("🛑 Sinal de parada enviado para gravação OBS")
}


// stopRecording para a gravação OBS
func (mm *MicrophoneMonitor) stopRecording() {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	mm.stopRecordingInternal()
}

// captureScreenshot executa uma captura de screenshot
func (cm *CaptureManager) captureScreenshot() {
	defer func() {
		if r := recover(); r != nil {
			cm.service.logger.Printf("⚠️ Panic em captureScreenshot: %v", r)
		}
	}()

	cm.service.logger.Println("📸 Executando captura de screenshot...")

	result := cm.service.executeScript("screenshot")

	if result.Error != "" {
		cm.service.logger.Printf("❌ Erro ao capturar screenshot: %s", result.Error)
		return
	}

	if err := cm.service.storeData(result); err != nil {
		cm.service.logger.Printf("❌ Erro ao salvar screenshot: %v", err)
		return
	}

	cm.service.logger.Println("✅ Screenshot capturado e salvo com sucesso")
}

// captureRecording executa uma gravação
func (cm *CaptureManager) captureRecording(cfg *MachineConfig) {
	defer func() {
		if r := recover(); r != nil {
			cm.service.logger.Printf("⚠️ Panic em captureRecording: %v", r)
		}
	}()

	cm.service.logger.Println("🎬 Executando gravação...")
	cm.service.executeRecordingBasedOnConfig(cfg)
}

// ============================================
// FUNÇÃO CORRIGIDA: executeRecordingBasedOnConfig
// Executa gravação usando screen_record ou obs_record
// ============================================
func (s *service) executeRecordingBasedOnConfig(cfg *MachineConfig) {
	// Verifica se precisa usar OBS (câmera OU microfone ativos)
	needsOBS := cfg.RecordCamera || cfg.RecordMicro

	if needsOBS {
		// Usa OBS quando câmera ou microfone estão ativos
		s.logger.Printf("🎥 Usando OBS para gravação (Camera=%v, Micro=%v, Duração=%ds)",
			cfg.RecordCamera, cfg.RecordMicro, cfg.RecordingTiming)
		s.executeOBSRecording(cfg)
	} else {
		// Usa FFmpeg quando apenas tela (sem câmera/micro)
		s.logger.Printf("🎬 Usando screen_record para gravação (Duração=%ds)", cfg.RecordingTiming)
		s.executeScreenRecording(cfg)
	}
}

func (s *service) executeOBSRecording(cfg *MachineConfig) {
	args := []string{
		fmt.Sprintf("%d", cfg.RecordingTiming),
	}

	if cfg.RecordCamera {
		args = append(args, "-RecordCamera")
	}
	if cfg.RecordMicro {
		args = append(args, "-RecordMicro")
	}

	s.logger.Printf("🎥 Iniciando gravação OBS: duration=%ds, camera=%v, micro=%v",
		cfg.RecordingTiming, cfg.RecordCamera, cfg.RecordMicro)

	result := s.executeScript("obs_record", args...)

	if err := s.storeData(result); err != nil {
		s.logError("Erro ao salvar gravação OBS", err)
	} else {
		s.logger.Printf("✅ Gravação OBS salva com sucesso")
	}
}

func (s *service) executeScreenRecording(cfg *MachineConfig) {
	args := []string{
		fmt.Sprintf("%d", cfg.RecordingTiming),
	}

	s.logger.Printf("🎬 Iniciando gravação screen_record: duration=%ds", cfg.RecordingTiming)

	result := s.executeScript("screen_record", args...)

	if err := s.storeData(result); err != nil {
		s.logError("Erro ao salvar gravação screen_record", err)
	} else {
		s.logger.Printf("✅ Gravação screen_record salva com sucesso")
	}
}

func (s *service) executeScriptWithArgs(scriptPath string, args ...string) *ScriptResult {
	result := &ScriptResult{
		ScriptName: filepath.Base(scriptPath),
		Timestamp:  time.Now(),
	}

	// Monta os argumentos do comando
	exec.Command("chcp", "65001").Run()
	cmdArgs := append([]string{"-OutputFormat", "Text", "-ExecutionPolicy", "Bypass", "-File", scriptPath}, args...)

	// Executa o script PowerShell com argumentos
	cmd := exec.Command("powershell", cmdArgs...)
	output, err := cmd.Output()
	if err != nil {
		result.Error = err.Error()
		return result
	}

	// Tenta fazer parse do JSON (mesmo código da sua função original)
	var data []map[string]interface{}

	// Tenta parsear como array
	if err := json.Unmarshal(output, &data); err != nil {
		// Se falhar, tenta parsear como objeto único
		var single map[string]interface{}
		if err2 := json.Unmarshal(output, &single); err2 == nil {
			data = []map[string]interface{}{single}
		} else {
			// Se ainda falhar, armazena como texto
			data = []map[string]interface{}{
				{"output": string(output)},
			}
		}
	}

	result.Data = data
	return result
}

func (s *service) storeData(result *ScriptResult) error {
	// Remove a extensão .ps1 do nome do script
	scriptNameClean := strings.ReplaceAll(strings.ReplaceAll(result.ScriptName, ".ps1", ""), "-", "_")

	// Roteamento específico para cada tipo de script
	switch scriptNameClean {
	case "machines":
		return s.storeMachineData(result)
	case "hardware_inventory":
		return s.storeHardwareInventoryData(result)
	case "software_inventory":
		return s.storeSoftwareInventoryData(result)
	case "antivirus_status":
		return s.storeAntivirusStatusData(result)
	case "system_metrics":
		return s.storeSystemMetricsData(result)
	case "windows_services":
		return s.storeWindowsServicesData(result)
	case "windows_updates":
		return s.storeWindowsUpdatesData(result)
	case "user_activity":
		return s.storeUserActivityData(result)
	case "application_usage":
		return s.storeApplicationUsageData(result)
	case "browser_history":
		return s.storeBrowserHistoryData(result)
	case "file_downloads":
		return s.storeFileDownloadsData(result)
	case "recent_files":
		return s.storeRecentFilesData(result)
	case "user_sessions":
		return s.storeUserSessionsData(result)
	case "applied_gpos":
		return s.storeAppliedGposData(result)
	case "login_attempts":
		return s.storeLoginAttemptsData(result)
	case "network_config":
		return s.storeNetworkConfigData(result)
	case "network_connections":
		return s.storeNetworkConnectionsData(result)
	case "running_processes":
		return s.storeRunningProcessesData(result)
	case "system_logs":
		return s.storeSystemLogsData(result)
	case "usb_devices":
		return s.storeUsbDevicesData(result)
	case "network_traffic":
		return s.storeNetworkTrafficData(result)
	case "network_mappings":
		return s.storeNetworkMappingsData(result)
	case "screenshot":
		return s.storeScreenshotsData(result)
	case "screen_record":
		return s.storeScreenRecordingsData(result)
	case "obs_record":
		return s.storeScreenRecordingsData(result)
	default:
		// Tabela genérica para scripts não mapeados
		return s.storeGenericData(result)
	}
}

func (s *service) storeMachineData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script machines retornou erro: %s", result.Error)
		return nil // Não trata como erro fatal
	}

	s.logger.Printf("Processando %d registros do script machines", len(result.Data))

	for i, item := range result.Data {
		s.logger.Printf("Processando registro %d: %+v", i+1, item)

		// Se o item tem uma chave "output" com JSON, tenta parsear
		if outputStr, hasOutput := item["output"].(string); hasOutput {
			s.logger.Printf("Detectado JSON dentro do campo 'output', tentando parsear...")
			var parsedData map[string]interface{}
			if err := json.Unmarshal([]byte(strings.TrimSpace(outputStr)), &parsedData); err != nil {
				s.logger.Printf("Erro ao parsear JSON do campo output: %v", err)
			} else {
				s.logger.Printf("JSON parseado com sucesso: %+v", parsedData)
				item = parsedData // Substitui o item pelo JSON parseado
			}
		}

		// Converte o mapa genérico para a estrutura específica
		var machineData MachineData

		// Extração segura dos dados
		if hostname, ok := item["hostname"].(string); ok {
			machineData.Hostname = strings.ToUpper(strings.TrimSpace(hostname))
			s.logger.Printf("Hostname encontrado: '%s'", machineData.Hostname)
		} else {
			s.logger.Printf("Hostname não encontrado ou não é string. Valor: %+v (tipo: %T)", item["hostname"], item["hostname"])
		}

		// 🔥 CORREÇÃO: Tratar campos que podem ser ponteiros ou strings
		// IP Address
		if ipAddr, ok := item["ip_address"].(string); ok && ipAddr != "" {
			machineData.IPAddress = strings.TrimSpace(ipAddr)
		} else if ipAddrPtr, ok := item["ip_address"].(*string); ok && ipAddrPtr != nil {
			machineData.IPAddress = strings.TrimSpace(*ipAddrPtr)
		} else {
			machineData.IPAddress = "" // valor vazio se não encontrado
		}

		// MAC Address
		if macAddr, ok := item["mac_address"].(string); ok && macAddr != "" {
			machineData.MACAddress = strings.TrimSpace(macAddr)
		} else if macAddrPtr, ok := item["mac_address"].(*string); ok && macAddrPtr != nil {
			machineData.MACAddress = strings.TrimSpace(*macAddrPtr)
		} else {
			machineData.MACAddress = "" // valor vazio se não encontrado
		}

		// Domain Name
		if domain, ok := item["domain_name"].(string); ok && domain != "" {
			machineData.DomainName = strings.TrimSpace(domain)
		} else if domainPtr, ok := item["domain_name"].(*string); ok && domainPtr != nil {
			machineData.DomainName = strings.TrimSpace(*domainPtr)
		} else {
			machineData.DomainName = "" // valor vazio se não encontrado
		}

		// Status
		if status, ok := item["status"].(string); ok {
			machineData.Status = strings.TrimSpace(status)
		} else {
			machineData.Status = "active" // valor padrão
		}

		s.logger.Printf("Dados extraídos - Hostname: '%s', IP: '%s', MAC: '%s', Domain: '%s', Status: '%s'",
			machineData.Hostname, machineData.IPAddress, machineData.MACAddress, machineData.DomainName, machineData.Status)

		// Validação básica
		if machineData.Hostname == "" {
			s.logger.Printf("Hostname vazio no registro %d, pulando...", i+1)
			continue
		}

		// Insert ou update da máquina e atualiza o machine_id em cache
		machineID, err := s.upsertMachine(machineData)
		if err != nil {
			s.logger.Printf("Erro ao inserir/atualizar máquina %s: %v", machineData.Hostname, err)
			return fmt.Errorf("erro ao inserir/atualizar máquina %s: %v", machineData.Hostname, err)
		}

		// Se esta é a máquina atual e não temos machine_id em cache, salva
		currentHostname := os.Getenv("COMPUTERNAME")
		if currentHostname != "" && strings.ToUpper(strings.TrimSpace(currentHostname)) == machineData.Hostname && s.machineID == "" {
			s.machineID = machineID
			if err := s.saveMachineIDToFile(machineID); err != nil {
				s.logger.Printf("Erro ao salvar machine_id no arquivo: %v", err)
			}
		}

		s.logger.Printf("Dados da máquina %s atualizados com sucesso no banco de dados", machineData.Hostname)
	}

	return nil
}

func (s *service) storeHardwareInventoryData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script hardware_inventory retornou erro: %s", result.Error)
		return nil // Não trata como erro fatal
	}

	s.logger.Printf("Processando %d registros do script hardware_inventory", len(result.Data))

	// Obtém o machine_id atual
	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}

	if machineID == "" {
		hostname := os.Getenv("COMPUTERNAME")
		s.logger.Printf("Máquina %s não encontrada na tabela machines. Pulando processamento do hardware.", hostname)
		return nil
	}

	s.logger.Printf("Machine ID utilizado: %s", machineID)

	// Processa cada componente de hardware
	for i, item := range result.Data {
		s.logger.Printf("Processando componente %d: %+v", i+1, item)

		// Se o item tem uma chave "output" com JSON, tenta parsear
		if outputStr, hasOutput := item["output"].(string); hasOutput {
			s.logger.Printf("Detectado JSON dentro do campo 'output', tentando parsear...")
			var parsedData []map[string]interface{}
			if err := json.Unmarshal([]byte(strings.TrimSpace(outputStr)), &parsedData); err != nil {
				s.logger.Printf("Erro ao parsear JSON do campo output: %v", err)
				continue
			} else {
				s.logger.Printf("JSON parseado com sucesso: %d componentes encontrados", len(parsedData))
				// Processa cada componente do array
				for j, component := range parsedData {
					s.logger.Printf("Processando componente %d do array: %+v", j+1, component)
					if err := s.processHardwareComponent(machineID, component); err != nil {
						s.logger.Printf("Erro ao processar componente %d: %v", j+1, err)
					}
				}
				continue
			}
		}

		// Processa como componente individual
		if err := s.processHardwareComponent(machineID, item); err != nil {
			s.logger.Printf("Erro ao processar componente %d: %v", i+1, err)
		}
	}

	s.logger.Printf("Processamento do hardware inventory finalizado")
	return nil
}

func (s *service) processHardwareComponent(machineID string, component map[string]interface{}) error {
	// Converte o mapa genérico para a estrutura de hardware
	var hardwareData HardwareData

	// Extração segura dos dados
	if componentType, ok := component["component_type"].(string); ok {
		hardwareData.ComponentType = strings.TrimSpace(componentType)
	}

	if manufacturer, ok := component["manufacturer"].(string); ok {
		hardwareData.Manufacturer = strings.TrimSpace(manufacturer)
	}

	if model, ok := component["model"].(string); ok {
		hardwareData.Model = strings.TrimSpace(model)
	}

	if serialNumber, ok := component["serial_number"].(string); ok {
		hardwareData.SerialNumber = strings.TrimSpace(serialNumber)
	}

	if capacity, ok := component["capacity"].(string); ok {
		hardwareData.Capacity = strings.TrimSpace(capacity)
	}

	// Para additional_info, aceita tanto map quanto string
	if additionalInfo, ok := component["additional_info"].(map[string]interface{}); ok {
		hardwareData.AdditionalInfo = additionalInfo
	} else if additionalInfoStr, ok := component["additional_info"].(string); ok {
		// Tenta parsear como JSON
		var parsedInfo map[string]interface{}
		if err := json.Unmarshal([]byte(additionalInfoStr), &parsedInfo); err == nil {
			hardwareData.AdditionalInfo = parsedInfo
		}
	}

	s.logger.Printf("Dados de hardware extraídos - Tipo: '%s', Fabricante: '%s', Modelo: '%s', S/N: '%s', Capacidade: '%s'",
		hardwareData.ComponentType, hardwareData.Manufacturer, hardwareData.Model, hardwareData.SerialNumber, hardwareData.Capacity)

	// Validação básica
	if hardwareData.ComponentType == "" {
		s.logger.Printf("Tipo de componente vazio, pulando...")
		return nil
	}

	// Valida se o tipo de componente é aceito
	validTypes := []string{"cpu", "memory", "disk", "gpu", "motherboard", "network", "other"}
	isValidType := false
	for _, validType := range validTypes {
		if hardwareData.ComponentType == validType {
			isValidType = true
			break
		}
	}

	if !isValidType {
		s.logger.Printf("Tipo de componente '%s' não é válido, usando 'other'", hardwareData.ComponentType)
		hardwareData.ComponentType = "other"
	}

	// Insere ou atualiza o componente de hardware
	if err := s.upsertHardware(machineID, hardwareData); err != nil {
		return fmt.Errorf("erro ao inserir/atualizar hardware: %v", err)
	}

	s.logger.Printf("Componente de hardware %s inserido/atualizado com sucesso", hardwareData.ComponentType)
	return nil
}

func (s *service) storeSoftwareInventoryData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script software_inventory retornou erro: %s", result.Error)
		return nil // Não trata como erro fatal
	}

	s.logger.Printf("Processando %d registros do script software_inventory", len(result.Data))

	// Obtém o machine_id atual
	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}

	if machineID == "" {
		hostname := os.Getenv("COMPUTERNAME")
		s.logger.Printf("Máquina %s não encontrada na tabela machines. Pulando processamento do software.", hostname)
		return nil
	}

	s.logger.Printf("Machine ID utilizado: %s", machineID)

	// Processa cada software
	for i, item := range result.Data {
		s.logger.Printf("Processando software %d: %+v", i+1, item)

		// Se o item tem uma chave "output" com JSON, tenta parsear
		if outputStr, hasOutput := item["output"].(string); hasOutput {
			s.logger.Printf("Detectado JSON dentro do campo 'output', tentando parsear...")
			var parsedData []map[string]interface{}
			if err := json.Unmarshal([]byte(strings.TrimSpace(outputStr)), &parsedData); err != nil {
				s.logger.Printf("Erro ao parsear JSON do campo output: %v", err)
				continue
			} else {
				s.logger.Printf("JSON parseado com sucesso: %d softwares encontrados", len(parsedData))
				// Processa cada software do array
				for j, software := range parsedData {
					s.logger.Printf("Processando software %d do array: %+v", j+1, software)
					if err := s.processSoftwareItem(machineID, software); err != nil {
						s.logger.Printf("Erro ao processar software %d: %v", j+1, err)
					}
				}
				continue
			}
		}

		// Processa como software individual
		if err := s.processSoftwareItem(machineID, item); err != nil {
			s.logger.Printf("Erro ao processar software %d: %v", i+1, err)
		}
	}

	s.logger.Printf("Processamento do software inventory finalizado")
	return nil
}

func (s *service) storeAntivirusStatusData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script antivirus_status retornou erro: %s", result.Error)
		return nil
	}

	s.logger.Printf("Processando %d registros do script antivirus_status", len(result.Data))

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		s.logger.Printf("Máquina não encontrada na tabela machines, pulando antivirus_status")
		return nil
	}

	for i, item := range result.Data {
		productName, _ := item["product_name"].(string)
		isEnabled, _ := item["is_enabled"].(bool)
		isUpToDate, _ := item["is_uptodate"].(bool)
		status, _ := item["status"].(string)

		var lastUpdate sql.NullTime
		if ts, ok := item["timestamp"].(string); ok && ts != "" {
			if parsed, err := time.Parse("2006-01-02 15:04:05", ts); err == nil {
				lastUpdate = sql.NullTime{Time: parsed, Valid: true}
			}
		}

		antivirusID := generateUUID()
		if err := s.upsertAntivirus(machineID, antivirusID, productName, isEnabled, isUpToDate, status, lastUpdate); err != nil {
			s.logger.Printf("Erro ao processar antivirus %d (%s): %v", i+1, productName, err)
		}
	}

	return nil
}

func (s *service) processSoftwareItem(machineID string, software map[string]interface{}) error {
	// Converte o mapa genérico para a estrutura de software
	var softwareData SoftwareData

	// Extração segura dos dados
	if softwareName, ok := software["software_name"].(string); ok {
		softwareData.SoftwareName = strings.TrimSpace(softwareName)
	}

	if version, ok := software["version"].(string); ok {
		softwareData.Version = strings.TrimSpace(version)
	} else {
		// Se não tem versão, usa string vazia
		softwareData.Version = ""
	}

	if publisher, ok := software["publisher"].(string); ok {
		softwareData.Publisher = strings.TrimSpace(publisher)
	}

	if installDate, ok := software["install_date"].(string); ok {
		softwareData.InstallDate = strings.TrimSpace(installDate)
	} else if software["install_date"] == nil {
		// Se install_date for explicitamente nil, deixa vazio
		softwareData.InstallDate = ""
	}

	if installPath, ok := software["install_path"].(string); ok {
		softwareData.InstallPath = strings.TrimSpace(installPath)
	} else if software["install_path"] == nil {
		// Se install_path for explicitamente nil, deixa vazio
		softwareData.InstallPath = ""
	}

	// Para size_mb, precisa tratar diferentes tipos numéricos e null
	if sizeMB, ok := software["size_mb"].(float64); ok {
		softwareData.SizeMB = int(sizeMB)
	} else if sizeMB, ok := software["size_mb"].(int); ok {
		softwareData.SizeMB = sizeMB
	} else if software["size_mb"] == nil {
		// Se for explicitamente null, deixa como 0
		softwareData.SizeMB = 0
	}

	// Define tipo de software baseado no nome (pode ser customizado)
	softwareData.SoftwareType = s.determineSoftwareType(softwareData.SoftwareName)

	s.logger.Printf("Dados de software extraídos - Nome: '%s', Versão: '%s', Fabricante: '%s', Tamanho: %d MB",
		softwareData.SoftwareName, softwareData.Version, softwareData.Publisher, softwareData.SizeMB)

	// Validação básica
	if softwareData.SoftwareName == "" {
		s.logger.Printf("Nome do software vazio, pulando...")
		return nil
	}

	// Insere ou atualiza o software
	if err := s.upsertSoftware(machineID, softwareData); err != nil {
		return fmt.Errorf("erro ao inserir/atualizar software: %v", err)
	}

	s.logger.Printf("Software '%s' inserido/atualizado com sucesso", softwareData.SoftwareName)
	return nil
}

func (s *service) determineSoftwareType(softwareName string) string {
	softwareNameLower := strings.ToLower(softwareName)

	// Drivers
	if strings.Contains(softwareNameLower, "driver") || strings.Contains(softwareNameLower, "device") {
		return "driver"
	}

	// Updates
	if strings.Contains(softwareNameLower, "update") || strings.Contains(softwareNameLower, "patch") ||
		strings.Contains(softwareNameLower, "hotfix") || strings.Contains(softwareNameLower, "kb") {
		return "update"
	}

	// System components
	if strings.Contains(softwareNameLower, "microsoft") && (strings.Contains(softwareNameLower, "visual c++") ||
		strings.Contains(softwareNameLower, ".net") || strings.Contains(softwareNameLower, "runtime")) {
		return "system"
	}

	// Default to application
	return "application"
}

// cleanJSONString limpa caracteres problemáticos do JSON
func (s *service) cleanJSONString(jsonStr string) string {
	// Remove caracteres de controle problemáticos
	cleaned := strings.ReplaceAll(jsonStr, "\u0026", "&")
	cleaned = strings.ReplaceAll(cleaned, "\u002f", "/")
	cleaned = strings.ReplaceAll(cleaned, "\u002c", ",")
	cleaned = strings.ReplaceAll(cleaned, "\u0022", "\"")

	// Remove quebras de linha e espaços extras dentro do JSON
	lines := strings.Split(cleaned, "\n")
	var cleanLines []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.Contains(trimmed, "Coletando inventário") {
			cleanLines = append(cleanLines, trimmed)
		}
	}

	return strings.Join(cleanLines, "")
}

// min retorna o menor entre dois inteiros
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *service) getMachineIDByHostname(hostname string) (string, error) {
	var machineID string
	query := "SELECT id FROM machines WHERE hostname = ?"

	if s.config.DB.Driver == "postgres" {
		query = "SELECT id FROM machines WHERE hostname = $1"
	}

	err := s.db.QueryRow(query, hostname).Scan(&machineID)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil // Máquina não encontrada
		}
		return "", err
	}

	return machineID, nil
}

func (s *service) clearMachineHardware(machineID string) error {
	query := "DELETE FROM hardware_inventory WHERE machine_id = ?"

	if s.config.DB.Driver == "postgres" {
		query = "DELETE FROM hardware_inventory WHERE machine_id = $1"
	}

	_, err := s.db.Exec(query, machineID)
	return err
}

func (s *service) upsertHardware(machineID string, data HardwareData) error {
	// Serializa additional_info para JSON
	var additionalInfoJSON []byte
	var err error

	if data.AdditionalInfo != nil {
		additionalInfoJSON, err = json.Marshal(data.AdditionalInfo)
		if err != nil {
			return fmt.Errorf("erro ao serializar additional_info: %v", err)
		}
	}

	// Gera um novo UUID para o hardware se não existir
	hardwareID := generateUUID()

	var upsertSQL string

	if s.config.DB.Driver == "mysql" {
		// Para MySQL, primeiro verifica se já existe
		var existingID string
		checkSQL := "SELECT id FROM hardware_inventory WHERE machine_id = ? AND component_type = ? AND serial_number = ?"
		err := s.db.QueryRow(checkSQL, machineID, data.ComponentType, data.SerialNumber).Scan(&existingID)

		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("erro ao verificar hardware existente: %v", err)
		}

		if err == sql.ErrNoRows {
			// Não existe, faz INSERT
			upsertSQL = `
			INSERT INTO hardware_inventory (id, machine_id, component_type, manufacturer, model, serial_number, capacity, additional_info, last_updated)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NOW())`

			_, err := s.db.Exec(upsertSQL,
				hardwareID,
				machineID,
				data.ComponentType,
				data.Manufacturer,
				data.Model,
				data.SerialNumber,
				data.Capacity,
				string(additionalInfoJSON))
			return err
		} else {
			// Já existe, faz UPDATE
			upsertSQL = `
			UPDATE hardware_inventory SET
				manufacturer = ?,
				model = ?,
				capacity = ?,
				additional_info = ?,
				last_updated = NOW()
			WHERE id = ?`

			_, err := s.db.Exec(upsertSQL,
				data.Manufacturer,
				data.Model,
				data.Capacity,
				string(additionalInfoJSON),
				existingID)
			return err
		}
	} else {
		// PostgreSQL - usa ON CONFLICT
		upsertSQL = `
		INSERT INTO hardware_inventory (id, machine_id, component_type, manufacturer, model, serial_number, capacity, additional_info, last_updated)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, CURRENT_TIMESTAMP)
		ON CONFLICT (machine_id, component_type, serial_number)
		DO UPDATE SET
			manufacturer = EXCLUDED.manufacturer,
			model = EXCLUDED.model,
			capacity = EXCLUDED.capacity,
			additional_info = EXCLUDED.additional_info,
			last_updated = CURRENT_TIMESTAMP`

		_, err := s.db.Exec(upsertSQL,
			hardwareID,
			machineID,
			data.ComponentType,
			data.Manufacturer,
			data.Model,
			data.SerialNumber,
			data.Capacity,
			string(additionalInfoJSON))
		return err
	}
}

func (s *service) upsertMachine(data MachineData) (string, error) {
	var machineID string

	if s.config.DB.Driver == "mysql" {
		// Para MySQL, primeiro verifica se já existe
		var existingID string
		checkSQL := "SELECT id FROM machines WHERE hostname = ?"

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.db.QueryRowContext(ctx, checkSQL, data.Hostname).Scan(&existingID)
		cancel()

		if err != nil && err != sql.ErrNoRows {
			return "", fmt.Errorf("erro ao verificar máquina existente: %v", err)
		}

		if err == sql.ErrNoRows {
			// Não existe, faz INSERT com UUID gerado
			machineID = generateUUID()
			insertSQL := `
			INSERT INTO machines (id, hostname, ip_address, mac_address, domain_name, status, last_seen, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, NOW(), NOW())`

			_, err := s.execWithRetry(insertSQL,
				machineID,
				data.Hostname,
				data.IPAddress,
				data.MACAddress,
				data.DomainName,
				data.Status)
			if err != nil {
				return "", err
			}
		} else {
			// Já existe, faz UPDATE
			machineID = existingID
			updateSQL := `
			UPDATE machines SET
				ip_address = ?,
				mac_address = ?,
				domain_name = ?,
				status = ?,
				last_seen = NOW(),
				updated_at = NOW()
			WHERE id = ?`

			_, err := s.execWithRetry(updateSQL,
				data.IPAddress,
				data.MACAddress,
				data.DomainName,
				data.Status,
				machineID)
			if err != nil {
				return "", err
			}
		}
	} else {
		// PostgreSQL - usa ON CONFLICT com RETURNING
		machineID = generateUUID()
		upsertSQL := `
		INSERT INTO machines (id, hostname, ip_address, mac_address, domain_name, status, last_seen, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT (hostname)
		DO UPDATE SET
			ip_address = EXCLUDED.ip_address,
			mac_address = EXCLUDED.mac_address,
			domain_name = EXCLUDED.domain_name,
			status = EXCLUDED.status,
			last_seen = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP
		RETURNING id`

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.db.QueryRowContext(ctx, upsertSQL,
			machineID,
			data.Hostname,
			data.IPAddress,
			data.MACAddress,
			data.DomainName,
			data.Status).Scan(&machineID)
		cancel()

		if err != nil {
			return "", err
		}
	}

	return machineID, nil
}

func (s *service) storeGenericData(result *ScriptResult) error {
	// Cria tabela se não existir
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS script_logs (
		id INT AUTO_INCREMENT PRIMARY KEY,
		script_name VARCHAR(255) NOT NULL,
		data JSON,
		error_message TEXT,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
	)`

	if s.config.DB.Driver == "postgres" {
		createTableSQL = `
		CREATE TABLE IF NOT EXISTS script_logs (
			id SERIAL PRIMARY KEY,
			script_name VARCHAR(255) NOT NULL,
			data JSONB,
			error_message TEXT,
			timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`
	}

	if _, err := s.db.Exec(createTableSQL); err != nil {
		return fmt.Errorf("erro ao criar tabela: %v", err)
	}

	// Serializa os dados para JSON
	dataJSON, err := json.Marshal(result.Data)
	if err != nil {
		return fmt.Errorf("erro ao serializar dados: %v", err)
	}

	// Insere os dados
	insertSQL := `
	INSERT INTO script_logs (script_name, data, error_message, timestamp)
	VALUES (?, ?, ?, ?)`

	if s.config.DB.Driver == "postgres" {
		insertSQL = `
		INSERT INTO script_logs (script_name, data, error_message, timestamp)
		VALUES ($1, $2, $3, $4)`
	}

	_, err = s.db.Exec(insertSQL, result.ScriptName, string(dataJSON), result.Error, result.Timestamp)
	return err
}

func (s *service) upsertSoftware(machineID string, data SoftwareData) error {
	// Gera um novo UUID para o software se não existir
	softwareID := generateUUID()

	// Converte install_date para formato de data apropriado
	var installDate sql.NullString
	if data.InstallDate != "" {
		// Tenta converter a data do formato YYYYMMDD para YYYY-MM-DD
		if len(data.InstallDate) == 8 && strings.Count(data.InstallDate, "-") == 0 {
			formattedDate := fmt.Sprintf("%s-%s-%s",
				data.InstallDate[:4],
				data.InstallDate[4:6],
				data.InstallDate[6:8])
			installDate = sql.NullString{String: formattedDate, Valid: true}
		} else if matched, _ := regexp.MatchString(`^\d{4}-\d{2}-\d{2}$`, data.InstallDate); matched {
			installDate = sql.NullString{String: data.InstallDate, Valid: true}
		} else {
			installDate = sql.NullString{Valid: false}
		}
	} else {
		installDate = sql.NullString{Valid: false}
	}

	// Trata campos que podem ser vazios usando sql.Null types
	var publisher sql.NullString
	if data.Publisher != "" {
		publisher = sql.NullString{String: data.Publisher, Valid: true}
	} else {
		publisher = sql.NullString{Valid: false}
	}

	var installPath sql.NullString
	if data.InstallPath != "" {
		installPath = sql.NullString{String: data.InstallPath, Valid: true}
	} else {
		installPath = sql.NullString{Valid: false}
	}

	var sizeMB sql.NullInt32
	if data.SizeMB > 0 {
		sizeMB = sql.NullInt32{Int32: int32(data.SizeMB), Valid: true}
	} else {
		sizeMB = sql.NullInt32{Valid: false}
	}

	// Log dos parâmetros para debug
	s.logger.Printf("Inserting software: ID=%s, MachineID=%s, Name=%s, Version=%s, Publisher.Valid=%v, InstallDate.Valid=%v, InstallPath.Valid=%v, SizeMB.Valid=%v",
		softwareID, machineID, data.SoftwareName, data.Version, publisher.Valid, installDate.Valid, installPath.Valid, sizeMB.Valid)

	if s.config.DB.Driver == "mysql" {
		// Para MySQL, primeiro verifica se já existe
		var existingID string
		checkSQL := "SELECT id FROM software_inventory WHERE machine_id = ? AND software_name = ? AND version = ?"
		err := s.db.QueryRow(checkSQL, machineID, data.SoftwareName, data.Version).Scan(&existingID)

		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("erro ao verificar software existente: %v", err)
		}

		if err == sql.ErrNoRows {
			// Não existe, faz INSERT - usando sql.Null types para campos nullable
			insertSQL := `
			INSERT INTO software_inventory (id, machine_id, software_name, version, publisher, install_date, install_path, size_mb, software_type)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

			s.logger.Printf("Executing INSERT SQL: %s", insertSQL)

			_, err := s.db.Exec(insertSQL,
				softwareID,        // 1
				machineID,         // 2
				data.SoftwareName, // 3
				data.Version,      // 4
				publisher,         // 5
				installDate,       // 6
				installPath,       // 7
				sizeMB,            // 8
				data.SoftwareType) // 9

			if err != nil {
				s.logger.Printf("INSERT failed with error: %v", err)
				return err
			}
			s.logger.Printf("INSERT successful")
			return nil
		} else {
			// Já existe, faz UPDATE
			updateSQL := `
			UPDATE software_inventory SET
				publisher = ?,
				install_date = ?,
				install_path = ?,
				size_mb = ?,
				software_type = ?,
				last_updated = NOW()
			WHERE id = ?`

			s.logger.Printf("Executing UPDATE SQL: %s", updateSQL)

			_, err := s.db.Exec(updateSQL,
				publisher,
				installDate,
				installPath,
				sizeMB,
				data.SoftwareType,
				existingID)

			if err != nil {
				s.logger.Printf("UPDATE failed with error: %v", err)
				return err
			}
			s.logger.Printf("UPDATE successful")
			return nil
		}
	} else {
		// PostgreSQL - usa ON CONFLICT com CURRENT_TIMESTAMP
		upsertSQL := `
		INSERT INTO software_inventory (id, machine_id, software_name, version, publisher, install_date, install_path, size_mb, software_type)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (machine_id, software_name, version)
		DO UPDATE SET
			publisher = EXCLUDED.publisher,
			install_date = EXCLUDED.install_date,
			install_path = EXCLUDED.install_path,
			size_mb = EXCLUDED.size_mb,
			software_type = EXCLUDED.software_type,
			last_updated = CURRENT_TIMESTAMP`

		_, err := s.db.Exec(upsertSQL,
			softwareID,
			machineID,
			data.SoftwareName,
			data.Version,
			publisher,
			installDate,
			installPath,
			sizeMB,
			data.SoftwareType)
		return err
	}
}

// getCargaHorariaHoje retorna a carga horária do funcionário para hoje
func (s *service) getCargaHorariaHoje(funcionarioID string) (*CargaHoraria, error) {
	diasSemana := map[time.Weekday]string{
		time.Sunday:    "domingo",
		time.Monday:    "segunda",
		time.Tuesday:   "terca",
		time.Wednesday: "quarta",
		time.Thursday:  "quinta",
		time.Friday:    "sexta",
		time.Saturday:  "sabado",
	}

	hoje := time.Now().Weekday()
	diaSemanaStr := diasSemana[hoje]

	query := `
        SELECT dia_semana, horario_inicio, horario_fim, intervalo_inicio, intervalo_fim
        FROM carga_horaria
        WHERE funcionario_id = ? AND dia_semana = ? AND ativo = 1
    `

	if s.config.DB.Driver == "postgres" {
		query = `
            SELECT dia_semana, horario_inicio, horario_fim, intervalo_inicio, intervalo_fim
            FROM carga_horaria
            WHERE funcionario_id = $1 AND dia_semana = $2 AND ativo = true
        `
	}

	var ch CargaHoraria
	var horarioInicio, horarioFim string
	var intervaloInicio, intervaloFim sql.NullString

	err := s.db.QueryRow(query, funcionarioID, diaSemanaStr).Scan(
		&ch.DiaSemana, &horarioInicio, &horarioFim, &intervaloInicio, &intervaloFim,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // Sem carga horária configurada para hoje
		}
		return nil, err
	}

	// Parse dos horários
	agora := time.Now()
	layout := "15:04:05"

	inicio, err := time.Parse(layout, horarioInicio)
	if err != nil {
		return nil, err
	}
	ch.HorarioInicio = time.Date(agora.Year(), agora.Month(), agora.Day(),
		inicio.Hour(), inicio.Minute(), inicio.Second(), 0, agora.Location())

	fim, err := time.Parse(layout, horarioFim)
	if err != nil {
		return nil, err
	}
	ch.HorarioFim = time.Date(agora.Year(), agora.Month(), agora.Day(),
		fim.Hour(), fim.Minute(), fim.Second(), 0, agora.Location())

	if intervaloInicio.Valid {
		intInicio, err := time.Parse(layout, intervaloInicio.String)
		if err == nil {
			t := time.Date(agora.Year(), agora.Month(), agora.Day(),
				intInicio.Hour(), intInicio.Minute(), intInicio.Second(), 0, agora.Location())
			ch.IntervaloInicio = &t
		}
	}

	if intervaloFim.Valid {
		intFim, err := time.Parse(layout, intervaloFim.String)
		if err == nil {
			t := time.Date(agora.Year(), agora.Month(), agora.Day(),
				intFim.Hour(), intFim.Minute(), intFim.Second(), 0, agora.Location())
			ch.IntervaloFim = &t
		}
	}

	return &ch, nil
}

// getHorasExtrasAtivas retorna as horas extras ativas para hoje
func (s *service) getHorasExtrasAtivas(funcionarioID string, machineID string) ([]HoraExtra, error) {
	hoje := time.Now().Format("2006-01-02")

	query := `
        SELECT id, data_liberacao, horario_inicio, horario_fim
        FROM horas_extras
        WHERE funcionario_id = ? AND machine_id = ?
        AND data_liberacao = ? AND status = 'ativa'
        ORDER BY horario_inicio
    `

	if s.config.DB.Driver == "postgres" {
		query = `
            SELECT id, data_liberacao, horario_inicio, horario_fim
            FROM horas_extras
            WHERE funcionario_id = $1 AND machine_id = $2
            AND data_liberacao = $3 AND status = 'ativa'
            ORDER BY horario_inicio
        `
	}

	rows, err := s.db.Query(query, funcionarioID, machineID, hoje)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var horasExtras []HoraExtra
	layout := "15:04:05"
	hojeParsed := time.Now()

	for rows.Next() {
		var he HoraExtra
		var dataLib, hrInicio, hrFim string

		err := rows.Scan(&he.ID, &dataLib, &hrInicio, &hrFim)
		if err != nil {
			continue
		}

		dataLibParsed, _ := time.Parse("2006-01-02", dataLib)
		he.DataLiberacao = dataLibParsed

		inicio, err := time.Parse(layout, hrInicio)
		if err == nil {
			he.HorarioInicio = time.Date(hojeParsed.Year(), hojeParsed.Month(), hojeParsed.Day(),
				inicio.Hour(), inicio.Minute(), inicio.Second(), 0, hojeParsed.Location())
		}

		fim, err := time.Parse(layout, hrFim)
		if err == nil {
			he.HorarioFim = time.Date(hojeParsed.Year(), hojeParsed.Month(), hojeParsed.Day(),
				fim.Hour(), fim.Minute(), fim.Second(), 0, hojeParsed.Location())
		}

		horasExtras = append(horasExtras, he)
	}

	return horasExtras, nil
}

// getAppsImprodutivos retorna lista de aplicativos improdutivos da máquina
func (s *service) getAppsImprodutivos(machineID string) ([]AppImprodutivo, error) {
	query := `
        SELECT DISTINCT application_name
        FROM aplicativos_improdutivos
        WHERE machine_id = ? AND ativo = 1
    `

	if s.config.DB.Driver == "postgres" {
		query = `
            SELECT DISTINCT application_name
            FROM aplicativos_improdutivos
            WHERE machine_id = $1 AND ativo = true
        `
	}

	rows, err := s.db.Query(query, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []AppImprodutivo
	for rows.Next() {
		var app AppImprodutivo
		if err := rows.Scan(&app.ApplicationName); err != nil {
			continue
		}
		apps = append(apps, app)
	}

	return apps, nil
}

// getFuncionarioIDByUsername retorna o ID do funcionário pelo username
// Se não existir, cria automaticamente
func (s *service) getFuncionarioIDByUsername(username string) (string, error) {
	var funcionarioID string
	query := "SELECT id FROM funcionarios WHERE username = ? AND ativo = 1"

	if s.config.DB.Driver == "postgres" {
		query = "SELECT id FROM funcionarios WHERE username = $1 AND ativo = true"
	}

	err := s.db.QueryRow(query, username).Scan(&funcionarioID)
	if err != nil {
		if err == sql.ErrNoRows {
			// Funcionário não existe, cria automaticamente
			s.logger.Printf("Funcionário '%s' não encontrado, criando automaticamente...", username)
			return s.criarFuncionarioAutomatico(username)
		}
		return "", err
	}

	return funcionarioID, nil
}

// criarFuncionarioAutomatico cria um novo funcionário automaticamente
func (s *service) criarFuncionarioAutomatico(username string) (string, error) {
	funcionarioID := generateUUID()

	var insertSQL string
	if s.config.DB.Driver == "mysql" {
		insertSQL = `
        INSERT INTO funcionarios (id, username, ativo, created_at, updated_at)
        VALUES (?, ?, 1, NOW(), NOW())`

		_, err := s.db.Exec(insertSQL, funcionarioID, strings.ToLower(strings.TrimSpace(username)))
		if err != nil {
			return "", fmt.Errorf("erro ao criar funcionário automaticamente: %v", err)
		}
	} else {
		insertSQL = `
        INSERT INTO funcionarios (id, username, ativo, created_at, updated_at)
        VALUES ($1, $2, true, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`

		_, err := s.db.Exec(insertSQL, funcionarioID, strings.ToLower(strings.TrimSpace(username)))
		if err != nil {
			return "", fmt.Errorf("erro ao criar funcionário automaticamente: %v", err)
		}
	}

	s.logger.Printf("✅ Funcionário '%s' criado automaticamente com ID: %s", username, funcionarioID)
	return funcionarioID, nil
}

// getCurrentUsername retorna o nome de exibição (FullName) do usuário atual do sistema.
// Usa os/user que no Windows retorna o FullName no campo Name.
// Fallback para os.Getenv("USERNAME") caso a API falhe ou o FullName esteja vazio.
func getCurrentUsername() string {
	u, err := user.Current()
	if err == nil && u.Name != "" {
		return u.Name
	}
	return os.Getenv("USERNAME")
}

// sincronizarFuncionarioAtual garante que o funcionário atual existe no banco
// Se o username da máquina mudou, atualiza o registro existente ao invés de criar um novo
func (s *service) sincronizarFuncionarioAtual() error {
	username := getCurrentUsername()
	if username == "" {
		return fmt.Errorf("não foi possível obter USERNAME do sistema")
	}

	username = strings.ToLower(strings.TrimSpace(username))
	s.logger.Printf("Sincronizando funcionário: %s", username)

	// Primeiro verifica se já existe com o username atual
	var funcionarioID string
	query := "SELECT id FROM funcionarios WHERE username = ? AND ativo = 1"
	if s.config.DB.Driver == "postgres" {
		query = "SELECT id FROM funcionarios WHERE username = $1 AND ativo = true"
	}

	err := s.db.QueryRow(query, username).Scan(&funcionarioID)
	if err == nil {
		// Funcionário já existe com o username atual
		s.logger.Printf("✅ Funcionário sincronizado: %s (ID: %s)", username, funcionarioID)
		return nil
	}

	if err != sql.ErrNoRows {
		return fmt.Errorf("erro ao buscar funcionário: %v", err)
	}

	// Username não encontrado - verifica se o machine_id atual tem um funcionário associado (username antigo)
	machineID, machineErr := s.getCurrentMachineID()
	if machineErr == nil && machineID != "" {
		var oldFuncionarioID, oldUsername string
		queryOld := `
			SELECT DISTINCT f.id, f.username FROM funcionarios f
			INNER JOIN application_usage au ON au.funcionario_id = f.id
			WHERE au.machine_id = ? AND f.ativo = 1
			ORDER BY au.created_at DESC LIMIT 1`
		if s.config.DB.Driver == "postgres" {
			queryOld = `
				SELECT DISTINCT f.id, f.username FROM funcionarios f
				INNER JOIN application_usage au ON au.funcionario_id = f.id
				WHERE au.machine_id = $1 AND f.ativo = true
				ORDER BY au.created_at DESC LIMIT 1`
		}

		errOld := s.db.QueryRow(queryOld, machineID).Scan(&oldFuncionarioID, &oldUsername)
		if errOld == nil && oldFuncionarioID != "" {
			// Encontrou funcionário com username antigo nesta máquina - atualiza o username
			s.logger.Printf("🔄 Username mudou de '%s' para '%s' na máquina %s, atualizando...", oldUsername, username, machineID)

			updateQuery := "UPDATE funcionarios SET username = ?, updated_at = NOW() WHERE id = ?"
			if s.config.DB.Driver == "postgres" {
				updateQuery = "UPDATE funcionarios SET username = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2"
			}

			_, errUpdate := s.db.Exec(updateQuery, username, oldFuncionarioID)
			if errUpdate != nil {
				s.logger.Printf("⚠️ Erro ao atualizar username do funcionário: %v", errUpdate)
			} else {
				s.logger.Printf("✅ Username do funcionário atualizado: '%s' -> '%s' (ID: %s)", oldUsername, username, oldFuncionarioID)
				return nil
			}
		}
	}

	// Nenhum funcionário associado a esta máquina, cria um novo
	funcionarioID, err = s.criarFuncionarioAutomatico(username)
	if err != nil {
		return fmt.Errorf("erro ao sincronizar funcionário: %v", err)
	}

	if funcionarioID == "" {
		return fmt.Errorf("erro: funcionário não foi criado")
	}

	s.logger.Printf("✅ Funcionário sincronizado: %s (ID: %s)", username, funcionarioID)
	return nil
}

// calcularHorasTrabalhadasHoje calcula o total de horas trabalhadas pelo funcionário hoje
func (s *service) calcularHorasTrabalhadasHoje(funcionarioID string, machineID string) (*HorasTrabalhadasDia, error) {
	// Busca o limite de horas E o username do funcionário
	var totalHoras sql.NullFloat64
	var username string
	queryLimite := "SELECT total_horas, username FROM funcionarios WHERE id = ? AND ativo = 1"
	if s.config.DB.Driver == "postgres" {
		queryLimite = "SELECT total_horas, username FROM funcionarios WHERE id = $1 AND ativo = true"
	}

	err := s.db.QueryRow(queryLimite, funcionarioID).Scan(&totalHoras, &username)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("funcionário não encontrado")
		}
		return nil, err
	}

	if !totalHoras.Valid || totalHoras.Float64 == 0 {
		return nil, fmt.Errorf("funcionário não tem limite de horas configurado")
	}

	limiteSegundos := int64(totalHoras.Float64 * 3600) // converte horas para segundos

	// Calcula o total trabalhado hoje usando o username diretamente
	hoje := time.Now().Format("2006-01-02")
	query := `
        SELECT COALESCE(SUM(duration_seconds), 0) as total_segundos
        FROM user_activity
        WHERE machine_id = ?
        AND username = ?
        AND activity_type = 'active'
        AND DATE(start_time) = ?
    `

	if s.config.DB.Driver == "postgres" {
		query = `
            SELECT COALESCE(SUM(duration_seconds), 0) as total_segundos
            FROM user_activity
            WHERE machine_id = $1
            AND username = $2
            AND activity_type = 'active'
            AND DATE(start_time) = $3
        `
	}

	var totalSegundos int64
	err = s.db.QueryRow(query, machineID, username, hoje).Scan(&totalSegundos)
	if err != nil {
		return nil, err
	}

	return &HorasTrabalhadasDia{
		TotalSegundos:  totalSegundos,
		LimiteSegundos: limiteSegundos,
	}, nil
}

// temHoraExtraAtivaAgora verifica se há hora extra ativa no momento atual
func (s *service) temHoraExtraAtivaAgora(funcionarioID string, machineID string) (bool, error) {
	hoje := time.Now().Format("2006-01-02")
	agora := time.Now()
	horaAtual := agora.Format("15:04:05")

	query := `
        SELECT COUNT(*)
        FROM horas_extras
        WHERE funcionario_id = ?
        AND machine_id = ?
        AND data_liberacao = ?
        AND horario_inicio <= ?
        AND horario_fim >= ?
        AND status = 'ativa'
    `

	if s.config.DB.Driver == "postgres" {
		query = `
            SELECT COUNT(*)
            FROM horas_extras
            WHERE funcionario_id = $1
            AND machine_id = $2
            AND data_liberacao = $3
            AND horario_inicio <= $4
            AND horario_fim >= $5
            AND status = 'ativa'
        `
	}

	var count int
	err := s.db.QueryRow(query, funcionarioID, machineID, hoje, horaAtual, horaAtual).Scan(&count)
	if err != nil {
		return false, err
	}

	return count > 0, nil
}

// verificaEBloqueiaTela verifica se o funcionário atingiu o limite de horas e bloqueia se necessário
func (s *service) verificaEBloqueiaTela() {
	username := getCurrentUsername()
	if username == "" {
		return
	}

	username = strings.ToLower(strings.TrimSpace(username))

	// 🔥 PROTEGE ACESSO AO BANCO
	funcionarioID, err := s.getFuncionarioIDByUsername(username)

	if err != nil || funcionarioID == "" {
		// Não loga erro para não poluir o log
		return
	}

	machineID, err := s.getCurrentMachineID()

	if err != nil || machineID == "" {
		return
	}

	horas, err := s.calcularHorasTrabalhadasHoje(funcionarioID, machineID)

	if err != nil {
		// Não loga erro de "sem limite configurado" para não poluir
		if !strings.Contains(err.Error(), "limite de horas configurado") {
			s.logger.Printf("Erro ao calcular horas trabalhadas: %v", err)
		}
		return
	}

	s.logger.Printf("Horas trabalhadas hoje: %.2f/%.2f horas (%.0f%%)",
		float64(horas.TotalSegundos)/3600,
		float64(horas.LimiteSegundos)/3600,
		float64(horas.TotalSegundos)/float64(horas.LimiteSegundos)*100)

	if horas.TotalSegundos < horas.LimiteSegundos {
		return
	}

	// Verifica hora extra com proteção
	temHoraExtra, err := s.temHoraExtraAtivaAgora(funcionarioID, machineID)

	if err != nil {
		s.logger.Printf("Erro ao verificar hora extra: %v", err)
	}

	if temHoraExtra {
		s.logger.Printf("Limite de horas atingido mas há hora extra ativa - não bloqueando")
		return
	}

	horasTrabalhadas := float64(horas.TotalSegundos) / 3600
	horasLimite := float64(horas.LimiteSegundos) / 3600
	mensagem := fmt.Sprintf("Você atingiu o limite de %.1f horas trabalhadas hoje (limite: %.1f horas). Para continuar trabalhando, solicite hora extra ao seu gestor.", horasTrabalhadas, horasLimite)

	s.logger.Printf("🔒 BLOQUEANDO TELA: %s", mensagem)
	s.bloquearTela(mensagem)
}

// ============================================
// FUNÇÃO MODIFICADA: bloquearTela
// ============================================
func (s *service) bloquearTela(mensagem string) {
	s.logger.Printf("🔒 BLOQUEANDO TELA: %s", mensagem)

	result := s.executeScript("lock_screen", "-Message", mensagem, "-Duration", "20")

	if result.Error != "" {
		s.logger.Printf("Erro ao executar bloqueio de tela: %s", result.Error)
	} else {
		s.logger.Printf("Tela bloqueada: %s", mensagem)
	}
}

// ============================================
// FUNÇÃO MODIFICADA: tirarPrintImprodutivo
// ============================================
func (s *service) tirarPrintImprodutivo(appName, processName, username string) {
	s.logger.Printf("📸 Tirando print do app improdutivo: %s (processo: %s, usuário: %s)",
		appName, processName, username)

	result := s.executeScript("screenshot_improdutivo")

	if result.Error != "" {
		s.logger.Printf("❌ Erro ao capturar print improdutivo: %s", result.Error)
		return
	}

	if len(result.Data) == 0 {
		s.logger.Printf("❌ Nenhum dado retornado pelo script")
		return
	}

	data := result.Data[0]
	data["trigger_type"] = "unproductive_app"
	data["application_name"] = appName
	data["process_name"] = processName

	if err := s.storePrintImprodutivo(data); err != nil {
		s.logger.Printf("❌ Erro ao salvar print improdutivo no banco: %v", err)
	} else {
		s.logger.Printf("✅ Print improdutivo salvo: %s (processo: %s)", appName, processName)
	}
}

// storePrintImprodutivo salva print improdutivo no banco
func (s *service) storePrintImprodutivo(data map[string]interface{}) error {
	machineID, err := s.getCurrentMachineID()
	if err != nil || machineID == "" {
		return fmt.Errorf("machine_id não encontrado")
	}

	// 🔹 Extração dos dados do JSON retornado pelo script
	username, _ := data["username"].(string)
	appName, _ := data["application_name"].(string)
	processName, _ := data["process_name"].(string)
	hostname, _ := data["hostname"].(string)

	// Paths do screenshot
	screenshotPath, _ := data["ftp_screenshot_uri"].(string)
	thumbPath, _ := data["ftp_thumb_uri"].(string)

	// Informações técnicas
	compressionType, _ := data["compression_type"].(string)

	// Monitor count
	monitorCount := 1
	if v, ok := data["monitor_count"].(float64); ok {
		monitorCount = int(v)
	}

	// 🔹 Validação básica
	if screenshotPath == "" {
		return fmt.Errorf("screenshot_path vazio - upload FTP pode ter falhado")
	}

	// File size
	var fileSize sql.NullInt64
	if v, ok := data["file_size_bytes"].(float64); ok {
		fileSize = sql.NullInt64{Int64: int64(v), Valid: true}
	}

	// Capture time
	captureTimeStr, _ := data["capture_time"].(string)
	captureTime, err := time.Parse("2006-01-02T15:04:05", captureTimeStr)
	if err != nil {
		s.logger.Printf("⚠️ Erro ao parsear capture_time '%s', usando NOW(): %v", captureTimeStr, err)
		captureTime = time.Now()
	}

	// 🔹 Busca funcionario_id pelo username
	funcionarioID, err := s.getFuncionarioIDByUsername(username)
	if err != nil {
		s.logger.Printf("⚠️ Erro ao buscar funcionário '%s': %v", username, err)
		// Não é erro fatal, continua sem funcionario_id
	}

	// 🔹 Calcula tempo de uso do app (busca na application_usage)
	var tempoUsoMinutos sql.NullInt32
	queryTempo := `
        SELECT COALESCE(duration_seconds, 0) / 60 as minutos
        FROM application_usage
        WHERE machine_id = ? AND username = ? AND application_name = ? AND is_active = 1
        ORDER BY start_time DESC
        LIMIT 1
    `
	if s.config.DB.Driver == "postgres" {
		queryTempo = `
            SELECT COALESCE(duration_seconds, 0) / 60 as minutos
            FROM application_usage
            WHERE machine_id = $1 AND username = $2 AND application_name = $3 AND is_active = true
            ORDER BY start_time DESC
            LIMIT 1
        `
	}

	var minutos int32
	err = s.db.QueryRow(queryTempo, machineID, username, appName).Scan(&minutos)
	if err == nil {
		tempoUsoMinutos = sql.NullInt32{Int32: minutos, Valid: true}
	} else if err != sql.ErrNoRows {
		s.logger.Printf("⚠️ Erro ao buscar tempo de uso: %v", err)
	}

	// 🔹 Gera UUID para o registro
	id := generateUUID()

	// 🔹 LOG COMPLETO antes do INSERT
	s.logger.Printf(`
=== PREPARANDO INSERT PRINT IMPRODUTIVO ===
ID: %s
MachineID: %s
Username: %s
FuncionarioID: %s (valid: %t)
ApplicationName: %s
ProcessName: %s
ScreenshotPath: %s
ThumbPath: %s
CaptureTime: %s
FileSize: %d bytes (valid: %t)
CompressionType: %s
MonitorCount: %d
TempoUso: %d minutos (valid: %t)
Hostname: %s
=========================================
`,
		id, machineID, username, funcionarioID, funcionarioID != "",
		appName, processName, screenshotPath, thumbPath,
		captureTime.Format("2006-01-02 15:04:05"),
		fileSize.Int64, fileSize.Valid,
		compressionType, monitorCount,
		tempoUsoMinutos.Int32, tempoUsoMinutos.Valid,
		hostname)

	// 🔹 INSERT no banco
	if s.config.DB.Driver == "mysql" {
		insertSQL := `
        INSERT INTO prints_improdutivas
            (id, machine_id, username, funcionario_id, application_name, process_name,
             screenshot_path, screenshot_thumb_path, capture_time, file_size_bytes,
             compression_type, monitor_count, trigger_reason, tempo_uso_minutos, hostname)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'app_improdutivo', ?, ?)`

		_, err = s.db.Exec(insertSQL,
			id,
			machineID,
			username,
			sql.NullString{String: funcionarioID, Valid: funcionarioID != ""},
			appName,
			processName,
			screenshotPath,
			thumbPath,
			captureTime,
			fileSize,
			compressionType,
			monitorCount,
			tempoUsoMinutos,
			hostname)
	} else {
		// PostgreSQL
		insertSQL := `
        INSERT INTO prints_improdutivas
            (id, machine_id, username, funcionario_id, application_name, process_name,
             screenshot_path, screenshot_thumb_path, capture_time, file_size_bytes,
             compression_type, monitor_count, trigger_reason, tempo_uso_minutos, hostname)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'app_improdutivo',$13,$14)`

		_, err = s.db.Exec(insertSQL,
			id,
			machineID,
			username,
			sql.NullString{String: funcionarioID, Valid: funcionarioID != ""},
			appName,
			processName,
			screenshotPath,
			thumbPath,
			captureTime,
			fileSize,
			compressionType,
			monitorCount,
			tempoUsoMinutos,
			hostname)
	}

	if err != nil {
		return fmt.Errorf("erro ao inserir print improdutivo no banco: %v", err)
	}

	s.logger.Printf("✅ Print improdutivo salvo no banco: app=%s, processo=%s, usuário=%s, tempo_uso=%d min, path=%s",
		appName, processName, username, tempoUsoMinutos.Int32, screenshotPath)
	return nil
}

func (s *service) upsertAntivirus(machineID, antivirusID, productName string, isEnabled, isUpToDate bool, status string, lastUpdate sql.NullTime) error {
	if s.config.DB.Driver == "mysql" {
		var existingID string
		checkSQL := "SELECT id FROM antivirus_status WHERE machine_id = ? AND product_name = ?"
		err := s.db.QueryRow(checkSQL, machineID, productName).Scan(&existingID)

		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("erro ao verificar antivirus existente: %v", err)
		}

		if err == sql.ErrNoRows {
			insertSQL := `
            INSERT INTO antivirus_status (id, machine_id, product_name, is_enabled, is_uptodate, status, last_update)
            VALUES (?, ?, ?, ?, ?, ?, ?)`
			_, err := s.db.Exec(insertSQL, antivirusID, machineID, productName, isEnabled, isUpToDate, status, lastUpdate)
			return err
		} else {
			updateSQL := `
            UPDATE antivirus_status SET
                is_enabled = ?,
                is_uptodate = ?,
                status = ?,
                last_update = ?
            WHERE id = ?`
			_, err := s.db.Exec(updateSQL, isEnabled, isUpToDate, status, lastUpdate, existingID)
			return err
		}
	} else {
		upsertSQL := `
        INSERT INTO antivirus_status (id, machine_id, product_name, is_enabled, is_uptodate, status, last_update)
        VALUES ($1, $2, $3, $4, $5, $6, $7)
        ON CONFLICT (machine_id, product_name)
        DO UPDATE SET
            is_enabled = EXCLUDED.is_enabled,
            is_uptodate = EXCLUDED.is_uptodate,
            status = EXCLUDED.status,
            last_update = EXCLUDED.last_update`
		_, err := s.db.Exec(upsertSQL, antivirusID, machineID, productName, isEnabled, isUpToDate, status, lastUpdate)
		return err
	}
}

func (s *service) storeSystemMetricsData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script system_metrics retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		s.logger.Printf("Machine não encontrada, pulando system_metrics")
		return nil
	}

	for _, item := range result.Data {
		if outputStr, hasOutput := item["output"].(string); hasOutput {
			var parsedData map[string]interface{}
			if err := json.Unmarshal([]byte(strings.TrimSpace(outputStr)), &parsedData); err == nil {
				item = parsedData
			}
		}

		id := generateUUID()
		tsStr, _ := item["timestamp"].(string)
		cpuUsage, _ := item["cpu_usage_percent"].(float64)
		memUsage, _ := item["memory_usage_percent"].(float64)
		uptime, _ := item["uptime_seconds"].(float64)

		// novos campos
		memTotal, _ := item["memory_total_mb"].(float64)
		memAvailable, _ := item["memory_available_mb"].(float64)
		loadAvg, _ := item["load_average"].(float64)

		// disk_usage e temperature precisam ser serializados em JSON
		diskUsageJSON, _ := json.Marshal(item["disk_usage"])
		temperatureJSON, _ := json.Marshal(item["temperature"])

		metricTimestamp, err := time.Parse("2006-01-02 15:04:05", tsStr)
		if err != nil {
			metricTimestamp = time.Now()
		}

		if s.config.DB.Driver == "mysql" {
			insertSQL := `
            INSERT INTO system_metrics (
                id, machine_id, metric_timestamp,
                cpu_usage_percent, memory_usage_percent, memory_total_mb,
                memory_available_mb, disk_usage, uptime_seconds,
                load_average, temperature
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            ON DUPLICATE KEY UPDATE
                cpu_usage_percent = VALUES(cpu_usage_percent),
                memory_usage_percent = VALUES(memory_usage_percent),
                memory_total_mb = VALUES(memory_total_mb),
                memory_available_mb = VALUES(memory_available_mb),
                disk_usage = VALUES(disk_usage),
                uptime_seconds = VALUES(uptime_seconds),
                load_average = VALUES(load_average),
                temperature = VALUES(temperature),
                created_at = CURRENT_TIMESTAMP`

			_, err = s.db.Exec(insertSQL,
				id, machineID, metricTimestamp,
				cpuUsage, memUsage, int64(memTotal),
				int64(memAvailable), string(diskUsageJSON), int64(uptime),
				loadAvg, string(temperatureJSON),
			)
			if err != nil {
				return fmt.Errorf("erro ao upsert system_metrics: %v", err)
			}

		} else {
			insertSQL := `
            INSERT INTO system_metrics (
                id, machine_id, metric_timestamp,
                cpu_usage_percent, memory_usage_percent, memory_total_mb,
                memory_available_mb, disk_usage, uptime_seconds,
                load_average, temperature
            ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
            ON CONFLICT (machine_id, metric_timestamp)
            DO UPDATE SET
                cpu_usage_percent = EXCLUDED.cpu_usage_percent,
                memory_usage_percent = EXCLUDED.memory_usage_percent,
                memory_total_mb = EXCLUDED.memory_total_mb,
                memory_available_mb = EXCLUDED.memory_available_mb,
                disk_usage = EXCLUDED.disk_usage,
                uptime_seconds = EXCLUDED.uptime_seconds,
                load_average = EXCLUDED.load_average,
                temperature = EXCLUDED.temperature,
                created_at = CURRENT_TIMESTAMP`

			_, err = s.db.Exec(insertSQL,
				id, machineID, metricTimestamp,
				cpuUsage, memUsage, int64(memTotal),
				int64(memAvailable), string(diskUsageJSON), int64(uptime),
				loadAvg, string(temperatureJSON),
			)
			if err != nil {
				return fmt.Errorf("erro ao upsert system_metrics: %v", err)
			}
		}

		s.logger.Printf("Métrica upserted: CPU=%.2f%%, MEM=%.2f%% (total=%dMB, avail=%dMB), Uptime=%ds, Load=%.2f",
			cpuUsage, memUsage, int64(memTotal), int64(memAvailable), int64(uptime), loadAvg)
	}

	return nil
}

func (s *service) storeWindowsServicesData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script windows_services retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		s.logger.Printf("Máquina não encontrada, pulando windows_services")
		return nil
	}

	for _, item := range result.Data {
		id := generateUUID()

		serviceName, _ := item["service_name"].(string)
		displayName, _ := item["display_name"].(string)
		status, _ := item["status"].(string)
		startupType, _ := item["startup_type"].(string)
		serviceType, _ := item["service_type"].(string)

		if s.config.DB.Driver == "mysql" {
			upsertSQL := `
            INSERT INTO windows_services (id, machine_id, service_name, display_name, status, startup_type, service_type, last_updated)
            VALUES (?, ?, ?, ?, ?, ?, ?, NOW())
            ON DUPLICATE KEY UPDATE
                display_name = VALUES(display_name),
                status = VALUES(status),
                startup_type = VALUES(startup_type),
                service_type = VALUES(service_type),
                last_updated = NOW()`

			_, err := s.db.Exec(upsertSQL,
				id, machineID, serviceName, displayName, status, startupType, serviceType)
			if err != nil {
				return fmt.Errorf("erro ao upsert windows_services (MySQL): %v", err)
			}

		} else {
			upsertSQL := `
            INSERT INTO windows_services (id, machine_id, service_name, display_name, status, startup_type, service_type, last_updated)
            VALUES ($1, $2, $3, $4, $5, $6, $7, CURRENT_TIMESTAMP)
            ON CONFLICT (machine_id, service_name)
            DO UPDATE SET
                display_name = EXCLUDED.display_name,
                status = EXCLUDED.status,
                startup_type = EXCLUDED.startup_type,
                service_type = EXCLUDED.service_type,
                last_updated = CURRENT_TIMESTAMP`

			_, err := s.db.Exec(upsertSQL,
				id, machineID, serviceName, displayName, status, startupType, serviceType)
			if err != nil {
				return fmt.Errorf("erro ao upsert windows_services (Postgres): %v", err)
			}
		}

		s.logger.Printf("Serviço %s (%s) atualizado com sucesso", serviceName, displayName)
	}

	return nil
}

func (s *service) storeWindowsUpdatesData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script windows_updates retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		s.logger.Printf("Máquina não encontrada, pulando windows_updates")
		return nil
	}

	for _, item := range result.Data {
		id := generateUUID()

		updateID, _ := item["update_id"].(string)
		description, _ := item["description"].(string)
		status, _ := item["status"].(string)
		category, _ := item["category"].(string)

		var sizeMB sql.NullInt32
		if val, ok := item["size_mb"].(float64); ok {
			sizeMB = sql.NullInt32{Int32: int32(val), Valid: true}
		} else {
			sizeMB = sql.NullInt32{Valid: false}
		}

		// tratar a data
		var installDate time.Time
		if ts, ok := item["install_date"].(string); ok && ts != "" {
			if parsed, err := time.Parse("2006-01-02 15:04:05", ts); err == nil {
				installDate = parsed
			} else {
				installDate = time.Now()
			}
		} else {
			installDate = time.Now()
		}

		if s.config.DB.Driver == "mysql" {
			upsertSQL := `
            INSERT INTO windows_updates (id, machine_id, update_id, title, description, install_date, status, category, size_mb)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
            ON DUPLICATE KEY UPDATE
                title = VALUES(title),
                description = VALUES(description),
                install_date = VALUES(install_date),
                status = VALUES(status),
                category = VALUES(category),
                size_mb = VALUES(size_mb),
                created_at = CURRENT_TIMESTAMP`

			_, err := s.db.Exec(upsertSQL,
				id, machineID, updateID, updateID, description, installDate, status, category, sizeMB)
			if err != nil {
				return fmt.Errorf("erro ao upsert windows_updates (MySQL): %v", err)
			}

		} else {
			upsertSQL := `
            INSERT INTO windows_updates (id, machine_id, update_id, title, description, install_date, status, category, size_mb)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
            ON CONFLICT (machine_id, update_id)
            DO UPDATE SET
                title = EXCLUDED.title,
                description = EXCLUDED.description,
                install_date = EXCLUDED.install_date,
                status = EXCLUDED.status,
                category = EXCLUDED.category,
                size_mb = EXCLUDED.size_mb,
                created_at = CURRENT_TIMESTAMP`

			_, err := s.db.Exec(upsertSQL,
				id, machineID, updateID, updateID, description, installDate, status, category, sizeMB)
			if err != nil {
				return fmt.Errorf("erro ao upsert windows_updates (Postgres): %v", err)
			}
		}

		s.logger.Printf("Update %s instalado em %s atualizado com sucesso", updateID, installDate.Format("2006-01-02"))
	}

	return nil
}

func (s *service) storeUserActivityData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script user_activity retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		s.logger.Printf("Máquina não encontrada na tabela machines. Pulando user_activity.")
		return nil
	}

	parseLayout := "2006-01-02 15:04:05"
	now := time.Now()
	currentDate := now.Format("2006-01-02")

	for i, item := range result.Data {
		username, _ := item["username"].(string)
		activityType, _ := item["activity_type"].(string)

		startTimeStr, _ := item["start_time"].(string)
		endTimeStr, _ := item["end_time"].(string)

		durationSec := int64(0)
		if v, ok := item["duration_seconds"].(float64); ok {
			durationSec = int64(v)
		}

		mouseEvents := int64(0)
		if v, ok := item["mouse_events"].(float64); ok {
			mouseEvents = int64(v)
		}

		keyboardEvents := int64(0)
		if v, ok := item["keyboard_events"].(float64); ok {
			keyboardEvents = int64(v)
		}

		var startTime, endTime time.Time

		// Parse start_time do script
		if t, err := time.Parse(parseLayout, startTimeStr); err == nil {
			startTime = t
		} else {
			startTime = now
		}

		// Parse end_time do script
		if endTimeStr != "" {
			if t, err := time.Parse(parseLayout, endTimeStr); err == nil {
				endTime = t
			}
		}

		// activity_date é SEMPRE a data atual da execução
		activityDate := currentDate

		s.logger.Printf(
			"[DEBUG] Preparando INSERT: machine=%s, user=%s, type=%s, date=%s, start=%s, duration=%d",
			machineID, username, activityType, activityDate, startTime.Format(parseLayout), durationSec,
		)

		if s.config.DB.Driver == "mysql" {
			insertSQL := `
				INSERT INTO user_activity
				(id, machine_id, username, activity_type, activity_date, start_time, end_time,
				 duration_seconds, mouse_events, keyboard_events, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE
					end_time = VALUES(end_time),
					duration_seconds = user_activity.duration_seconds + VALUES(duration_seconds),
					mouse_events = user_activity.mouse_events + VALUES(mouse_events),
					keyboard_events = user_activity.keyboard_events + VALUES(keyboard_events)
			`

			// Gerar ID somente quando for um INSERT novo
			id := generateUUID()

			s.logger.Printf("[DEBUG] Executando SQL MySQL...")

			result, err := s.db.Exec(insertSQL,
				id, machineID, username, activityType, activityDate,
				startTime,
				sql.NullTime{Time: endTime, Valid: !endTime.IsZero()},
				sql.NullInt32{Int32: int32(durationSec), Valid: durationSec > 0},
				sql.NullInt32{Int32: int32(mouseEvents), Valid: true},
				sql.NullInt32{Int32: int32(keyboardEvents), Valid: true},
				now, // created_at é sempre a data-hora atual
			)
			if err != nil {
				s.logger.Printf("❌ ERRO ao inserir user_activity registro %d: %v", i+1, err)
				return fmt.Errorf("erro ao inserir user_activity registro %d: %v", i+1, err)
			}

			rowsAffected, _ := result.RowsAffected()
			lastInsertId, _ := result.LastInsertId()
			s.logger.Printf("✅ SQL executado: RowsAffected=%d, LastInsertId=%d", rowsAffected, lastInsertId)

		} else {
			insertSQL := `
				INSERT INTO user_activity
				(id, machine_id, username, activity_type, activity_date, start_time, end_time,
				 duration_seconds, mouse_events, keyboard_events, created_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
				ON CONFLICT (machine_id, username, activity_type, activity_date)
				DO UPDATE SET
					end_time = EXCLUDED.end_time,
					duration_seconds = user_activity.duration_seconds + EXCLUDED.duration_seconds,
					mouse_events = user_activity.mouse_events + EXCLUDED.mouse_events,
					keyboard_events = user_activity.keyboard_events + EXCLUDED.keyboard_events
			`

			id := generateUUID()

			s.logger.Printf("[DEBUG] Executando SQL PostgreSQL...")

			result, err := s.db.Exec(insertSQL,
				id, machineID, username, activityType, activityDate,
				startTime,
				sql.NullTime{Time: endTime, Valid: !endTime.IsZero()},
				sql.NullInt32{Int32: int32(durationSec), Valid: durationSec > 0},
				sql.NullInt32{Int32: int32(mouseEvents), Valid: true},
				sql.NullInt32{Int32: int32(keyboardEvents), Valid: true},
				now, // created_at é sempre a data-hora atual
			)
			if err != nil {
				s.logger.Printf("❌ ERRO ao inserir user_activity registro %d: %v", i+1, err)
				return fmt.Errorf("erro ao inserir user_activity registro %d: %v", i+1, err)
			}

			rowsAffected, _ := result.RowsAffected()
			s.logger.Printf("✅ SQL executado: RowsAffected=%d", rowsAffected)
		}

		s.logger.Printf("✅ User activity registrada: %s %s (%ds, mouse=%d, kb=%d)",
			username, activityType, int(durationSec), int(mouseEvents), int(keyboardEvents))
	}

	return nil
}

func (s *service) getAppsImprodutivosPorFuncionario(username string) ([]AppImprodutivo, error) {
	// Primeiro busca o funcionário e sua função
	var funcaoID sql.NullString
	queryFuncionario := `
        SELECT funcao_id
        FROM funcionarios
        WHERE username = ? AND ativo = 1
    `

	if s.config.DB.Driver == "postgres" {
		queryFuncionario = `
            SELECT funcao_id
            FROM funcionarios
            WHERE username = $1 AND ativo = true
        `
	}

	err := s.db.QueryRow(queryFuncionario, username).Scan(&funcaoID)
	if err != nil {
		if err == sql.ErrNoRows {
			s.logger.Printf("Funcionário %s não encontrado ou sem função definida", username)
			return []AppImprodutivo{}, nil
		}
		return nil, err
	}

	if !funcaoID.Valid {
		s.logger.Printf("Funcionário %s não tem função definida", username)
		return []AppImprodutivo{}, nil
	}

	// Agora busca os apps improdutivos da função
	query := `
        SELECT DISTINCT ai.application_name
        FROM aplicativos_improdutivos ai
        INNER JOIN funcoes_aplicativos_improdutivos fai ON ai.id = fai.aplicativo_id
        WHERE fai.funcao_id = ? AND ai.ativo = 1
    `

	if s.config.DB.Driver == "postgres" {
		query = `
            SELECT DISTINCT ai.application_name
            FROM aplicativos_improdutivos ai
            INNER JOIN funcoes_aplicativos_improdutivos fai ON ai.id = fai.aplicativo_id
            WHERE fai.funcao_id = $1 AND ai.ativo = true
        `
	}

	rows, err := s.db.Query(query, funcaoID.String)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []AppImprodutivo
	for rows.Next() {
		var app AppImprodutivo
		if err := rows.Scan(&app.ApplicationName); err != nil {
			continue
		}
		apps = append(apps, app)
	}

	s.logger.Printf("Encontrados %d aplicativos improdutivos para o funcionário %s (função: %s)",
		len(apps), username, funcaoID.String)

	return apps, nil
}

func (s *service) storeApplicationUsageData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script application_usage retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		s.logger.Printf("Máquina não encontrada na tabela machines. Pulando application_usage.")
		return nil
	}

	parseLayout := "2006-01-02 15:04:05"

	// --- 1. Monta lista de processos ativos da coleta
	activeProcesses := make(map[string]bool)
	for _, item := range result.Data {
		username, _ := item["username"].(string)
		processName, _ := item["process_name"].(string)
		startTimeStr, _ := item["start_time"].(string)
		windowState, _ := item["window_state"].(string)
		if windowState == "" {
			windowState = "background"
		}

		key := fmt.Sprintf("%s|%s|%s|%s", username, processName, startTimeStr, windowState)
		activeProcesses[key] = true
	}

	// --- 2. Finaliza sessões que não apareceram nesta coleta
	var query string
	if s.config.DB.Driver == "mysql" {
		query = "SELECT id, username, process_name, start_time, window_state FROM application_usage WHERE machine_id = ? AND is_active = 1"
	} else {
		query = "SELECT id, username, process_name, start_time, window_state FROM application_usage WHERE machine_id = $1 AND is_active = 1"
	}

	rows, err := s.db.Query(query, machineID)
	if err != nil {
		return fmt.Errorf("erro ao buscar sessões ativas: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, username, processName, windowState string
		var startTime time.Time
		if err := rows.Scan(&id, &username, &processName, &startTime, &windowState); err != nil {
			continue
		}

		key := fmt.Sprintf("%s|%s|%s|%s", username, processName, startTime.Format(parseLayout), windowState)
		if !activeProcesses[key] {
			var updateSQL string
			if s.config.DB.Driver == "mysql" {
				updateSQL = `
                UPDATE application_usage
                SET end_time = NOW(), is_active = 0, duration_seconds = TIMESTAMPDIFF(SECOND, start_time, NOW())
                WHERE id = ?`
			} else {
				updateSQL = `
                UPDATE application_usage
                SET end_time = CURRENT_TIMESTAMP, is_active = 0, duration_seconds = EXTRACT(EPOCH FROM (CURRENT_TIMESTAMP - start_time))::INT
                WHERE id = $1`
			}
			if _, err := s.db.Exec(updateSQL, id); err != nil {
				s.logger.Printf("Erro ao finalizar sessão %s: %v", id, err)
			} else {
				s.logger.Printf("Sessão finalizada: %s (%s)", processName, username)
			}
		}
	}

	// Cache de apps improdutivos por usuário para evitar queries repetidas
	userAppsCache := make(map[string]map[string]bool)

	// --- 3. Insere/atualiza processos ativos da coleta
	for i, item := range result.Data {
		id := generateUUID()

		username, _ := item["username"].(string)
		processName, _ := item["process_name"].(string)
		appName, _ := item["application_name"].(string)

		startTimeStr, _ := item["start_time"].(string)
		collectedAtStr, _ := item["collected_at"].(string)

		isForeground := 0
		if fg, ok := item["is_foreground"].(bool); ok && fg {
			isForeground = 1
		}

		windowState := "background"
		if ws, ok := item["window_state"].(string); ok && ws != "" {
			if ws == "foreground" || ws == "background" || ws == "minimized" {
				windowState = ws
			}
		}

		if startTimeStr == "" {
			s.logger.Printf("Ignorando processo %s pois start_time vazio", processName)
			continue
		}

		startTime, err := time.Parse(parseLayout, startTimeStr)
		if err != nil {
			s.logger.Printf("Erro ao fazer parse do start_time '%s' para processo %s: %v", startTimeStr, processName, err)
			continue
		}

		var createdAt time.Time
		if t, err := time.Parse(parseLayout, collectedAtStr); err == nil {
			createdAt = t
		} else {
			createdAt = time.Now()
		}

		durationSec := int64(time.Since(startTime).Seconds())
		if durationSec < 0 {
			durationSec = 0
		}

		isActive := 1

		if s.config.DB.Driver == "mysql" {
			insertSQL := `
            INSERT INTO application_usage
                (id, machine_id, username, application_name, process_name, start_time, end_time, duration_seconds, is_active, is_foreground, window_state, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            ON DUPLICATE KEY UPDATE
                end_time = VALUES(end_time),
                duration_seconds = VALUES(duration_seconds),
                is_active = VALUES(is_active),
                is_foreground = VALUES(is_foreground),
                window_state = VALUES(window_state),
                created_at = VALUES(created_at)`

			_, err := s.db.Exec(insertSQL,
				id,
				machineID,
				username,
				appName,
				processName,
				startTime,
				nil,
				sql.NullInt32{Int32: int32(durationSec), Valid: durationSec > 0},
				isActive,
				isForeground,
				windowState,
				createdAt,
			)
			if err != nil {
				return fmt.Errorf("erro ao inserir application_usage registro %d: %v", i+1, err)
			}

		} else {
			insertSQL := `
            INSERT INTO application_usage
                (id, machine_id, username, application_name, process_name, start_time, end_time, duration_seconds, is_active, is_foreground, window_state, created_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
            ON CONFLICT (machine_id, username, process_name, start_time, window_state)
            DO UPDATE SET
                end_time = EXCLUDED.end_time,
                duration_seconds = EXCLUDED.duration_seconds,
                is_active = EXCLUDED.is_active,
                is_foreground = EXCLUDED.is_foreground,
                window_state = EXCLUDED.window_state,
                created_at = EXCLUDED.created_at`

			_, err := s.db.Exec(insertSQL,
				id,
				machineID,
				username,
				appName,
				processName,
				startTime,
				nil,
				sql.NullInt32{Int32: int32(durationSec), Valid: durationSec > 0},
				isActive,
				isForeground,
				windowState,
				createdAt,
			)
			if err != nil {
				return fmt.Errorf("erro ao inserir application_usage registro %d: %v", i+1, err)
			}
		}

		s.logger.Printf("Application usage registrado: %s - %s (%ds ativo, foreground: %v, state: %s)",
			username, appName, durationSec, isForeground == 1, windowState)

		// 🔹 VERIFICA SE É APP IMPRODUTIVO EM FOREGROUND
		if isForeground == 1 && username != "" {
			// Carrega apps improdutivos do usuário se ainda não estiver no cache
			if _, exists := userAppsCache[username]; !exists {
				apps, err := s.getAppsImprodutivosPorFuncionario(username)
				if err != nil {
					s.logger.Printf("Erro ao buscar apps improdutivos para %s: %v", username, err)
					userAppsCache[username] = make(map[string]bool)
				} else {
					appsMap := make(map[string]bool)
					for _, app := range apps {
						appsMap[strings.ToLower(strings.TrimSpace(app.ApplicationName))] = true
					}
					userAppsCache[username] = appsMap
				}
			}

			appsMap := userAppsCache[username]
			appNameLower := strings.ToLower(strings.TrimSpace(appName))
			processNameLower := strings.ToLower(strings.TrimSpace(processName))

			// Verifica tanto pelo application_name quanto pelo process_name
			if appsMap[appNameLower] || appsMap[processNameLower] {
				s.logger.Printf("🚨 APP IMPRODUTIVO DETECTADO EM FOREGROUND: %s (processo: %s) - Usuário: %s",
					appName, processName, username)

				// Tira print de forma assíncrona para não bloquear
				go s.tirarPrintImprodutivo(appName, processName, username)
			}
		}
	}

	return nil
}

func (s *service) storeBrowserHistoryData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script browser_history retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		hostname := os.Getenv("COMPUTERNAME")
		s.logger.Printf("Máquina %s não encontrada na tabela machines. Pulando browser_history.", hostname)
		return nil
	}

	parseLayout := "2006-01-02 15:04:05"
	username := getCurrentUsername()

	for i, item := range result.Data {
		id := generateUUID()

		browser, _ := item["browser"].(string)
		url, _ := item["url"].(string)
		title, _ := item["title"].(string)
		lastVisitStr, _ := item["last_visit"].(string)

		var visitTime time.Time
		if lastVisitStr != "" {
			if parsed, err := time.Parse(parseLayout, lastVisitStr); err == nil {
				visitTime = parsed
			} else {
				visitTime = time.Now()
			}
		} else {
			visitTime = time.Now()
		}

		// 🔹 LOG DE DEBUG ANTES DO INSERT
		s.logger.Printf(
			"Inserindo browser_history: ID=%s, MachineID=%s, User=%s, Browser=%s, URL=%s, Title=%s, VisitTime=%s",
			id, machineID, username, browser, url, title, visitTime.Format("2006-01-02 15:04:05"),
		)

		if s.config.DB.Driver == "mysql" {
			insertSQL := `
            INSERT INTO browser_history (id, machine_id, username, browser, url, title, visit_time)
            VALUES (?, ?, ?, ?, ?, ?, ?)
            ON DUPLICATE KEY UPDATE
                title = VALUES(title),
                visit_time = VALUES(visit_time)`

			_, err := s.db.Exec(insertSQL,
				id, machineID, username, strings.ToLower(browser), url, title, visitTime,
			)
			if err != nil {
				return fmt.Errorf("erro ao inserir browser_history registro %d: %v", i+1, err)
			}

		} else {
			insertSQL := `
            INSERT INTO browser_history (id, machine_id, username, browser, url, title, visit_time)
            VALUES ($1,$2,$3,$4,$5,$6,$7)
            ON CONFLICT (machine_id, browser, url, visit_time)
            DO UPDATE SET
                title = EXCLUDED.title,
                visit_time = EXCLUDED.visit_time`

			_, err := s.db.Exec(insertSQL,
				id, machineID, username, strings.ToLower(browser), url, title, visitTime,
			)
			if err != nil {
				return fmt.Errorf("erro ao inserir browser_history registro %d: %v", i+1, err)
			}
		}
	}

	return nil
}

func (s *service) storeFileDownloadsData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script file_downloads retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		hostname := os.Getenv("COMPUTERNAME")
		s.logger.Printf("Máquina %s não encontrada na tabela machines. Pulando file_downloads.", hostname)
		return nil
	}

	parseLayout := "2006-01-02 15:04:05"

	for i, item := range result.Data {
		id := generateUUID()

		username, _ := item["username"].(string)
		fileName, _ := item["file_name"].(string)
		filePath, _ := item["file_path"].(string)
		fileType, _ := item["file_type"].(string) // vem como .drawio, .svg etc
		downloadURL, _ := item["download_url"].(string)
		browser, _ := item["browser"].(string)

		// file_size_bytes
		var fileSize sql.NullInt64
		if v, ok := item["file_size_bytes"].(float64); ok {
			fileSize = sql.NullInt64{Int64: int64(v), Valid: true}
		} else {
			fileSize = sql.NullInt64{Valid: false}
		}

		// download_time (se não tiver no JSON, usa last_accessed)
		var downloadTime time.Time
		if ts, ok := item["download_time"].(string); ok && ts != "" {
			if parsed, err := time.Parse(parseLayout, ts); err == nil {
				downloadTime = parsed
			}
		} else if ts, ok := item["last_accessed"].(string); ok && ts != "" {
			if parsed, err := time.Parse(parseLayout, ts); err == nil {
				downloadTime = parsed
			}
		}
		if downloadTime.IsZero() {
			downloadTime = time.Now()
		}

		// 🔹 LOG DE DEBUG ANTES DO INSERT
		s.logger.Printf(
			"Inserindo file_download: ID=%s, MachineID=%s, User=%s, File=%s, Path=%s, Size=%d, Browser=%s, URL=%s, Time=%s",
			id, machineID, username, fileName, filePath, fileSize.Int64, browser, downloadURL, downloadTime.Format("2006-01-02 15:04:05"),
		)

		if s.config.DB.Driver == "mysql" {
			insertSQL := `
            INSERT INTO file_downloads (id, machine_id, username, file_name, file_path, file_size_bytes, download_url, browser, download_time, file_type)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            ON DUPLICATE KEY UPDATE
                file_path = VALUES(file_path),
                file_size_bytes = VALUES(file_size_bytes),
                download_url = VALUES(download_url),
                browser = VALUES(browser),
                file_type = VALUES(file_type),
                created_at = CURRENT_TIMESTAMP`

			_, err := s.db.Exec(insertSQL,
				id, machineID, username, fileName, filePath, fileSize, downloadURL, strings.ToLower(browser), downloadTime, strings.TrimPrefix(fileType, "."),
			)
			if err != nil {
				return fmt.Errorf("erro ao inserir file_download registro %d: %v", i+1, err)
			}

		} else {
			insertSQL := `
            INSERT INTO file_downloads (id, machine_id, username, file_name, file_path, file_size_bytes, download_url, browser, download_time, file_type)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
            ON CONFLICT (machine_id, file_name, download_time)
            DO UPDATE SET
                file_path = EXCLUDED.file_path,
                file_size_bytes = EXCLUDED.file_size_bytes,
                download_url = EXCLUDED.download_url,
                browser = EXCLUDED.browser,
                file_type = EXCLUDED.file_type,
                created_at = CURRENT_TIMESTAMP`

			_, err := s.db.Exec(insertSQL,
				id, machineID, username, fileName, filePath, fileSize, downloadURL, strings.ToLower(browser), downloadTime, strings.TrimPrefix(fileType, "."),
			)
			if err != nil {
				return fmt.Errorf("erro ao inserir file_download registro %d: %v", i+1, err)
			}
		}
	}

	return nil
}

func (s *service) storeRecentFilesData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script recent_files retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		s.logger.Printf("Máquina não encontrada, pulando recent_files")
		return nil
	}

	parseLayout := "2006-01-02 15:04:05"

	for i, item := range result.Data {
		id := generateUUID()

		username, _ := item["username"].(string)
		filePath, _ := item["file_path"].(string)
		fileName, _ := item["file_name"].(string)
		fileExt, _ := item["file_extension"].(string)
		application, _ := item["application"].(string)

		// file_size_bytes pode vir float64 (JSON)
		var fileSize sql.NullInt64
		if val, ok := item["file_size_bytes"].(float64); ok {
			fileSize = sql.NullInt64{Int64: int64(val), Valid: true}
		} else {
			fileSize = sql.NullInt64{Valid: false}
		}

		// last_accessed é string "YYYY-MM-DD HH:mm:ss"
		var lastAccessed time.Time
		if ts, ok := item["last_accessed"].(string); ok && ts != "" {
			if parsed, err := time.Parse(parseLayout, ts); err == nil {
				lastAccessed = parsed
			} else {
				lastAccessed = time.Now()
			}
		} else {
			lastAccessed = time.Now()
		}

		s.logger.Printf("Inserindo RecentFile[%d]: path=%s, name=%s, ext=%s, size=%d, accessed=%s",
			i+1, filePath, fileName, fileExt, fileSize.Int64, lastAccessed.Format(parseLayout))

		var insertSQL string
		if s.config.DB.Driver == "mysql" {
			insertSQL = `
            INSERT INTO recent_files (id, machine_id, username, file_path, file_name, file_extension, last_accessed, file_size_bytes, application, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
            ON DUPLICATE KEY UPDATE
                file_name = VALUES(file_name),
                file_extension = VALUES(file_extension),
                file_size_bytes = VALUES(file_size_bytes),
                application = VALUES(application),
                last_accessed = VALUES(last_accessed)`
			_, err = s.db.Exec(insertSQL,
				id, machineID, username, filePath, fileName, fileExt, lastAccessed, fileSize, application)
		} else {
			insertSQL = `
            INSERT INTO recent_files (id, machine_id, username, file_path, file_name, file_extension, last_accessed, file_size_bytes, application, created_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,CURRENT_TIMESTAMP)
            ON CONFLICT (machine_id, username, file_path, last_accessed)
            DO UPDATE SET
                file_name = EXCLUDED.file_name,
                file_extension = EXCLUDED.file_extension,
                file_size_bytes = EXCLUDED.file_size_bytes,
                application = EXCLUDED.application,
                last_accessed = EXCLUDED.last_accessed`
			_, err = s.db.Exec(insertSQL,
				id, machineID, username, filePath, fileName, fileExt, lastAccessed, fileSize, application)
		}

		if err != nil {
			s.logger.Printf("Erro ao inserir RecentFile[%d] (%s): %v", i+1, filePath, err)
		} else {
			s.logger.Printf("RecentFile[%d] inserido/atualizado com sucesso: %s", i+1, filePath)
		}
	}

	return nil
}

func (s *service) storeUserSessionsData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script user_sessions retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		s.logger.Printf("Máquina não encontrada, pulando user_sessions")
		return nil
	}

	parseLayout := "2006-01-02 15:04:05"

	for i, item := range result.Data {
		id := generateUUID()

		username, _ := item["username"].(string)
		sessionType, _ := item["session_type"].(string)
		sessionID, _ := item["session_id"].(string)
		logonType, _ := item["logon_type"].(string)
		sourceIP, _ := item["source_ip"].(string)

		// event_time
		var eventTime time.Time
		if ts, ok := item["event_time"].(string); ok && ts != "" {
			if parsed, err := time.Parse(parseLayout, ts); err == nil {
				eventTime = parsed
			} else {
				eventTime = time.Now()
			}
		} else {
			eventTime = time.Now()
		}

		s.logger.Printf("Inserindo UserSession[%d]: user=%s, type=%s, time=%s, sessionID=%s, logonType=%s, ip=%s",
			i+1, username, sessionType, eventTime.Format(parseLayout), sessionID, logonType, sourceIP)

		var insertSQL string
		if s.config.DB.Driver == "mysql" {
			insertSQL = `
            INSERT INTO user_sessions (id, machine_id, username, session_type, event_time, session_id, logon_type, source_ip, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, NOW())
            ON DUPLICATE KEY UPDATE
                logon_type = VALUES(logon_type),
                source_ip = VALUES(source_ip),
                event_time = VALUES(event_time)`
			_, err = s.db.Exec(insertSQL,
				id, machineID, username, sessionType, eventTime, sessionID, logonType, sourceIP)
		} else {
			insertSQL = `
            INSERT INTO user_sessions (id, machine_id, username, session_type, event_time, session_id, logon_type, source_ip, created_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,CURRENT_TIMESTAMP)
            ON CONFLICT (machine_id, username, session_type, event_time)
            DO UPDATE SET
                logon_type = EXCLUDED.logon_type,
                source_ip = EXCLUDED.source_ip,
                event_time = EXCLUDED.event_time`
			_, err = s.db.Exec(insertSQL,
				id, machineID, username, sessionType, eventTime, sessionID, logonType, sourceIP)
		}

		if err != nil {
			s.logger.Printf("Erro ao inserir UserSession[%d] (user=%s, type=%s): %v", i+1, username, sessionType, err)
		} else {
			s.logger.Printf("UserSession[%d] inserido/atualizado com sucesso: user=%s, type=%s", i+1, username, sessionType)
		}
	}

	return nil
}

func (s *service) storeAppliedGposData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script applied_gpos retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		hostname := os.Getenv("COMPUTERNAME")
		s.logger.Printf("Máquina %s não encontrada na tabela machines. Pulando applied_gpos.", hostname)
		return nil
	}

	parseLayout := "2006-01-02 15:04:05"

	for i, item := range result.Data {
		id := generateUUID()

		gpoName, _ := item["gpo_name"].(string)
		gpoGuid, _ := item["gpo_guid"].(string)
		gpoType, _ := item["gpo_type"].(string)
		status, _ := item["status"].(string)
		appliedTimeStr, _ := item["applied_time"].(string)

		// 🔹 Serializa details se existir
		var detailsJSON string
		if detailsRaw, ok := item["details"]; ok && detailsRaw != nil {
			if b, err := json.Marshal(detailsRaw); err == nil {
				detailsJSON = string(b)
			} else {
				s.logger.Printf("Erro ao serializar details para GPO %s: %v", gpoName, err)
			}
		}

		var appliedTime time.Time
		if appliedTimeStr != "" {
			if parsed, err := time.Parse(parseLayout, appliedTimeStr); err == nil {
				appliedTime = parsed
			} else {
				appliedTime = time.Now()
			}
		} else {
			appliedTime = time.Now()
		}

		// 🔹 Log antes do insert/update
		s.logger.Printf(
			"Inserindo applied_gpos[%d]: ID=%s, MachineID=%s, Name=%s, GUID=%s, Type=%s, Status=%s, AppliedTime=%s",
			i+1, id, machineID, gpoName, gpoGuid, gpoType, status, appliedTime.Format(parseLayout),
		)

		if s.config.DB.Driver == "mysql" {
			insertSQL := `
            INSERT INTO applied_gpos (id, machine_id, gpo_name, gpo_guid, gpo_type, details, applied_time, status)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?)
            ON DUPLICATE KEY UPDATE
                gpo_name = VALUES(gpo_name),
                gpo_type = VALUES(gpo_type),
                details = VALUES(details),
                applied_time = VALUES(applied_time),
                status = VALUES(status),
                last_updated = NOW()`

			_, err := s.db.Exec(insertSQL,
				id, machineID, gpoName, gpoGuid, gpoType, detailsJSON, appliedTime, status,
			)
			if err != nil {
				s.logger.Printf("Erro ao inserir applied_gpos[%d] (%s): %v", i+1, gpoName, err)
			} else {
				s.logger.Printf("applied_gpos[%d] inserido/atualizado com sucesso: %s", i+1, gpoName)
			}

		} else {
			insertSQL := `
            INSERT INTO applied_gpos (id, machine_id, gpo_name, gpo_guid, gpo_type, details, applied_time, status)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
            ON CONFLICT (machine_id, gpo_guid)
            DO UPDATE SET
                gpo_name = EXCLUDED.gpo_name,
                gpo_type = EXCLUDED.gpo_type,
                details = EXCLUDED.details,
                applied_time = EXCLUDED.applied_time,
                status = EXCLUDED.status,
                last_updated = CURRENT_TIMESTAMP`

			_, err := s.db.Exec(insertSQL,
				id, machineID, gpoName, gpoGuid, gpoType, detailsJSON, appliedTime, status,
			)
			if err != nil {
				s.logger.Printf("Erro ao inserir applied_gpos[%d] (%s): %v", i+1, gpoName, err)
			} else {
				s.logger.Printf("applied_gpos[%d] inserido/atualizado com sucesso: %s", i+1, gpoName)
			}
		}
	}

	return nil
}

func (s *service) storeLoginAttemptsData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script login_attempts retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		s.logger.Printf("Máquina não encontrada na tabela machines. Pulando login_attempts")
		return nil
	}

	parseLayout := "2006-01-02T15:04:05"

	for i, item := range result.Data {
		id := generateUUID()

		username, _ := item["username"].(string)
		loginType, _ := item["login_type"].(string)
		success, _ := item["success"].(bool)
		failureReason, _ := item["failure_reason"].(string)
		sourceIP, _ := item["source_ip"].(string)
		attemptTimeStr, _ := item["attempt_time"].(string)

		// Parse da data
		var attemptTime time.Time
		if attemptTimeStr != "" {
			if parsed, err := time.Parse(parseLayout, attemptTimeStr); err == nil {
				attemptTime = parsed
			} else {
				attemptTime = time.Now()
			}
		} else {
			attemptTime = time.Now()
		}

		// LOG antes do insert
		s.logger.Printf(
			"Inserindo LoginAttempt[%d]: user=%s, type=%s, success=%t, reason=%s, ip=%s, time=%s",
			i+1, username, loginType, success, failureReason, sourceIP,
			attemptTime.Format("2006-01-02 15:04:05"),
		)

		if s.config.DB.Driver == "mysql" {
			insertSQL := `
            INSERT INTO login_attempts
                (id, machine_id, username, login_type, success, failure_reason, source_ip, attempt_time, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, NOW())
            ON DUPLICATE KEY UPDATE
                login_type = VALUES(login_type),
                success = VALUES(success),
                failure_reason = VALUES(failure_reason),
                source_ip = VALUES(source_ip),
                attempt_time = VALUES(attempt_time),
                created_at = CURRENT_TIMESTAMP`

			_, err = s.db.Exec(insertSQL,
				id, machineID, username, loginType, success, failureReason, sourceIP, attemptTime)
		} else {
			insertSQL := `
            INSERT INTO login_attempts
                (id, machine_id, username, login_type, success, failure_reason, source_ip, attempt_time, created_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,CURRENT_TIMESTAMP)
            ON CONFLICT (machine_id, username, attempt_time)
            DO UPDATE SET
                login_type = EXCLUDED.login_type,
                success = EXCLUDED.success,
                failure_reason = EXCLUDED.failure_reason,
                source_ip = EXCLUDED.source_ip,
                attempt_time = EXCLUDED.attempt_time,
                created_at = CURRENT_TIMESTAMP`

			_, err = s.db.Exec(insertSQL,
				id, machineID, username, loginType, success, failureReason, sourceIP, attemptTime)
		}

		if err != nil {
			s.logger.Printf("Erro ao inserir LoginAttempt[%d] (user=%s, type=%s): %v",
				i+1, username, loginType, err)
		} else {
			s.logger.Printf("LoginAttempt[%d] inserido/atualizado com sucesso: user=%s, type=%s",
				i+1, username, loginType)
		}
	}

	return nil
}

func (s *service) storeNetworkConfigData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script network_config retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		s.logger.Printf("Máquina não encontrada na tabela machines. Pulando network_config")
		return nil
	}

	for i, item := range result.Data {
		id := generateUUID()

		interfaceName, _ := item["interface_name"].(string)
		interfaceType, _ := item["interface_type"].(string)
		ipAddress, _ := item["ip_address"].(string)
		macAddress, _ := item["mac_address"].(string)
		gateway, _ := item["gateway"].(string)
		wifiSSID, _ := item["wifi_ssid"].(string)

		isActive, _ := item["is_active"].(bool)
		var wifiSignal sql.NullInt32
		if val, ok := item["wifi_signal_strength"].(float64); ok {
			wifiSignal = sql.NullInt32{Int32: int32(val), Valid: true}
		} else {
			wifiSignal = sql.NullInt32{Valid: false}
		}

		// Serializa lista de DNS para JSON
		var dnsJSON string
		if dnsList, ok := item["dns_servers"].([]interface{}); ok {
			dnsBytes, _ := json.Marshal(dnsList)
			dnsJSON = string(dnsBytes)
		}

		s.logger.Printf(
			"Inserindo NetworkConfig[%d]: iface=%s, type=%s, ip=%s, mac=%s, gw=%s, ssid=%s, active=%t",
			i+1, interfaceName, interfaceType, ipAddress, macAddress, gateway, wifiSSID, isActive,
		)

		if s.config.DB.Driver == "mysql" {
			upsertSQL := `
            INSERT INTO network_config
                (id, machine_id, interface_name, interface_type, ip_address, mac_address, dns_servers,
                 gateway, is_active, wifi_ssid, wifi_signal_strength, last_updated)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
            ON DUPLICATE KEY UPDATE
                ip_address = VALUES(ip_address),
                mac_address = VALUES(mac_address),
                dns_servers = VALUES(dns_servers),
                gateway = VALUES(gateway),
                is_active = VALUES(is_active),
                wifi_ssid = VALUES(wifi_ssid),
                wifi_signal_strength = VALUES(wifi_signal_strength),
                last_updated = NOW()`

			_, err = s.db.Exec(upsertSQL,
				id, machineID, interfaceName, interfaceType, ipAddress, macAddress,
				dnsJSON, gateway, isActive, wifiSSID, wifiSignal,
			)
		} else {
			upsertSQL := `
            INSERT INTO network_config
                (id, machine_id, interface_name, interface_type, ip_address, mac_address, dns_servers,
                 gateway, is_active, wifi_ssid, wifi_signal_strength, last_updated)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,CURRENT_TIMESTAMP)
            ON CONFLICT (machine_id, interface_name)
            DO UPDATE SET
                ip_address = EXCLUDED.ip_address,
                mac_address = EXCLUDED.mac_address,
                dns_servers = EXCLUDED.dns_servers,
                gateway = EXCLUDED.gateway,
                is_active = EXCLUDED.is_active,
                wifi_ssid = EXCLUDED.wifi_ssid,
                wifi_signal_strength = EXCLUDED.wifi_signal_strength,
                last_updated = CURRENT_TIMESTAMP`

			_, err = s.db.Exec(upsertSQL,
				id, machineID, interfaceName, interfaceType, ipAddress, macAddress,
				dnsJSON, gateway, isActive, wifiSSID, wifiSignal,
			)
		}

		if err != nil {
			s.logger.Printf("Erro ao inserir NetworkConfig[%d] (iface=%s): %v", i+1, interfaceName, err)
		} else {
			s.logger.Printf("NetworkConfig[%d] inserido/atualizado com sucesso (iface=%s)", i+1, interfaceName)
		}
	}

	return nil
}

func (s *service) storeNetworkConnectionsData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script network_connections retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		s.logger.Printf("Máquina não encontrada na tabela machines. Pulando network_connections")
		return nil
	}

	parseLayout := "2006-01-02T15:04:05"

	for i, item := range result.Data {
		id := generateUUID()

		protocol, _ := item["protocol"].(string)
		localAddress, _ := item["local_address"].(string)
		remoteAddress, _ := item["remote_address"].(string)
		processName, _ := item["process_name"].(string)
		status, _ := item["status"].(string)

		var localPort, remotePort sql.NullInt32
		if v, ok := item["local_port"].(float64); ok {
			localPort = sql.NullInt32{Int32: int32(v), Valid: true}
		} else {
			localPort = sql.NullInt32{Valid: false}
		}
		if v, ok := item["remote_port"].(float64); ok {
			remotePort = sql.NullInt32{Int32: int32(v), Valid: true}
		} else {
			remotePort = sql.NullInt32{Valid: false}
		}

		var pid sql.NullInt32
		if v, ok := item["pid"].(float64); ok {
			pid = sql.NullInt32{Int32: int32(v), Valid: true}
		} else {
			pid = sql.NullInt32{Valid: false}
		}

		tsStr, _ := item["timestamp"].(string)
		var connTime time.Time
		if tsStr != "" {
			if parsed, err := time.Parse(parseLayout, tsStr); err == nil {
				connTime = parsed
			} else {
				connTime = time.Now()
			}
		} else {
			connTime = time.Now()
		}

		// LOG antes do insert
		s.logger.Printf(
			"Inserindo NetworkConnection[%d]: proto=%s, laddr=%s:%v, raddr=%s:%v, pid=%v, proc=%s, status=%s",
			i+1, protocol, localAddress, localPort, remoteAddress, remotePort, pid, processName, status,
		)

		if s.config.DB.Driver == "mysql" {
			insertSQL := `
            INSERT INTO network_connections
                (id, machine_id, timestamp, protocol, local_address, local_port, remote_address, remote_port,
                 status, process_name, pid, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())`

			_, err = s.db.Exec(insertSQL,
				id, machineID, connTime, protocol, localAddress, localPort, remoteAddress, remotePort,
				status, processName, pid,
			)
		} else {
			insertSQL := `
            INSERT INTO network_connections
                (id, machine_id, timestamp, protocol, local_address, local_port, remote_address, remote_port,
                 status, process_name, pid, created_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,CURRENT_TIMESTAMP)`

			_, err = s.db.Exec(insertSQL,
				id, machineID, connTime, protocol, localAddress, localPort, remoteAddress, remotePort,
				status, processName, pid,
			)
		}

		if err != nil {
			s.logger.Printf("Erro ao inserir NetworkConnection[%d] (proto=%s, laddr=%s): %v",
				i+1, protocol, localAddress, err)
		} else {
			s.logger.Printf("NetworkConnection[%d] inserido com sucesso (proto=%s, laddr=%s)",
				i+1, protocol, localAddress)
		}
	}

	return nil
}

func (s *service) storeRunningProcessesData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script running_processes retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		s.logger.Printf("Máquina não encontrada na tabela machines. Pulando running_processes")
		return nil
	}

	parseLayout := "2006-01-02T15:04:05"

	for i, item := range result.Data {
		id := generateUUID()

		processName, _ := item["process_name"].(string)
		commandLine, _ := item["command_line"].(string)
		username, _ := item["username"].(string)

		var pid, parentPid sql.NullInt32
		if v, ok := item["pid"].(float64); ok {
			pid = sql.NullInt32{Int32: int32(v), Valid: true}
		}
		if v, ok := item["parent_pid"].(float64); ok {
			parentPid = sql.NullInt32{Int32: int32(v), Valid: true}
		}

		var cpuPercent, memPercent sql.NullFloat64
		if v, ok := item["cpu_percent"].(float64); ok {
			cpuPercent = sql.NullFloat64{Float64: v, Valid: true}
		}
		if v, ok := item["memory_percent"].(float64); ok {
			memPercent = sql.NullFloat64{Float64: v, Valid: true}
		}

		var memoryMB sql.NullInt32
		if v, ok := item["memory_mb"].(float64); ok {
			memoryMB = sql.NullInt32{Int32: int32(v), Valid: true}
		}

		statusStr := "Unknown"
		if v, ok := item["status"].(bool); ok {
			if v {
				statusStr = "Running"
			} else {
				statusStr = "Not Responding"
			}
		} else if v, ok := item["status"].(string); ok {
			statusStr = v
		}

		tsStr, _ := item["timestamp"].(string)
		var procTime time.Time
		if tsStr != "" {
			if parsed, err := time.Parse(parseLayout, tsStr); err == nil {
				procTime = parsed
			} else {
				procTime = time.Now()
			}
		} else {
			procTime = time.Now()
		}

		// Log antes do insert
		s.logger.Printf(
			"Inserindo RunningProcess[%d]: pid=%v, name=%s, cpu=%.2f, mem=%.2f%% (%v MB), user=%s, status=%s",
			i+1, pid, processName, cpuPercent.Float64, memPercent.Float64, memoryMB.Int32, username, statusStr,
		)

		if s.config.DB.Driver == "mysql" {
			insertSQL := `
            INSERT INTO running_processes
                (id, machine_id, timestamp, process_name, pid, parent_pid, cpu_percent,
                 memory_percent, memory_mb, status, command_line, username, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())`

			_, err = s.db.Exec(insertSQL,
				id, machineID, procTime, processName, pid, parentPid, cpuPercent,
				memPercent, memoryMB, statusStr, commandLine, username,
			)
		} else {
			insertSQL := `
            INSERT INTO running_processes
                (id, machine_id, timestamp, process_name, pid, parent_pid, cpu_percent,
                 memory_percent, memory_mb, status, command_line, username, created_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,CURRENT_TIMESTAMP)`

			_, err = s.db.Exec(insertSQL,
				id, machineID, procTime, processName, pid, parentPid, cpuPercent,
				memPercent, memoryMB, statusStr, commandLine, username,
			)
		}

		if err != nil {
			s.logger.Printf("Erro ao inserir RunningProcess[%d] (pid=%v, name=%s): %v",
				i+1, pid, processName, err)
		} else {
			s.logger.Printf("RunningProcess[%d] inserido com sucesso (pid=%v, name=%s)",
				i+1, pid, processName)
		}
	}

	return nil
}

func (s *service) storeSystemLogsData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script system_logs retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		s.logger.Printf("Máquina não encontrada na tabela machines. Pulando system_logs")
		return nil
	}

	parseLayout := "2006-01-02T15:04:05"

	for i, item := range result.Data {
		id := generateUUID()

		logName, _ := item["log_name"].(string)
		level, _ := item["level"].(string)
		source, _ := item["source"].(string)
		message, _ := item["message"].(string)
		username, _ := item["username"].(string)

		var eventID sql.NullInt32
		if v, ok := item["event_id"].(float64); ok {
			eventID = sql.NullInt32{Int32: int32(v), Valid: true}
		}

		tsStr, _ := item["event_time"].(string)
		var eventTime time.Time
		if tsStr != "" {
			if parsed, err := time.Parse(parseLayout, tsStr); err == nil {
				eventTime = parsed
			} else {
				eventTime = time.Now()
			}
		} else {
			eventTime = time.Now()
		}

		// Log antes do insert
		s.logger.Printf(
			"Inserindo SystemLog[%d]: log=%s, eventID=%v, level=%s, source=%s, user=%s, time=%s",
			i+1, logName, eventID.Int32, level, source, username, eventTime.Format("2006-01-02 15:04:05"),
		)

		if s.config.DB.Driver == "mysql" {
			insertSQL := `
            INSERT INTO system_logs
                (id, machine_id, log_name, event_id, level, source, message, event_time, username, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())`

			_, err = s.db.Exec(insertSQL,
				id, machineID, logName, eventID, level, source, message, eventTime, username,
			)
		} else {
			insertSQL := `
            INSERT INTO system_logs
                (id, machine_id, log_name, event_id, level, source, message, event_time, username, created_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,CURRENT_TIMESTAMP)`

			_, err = s.db.Exec(insertSQL,
				id, machineID, logName, eventID, level, source, message, eventTime, username,
			)
		}

		if err != nil {
			s.logger.Printf("Erro ao inserir SystemLog[%d] (log=%s, eventID=%v): %v",
				i+1, logName, eventID.Int32, err)
		} else {
			s.logger.Printf("SystemLog[%d] inserido com sucesso (log=%s, eventID=%v)",
				i+1, logName, eventID.Int32)
		}
	}

	return nil
}

func (s *service) storeUsbDevicesData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script usb_devices retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		s.logger.Printf("Máquina não encontrada na tabela machines. Pulando usb_devices")
		return nil
	}

	parseLayout := "2006-01-02T15:04:05"

	for i, item := range result.Data {
		id := generateUUID()

		username, _ := item["username"].(string)
		deviceName, _ := item["device_name"].(string)
		deviceID, _ := item["device_id"].(string)
		vendorID, _ := item["vendor_id"].(string)
		productID, _ := item["product_id"].(string)
		serialNumber, _ := item["serial_number"].(string)
		deviceType, _ := item["device_type"].(string)

		tsStr, _ := item["connect_time"].(string)
		var connectTime time.Time
		if tsStr != "" {
			if parsed, err := time.Parse(parseLayout, tsStr); err == nil {
				connectTime = parsed
			} else {
				connectTime = time.Now()
			}
		} else {
			connectTime = time.Now()
		}

		var disconnectTime sql.NullTime
		if tsStr, ok := item["disconnect_time"].(string); ok && tsStr != "" {
			if parsed, err := time.Parse(parseLayout, tsStr); err == nil {
				disconnectTime = sql.NullTime{Time: parsed, Valid: true}
			}
		}

		// LOG antes do insert
		s.logger.Printf(
			"Inserindo UsbDevice[%d]: name=%s, id=%s, vendor=%s, product=%s, serial=%s, type=%s, user=%s",
			i+1, deviceName, deviceID, vendorID, productID, serialNumber, deviceType, username,
		)

		if s.config.DB.Driver == "mysql" {
			insertSQL := `
            INSERT INTO usb_devices
                (id, machine_id, username, device_name, device_id, vendor_id, product_id, serial_number,
                 connect_time, disconnect_time, device_type, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())`

			_, err = s.db.Exec(insertSQL,
				id, machineID, username, deviceName, deviceID, vendorID, productID, serialNumber,
				connectTime, disconnectTime, deviceType,
			)
		} else {
			insertSQL := `
            INSERT INTO usb_devices
                (id, machine_id, username, device_name, device_id, vendor_id, product_id, serial_number,
                 connect_time, disconnect_time, device_type, created_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,CURRENT_TIMESTAMP)`

			_, err = s.db.Exec(insertSQL,
				id, machineID, username, deviceName, deviceID, vendorID, productID, serialNumber,
				connectTime, disconnectTime, deviceType,
			)
		}

		if err != nil {
			s.logger.Printf("Erro ao inserir UsbDevice[%d] (name=%s, id=%s): %v",
				i+1, deviceName, deviceID, err)
		} else {
			s.logger.Printf("UsbDevice[%d] inserido com sucesso (name=%s, id=%s)",
				i+1, deviceName, deviceID)
		}
	}

	return nil
}

func (s *service) storeNetworkTrafficData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script network_traffic retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		s.logger.Printf("Máquina não encontrada na tabela machines. Pulando network_traffic.")
		return nil
	}

	for i, item := range result.Data {
		id := generateUUID()

		tsStr, _ := item["timestamp"].(string)
		iface, _ := item["interface_name"].(string)

		var metricTimestamp time.Time
		if tsStr != "" {
			metricTimestamp, err = time.Parse("2006-01-02T15:04:05", tsStr)
			if err != nil {
				metricTimestamp = time.Now()
			}
		} else {
			metricTimestamp = time.Now()
		}

		bytesSent := int64(0)
		if v, ok := item["bytes_sent"].(float64); ok {
			bytesSent = int64(v)
		}
		bytesReceived := int64(0)
		if v, ok := item["bytes_received"].(float64); ok {
			bytesReceived = int64(v)
		}
		packetsSent := int64(0)
		if v, ok := item["packets_sent"].(float64); ok {
			packetsSent = int64(v)
		}
		packetsReceived := int64(0)
		if v, ok := item["packets_received"].(float64); ok {
			packetsReceived = int64(v)
		}

		if s.config.DB.Driver == "mysql" {
			insertSQL := `
            INSERT INTO network_traffic
                (id, machine_id, timestamp, interface_name, bytes_sent, bytes_received, packets_sent, packets_received, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, NOW())
            ON DUPLICATE KEY UPDATE
                bytes_sent = VALUES(bytes_sent),
                bytes_received = VALUES(bytes_received),
                packets_sent = VALUES(packets_sent),
                packets_received = VALUES(packets_received),
                created_at = NOW()`

			_, err := s.db.Exec(insertSQL,
				id, machineID, metricTimestamp, iface,
				bytesSent, bytesReceived, packetsSent, packetsReceived,
			)
			if err != nil {
				s.logger.Printf("Erro ao upsert network_traffic registro %d (%s): %v", i+1, iface, err)
			} else {
				s.logger.Printf("NetworkTraffic UPSERT: iface=%s, sent=%d, recv=%d, pktsSent=%d, pktsRecv=%d",
					iface, bytesSent, bytesReceived, packetsSent, packetsReceived)
			}

		} else {
			insertSQL := `
            INSERT INTO network_traffic
                (id, machine_id, timestamp, interface_name, bytes_sent, bytes_received, packets_sent, packets_received, created_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,CURRENT_TIMESTAMP)
            ON CONFLICT (machine_id, timestamp, interface_name)
            DO UPDATE SET
                bytes_sent = EXCLUDED.bytes_sent,
                bytes_received = EXCLUDED.bytes_received,
                packets_sent = EXCLUDED.packets_sent,
                packets_received = EXCLUDED.packets_received,
                created_at = CURRENT_TIMESTAMP`

			_, err := s.db.Exec(insertSQL,
				id, machineID, metricTimestamp, iface,
				bytesSent, bytesReceived, packetsSent, packetsReceived,
			)
			if err != nil {
				s.logger.Printf("Erro ao upsert network_traffic registro %d (%s): %v", i+1, iface, err)
			} else {
				s.logger.Printf("NetworkTraffic UPSERT: iface=%s, sent=%d, recv=%d, pktsSent=%d, pktsRecv=%d",
					iface, bytesSent, bytesReceived, packetsSent, packetsReceived)
			}
		}
	}

	return nil
}

func (s *service) cleanup() {
	if s.db != nil {
		s.db.Close()
	}
	if s.logger != nil {
		s.logger.Println("Serviço finalizado")
	}
}

func (s *service) storeNetworkMappingsData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script network_mappings retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		s.logger.Printf("Máquina não encontrada na tabela machines. Pulando network_mappings.")
		return nil
	}

	for i, item := range result.Data {
		id := generateUUID()

		username, _ := item["username"].(string)
		driveLetter, _ := item["drive_letter"].(string)
		networkPath, _ := item["network_path"].(string)
		status, _ := item["status"].(string)
		lastAccessedStr, _ := item["last_accessed"].(string)

		var lastAccessed time.Time
		if lastAccessedStr != "" {
			parsed, err := time.Parse("2006-01-02 15:04:05", lastAccessedStr)
			if err == nil {
				lastAccessed = parsed
			} else {
				lastAccessed = time.Now()
			}
		} else {
			lastAccessed = time.Now()
		}

		if s.config.DB.Driver == "mysql" {
			insertSQL := `
            INSERT INTO network_mappings
                (id, machine_id, username, drive_letter, network_path, status, last_accessed, created_at, updated_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, NOW(), NOW())
            ON DUPLICATE KEY UPDATE
                network_path = VALUES(network_path),
                status = VALUES(status),
                last_accessed = VALUES(last_accessed),
                updated_at = NOW()`

			_, err := s.db.Exec(insertSQL,
				id, machineID, username, driveLetter, networkPath, status, lastAccessed,
			)
			if err != nil {
				s.logger.Printf("Erro ao upsert network_mapping registro %d (user=%s drive=%s): %v", i+1, username, driveLetter, err)
			} else {
				s.logger.Printf("NetworkMapping UPSERT: user=%s, drive=%s, path=%s, status=%s",
					username, driveLetter, networkPath, status)
			}

		} else {
			insertSQL := `
            INSERT INTO network_mappings
                (id, machine_id, username, drive_letter, network_path, status, last_accessed, created_at, updated_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)
            ON CONFLICT (machine_id, username, drive_letter)
            DO UPDATE SET
                network_path = EXCLUDED.network_path,
                status = EXCLUDED.status,
                last_accessed = EXCLUDED.last_accessed,
                updated_at = CURRENT_TIMESTAMP`

			_, err := s.db.Exec(insertSQL,
				id, machineID, username, driveLetter, networkPath, status, lastAccessed,
			)
			if err != nil {
				s.logger.Printf("Erro ao upsert network_mapping registro %d (user=%s drive=%s): %v", i+1, username, driveLetter, err)
			} else {
				s.logger.Printf("NetworkMapping UPSERT: user=%s, drive=%s, path=%s, status=%s",
					username, driveLetter, networkPath, status)
			}
		}
	}

	return nil
}

func (s *service) storeScreenshotsData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script screenshots retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		hostname := os.Getenv("COMPUTERNAME")
		s.logger.Printf("Máquina %s não encontrada na tabela machines. Pulando screenshots.", hostname)
		return nil
	}

	parseLayout := "2006-01-02T15:04:05"

	for i, item := range result.Data {
		id := generateUUID()

		username, _ := item["username"].(string)
		screenshotPath, _ := item["ftp_screenshot_uri"].(string)
		screenshotThumbPath, _ := item["ftp_thumb_uri"].(string)
		compressionType, _ := item["compression_type"].(string)

		// file_size_bytes
		var fileSize sql.NullInt64
		if v, ok := item["file_size_bytes"].(float64); ok {
			fileSize = sql.NullInt64{Int64: int64(v), Valid: true}
		}

		// monitor_count
		monitorCount := 1
		if v, ok := item["monitor_count"].(float64); ok {
			monitorCount = int(v)
		}

		// capture_time
		captureTime := time.Now()
		if ts, ok := item["capture_time"].(string); ok && ts != "" {
			if parsed, err := time.Parse(parseLayout, ts); err == nil {
				captureTime = parsed
			}
		}

		// trigger_type - default = scheduled
		triggerType := "scheduled"
		if v, ok := item["trigger_type"].(string); ok && v != "" {
			triggerType = v
		}

		s.logger.Printf(
			"Inserindo Screenshot[%d]: ID=%s, MachineID=%s, User=%s, Path=%s, Thumb=%s, Size=%d, Compression=%s, Monitors=%d, CaptureTime=%s, Trigger=%s",
			i+1, id, machineID, username, screenshotPath, screenshotThumbPath,
			fileSize.Int64, compressionType, monitorCount, captureTime.Format(parseLayout), triggerType,
		)

		if s.config.DB.Driver == "mysql" {
			insertSQL := `
            INSERT INTO screenshots
                (id, machine_id, username, screenshot_path, screenshot_thumb_path, file_size_bytes,
                 compression_type, monitor_count, capture_time, trigger_type, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())`

			_, err = s.db.Exec(insertSQL,
				id, machineID, username, screenshotPath, screenshotThumbPath,
				fileSize, compressionType, monitorCount, captureTime, triggerType,
			)
		} else {
			insertSQL := `
            INSERT INTO screenshots
                (id, machine_id, username, screenshot_path, screenshot_thumb_path, file_size_bytes,
                 compression_type, monitor_count, capture_time, trigger_type, created_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,CURRENT_TIMESTAMP)`

			_, err = s.db.Exec(insertSQL,
				id, machineID, username, screenshotPath, screenshotThumbPath,
				fileSize, compressionType, monitorCount, captureTime, triggerType,
			)
		}

		if err != nil {
			s.logger.Printf("❌ Erro ao inserir Screenshot[%d] (User=%s, Path=%s): %v", i+1, username, screenshotPath, err)
		} else {
			s.logger.Printf("✅ Screenshot[%d] inserido com sucesso (User=%s, Path=%s)", i+1, username, screenshotPath)
		}
	}

	return nil
}

func (s *service) storeScreenRecordingsData(result *ScriptResult) error {
	if result.Error != "" {
		s.logger.Printf("Script screen_recordings retornou erro: %s", result.Error)
		return nil
	}

	machineID, err := s.getCurrentMachineID()
	if err != nil {
		return fmt.Errorf("erro ao obter machine_id: %v", err)
	}
	if machineID == "" {
		hostname := os.Getenv("COMPUTERNAME")
		s.logger.Printf("Máquina %s não encontrada na tabela machines. Pulando screen_recordings.", hostname)
		return nil
	}

	parseLayout := "2006-01-02T15:04:05"

	for i, item := range result.Data {
		id := generateUUID()

		username, _ := item["username"].(string)
		videoPath, _ := item["ftp_video_uri"].(string)
		videoThumbPath, _ := item["ftp_thumb_uri"].(string)

		// file_size_bytes
		var fileSize sql.NullInt64
		if v, ok := item["file_size_bytes"].(float64); ok {
			fileSize = sql.NullInt64{Int64: int64(v), Valid: true}
		}

		// duration_seconds
		var duration sql.NullInt64
		if v, ok := item["duration_seconds"].(float64); ok {
			duration = sql.NullInt64{Int64: int64(v), Valid: true}
		}

		// capture_time
		captureTime := time.Now()
		if ts, ok := item["capture_time"].(string); ok && ts != "" {
			if parsed, err := time.Parse(parseLayout, ts); err == nil {
				captureTime = parsed
			}
		}

		s.logger.Printf(
			"Inserindo ScreenRecording[%d]: ID=%s, MachineID=%s, User=%s, Video=%s, Thumb=%s, Size=%d, Duration=%d, CaptureTime=%s",
			i+1, id, machineID, username, videoPath, videoThumbPath,
			fileSize.Int64, duration.Int64, captureTime.Format(parseLayout),
		)

		if s.config.DB.Driver == "mysql" {
			insertSQL := `
            INSERT INTO screen_recordings
                (id, machine_id, username, video_path, video_thumb_path, file_size_bytes,
                 duration_seconds, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
			_, err = s.db.Exec(insertSQL,
				id, machineID, username, videoPath, videoThumbPath,
				fileSize, duration, captureTime,
			)
		} else {
			insertSQL := `
            INSERT INTO screen_recordings
                (id, machine_id, username, video_path, video_thumb_path, file_size_bytes,
                 duration_seconds, created_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
			_, err = s.db.Exec(insertSQL,
				id, machineID, username, videoPath, videoThumbPath,
				fileSize, duration, captureTime,
			)
		}

		if err != nil {
			s.logger.Printf("❌ Erro ao inserir ScreenRecording[%d] (User=%s, Video=%s): %v", i+1, username, videoPath, err)
		} else {
			s.logger.Printf("✅ ScreenRecording[%d] inserido com sucesso (User=%s, Video=%s)", i+1, username, videoPath)
		}
	}

	return nil
}

func (s *service) logError(message string, err error) {
	if s.logger != nil {
		s.logger.Printf("ERRO - %s: %v", message, err)
	} else {
		log.Printf("ERRO - %s: %v", message, err)
	}
}

// Funções para gerenciar o serviço do Windows
func installService() error {
	exepath, err := os.Executable()
	if err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err == nil {
		s.Close()
		return fmt.Errorf("serviço %s já existe", serviceName)
	}

	s, err = m.CreateService(serviceName, exepath, mgr.Config{
		DisplayName: serviceDesc,
		StartType:   mgr.StartAutomatic,
	})
	if err != nil {
		return err
	}
	defer s.Close()

	err = eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Warning|eventlog.Info)
	if err != nil {
		s.Delete()
		return fmt.Errorf("SetupEventLogSource() falhou: %s", err)
	}

	return nil
}

func removeService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("erro ao conectar ao SCM: %v", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("erro ao abrir serviço %s: %v", serviceName, err)
	}
	defer s.Close()

	// Primeiro tenta parar o serviço
	status, err := s.Control(svc.Stop)
	if err == nil {
		// Espera o serviço realmente parar
		timeout := time.Now().Add(10 * time.Second)
		for status.State != svc.Stopped {
			if time.Now().After(timeout) {
				break
			}
			time.Sleep(500 * time.Millisecond)
			status, err = s.Query()
			if err != nil {
				break
			}
		}
	}

	// Remove o serviço
	if err := s.Delete(); err != nil {
		return fmt.Errorf("erro ao remover serviço: %v", err)
	}

	// 🔑 Garante que o processo atual finalize
	go func() {
		time.Sleep(1 * time.Second) // dá tempo do SCM processar
		os.Exit(0)                  // encerra o executável atual
	}()

	return nil
}

func startService() error {
	fmt.Printf("Conectando ao Service Control Manager...\n")
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("erro ao conectar ao SCM (execute como Administrador): %v", err)
	}
	defer m.Disconnect()

	fmt.Printf("Abrindo serviço %s...\n", serviceName)
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("serviço não encontrado (instale primeiro): %v", err)
	}
	defer s.Close()

	fmt.Printf("Verificando status atual do serviço...\n")
	status, err := s.Query()
	if err != nil {
		return fmt.Errorf("erro ao consultar status: %v", err)
	}

	if status.State == svc.Running {
		return fmt.Errorf("serviço já está rodando")
	}

	fmt.Printf("Iniciando serviço...\n")
	err = s.Start("is", "manual-started")
	if err != nil {
		return fmt.Errorf("erro ao iniciar serviço: %v", err)
	}

	fmt.Printf("Serviço iniciado com sucesso!\n")
	return nil
}

// runDirectly executa o programa em modo de console para depuração.
func runDirectly() {
	fmt.Println("Executando em modo de depuração direta (não como serviço)...")

	s := &service{}

	// 1. Inicializa o serviço (config, logger, banco de dados)
	if err := s.initialize(); err != nil {
		log.Fatalf("ERRO FATAL na inicialização: %v", err)
	}
	defer s.cleanup() // Garante que a conexão com o DB seja fechada ao sair

	// 2. Reseta tracking de scripts
	s.resetScriptTracking()

	// 3. Executa a primeira coleta de dados
	fmt.Println("Executando a primeira coleta de dados...")
	s.executeDailyScripts()
	s.executePeriodicScripts()
	fmt.Println("✅ Coleta inicial concluída")

	// 4. Canal para parar todas as goroutines
	stopChan := make(chan struct{})

	// 5. 🔹 INICIA TODAS AS GOROUTINES PERSISTENTES
	s.startBackgroundTasks(stopChan)

	fmt.Println("📊 Sistema de coleta de dados operacional em modo debug")
	fmt.Println("Pressione Ctrl+C para sair")

	// 6. Aguarda sinal de interrupção (Ctrl+C)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	<-sigChan
	fmt.Println("\n🛑 Sinal de interrupção recebido, encerrando...")
	close(stopChan)
	time.Sleep(2 * time.Second) // Aguarda goroutines finalizarem
	fmt.Println("✅ Encerrado com sucesso")
}

func stopService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("não foi possível acessar o serviço: %v", err)
	}
	defer s.Close()

	// Tenta parar normalmente
	status, err := s.Control(svc.Stop)
	if err != nil {
		return fmt.Errorf("não foi possível enviar controle=%d: %v", svc.Stop, err)
	}

	timeout := time.Now().Add(3 * time.Second) // aguarda só 3s
	for status.State != svc.Stopped {
		if time.Now().After(timeout) {
			break
		}
		time.Sleep(200 * time.Millisecond)
		status, err = s.Query()
		if err != nil {
			break
		}
	}

	// Se ainda não parou, força via taskkill
	if status.State != svc.Stopped {
		// Obtém informações do serviço
		cfg, err := s.Config()
		if err == nil {
			// Força matar o executável associado
			exe := filepath.Base(cfg.BinaryPathName)
			cmd := exec.Command("taskkill", "/F", "/IM", exe)
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("falha ao forçar parada (taskkill %s): %v", exe, err)
			}
			return nil
		}
		return fmt.Errorf("serviço não parou e não foi possível obter config para forçar kill")
	}

	return nil
}
