// funcionario_data.go
package scripts

import (
	"fmt"
	"os"
	"strings"
)

// FuncionarioData representa os dados do funcionário
type FuncionarioData struct {
	Username string `json:"username"`
}

// FuncionarioDataScript implementa a interface Script
type FuncionarioDataScript struct{}

func (f *FuncionarioDataScript) Name() string {
	return "funcionario_data"
}

func (f *FuncionarioDataScript) Execute(args ...string) ([]map[string]interface{}, error) {
	// Coleta dados do funcionário atual do Windows
	username := os.Getenv("USERNAME")
	if strings.TrimSpace(username) == "" {
		return []map[string]interface{}{
			{
				"error":  "não foi possível obter o username do Windows",
				"status": "error",
			},
		}, fmt.Errorf("username vazio")
	}

	// Prepara os dados
	result := FuncionarioData{
		Username: strings.ToLower(strings.TrimSpace(username)),
	}

	// Converte para []map[string]interface{}
	return []map[string]interface{}{
		{
			"username": result.Username,
		},
	}, nil
}
