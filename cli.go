package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	defaultAgentURL = "http://localhost:8081"
	templatesDir    = "templates"
	functionsDir    = "functions"
)

//go:embed all:templates
var embeddedTemplates embed.FS

var cliHTTPClient = &http.Client{
	Timeout: 5 * time.Minute,
}

func resolveAgentURL() string {
	if raw := strings.TrimSpace(os.Getenv("FAAS_AGENT_URL")); raw != "" {
		return strings.TrimRight(raw, "/")
	}
	return defaultAgentURL
}

func runCLI(args []string) error {
	return runCLIWithIO(args, os.Stdin, os.Stdout, os.Stderr)
}

func runCLIWithIO(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printCLIHelp(stdout)
		return nil
	}

	var err error
	switch args[0] {
	case "new":
		err = cliNew(args[1:], stdout)
	case "templates":
		err = cliTemplates(args[1:], stdout)
	case "deploy":
		err = cliDeploy(args[1:], stdout)
	case "delete", "rm":
		err = cliDelete(args[1:], stdout)
	case "invoke":
		err = cliInvoke(args[1:], stdin, stdout, stderr)
	case "logs":
		err = cliLogs(args[1:], stdout)
	case "functions", "ls":
		err = cliFunctions(args[1:], stdout)
	case "help", "-h", "--help":
		printCLIHelp(stdout)
	default:
		fmt.Fprintf(stderr, "Subcomando desconhecido: '%s'\n\n", args[0])
		printCLIHelp(stderr)
		return fmt.Errorf("subcomando inválido: %s", args[0])
	}

	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func printCLIHelp(out io.Writer) {
	fmt.Fprintln(out, "Uso:")
	fmt.Fprintln(out, "  faas-agent [serve]                      Inicia o servidor HTTP na porta :8081")
	fmt.Fprintln(out, "  faas-agent new <template> <funcao>      Cria uma nova função em functions/<funcao>")
	fmt.Fprintln(out, "  faas-agent templates                    Lista os templates disponíveis")
	fmt.Fprintln(out, "  faas-agent deploy <funcao> <imagem>     Implanta uma função no agente em execução")
	fmt.Fprintln(out, "  faas-agent functions [funcao]   (ls)    Lista funções implantadas ou detalha uma função")
	fmt.Fprintln(out, "  faas-agent invoke <funcao> [-d JSON]    Invoca uma função e exibe a resposta")
	fmt.Fprintln(out, "  faas-agent logs [funcao] [-n 20]        Consulta os logs de execução do agente")
	fmt.Fprintln(out, "  faas-agent delete <funcao>      (rm)    Remove uma função e suas réplicas")
	fmt.Fprintln(out, "  faas-agent help                         Exibe esta ajuda")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Use '<subcomando> --help' para ver as flags disponíveis (ex.: 'faas-agent invoke --help').")
	fmt.Fprintln(out, "Variável de ambiente: FAAS_AGENT_URL (padrão: http://localhost:8081)")
}

type boolFlag interface {
	flag.Value
	IsBoolFlag() bool
}

func newSubcommandFlagSet(name, usageLine string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {
		fmt.Fprintf(out, "Uso: %s\n", usageLine)
		fs.SetOutput(out)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
	}
	return fs
}

// parseFlagsFlexible permite passar flags tanto antes quanto depois dos argumentos posicionais.
func parseFlagsFlexible(fs *flag.FlagSet, args []string) ([]string, error) {
	isBool := make(map[string]bool)
	fs.VisitAll(func(f *flag.Flag) {
		bf, ok := f.Value.(boolFlag)
		isBool[f.Name] = ok && bf.IsBoolFlag()
	})

	var flagArgs []string
	var positionals []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			trimmed := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
			name, _, hasEq := strings.Cut(trimmed, "=")
			flagArgs = append(flagArgs, arg)
			if !hasEq {
				if boolVal, known := isBool[name]; known && !boolVal && i+1 < len(args) {
					flagArgs = append(flagArgs, args[i+1])
					i++
				}
			}
			continue
		}
		positionals = append(positionals, arg)
	}

	// fs.Parse já invoca fs.Usage() internamente antes de retornar flag.ErrHelp.
	if err := fs.Parse(flagArgs); err != nil {
		return nil, err
	}

	return append(positionals, fs.Args()...), nil
}

func doAgentRequest(method, rawURL, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("erro ao montar requisição HTTP: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := cliHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("não foi possível conectar ao faas-agent em %s (%v). Verifique se o servidor está rodando", rawURL, err)
	}
	return resp, nil
}

func readHTTPError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("agente retornou HTTP %d: %s", resp.StatusCode, msg)
}

func cliNew(args []string, stdout io.Writer) error {
	var (
		templateName string
		functionName string
	)

	fsFlag := newSubcommandFlagSet("new", "faas-agent new [<template> <funcao>] [flags]", stdout)
	fsFlag.StringVar(&templateName, "template", "", "Nome do template da função")
	fsFlag.StringVar(&functionName, "name", "", "Nome da função")

	pos, err := parseFlagsFlexible(fsFlag, args)
	if err != nil {
		return err
	}

	posIdx := 0
	if templateName == "" && posIdx < len(pos) {
		templateName = pos[posIdx]
		posIdx++
	}
	if functionName == "" && posIdx < len(pos) {
		functionName = pos[posIdx]
		posIdx++
	}
	if posIdx < len(pos) {
		return fmt.Errorf("argumentos extras inesperados para 'new': %s", strings.Join(pos[posIdx:], " "))
	}

	if templateName == "" || functionName == "" {
		available := listAvailableTemplates()
		if len(available) > 0 {
			return fmt.Errorf("informe <template> e <funcao> (ex.: faas-agent new %s minha-funcao; disponíveis: %s)", available[0], strings.Join(available, ", "))
		}
		return fmt.Errorf("informe <template> e <funcao> (ex.: faas-agent new python3-http minha-funcao)")
	}

	if err := scaffoldFunction(templateName, functionName); err != nil {
		return err
	}

	destDir := filepath.Join(functionsDir, functionName)
	fmt.Fprintf(stdout, "Função '%s' criada em '%s/' a partir do template '%s'.\n", functionName, destDir, templateName)
	fmt.Fprintf(stdout, "Próximos passos:\n")
	fmt.Fprintf(stdout, "  1. Edite o código em %s/function/handler.py\n", destDir)
	fmt.Fprintf(stdout, "  2. Gere a imagem Docker:\n")
	fmt.Fprintf(stdout, "     docker build -t %s:latest ./%s\n", functionName, destDir)
	fmt.Fprintf(stdout, "  3. Faça o deploy no agente:\n")
	fmt.Fprintf(stdout, "     faas-agent deploy %s %s:latest\n", functionName, functionName)
	return nil
}

func cliTemplates(args []string, stdout io.Writer) error {
	var jsonOutput bool

	fsFlag := newSubcommandFlagSet("templates", "faas-agent templates [--json]", stdout)
	fsFlag.BoolVar(&jsonOutput, "json", false, "Exibe os templates disponíveis em JSON")

	pos, err := parseFlagsFlexible(fsFlag, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("argumentos inesperados para 'templates': %s", strings.Join(pos, " "))
	}

	names := listAvailableTemplates()
	if jsonOutput {
		raw, err := json.Marshal(names)
		if err != nil {
			return fmt.Errorf("erro ao serializar templates: %w", err)
		}
		fmt.Fprintln(stdout, string(raw))
		return nil
	}

	printAvailableTemplates(stdout, names)
	return nil
}

func printAvailableTemplates(out io.Writer, names []string) {
	if len(names) == 0 {
		fmt.Fprintln(out, "Nenhum template disponível.")
		return
	}
	fmt.Fprintln(out, "Templates disponíveis:")
	for _, name := range names {
		fmt.Fprintf(out, "  - %s\n", name)
	}
}

func listAvailableTemplates() []string {
	seen := make(map[string]bool)

	if entries, err := os.ReadDir(templatesDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				seen[entry.Name()] = true
			}
		}
	}

	if entries, err := fs.ReadDir(embeddedTemplates, templatesDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				seen[entry.Name()] = true
			}
		}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func resolveTemplateFS(templateName string) (fs.FS, error) {
	if !isSafeSegment(templateName) {
		return nil, fmt.Errorf("nome de template inválido: '%s'", templateName)
	}

	localPath := filepath.Join(templatesDir, templateName)
	if info, err := os.Stat(localPath); err == nil && info.IsDir() {
		return os.DirFS(localPath), nil
	}

	embeddedPath := templatesDir + "/" + templateName
	if _, err := fs.ReadDir(embeddedTemplates, embeddedPath); err == nil {
		return fs.Sub(embeddedTemplates, embeddedPath)
	}

	available := listAvailableTemplates()
	if len(available) > 0 {
		return nil, fmt.Errorf("template '%s' não encontrado (disponíveis: %s)", templateName, strings.Join(available, ", "))
	}
	return nil, fmt.Errorf("template '%s' não encontrado", templateName)
}

func scaffoldFunction(templateName, functionName string) error {
	if !isSafeSegment(functionName) {
		return fmt.Errorf("nome de função inválido: '%s' (não use espaços, '/' ou '..')", functionName)
	}

	srcFS, err := resolveTemplateFS(templateName)
	if err != nil {
		return err
	}

	destDir := filepath.Join(functionsDir, functionName)
	if _, err := os.Stat(destDir); err == nil {
		return fmt.Errorf("o diretório '%s' já existe; remova-o ou escolha outro nome", destDir)
	}

	if err := os.MkdirAll(functionsDir, 0o755); err != nil {
		return fmt.Errorf("erro ao criar diretório '%s': %w", functionsDir, err)
	}

	if err := os.CopyFS(destDir, srcFS); err != nil {
		return fmt.Errorf("erro ao copiar template '%s' para '%s': %w", templateName, destDir, err)
	}

	return nil
}

func isSafeSegment(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || trimmed != name || trimmed == "." || trimmed == ".." {
		return false
	}
	return !strings.ContainsAny(trimmed, " \t\n\r/\\")
}

func cliDeploy(args []string, stdout io.Writer) error {
	var (
		functionName   string
		imageName      string
		idleTimeout    int
		maxReplicas    int
		maxConcurrency int
		agentURL       string
		jsonOutput     bool
	)

	fs := newSubcommandFlagSet("deploy", "faas-agent deploy <funcao> <imagem> [flags]", stdout)
	fs.StringVar(&functionName, "name", "", "Nome da função")
	fs.StringVar(&imageName, "image", "", "Imagem Docker da função")
	fs.IntVar(&idleTimeout, "idle-timeout", 0, "Tempo limite de ociosidade em segundos (padrão do agente: 60)")
	fs.IntVar(&maxReplicas, "max-replicas", 0, "Número máximo de réplicas (padrão do agente: 3)")
	fs.IntVar(&maxConcurrency, "max-concurrency", 0, "Concorrência por réplica antes de escalar (padrão do agente: 4)")
	fs.StringVar(&agentURL, "url", resolveAgentURL(), "URL base do faas-agent")
	fs.BoolVar(&jsonOutput, "json", false, "Exibe a resposta bruta em JSON")

	pos, err := parseFlagsFlexible(fs, args)
	if err != nil {
		return err
	}

	if functionName == "" && len(pos) > 0 {
		functionName = pos[0]
	}
	if imageName == "" && len(pos) > 1 {
		imageName = pos[1]
	}

	if functionName == "" || imageName == "" {
		return fmt.Errorf("informe <funcao> e <imagem> (ex.: faas-agent deploy hello-python switchfaas/hello-python:latest)")
	}

	payload := DeployRequest{
		FunctionName:       functionName,
		ImageName:          imageName,
		IdleTimeoutSeconds: idleTimeout,
		MaxReplicas:        maxReplicas,
		MaxConcurrency:     maxConcurrency,
	}

	rawBody, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("erro ao serializar requisição: %w", err)
	}

	targetURL := strings.TrimRight(agentURL, "/") + "/deploy"
	resp, err := doAgentRequest(http.MethodPost, targetURL, "application/json", bytes.NewReader(rawBody))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return readHTTPError(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("erro ao ler resposta do deploy: %w", err)
	}

	if jsonOutput {
		fmt.Fprintln(stdout, strings.TrimSpace(string(body)))
		return nil
	}

	var fn FunctionInstance
	if err := json.Unmarshal(body, &fn); err != nil {
		fmt.Fprintln(stdout, strings.TrimSpace(string(body)))
		return nil
	}

	fmt.Fprintf(stdout, "Função '%s' implantada com sucesso!\n", fn.FunctionName)
	fmt.Fprintf(stdout, "  Imagem:      %s\n", fn.ImageName)
	fmt.Fprintf(stdout, "  Status:      %s (%s)\n", fn.Status, fn.ExecutionState)
	fmt.Fprintf(stdout, "  Escala:      maxReplicas=%d | maxConcurrency=%d | idleTimeout=%ds\n",
		fn.MaxReplicas, fn.MaxConcurrency, fn.IdleTimeoutSeconds)
	for _, rep := range fn.Replicas {
		fmt.Fprintf(stdout, "  Réplica:     %s (127.0.0.1:%s | container=%s | status=%s)\n",
			rep.ReplicaName, rep.HostPort, rep.ContainerID, rep.Status)
	}
	return nil
}

func cliDelete(args []string, stdout io.Writer) error {
	var (
		agentURL   string
		jsonOutput bool
	)

	fs := newSubcommandFlagSet("delete", "faas-agent delete <funcao> [flags]", stdout)
	fs.StringVar(&agentURL, "url", resolveAgentURL(), "URL base do faas-agent")
	fs.BoolVar(&jsonOutput, "json", false, "Exibe a resposta bruta em JSON")

	pos, err := parseFlagsFlexible(fs, args)
	if err != nil {
		return err
	}

	if len(pos) != 1 {
		return fmt.Errorf("informe exatamente o nome da função (ex.: faas-agent delete hello-python)")
	}

	functionName := pos[0]
	targetURL := strings.TrimRight(agentURL, "/") + "/function/" + url.PathEscape(functionName)
	resp, err := doAgentRequest(http.MethodDelete, targetURL, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return readHTTPError(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("erro ao ler resposta da remoção: %w", err)
	}

	if jsonOutput {
		fmt.Fprintln(stdout, strings.TrimSpace(string(body)))
		return nil
	}

	var result struct {
		Message         string `json:"message"`
		FunctionName    string `json:"functionName"`
		RemovedReplicas int    `json:"removedReplicas"`
	}
	if err := json.Unmarshal(body, &result); err == nil && result.Message != "" {
		fmt.Fprintln(stdout, result.Message)
		return nil
	}

	fmt.Fprintf(stdout, "Função '%s' removida com sucesso.\n", functionName)
	return nil
}

func cliInvoke(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	var (
		data        string
		contentType string
		agentURL    string
		quiet       bool
	)

	fs := newSubcommandFlagSet("invoke", "faas-agent invoke <funcao> [-d JSON|-] [flags]", stdout)
	fs.StringVar(&data, "d", "", "Corpo da requisição (ou '-' para ler do stdin)")
	fs.StringVar(&data, "data", "", "Corpo da requisição (ou '-' para ler do stdin)")
	fs.StringVar(&contentType, "content-type", "application/json", "Cabeçalho Content-Type da requisição")
	fs.StringVar(&agentURL, "url", resolveAgentURL(), "URL base do faas-agent")
	fs.BoolVar(&quiet, "q", false, "Oculta a linha de telemetria no stderr")
	fs.BoolVar(&quiet, "quiet", false, "Oculta a linha de telemetria no stderr")

	pos, err := parseFlagsFlexible(fs, args)
	if err != nil {
		return err
	}

	if len(pos) != 1 {
		return fmt.Errorf("informe exatamente o nome da função (ex.: faas-agent invoke hello-python -d '{\"name\":\"Rafael\"}')")
	}

	functionName := pos[0]
	var reqBody io.Reader
	if data == "-" {
		rawStdin, readErr := io.ReadAll(stdin)
		if readErr != nil {
			return fmt.Errorf("erro ao ler payload do stdin: %w", readErr)
		}
		reqBody = bytes.NewReader(rawStdin)
	} else if data != "" {
		reqBody = strings.NewReader(data)
	} else {
		reqBody = http.NoBody
	}

	targetURL := strings.TrimRight(agentURL, "/") + "/invoke/" + url.PathEscape(functionName)
	resp, err := doAgentRequest(http.MethodPost, targetURL, contentType, reqBody)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	replicaName := resp.Header.Get("X-Replica-Name")
	if resp.StatusCode >= 400 && replicaName == "" {
		return readHTTPError(resp)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("erro ao ler resposta da função: %w", err)
	}

	if len(respBody) > 0 {
		outStr := string(respBody)
		if !strings.HasSuffix(outStr, "\n") {
			outStr += "\n"
		}
		fmt.Fprint(stdout, outStr)
	}

	if !quiet {
		duration := resp.Header.Get("X-Execution-Duration-Ms")
		coldStart := resp.Header.Get("X-Cold-Start")
		fmt.Fprintf(stderr, "-> [Invoke] Status: %d | Réplica: %s | Cold Start: %s | Duração: %sms\n",
			resp.StatusCode, replicaName, coldStart, duration)
	}

	return nil
}

func cliLogs(args []string, stdout io.Writer) error {
	var (
		functionName string
		limit        int
		afterID      int64
		agentURL     string
		jsonOutput   bool
	)

	fs := newSubcommandFlagSet("logs", "faas-agent logs [funcao] [flags]", stdout)
	fs.StringVar(&functionName, "f", "", "Filtra pelo nome da função")
	fs.StringVar(&functionName, "function", "", "Filtra pelo nome da função")
	fs.IntVar(&limit, "n", 20, "Quantidade máxima de registros (0 para todos)")
	fs.IntVar(&limit, "limit", 20, "Quantidade máxima de registros (0 para todos)")
	fs.Int64Var(&afterID, "after-id", -1, "Retorna apenas logs com id > after-id")
	fs.StringVar(&agentURL, "url", resolveAgentURL(), "URL base do faas-agent")
	fs.BoolVar(&jsonOutput, "json", false, "Exibe a resposta bruta em JSON")

	pos, err := parseFlagsFlexible(fs, args)
	if err != nil {
		return err
	}

	if functionName == "" && len(pos) > 0 {
		functionName = pos[0]
	}

	q := url.Values{}
	if functionName != "" {
		q.Set("functionName", functionName)
	}
	if afterID >= 0 {
		q.Set("afterId", strconv.FormatInt(afterID, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}

	targetURL := strings.TrimRight(agentURL, "/") + "/logs"
	if encoded := q.Encode(); encoded != "" {
		targetURL += "?" + encoded
	}

	resp, err := doAgentRequest(http.MethodGet, targetURL, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return readHTTPError(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("erro ao ler logs: %w", err)
	}

	if jsonOutput {
		fmt.Fprintln(stdout, strings.TrimSpace(string(body)))
		return nil
	}

	var entries []ExecutionLog
	if err := json.Unmarshal(body, &entries); err != nil {
		return fmt.Errorf("erro ao decodificar logs JSON: %w", err)
	}

	if len(entries) == 0 {
		fmt.Fprintln(stdout, "Nenhum log de execução encontrado.")
		return nil
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tDATA/HORA\tFUNÇÃO\tRÉPLICA\tSTATUS\tRESULTADO\tCOLD\tDURAÇÃO\tERRO")
	for _, entry := range entries {
		res := "FALHA"
		if entry.Success {
			res = "OK"
		}
		cold := "não"
		if entry.ColdStart {
			cold = "sim"
		}
		replica := entry.ReplicaName
		if replica == "" {
			replica = "-"
		}
		errText := entry.ErrorMessage
		if errText == "" {
			errText = "-"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d\t%s\t%s\t%dms\t%s\n",
			entry.ID,
			entry.ExecutedAt.Local().Format("02/01/2006 15:04:05"),
			entry.FunctionName,
			replica,
			entry.StatusCode,
			res,
			cold,
			entry.RequestDurationMs,
			errText,
		)
	}
	return tw.Flush()
}

func cliFunctions(args []string, stdout io.Writer) error {
	var (
		agentURL   string
		jsonOutput bool
	)

	fs := newSubcommandFlagSet("functions", "faas-agent functions [funcao] [flags]", stdout)
	fs.StringVar(&agentURL, "url", resolveAgentURL(), "URL base do faas-agent")
	fs.BoolVar(&jsonOutput, "json", false, "Exibe a resposta bruta em JSON")

	pos, err := parseFlagsFlexible(fs, args)
	if err != nil {
		return err
	}

	if len(pos) > 1 {
		return fmt.Errorf("uso: faas-agent functions [funcao]")
	}

	baseURL := strings.TrimRight(agentURL, "/")
	if len(pos) == 1 {
		return printSingleFunction(baseURL, pos[0], jsonOutput, stdout)
	}

	resp, err := doAgentRequest(http.MethodGet, baseURL+"/functions", "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return readHTTPError(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("erro ao ler lista de funções: %w", err)
	}

	if jsonOutput {
		fmt.Fprintln(stdout, strings.TrimSpace(string(body)))
		return nil
	}

	var functions []*FunctionInstance
	if err := json.Unmarshal(body, &functions); err != nil {
		return fmt.Errorf("erro ao decodificar funções JSON: %w", err)
	}

	if len(functions) == 0 {
		fmt.Fprintln(stdout, "Nenhuma função implantada no agente.")
		return nil
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FUNÇÃO\tIMAGEM\tSTATUS\tESTADO\tRÉPLICAS\tATIVAS\tÚLTIMO USO")
	for _, fn := range functions {
		lastUsed := "-"
		if !fn.LastUsedAt.IsZero() {
			lastUsed = fn.LastUsedAt.Local().Format("02/01/2006 15:04:05")
		}
		replicasInfo := fmt.Sprintf("%d/%d (max %d)", fn.RunningReplicas, fn.TotalReplicas, fn.MaxReplicas)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			fn.FunctionName,
			fn.ImageName,
			fn.Status,
			fn.ExecutionState,
			replicasInfo,
			fn.ActiveRequests,
			lastUsed,
		)
	}
	return tw.Flush()
}

func printSingleFunction(baseURL, functionName string, jsonOutput bool, stdout io.Writer) error {
	targetURL := baseURL + "/function/" + url.PathEscape(functionName)
	resp, err := doAgentRequest(http.MethodGet, targetURL, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return readHTTPError(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("erro ao ler detalhes da função: %w", err)
	}

	if jsonOutput {
		fmt.Fprintln(stdout, strings.TrimSpace(string(body)))
		return nil
	}

	var fn FunctionInstance
	if err := json.Unmarshal(body, &fn); err != nil {
		return fmt.Errorf("erro ao decodificar função JSON: %w", err)
	}

	fmt.Fprintf(stdout, "Função: %s (%s)\n", fn.FunctionName, fn.ImageName)
	fmt.Fprintf(stdout, "Status: %s | Estado: %s | Ativas: %d | Réplicas: %d/%d (maxReplicas=%d, maxConcurrency=%d, idleTimeout=%ds)\n",
		fn.Status, fn.ExecutionState, fn.ActiveRequests, fn.RunningReplicas, fn.TotalReplicas,
		fn.MaxReplicas, fn.MaxConcurrency, fn.IdleTimeoutSeconds)

	if len(fn.Replicas) == 0 {
		return nil
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RÉPLICA\tCONTAINER\tPORTA\tSTATUS\tATIVAS\tÚLTIMO USO")
	for _, rep := range fn.Replicas {
		port := rep.HostPort
		if port == "" {
			port = "-"
		}
		lastUsed := "-"
		if !rep.LastUsedAt.IsZero() {
			lastUsed = rep.LastUsedAt.Local().Format("02/01/2006 15:04:05")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
			rep.ReplicaName,
			rep.ContainerID,
			port,
			rep.Status,
			rep.ActiveRequests,
			lastUsed,
		)
	}
	return tw.Flush()
}
