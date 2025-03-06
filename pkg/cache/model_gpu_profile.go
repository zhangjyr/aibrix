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

package cache

import (
	"encoding/json"
	"fmt"
	"math"
)

const ModelGPUNameTemplate = "aibrix:profile_%s_%s"

// Assuming indexes are of equal distances, the signature_tolerance
// control how a value in the middle of two indexes should aligned to a
// certain index. While 0.5 indicate neutual preference. 0.25 set
// threshold closer to lower index and then prefer higher index.
const SignatureTolerance = 0.5

type ModelGPUProfile struct {
	Deployment string      `json:"gpu"` // k8s deployment that specified model and GPU information.
	Cost       float64     `json:"cost"`
	Tputs      [][]float64 `json:"tputs"`   // Max RPS per correspondent index.
	Indexes    [][]float64 `json:"indexes"` // [output tokens, input tokens]
	Created    float64     `json:"created"`
	E2E        [][]float64 `json:"e2e"`  // Mean E2E latency per correspondent RPS.
	TTFT       [][]float64 `json:"ttft"` // Mean TTFT per correspondent RPS.
	SLOs       ModelSLOs   `json:"slos"` // SLOs used for specified model and GPU.
}

type ModelSLOs struct {
	Percentile int     `json:"percentile"`
	TPUT       float64 `json:"tput"` // Request Throughput: RPS
	TT         float64 `json:"tt"`   // Token Throughput
	E2E        float64 `json:"e2e"`  // End-to-end latency
	TTFT       float64 `json:"ttft"` // Time to first token
	TPAT       float64 `json:"tpat"` // Time per all tokens
	TPOT       float64 `json:"tpot"` // Time per output tokens
}

func ModelGPUProfileKey(modelName string, deploymentName string) string {
	return fmt.Sprintf(ModelGPUNameTemplate, modelName, deploymentName)
}

func (pf *ModelGPUProfile) Unmarshal(data []byte) error {
	err := json.Unmarshal(data, pf)
	if err != nil {
		return err
	}

	for i := 0; i < len(pf.Indexes); i++ {
		for j := 0; j < len(pf.Indexes[i]); j++ {
			pf.Indexes[i][j] = math.Log2(pf.Indexes[i][j])
		}
	}
	return nil
}

func (pf *ModelGPUProfile) GetSignature(features ...float64) ([]int, error) {
	if len(features) == 0 {
		return nil, fmt.Errorf("missing values on getting profile signature")
	}
	indexes := pf.Indexes

	formalizedVals := make([]float64, len(features))
	for i, val := range features {
		formalizedVals[i] = math.Log2(val)
	}

	ret := make([]int, len(features))
	for i, value := range formalizedVals {
		if len(indexes[i]) == 0 {
			return nil, fmt.Errorf("profile index size mismatch, at least 1")
		} else if len(indexes[i]) == 1 {
			// No need to assign index 0.
			continue
		}

		// Assuming indexes are ascending ordered.
		size := len(indexes[i]) - 1
		if value < indexes[i][0] {
			ret[i] = 0
		} else if value > indexes[i][size-1] {
			ret[i] = size - 1
		} else {
			// Find the index using binary search.
			left, right := 0, size-1
			found := false
			for left < right-1 {
				mid := (left + right) / 2
				if value < indexes[i][mid] {
					right = mid
				} else if value > indexes[i][mid] {
					left = mid
				} else {
					ret[i] = mid
					found = true
				}
				break
			}
			if !found {
				if value < indexes[i][left]+(indexes[i][right]-indexes[i][left])*SignatureTolerance {
					ret[i] = left
				} else {
					ret[i] = right
				}
			}
		}
	}
	return ret, nil
}

func (pf *ModelGPUProfile) getValue(ref [][]float64, signature ...int) (float64, error) {
	if len(signature) < 2 {
		return 0.0, fmt.Errorf("too few signature dimensions: %v", signature)
	} else if signature[0] >= len(ref) || signature[1] >= len(ref[signature[0]]) {
		return 0.0, fmt.Errorf("signature out of bound: %v", signature)
	}

	return ref[signature[0]][signature[1]], nil
}

func (pf *ModelGPUProfile) ThroughputRPS(signature ...int) (float64, error) {
	return pf.getValue(pf.Tputs, signature...)
}

func (pf *ModelGPUProfile) LatencySeconds(signature ...int) (float64, error) {
	return pf.getValue(pf.E2E, signature...)
}

func (pf *ModelGPUProfile) TTFTSeconds(signature ...int) (float64, error) {
	return pf.getValue(pf.TTFT, signature...)
}
