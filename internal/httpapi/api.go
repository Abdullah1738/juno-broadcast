package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Abdullah1738/juno-broadcast/internal/broadcast"
)

type Broadcaster interface {
	Submit(ctx context.Context, rawTxHex string) (string, error)
	Status(ctx context.Context, txid string) (broadcast.TxStatus, bool, error)
	WaitForConfirmations(ctx context.Context, txid string, confirmations int64) (broadcast.TxStatus, error)
}

type API struct {
	bc           Broadcaster
	maxBodyBytes int64
}

type Option func(*API)

func WithMaxBodyBytes(n int64) Option {
	return func(a *API) {
		if n > 0 {
			a.maxBodyBytes = n
		}
	}
}

func New(bc Broadcaster, opts ...Option) (*API, error) {
	if bc == nil {
		return nil, errors.New("httpapi: broadcaster is nil")
	}
	a := &API{
		bc:           bc,
		maxBodyBytes: 20 << 20, // 20 MiB (hex-encoded tx payloads can be large)
	}
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	return a, nil
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.handleHealthz)
	mux.HandleFunc("POST /v1/tx/submit", a.handleSubmit)
	mux.HandleFunc("GET /v1/tx/{txid}", a.handleStatus)
	return mux
}

func (a *API) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type submitRequest struct {
	RawTxHex          string `json:"raw_tx_hex"`
	WaitConfirmations *int64 `json:"wait_confirmations,omitempty"`
}

type submitResponse struct {
	TxID   string              `json:"txid"`
	Status *broadcast.TxStatus `json:"status,omitempty"`
}

func (a *API) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req submitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, a.maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	raw := strings.TrimSpace(req.RawTxHex)
	if raw == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "raw_tx_hex required")
		return
	}
	if _, err := hex.DecodeString(raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "raw_tx_hex must be hex")
		return
	}

	ctx := r.Context()
	if req.WaitConfirmations != nil && *req.WaitConfirmations > 0 {
		txid, err := a.bc.Submit(ctx, raw)
		if err != nil {
			writeError(w, http.StatusBadGateway, "node_rpc_error", err.Error())
			return
		}

		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()

		st, err := a.bc.WaitForConfirmations(waitCtx, txid, *req.WaitConfirmations)
		if err != nil {
			writeError(w, http.StatusBadGateway, "node_rpc_error", err.Error())
			return
		}

		writeJSON(w, http.StatusOK, submitResponse{TxID: txid, Status: &st})
		return
	}

	txid, err := a.bc.Submit(ctx, raw)
	if err != nil {
		writeError(w, http.StatusBadGateway, "node_rpc_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, submitResponse{TxID: txid})
}

func (a *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	txid := strings.ToLower(strings.TrimSpace(r.PathValue("txid")))
	if txid == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "txid required")
		return
	}
	if _, err := hex.DecodeString(txid); err != nil || len(txid) != 64 {
		writeError(w, http.StatusBadRequest, "invalid_request", "txid must be 32-byte hex")
		return
	}

	st, found, err := a.bc.Status(r.Context(), txid)
	if err != nil {
		writeError(w, http.StatusBadGateway, "node_rpc_error", err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "unknown txid")
		return
	}

	writeJSON(w, http.StatusOK, st)
}

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var resp errorResponse
	resp.Error.Code = code
	resp.Error.Message = message
	_ = json.NewEncoder(w).Encode(resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
