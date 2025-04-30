package controller

import (
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/samber/lo"
	"io"
	"net/http"
	"strings"
	"time"
	"unlimitedai2api/common"
	"unlimitedai2api/common/config"
	logger "unlimitedai2api/common/loggger"
	"unlimitedai2api/cycletls"
	"unlimitedai2api/model"
	"unlimitedai2api/unlimitedai-api"
)

const (
	errServerErrMsg  = "Service Unavailable"
	responseIDFormat = "chatcmpl-%s"
)

// ChatForOpenAI @Summary OpenAI对话接口
// @Description OpenAI对话接口
// @Tags OpenAI
// @Accept json
// @Produce json
// @Param req body model.OpenAIChatCompletionRequest true "OpenAI对话请求"
// @Param Authorization header string true "Authorization API-KEY"
// @Router /v1/chat/completions [post]
func ChatForOpenAI(c *gin.Context) {
	client := cycletls.Init()
	defer safeClose(client)

	var openAIReq model.OpenAIChatCompletionRequest
	if err := c.BindJSON(&openAIReq); err != nil {
		logger.Errorf(c.Request.Context(), err.Error())
		c.JSON(http.StatusInternalServerError, model.OpenAIErrorResponse{
			OpenAIError: model.OpenAIError{
				Message: "Invalid request parameters",
				Type:    "request_error",
				Code:    "500",
			},
		})
		return
	}

	openAIReq.RemoveEmptyContentMessages()

	modelInfo, b := common.GetModelInfo(openAIReq.Model)
	if !b {
		c.JSON(http.StatusBadRequest, model.OpenAIErrorResponse{
			OpenAIError: model.OpenAIError{
				Message: fmt.Sprintf("Model %s not supported", openAIReq.Model),
				Type:    "invalid_request_error",
				Code:    "invalid_model",
			},
		})
		return
	}
	if openAIReq.MaxTokens > modelInfo.MaxTokens {
		c.JSON(http.StatusBadRequest, model.OpenAIErrorResponse{
			OpenAIError: model.OpenAIError{
				Message: fmt.Sprintf("Max tokens %d exceeds limit %d", openAIReq.MaxTokens, modelInfo.MaxTokens),
				Type:    "invalid_request_error",
				Code:    "invalid_max_tokens",
			},
		})
		return
	}

	if openAIReq.Stream {
		handleStreamRequest(c, client, openAIReq, modelInfo)
	} else {
		handleNonStreamRequest(c, client, openAIReq, modelInfo)
	}
}

func handleNonStreamRequest(c *gin.Context, client cycletls.CycleTLS, openAIReq model.OpenAIChatCompletionRequest, modelInfo common.ModelInfo) {
	ctx := c.Request.Context()
	cookieManager := config.NewCookieManager()
	maxRetries := len(cookieManager.Cookies)
	cookie, err := cookieManager.GetRandomCookie()
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	for attempt := 0; attempt < maxRetries; attempt++ {
		requestBody, err := createRequestBody(c, &openAIReq, modelInfo)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}

		jsonData, err := json.Marshal(requestBody)
		if err != nil {
			c.JSON(500, gin.H{"error": "Failed to marshal request body"})
			return
		}
		sseChan, err := unlimitedai_api.MakeStreamChatRequest(c, client, jsonData, cookie, modelInfo)
		if err != nil {
			logger.Errorf(ctx, "MakeStreamChatRequest err on attempt %d: %v", attempt+1, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		isRateLimit := false
		var delta string
		var assistantMsgContent string
		var shouldContinue bool
		thinkStartType := new(bool)
		thinkEndType := new(bool)
	SSELoop:
		for response := range sseChan {
			data := response.Data
			if data == "" {
				continue
			}
			if response.Done {
				switch {
				case common.IsUsageLimitExceeded(data):
					if config.CheatEnabled {
						split := strings.Split(cookie, "=")
						if len(split) == 2 {
							cookieSession := split[1]
							cheatResp, err := client.Do(config.CheatUrl, cycletls.Options{
								Timeout: 10 * 60 * 60,
								Proxy:   config.ProxyUrl, // 在每个请求中设置代理
								Body:    "",
								Headers: map[string]string{
									"Cookie": cookieSession,
								},
							}, "POST")
							if err != nil {
								logger.Errorf(ctx, "Cheat err Cookie: %s err: %v", cookie, err)
								c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
								return
							}
							if cheatResp.Status == 200 {
								logger.Debug(c, fmt.Sprintf("Cheat Success Cookie: %s", cookie))
								attempt-- // 抵消循环结束时的attempt++
								break SSELoop
							}
							if cheatResp.Status == 402 {
								logger.Warnf(ctx, "Cookie Unlink Card Cookie: %s", cookie)
							} else {
								logger.Errorf(ctx, "Cheat err Cookie: %s Resp: %v", cookie, cheatResp.Body)
								c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Cheat Resp.Status:%v Resp.Body:%v", cheatResp.Status, cheatResp.Body)})
								return
							}
						}
					}

					isRateLimit = true
					logger.Warnf(ctx, "Cookie Usage limit exceeded, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
					config.RemoveCookie(cookie)
					break SSELoop
				case common.IsServerError(data):
					logger.Errorf(ctx, errServerErrMsg)
					c.JSON(http.StatusInternalServerError, gin.H{"error": errServerErrMsg})
					return
				case common.IsNotLogin(data):
					isRateLimit = true
					logger.Warnf(ctx, "Cookie Not Login, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
					break SSELoop
				case common.IsRateLimit(data):
					isRateLimit = true
					logger.Warnf(ctx, "Cookie rate limited, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
					config.AddRateLimitCookie(cookie, time.Now().Add(time.Duration(config.RateLimitCookieLockDuration)*time.Second))
					break SSELoop
				}
				logger.Warnf(ctx, response.Data)
				return
			}

			logger.Debug(ctx, strings.TrimSpace(data))

			streamDelta, streamShouldContinue := processNoStreamData(c, data, modelInfo, thinkStartType, thinkEndType)
			delta = streamDelta
			shouldContinue = streamShouldContinue
			// 处理事件流数据
			if !shouldContinue {
				promptTokens := model.CountTokenText(string(jsonData), openAIReq.Model)
				completionTokens := model.CountTokenText(assistantMsgContent, openAIReq.Model)
				finishReason := "stop"

				c.JSON(http.StatusOK, model.OpenAIChatCompletionResponse{
					ID:      fmt.Sprintf(responseIDFormat, time.Now().Format("20060102150405")),
					Object:  "chat.completion",
					Created: time.Now().Unix(),
					Model:   openAIReq.Model,
					Choices: []model.OpenAIChoice{{
						Message: model.OpenAIMessage{
							Role:    "assistant",
							Content: assistantMsgContent,
						},
						FinishReason: &finishReason,
					}},
					Usage: model.OpenAIUsage{
						PromptTokens:     promptTokens,
						CompletionTokens: completionTokens,
						TotalTokens:      promptTokens + completionTokens,
					},
				})

				return
			} else {
				assistantMsgContent = assistantMsgContent + delta
			}
		}
		if !isRateLimit {
			return
		}

		// 获取下一个可用的cookie继续尝试
		cookie, err = cookieManager.GetNextCookie()
		if err != nil {
			logger.Errorf(ctx, "No more valid cookies available after attempt %d", attempt+1)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

	}
	logger.Errorf(ctx, "All cookies exhausted after %d attempts", maxRetries)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "All cookies are temporarily unavailable."})
	return
}

func createRequestBody(c *gin.Context, openAIReq *model.OpenAIChatCompletionRequest, modelInfo common.ModelInfo) (map[string]interface{}, error) {

	client := cycletls.Init()
	defer safeClose(client)

	if config.PRE_MESSAGES_JSON != "" {
		err := openAIReq.PrependMessagesFromJSON(config.PRE_MESSAGES_JSON)
		if err != nil {
			return nil, fmt.Errorf("PrependMessagesFromJSON err: %v JSON:%s", err, config.PRE_MESSAGES_JSON)
		}
	}

	if openAIReq.MaxTokens <= 1 {
		openAIReq.MaxTokens = 8000
	}

	// 创建兼容格式的请求体
	requestID := uuid.New().String()

	// 转换消息格式
	messages := make([]map[string]interface{}, 0, len(openAIReq.Messages))
	for _, msg := range openAIReq.Messages {
		messageID := uuid.New().String()
		createdAt := time.Now().Format(time.RFC3339Nano)

		// 创建消息内容
		content := ""
		if textContent, ok := msg.Content.(string); ok {
			content = textContent
		} else if contentList, ok := msg.Content.([]interface{}); ok {
			// 处理多部分内容
			var textParts []string
			for _, part := range contentList {
				if partMap, ok := part.(map[string]interface{}); ok {
					if text, exists := partMap["text"].(string); exists {
						textParts = append(textParts, text)
					}
				}
			}
			content = strings.Join(textParts, "\n")
		}

		// 创建parts部分
		parts := []map[string]interface{}{
			{
				"type": "text",
				"text": content,
			},
		}

		// 构建消息对象
		message := map[string]interface{}{
			"id":        messageID,
			"createdAt": createdAt,
			"role":      msg.Role,
			"content":   content,
			"parts":     parts,
		}

		messages = append(messages, message)
	}

	// 构建完整请求体
	requestBody := map[string]interface{}{
		"id":                requestID,
		"messages":          messages,
		"selectedChatModel": "chat-model-reasoning",
	}

	// 如果需要添加其他参数
	if openAIReq.Temperature > 0 {
		requestBody["temperature"] = openAIReq.Temperature
	}

	if openAIReq.MaxTokens > 0 {
		requestBody["maxOutputTokens"] = openAIReq.MaxTokens
	}

	logger.Debug(c.Request.Context(), fmt.Sprintf("RequestBody: %v", requestBody))

	return requestBody, nil
}

// createStreamResponse 创建流式响应
func createStreamResponse(responseId, modelName string, jsonData []byte, delta model.OpenAIDelta, finishReason *string) model.OpenAIChatCompletionResponse {
	promptTokens := model.CountTokenText(string(jsonData), modelName)
	completionTokens := model.CountTokenText(delta.Content, modelName)
	return model.OpenAIChatCompletionResponse{
		ID:      responseId,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   modelName,
		Choices: []model.OpenAIChoice{
			{
				Index:        0,
				Delta:        delta,
				FinishReason: finishReason,
			},
		},
		Usage: model.OpenAIUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}
}

// handleDelta 处理消息字段增量
func handleDelta(c *gin.Context, delta string, responseId, modelName string, jsonData []byte) error {
	// 创建基础响应
	createResponse := func(content string) model.OpenAIChatCompletionResponse {
		return createStreamResponse(
			responseId,
			modelName,
			jsonData,
			model.OpenAIDelta{Content: content, Role: "assistant"},
			nil,
		)
	}

	// 发送基础事件
	var err error
	if err = sendSSEvent(c, createResponse(delta)); err != nil {
		return err
	}

	return err
}

// handleMessageResult 处理消息结果
func handleMessageResult(c *gin.Context, responseId, modelName string, jsonData []byte) bool {
	finishReason := "stop"
	var delta string

	promptTokens := 0
	completionTokens := 0

	streamResp := createStreamResponse(responseId, modelName, jsonData, model.OpenAIDelta{Content: delta, Role: "assistant"}, &finishReason)
	streamResp.Usage = model.OpenAIUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}

	if err := sendSSEvent(c, streamResp); err != nil {
		logger.Warnf(c.Request.Context(), "sendSSEvent err: %v", err)
		return false
	}
	c.SSEvent("", " [DONE]")
	return false
}

// sendSSEvent 发送SSE事件
func sendSSEvent(c *gin.Context, response model.OpenAIChatCompletionResponse) error {
	jsonResp, err := json.Marshal(response)
	if err != nil {
		logger.Errorf(c.Request.Context(), "Failed to marshal response: %v", err)
		return err
	}
	c.SSEvent("", " "+string(jsonResp))
	c.Writer.Flush()
	return nil
}

func handleStreamRequest(c *gin.Context, client cycletls.CycleTLS, openAIReq model.OpenAIChatCompletionRequest, modelInfo common.ModelInfo) {

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	responseId := fmt.Sprintf(responseIDFormat, time.Now().Format("20060102150405"))
	ctx := c.Request.Context()

	cookieManager := config.NewCookieManager()
	maxRetries := len(cookieManager.Cookies)
	cookie, err := cookieManager.GetRandomCookie()
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	thinkStartType := new(bool)
	thinkEndType := new(bool)

	c.Stream(func(w io.Writer) bool {
		for attempt := 0; attempt < maxRetries; attempt++ {
			requestBody, err := createRequestBody(c, &openAIReq, modelInfo)
			if err != nil {
				c.JSON(500, gin.H{"error": err.Error()})
				return false
			}

			jsonData, err := json.Marshal(requestBody)
			if err != nil {
				c.JSON(500, gin.H{"error": "Failed to marshal request body"})
				return false
			}
			sseChan, err := unlimitedai_api.MakeStreamChatRequest(c, client, jsonData, cookie, modelInfo)
			if err != nil {
				logger.Errorf(ctx, "MakeStreamChatRequest err on attempt %d: %v", attempt+1, err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return false
			}

			isRateLimit := false
		SSELoop:
			for response := range sseChan {

				if response.Status == 403 {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "Forbidden"})
					return false
				}

				data := response.Data
				if data == "" {
					continue
				}

				if response.Done {
					switch {
					case common.IsUsageLimitExceeded(data):
						isRateLimit = true
						logger.Warnf(ctx, "Cookie Usage limit exceeded, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
						config.RemoveCookie(cookie)
						break SSELoop
					case common.IsServerError(data):
						logger.Errorf(ctx, errServerErrMsg)
						c.JSON(http.StatusInternalServerError, gin.H{"error": errServerErrMsg})
						return false
					case common.IsNotLogin(data):
						isRateLimit = true
						logger.Warnf(ctx, "Cookie Not Login, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
						break SSELoop // 使用 label 跳出 SSE 循环
					case common.IsRateLimit(data):
						isRateLimit = true
						logger.Warnf(ctx, "Cookie rate limited, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
						config.AddRateLimitCookie(cookie, time.Now().Add(time.Duration(config.RateLimitCookieLockDuration)*time.Second))
						break SSELoop
					}
					logger.Warnf(ctx, response.Data)
					return false
				}

				logger.Debug(ctx, strings.TrimSpace(data))

				_, shouldContinue := processStreamData(c, data, responseId, openAIReq.Model, modelInfo, jsonData, thinkStartType, thinkEndType)
				// 处理事件流数据

				if !shouldContinue {
					return false
				}
			}

			if !isRateLimit {
				return true
			}

			// 获取下一个可用的cookie继续尝试
			cookie, err = cookieManager.GetNextCookie()
			if err != nil {
				logger.Errorf(ctx, "No more valid cookies available after attempt %d", attempt+1)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return false
			}
		}

		logger.Errorf(ctx, "All cookies exhausted after %d attempts", maxRetries)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "All cookies are temporarily unavailable."})
		return false
	})
}

// 处理流式数据的辅助函数，返回bool表示是否继续处理
func processStreamData(c *gin.Context, data, responseId, model string, modelInfo common.ModelInfo, jsonData []byte, thinkStartType, thinkEndType *bool) (string, bool) {
	data = strings.TrimSpace(data)
	data = strings.TrimPrefix(data, "data: ")

	// 处理[DONE]标记
	if data == "[DONE]" {
		return "", false
	}

	// 处理新格式的流数据
	if len(data) > 2 && data[1] == ':' {
		prefix := data[0:1]
		content := data[2:]

		// 去除内容前后的引号
		if len(content) >= 2 && content[0] == '"' && content[len(content)-1] == '"' {
			content = content[1 : len(content)-1]
		}

		// 处理转义字符
		content = strings.ReplaceAll(content, "&quot;", "\"") // 替换HTML转义的引号
		content = strings.ReplaceAll(content, "\\n", "\n")    // 替换转义的换行符
		content = strings.ReplaceAll(content, "\\\"", "\"")   // 替换转义的引号
		content = strings.ReplaceAll(content, "\\\\", "\\")   // 替换转义的反斜杠

		var text string

		switch prefix {
		case "0": // 实际回答内容
			// 如果之前有思考内容，现在切换到实际内容，需要关闭思考标签
			if *thinkStartType && !*thinkEndType {
				if config.ReasoningHide != 1 {
					text = "</think>\n\n" + content
				} else {
					text = content
				}
				*thinkStartType = false
				*thinkEndType = true
			} else {
				text = content
			}

			// 处理文本内容
			if err := handleDelta(c, text, responseId, model, jsonData); err != nil {
				logger.Errorf(c.Request.Context(), "handleDelta err: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return "", false
			}
			return text, true

		case "g": // 思考内容
			// 如果 ReasoningHide 为 1，则不返回思考内容
			if config.ReasoningHide == 1 {
				return "", true
			}

			// 如果是第一次收到思考内容
			if !*thinkStartType {
				text = "<think>\n\n" + content
				*thinkStartType = true
				*thinkEndType = false
			} else {
				text = content
			}

			// 处理思考内容
			if err := handleDelta(c, text, responseId, model, jsonData); err != nil {
				logger.Errorf(c.Request.Context(), "handleDelta for thinking err: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return "", false
			}
			return text, true

		case "e": // 结束消息
			// 如果思考没有正常结束，确保关闭思考标签
			if *thinkStartType && !*thinkEndType {
				if config.ReasoningHide != 1 {
					text = "</think>\n\n"
					if err := handleDelta(c, text, responseId, model, jsonData); err != nil {
						logger.Errorf(c.Request.Context(), "handleDelta for closing think tag err: %v", err)
						c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
						return "", false
					}
				}
				*thinkStartType = false
				*thinkEndType = true
			}

			// 对于e:消息，内容是JSON，不应处理转义字符
			var finishData map[string]interface{}
			if err := json.Unmarshal([]byte(content), &finishData); err == nil {
				if finishReason, ok := finishData["finishReason"]; ok && finishReason != nil && finishReason != "" {
					// 处理完成的消息
					handleMessageResult(c, responseId, model, jsonData)
					return "", false
				}
			}
			return "", true

		case "f", "d": // 消息ID和详细信息，不需要特别处理
			return "", true

		default:
			// 未知前缀，记录日志并继续
			logger.Warnf(c.Request.Context(), "Unknown message prefix: %s", prefix)
			return "", true
		}
	}

	// 如果不是新格式，记录日志并继续
	logger.Warnf(c.Request.Context(), "Unrecognized data format: %s", data)
	return "", true
}

func processNoStreamData(c *gin.Context, data string, modelInfo common.ModelInfo, thinkStartType *bool, thinkEndType *bool) (string, bool) {
	data = strings.TrimSpace(data)
	data = strings.TrimPrefix(data, "data: ")

	// 处理[DONE]标记
	if data == "[DONE]" {
		return "", false
	}

	// 处理新格式的流数据
	if len(data) > 2 && data[1] == ':' {
		prefix := data[0:1]
		content := data[2:]

		// 去除内容前后的引号
		if len(content) >= 2 && content[0] == '"' && content[len(content)-1] == '"' {
			content = content[1 : len(content)-1]
		}

		// 处理转义字符
		content = strings.ReplaceAll(content, "&quot;", "\"") // 替换HTML转义的引号
		content = strings.ReplaceAll(content, "\\n", "\n")    // 替换转义的换行符
		content = strings.ReplaceAll(content, "\\\"", "\"")   // 替换转义的引号
		content = strings.ReplaceAll(content, "\\\\", "\\")   // 替换转义的反斜杠

		var text string

		switch prefix {
		case "0": // 实际回答内容
			// 如果之前有思考内容，现在切换到实际内容，需要关闭思考标签
			if *thinkStartType && !*thinkEndType {
				if config.ReasoningHide != 1 {
					text = "</think>\n\n" + content
				} else {
					text = content
				}
				*thinkStartType = false
				*thinkEndType = true
			} else {
				text = content
			}

			// 处理文本内容
			return text, true

		case "g": // 思考内容
			// 如果 ReasoningHide 为 1，则不返回思考内容
			if config.ReasoningHide == 1 {
				return "", true
			}

			// 如果是第一次收到思考内容
			if !*thinkStartType {
				text = "<think>\n\n" + content
				*thinkStartType = true
				*thinkEndType = false
			} else {
				text = content
			}

			// 处理思考内容
			return text, true

		case "e": // 结束消息
			// 如果思考没有正常结束，确保关闭思考标签
			if *thinkStartType && !*thinkEndType {
				if config.ReasoningHide != 1 {
					text = "</think>\n\n"
				}
				*thinkStartType = false
				*thinkEndType = true
			}

			// 对于e:消息，内容是JSON，不应处理转义字符
			var finishData map[string]interface{}
			if err := json.Unmarshal([]byte(content), &finishData); err == nil {
				if finishReason, ok := finishData["finishReason"]; ok && finishReason != nil && finishReason != "" {
					// 处理完成的消息
					return "", false
				}
			}
			return "", true

		case "f", "d": // 消息ID和详细信息，不需要特别处理
			return "", true

		default:
			// 未知前缀，记录日志并继续
			logger.Warnf(c.Request.Context(), "Unknown message prefix: %s", prefix)
			return "", true
		}
	}

	// 如果不是新格式，记录日志并继续
	logger.Warnf(c.Request.Context(), "Unrecognized data format: %s", data)
	return "", true

}

// OpenaiModels @Summary OpenAI模型列表接口
// @Description OpenAI模型列表接口
// @Tags OpenAI
// @Accept json
// @Produce json
// @Param Authorization header string true "Authorization API-KEY"
// @Success 200 {object} common.ResponseResult{data=model.OpenaiModelListResponse} "成功"
// @Router /v1/models [get]
func OpenaiModels(c *gin.Context) {
	var modelsResp []string

	modelsResp = lo.Union(common.GetModelList())

	var openaiModelListResponse model.OpenaiModelListResponse
	var openaiModelResponse []model.OpenaiModelResponse
	openaiModelListResponse.Object = "list"

	for _, modelResp := range modelsResp {
		openaiModelResponse = append(openaiModelResponse, model.OpenaiModelResponse{
			ID:     modelResp,
			Object: "model",
		})
	}
	openaiModelListResponse.Data = openaiModelResponse
	c.JSON(http.StatusOK, openaiModelListResponse)
	return
}

func safeClose(client cycletls.CycleTLS) {
	if client.ReqChan != nil {
		close(client.ReqChan)
	}
	if client.RespChan != nil {
		close(client.RespChan)
	}
}

//
//func processUrl(c *gin.Context, client cycletls.CycleTLS, chatId, cookie string, url string) (string, error) {
//	// 判断是否为URL
//	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
//		// 下载文件
//		bytes, err := fetchImageBytes(url)
//		if err != nil {
//			logger.Errorf(c.Request.Context(), fmt.Sprintf("fetchImageBytes err  %v\n", err))
//			return "", fmt.Errorf("fetchImageBytes err  %v\n", err)
//		}
//
//		base64Str := base64.StdEncoding.EncodeToString(bytes)
//
//		finalUrl, err := processBytes(c, client, chatId, cookie, base64Str)
//		if err != nil {
//			logger.Errorf(c.Request.Context(), fmt.Sprintf("processBytes err  %v\n", err))
//			return "", fmt.Errorf("processBytes err  %v\n", err)
//		}
//		return finalUrl, nil
//	} else {
//		finalUrl, err := processBytes(c, client, chatId, cookie, url)
//		if err != nil {
//			logger.Errorf(c.Request.Context(), fmt.Sprintf("processBytes err  %v\n", err))
//			return "", fmt.Errorf("processBytes err  %v\n", err)
//		}
//		return finalUrl, nil
//	}
//}
//
//func fetchImageBytes(url string) ([]byte, error) {
//	resp, err := http.Get(url)
//	if err != nil {
//		return nil, fmt.Errorf("http.Get err: %v\n", err)
//	}
//	defer resp.Body.Close()
//
//	return io.ReadAll(resp.Body)
//}
//
//func processBytes(c *gin.Context, client cycletls.CycleTLS, chatId, cookie string, base64Str string) (string, error) {
//	// 检查类型
//	fileType := common.DetectFileType(base64Str)
//	if !fileType.IsValid {
//		return "", fmt.Errorf("invalid file type %s", fileType.Extension)
//	}
//	signUrl, err := unlimitedai-api.GetSignURL(client, cookie, chatId, fileType.Extension)
//	if err != nil {
//		logger.Errorf(c.Request.Context(), fmt.Sprintf("GetSignURL err  %v\n", err))
//		return "", fmt.Errorf("GetSignURL err: %v\n", err)
//	}
//
//	err = unlimitedai-api.UploadToS3(client, signUrl, base64Str, fileType.MimeType)
//	if err != nil {
//		logger.Errorf(c.Request.Context(), fmt.Sprintf("UploadToS3 err  %v\n", err))
//		return "", err
//	}
//
//	u, err := url.Parse(signUrl)
//	if err != nil {
//		return "", err
//	}
//
//	return fmt.Sprintf("%s://%s%s", u.Scheme, u.Host, u.Path), nil
//}
