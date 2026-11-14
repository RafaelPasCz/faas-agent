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
	reaperInterval        = 5 * time.Second
	coldStartTimeout      = 15 * time.Second
)

type DeployRequest struct {
	FunctionName       string `json:"functionName"`
	ImageName          string `json:"imageName"`
	IdleTimeoutSeconds int    `json:"idleTimeoutSeconds,omitempty"`
}

// FunctionInstance representa o estado de uma função gerenciada pelo faas-agent no host.
type FunctionInstance struct {
	FunctionName       string        `json:"functionName"`
	ImageName          string        `json:"imageName"`
	ContainerID        string        `json:"containerId"`
	HostPort           string        `json:"hostPort"`
	Status             string        `json:"status"` // "RUNNING" ou "STOPPED"
	LastUsedAt         time.Time     `json:"lastUsedAt"`
	ActiveRequests     int           `json:"activeRequests"`
	IdleTimeoutSeconds int           `json:"idleTimeoutSeconds"`
	IdleTimeout        time.Duration `json:"-"`
	mu                 sync.Mutex    // Evita que duas requisições façam Cold Start do mesmo container ao mesmo tempo
}

var (
	registry   = make(map[string]*FunctionInstance)
	registryMu sync.RWMutex
)

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

	fmt.Printf("-> [Deploy] Solicitado deploy da função '%s' com a imagem '%s' (IdleTimeout: %v)\n",
		req.FunctionName, req.ImageName, idleTimeout)

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

	// 2. Configura a exposição da porta 8080/tcp do contrato e o Port Binding dinâmico no host (HostPort: "").
	exposedPort := nat.Port(containerFunctionPort)

	containerConfig := &container.Config{
		Image: req.ImageName,
		Env: []string{
			"FUNCTION_NAME=" + req.FunctionName,
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
					HostPort: "", // Porta vazia instrui o Docker a escolher qualquer porta livre no host
				},
			},
		},
	}

	fmt.Println("-> [Docker] Criando container com Port Binding dinâmico...")
	resp, err := cli.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, req.FunctionName)
	if err != nil {
		http.Error(w, "Erro ao criar container: "+err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Println("-> [Docker] Iniciando container...")
	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		http.Error(w, "Erro ao iniciar container: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 3. Inspeciona o container em execução para descobrir qual porta do host o Docker alocou.
	inspect, err := cli.ContainerInspect(ctx, resp.ID)
	if err != nil {
		http.Error(w, "Erro ao inspecionar container: "+err.Error(), http.StatusInternalServerError)
		return
	}

	bindings := inspect.NetworkSettings.Ports[exposedPort]
	if len(bindings) == 0 || bindings[0].HostPort == "" {
		http.Error(w, "Nenhuma porta foi mapeada no host para o container", http.StatusInternalServerError)
		return
	}
	hostPort := bindings[0].HostPort

	// 4. Salva o estado da função no registro em memória.
	instance := &FunctionInstance{
		FunctionName:       req.FunctionName,
		ImageName:          req.ImageName,
		ContainerID:        resp.ID[:12],
		HostPort:           hostPort,
		Status:             "RUNNING",
		LastUsedAt:         time.Now(),
		ActiveRequests:     0,
		IdleTimeoutSeconds: int(idleTimeout.Seconds()),
		IdleTimeout:        idleTimeout,
	}

	registryMu.Lock()
	registry[req.FunctionName] = instance
	registryMu.Unlock()

	fmt.Printf("-> [Sucesso] Função '%s' rodando em 127.0.0.1:%s (Container ID: %s)\n",
		instance.FunctionName, instance.HostPort, instance.ContainerID)

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

// ensureFunctionRunning garante que o container da função esteja rodando.
// Se estiver "STOPPED", realiza o Cold Start (ContainerStart + atualização da porta + espera pelo /health).
func ensureFunctionRunning(ctx context.Context, instance *FunctionInstance) (bool, error) {
	instance.mu.Lock()
	defer instance.mu.Unlock()

	registryMu.RLock()
	status := instance.Status
	registryMu.RUnlock()

	if status == "RUNNING" {
		return false, nil
	}

	fmt.Printf("-> [Cold Start] Acordando container da função '%s' (%s)...\n",
		instance.FunctionName, instance.ContainerID)

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return false, fmt.Errorf("erro ao conectar no Docker: %w", err)
	}
	defer cli.Close()

	if err := cli.ContainerStart(ctx, instance.ContainerID, container.StartOptions{}); err != nil {
		return false, fmt.Errorf("erro ao iniciar container: %w", err)
	}

	// Ao religar um container com porta dinâmica, o Docker pode alocar uma nova porta livre no host.
	inspect, err := cli.ContainerInspect(ctx, instance.ContainerID)
	if err != nil {
		return false, fmt.Errorf("erro ao inspecionar container após start: %w", err)
	}

	exposedPort := nat.Port(containerFunctionPort)
	bindings := inspect.NetworkSettings.Ports[exposedPort]
	if len(bindings) == 0 || bindings[0].HostPort == "" {
		return false, fmt.Errorf("nenhuma porta mapeada no host após reiniciar container")
	}
	newHostPort := bindings[0].HostPort

	// Faz polling no endpoint /health do container até o servidor Python estar pronto para aceitar requisições.
	healthURL := fmt.Sprintf("http://127.0.0.1:%s/health", newHostPort)
	httpClient := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(coldStartTimeout)

	ready := false
	for time.Now().Before(deadline) {
		resp, err := httpClient.Get(healthURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !ready {
		return false, fmt.Errorf("timeout aguardando /health da função '%s' na porta %s", instance.FunctionName, newHostPort)
	}

	registryMu.Lock()
	instance.HostPort = newHostPort
	instance.Status = "RUNNING"
	instance.LastUsedAt = time.Now()
	registryMu.Unlock()

	fmt.Printf("-> [Cold Start] Função '%s' pronta em 127.0.0.1:%s!\n", instance.FunctionName, newHostPort)
	return true, nil
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

	// 1. Busca a função e incrementa ActiveRequests para impedir que o Reaper pare o container durante a chamada.
	registryMu.Lock()
	instance, exists := registry[functionName]
	if exists {
		instance.ActiveRequests++
		instance.LastUsedAt = time.Now()
	}
	registryMu.Unlock()

	if !exists {
		http.Error(w, fmt.Sprintf("Função '%s' não encontrada no agente", functionName), http.StatusNotFound)
		return
	}

	// Garante que ao final da requisição o contador de chamadas ativas seja decrementado e LastUsedAt atualizado.
	defer func() {
		registryMu.Lock()
		instance.ActiveRequests--
		instance.LastUsedAt = time.Now()
		registryMu.Unlock()
	}()

	startTime := time.Now()

	// 2. Se o container estiver parado (STOPPED), realiza o Cold Start transparente antes do proxy.
	coldStart, err := ensureFunctionRunning(r.Context(), instance)
	if err != nil {
		http.Error(w, fmt.Sprintf("Falha no Cold Start da função '%s': %v", functionName, err), http.StatusBadGateway)
		return
	}

	registryMu.RLock()
	hostPort := instance.HostPort
	registryMu.RUnlock()

	// 3. Monta a URL interna do container na porta mapeada no host.
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

	// 4. Dispara a requisição para o container.
	resp, err := http.DefaultClient.Do(proxyReq)
	durationMs := time.Since(startTime).Milliseconds()

	if err != nil {
		http.Error(w, fmt.Sprintf("Erro ao comunicar com o container da função '%s': %v", functionName, err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	mode := "Warm Start"
	if coldStart {
		mode = "Cold Start"
	}

	fmt.Printf("-> [Invoke] Função '%s' executada (%s) | Status: %d | Duração: %dms\n",
		functionName, mode, resp.StatusCode, durationMs)

	// 5. Repassa cabeçalhos da resposta do container + metadados de telemetria.
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Execution-Duration-Ms", fmt.Sprintf("%d", durationMs))
	w.Header().Set("X-Cold-Start", fmt.Sprintf("%t", coldStart))

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

	registryMu.Lock()
	instance, exists := registry[functionName]
	if !exists {
		registryMu.Unlock()
		http.Error(w, fmt.Sprintf("Função '%s' não encontrada", functionName), http.StatusNotFound)
		return
	}
	if instance.ActiveRequests > 0 {
		registryMu.Unlock()
		http.Error(w, "Não é possível remover a função enquanto há requisições em andamento", http.StatusConflict)
		return
	}
	delete(registry, functionName)
	registryMu.Unlock()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		http.Error(w, "Erro ao conectar no Docker: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer cli.Close()

	if err := cli.ContainerRemove(context.Background(), instance.ContainerID, container.RemoveOptions{Force: true}); err != nil {
		http.Error(w, "Erro ao remover container: "+err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Printf("-> [Delete] Função '%s' (Container ID: %s) removida com sucesso!\n",
		functionName, instance.ContainerID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message":      fmt.Sprintf("Função '%s' removida com sucesso", functionName),
		"functionName": functionName,
	})
}

// startIdleReaper monitora periodicamente as funções em memória e para os containers ociosos (Scale-to-Zero).
func startIdleReaper() {
	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()

	for range ticker.C {
		// Coleta candidatas ociosas primeiro para não segurar o lock global durante chamadas ao Docker.
		var idleCandidates []*FunctionInstance

		registryMu.RLock()
		for _, fn := range registry {
			if fn.Status == "RUNNING" && fn.ActiveRequests == 0 && time.Since(fn.LastUsedAt) > fn.IdleTimeout {
				idleCandidates = append(idleCandidates, fn)
			}
		}
		registryMu.RUnlock()

		for _, fn := range idleCandidates {
			fn.mu.Lock()

			// Revalida o estado após adquirir o lock individual da função.
			registryMu.RLock()
			stillIdle := fn.Status == "RUNNING" && fn.ActiveRequests == 0 && time.Since(fn.LastUsedAt) > fn.IdleTimeout
			registryMu.RUnlock()

			if !stillIdle {
				fn.mu.Unlock()
				continue
			}

			cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
			if err != nil {
				log.Printf("-> [Reaper] Erro ao conectar no Docker: %v\n", err)
				fn.mu.Unlock()
				continue
			}

			fmt.Printf("-> [Reaper] Função '%s' ociosa há >%v. Parando container %s (Scale-to-Zero)...\n",
				fn.FunctionName, fn.IdleTimeout, fn.ContainerID)

			if err := cli.ContainerStop(context.Background(), fn.ContainerID, container.StopOptions{}); err != nil {
				log.Printf("-> [Reaper] Erro ao parar container %s: %v\n", fn.ContainerID, err)
			} else {
				registryMu.Lock()
				fn.Status = "STOPPED"
				fn.HostPort = ""
				registryMu.Unlock()
				fmt.Printf("-> [Reaper] Container da função '%s' parado com sucesso!\n", fn.FunctionName)
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

	fmt.Println("Faas-Agent rodando na porta 8081 (Idle Reaper ativo)...")
	if err := http.ListenAndServe(":8081", nil); err != nil {
		log.Fatal("Erro ao iniciar o servidor: ", err)
	}
}
