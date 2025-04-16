package kilo_api

import (
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
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

	endpoint := chatEndpoint
	headers := map[string]string{
		"accept":          "*/*",
		"accept-language": "zh-CN,zh;q=0.9,en;q=0.8",
		"content-type":    "application/json",
		//"cookie":             cookie,
		"origin":             "https://app.unlimitedai.chat",
		"priority":           "u=1, i",
		"referer":            "https://app.unlimitedai.chat/chat/",
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
		return nil, fmt.Errorf("Failed to make stream request: %v", err)
	}
	return sseChan, nil
}
