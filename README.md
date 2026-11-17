# FaaS Agent

Agente leve de execução serverless (**Data Plane / Edge Node**) integrante do ecossistema **FaaSwitch (v2.0)** — uma plataforma *cloud-native* em Go voltada à execução e ao roteamento serverless adaptativo no *continuum* Edge/Fog Computing (evolução arquitetural do protótipo original de roteamento adaptativo desenvolvido em Python).

O `faas-agent` foi projetado sob a filosofia Unix: é um componente **100% autônomo e independente** escrito em Go. Ele roda diretamente nos nós de borda (*Function Hosts*), gerencia o ciclo de vida e o escalonamento horizontal de containers localmente via **Docker Daemon** (`Docker SDK for Go`) e expõe uma API HTTP para deploy, invocação balanceada e gerenciamento de funções serverless.

---

## Arquitetura no Ecossistema FaaSwitch

1. **FaaS Manager (Control Plane):** Painel central de controle, catálogo de funções, gerenciamento de dispositivos e auditoria de logs.
2. **FaaSwitch (Camada Intermediária):** Motor decisório e roteador adaptativo entre o Control Plane e os nós de borda.
3. **FaaS Agent (Data Plane — este repositório):** Agente executado nos hosts de borda para provisionamento, balanceamento de carga local (*Least Connections*), *Auto-Scaling* horizontal de réplicas e controle de ociosidade (*Scale-Down* e *Scale-to-Zero*). Pode operar integrado ao ecossistema ou de forma isolada (`standalone`).

---

## Principais Funcionalidades

- **Auto-Scaling Horizontal Intra-Nó (`1:N` réplicas):** Cria novas réplicas de container automaticamente (`<função>-rep-1`, `<função>-rep-2`, ...) quando todas as réplicas ativas atingem o limite de concorrência (`maxConcurrency`), respeitando o teto configurável de containers por função (`maxReplicas`).
- **Load Balancing por *Least Connections*:** Direciona cada requisição para a réplica `RUNNING` com o menor número de chamadas em andamento (`activeRequests`).
- **Ciclo de Vida Inteligente (*Idle Reaper*):**
  - **Scale-Down:** Réplicas extras criadas durante picos de tráfego são removidas (`ContainerRemove`) ao ficarem ociosas além do `idleTimeoutSeconds`.
  - **Scale-to-Zero:** A réplica primária (`#1`) é pausada (`ContainerStop`) ao ficar ociosa, liberando memória RAM no host, e acordada automaticamente (*Cold Start* transparente) na próxima invocação.
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
   go run main.go
   ```
   O servidor HTTP iniciará escutando na porta `:8081` com o *Auto-Scaler* e o *Idle Reaper* ativos em background.

---

## Endpoints da API

### `POST /deploy`
Verifica se a imagem existe localmente (ou faz o pull caso não exista), cria e inicia a réplica primária (`<functionName>-rep-1`) com mapeamento dinâmico de porta (`127.0.0.1:<porta_livre> -> 8080/tcp`), aguarda o `/health` responder `200 OK` e registra a função no agente.

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
Lista todas as funções registradas no nó, suas configurações de escalonamento e o estado individual de cada réplica no pool (`replicaName`, `containerId`, `hostPort`, `status`, `activeRequests` e `lastUsedAt`).

```bash
curl http://localhost:8081/functions
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

### `DELETE /function/{functionName}`
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
- [x] Gerenciamento de ciclo de vida (`GET /functions`, `DELETE /function/{functionName}`, *Scale-to-Zero* com `ContainerStop` e *Cold Start* automático)
- [x] Escalabilidade horizontal intra-nó (*Auto-Scaling* de múltiplas réplicas, balanceamento *Least Connections* e *Scale-Down* automático)
- [ ] Envio de telemetria de execução para o **FaaS Manager** (`/api/execution-logs`)
