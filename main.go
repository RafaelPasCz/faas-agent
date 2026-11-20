package main

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// Agent encapsula o estado em memória do faas-agent e suas dependências injetadas.
type Agent struct {
	registryMu   sync.RWMutex
	registry     map[string]*FunctionInstance
	logStore     *ExecutionLogStore
	proxyClient  *http.Client
	healthClient *http.Client
}

// NewAgent cria uma nova instância do agente com pool de conexões HTTP configurado.
func NewAgent(logStore *ExecutionLogStore) *Agent {
	sharedTransport := &http.Transport{
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}

	return &Agent{
		registry: make(map[string]*FunctionInstance),
		logStore: logStore,
		proxyClient: &http.Client{
			Timeout:   60 * time.Second,
			Transport: sharedTransport,
		},
		healthClient: &http.Client{
			Timeout:   500 * time.Millisecond,
			Transport: sharedTransport,
		},
	}
}

func main() {
	logStore, err := NewExecutionLogStore(resolveLogFilePath())
	if err != nil {
		log.Printf("-> [Logs] Aviso ao carregar histórico de execuções: %v\n", err)
	}

	agent := NewAgent(logStore)

	go agent.startIdleReaper()

	http.HandleFunc("/deploy", agent.deployHandler)
	http.HandleFunc("/functions", agent.listFunctionsHandler)
	http.HandleFunc("/function/{functionName}", agent.functionByNameHandler)
	http.HandleFunc("/functions/{functionName}", agent.functionByNameHandler)
	http.HandleFunc("/invoke/{functionName}", agent.invokeHandler)
	http.HandleFunc("/logs", agent.listLogsHandler)

	fmt.Printf("Faas-Agent rodando na porta 8081 (Auto-Scaler + Idle Reaper + Logs em %s ativos)...\n", logStore.FilePath())
	if err := http.ListenAndServe(":8081", nil); err != nil {
		log.Fatal("Erro ao iniciar o servidor: ", err)
	}
}
