# Steel ControlL

## Visão Geral

O **Steel Control** é um sistema de **monitoramento de produtividade** escrito em **Go**, voltado para coleta, consolidação e análise de atividades do usuário em estações de trabalho.

O foco do sistema é gerar **dados confiáveis de uso**, não fazer julgamento, gamificação ou UI. Ele coleta, normaliza e disponibiliza informações para consumo posterior.

---

## Responsabilidade do Sistema

O Steel Control é responsável por:

1. Coletar atividades do usuário (aplicações, navegação, tempo ativo)
2. Consolidar dados em uma estrutura padronizada
3. Executar de forma contínua e previsível
4. Permitir atualização e evolução controlada do agente

O sistema **não** é dashboard, **não** é sistema de RH e **não** faz avaliação humana. Ele apenas mede.

---

## Stack

* **Linguagem**: Go
* **Runtime**: Binário nativo
* **Execução**: Windows
* **Dependências**: Go Modules

---

## Estrutura de Pastas

```
.
├── scripts/            # Módulos de coleta por funcionalidade
│   ├── user_activity.go   # Atividade do usuário
│   ├── application_usage.go # Uso de aplicações
│   ├── browser_history.go # Histórico de navegação
│   └── ...
├── config.json         # Configuração base (ambiente de teste)
├── main.go             # Orquestração principal do agente
├── cmd/updater/        # Lógica de atualização do sistema
├── go.mod              # Definição do módulo
├── go.sum              # Lockfile de dependências
```

Cada arquivo em `scripts/` representa uma responsabilidade isolada de coleta.

---

## Pré-requisitos

* Go (versão compatível com `go.mod`)
* Windows 10 / Windows Server
* Permissões adequadas para leitura de processos e eventos do sistema

---

## Clonando o Projeto

```bash
git clone https://github.com/StratusTI/SteelControll.git
cd SteelControll
```

---

## Configuração

Arquivo `config.json` define parâmetros de execução (ex: ambiente, grupos de teste, flags de coleta).

Qualquer mudança de comportamento deve ser refletida nesse arquivo ou em um mecanismo de configuração equivalente.

---

## Build do Sistema

```bash
go build -ldflags="-s -w -H windowsgui" -o Produtividade.exe main.go
```

O binário gerado contém o agente completo.

---

## Execução

```bash
./Produtividade.exe
```

O processo deve ser executado de forma contínua. A estratégia de inicialização (serviço, scheduler, startup) depende do ambiente.

---

## Fluxo de Funcionamento (Alto Nível)

```
[ Sistema Operacional ]
          ↓
       Scripts
          ↓
   Consolidação
          ↓
       main.go
          ↓
   Saída / Persistência
```

* Os scripts **não se comunicam entre si**
* O `main.go` orquestra execução, intervalos e consolidação
* `cmd/updater/` trata evolução controlada do agente

---

## Versionamento

* Git como controle de versão
* Branch principal: `main`
* Commits pequenos e funcionais
* Mudanças estruturais exigem revisão do fluxo

---

## Boas Práticas do Projeto

* Manter scripts desacoplados
* Evitar dependências cruzadas entre módulos
* Não misturar coleta com decisão de negócio
* Testar novos scripts em ambiente controlado

---

## Onboarding Rápido

1. Instalar Go
2. Clonar o repositório
3. Ajustar `config.json` se necessário
4. Rodar `go build`
5. Executar o binário em ambiente de teste

Se você entendeu o papel de `scripts`, `main.go` e `cmd/updater/`, você já entendeu o sistema.

---

## Licença

Privado / Uso interno
