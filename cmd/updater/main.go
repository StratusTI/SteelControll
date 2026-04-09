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
	"time"

	version "github.com/hashicorp/go-version"
)

const (
	taskName      = "produtividade"
	githubAPIURL  = "https://api.github.com/repos/StratusTI/SteelControll/releases/latest"
)

type Updater struct {
	logger      *log.Logger
	currentPath string
	taskName    string
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
		taskName:    taskName,
		versionPath: filepath.Join(filepath.Dir(mainExePath), "version.txt"),
	}

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

	// 6. Atualiza arquivo de versão
	if remoteVersion != "unknown" {
		if err := u.updateVersionFile(remoteVersion); err != nil {
			u.logger.Printf("Aviso: erro ao atualizar arquivo de versão: %v", err)
		}
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
	u.logger.Println("Verificando status da tarefa agendada...")

	checkCmd := exec.Command("schtasks", "/Query", "/TN", u.taskName, "/FO", "LIST", "/V")
	output, err := checkCmd.CombinedOutput()
	if err != nil {
		u.logger.Printf("Tarefa não encontrada ou erro ao consultar: %v", err)
		return false, nil
	}

	outputStr := string(output)
	wasRunning := strings.Contains(outputStr, "Status:") && strings.Contains(outputStr, "Running")

	if wasRunning {
		u.logger.Println("Parando tarefa agendada...")
		stopCmd := exec.Command("schtasks", "/End", "/TN", u.taskName)
		if err := stopCmd.Run(); err != nil {
			u.logger.Printf("Aviso: erro ao parar tarefa: %v", err)
		}
		time.Sleep(2 * time.Second)
		u.forceKillProcess()
		u.logger.Println("✓ Tarefa parada")
	} else {
		u.logger.Println("Tarefa não estava em execução")
	}

	return wasRunning, nil
}

func (u *Updater) forceKillProcess() {
	exeName := filepath.Base(u.currentPath)
	cmd := exec.Command("taskkill", "/F", "/IM", exeName)
	if err := cmd.Run(); err != nil {
		u.logger.Printf("Processo não estava rodando ou já foi encerrado: %v", err)
	} else {
		u.logger.Println("✓ Processo encerrado")
	}
	time.Sleep(3 * time.Second)
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
	u.logger.Println("Iniciando tarefa agendada...")
	cmd := exec.Command("schtasks", "/Run", "/TN", u.taskName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("erro ao iniciar tarefa: %v", err)
	}
	u.logger.Println("✓ Tarefa iniciada")
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
