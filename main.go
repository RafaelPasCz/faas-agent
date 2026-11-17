package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

const (
	containerFunctionPort = "8080/tcp"
	defaultIdleTimeout    = 60 * time.Second
	defaultMaxReplicas    = 3
	defaultMaxConcurrency = 4
	reaperInterval        = 5 * time.Second
	coldStartTimeout      = 15 * time.Second
)

type DeployRequest struct {
	FunctionName       string `json:"functionName"`
	ImageName          string `json:"imageName"`
	IdleTimeoutSeconds int    `json:"idleTimeoutSeconds,omitempty"`
	MaxReplicas        int    `json:"maxReplicas,omitempty"`
	MaxConcurrency     int    `json:"maxConcurrency,omitempty"`
}

// FunctionReplica representa um container individual (réplica) de uma função no host.
type FunctionReplica struct {
	ReplicaName    string    `json:"replicaName"`
	ContainerID    string    `json:"containerId"`
	HostPort       string    `json:"hostPort"`
	Status         string    `json:"status"` // "RUNNING" ou "STOPPED"
	ActiveRequests int       `json:"activeRequests"`
	LastUsedAt     time.Time `json:"lastUsedAt"`
}

// FunctionInstance representa o serviço de uma função gerenciada pelo faas-agent e seu pool de réplicas (1:N).
type FunctionInstance struct {
	FunctionName       string             `json:"functionName"`
	ImageName          string             `json:"imageName"`
	IdleTimeoutSeconds int                `json:"idleTimeoutSeconds"`
	IdleTimeout        time.Duration      `json:"-"`
	MaxReplicas        int                `json:"maxReplicas"`
	MaxConcurrency     int                `json:"maxConcurrency"`
	Replicas           []*FunctionReplica `json:"replicas"`
	nextReplicaSeq     int                // Contador interno para gerar nomes únicos de réplica
	mu                 sync.Mutex         // Serializa operações no Docker (Cold Start, Scale-Up e Scale-Down)
}

var (
	registry   = make(map[string]*FunctionInstance)
	registryMu sync.RWMutex

	sharedTransport = &http.Transport{
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}

	proxyClient = &http.Client{
		Timeout:   60 * time.Second,
		Transport: sharedTransport,
	}

	healthClient = &http.Client{
		Timeout:   500 * time.Millisecond,
		Transport: sharedTransport,
	}
)

func waitForContainerHealth(functionName, hostPort string) error {
	healthURL := fmt.Sprintf("http://127.0.0.1:%s/health", hostPort)
	deadline := time.Now().Add(coldStartTimeout)

	for time.Now().Before(deadline) {
		resp, err := healthClient.Get(healthURL)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	return fmt.Errorf("timeout aguardando /health da função '%s' na porta %s", functionName, hostPort)
}

func createAndStartReplica(ctx context.Context, cli *client.Client, functionName, imageName, replicaName string) (*FunctionReplica, error) {
	exposedPort := nat.Port(containerFunctionPort)

	containerConfig := &container.Config{
		Image: imageName,
		Env: []string{
			"FUNCTION_NAME=" + functionName,
		},
		Labels: map[string]string{
			"faas.function": functionName,
			"faas.replica":  replicaName,
		},
		ExposedPorts: nat.PortSet{
			exposedPort: struct{}{},
		},
	}

	hostConfig := &container.HostConfig{
		PortBindings: nat.PortMap{
			exposedPort: []nat.PortBinding{
				{
					HostIP:   "127.0.0.1",
					HostPort: "",
				},
			},
		},
	}

	resp, err := cli.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, replicaName)
	if err != nil {
		return nil, fmt.Errorf("erro ao criar container '%s': %w", replicaName, err)
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("erro ao iniciar container '%s': %w", replicaName, err)
	}

	inspect, err := cli.ContainerInspect(ctx, resp.ID)
	if err != nil {
		return nil, fmt.Errorf("erro ao inspecionar container '%s': %w", replicaName, err)
	}

	bindings := inspect.NetworkSettings.Ports[exposedPort]
	if len(bindings) == 0 || bindings[0].HostPort == "" {
		return nil, fmt.Errorf("nenhuma porta foi mapeada no host para a réplica '%s'", replicaName)
	}
	hostPort := bindings[0].HostPort

	if err := waitForContainerHealth(functionName, hostPort); err != nil {
		return nil, err
	}

	return &FunctionReplica{
		ReplicaName:    replicaName,
		ContainerID:    resp.ID[:12],
		HostPort:       hostPort,
		Status:         "RUNNING",
		ActiveRequests: 0,
		LastUsedAt:     time.Now(),
	}, nil
}

func wakeReplica(ctx context.Context, functionName string, rep *FunctionReplica) (string, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return "", fmt.Errorf("erro ao conectar no Docker: %w", err)
	}
	defer cli.Close()

	if err := cli.ContainerStart(ctx, rep.ContainerID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("erro ao iniciar container %s: %w", rep.ContainerID, err)
	}

	inspect, err := cli.ContainerInspect(ctx, rep.ContainerID)
	if err != nil {
		return "", fmt.Errorf("erro ao inspecionar container após start: %w", err)
	}

	exposedPort := nat.Port(containerFunctionPort)
	bindings := inspect.NetworkSettings.Ports[exposedPort]
	if len(bindings) == 0 || bindings[0].HostPort == "" {
		return "", fmt.Errorf("nenhuma porta mapeada no host após reiniciar réplica '%s'", rep.ReplicaName)
	}
	newHostPort := bindings[0].HostPort

	if err := waitForContainerHealth(functionName, newHostPort); err != nil {
		return "", err
	}

	return newHostPort, nil
}

// Deve ser chamada com registryMu (Lock ou RLock) já adquirido.
func findLeastLoadedRunningReplica(fn *FunctionInstance) *FunctionReplica {
	var best *FunctionReplica
	for _, rep := range fn.Replicas {
		if rep.Status == "RUNNING" {
			if best == nil || rep.ActiveRequests < best.ActiveRequests {
				best = rep
			}
		}
	}
	return best
}

// selectOrScaleReplica escolhe a melhor réplica para atender a requisição.
// Se necessário, acorda uma réplica parada (Cold Start) ou cria uma nova réplica (Scale-Up).
func selectOrScaleReplica(ctx context.Context, fn *FunctionInstance) (*FunctionReplica, bool, error) {
	// 1. Caminho Rápido: verifica se já existe uma réplica RUNNING com folga de concorrência,
	// ou se o limite máximo de réplicas já foi atingido e todas estão rodando.
	registryMu.Lock()
	best := findLeastLoadedRunningReplica(fn)
	allRunningAtMaxCap := len(fn.Replicas) >= fn.MaxReplicas && best != nil
	hasStoppedReplica := false
	for _, rep := range fn.Replicas {
		if rep.Status == "STOPPED" {
			hasStoppedReplica = true
			break
		}
	}

	if best != nil && (best.ActiveRequests < fn.MaxConcurrency || (allRunningAtMaxCap && !hasStoppedReplica)) {
		best.ActiveRequests++
		best.LastUsedAt = time.Now()
		registryMu.Unlock()
		return best, false, nil
	}
	registryMu.Unlock()

	// 2. Caminho Lento: precisamos fazer Cold Start de réplica parada ou Scale-Up de nova réplica.
	fn.mu.Lock()
	defer fn.mu.Unlock()

	// Revalida o estado após adquirir fn.mu (outra goroutine pode ter acabado de subir uma réplica).
	registryMu.Lock()
	best = findLeastLoadedRunningReplica(fn)
	if best != nil && best.ActiveRequests < fn.MaxConcurrency {
		best.ActiveRequests++
		best.LastUsedAt = time.Now()
		registryMu.Unlock()
		return best, false, nil
	}

	// 2a. Verifica se há alguma réplica STOPPED que possamos acordar antes de criar um container novo.
	var stoppedRep *FunctionReplica
	for _, rep := range fn.Replicas {
		if rep.Status == "STOPPED" {
			stoppedRep = rep
			break
		}
	}
	if stoppedRep != nil {
		registryMu.Unlock()

		fmt.Printf("-> [Cold Start] Acordando réplica '%s' (%s)...\n", stoppedRep.ReplicaName, stoppedRep.ContainerID)
		newHostPort, err := wakeReplica(ctx, fn.FunctionName, stoppedRep)
		if err != nil {
			return nil, false, err
		}

		registryMu.Lock()
		stoppedRep.HostPort = newHostPort
		stoppedRep.Status = "RUNNING"
		stoppedRep.ActiveRequests++
		stoppedRep.LastUsedAt = time.Now()
		registryMu.Unlock()

		fmt.Printf("-> [Cold Start] Réplica '%s' pronta em 127.0.0.1:%s!\n", stoppedRep.ReplicaName, newHostPort)
		return stoppedRep, true, nil
	}

	// 2b. Se todas as réplicas atuais estão ocupadas (>= MaxConcurrency) e ainda podemos escalar (< MaxReplicas),
	// criamos uma nova réplica horizontalmente (Scale-Up).
	if len(fn.Replicas) < fn.MaxReplicas {
		fn.nextReplicaSeq++
		replicaName := fmt.Sprintf("%s-rep-%d", fn.FunctionName, fn.nextReplicaSeq)
		currentCount := len(fn.Replicas) + 1
		maxReplicas := fn.MaxReplicas
		registryMu.Unlock()

		fmt.Printf("-> [Auto-Scale] Alta demanda em '%s'! Criando nova réplica '%s' (%d/%d)...\n",
			fn.FunctionName, replicaName, currentCount, maxReplicas)

		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, false, fmt.Errorf("erro ao conectar no Docker para Scale-Up: %w", err)
		}
		defer cli.Close()

		newRep, err := createAndStartReplica(ctx, cli, fn.FunctionName, fn.ImageName, replicaName)
		if err != nil {
			return nil, false, err
		}

		registryMu.Lock()
		newRep.ActiveRequests = 1
		newRep.LastUsedAt = time.Now()
		fn.Replicas = append(fn.Replicas, newRep)
		registryMu.Unlock()

		fmt.Printf("-> [Auto-Scale] Réplica '%s' pronta em 127.0.0.1:%s (Container ID: %s)!\n",
			newRep.ReplicaName, newRep.HostPort, newRep.ContainerID)
		return newRep, true, nil
	}

	// 2c. Já atingimos MaxReplicas e todas estão RUNNING: usa Least Connections na réplica menos sobrecarregada.
	best = findLeastLoadedRunningReplica(fn)
	if best == nil {
		registryMu.Unlock()
		return nil, false, fmt.Errorf("nenhuma réplica disponível para a função '%s'", fn.FunctionName)
	}
	best.ActiveRequests++
	best.LastUsedAt = time.Now()
	registryMu.Unlock()
	return best, false, nil
}

func deployHandler(w http.ResponseWriter, r *http.Request) {
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

	firstReplica, err := createAndStartReplica(ctx, cli, req.FunctionName, req.ImageName, firstReplicaName)
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

	registryMu.Lock()
	registry[req.FunctionName] = instance
	registryMu.Unlock()

	fmt.Printf("-> [Sucesso] Função '%s' implantada! Réplica '%s' rodando em 127.0.0.1:%s (Container ID: %s)\n",
		instance.FunctionName, firstReplica.ReplicaName, firstReplica.HostPort, firstReplica.ContainerID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(instance)
}

func listFunctionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Apenas o método GET é permitido", http.StatusMethodNotAllowed)
		return
	}

	registryMu.RLock()
	defer registryMu.RUnlock()

	functions := make([]*FunctionInstance, 0, len(registry))
	for _, fn := range registry {
		functions = append(functions, fn)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(functions)
}

func invokeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Apenas o método POST é permitido", http.StatusMethodNotAllowed)
		return
	}

	functionName := r.PathValue("functionName")
	if functionName == "" {
		http.Error(w, "Nome da função é obrigatório na URL (/invoke/{functionName})", http.StatusBadRequest)
		return
	}

	registryMu.RLock()
	instance, exists := registry[functionName]
	registryMu.RUnlock()

	if !exists {
		http.Error(w, fmt.Sprintf("Função '%s' não encontrada no agente", functionName), http.StatusNotFound)
		return
	}

	startTime := time.Now()

	// 1. Seleciona a réplica menos ocupada (Least Connections), acorda uma parada (Cold Start)
	// ou cria uma réplica nova (Scale-Up) se todas estiverem saturadas.
	replica, coldStart, err := selectOrScaleReplica(r.Context(), instance)
	if err != nil {
		http.Error(w, fmt.Sprintf("Falha ao obter réplica para '%s': %v", functionName, err), http.StatusBadGateway)
		return
	}

	// Garante que o contador de requisições ativas desta réplica específica seja decrementado ao terminar.
	defer func() {
		registryMu.Lock()
		replica.ActiveRequests--
		replica.LastUsedAt = time.Now()
		registryMu.Unlock()
	}()

	registryMu.RLock()
	hostPort := replica.HostPort
	replicaName := replica.ReplicaName
	registryMu.RUnlock()

	// 2. Monta a URL interna da réplica selecionada.
	targetURL := fmt.Sprintf("http://127.0.0.1:%s/", hostPort)
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, r.Body)
	if err != nil {
		http.Error(w, "Erro ao criar requisição para o container: "+err.Error(), http.StatusInternalServerError)
		return
	}

	for key, values := range r.Header {
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}

	// 3. Dispara a requisição usando o pool de conexões HTTP (proxyClient).
	resp, err := proxyClient.Do(proxyReq)
	durationMs := time.Since(startTime).Milliseconds()

	if err != nil {
		http.Error(w, fmt.Sprintf("Erro ao comunicar com a réplica '%s': %v", replicaName, err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

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

func deleteFunctionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Apenas o método DELETE é permitido", http.StatusMethodNotAllowed)
		return
	}

	functionName := r.PathValue("functionName")
	if functionName == "" {
		http.Error(w, "Nome da função é obrigatório na URL (/function/{functionName})", http.StatusBadRequest)
		return
	}

	registryMu.RLock()
	instance, exists := registry[functionName]
	registryMu.RUnlock()

	if !exists {
		http.Error(w, fmt.Sprintf("Função '%s' não encontrada", functionName), http.StatusNotFound)
		return
	}

	instance.mu.Lock()
	defer instance.mu.Unlock()

	registryMu.Lock()
	for _, rep := range instance.Replicas {
		if rep.ActiveRequests > 0 {
			registryMu.Unlock()
			http.Error(w, "Não é possível remover a função enquanto há requisições em andamento", http.StatusConflict)
			return
		}
	}
	replicasToRemove := make([]*FunctionReplica, len(instance.Replicas))
	copy(replicasToRemove, instance.Replicas)
	delete(registry, functionName)
	registryMu.Unlock()

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

// startIdleReaper monitora periodicamente as réplicas de cada função:
// - Réplicas extras (índice > 0) ociosas são removidas (Scale-Down com ContainerRemove).
// - A réplica primária (índice == 0) ociosa é apenas parada (Scale-to-Zero com ContainerStop).
func startIdleReaper() {
	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()

	for range ticker.C {
		var functions []*FunctionInstance
		registryMu.RLock()
		for _, fn := range registry {
			functions = append(functions, fn)
		}
		registryMu.RUnlock()

		for _, fn := range functions {
			fn.mu.Lock()

			registryMu.RLock()
			if len(fn.Replicas) == 0 {
				registryMu.RUnlock()
				fn.mu.Unlock()
				continue
			}

			var extraIdle []*FunctionReplica
			var stopPrimary *FunctionReplica

			for i, rep := range fn.Replicas {
				isIdle := rep.Status == "RUNNING" && rep.ActiveRequests == 0 && time.Since(rep.LastUsedAt) > fn.IdleTimeout
				if !isIdle {
					continue
				}
				if i == 0 {
					stopPrimary = rep
				} else {
					extraIdle = append(extraIdle, rep)
				}
			}
			registryMu.RUnlock()

			if stopPrimary == nil && len(extraIdle) == 0 {
				fn.mu.Unlock()
				continue
			}

			cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
			if err != nil {
				log.Printf("-> [Reaper] Erro ao conectar no Docker: %v\n", err)
				fn.mu.Unlock()
				continue
			}

			// 1. Scale-Down: remove containers de réplicas extras ociosas.
			if len(extraIdle) > 0 {
				removedIDs := make(map[string]bool)
				for _, rep := range extraIdle {
					fmt.Printf("-> [Reaper / Scale-Down] Réplica extra '%s' ociosa há >%v. Removendo container %s...\n",
						rep.ReplicaName, fn.IdleTimeout, rep.ContainerID)
					if err := cli.ContainerRemove(context.Background(), rep.ContainerID, container.RemoveOptions{Force: true}); err != nil {
						log.Printf("-> [Reaper] Erro ao remover réplica extra %s: %v\n", rep.ContainerID, err)
					} else {
						removedIDs[rep.ContainerID] = true
						fmt.Printf("-> [Reaper / Scale-Down] Réplica extra '%s' removida com sucesso!\n", rep.ReplicaName)
					}
				}

				if len(removedIDs) > 0 {
					registryMu.Lock()
					kept := make([]*FunctionReplica, 0, len(fn.Replicas))
					for _, rep := range fn.Replicas {
						if !removedIDs[rep.ContainerID] {
							kept = append(kept, rep)
						}
					}
					fn.Replicas = kept
					registryMu.Unlock()
				}
			}

			// 2. Scale-to-Zero: para o container da réplica primária (#1) se também estiver ociosa.
			if stopPrimary != nil {
				fmt.Printf("-> [Reaper / Scale-to-Zero] Réplica primária '%s' ociosa há >%v. Parando container %s...\n",
					stopPrimary.ReplicaName, fn.IdleTimeout, stopPrimary.ContainerID)
				if err := cli.ContainerStop(context.Background(), stopPrimary.ContainerID, container.StopOptions{}); err != nil {
					log.Printf("-> [Reaper] Erro ao parar réplica primária %s: %v\n", stopPrimary.ContainerID, err)
				} else {
					registryMu.Lock()
					stopPrimary.Status = "STOPPED"
					stopPrimary.HostPort = ""
					registryMu.Unlock()
					fmt.Printf("-> [Reaper / Scale-to-Zero] Réplica primária '%s' parada com sucesso!\n", stopPrimary.ReplicaName)
				}
			}

			cli.Close()
			fn.mu.Unlock()
		}
	}
}

func main() {
	go startIdleReaper()

	http.HandleFunc("/deploy", deployHandler)
	http.HandleFunc("/functions", listFunctionsHandler)
	http.HandleFunc("/invoke/{functionName}", invokeHandler)
	http.HandleFunc("/function/{functionName}", deleteFunctionHandler)

	fmt.Println("Faas-Agent rodando na porta 8081 (Auto-Scaler + Idle Reaper ativos)...")
	if err := http.ListenAndServe(":8081", nil); err != nil {
		log.Fatal("Erro ao iniciar o servidor: ", err)
	}
}
