package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ExecutionLogStore gerencia o buffer em memória e a persistência append-only (.jsonl) dos logs de execução.
type ExecutionLogStore struct {
	mu          sync.RWMutex
	filePath    string
	logs        []ExecutionLog
	lastID      int64
	linesOnDisk int
}

func resolveLogFilePath() string {
	if customPath := strings.TrimSpace(os.Getenv("FAAS_AGENT_LOG_FILE")); customPath != "" {
		return customPath
	}
	return defaultLogFilePath
}

// NewExecutionLogStore inicializa o armazenamento de logs e carrega o histórico persistido em disco.
func NewExecutionLogStore(filePath string) (*ExecutionLogStore, error) {
	store := &ExecutionLogStore{
		filePath: filePath,
		logs:     make([]ExecutionLog, 0),
	}
	if err := store.loadFromDisk(); err != nil {
		return store, err
	}
	return store, nil
}

// FilePath retorna o caminho configurado para o arquivo .jsonl de logs.
func (s *ExecutionLogStore) FilePath() string {
	return s.filePath
}

// loadFromDisk carrega o histórico persistido em formato JSON Lines (.jsonl) para a memória.
func (s *ExecutionLogStore) loadFromDisk() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.filePath), 0o755); err != nil {
		return fmt.Errorf("erro ao criar diretório de logs: %w", err)
	}

	file, err := os.Open(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			s.logs = make([]ExecutionLog, 0)
			s.lastID = 0
			s.linesOnDisk = 0
			return nil
		}
		return fmt.Errorf("erro ao abrir arquivo de logs '%s': %w", s.filePath, err)
	}
	defer file.Close()

	loaded := make([]ExecutionLog, 0)
	var maxID int64
	diskLines := 0

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var entry ExecutionLog
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			log.Printf("-> [Logs] Linha inválida ignorada em %s: %v\n", s.filePath, err)
			continue
		}

		diskLines++
		if entry.ID > maxID {
			maxID = entry.ID
		}
		loaded = append(loaded, entry)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("erro ao ler arquivo de logs '%s': %w", s.filePath, err)
	}

	if len(loaded) > maxInMemoryLogs {
		trimmed := make([]ExecutionLog, maxInMemoryLogs)
		copy(trimmed, loaded[len(loaded)-maxInMemoryLogs:])
		loaded = trimmed
	}

	s.logs = loaded
	s.lastID = maxID
	s.linesOnDisk = diskLines

	if s.linesOnDisk > maxInMemoryLogs {
		if err := s.compactLocked(); err != nil {
			log.Printf("-> [Logs] Aviso ao compactar logs na inicialização: %v\n", err)
		}
	}

	return nil
}

// compactLocked reescreve o arquivo .jsonl de forma atômica mantendo apenas os registros em memória.
// Deve ser chamada com s.mu.Lock() já adquirido.
func (s *ExecutionLogStore) compactLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.filePath), 0o755); err != nil {
		return fmt.Errorf("erro ao garantir diretório de logs: %w", err)
	}

	tmpPath := s.filePath + ".tmp"
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("erro ao criar arquivo temporário de compactação: %w", err)
	}

	writer := bufio.NewWriter(tmpFile)
	for _, entry := range s.logs {
		data, marshalErr := json.Marshal(entry)
		if marshalErr != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("erro ao serializar log durante compactação: %w", marshalErr)
		}
		if _, writeErr := writer.Write(data); writeErr != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("erro ao escrever log durante compactação: %w", writeErr)
		}
		if writeErr := writer.WriteByte('\n'); writeErr != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("erro ao escrever quebra de linha durante compactação: %w", writeErr)
		}
	}

	if err := writer.Flush(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("erro ao descarregar buffer de compactação: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("erro ao fechar arquivo temporário de compactação: %w", err)
	}

	if err := os.Rename(tmpPath, s.filePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("erro ao substituir arquivo de logs após compactação: %w", err)
	}

	s.linesOnDisk = len(s.logs)
	return nil
}

// Record registra a execução em memória e persiste no arquivo .jsonl em modo append-only.
func (s *ExecutionLogStore) Record(functionName, replicaName string, statusCode int, durationMs int64, coldStart bool, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastID++
	entry := ExecutionLog{
		ID:                s.lastID,
		FunctionName:      functionName,
		ReplicaName:       replicaName,
		StatusCode:        statusCode,
		RequestDurationMs: durationMs,
		Success:           errMsg == "" && statusCode >= 200 && statusCode < 400,
		ColdStart:         coldStart,
		ExecutedAt:        time.Now(),
		ErrorMessage:      errMsg,
	}

	s.logs = append(s.logs, entry)
	if len(s.logs) > maxInMemoryLogs {
		trimmed := make([]ExecutionLog, maxInMemoryLogs)
		copy(trimmed, s.logs[len(s.logs)-maxInMemoryLogs:])
		s.logs = trimmed
	}

	data, err := json.Marshal(entry)
	if err != nil {
		log.Printf("-> [Logs] Erro ao serializar log #%d: %v\n", entry.ID, err)
		return
	}

	if err := os.MkdirAll(filepath.Dir(s.filePath), 0o755); err != nil {
		log.Printf("-> [Logs] Erro ao criar diretório de logs: %v\n", err)
		return
	}

	file, err := os.OpenFile(s.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("-> [Logs] Erro ao abrir arquivo '%s' para append: %v\n", s.filePath, err)
		return
	}

	_, writeErr := file.Write(append(data, '\n'))
	closeErr := file.Close()
	if writeErr != nil {
		log.Printf("-> [Logs] Erro ao persistir log #%d: %v\n", entry.ID, writeErr)
		return
	}
	if closeErr != nil {
		log.Printf("-> [Logs] Erro ao fechar arquivo '%s': %v\n", s.filePath, closeErr)
		return
	}

	s.linesOnDisk++
	if s.linesOnDisk >= compactionThreshold {
		if err := s.compactLocked(); err != nil {
			log.Printf("-> [Logs] Erro ao compactar '%s': %v\n", s.filePath, err)
		}
	}
}

// Query filtra os logs em memória por função, cursor incremental (afterId) e limite de registros.
func (s *ExecutionLogStore) Query(functionName string, afterID int64, hasAfterID bool, limit int) []ExecutionLog {
	s.mu.RLock()
	filtered := make([]ExecutionLog, 0, len(s.logs))
	for _, entry := range s.logs {
		if functionName != "" && entry.FunctionName != functionName {
			continue
		}
		if hasAfterID && entry.ID <= afterID {
			continue
		}
		filtered = append(filtered, entry)
	}
	s.mu.RUnlock()

	if limit > 0 && len(filtered) > limit {
		if hasAfterID {
			filtered = filtered[:limit]
		} else {
			filtered = filtered[len(filtered)-limit:]
		}
	}

	return filtered
}

func (a *Agent) listLogsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Apenas o método GET é permitido", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	functionName := strings.TrimSpace(query.Get("functionName"))

	var afterID int64
	hasAfterID := false
	if rawAfterID := strings.TrimSpace(query.Get("afterId")); rawAfterID != "" {
		parsed, err := strconv.ParseInt(rawAfterID, 10, 64)
		if err != nil || parsed < 0 {
			http.Error(w, "Parâmetro 'afterId' deve ser um inteiro >= 0", http.StatusBadRequest)
			return
		}
		afterID = parsed
		hasAfterID = true
	}

	limit := 0
	if rawLimit := strings.TrimSpace(query.Get("limit")); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed <= 0 {
			http.Error(w, "Parâmetro 'limit' deve ser um inteiro > 0", http.StatusBadRequest)
			return
		}
		limit = parsed
	}

	filtered := a.logStore.Query(functionName, afterID, hasAfterID, limit)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(filtered)
}
