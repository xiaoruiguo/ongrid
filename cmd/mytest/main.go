package main

import (
    "context"
    "fmt"
    "io"
    openai "github.com/sashabaranov/go-openai"
)

func main() {
    config := openai.DefaultConfig("ollama")          // API Key 可随意填
    config.BaseURL = "http://172.31.84.217:11434/v1"      // 指向你的 Ollama 服务
    client := openai.NewClientWithConfig(config)

	//chat completion
    // resp, err := client.CreateChatCompletion(
    //     context.Background(),
    //     openai.ChatCompletionRequest{
    //         Model: "glm4:9b",                 // Ollama 中的模型名
    //         Messages: []openai.ChatCompletionMessage{
    //             {Role: openai.ChatMessageRoleUser, Content: "用 Go 写一个快速排序"},
    //         },
    //     },
    // )
    // if err != nil {
    //     fmt.Printf("Error: %v\n", err)
    //     return
    // }
    // fmt.Println(resp.Choices[0].Message.Content)
	// 
	

	//stream completion
	ctx := context.Background()

stream, err := client.CreateChatCompletionStream(ctx, openai.ChatCompletionRequest{
    Model: "glm4:9b",
    Messages: []openai.ChatCompletionMessage{
        {Role: openai.ChatMessageRoleUser, Content: "讲一个笑话"},
    },
    Stream: true,
})
if err != nil {
    fmt.Println(err)
    return
}
defer stream.Close()

for {
    response, err := stream.Recv()
    if err == io.EOF {
        break
    }
    if err != nil {
        fmt.Println(err)
        break
    }
    fmt.Print(response.Choices[0].Delta.Content)   // 实时打印每个 token
}
}