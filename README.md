# FaaS Agent

Agente leve de execução serverless (**Data Plane / Edge Node**) integrante do ecossistema **SwitchFaaS** (Trabalho de Conclusão de Curso focado em execução serverless adaptativa no *continuum* Edge/Fog Computing).

O `faas-agent` foi projetado sob a filosofia Unix: é um componente **100% autônomo e independente** escrito em Go. Ele roda diretamente nos nós de borda (*Function Hosts*), gerencia o ciclo de vida de containers localmente comunicando-se via socket com o **Docker Daemon** (`Docker SDK for Go`) e expõe uma API HTTP leve para deploy, invocação e gerenciamento de funções serverless.

---

## Arquitetura no Ecossistema SwitchFaaS

1. **FaaS Manager (Control Plane):** Painel central de controle, catálogo de funções, gerenciamento de dispositivos e auditoria de logs.
2. **SwitchFaaS (Camada Intermediária):** Motor decisório e roteador adaptativo entre o Control Plane e os nós de borda.
3. **FaaS Agent (Data Plane — este repositório):** Agente executado nos hosts de borda para provisionamento, invocação local e controle de ociosidade (*Scale-to-Zero*) de funções em containers. Pode operar integrado ao ecossistema ou de forma isolada (`standalone`).

---

## Tecnologias e Pré-requisitos

- **Go:** `1.27+` (ou compatível com o `go.mod`)
- **Docker Engine:** Rodando localmente com acesso ao socket `/var/run/docker.sock`
- **Docker SDK for Go:** `github.com/docker/docker v27.3.1+incompatible`

---

## Padrão de Container de Função (Runtime Python)

Em `templates/python3-http/` está o template base para criação de funções em Python (`python:3.12-slim` + `Flask` + `Waitress`):

- **Porta interna padrão:** `8080/tcp`
- **Rotas internas do container:**
  - `GET /health`: Healthcheck usado pelo agente durante o *Cold Start*.
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
   O servidor HTTP iniciará escutando na porta `:8081` com o coletor de ociosidade (*Idle Reaper*) ativo em background.

---

## Endpoints da API

### `POST /deploy`
Verifica se a imagem existe localmente (ou faz o pull caso não exista), cria e inicia o container com mapeamento dinâmico de porta (`127.0.0.1:<porta_livre> -> 8080/tcp`) e registra a função no agente.

**Payload (`application/json`):**
```json
{
  "functionName": "hello-python",
  "imageName": "switchfaas/hello-python:latest",
  "idleTimeoutSeconds": 60
}
```
*(O campo `idleTimeoutSeconds` é opcional; padrão: `60` segundos).*

**Exemplo via `curl`:**
```bash
curl -X POST http://localhost:8081/deploy \
  -H "Content-Type: application/json" \
  -d '{"functionName": "hello-python", "imageName": "switchfaas/hello-python:latest", "idleTimeoutSeconds": 30}'
```

### `GET /functions`
Lista todas as funções registradas no nó, incluindo porta mapeada, status (`RUNNING` ou `STOPPED`), horário do último uso e requisições ativas.

```bash
curl http://localhost:8081/functions
```

### `POST /invoke/{functionName}`
Atua como proxy reverso para o container da função. Se o container estiver parado por ociosidade (`STOPPED`), realiza o *Cold Start* automaticamente (`ContainerStart` + espera pelo `/health`) antes de repassar a requisição.

Retorna os cabeçalhos `X-Execution-Duration-Ms` (tempo total em ms) e `X-Cold-Start` (`true` ou `false`).

```bash
curl -i -X POST http://localhost:8081/invoke/hello-python \
  -H "Content-Type: application/json" \
  -d '{"name": "Rafael"}'
```

### `DELETE /function/{functionName}`
Remove o container do Docker e apaga o registro da função no agente (desde que não haja requisições em execução no momento).

```bash
curl -X DELETE http://localhost:8081/function/hello-python
```

---

## Roadmap de Desenvolvimento

- [x] Servidor HTTP nativo na porta `:8081`
- [x] Integração com Docker Daemon via SDK oficial (`ImagePull`, `ContainerCreate`, `ContainerStart`)
- [x] Endpoint `POST /deploy`
- [x] Padronização de containers de funções (contrato de porta/healthcheck e template Python em `templates/python3-http`)
- [x] Mapeamento dinâmico de portas (*Port Binding*) no deploy
- [x] Endpoint de invocação (`POST /invoke/{functionName}`) atuando como proxy reverso com medição de latência
- [x] Gerenciamento de ciclo de vida (`GET /functions`, `DELETE /function/{functionName}`, *Scale-to-Zero* com `ContainerStop` e *Cold Start* automático)
- [ ] Envio de telemetria de execução para o **FaaS Manager** (`/api/execution-logs`)
