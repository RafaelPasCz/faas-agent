# FaaS Agent

Agente leve de execução serverless (**Data Plane / Edge Node**) integrante do ecossistema **FaaSwitch (v2.0)** — uma plataforma *cloud-native* em Go voltada à execução e ao roteamento serverless adaptativo no *continuum* Edge/Fog Computing (evolução arquitetural do protótipo original de roteamento adaptativo desenvolvido em Python).

O `faas-agent` foi projetado sob a filosofia Unix: é um componente **100% autônomo e independente** escrito em Go. Ele roda diretamente nos nós de borda (*Function Hosts*), gerencia o ciclo de vida e o escalonamento horizontal de containers localmente via **Docker Daemon** (`Docker SDK for Go`) e expõe uma API HTTP para deploy, invocação balanceada, consulta de estado e histórico persistente de execuções.

---

## Arquitetura no Ecossistema FaaSwitch

1. **FaaS Manager (Control Plane):** Painel central de controle, catálogo de funções, gerenciamento de dispositivos e auditoria de logs.
2. **FaaSwitch (Camada Intermediária):** Motor decisório e roteador adaptativo entre o Control Plane e os nós de borda.
3. **FaaS Agent (Data Plane — este repositório):** Agente executado nos hosts de borda para provisionamento, balanceamento de carga local (*Least Connections*), *Auto-Scaling* horizontal de réplicas, controle de ociosidade (*Scale-Down* e *Scale-to-Zero*) e registro local de logs. Opera de forma totalmente desacoplada (`standalone`).

---

## Principais Funcionalidades

- **Auto-Scaling Horizontal Intra-Nó (`1:N` réplicas):** Cria novas réplicas de container automaticamente (`<função>-rep-1`, `<função>-rep-2`, ...) quando todas as réplicas ativas atingem o limite de concorrência (`maxConcurrency`), respeitando o teto configurável de containers por função (`maxReplicas`).
- **Load Balancing por *Least Connections*:** Direciona cada requisição para a réplica `RUNNING` com o menor número de chamadas em andamento (`activeRequests`).
- **Estado Consolidado da Função:** Calcula e expõe em tempo real o estado consolidado de cada função (`status`, `executionState`, `activeRequests`, `runningReplicas`, `totalReplicas` e `lastUsedAt`), tanto na listagem geral quanto por função individual.
- **Ciclo de Vida Inteligente (*Idle Reaper*):**
  - **Scale-Down:** Réplicas extras criadas durante picos de tráfego são removidas (`ContainerRemove`) ao ficarem ociosas além do `idleTimeoutSeconds`.
  - **Scale-to-Zero:** A réplica primária (`#1`) é pausada (`ContainerStop`) ao ficar ociosa, liberando memória RAM no host, e acordada automaticamente (*Cold Start* transparente) na próxima invocação.
- **Persistência de Estado das Funções (`data/functions.json`):** Salva as funções implantadas de forma atômica (configurável via `FAAS_AGENT_STATE_FILE`) e reconcilia automaticamente com os containers Docker existentes no boot (re-adotando containers `RUNNING` ou `STOPPED` pela label `faas.function`).
- **Persistência de Logs de Execução (JSON Lines):** Registra cada invocação em memória (últimas `1000` execuções) e persiste em disco no formato *append-only* (`data/execution_logs.jsonl`, configurável via `FAAS_AGENT_LOG_FILE`) com compactação atômica automática ao atingir `1200` linhas, recarregando o histórico e o último `id` ao reiniciar.
- **Alta Performance de Rede:** Pool de conexões TCP *Keep-Alive* customizado (`http.Transport` com até 100 conexões ociosas por host) para eliminar gargalos de abertura de sockets em rajadas concorrentes.

---

## Tecnologias e Pré-requisitos

- **Go:** `1.27+` (ou compatível com o `go.mod`)
- **Docker Engine:** Rodando localmente com acesso ao socket `/var/run/docker.sock`
- **Docker SDK for Go:** `github.com/docker/docker v27.3.1+incompatible`

---

## Padrão de Container de Função (Runtime Python)

Em `templates/python3-http/` está o template base para criação de funções em Python (`python:3.12-slim` + `Flask` + `Waitress`):

- **Porta interna padrão:** `8080/tcp`
- **Threads por réplica (`WORKERS`):** `8` worker threads por padrão (configurável via variável de ambiente `WORKERS`).
- **Rotas internas do container:**
  - `GET /health`: Healthcheck consultado pelo agente no deploy, no *Scale-Up* e no *Cold Start*.
  - `POST /`: Recebe o evento repassado pelo agente e executa `handle(event, context)` em `function/handler.py`.

Para construir a imagem de exemplo localmente:
```bash
docker build -t switchfaas/hello-python:latest ./templates/python3-http
```

---

## Como Executar o Agente

1. **Clonar o repositório:**
   ```bash
   git clone git@github.com:RafaelPasCz/faas-agent.git
   cd faas-agent
   ```

2. **Baixar as dependências:**
   ```bash
   go mod tidy
   ```

3. **Executar o agente:**
   ```bash
   go run .
   ```
   O servidor HTTP iniciará escutando na porta `:8081` com o *Auto-Scaler*, *Idle Reaper*, restauração automática das funções implantadas (`data/functions.json`, customizável com `FAAS_AGENT_STATE_FILE`) e persistência de logs ativos (`data/execution_logs.jsonl`, customizável com `FAAS_AGENT_LOG_FILE`).

---

## Estrutura dos Arquivos

```text
faas-agent/
├── main.go       # Struct Agent, injeção de dependências e inicialização do servidor HTTP
├── models.go     # Constantes, structs de domínio e cálculo de snapshot consolidado
├── handlers.go   # Handlers HTTP (/deploy, /functions, /function/{name}, /invoke/{name})
├── docker.go     # Ciclo de vida de containers, Cold Start, Auto-Scaling e Idle Reaper
├── state.go      # Persistência de funções (data/functions.json) e reconciliação no boot
└── logs.go       # ExecutionLogStore (.jsonl append-only + compactação atômica) e GET /logs
```

---

## Endpoints da API

### `POST /deploy`
Verifica se a imagem existe localmente (ou faz o pull caso não exista), cria e inicia a réplica primária (`<functionName>-rep-1`) com mapeamento dinâmico de porta (`127.0.0.1:<porta_livre> -> 8080/tcp`), aguarda o `/health` responder `200 OK`, registra a função no agente e persiste o estado em `data/functions.json`.

- Retorna `409 Conflict` caso a função já esteja implantada na sessão atual do agente.
- Caso haja um container órfão de sessão anterior com o mesmo nome, ele só é removido após validar a label `faas.function` (protegendo containers externos do host).

**Payload (`application/json`):**
```json
{
  "functionName": "hello-python",
  "imageName": "switchfaas/hello-python:latest",
  "idleTimeoutSeconds": 60,
  "maxReplicas": 3,
  "maxConcurrency": 4
}
```
| Campo | Obrigatório | Padrão | Descrição |
| :--- | :---: | :---: | :--- |
| `functionName` | Sim | — | Nome identificador da função |
| `imageName` | Sim | — | Imagem Docker local ou do Docker Hub |
| `idleTimeoutSeconds` | Não | `60` | Tempo máximo de ociosidade (em segundos) antes do *Scale-Down* / *Scale-to-Zero* |
| `maxReplicas` | Não | `3` | Número máximo de containers simultâneos para esta função no host |
| `maxConcurrency` | Não | `4` | Requisições simultâneas por réplica antes de disparar o *Scale-Up* de uma nova réplica |

**Exemplo via `curl`:**
```bash
curl -X POST http://localhost:8081/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "functionName": "hello-python",
    "imageName": "switchfaas/hello-python:latest",
    "idleTimeoutSeconds": 30,
    "maxReplicas": 3,
    "maxConcurrency": 4
  }'
```

### `GET /functions`
Lista todas as funções registradas no nó com seus campos consolidados e o detalhe individual de cada réplica no pool:
- Campos consolidados da função: `status` (`"RUNNING"` ou `"STOPPED"`), `executionState` (`"IDLE"` ou `"EXECUTING"`), `activeRequests`, `runningReplicas`, `totalReplicas` e `lastUsedAt`.
- Campos de cada réplica em `replicas`: `replicaName`, `containerId`, `hostPort`, `status`, `activeRequests` e `lastUsedAt`.

```bash
curl http://localhost:8081/functions
```

### `GET /function/{functionName}` (ou `GET /functions/{functionName}`)
Retorna o objeto JSON consolidado e o pool de réplicas de uma função específica implantada no nó (ou `404 Not Found` caso não exista).

```bash
curl http://localhost:8081/function/hello-python
```

### `POST /invoke/{functionName}`
Atua como balanceador de carga (*Least Connections*) e proxy reverso para o pool de réplicas da função:
- Se houver réplica `RUNNING` com folga, encaminha imediatamente (*Warm Start*).
- Se a réplica estiver `STOPPED`, realiza o *Cold Start* transparente.
- Se todas as réplicas `RUNNING` estiverem com `activeRequests >= maxConcurrency` e `len(replicas) < maxReplicas`, provisiona uma nova réplica automaticamente (*Scale-Up*).

Retorna na resposta HTTP os cabeçalhos de telemetria:
- `X-Execution-Duration-Ms`: tempo total em milissegundos
- `X-Cold-Start`: `true` (se exigiu *Cold Start* ou *Scale-Up*) ou `false` (*Warm Start*)
- `X-Replica-Name`: nome da réplica que processou a chamada (ex.: `hello-python-rep-1`)

```bash
curl -i -X POST http://localhost:8081/invoke/hello-python \
  -H "Content-Type: application/json" \
  -d '{"name": "Rafael"}'
```

### `GET /logs`
Retorna em formato JSON array (`[]ExecutionLog`) o histórico das últimas execuções registradas pelo agente (`id`, `functionName`, `replicaName`, `statusCode`, `requestDurationMs`, `success`, `coldStart`, `executedAt` e `errorMessage` quando houver falha).

**Parâmetros opcionais de query string:**
- `functionName`: filtra apenas logs da função informada.
- `afterId`: retorna apenas registros com `id > afterId` (ideal para coleta incremental pelo Control Plane sem duplicações).
- `limit`: limita a quantidade de registros retornados (com `afterId`, retorna os primeiros `limit` após o cursor; sem `afterId`, retorna os `limit` mais recentes).

```bash
# Todas as execuções em memória (até 1000)
curl http://localhost:8081/logs

# Filtrar por função e limitar às últimas 20
curl "http://localhost:8081/logs?functionName=hello-python&limit=20"

# Coleta incremental a partir do ID 15
curl "http://localhost:8081/logs?afterId=15&limit=100"
```

### `DELETE /function/{functionName}` (ou `DELETE /functions/{functionName}`)
Remove todos os containers (réplicas) da função no Docker e apaga o registro no agente (retorna `409 Conflict` caso haja requisições em andamento no momento).

```bash
curl -X DELETE http://localhost:8081/function/hello-python
```

---

## Roadmap de Desenvolvimento

- [x] Servidor HTTP nativo na porta `:8081`
- [x] Integração com Docker Daemon via SDK oficial (`ImagePull`, `ContainerCreate`, `ContainerStart`)
- [x] Endpoint `POST /deploy` com verificação de imagem local
- [x] Padronização de containers de funções (contrato de porta/healthcheck e template Python em `templates/python3-http`)
- [x] Mapeamento dinâmico de portas (*Port Binding*) por réplica
- [x] Endpoint de invocação (`POST /invoke/{functionName}`) com medição de latência e pool de conexões HTTP *Keep-Alive*
- [x] Gerenciamento de ciclo de vida (`GET /functions`, `GET /function/{functionName}`, `DELETE /function/{functionName}`, *Scale-to-Zero* com `ContainerStop` e *Cold Start* automático)
- [x] Escalabilidade horizontal intra-nó (*Auto-Scaling* de múltiplas réplicas, balanceamento *Least Connections* e *Scale-Down* automático)
- [x] Histórico persistente de logs de execução em JSON Lines (`.jsonl`) com endpoint `GET /logs` e coleta incremental (`afterId`)
- [x] Persistência do estado das funções em disco (`data/functions.json`) com reconciliação de containers no boot e validação de segurança por label (`faas.function`)
