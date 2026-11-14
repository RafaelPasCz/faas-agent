package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

type DeployRequest struct {
	FunctionName string `json:"functionName"`
	ImageName    string `json:"imageName"`
}

func deployHandler(w http.ResponseWriter, r *http.Request) {

	if r.Method != http.MethodPost {
		http.Error(w, "Apenas o método POST é permitido", http.StatusBadRequest)
		return
	}

	var req DeployRequest

	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}

	logMsg := fmt.Sprintf("-> [Deploy] Solicitado deploy da função '%s' com a imagem '%s'", req.FunctionName, req.ImageName)
	fmt.Println(logMsg)

	ctx := context.Background()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		http.Error(w, "Erro ao conectar no Docker: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer cli.Close()

	fmt.Println("-> [Docker] Baixando imagem...")
	reader, err := cli.ImagePull(ctx, req.ImageName, image.PullOptions{})
	if err != nil {
		http.Error(w, "Erro ao baixar imagem: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	io.Copy(os.Stdout, reader)

	fmt.Println("\n-> [Docker] Criando container...")
	resp, err := cli.ContainerCreate(ctx, &container.Config{
		Image: req.ImageName,
	}, nil, nil, nil, req.FunctionName)
	if err != nil {
		http.Error(w, "Erro ao criar container: "+err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Println("-> [Docker] Iniciando container...")
	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		http.Error(w, "Erro ao iniciar container: "+err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Printf("-> [Sucesso] Função '%s' rodando! Container ID: %s\n", req.FunctionName, resp.ID[:12])

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Deploy concluído! Container ID: %s\n", resp.ID[:12])

}

func main() {
	http.HandleFunc("/deploy", deployHandler)

	fmt.Println("Faas-Agent rodando na porta 8081...")
	if err := http.ListenAndServe(":8081", nil); err != nil {
		log.Fatal("Erro ao iniciar o servidor: ", err)
	}
}
