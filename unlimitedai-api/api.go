package unlimitedai_api

import (
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"strings"
	"unlimitedai2api/common"
	"unlimitedai2api/common/config"
	logger "unlimitedai2api/common/loggger"
	"unlimitedai2api/cycletls"
)

const (
	baseURL      = "https://app.unlimitedai.chat"
	chatEndpoint = baseURL + "/api/chat"
)

func MakeStreamChatRequest(c *gin.Context, client cycletls.CycleTLS, jsonData []byte, cookie string, modelInfo common.ModelInfo) (<-chan cycletls.SSEResponse, error) {
	split := strings.Split(cookie, "=")
	if len(split) >= 2 {
		cookie = split[0]
	}

	// 获取最新的令牌
	token, err := GetUnlimitedAIToken(c, client)
	if err != nil {
		logger.Errorf(c, "Failed to get token: %v", err)
		return nil, fmt.Errorf("Failed to get authentication token: %v", err)
	}

	endpoint := chatEndpoint
	headers := map[string]string{
		"accept":             "*/*",
		"accept-language":    "zh-CN,zh;q=0.9,en;q=0.8",
		"content-type":       "application/json",
		"x-api-token":        token, // 使用新获取的令牌
		"origin":             "https://app.unlimitedai.chat",
		"priority":           "u=1, i",
		"referer":            "https://app.unlimitedai.chat/chat/" + uuid.New().String(),
		"sec-ch-ua":          "\"Google Chrome\";v=\"135\", \"Not-A.Brand\";v=\"8\", \"Chromium\";v=\"135\"",
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": "\"macOS\"",
		"sec-fetch-dest":     "empty",
		"sec-fetch-mode":     "cors",
		"sec-fetch-site":     "same-origin",
		"user-agent":         "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36",
	}

	// 如果有请求体中的对话ID，添加到referer中
	var requestBody map[string]interface{}
	if err := json.Unmarshal(jsonData, &requestBody); err == nil {
		if chatID, ok := requestBody["id"].(string); ok && chatID != "" {
			headers["referer"] = "https://app.unlimitedai.chat/chat/" + chatID
		}
	}

	options := cycletls.Options{
		Timeout: 10 * 60 * 60,
		Proxy:   config.ProxyUrl, // 在每个请求中设置代理
		Body:    string(jsonData),
		Method:  "POST",
		Headers: headers,
	}

	logger.Debug(c.Request.Context(), fmt.Sprintf("cookie: %v", cookie))

	sseChan, err := client.DoSSE(endpoint, options, "POST")
	if err != nil {
		logger.Errorf(c, "Failed to make stream request: %v", err)
		return nil, fmt.Errorf("failed to make stream request: %v", err)
	}
	return sseChan, nil
}

// GetUnlimitedAIToken 获取最新的认证令牌
func GetUnlimitedAIToken(c *gin.Context, client cycletls.CycleTLS) (string, error) {
	tokenEndpoint := baseURL + "/api/token"

	headers := map[string]string{
		"accept":             "*/*",
		"accept-language":    "zh-CN,zh;q=0.9",
		"priority":           "u=1, i",
		"referer":            "https://app.unlimitedai.chat/chat/" + uuid.New().String(),
		"sec-ch-ua":          "\"Google Chrome\";v=\"135\", \"Not-A.Brand\";v=\"8\", \"Chromium\";v=\"135\"",
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": "\"macOS\"",
		"sec-fetch-dest":     "empty",
		"sec-fetch-mode":     "cors",
		"sec-fetch-site":     "same-origin",
		"user-agent":         "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36",
	}

	options := cycletls.Options{
		Timeout: 30, // 30秒超时
		Proxy:   config.ProxyUrl,
		Method:  "GET",
		Headers: headers,
	}

	response, err := client.Do(tokenEndpoint, options, "GET")
	if err != nil {
		logger.Errorf(c, "Failed to get token: %v", err)
		return "", fmt.Errorf("failed to get token: %v", err)
	}

	if response.Status != 200 {
		logger.Errorf(c, "Failed to get token, status: %d, body: %s", response.Status, response.Body)
		return "", fmt.Errorf("failed to get token, status: %d", response.Status)
	}

	// 解析响应获取token
	var tokenResponse struct {
		Token string `json:"token"`
	}

	if err := json.Unmarshal([]byte(response.Body), &tokenResponse); err != nil {
		logger.Errorf(c, "Failed to parse token response: %v", err)
		return "", fmt.Errorf("failed to parse token response: %v", err)
	}

	if tokenResponse.Token == "" {
		logger.Error(c, "Empty token received")
		return "", fmt.Errorf("empty token received")
	}

	logger.Debug(c.Request.Context(), fmt.Sprintf("Got token: %s", tokenResponse.Token))
	return tokenResponse.Token, nil
}
