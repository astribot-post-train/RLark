package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func (h Handler) serveLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	id, err := requestIdentity(r, h.Reporter.now())
	if err != nil {
		http.Error(w, "authenticated container Agent required", http.StatusForbidden)
		return
	}
	var req LogRequest
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil || d.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid scoped log request", http.StatusBadRequest)
		return
	}
	work, err := (LogService{Tasks: TaskService{Reporter: h.Reporter}, Broker: h.Logs}).Handle(r.Context(), id, req)
	if err != nil {
		status := http.StatusServiceUnavailable
		switch {
		case errors.Is(err, ErrIdentity), apierrors.IsNotFound(err):
			status = http.StatusForbidden
		case errors.Is(err, ErrCapability):
			status = http.StatusBadRequest
		}
		http.Error(w, "scoped log operation unavailable", status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(work)
}

// Log exchanges only the node-scoped read protocol over the existing mTLS client.
func (c *Client) Log(ctx context.Context, req LogRequest) (*LogWork, error) {
	if c == nil || c.HTTPClient == nil || c.Endpoint == "" {
		return nil, errors.New("configured Agent client required")
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if len(data) > 128<<10 {
		return nil, ErrCapability
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/v1/logs", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	res, err := c.HTTPClient.Do(r)
	if err != nil {
		return nil, fmt.Errorf("send scoped log request: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		return nil, fmt.Errorf("scoped log request returned HTTP %d", res.StatusCode)
	}
	var work *LogWork
	d := json.NewDecoder(io.LimitReader(res.Body, 16<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&work); err != nil || d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid scoped log response")
	}
	return work, nil
}

// RespondLog reads only server-selected Task output. Failures become a bounded
// unavailable result; local paths/errors and workload payloads never enter logs.
func RespondLog(ctx context.Context, c *Client, reader raw.LogReader) error {
	work, err := c.Log(ctx, LogRequest{Action: "poll"})
	if err != nil || work == nil {
		return err
	}
	if reader == nil || work.ID == "" || work.Task.ObjectUID != work.Target.TaskUID || work.Task.ObjectUID == "" || work.Task.Logical.Validate() != nil || work.Task.Generation < 1 || work.Target.Allocation.Namespace != work.Task.Logical.Namespace {
		return ErrCapability
	}
	snapshot, readErr := reader.Logs(ctx, work.Task, raw.MaxLogBytes)
	req := LogRequest{Action: "complete", Work: work, Unavailable: readErr != nil}
	if readErr == nil {
		req.Snapshot = &snapshot
	}
	_, err = c.Log(ctx, req)
	return err
}

// RunLogReads is independent of heartbeat/execution so a failed diagnostic read
// cannot delay exact-owner cleanup. Each read has a ten-second context bound.
func RunLogReads(ctx context.Context, c *Client, reader raw.LogReader) {
	delay := 2 * time.Second
	for ctx.Err() == nil {
		step, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := RespondLog(step, c, reader)
		cancel()
		if err == nil {
			delay = 2 * time.Second
		} else if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
