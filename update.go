package main

import (
	"archive/zip"
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
	taskName       = "produtividade"
	updateCheckURL = "https://painel.stratustelecom.com.br/main/produtividade/update.json"
)

type UpdateInfo struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

type Updater struct {
	logger      *log.Logger
	currentPath string
	taskName    string
	versionPath string
}

func main() {
	// Configura logger
	logger := log.New(os.Stdout, "[UPDATER] ", log.LstdFlags|log.Lshortfile)

	// Obtém o caminho do executável principal
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
	// 1. Verifica conectividade
	if err := u.checkConnectivity(); err != nil {
		return fmt.Errorf("falha na conectividade: %v", err)
	}

	// 2. Busca informações de atualização
	updateInfo, err := u.fetchUpdateInfo()
	if err != nil {
		return fmt.Errorf("erro ao buscar informações de atualização: %v", err)
	}

	// 3. Verifica se precisa atualizar
	needsUpdate, err := u.needsUpdate(updateInfo.Version)
	if err != nil {
		return fmt.Errorf("erro ao verificar necessidade de atualização: %v", err)
	}

	if !needsUpdate {
		u.logger.Printf("Sistema já está na versão mais recente: %s", updateInfo.Version)
		return nil
	}

	u.logger.Printf("Nova versão disponível: %s", updateInfo.Version)

	// 4. Para a tarefa agendada
	wasRunning, err := u.stopTask()
	if err != nil {
		return fmt.Errorf("erro ao parar tarefa: %v", err)
	}

	// 5. Faz backup do executável atual
	backupPath, err := u.createBackup()
	if err != nil {
		u.logger.Printf("Aviso: não foi possível criar backup: %v", err)
	} else {
		u.logger.Printf("Backup criado em: %s", backupPath)
	}

	// 6. Baixa nova versão
	tempFile, err := u.downloadNewVersion(updateInfo.URL)
	if err != nil {
		u.restoreFromBackup(backupPath)
		return fmt.Errorf("erro ao baixar nova versão: %v", err)
	}
	defer os.Remove(tempFile)

	// 7. Extrai e substitui arquivos do ZIP
	if err := u.extractAndReplace(tempFile); err != nil {
		u.restoreFromBackup(backupPath)
		return fmt.Errorf("erro ao aplicar nova versão: %v", err)
	}

	// 8. Atualiza arquivo de versão
	if err := u.updateVersionFile(updateInfo.Version); err != nil {
		u.logger.Printf("Aviso: erro ao atualizar arquivo de versão: %v", err)
	}

	// 9. Reinicia a tarefa se estava rodando
	if wasRunning {
		if err := u.startTask(); err != nil {
			return fmt.Errorf("erro ao reiniciar tarefa: %v", err)
		}
	}

	// 10. Remove backup se tudo deu certo
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

func (u *Updater) fetchUpdateInfo() (*UpdateInfo, error) {
	u.logger.Printf("Buscando informações de atualização em: %s", updateCheckURL)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(updateCheckURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status HTTP inválido: %d", resp.StatusCode)
	}

	var updateInfo UpdateInfo
	if err := json.NewDecoder(resp.Body).Decode(&updateInfo); err != nil {
		return nil, fmt.Errorf("erro ao decodificar JSON: %v", err)
	}

	if updateInfo.Version == "" || updateInfo.URL == "" {
		return nil, fmt.Errorf("informações de atualização inválidas")
	}

	u.logger.Printf("Informações recebidas - Versão: %s, URL: %s", updateInfo.Version, updateInfo.URL)
	return &updateInfo, nil
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

	// Verifica se a tarefa existe e está em execução
	checkCmd := exec.Command("schtasks", "/Query", "/TN", u.taskName, "/FO", "LIST", "/V")
	output, err := checkCmd.CombinedOutput()
	if err != nil {
		// Tarefa pode não existir
		u.logger.Printf("Tarefa não encontrada ou erro ao consultar: %v", err)
		return false, nil
	}

	outputStr := string(output)
	wasRunning := strings.Contains(outputStr, "Status:") && strings.Contains(outputStr, "Running")

	if wasRunning {
		u.logger.Println("Parando tarefa agendada...")

		// Para a tarefa
		stopCmd := exec.Command("schtasks", "/End", "/TN", u.taskName)
		if err := stopCmd.Run(); err != nil {
			u.logger.Printf("Aviso: erro ao parar tarefa: %v", err)
		}

		// Aguarda um pouco para garantir que parou
		time.Sleep(2 * time.Second)

		// Force kill do processo se ainda estiver rodando
		u.forceKillProcess()

		u.logger.Println("✓ Tarefa parada")
	} else {
		u.logger.Println("Tarefa não estava em execução")
	}

	return wasRunning, nil
}

func (u *Updater) forceKillProcess() {
	u.logger.Println("Encerrando processo relacionado à tarefa...")

	exeName := filepath.Base(u.currentPath)
	cmd := exec.Command("taskkill", "/F", "/IM", exeName)
	if err := cmd.Run(); err != nil {
		u.logger.Printf("Processo não estava rodando ou já foi encerrado: %v", err)
	} else {
		u.logger.Println("✓ Processo encerrado")
	}

	// Aguarda um pouco para garantir que o processo foi encerrado
	time.Sleep(3 * time.Second)
}

func (u *Updater) createBackup() (string, error) {
	backupPath := u.currentPath + ".backup." + time.Now().Format("20060102_150405")

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

	_, err = io.Copy(backupFile, sourceFile)
	if err != nil {
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

	tempFile := filepath.Join(os.TempDir(), "update_"+time.Now().Format("20060102_150405")+".zip")

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

	u.logger.Printf("✓ Download concluído: %d bytes salvos em %s", size, tempFile)

	if size < 1024*10 { // menos de 10 KB é suspeito
		os.Remove(tempFile)
		return "", fmt.Errorf("arquivo baixado muito pequeno (%d bytes)", size)
	}

	return tempFile, nil
}

func (u *Updater) extractAndReplace(zipPath string) error {
	u.logger.Println("Extraindo nova versão e substituindo arquivos...")

	destDir := filepath.Dir(u.currentPath)

	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("erro ao abrir zip: %v", err)
	}
	defer r.Close()

	for _, f := range r.File {
		relPath := filepath.FromSlash(f.Name)
		targetPath := filepath.Join(destDir, relPath)

		if f.FileInfo().IsDir() {
			os.MkdirAll(targetPath, os.ModePerm)
			continue
		}

		// Cria diretório se necessário
		if err := os.MkdirAll(filepath.Dir(targetPath), os.ModePerm); err != nil {
			return fmt.Errorf("erro ao criar diretório: %v", err)
		}

		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("erro ao abrir arquivo no zip: %v", err)
		}

		// Remove arquivo antigo se existir
		os.Remove(targetPath)

		out, err := os.Create(targetPath)
		if err != nil {
			rc.Close()
			return fmt.Errorf("erro ao criar arquivo de destino: %v", err)
		}

		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			return fmt.Errorf("erro ao extrair %s: %v", relPath, err)
		}

		u.logger.Printf("Atualizado: %s", relPath)
	}

	u.logger.Println("✓ Atualização aplicada com sucesso")
	return nil
}

func (u *Updater) updateVersionFile(newVersion string) error {
	return os.WriteFile(u.versionPath, []byte(newVersion), 0644)
}

func (u *Updater) startTask() error {
	u.logger.Println("Iniciando tarefa agendada...")

	// Inicia a tarefa
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

	// Remove arquivo corrompido se existir
	os.Remove(u.currentPath)

	// Restaura backup
	sourceFile, err := os.Open(backupPath)
	if err != nil {
		u.logger.Printf("Erro ao abrir backup: %v", err)
		return
	}
	defer sourceFile.Close()

	destFile, err := os.Create(u.currentPath)
	if err != nil {
		u.logger.Printf("Erro ao restaurar backup: %v", err)
		return
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	if err != nil {
		u.logger.Printf("Erro ao copiar backup: %v", err)
		return
	}

	u.logger.Println("✓ Backup restaurado")
}
