package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

func (a *Agent) deployHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Apenas o método POST é permitido", http.StatusMethodNotAllowed)
		return
	}

	var req DeployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}

	if req.FunctionName == "" || req.ImageName == "" {
		http.Error(w, "Os campos 'functionName' e 'imageName' são obrigatórios", http.StatusBadRequest)
		return
	}

	idleTimeout := defaultIdleTimeout
	if req.IdleTimeoutSeconds > 0 {
		idleTimeout = time.Duration(req.IdleTimeoutSeconds) * time.Second
	}

	maxReplicas := defaultMaxReplicas
	if req.MaxReplicas > 0 {
		maxReplicas = req.MaxReplicas
	}

	maxConcurrency := defaultMaxConcurrency
	if req.MaxConcurrency > 0 {
		maxConcurrency = req.MaxConcurrency
	}

	fmt.Printf("-> [Deploy] Função '%s' | Imagem: '%s' | IdleTimeout: %v | MaxReplicas: %d | MaxConcurrency: %d\n",
		req.FunctionName, req.ImageName, idleTimeout, maxReplicas, maxConcurrency)

	ctx := context.Background()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		http.Error(w, "Erro ao conectar no Docker: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer cli.Close()

	// 1. Verifica se a imagem já existe localmente; se não existir, faz o pull do registro remoto.
	_, _, err = cli.ImageInspectWithRaw(ctx, req.ImageName)
	if err != nil {
		fmt.Printf("-> [Docker] Imagem '%s' não encontrada localmente. Baixando...\n", req.ImageName)
		reader, pullErr := cli.ImagePull(ctx, req.ImageName, image.PullOptions{})
		if pullErr != nil {
			http.Error(w, "Erro ao baixar imagem: "+pullErr.Error(), http.StatusInternalServerError)
			return
		}
		defer reader.Close()
		io.Copy(os.Stdout, reader)
	} else {
		fmt.Printf("-> [Docker] Imagem '%s' encontrada localmente, pulando download.\n", req.ImageName)
	}

	// 2. Cria e inicia a réplica primária (#1) aguardando seu /health.
	firstReplicaName := fmt.Sprintf("%s-rep-1", req.FunctionName)
	fmt.Printf("-> [Docker] Criando réplica inicial '%s'...\n", firstReplicaName)

	firstReplica, err := a.createAndStartReplica(ctx, cli, req.FunctionName, req.ImageName, firstReplicaName)
	if err != nil {
		http.Error(w, "Erro ao criar réplica inicial: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 3. Registra o serviço da função e seu pool de réplicas em memória.
	instance := &FunctionInstance{
		FunctionName:       req.FunctionName,
		ImageName:          req.ImageName,
		IdleTimeoutSeconds: int(idleTimeout.Seconds()),
		IdleTimeout:        idleTimeout,
		MaxReplicas:        maxReplicas,
		MaxConcurrency:     maxConcurrency,
		Replicas:           []*FunctionReplica{firstReplica},
		nextReplicaSeq:     1,
	}

	a.registryMu.Lock()
	a.registry[req.FunctionName] = instance
	snapshot := buildFunctionSnapshot(instance)
	a.registryMu.Unlock()

	fmt.Printf("-> [Sucesso] Função '%s' implantada! Réplica '%s' rodando em 127.0.0.1:%s (Container ID: %s)\n",
		snapshot.FunctionName, firstReplica.ReplicaName, firstReplica.HostPort, firstReplica.ContainerID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(snapshot)
}

func (a *Agent) listFunctionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Apenas o método GET é permitido", http.StatusMethodNotAllowed)
		return
	}

	a.registryMu.RLock()
	functions := make([]*FunctionInstance, 0, len(a.registry))
	for _, fn := range a.registry {
		functions = append(functions, buildFunctionSnapshot(fn))
	}
	a.registryMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(functions)
}

func (a *Agent) getFunctionHandler(w http.ResponseWriter, r *http.Request) {
	functionName := r.PathValue("functionName")
	if functionName == "" {
		http.Error(w, "Nome da função é obrigatório na URL (/function/{functionName})", http.StatusBadRequest)
		return
	}

	a.registryMu.RLock()
	instance, exists := a.registry[functionName]
	var snapshot *FunctionInstance
	if exists {
		snapshot = buildFunctionSnapshot(instance)
	}
	a.registryMu.RUnlock()

	if !exists {
		http.Error(w, fmt.Sprintf("Função '%s' não encontrada", functionName), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snapshot)
}

func (a *Agent) functionByNameHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.getFunctionHandler(w, r)
	case http.MethodDelete:
		a.deleteFunctionHandler(w, r)
	default:
		http.Error(w, "Apenas os métodos GET e DELETE são permitidos", http.StatusMethodNotAllowed)
	}
}

func (a *Agent) invokeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Apenas o método POST é permitido", http.StatusMethodNotAllowed)
		return
	}

	functionName := r.PathValue("functionName")
	if functionName == "" {
		http.Error(w, "Nome da função é obrigatório na URL (/invoke/{functionName})", http.StatusBadRequest)
		return
	}

	a.registryMu.RLock()
	instance, exists := a.registry[functionName]
	a.registryMu.RUnlock()

	if !exists {
		http.Error(w, fmt.Sprintf("Função '%s' não encontrada no agente", functionName), http.StatusNotFound)
		return
	}

	startTime := time.Now()

	// 1. Seleciona a réplica menos ocupada (Least Connections), acorda uma parada (Cold Start)
	// ou cria uma réplica nova (Scale-Up) se todas estiverem saturadas.
	replica, coldStart, err := a.selectOrScaleReplica(r.Context(), instance)
	if err != nil {
		durationMs := time.Since(startTime).Milliseconds()
		a.logStore.Record(functionName, "", http.StatusBadGateway, durationMs, coldStart, err.Error())
		http.Error(w, fmt.Sprintf("Falha ao obter réplica para '%s': %v", functionName, err), http.StatusBadGateway)
		return
	}

	// Garante que o contador de requisições ativas desta réplica específica seja decrementado ao terminar.
	defer func() {
		a.registryMu.Lock()
		replica.ActiveRequests--
		replica.LastUsedAt = time.Now()
		a.registryMu.Unlock()
	}()

	a.registryMu.RLock()
	hostPort := replica.HostPort
	replicaName := replica.ReplicaName
	a.registryMu.RUnlock()

	// 2. Monta a URL interna da réplica selecionada.
	targetURL := fmt.Sprintf("http://127.0.0.1:%s/", hostPort)
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, r.Body)
	if err != nil {
		durationMs := time.Since(startTime).Milliseconds()
		a.logStore.Record(functionName, replicaName, http.StatusInternalServerError, durationMs, coldStart, err.Error())
		http.Error(w, "Erro ao criar requisição para o container: "+err.Error(), http.StatusInternalServerError)
		return
	}

	for key, values := range r.Header {
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}

	// 3. Dispara a requisição usando o pool de conexões HTTP (a.proxyClient).
	resp, err := a.proxyClient.Do(proxyReq)
	durationMs := time.Since(startTime).Milliseconds()

	if err != nil {
		a.logStore.Record(functionName, replicaName, http.StatusBadGateway, durationMs, coldStart, err.Error())
		http.Error(w, fmt.Sprintf("Erro ao comunicar com a réplica '%s': %v", replicaName, err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	a.logStore.Record(functionName, replicaName, resp.StatusCode, durationMs, coldStart, "")

	mode := "Warm Start"
	if coldStart {
		mode = "Cold Start / Scale-Up"
	}

	fmt.Printf("-> [Invoke] Função '%s' [%s] (%s) | Status: %d | Duração: %dms\n",
		functionName, replicaName, mode, resp.StatusCode, durationMs)

	// 4. Repassa cabeçalhos da resposta do container + metadados de telemetria.
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Execution-Duration-Ms", fmt.Sprintf("%d", durationMs))
	w.Header().Set("X-Cold-Start", fmt.Sprintf("%t", coldStart))
	w.Header().Set("X-Replica-Name", replicaName)

	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (a *Agent) deleteFunctionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Apenas o método DELETE é permitido", http.StatusMethodNotAllowed)
		return
	}

	functionName := r.PathValue("functionName")
	if functionName == "" {
		http.Error(w, "Nome da função é obrigatório na URL (/function/{functionName})", http.StatusBadRequest)
		return
	}

	a.registryMu.RLock()
	instance, exists := a.registry[functionName]
	a.registryMu.RUnlock()

	if !exists {
		http.Error(w, fmt.Sprintf("Função '%s' não encontrada", functionName), http.StatusNotFound)
		return
	}

	instance.mu.Lock()
	defer instance.mu.Unlock()

	a.registryMu.Lock()
	for _, rep := range instance.Replicas {
		if rep.ActiveRequests > 0 {
			a.registryMu.Unlock()
			http.Error(w, "Não é possível remover a função enquanto há requisições em andamento", http.StatusConflict)
			return
		}
	}
	replicasToRemove := make([]*FunctionReplica, len(instance.Replicas))
	copy(replicasToRemove, instance.Replicas)
	delete(a.registry, functionName)
	a.registryMu.Unlock()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		http.Error(w, "Erro ao conectar no Docker: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer cli.Close()

	for _, rep := range replicasToRemove {
		if err := cli.ContainerRemove(context.Background(), rep.ContainerID, container.RemoveOptions{Force: true}); err != nil {
			log.Printf("-> [Delete] Aviso ao remover container %s (%s): %v\n", rep.ReplicaName, rep.ContainerID, err)
		} else {
			fmt.Printf("-> [Delete] Réplica '%s' (Container ID: %s) removida.\n", rep.ReplicaName, rep.ContainerID)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"message":         fmt.Sprintf("Função '%s' e suas %d réplica(s) removidas com sucesso", functionName, len(replicasToRemove)),
		"functionName":    functionName,
		"removedReplicas": len(replicasToRemove),
	})
}
