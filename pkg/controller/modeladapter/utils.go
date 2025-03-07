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

package modeladapter

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/vllm-project/aibrix/pkg/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	aSet := make(map[string]struct{}, len(a))
	for _, item := range a {
		aSet[item] = struct{}{}
	}

	for _, item := range b {
		if _, exists := aSet[item]; !exists {
			return false
		}
	}

	return true
}

// getEnvKey retrieves the value of the environment variable named by the key.
// If the variable is present, the function returns the value and a boolean true.
// If the variable is not present, the function returns an empty string and a boolean false.
func getEnvKey(key string) (string, bool) {
	value, exists := os.LookupEnv(key)
	return value, exists
}

func extractHuggingFacePath(artifactURL string) (string, error) {
	parsedURL, err := url.Parse(artifactURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse URL: %w", err)
	}

	// Check if the scheme is "huggingface"
	if parsedURL.Scheme != "huggingface" {
		return "", errors.New("unsupported protocol, only huggingface:// is allowed")
	}

	// Extract the path part (xxx/yyy) and trim any leading slashes
	// TODO: replace url.Parse with something else.
	path := strings.TrimPrefix(parsedURL.Host, "/") + parsedURL.Path

	if path == "" {
		return "", errors.New("invalid huggingface path, path cannot be empty")
	}

	return path, nil
}

func StringInSlice(slice []string, str string) bool {
	for _, v := range slice {
		if v == str {
			return true
		}
	}
	return false
}

// RemoveInstanceFromList removes a string from a slice of strings
func RemoveInstanceFromList(slice []string, strToRemove string) []string {
	var result []string
	for _, s := range slice {
		if s != strToRemove {
			result = append(result, s)
		}
	}
	return result
}

// NewCondition creates a new condition.
func NewCondition(condType string, status metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            msg,
	}
}

func BuildURLs(podIP string, config config.RuntimeConfig) URLConfig {
	var host string
	if config.DebugMode {
		host = fmt.Sprintf("http://%s:%s", "localhost", DefaultDebugInferenceEnginePort)
	} else if config.EnableRuntimeSidecar {
		host = fmt.Sprintf("http://%s:%s", podIP, DefaultRuntimeAPIPort)
	} else {
		host = fmt.Sprintf("http://%s:%s", podIP, DefaultInferenceEnginePort)
	}

	apiPath := ModelListPath
	loadPath := LoadLoraAdapterPath
	unloadPath := UnloadLoraAdapterPath
	if config.EnableRuntimeSidecar {
		apiPath = ModelListRuntimeAPIPath
		loadPath = LoadLoraRuntimeAPIPath
		unloadPath = UnloadLoraRuntimeAPIPath
	}

	return URLConfig{
		BaseURL:          host,
		ListModelsURL:    fmt.Sprintf("%s%s", host, apiPath),
		LoadAdapterURL:   fmt.Sprintf("%s%s", host, loadPath),
		UnloadAdapterURL: fmt.Sprintf("%s%s", host, unloadPath),
	}
}
