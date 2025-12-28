package broadcast

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/Abdullah1738/juno-sdk-go/junocashd"
)

type TxStatus struct {
	TxID          string `json:"txid"`
	InMempool     bool   `json:"in_mempool"`
	Confirmations int64  `json:"confirmations"`
	BlockHash     string `json:"blockhash,omitempty"`
}

type RPC interface {
	Call(ctx context.Context, method string, params any, out any) error
	SendRawTransaction(ctx context.Context, txHex string) (string, error)
}

type Client struct {
	rpc           RPC
	pollInterval  time.Duration
	chainLookback int64
	retry         RetryPolicy
}

type Option func(*Client)

func WithPollInterval(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.pollInterval = d
		}
	}
}

func WithChainLookback(lookback int64) Option {
	return func(c *Client) {
		if lookback >= 0 {
			c.chainLookback = lookback
		}
	}
}

type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

func WithRetryPolicy(p RetryPolicy) Option {
	return func(c *Client) {
		if p.MaxAttempts > 0 {
			c.retry = p
		}
	}
}

func New(rpc RPC, opts ...Option) (*Client, error) {
	if rpc == nil {
		return nil, errors.New("broadcast: rpc is nil")
	}
	c := &Client{
		rpc:           rpc,
		pollInterval:  500 * time.Millisecond,
		chainLookback: 2000,
		retry: RetryPolicy{
			MaxAttempts: 5,
			BaseDelay:   200 * time.Millisecond,
			MaxDelay:    2 * time.Second,
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c, nil
}

func (c *Client) Submit(ctx context.Context, rawTxHex string) (string, error) {
	raw, err := normalizeHex(rawTxHex)
	if err != nil {
		return "", err
	}

	var txid string
	if err := doWithRetry(ctx, c.retry, func(err error) bool {
		return isRetryableErr(err)
	}, func(ctx context.Context) error {
		got, err := c.rpc.SendRawTransaction(ctx, raw)
		if err != nil {
			return err
		}
		txid = got
		return nil
	}); err != nil {
		return "", err
	}

	txid = strings.ToLower(strings.TrimSpace(txid))
	if _, err := hex.DecodeString(txid); err != nil || len(txid) != 64 {
		return "", errors.New("broadcast: node returned invalid txid")
	}
	return txid, nil
}

func (c *Client) Status(ctx context.Context, txid string) (TxStatus, bool, error) {
	txid = strings.ToLower(strings.TrimSpace(txid))
	if _, err := hex.DecodeString(txid); err != nil || len(txid) != 64 {
		return TxStatus{}, false, errors.New("broadcast: txid must be 32-byte hex")
	}

	// Prefer a direct lookup (works for mempool; and for chain when txindex is enabled or the tx is wallet-owned).
	var verbose struct {
		TxID          string `json:"txid"`
		BlockHash     string `json:"blockhash"`
		Confirmations int64  `json:"confirmations"`
	}
	err := doWithRetry(ctx, c.retry, func(err error) bool {
		return isRetryableErr(err) && !isNotFoundErr(err)
	}, func(ctx context.Context) error {
		return c.rpc.Call(ctx, "getrawtransaction", []any{txid, 1}, &verbose)
	})
	if err == nil {
		return TxStatus{
			TxID:          txid,
			InMempool:     verbose.Confirmations == 0 && verbose.BlockHash == "",
			Confirmations: verbose.Confirmations,
			BlockHash:     strings.TrimSpace(verbose.BlockHash),
		}, true, nil
	}
	if err != nil && !isNotFoundErr(err) {
		return TxStatus{}, false, err
	}

	inMempool, err := c.mempoolContains(ctx, txid)
	if err != nil {
		return TxStatus{}, false, err
	}
	if inMempool {
		return TxStatus{
			TxID:          txid,
			InMempool:     true,
			Confirmations: 0,
		}, true, nil
	}

	// Final fallback (no txindex): scan backwards from the tip for a limited window.
	if c.chainLookback > 0 {
		st, found, err := c.findInRecentBlocks(ctx, txid, c.chainLookback)
		if err != nil {
			return TxStatus{}, false, err
		}
		if found {
			return st, true, nil
		}
	}

	return TxStatus{}, false, nil
}

func (c *Client) WaitForConfirmations(ctx context.Context, txid string, confirmations int64) (TxStatus, error) {
	if confirmations < 0 {
		return TxStatus{}, errors.New("broadcast: confirmations must be >= 0")
	}

	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	var pinnedBlockHash string

	for {
		if pinnedBlockHash != "" {
			confs, ok, err := c.blockConfirmations(ctx, pinnedBlockHash)
			if err != nil {
				return TxStatus{}, err
			}
			if !ok {
				pinnedBlockHash = ""
			} else {
				st := TxStatus{
					TxID:          txid,
					InMempool:     false,
					Confirmations: confs,
					BlockHash:     pinnedBlockHash,
				}
				if confirmations == 0 || confs >= confirmations {
					return st, nil
				}
			}
		} else {
			st, found, err := c.Status(ctx, txid)
			if err != nil {
				return TxStatus{}, err
			}
			if found && st.BlockHash != "" {
				pinnedBlockHash = st.BlockHash
			}
			if found && (confirmations == 0 || st.Confirmations >= confirmations) {
				return st, nil
			}
		}

		select {
		case <-ctx.Done():
			return TxStatus{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func normalizeHex(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("broadcast: raw tx hex is required")
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", errors.New("broadcast: raw tx hex must be hex")
	}
	return s, nil
}

func isNotFoundErr(err error) bool {
	var rpcErr *junocashd.RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	msg := strings.ToLower(rpcErr.Message)
	return strings.Contains(msg, "no such mempool") ||
		strings.Contains(msg, "no such") ||
		strings.Contains(msg, "not found")
}

func isMethodNotFoundErr(err error) bool {
	var rpcErr *junocashd.RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	if rpcErr.Code == -32601 {
		return true
	}
	msg := strings.ToLower(rpcErr.Message)
	return strings.Contains(msg, "method not found") ||
		strings.Contains(msg, "unknown method") ||
		strings.Contains(msg, "unknown command")
}

func (c *Client) mempoolContains(ctx context.Context, txid string) (bool, error) {
	var entry map[string]any
	err := doWithRetry(ctx, c.retry, func(err error) bool {
		return isRetryableErr(err) && !isNotFoundErr(err) && !isMethodNotFoundErr(err)
	}, func(ctx context.Context) error {
		return c.rpc.Call(ctx, "getmempoolentry", []any{txid}, &entry)
	})
	if err == nil {
		return true, nil
	}
	if !isMethodNotFoundErr(err) {
		if isNotFoundErr(err) {
			return false, nil
		}
		return false, fmt.Errorf("broadcast: getmempoolentry: %w", err)
	}

	var mempool []string
	if err := doWithRetry(ctx, c.retry, isRetryableErr, func(ctx context.Context) error {
		return c.rpc.Call(ctx, "getrawmempool", []any{false}, &mempool)
	}); err != nil {
		return false, fmt.Errorf("broadcast: getrawmempool: %w", err)
	}
	for _, id := range mempool {
		if strings.ToLower(strings.TrimSpace(id)) == txid {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) findInRecentBlocks(ctx context.Context, txid string, lookback int64) (TxStatus, bool, error) {
	tipHash, err := callString(ctx, c.retry, c.rpc, "getbestblockhash", nil)
	if err != nil {
		return TxStatus{}, false, err
	}

	curHash := strings.TrimSpace(tipHash)
	for i := int64(0); i < lookback && curHash != ""; i++ {
		var blk struct {
			Hash              string   `json:"hash"`
			Confirmations     int64    `json:"confirmations"`
			PreviousBlockHash string   `json:"previousblockhash"`
			Tx                []string `json:"tx"`
		}

		if err := doWithRetry(ctx, c.retry, isRetryableErr, func(ctx context.Context) error {
			return c.rpc.Call(ctx, "getblock", []any{curHash, 1}, &blk)
		}); err != nil {
			return TxStatus{}, false, err
		}

		for _, id := range blk.Tx {
			if strings.ToLower(strings.TrimSpace(id)) == txid {
				return TxStatus{
					TxID:          txid,
					InMempool:     false,
					Confirmations: blk.Confirmations,
					BlockHash:     blk.Hash,
				}, true, nil
			}
		}

		curHash = strings.TrimSpace(blk.PreviousBlockHash)
	}

	return TxStatus{}, false, nil
}

func (c *Client) blockConfirmations(ctx context.Context, blockHash string) (int64, bool, error) {
	blockHash = strings.TrimSpace(blockHash)
	if blockHash == "" {
		return 0, false, nil
	}

	var hdr struct {
		Hash          string `json:"hash"`
		Confirmations int64  `json:"confirmations"`
	}
	if err := doWithRetry(ctx, c.retry, func(err error) bool {
		return isRetryableErr(err) && !isNotFoundErr(err)
	}, func(ctx context.Context) error {
		return c.rpc.Call(ctx, "getblockheader", []any{blockHash, true}, &hdr)
	}); err != nil {
		if isNotFoundErr(err) {
			return 0, false, nil
		}
		return 0, false, err
	}

	if hdr.Confirmations <= 0 {
		return 0, false, nil
	}
	return hdr.Confirmations, true, nil
}

func callString(ctx context.Context, retry RetryPolicy, rpc RPC, method string, params any) (string, error) {
	var out string
	if err := doWithRetry(ctx, retry, isRetryableErr, func(ctx context.Context) error {
		return rpc.Call(ctx, method, params, &out)
	}); err != nil {
		return "", err
	}
	return out, nil
}

func doWithRetry(ctx context.Context, p RetryPolicy, shouldRetry func(error) bool, fn func(context.Context) error) error {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 1
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = 100 * time.Millisecond
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = 2 * time.Second
	}

	var lastErr error
	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := fn(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}

		if attempt == p.MaxAttempts || shouldRetry == nil || !shouldRetry(lastErr) {
			break
		}

		sleep := backoff(p.BaseDelay, p.MaxDelay, attempt)
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

func backoff(base, max time.Duration, attempt int) time.Duration {
	if attempt <= 1 {
		return base
	}
	d := base * time.Duration(1<<(attempt-1))
	if d > max {
		return max
	}
	return d
}

func isRetryableErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var rpcErr *junocashd.RPCError
	if errors.As(err, &rpcErr) {
		if rpcErr.Code == -28 {
			return true
		}
		msg := strings.ToLower(rpcErr.Message)
		return strings.Contains(msg, "loading block index") ||
			strings.Contains(msg, "loading wallet") ||
			strings.Contains(msg, "warming up")
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary()
	}

	msg := err.Error()
	if status, ok := parseHTTPStatus(msg); ok && status >= 500 {
		return true
	}
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "EOF") {
		return true
	}

	return false
}

func parseHTTPStatus(msg string) (int, bool) {
	const prefix = "junocashd: http "
	if !strings.HasPrefix(msg, prefix) {
		return 0, false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(msg, prefix))
	n, _, ok := strings.Cut(rest, ":")
	if !ok {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(n))
	if err != nil {
		return 0, false
	}
	return code, true
}
