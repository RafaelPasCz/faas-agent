package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

func resolveStateFilePath() string {
	if customPath := strings.TrimSpace(os.Getenv("FAAS_AGENT_STATE_FILE")); customPath != "" {
		return customPath
	}
	return defaultStateFilePath
}

// saveFunctionsState persiste as definições das funções implantadas em data/functions.json de forma atômica.
func (a *Agent) saveFunctionsState() error {
	a.registryMu.RLock()
	persisted := make([]PersistedFunction, 0, len(a.registry))
	for _, fn := range a.registry {
		persisted = append(persisted, PersistedFunction{
			FunctionName:       fn.FunctionName,
			ImageName:          fn.ImageName,
			IdleTimeoutSeconds: fn.IdleTimeoutSeconds,
			MaxReplicas:        fn.MaxReplicas,
			MaxConcurrency:     fn.MaxConcurrency,
		})
	}
	a.registryMu.RUnlock()

	sort.Slice(persisted, func(i, j int) bool {
		return persisted[i].FunctionName < persisted[j].FunctionName
	})

	a.stateMu.Lock()
	defer a.stateMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(a.stateFilePath), 0o755); err != nil {
		return fmt.Errorf("erro ao criar diretório de estado: %w", err)
	}

	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return fmt.Errorf("erro ao serializar estado das funções: %w", err)
	}
	data = append(data, '\n')

	tmpPath := a.stateFilePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("erro ao gravar arquivo temporário de estado: %w", err)
	}

	if err := os.Rename(tmpPath, a.stateFilePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("erro ao substituir arquivo de estado '%s': %w", a.stateFilePath, err)
	}

	return nil
}

// restoreFunctionsState recarrega as funções de data/functions.json e reconcilia com os containers Docker existentes.
func (a *Agent) restoreFunctionsState() error {
	raw, err := os.ReadFile(a.stateFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("erro ao ler arquivo de estado '%s': %w", a.stateFilePath, err)
	}

	if strings.TrimSpace(string(raw)) == "" {
		return nil
	}

	var persisted []PersistedFunction
	if err := json.Unmarshal(raw, &persisted); err != nil {
		return fmt.Errorf("erro ao decodificar arquivo de estado '%s': %w", a.stateFilePath, err)
	}
	if len(persisted) == 0 {
		return nil
	}

	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("erro ao conectar no Docker durante reconciliação: %w", err)
	}
	defer cli.Close()

	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return fmt.Errorf("erro ao listar containers Docker durante reconciliação: %w", err)
	}

	byFunction := make(map[string][]types.Container)
	for _, c := range containers {
		fnName := c.Labels["faas.function"]
		if fnName != "" {
			byFunction[fnName] = append(byFunction[fnName], c)
		}
	}

	for _, pf := range persisted {
		if pf.FunctionName == "" || pf.ImageName == "" {
			continue
		}

		idleTimeout := defaultIdleTimeout
		if pf.IdleTimeoutSeconds > 0 {
			idleTimeout = time.Duration(pf.IdleTimeoutSeconds) * time.Second
		}
		maxReplicas := defaultMaxReplicas
		if pf.MaxReplicas > 0 {
			maxReplicas = pf.MaxReplicas
		}
		maxConcurrency := defaultMaxConcurrency
		if pf.MaxConcurrency > 0 {
			maxConcurrency = pf.MaxConcurrency
		}

		existingContainers := byFunction[pf.FunctionName]
		sort.Slice(existingContainers, func(i, j int) bool {
			nameI := containerReplicaName(existingContainers[i])
			nameJ := containerReplicaName(existingContainers[j])
			seqI := extractReplicaSeq(pf.FunctionName, nameI)
			seqJ := extractReplicaSeq(pf.FunctionName, nameJ)
			if seqI != seqJ {
				return seqI < seqJ
			}
			return nameI < nameJ
		})

		replicas := make([]*FunctionReplica, 0)
		maxSeq := 0

		for _, c := range existingContainers {
			replicaName := containerReplicaName(c)
			seq := extractReplicaSeq(pf.FunctionName, replicaName)
			if seq > maxSeq {
				maxSeq = seq
			}

			shortID := c.ID
			if len(shortID) > 12 {
				shortID = shortID[:12]
			}

			if c.State == "running" {
				inspect, inspectErr := cli.ContainerInspect(ctx, c.ID)
				if inspectErr != nil {
					log.Printf("-> [State] Erro ao inspecionar container '%s': %v\n", replicaName, inspectErr)
					continue
				}
				bindings := inspect.NetworkSettings.Ports[nat.Port(containerFunctionPort)]
				if len(bindings) == 0 || bindings[0].HostPort == "" {
					log.Printf("-> [State] Container '%s' sem porta mapeada; removendo órfão.\n", replicaName)
					_ = cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
					continue
				}

				replicas = append(replicas, &FunctionReplica{
					ReplicaName:    replicaName,
					ContainerID:    shortID,
					HostPort:       bindings[0].HostPort,
					Status:         "RUNNING",
					ActiveRequests: 0,
					LastUsedAt:     time.Now(),
				})
				continue
			}

			// Réplica parada: preserva apenas se for a primeira réplica adotada (primária em Scale-to-Zero).
			if len(replicas) == 0 {
				replicas = append(replicas, &FunctionReplica{
					ReplicaName:    replicaName,
					ContainerID:    shortID,
					HostPort:       "",
					Status:         "STOPPED",
					ActiveRequests: 0,
					LastUsedAt:     time.Now(),
				})
			} else {
				_ = cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
			}
		}

		// Se nenhum container sobreviveu no Docker, recria a réplica inicial #1.
		if len(replicas) == 0 {
			if _, _, imgErr := cli.ImageInspectWithRaw(ctx, pf.ImageName); imgErr != nil {
				fmt.Printf("-> [State] Baixando imagem '%s' para restaurar '%s'...\n", pf.ImageName, pf.FunctionName)
				reader, pullErr := cli.ImagePull(ctx, pf.ImageName, image.PullOptions{})
				if pullErr != nil {
					log.Printf("-> [State] Falha ao baixar imagem '%s' de '%s': %v\n", pf.ImageName, pf.FunctionName, pullErr)
					continue
				}
				io.Copy(os.Stdout, reader)
				reader.Close()
			}

			firstReplicaName := fmt.Sprintf("%s-rep-1", pf.FunctionName)
			rep, createErr := a.createAndStartReplica(ctx, cli, pf.FunctionName, pf.ImageName, firstReplicaName)
			if createErr != nil {
				log.Printf("-> [State] Falha ao recriar réplica de '%s': %v\n", pf.FunctionName, createErr)
				continue
			}
			replicas = append(replicas, rep)
			if maxSeq < 1 {
				maxSeq = 1
			}
		}

		if maxSeq < len(replicas) {
			maxSeq = len(replicas)
		}

		instance := &FunctionInstance{
			FunctionName:       pf.FunctionName,
			ImageName:          pf.ImageName,
			IdleTimeoutSeconds: int(idleTimeout.Seconds()),
			IdleTimeout:        idleTimeout,
			MaxReplicas:        maxReplicas,
			MaxConcurrency:     maxConcurrency,
			Replicas:           replicas,
			nextReplicaSeq:     maxSeq,
		}

		a.registryMu.Lock()
		a.registry[pf.FunctionName] = instance
		a.registryMu.Unlock()

		fmt.Printf("-> [State] Função '%s' restaurada com %d réplica(s) (status primário: %s).\n",
			pf.FunctionName, len(replicas), replicas[0].Status)
	}

	return nil
}

func containerReplicaName(c types.Container) string {
	if name := c.Labels["faas.replica"]; name != "" {
		return name
	}
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	if len(c.ID) > 12 {
		return c.ID[:12]
	}
	return c.ID
}

func extractReplicaSeq(functionName, replicaName string) int {
	prefix := functionName + "-rep-"
	if strings.HasPrefix(replicaName, prefix) {
		if seq, err := strconv.Atoi(strings.TrimPrefix(replicaName, prefix)); err == nil && seq > 0 {
			return seq
		}
	}
	return 999999
}
