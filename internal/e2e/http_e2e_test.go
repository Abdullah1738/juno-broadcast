//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abdullah1738/juno-broadcast/internal/testutil/containers"
)

func TestHTTPAPI_SubmitWaitAndStatus_E2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	jd, err := containers.StartJunocashd(ctx)
	if err != nil {
		t.Fatalf("StartJunocashd: %v", err)
	}
	defer func() { _ = jd.Terminate(context.Background()) }()

	mustRunCLI(t, ctx, jd, "generate", "101")

	addr := mustCreateOrchardAddress(t, ctx, jd)
	opid := mustShieldCoinbase(t, ctx, jd, addr)
	txid := mustWaitOpTxID(t, ctx, jd, opid)
	raw := strings.TrimSpace(string(mustRunCLI(t, ctx, jd, "getrawtransaction", txid)))

	bin := filepath.Join("..", "..", "bin", "juno-broadcast")
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("missing binary: %v", err)
	}

	listen := mustFreeAddr(t)
	baseURL := "http://" + listen

	cmd := exec.Command(bin, "serve",
		"--rpc-url", jd.RPCURL,
		"--rpc-user", jd.RPCUser,
		"--rpc-pass", jd.RPCPassword,
		"--listen", listen,
		"--poll", "50ms",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		case <-done:
		}
		if t.Failed() {
			t.Logf("serve stderr:\n%s", stderr.String())
			t.Logf("serve stdout:\n%s", stdout.String())
		}
	})

	if err := waitForHealthz(ctx, baseURL+"/healthz"); err != nil {
		t.Fatalf("healthz: %v\nstderr=%s", err, stderr.String())
	}

	type submitResp struct {
		TxID   string `json:"txid"`
		Status struct {
			TxID          string `json:"txid"`
			InMempool     bool   `json:"in_mempool"`
			Confirmations int64  `json:"confirmations"`
			BlockHash     string `json:"blockhash"`
		} `json:"status"`
	}

	submitCh := make(chan submitResp, 1)
	errCh := make(chan error, 1)
	go func() {
		body, _ := json.Marshal(map[string]any{
			"raw_tx_hex":         raw,
			"wait_confirmations": int64(1),
		})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/tx/submit", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			errCh <- fmt.Errorf("submit status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(b)))
			return
		}
		var sr submitResp
		if err := json.Unmarshal(b, &sr); err != nil {
			errCh <- fmt.Errorf("submit invalid json: %w body=%s", err, strings.TrimSpace(string(b)))
			return
		}
		submitCh <- sr
	}()

	time.Sleep(200 * time.Millisecond)
	mustRunCLI(t, ctx, jd, "generate", "2")

	var sr submitResp
	select {
	case err := <-errCh:
		t.Fatalf("submit: %v\nstderr=%s", err, stderr.String())
	case sr = <-submitCh:
	case <-ctx.Done():
		t.Fatalf("submit timeout: %v\nstderr=%s", ctx.Err(), stderr.String())
	}

	if sr.TxID != txid {
		t.Fatalf("txid mismatch: got %q want %q", sr.TxID, txid)
	}
	if sr.Status.Confirmations < 1 {
		t.Fatalf("confirmations=%d want >=1", sr.Status.Confirmations)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := getTxStatus(ctx, baseURL+"/v1/tx/"+txid)
		if err != nil {
			t.Fatalf("status: %v\nstderr=%s", err, stderr.String())
		}
		if st.Confirmations >= 1 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for confirmations via status endpoint")
}

func mustFreeAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func waitForHealthz(ctx context.Context, url string) error {
	deadline := time.Now().Add(20 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout")
}

type txStatus struct {
	TxID          string `json:"txid"`
	InMempool     bool   `json:"in_mempool"`
	Confirmations int64  `json:"confirmations"`
	BlockHash     string `json:"blockhash"`
}

func getTxStatus(ctx context.Context, url string) (txStatus, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return txStatus{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return txStatus{}, fmt.Errorf("status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var st txStatus
	if err := json.Unmarshal(b, &st); err != nil {
		return txStatus{}, fmt.Errorf("invalid json: %w body=%s", err, strings.TrimSpace(string(b)))
	}
	return st, nil
}
