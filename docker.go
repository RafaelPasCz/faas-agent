package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

func (a *Agent) waitForContainerHealth(functionName, hostPort string) error {
	healthURL := fmt.Sprintf("http://127.0.0.1:%s/health", hostPort)
	deadline := time.Now().Add(coldStartTimeout)

	for time.Now().Before(deadline) {
		resp, err := a.healthClient.Get(healthURL)
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

func (a *Agent) createAndStartReplica(ctx context.Context, cli *client.Client, functionName, imageName, replicaName string) (*FunctionReplica, error) {
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

	if err := a.waitForContainerHealth(functionName, hostPort); err != nil {
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

func (a *Agent) wakeReplica(ctx context.Context, functionName string, rep *FunctionReplica) (string, error) {
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

	if err := a.waitForContainerHealth(functionName, newHostPort); err != nil {
		return "", err
	}

	return newHostPort, nil
}

// Deve ser chamada com a.registryMu (Lock ou RLock) já adquirido.
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
func (a *Agent) selectOrScaleReplica(ctx context.Context, fn *FunctionInstance) (*FunctionReplica, bool, error) {
	// 1. Caminho Rápido: verifica se já existe uma réplica RUNNING com folga de concorrência,
	// ou se o limite máximo de réplicas já foi atingido e todas estão rodando.
	a.registryMu.Lock()
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
		a.registryMu.Unlock()
		return best, false, nil
	}
	a.registryMu.Unlock()

	// 2. Caminho Lento: precisamos fazer Cold Start de réplica parada ou Scale-Up de nova réplica.
	fn.mu.Lock()
	defer fn.mu.Unlock()

	// Revalida o estado após adquirir fn.mu (outra goroutine pode ter acabado de subir uma réplica).
	a.registryMu.Lock()
	best = findLeastLoadedRunningReplica(fn)
	if best != nil && best.ActiveRequests < fn.MaxConcurrency {
		best.ActiveRequests++
		best.LastUsedAt = time.Now()
		a.registryMu.Unlock()
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
		a.registryMu.Unlock()

		fmt.Printf("-> [Cold Start] Acordando réplica '%s' (%s)...\n", stoppedRep.ReplicaName, stoppedRep.ContainerID)
		newHostPort, err := a.wakeReplica(ctx, fn.FunctionName, stoppedRep)
		if err != nil {
			return nil, true, err
		}

		a.registryMu.Lock()
		stoppedRep.HostPort = newHostPort
		stoppedRep.Status = "RUNNING"
		stoppedRep.ActiveRequests++
		stoppedRep.LastUsedAt = time.Now()
		a.registryMu.Unlock()

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
		a.registryMu.Unlock()

		fmt.Printf("-> [Auto-Scale] Alta demanda em '%s'! Criando nova réplica '%s' (%d/%d)...\n",
			fn.FunctionName, replicaName, currentCount, maxReplicas)

		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, true, fmt.Errorf("erro ao conectar no Docker para Scale-Up: %w", err)
		}
		defer cli.Close()

		newRep, err := a.createAndStartReplica(ctx, cli, fn.FunctionName, fn.ImageName, replicaName)
		if err != nil {
			return nil, true, err
		}

		a.registryMu.Lock()
		newRep.ActiveRequests = 1
		newRep.LastUsedAt = time.Now()
		fn.Replicas = append(fn.Replicas, newRep)
		a.registryMu.Unlock()

		fmt.Printf("-> [Auto-Scale] Réplica '%s' pronta em 127.0.0.1:%s (Container ID: %s)!\n",
			newRep.ReplicaName, newRep.HostPort, newRep.ContainerID)
		return newRep, true, nil
	}

	// 2c. Já atingimos MaxReplicas e todas estão RUNNING: usa Least Connections na réplica menos sobrecarregada.
	best = findLeastLoadedRunningReplica(fn)
	if best == nil {
		a.registryMu.Unlock()
		return nil, false, fmt.Errorf("nenhuma réplica disponível para a função '%s'", fn.FunctionName)
	}
	best.ActiveRequests++
	best.LastUsedAt = time.Now()
	a.registryMu.Unlock()
	return best, false, nil
}

// startIdleReaper monitora periodicamente as réplicas de cada função:
// - Réplicas extras (índice > 0) ociosas são removidas (Scale-Down com ContainerRemove).
// - A réplica primária (índice == 0) ociosa é apenas parada (Scale-to-Zero com ContainerStop).
func (a *Agent) startIdleReaper() {
	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()

	for range ticker.C {
		var functions []*FunctionInstance
		a.registryMu.RLock()
		for _, fn := range a.registry {
			functions = append(functions, fn)
		}
		a.registryMu.RUnlock()

		for _, fn := range functions {
			fn.mu.Lock()

			a.registryMu.RLock()
			if len(fn.Replicas) == 0 {
				a.registryMu.RUnlock()
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
			a.registryMu.RUnlock()

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
					a.registryMu.Lock()
					kept := make([]*FunctionReplica, 0, len(fn.Replicas))
					for _, rep := range fn.Replicas {
						if !removedIDs[rep.ContainerID] {
							kept = append(kept, rep)
						}
					}
					fn.Replicas = kept
					a.registryMu.Unlock()
				}
			}

			// 2. Scale-to-Zero: para o container da réplica primária (#1) se também estiver ociosa.
			if stopPrimary != nil {
				fmt.Printf("-> [Reaper / Scale-to-Zero] Réplica primária '%s' ociosa há >%v. Parando container %s...\n",
					stopPrimary.ReplicaName, fn.IdleTimeout, stopPrimary.ContainerID)
				if err := cli.ContainerStop(context.Background(), stopPrimary.ContainerID, container.StopOptions{}); err != nil {
					log.Printf("-> [Reaper] Erro ao parar réplica primária %s: %v\n", stopPrimary.ContainerID, err)
				} else {
					a.registryMu.Lock()
					stopPrimary.Status = "STOPPED"
					stopPrimary.HostPort = ""
					a.registryMu.Unlock()
					fmt.Printf("-> [Reaper / Scale-to-Zero] Réplica primária '%s' parada com sucesso!\n", stopPrimary.ReplicaName)
				}
			}

			cli.Close()
			fn.mu.Unlock()
		}
	}
}
