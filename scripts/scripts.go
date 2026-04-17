package scripts

import (
	"encoding/json"
)

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
