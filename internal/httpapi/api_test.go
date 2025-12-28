package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Abdullah1738/juno-broadcast/internal/broadcast"
)

type fakeBroadcaster struct {
	submit               func(ctx context.Context, rawTxHex string) (string, error)
	status               func(ctx context.Context, txid string) (broadcast.TxStatus, bool, error)
	waitForConfirmations func(ctx context.Context, txid string, confirmations int64) (broadcast.TxStatus, error)
}

func (f fakeBroadcaster) Submit(ctx context.Context, rawTxHex string) (string, error) {
	if f.submit == nil {
		return "", nil
	}
	return f.submit(ctx, rawTxHex)
}

func (f fakeBroadcaster) Status(ctx context.Context, txid string) (broadcast.TxStatus, bool, error) {
	if f.status == nil {
		return broadcast.TxStatus{}, false, nil
	}
	return f.status(ctx, txid)
}

func (f fakeBroadcaster) WaitForConfirmations(ctx context.Context, txid string, confirmations int64) (broadcast.TxStatus, error) {
	if f.waitForConfirmations == nil {
		return broadcast.TxStatus{}, nil
	}
	return f.waitForConfirmations(ctx, txid, confirmations)
}

func TestAPI_Healthz(t *testing.T) {
	api, err := New(fakeBroadcaster{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	api.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want %d", rr.Code, http.StatusOK)
	}
}

func TestAPI_Submit_ValidatesRequest(t *testing.T) {
	api, err := New(fakeBroadcaster{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tx/submit", strings.NewReader(`{"raw_tx_hex":""}`))
	api.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want %d", rr.Code, http.StatusBadRequest)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/tx/submit", strings.NewReader(`{"raw_tx_hex":"zz"}`))
	api.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAPI_Submit_Success(t *testing.T) {
	api, err := New(fakeBroadcaster{
		submit: func(ctx context.Context, rawTxHex string) (string, error) {
			if rawTxHex != "00" {
				t.Fatalf("rawTxHex=%q want %q", rawTxHex, "00")
			}
			return strings.Repeat("a", 64), nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tx/submit", bytes.NewReader([]byte(`{"raw_tx_hex":"00"}`)))
	api.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var resp struct {
		TxID string `json:"txid"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rr.Body.String())
	}
	if resp.TxID != strings.Repeat("a", 64) {
		t.Fatalf("txid=%q", resp.TxID)
	}
}

func TestAPI_Submit_WaitConfirmations(t *testing.T) {
	txid := strings.Repeat("b", 64)
	api, err := New(fakeBroadcaster{
		submit: func(ctx context.Context, rawTxHex string) (string, error) {
			return txid, nil
		},
		waitForConfirmations: func(ctx context.Context, gotTxID string, confirmations int64) (broadcast.TxStatus, error) {
			if gotTxID != txid {
				t.Fatalf("txid=%q want %q", gotTxID, txid)
			}
			if confirmations != 1 {
				t.Fatalf("confirmations=%d want 1", confirmations)
			}
			return broadcast.TxStatus{TxID: txid, Confirmations: 1, BlockHash: "h"}, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tx/submit", bytes.NewReader([]byte(`{"raw_tx_hex":"00","wait_confirmations":1}`)))
	api.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var resp struct {
		TxID   string              `json:"txid"`
		Status *broadcast.TxStatus `json:"status"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rr.Body.String())
	}
	if resp.TxID != txid {
		t.Fatalf("txid=%q want %q", resp.TxID, txid)
	}
	if resp.Status == nil || resp.Status.Confirmations != 1 || resp.Status.BlockHash != "h" {
		t.Fatalf("unexpected status: %+v", resp.Status)
	}
}

func TestAPI_Status_NotFound(t *testing.T) {
	api, err := New(fakeBroadcaster{
		status: func(ctx context.Context, txid string) (broadcast.TxStatus, bool, error) {
			return broadcast.TxStatus{}, false, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tx/"+strings.Repeat("c", 64), nil)
	api.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d want %d body=%s", rr.Code, http.StatusNotFound, rr.Body.String())
	}
}

func TestAPI_Status_Success(t *testing.T) {
	txid := strings.Repeat("d", 64)
	api, err := New(fakeBroadcaster{
		status: func(ctx context.Context, gotTxID string) (broadcast.TxStatus, bool, error) {
			if gotTxID != txid {
				t.Fatalf("txid=%q want %q", gotTxID, txid)
			}
			return broadcast.TxStatus{TxID: txid, Confirmations: 2, BlockHash: "h"}, true, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tx/"+txid, nil)
	api.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var st broadcast.TxStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rr.Body.String())
	}
	if st.TxID != txid || st.Confirmations != 2 || st.BlockHash != "h" {
		t.Fatalf("unexpected status: %+v", st)
	}
}
