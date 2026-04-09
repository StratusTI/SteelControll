package scripts

import (
	"encoding/json"
	"os/exec"
	"syscall"
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

// Script é a interface que todos os scripts devem implementar
type Script interface {
	Execute(args ...string) ([]map[string]interface{}, error)
	Name() string
}

// Result representa o resultado de um script
type Result struct {
	Data  []map[string]interface{}
	Error error
}

// ToJSON converte os dados para JSON
func (r *Result) ToJSON() ([]byte, error) {
	return json.MarshalIndent(r.Data, "", "  ")
}
