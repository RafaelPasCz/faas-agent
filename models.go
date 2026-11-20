package main

import (
	"sync"
	"time"
)

const (
	containerFunctionPort = "8080/tcp"
	defaultIdleTimeout    = 60 * time.Second
	defaultMaxReplicas    = 3
	defaultMaxConcurrency = 4
	reaperInterval        = 5 * time.Second
	coldStartTimeout      = 15 * time.Second

	defaultLogFilePath   = "data/execution_logs.jsonl"
	defaultStateFilePath = "data/functions.json"
	maxInMemoryLogs      = 1000
	compactionThreshold  = 1200
)

type DeployRequest struct {
	FunctionName       string `json:"functionName"`
	ImageName          string `json:"imageName"`
	IdleTimeoutSeconds int    `json:"idleTimeoutSeconds,omitempty"`
	MaxReplicas        int    `json:"maxReplicas,omitempty"`
	MaxConcurrency     int    `json:"maxConcurrency,omitempty"`
}

// PersistedFunction representa a configuração persistida em disco de uma função implantada no agente.
type PersistedFunction struct {
	FunctionName       string `json:"functionName"`
	ImageName          string `json:"imageName"`
	IdleTimeoutSeconds int    `json:"idleTimeoutSeconds"`
	MaxReplicas        int    `json:"maxReplicas"`
	MaxConcurrency     int    `json:"maxConcurrency"`
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
	Status             string             `json:"status"`         // "RUNNING" ou "STOPPED"
	ExecutionState     string             `json:"executionState"` // "IDLE" ou "EXECUTING"
	ActiveRequests     int                `json:"activeRequests"`
	RunningReplicas    int                `json:"runningReplicas"`
	TotalReplicas      int                `json:"totalReplicas"`
	LastUsedAt         time.Time          `json:"lastUsedAt"`
	IdleTimeoutSeconds int                `json:"idleTimeoutSeconds"`
	IdleTimeout        time.Duration      `json:"-"`
	MaxReplicas        int                `json:"maxReplicas"`
	MaxConcurrency     int                `json:"maxConcurrency"`
	Replicas           []*FunctionReplica `json:"replicas"`
	nextReplicaSeq     int                // Contador interno para gerar nomes únicos de réplica
	mu                 sync.Mutex         // Serializa operações no Docker (Cold Start, Scale-Up e Scale-Down)
}

// ExecutionLog representa o registro imutável de uma invocação processada pelo faas-agent.
type ExecutionLog struct {
	ID                int64     `json:"id"`
	FunctionName      string    `json:"functionName"`
	ReplicaName       string    `json:"replicaName,omitempty"`
	StatusCode        int       `json:"statusCode"`
	RequestDurationMs int64     `json:"requestDurationMs"`
	Success           bool      `json:"success"`
	ColdStart         bool      `json:"coldStart"`
	ExecutedAt        time.Time `json:"executedAt"`
	ErrorMessage      string    `json:"errorMessage,omitempty"`
}

// buildFunctionSnapshot constrói uma cópia segura da função com os campos consolidados calculados.
// Deve ser chamada com registryMu (RLock ou Lock) já adquirido.
func buildFunctionSnapshot(fn *FunctionInstance) *FunctionInstance {
	runningReplicas := 0
	activeRequests := 0
	var lastUsedAt time.Time

	replicasCopy := make([]*FunctionReplica, 0, len(fn.Replicas))
	for _, rep := range fn.Replicas {
		if rep.Status == "RUNNING" {
			runningReplicas++
		}
		activeRequests += rep.ActiveRequests
		if rep.LastUsedAt.After(lastUsedAt) {
			lastUsedAt = rep.LastUsedAt
		}
		repCopy := *rep
		replicasCopy = append(replicasCopy, &repCopy)
	}

	status := "STOPPED"
	if runningReplicas > 0 {
		status = "RUNNING"
	}

	executionState := "IDLE"
	if activeRequests > 0 {
		executionState = "EXECUTING"
	}

	return &FunctionInstance{
		FunctionName:       fn.FunctionName,
		ImageName:          fn.ImageName,
		Status:             status,
		ExecutionState:     executionState,
		ActiveRequests:     activeRequests,
		RunningReplicas:    runningReplicas,
		TotalReplicas:      len(replicasCopy),
		LastUsedAt:         lastUsedAt,
		IdleTimeoutSeconds: fn.IdleTimeoutSeconds,
		MaxReplicas:        fn.MaxReplicas,
		MaxConcurrency:     fn.MaxConcurrency,
		Replicas:           replicasCopy,
	}
}
