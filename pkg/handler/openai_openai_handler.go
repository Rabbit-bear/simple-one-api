package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/sashabaranov/go-openai"
	"go.uber.org/zap"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"simple-one-api/pkg/adapter"
	"simple-one-api/pkg/config"
	"simple-one-api/pkg/mylog"
	"simple-one-api/pkg/utils"
	"strings"
)

func formatAzureURL(inputURL string) (string, error) {
	// 解析URL
	parsedURL, err := url.Parse(inputURL)
	if err != nil {
		return "", err
	}

	// 构建新的URL
	formattedURL := &url.URL{
		Scheme: parsedURL.Scheme,
		Host:   parsedURL.Host,
	}

	return formattedURL.String(), nil
}

// validateAndFormatURL checks if the given URL matches the specified formats and returns the formatted URL
func validateAndFormatURL(rawurl string) (string, bool) {
	parsedURL, err := url.Parse(rawurl)
	if err != nil {
		return "", false
	}

	re := regexp.MustCompile(`/v([1-9]|[1-4][0-9]|50)(/chat/completions|/)?$`)
	if re.MatchString(parsedURL.Path) {
		if submatch := re.FindStringSubmatch(parsedURL.Path); submatch[2] == "/chat/completions" {
			formattedURL := fmt.Sprintf("%s://%s%s", parsedURL.Scheme, parsedURL.Host, parsedURL.Path[:len(parsedURL.Path)-len("/chat/completions")])
			return formattedURL, true
		}
		return rawurl, true
	}
	return rawurl, false
}

// getDefaultServerURL returns the default server URL based on the model prefix
func getDefaultServerURL(model string) string {
	model = strings.ToLower(model)
	switch {
	case strings.HasPrefix(model, "glm-"):
		return "https://open.bigmodel.cn/api/paas/v4/chat/completions"
	case strings.HasPrefix(model, "deepseek-"):
		return "https://api.deepseek.com/v1"
	case strings.HasPrefix(model, "yi-"):
		return "https://api.lingyiwanwu.com/v1/chat/completions"
	default:
		return ""
	}
}

// getConfig generates the OpenAI client configuration based on model details and request
func getConfig(s *config.ModelDetails, req openai.ChatCompletionRequest) (openai.ClientConfig, error) {
	apiKey := s.Credentials[config.KEYNAME_API_KEY]
	conf := openai.DefaultConfig(apiKey)

	serverURL := s.ServerURL
	if serverURL == "" {
		serverURL = getDefaultServerURL(req.Model)
		mylog.Logger.Info("Using default server URL",
			zap.String("server_url", serverURL)) // 记录默认服务器 URL
	}

	if serverURL != "" {
		if formattedURL, ok := validateAndFormatURL(serverURL); ok {
			conf.BaseURL = formattedURL

			mylog.Logger.Info("Formatted server URL is valid",
				zap.String("formatted_url", formattedURL)) // 记录格式化后的服务器 URL 是否有效
		} else {
			return conf, errors.New("formatted server URL is invalid")
		}
	} else {
		return conf, errors.New("server URL is empty")
	}

	return conf, nil
}

// handleOpenAIRequest handles OpenAI requests, supporting both streaming and non-streaming modes
func handleOpenAIOpenAIRequest(conf openai.ClientConfig, c *gin.Context, req openai.ChatCompletionRequest) error {
	openaiClient := openai.NewClientWithConfig(conf)
	ctx := context.Background()

	if req.Stream {
		return handleOpenAIOpenAIStreamRequest(c, openaiClient, ctx, req)
	}

	return handleOpenAIStandardRequest(c, openaiClient, ctx, req)
}

// handleStreamRequest handles streaming OpenAI requests
func handleOpenAIOpenAIStreamRequest(c *gin.Context, client *openai.Client, ctx context.Context, req openai.ChatCompletionRequest) error {
	utils.SetEventStreamHeaders(c)
	stream, err := client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		mylog.Logger.Error("An error occurred",
			zap.Error(err))
		return fmt.Errorf("ChatCompletionStream error: %w", err)
	}
	defer stream.Close()

	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			mylog.Logger.Error("An error occurred",
				zap.Error(err))
			return err
		}

		response.Model = req.Model
		respData, err := json.Marshal(&response)
		if err != nil {
			mylog.Logger.Error("An error occurred",
				zap.Error(err))
			return err
		}

		mylog.Logger.Info("Response data",
			zap.String("resp_data", string(respData))) // 记录响应数据

		_, err = c.Writer.WriteString("data: " + string(respData) + "\n\n")
		if err != nil {
			mylog.Logger.Error("An error occurred",
				zap.Error(err))
			return err
		}
		c.Writer.(http.Flusher).Flush()
	}
}

// handleStandardRequest handles non-streaming OpenAI requests
func handleOpenAIStandardRequest(c *gin.Context, client *openai.Client, ctx context.Context, req openai.ChatCompletionRequest) error {
	resp, err := client.CreateChatCompletion(ctx, req)
	if err != nil {
		mylog.Logger.Error("An error occurred",
			zap.Any("req", req),
			zap.Error(err))
		return err
	}

	myResp := adapter.OpenAIResponseToOpenAIResponse(&resp)
	myResp.Model = req.Model

	respJsonStr, err := json.Marshal(*myResp)
	if err != nil {
		mylog.Logger.Error("An error occurred",
			zap.Error(err)) // 记录错误对象
	}

	mylog.Logger.Info("Response JSON String",
		zap.String("resp_json_str", string(respJsonStr))) // 记录响应 JSON 字符串

	c.JSON(http.StatusOK, myResp)
	return nil
}

// OpenAI2OpenAIHandler handles OpenAI to OpenAI requests
func OpenAI2OpenAIHandler(c *gin.Context, s *config.ModelDetails, req openai.ChatCompletionRequest) error {
	conf, err := getConfig(s, req)
	if err != nil {
		return err
	}

	if strings.HasPrefix(s.ServerURL, "https://api.groq.com/openai/v1") {
		adjustGroqReq(&req)
	}

	return handleOpenAIOpenAIRequest(conf, c, req)
}

// getAzureConfig generates the OpenAI client configuration for Azure based on model details and request
func getAzureConfig(s *config.ModelDetails) (openai.ClientConfig, error) {
	apiKey := s.Credentials[config.KEYNAME_API_KEY]
	serverURL, err := formatAzureURL(s.ServerURL)
	if err != nil {
		serverURL = s.ServerURL
	}
	conf := openai.DefaultAzureConfig(apiKey, serverURL)

	if s.ServerURL == "" {
		return conf, errors.New("server URL is empty")
	}

	return conf, nil
}

// OpenAI2AzureOpenAIHandler handles OpenAI to Azure OpenAI requests
func OpenAI2AzureOpenAIHandler(c *gin.Context, s *config.ModelDetails, req openai.ChatCompletionRequest) error {
	conf, err := getAzureConfig(s)
	if err != nil {
		return err
	}
	return handleOpenAIOpenAIRequest(conf, c, req)
}
