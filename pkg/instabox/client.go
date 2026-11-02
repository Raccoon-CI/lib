// Package instabox is the small part of the InstaBox API that Raccoon CI
// needs to run a job on a machine of its own: create one from a snapshot, put
// a file on it, delete it. The API key comes from the caller (the
// INSTABOX_API_KEY environment variable in `raccoonci run`) and is only ever
// sent in the X-API-Key header of requests to the API's own address.
package instabox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultURL is the public API.
const DefaultURL = "https://api.instabox.sh"

// Client talks to one InstaBox API.
type Client struct {
	URL    string // default DefaultURL
	Key    string
	HTTP   *http.Client
	Poll   time.Duration      // operation polling interval; default 500 ms
	OnWait func(op Operation) // optional progress hook
}

// Error is an API error reply.
type Error struct {
	Status    int
	Code      string   `json:"code"`
	Message   string   `json:"message"`
	Retryable bool     `json:"retryable"`
	Scopes    []string `json:"requiredScopes"`
}

func (e *Error) Error() string {
	s := fmt.Sprintf("instabox: %s (%d %s)", e.Message, e.Status, e.Code)
	if len(e.Scopes) > 0 {
		s += ", needs scope " + strings.Join(e.Scopes, ", ")
	}
	return s
}

// CreateRequest is the part of the create body we use.
type CreateRequest struct {
	Name       string            `json:"name,omitempty"`
	SnapshotID string            `json:"snapshotId,omitempty"`
	TemplateID string            `json:"templateId,omitempty"`
	VCPUs      int               `json:"vcpus,omitempty"`
	MemMiB     int               `json:"memMiB,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	// DeleteAfterSeconds (60 to 86400) makes the server delete the machine
	// itself, so one cannot outlive a controller that died.
	DeleteAfterSeconds int `json:"deleteAfterSeconds,omitempty"`
	// AutoSuspendAfterSeconds is left to the server's default when 0.
	FailFast bool `json:"failFast,omitempty"`
}

// Operation is the progress of a change.
type Operation struct {
	ID        string `json:"id"`
	MachineID string `json:"machineId"`
	Type      string `json:"type"`
	State     string `json:"state"`
	Phase     string `json:"phase"`
	Error     *Error `json:"error"`
	Machine   *struct {
		Spec Spec `json:"spec"`
	} `json:"machine"`
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, ctype string, hdr map[string]string, out any) error {
	base := c.URL
	if base == "" {
		base = DefaultURL
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.Key)
	if method != "GET" {
		// Required by the API on changes; a fresh key per call, since we never
		// replay a request.
		var k [16]byte
		rand.Read(k[:])
		req.Header.Set("Idempotency-Key", hex.EncodeToString(k[:]))
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		e := &Error{Status: resp.StatusCode}
		var wrap struct {
			Error *Error `json:"error"`
		}
		if json.Unmarshal(data, &wrap) == nil && wrap.Error != nil {
			wrap.Error.Status = resp.StatusCode
			e = wrap.Error
		} else {
			e.Message = strings.TrimSpace(string(data[:min(len(data), 200)]))
		}
		return e
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// CreateMachine starts the creation and waits until the machine runs. The
// machine id is returned even on failure once the API has assigned one, so
// the caller can delete a half-made machine.
func (c *Client) CreateMachine(ctx context.Context, r CreateRequest) (id string, err error) {
	id, _, err = c.CreateMachineSpec(ctx, r)
	return id, err
}

// CreateMachineSpec is CreateMachine that also returns the machine's size, as
// the API reports it (zero if it does not).
func (c *Client) CreateMachineSpec(ctx context.Context, r CreateRequest) (id string, spec Spec, err error) {
	b, _ := json.Marshal(r)
	var op Operation
	if err := c.do(ctx, "POST", "/v1/machines", bytes.NewReader(b), "application/json", nil, &op); err != nil {
		return "", Spec{}, err
	}
	if op.Machine != nil {
		spec = op.Machine.Spec
	}
	done, err := c.wait(ctx, op.ID)
	if done.Machine != nil {
		spec = done.Machine.Spec
	}
	return op.MachineID, spec, err
}

// Size is a machine size the organization may create: vCPUs with their memory.
type Size struct {
	VCPUs  int `json:"vcpus"`
	MemMiB int `json:"memMiB"`
}

// AllowedSizes lists the sizes the organization of the key may create
// (GET /v1/orgs: what its plan allows, plus any an admin granted).
func (c *Client) AllowedSizes(ctx context.Context) ([]Size, error) {
	var res struct {
		Orgs []struct {
			AllowedSpecs []Size `json:"allowedSpecs"`
		} `json:"orgs"`
	}
	if err := c.do(ctx, "GET", "/v1/orgs", nil, "", nil, &res); err != nil {
		return nil, err
	}
	if len(res.Orgs) == 0 {
		return nil, errors.New("instabox: the key has no organization")
	}
	return res.Orgs[0].AllowedSpecs, nil
}

// Wait polls an operation until it ends.
func (c *Client) Wait(ctx context.Context, opID string) error {
	_, err := c.wait(ctx, opID)
	return err
}

func (c *Client) wait(ctx context.Context, opID string) (Operation, error) {
	poll := c.Poll
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	for {
		var op Operation
		if err := c.do(ctx, "GET", "/v1/operations/"+url.PathEscape(opID), nil, "", nil, &op); err != nil {
			return op, err
		}
		if c.OnWait != nil {
			c.OnWait(op)
		}
		switch op.State {
		case "succeeded":
			return op, nil
		case "failed":
			if op.Error != nil {
				return op, fmt.Errorf("instabox: %s: %s", op.Type, op.Error.Message)
			}
			return op, fmt.Errorf("instabox: %s failed", op.Type)
		}
		select {
		case <-ctx.Done():
			return op, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// PutFile writes a file on a running machine (mkdir -p for its directory).
func (c *Client) PutFile(ctx context.Context, machine, path string, mode string, data []byte) error {
	q := url.Values{"path": {path}, "mode": {mode}, "parents": {"true"}}
	return c.do(ctx, "PUT", "/v1/machines/"+url.PathEscape(machine)+"/files?"+q.Encode(),
		bytes.NewReader(data), "application/octet-stream", nil, nil)
}

// Exec runs a command (bash -lc) on a machine and returns its exit status.
// Used only to start the agent when the snapshot does not do it by itself.
func (c *Client) Exec(ctx context.Context, machine, command string) (exit int, out string, err error) {
	b, _ := json.Marshal(map[string]any{"cmd": command})
	var res struct {
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := c.do(ctx, "POST", "/v1/machines/"+url.PathEscape(machine)+"/exec", bytes.NewReader(b), "application/json", nil, &res); err != nil {
		return 0, "", err
	}
	return res.ExitCode, res.Stdout + res.Stderr, nil
}

// DeleteMachine deletes a machine (the confirm header makes a wrong id refuse)
// and waits for it to go. A machine that is already gone is not an error.
func (c *Client) DeleteMachine(ctx context.Context, machine string) error {
	// A machine that is still being created cannot be deleted (409
	// operation_conflict): a run cancelled while its machine was starting left
	// one running for the two hours of its deadline. Wait for the operation
	// and ask again, until ctx ends.
	for {
		err := c.deleteMachine(ctx, machine)
		var e *Error
		if !errors.As(err, &e) || e.Status != http.StatusConflict || e.Code != "operation_conflict" {
			return err
		}
		poll := c.Poll
		if poll <= 0 {
			poll = 500 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(2 * poll):
		}
	}
}

func (c *Client) deleteMachine(ctx context.Context, machine string) error {
	var op Operation
	err := c.do(ctx, "DELETE", "/v1/machines/"+url.PathEscape(machine), nil, "",
		map[string]string{"X-Confirm-Delete": machine}, &op)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	if op.ID == "" {
		return nil
	}
	return c.Wait(ctx, op.ID)
}

// MachineInfo is what a machine listing says about one machine.
type MachineInfo struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	State    string            `json:"state"` // running, suspended, stopped...
	Metadata map[string]string `json:"metadata"`
	Spec     Spec              `json:"spec"`
}

// ListMachines lists the machines whose metadata has all the given
// key/value pairs.
func (c *Client) ListMachines(ctx context.Context, metadata map[string]string) ([]MachineInfo, error) {
	var out []MachineInfo
	cursor := ""
	for {
		q := url.Values{"limit": {"100"}}
		for k, v := range metadata {
			q.Add("metadata", k+":"+v)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Machines   []MachineInfo `json:"machines"`
			NextCursor string        `json:"nextCursor"`
		}
		if err := c.do(ctx, "GET", "/v1/machines?"+q.Encode(), nil, "", nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Machines...)
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// GetMachine reads one machine.
func (c *Client) GetMachine(ctx context.Context, machine string) (MachineInfo, error) {
	var m MachineInfo
	err := c.do(ctx, "GET", "/v1/machines/"+url.PathEscape(machine), nil, "", nil, &m)
	return m, err
}

// Suspend pauses a running machine, keeping its programs, and waits until it
// is paused.
func (c *Client) Suspend(ctx context.Context, machine string) error {
	return c.transition(ctx, machine, "suspend")
}

// Resume brings a paused machine back where it left off and waits until it
// runs. Resuming a machine that already runs is not an error, so it cannot be
// used to tell who got a machine first (see Spares.Claim).
func (c *Client) Resume(ctx context.Context, machine string) error {
	return c.transition(ctx, machine, "resume")
}

func (c *Client) transition(ctx context.Context, machine, verb string) error {
	var op Operation
	if err := c.do(ctx, "POST", "/v1/machines/"+url.PathEscape(machine)+"/"+verb, nil, "", nil, &op); err != nil {
		return err
	}
	return c.Wait(ctx, op.ID)
}

// SetMetadata replaces a machine's metadata.
func (c *Client) SetMetadata(ctx context.Context, machine string, md map[string]string) error {
	b, _ := json.Marshal(map[string]any{"metadata": md})
	return c.do(ctx, "PATCH", "/v1/machines/"+url.PathEscape(machine), bytes.NewReader(b), "application/json", nil, nil)
}

// Snapshot saves a machine's system files and storage as a snapshot of the
// given name (unique, lowercase letters, digits and dashes) and waits until
// it can be used. The snapshot's id is returned even when the wait fails, so
// the caller can delete it.
func (c *Client) Snapshot(ctx context.Context, machine, name, description string) (id string, err error) {
	b, _ := json.Marshal(map[string]string{"name": name, "description": description})
	var snap struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := c.do(ctx, "POST", "/v1/machines/"+url.PathEscape(machine)+"/snapshots", bytes.NewReader(b), "application/json", nil, &snap); err != nil {
		return "", err
	}
	poll := c.Poll
	if poll <= 0 {
		poll = time.Second
	}
	for snap.Status == "pending" {
		select {
		case <-ctx.Done():
			return snap.ID, ctx.Err()
		case <-time.After(poll):
		}
		if err := c.do(ctx, "GET", "/v1/snapshots/"+url.PathEscape(snap.ID), nil, "", nil, &snap); err != nil {
			return snap.ID, err
		}
	}
	if snap.Status != "ready" {
		return snap.ID, fmt.Errorf("instabox: snapshot %s is %s", snap.ID, snap.Status)
	}
	return snap.ID, nil
}

// DeleteSnapshot deletes a snapshot (key scope images:delete).
func (c *Client) DeleteSnapshot(ctx context.Context, id string) error {
	err := c.do(ctx, "DELETE", "/v1/snapshots/"+url.PathEscape(id), nil, "", nil, nil)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusNotFound {
		return nil
	}
	return err
}
