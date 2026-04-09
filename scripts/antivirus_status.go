// antivirus_status.go
package scripts

import (
	"fmt"
	"os"
	"time"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

type AntivirusInfo struct {
	Timestamp   string `json:"timestamp"`
	ProductName string `json:"product_name"`
	IsEnabled   bool   `json:"is_enabled"`
	IsUpToDate  bool   `json:"is_uptodate"`
	Status      string `json:"status"`
}

// AntivirusStatusScript implementa a interface Script
type AntivirusStatusScript struct{}

func (a *AntivirusStatusScript) Name() string {
	return "antivirus_status"
}

func (a *AntivirusStatusScript) Execute(args ...string) ([]map[string]interface{}, error) {
	antivirusList, err := a.getAntivirusProducts()

	if err != nil {
		// Retorna erro como um item na lista
		hostname, _ := os.Hostname()
		return []map[string]interface{}{
			{
				"hostname": hostname,
				"error":    err.Error(),
				"status":   "error",
			},
		}, err
	}

	// Converte []AntivirusInfo para []map[string]interface{}
	result := make([]map[string]interface{}, 0, len(antivirusList))
	for _, av := range antivirusList {
		result = append(result, map[string]interface{}{
			"timestamp":    av.Timestamp,
			"product_name": av.ProductName,
			"is_enabled":   av.IsEnabled,
			"is_uptodate":  av.IsUpToDate,
			"status":       av.Status,
		})
	}

	return result, nil
}

// getAntivirusProducts obtém informações dos antivírus instalados
func (a *AntivirusStatusScript) getAntivirusProducts() ([]AntivirusInfo, error) {
	// Inicializa COM
	err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	if err != nil {
		return nil, fmt.Errorf("falha ao inicializar COM: %v", err)
	}
	defer ole.CoUninitialize()

	// Conecta ao WMI
	unknown, err := oleutil.CreateObject("WbemScripting.SWbemLocator")
	if err != nil {
		return nil, fmt.Errorf("falha ao criar objeto WMI: %v", err)
	}
	defer unknown.Release()

	wmi, err := unknown.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return nil, fmt.Errorf("falha ao obter interface IDispatch: %v", err)
	}
	defer wmi.Release()

	// Conecta ao namespace root\SecurityCenter2
	serviceRaw, err := oleutil.CallMethod(wmi, "ConnectServer", nil, "root\\SecurityCenter2")
	if err != nil {
		return nil, fmt.Errorf("falha ao conectar ao SecurityCenter2: %v", err)
	}
	service := serviceRaw.ToIDispatch()
	defer service.Release()

	// Executa a query WMI
	resultRaw, err := oleutil.CallMethod(service, "ExecQuery", "SELECT * FROM AntivirusProduct")
	if err != nil {
		return nil, fmt.Errorf("falha ao executar query WMI: %v", err)
	}
	result := resultRaw.ToIDispatch()
	defer result.Release()

	// Obtém a contagem
	countVar, err := oleutil.GetProperty(result, "Count")
	if err != nil {
		return nil, fmt.Errorf("falha ao obter contagem: %v", err)
	}
	count := int(countVar.Val)

	antivirusList := make([]AntivirusInfo, 0, count)
	timestamp := time.Now().Format("2006-01-02 15:04:05")

	// Itera sobre os produtos usando ItemIndex
	for i := 0; i < count; i++ {
		itemRaw, err := oleutil.CallMethod(result, "ItemIndex", i)
		if err != nil {
			continue
		}

		antivirus := itemRaw.ToIDispatch()
		if antivirus == nil {
			continue
		}

		// Obtém o nome do produto
		displayNameVar, err := oleutil.GetProperty(antivirus, "displayName")
		var displayName string
		if err == nil && displayNameVar.VT == ole.VT_BSTR {
			displayName = displayNameVar.ToString()
		}

		// Obtém o productState
		productStateVar, err := oleutil.GetProperty(antivirus, "productState")
		var productState uint32
		if err == nil {
			// Converte para uint32 de forma segura
			switch productStateVar.VT {
			case ole.VT_I4:
				productState = uint32(productStateVar.Val)
			case ole.VT_UI4:
				productState = uint32(productStateVar.Val)
			case ole.VT_I2:
				productState = uint32(productStateVar.Val)
			}
		}

		// Decodifica o productState
		// Bit 0x1000 (4096) = Antivírus habilitado
		// Bit 0x10 (16) = Definições atualizadas
		isEnabled := (productState & 0x1000) != 0
		isUpToDate := (productState & 0x10) != 0

		// Define o status
		status := "inactive"
		if isEnabled && isUpToDate {
			status = "active"
		}

		antivirusInfo := AntivirusInfo{
			Timestamp:   timestamp,
			ProductName: displayName,
			IsEnabled:   isEnabled,
			IsUpToDate:  isUpToDate,
			Status:      status,
		}

		antivirusList = append(antivirusList, antivirusInfo)
		antivirus.Release()
	}

	return antivirusList, nil
}
