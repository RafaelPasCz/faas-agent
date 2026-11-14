# FaaS Agent

Agente leve de execução serverless (**Data Plane / Edge Node**) integrante do ecossistema **SwitchFaaS** (Trabalho de Conclusão de Curso focado em execução serverless adaptativa no *continuum* Edge/Fog Computing).

O `faas-agent` foi projetado sob a filosofia Unix: é um componente **100% autônomo e independente** escrito em Go. Ele roda diretamente nos nós de borda (*Function Hosts*), gerencia o ciclo de vida de containers localmente comunicando-se via socket com o **Docker Daemon** (`Docker SDK for Go`) e expõe uma API HTTP leve para deploy e invocação de funções serverless.

---

## Arquitetura no Ecossistema SwitchFaaS

1. **FaaS Manager (Control Plane):** Painel central de controle, catálogo de funções, gerenciamento de dispositivos e auditoria de logs.
2. **SwitchFaaS (Camada Intermediária):** Motor decisório e roteador adaptativo entre o Control Plane e os nós de borda.
3. **FaaS Agent (Data Plane — este repositório):** Agente executado nos hosts de borda para provisionamento e invocação local de funções em containers. Pode operar integrado ao ecossistema ou de forma isolada (`standalone`).

---

## Tecnologias e Pré-requisitos

- **Go:** `1.27+` (ou compatível com o `go.mod`)
- **Docker Engine:** Rodando localmente com acesso ao socket `/var/run/docker.sock`
- **Docker SDK for Go:** `github.com/docker/docker v27.3.1+incompatible`

---

## Como Executar

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
   O servidor HTTP iniciará escutando na porta `:8081`.

---

## Endpoints da API

### `POST /deploy`
Realiza o pull da imagem Docker, cria e inicia o container da função localmente.

**Payload (`application/json`):**
```json
{
  "functionName": "minha-funcao",
  "imageName": "nginx:alpine"
}
```

**Exemplo via `curl`:**
```bash
curl -X POST http://localhost:8081/deploy \
  -H "Content-Type: application/json" \
  -d '{"functionName": "hello-edge", "imageName": "nginx:alpine"}'
```

---

## Roadmap de Desenvolvimento

- [x] Servidor HTTP nativo na porta `:8081`
- [x] Integração com Docker Daemon via SDK oficial (`ImagePull`, `ContainerCreate`, `ContainerStart`)
- [x] Endpoint `POST /deploy`
- [ ] Padronização de containers de funções (contrato de porta/healthcheck e templates de Dockerfile por runtime)
- [ ] Mapeamento dinâmico de portas (*Port Binding*) no deploy
- [ ] Endpoint de invocação (`POST /invoke/{functionName}`) atuando como proxy reverso com medição de latência
- [ ] Gerenciamento de ciclo de vida (`GET /functions`, `DELETE /function/{name}`)
- [ ] Envio de telemetria de execução para o **FaaS Manager** (`/api/execution-logs`)
