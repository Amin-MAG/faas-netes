package handlers

import (
	"encoding/json"
	providertypes "github.com/Amin-MAG/faas-provider/types"
	"log"
	"net/http"
)

func MakeFlowsHandler(flows providertypes.Flows) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		jsonResp, err := json.Marshal(flows)
		if err != nil {
			log.Fatalf("Error happened in JSON marshal. Err: %s", err)
		}
		w.Write(jsonResp)
		return
	}
}
