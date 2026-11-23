/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package tenant

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type errorBody struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, errorType, message string) {
	payload, err := json.Marshal(errorBody{Status: "error", ErrorType: errorType, Error: message})
	if err != nil {
		http.Error(w, http.StatusText(status), status)
		return
	}
	writeRaw(w, status, string(payload))
}

func writeNotImplemented(w http.ResponseWriter, path string) {
	writeError(w, http.StatusNotImplemented, "not_implemented",
		fmt.Sprintf("the endpoint %s is not supported by this proxy", path))
}

func writeRaw(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
