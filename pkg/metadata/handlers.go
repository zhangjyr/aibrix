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

package metadata

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

type httpServer struct {
	redisClient *redis.Client
	cache       *cache.Cache
}

func NewHTTPServer(addr string, redis *redis.Client) *http.Server {
	c, err := cache.GetCache()
	if err != nil {
		panic(err)
	}

	server := &httpServer{
		redisClient: redis,
		cache:       c,
	}
	r := mux.NewRouter()
	// User related handlers
	r.HandleFunc("/CreateUser", server.createUser).Methods("POST")
	r.HandleFunc("/ReadUser", server.readUser).Methods("POST")
	r.HandleFunc("/UpdateUser", server.updateUser).Methods("POST")
	r.HandleFunc("/DeleteUser", server.deleteUser).Methods("POST")
	// OpenAI API related handlers
	r.HandleFunc("/v1/models", server.models).Methods("GET")

	return &http.Server{
		Addr:    addr,
		Handler: r,
	}
}

// models returns base and lora adapters registered to aibrix control plane
func (s *httpServer) models(w http.ResponseWriter, r *http.Request) {
	modelNames := s.cache.GetModels()
	response := BuildModelsResponse(modelNames)
	jsonBytes, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "error in processing model list", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "%s", string(jsonBytes))
}

func (s *httpServer) createUser(w http.ResponseWriter, r *http.Request) {
	var u utils.User

	err := decodeJSONBody(w, r, &u)
	if err != nil {
		var mr *malformedRequest
		if errors.As(err, &mr) {
			http.Error(w, mr.msg, mr.status)
		} else {
			klog.Info(err.Error())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}

	if utils.CheckUser(r.Context(), u, s.redisClient) {
		fmt.Fprintf(w, "User: %+v exists", u.Name)
		return
	}

	if err := utils.SetUser(r.Context(), u, s.redisClient); err != nil {
		http.Error(w, fmt.Sprintf("error occurred on creating user: %+v", err), http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, "Created User: %+v", u)
}

func (s *httpServer) readUser(w http.ResponseWriter, r *http.Request) {
	var u utils.User

	err := decodeJSONBody(w, r, &u)
	if err != nil {
		var mr *malformedRequest
		if errors.As(err, &mr) {
			http.Error(w, mr.msg, mr.status)
		} else {
			klog.Info(err.Error())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}

	user, err := utils.GetUser(r.Context(), u, s.redisClient)
	if err != nil {
		fmt.Fprint(w, "user does not exists")
		return
	}

	fmt.Fprintf(w, "User: %+v", user)
}

func (s *httpServer) updateUser(w http.ResponseWriter, r *http.Request) {
	var u utils.User

	err := decodeJSONBody(w, r, &u)
	if err != nil {
		var mr *malformedRequest
		if errors.As(err, &mr) {
			http.Error(w, mr.msg, mr.status)
		} else {
			klog.Info(err.Error())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}

	if !utils.CheckUser(r.Context(), u, s.redisClient) {
		fmt.Fprintf(w, "User: %+v does not exists", u.Name)
		return
	}

	if err := utils.SetUser(r.Context(), u, s.redisClient); err != nil {
		http.Error(w, fmt.Sprintf("error occurred on updating user: %+v", err), http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, "Updated User: %+v", u)
}

func (s *httpServer) deleteUser(w http.ResponseWriter, r *http.Request) {
	var u utils.User

	err := decodeJSONBody(w, r, &u)
	if err != nil {
		var mr *malformedRequest
		if errors.As(err, &mr) {
			http.Error(w, mr.msg, mr.status)
		} else {
			klog.Info(err.Error())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}

	if !utils.CheckUser(r.Context(), u, s.redisClient) {
		fmt.Fprintf(w, "User: %+v does not exists", u.Name)
		return
	}

	if err := utils.DelUser(r.Context(), u, s.redisClient); err != nil {
		http.Error(w, fmt.Sprintf("error occurred on deleting user: %+v", err), http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, "Deleted User: %+v", u)
}
