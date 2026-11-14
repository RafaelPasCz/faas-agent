# Contexto do Projeto: Ecossistema Switchfaas (TCC)

Este documento resume a arquitetura, o stack tecnológico e o estado atual de desenvolvimento do projeto de Trabalho de Conclusão de Curso (TCC) focado em **execução serverless adaptativa no continuum Edge/Fog Computing**.

---

## 1. Visão Geral da Arquitetura

O sistema completo é dividido em três camadas principais e desacopladas:

1. **FaaS Manager (Control Plane):** Painel central de controle, orquestração e observabilidade da infraestrutura física, catálogo de funções e auditoria de logs.
2. **Switchfaas (Camada Intermediária / Roteamento Adaptativo):** O motor decisório que ficará entre o Control Plane e os nós de borda, responsável pela execução serverless adaptativa. *(Em fase de planejamento futuro)*.
3. **FaaS Agent (Data Plane / Edge Node):** Agente leve de execução que roda diretamente nos dispositivos hosts na borda. Ele gerencia os containers localmente e expõe uma API para deploy e invocação de funções.

### Decisão Arquitetural Crítica: Independência das Pontas
Optou-se por desenvolver e amadurecer as duas pontas da arquitetura (**FaaS Manager** e **FaaS Agent**) antes de construir o intermediário (**Switchfaas**). 
* O **FaaS Agent** foi projetado sob a filosofia Unix: deve ser um componente **100% autônomo e independente**. Se um desenvolvedor quiser rodar apenas o binário do `faas-agent` em uma máquina isolada (sem o FaaS Manager e sem o Switchfaas), ele deve ser capaz de fazer deploy e invocar funções serverless localmente via API REST (e futuramente CLI).

---

## 2. Stack Tecnológico e Estado dos Componentes

### A. FaaS Manager (Concluído - V1 Estável)
O Control Plane já atingiu sua primeira versão madura, integrando front-end e back-end.
* **Back-end:** Java 17+, Spring Boot 3 (Web, Data JPA), Hibernate (`AuditingEntityListener`), PostgreSQL, Maven.
  * *Módulos:* Gerenciamento de Dispositivos (diferenciando Function Hosts e Prometheus Hosts), Catálogo de Funções Abstratas (imagem Docker, runtime), Instâncias de Função (vínculo dispositivo-função e status de deploy) e Logs de Execução (ingestão de telemetria imutável/append-only).
* **Front-end:** React (SPA), Tailwind CSS, Tremor UI.
  * *Estrutura:* Abas divididas em Dispositivos (cards expansíveis com métricas, deploys ativos e mini-feed de logs local), Catálogo de Funções e Auditoria Global de Logs (tabela com filtros reativos em memória por texto, data/hora exata, status code HTTP e resultado).

### B. FaaS Agent (Em Desenvolvimento Ativo)
O agente de execução que roda nos hosts físicos (ambiente de desenvolvimento atual: Linux Mint).
* **Linguagem:** Go (Golang) — escolhida pelo baixo consumo de memória (footprint mínimo na borda), compilação para binário nativo estático, alta performance em concorrência (goroutines) e por ser a linguagem nativa do ecossistema de containers.
* **Container Runtime:** Integração direta com o Docker Daemon via socket (`/var/run/docker.sock`) utilizando o **Docker SDK for Go** oficial (sem uso de `os/exec` no terminal).

---

## 3. Fase Atual do `faas-agent` (Onde paramos)

O ambiente Go foi configurado e o módulo `faas-agent` foi inicializado.

### O que já está funcionando:
1. **Servidor HTTP Nativo:** Rodando na porta `8081` utilizando o pacote padrão `net/http`.
2. **Endpoint `POST /deploy`:** Recebe um payload JSON mapeado para uma struct Go (`DeployRequest` contendo `functionName` e `imageName`).
3. **Integração com Docker SDK:** Ao receber a requisição no `/deploy`, o agente conecta ao socket local do Docker e executa com sucesso o ciclo básico:
   * `ImagePull`: Baixa a imagem solicitada do Docker Hub.
   * `ContainerCreate`: Cria o container nomeando-o com o `functionName`.
   * `ContainerStart`: Inicia o container em background.

### Notas sobre Dependências (Go Mod):
Para evitar conflitos históricos de nomenclatura entre os repositórios `docker/docker` e `moby/moby` (e o erro `undefined: sockets.DialPipe`), as dependências no `go.mod` foram estabilizadas utilizando:
* `github.com/docker/docker@v27.3.1+incompatible`
* `github.com/docker/go-connections@v0.5.0`

---

**OBS adicionada pelo o humano: tambem precisamos definir os ambientes que serão rodaveis no faas-agent, bem como um padrão para os containers que irão rodar (Dockerfile)**

## 4. Próximos Passos Imediatos para o `faas-agent`

1. **Mapeamento Dinâmico de Portas (Port Binding):** Alterar a criação do container no `/deploy` para expor e mapear a porta interna da função para uma porta disponível no host (necessário para que o agente consiga se comunicar com o container via rede).
2. **Endpoint de Invocação (`POST /invoke/{functionName}`):** Criar a rota que atuará como um proxy reverso leve. Ela deverá descobrir em qual porta local o container da função alvo está rodando, repassar a requisição HTTP para lá, medir o tempo de duração (em ms), capturar o Status Code e devolver a resposta ao cliente.
3. **Gerenciamento de Ciclo de Vida:** Adicionar rotas para listar funções rodando no nó e remover/parar containers (`DELETE /function/{name}`).
4. **Integração de Telemetria:** Futuramente, permitir que o agente envie os logs de execução gerados no `/invoke` para o endpoint `/api/execution-logs` do FaaS Manager.
