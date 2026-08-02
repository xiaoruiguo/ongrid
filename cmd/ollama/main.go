package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	// "strings"
	// "time"
)

// ChatRequest 前端发送的请求结构
type ChatRequest struct {
	Message string `json:"message"`
}

// OllamaRequest 发送给 Ollama 的请求结构
type OllamaRequest struct {
	Model    string        `json:"model"`
	Messages []OllamaMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type OllamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// OllamaChunk Ollama 流式响应的每个 chunk
type OllamaChunk struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Message   struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Done   bool   `json:"done"`
	Error  string `json:"error,omitempty"`
}

func main() {
	// 提供静态文件服务（前端构建产物）
	fs := http.FileServer(http.Dir("../frontend/dist"))
	http.Handle("/", fs)

	// SSE 聊天接口
	http.HandleFunc("/api/chat", handleChat)
// 172.31.84.217
	log.Println("🚀 Server starting on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	// 1. 只接受 POST 请求
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 2. 解析请求体
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// 3. 设置 SSE 响应头[reference:4]
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// 4. 构建发送给 Ollama 的请求
	ollamaReq := OllamaRequest{
		Model: "glm4:9b", // 可改为你本地已下载的模型
		Messages: []OllamaMessage{
			{Role: "user", Content: req.Message},
		},
		Stream: true, // 启用流式[reference:5]
	}

	jsonData, err := json.Marshal(ollamaReq)
	if err != nil {
		sendSSEError(w, flusher, "Failed to marshal request")
		return
	}

	// 5. 调用 Ollama API
	ollamaResp, err := http.Post(
		"http://172.31.84.217:11434/api/chat",
		"application/json",
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		sendSSEError(w, flusher, "Failed to connect to Ollama")
		return
	}
	defer ollamaResp.Body.Close()

	// 6. 逐行读取 Ollama 的 NDJSON 流式响应[reference:6]
	scanner := bufio.NewScanner(ollamaResp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var chunk OllamaChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue // 跳过无法解析的行
		}

		// 如果出现错误[reference:7]
		if chunk.Error != "" {
			sendSSEError(w, flusher, chunk.Error)
			return
		}

		// 发送 SSE 事件[reference:8]
		content := chunk.Message.Content
		if content != "" {
			fmt.Fprintf(w, "data: %s\n\n", content)
			flusher.Flush()
		}

		// 如果是最后一个 chunk
		if chunk.Done {
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			break
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Scanner error: %v", err)
	}
}

// sendSSEError 发送 SSE 格式的错误
func sendSSEError(w http.ResponseWriter, flusher http.Flusher, msg string) {
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", msg)
	flusher.Flush()
}