package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	templatesDir = "templates"
	functionsDir = "functions"
)

//go:embed all:templates
var embeddedTemplates embed.FS

func runCLI(args []string) {
	switch args[0] {
	case "new":
		if len(args) != 3 {
			fmt.Fprintf(os.Stderr, "Uso: faas-agent new <template> <nome-da-funcao>\n")
			printAvailableTemplates(os.Stderr)
			os.Exit(1)
		}
		templateName := args[1]
		functionName := args[2]
		if err := scaffoldFunction(templateName, functionName); err != nil {
			fmt.Fprintf(os.Stderr, "Erro: %v\n", err)
			os.Exit(1)
		}
		destDir := filepath.Join(functionsDir, functionName)
		fmt.Printf("Função '%s' criada em '%s/' a partir do template '%s'.\n", functionName, destDir, templateName)
		fmt.Printf("Próximos passos:\n")
		fmt.Printf("  1. Edite o código em %s/function/handler.py\n", destDir)
		fmt.Printf("  2. Gere a imagem Docker:\n")
		fmt.Printf("     docker build -t %s:latest ./%s\n", functionName, destDir)

	case "templates":
		printAvailableTemplates(os.Stdout)

	case "help", "-h", "--help":
		printCLIHelp()

	default:
		fmt.Fprintf(os.Stderr, "Subcomando desconhecido: '%s'\n\n", args[0])
		printCLIHelp()
		os.Exit(1)
	}
}

func printCLIHelp() {
	fmt.Println("Uso:")
	fmt.Println("  faas-agent                              Inicia o servidor HTTP na porta :8081")
	fmt.Println("  faas-agent new <template> <funcao>      Cria uma nova função em functions/<funcao>")
	fmt.Println("  faas-agent templates                    Lista os templates disponíveis")
	fmt.Println("  faas-agent help                         Exibe esta ajuda")
}

func printAvailableTemplates(out *os.File) {
	names := listAvailableTemplates()
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
	if entries, err := fs.ReadDir(embeddedTemplates, embeddedPath); err == nil && len(entries) >= 0 {
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
