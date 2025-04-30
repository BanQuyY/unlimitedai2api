package common

import "time"

var StartTime = time.Now().Unix() // unit: second
var Version = "v1.1.0"            // this hard coding will be replaced automatically when building, no need to manually change

type ModelInfo struct {
	Model     string
	MaxTokens int
}

// ModelRegistry 创建映射表（假设用 model 名称作为 key）
var ModelRegistry = map[string]ModelInfo{
	"chat-model-reasoning": {"chat-model-reasoning", 100000},
}

// 通过 model 名称查询的方法
func GetModelInfo(modelName string) (ModelInfo, bool) {
	info, exists := ModelRegistry[modelName]
	return info, exists
}

func GetModelList() []string {
	var modelList []string
	for k := range ModelRegistry {
		modelList = append(modelList, k)
	}
	return modelList
}
