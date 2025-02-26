/*
Copyright 2024 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
	"k8s.io/klog/v2"
)

// https://cookbook.openai.com/examples/how_to_count_tokens_with_tiktoken
const encoding = "cl100k_base"

func TokenizeInputText(text string) ([]int, error) {
	// if you don't want download dictionary at runtime, you can use offline loader
	tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	tke, err := tiktoken.GetEncoding(encoding)
	if err != nil {
		return nil, err
	}

	// encode
	token := tke.Encode(text, nil, nil)
	return token, nil
}

func DetokenizeText(tokenIds []int) (string, error) {
	tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	tke, err := tiktoken.GetEncoding(encoding)
	if err != nil {
		return "", fmt.Errorf("failed to get encoding: %v", err)
	}
	decoded := tke.Decode(tokenIds)
	return decoded, nil
}

type Message struct {
	Content string `json:"content"`
	Role    string `json:"role"`
}

func TrimMessage(message string) string {
	var messages []Message
	if err := json.Unmarshal([]byte(message), &messages); err != nil {
		// If array parsing fails, try single message
		var msg Message
		if err := json.Unmarshal([]byte(message), &msg); err != nil {
			return message
		}
		return msg.Content
	}
	if len(messages) > 0 {
		return messages[0].Content
	}
	return message
}

// LoadEnv loads an environment variable or returns a default value if not set.
func LoadEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		klog.Warningf("environment variable %s is not set, using default value: %s", key, defaultValue)
		return defaultValue
	}
	return value
}
