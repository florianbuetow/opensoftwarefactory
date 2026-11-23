package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const maxStdioMessage = 16 << 20

// stdioUpstream starts an externally supplied MCP program only when it is
// first used. One process owns one shared, newline-delimited JSON-RPC channel.
type stdioUpstream struct {
	process    *stdioProcess
	connection Connection
	mu         sync.Mutex
	nextID     atomic.Uint64
	closed     bool
}

type stdioProcess struct {
	owner   *stdioUpstream
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	pending map[string]stdioPending
	done    chan struct{}
	exited  chan struct{}
	init    []byte
	mu      sync.Mutex
	writeMu sync.Mutex
	initMu  sync.Mutex
	closed  bool
}

type stdioPending struct {
	result     chan stdioResult
	stream     *io.PipeWriter
	finished   chan struct{}
	originalID json.RawMessage
	progress   json.RawMessage
}

type stdioResult struct {
	err  error
	body []byte
}

// stdioMessage is the part of a line from the child that routing needs.
type stdioMessage struct {
	Params map[string]json.RawMessage `json:"params"`
	Method string                     `json:"method"`
	ID     json.RawMessage            `json:"id"`
}

func (u *stdioUpstream) RoundTrip(ctx context.Context, body []byte, _ http.Header) (*http.Response, error) {
	message := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &message); err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC message: %w", err)
	}
	var method string
	if err := json.Unmarshal(message["method"], &method); err != nil || method == "" {
		return nil, errors.New("stdio upstream requires a JSON-RPC method")
	}

	for {
		process, err := u.getProcess()
		if err != nil {
			return nil, err
		}
		if method == methodCancelled {
			err = process.cancelOriginal(ctx, message)
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: http.StatusAccepted, Status: "202 Accepted", Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil))}, nil
		}
		initialized, err := process.ensureInitialized(ctx)
		if errors.Is(err, errProcessClosed) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if method == methodInitialize {
			var reply map[string]json.RawMessage
			err = json.Unmarshal(initialized, &reply)
			if err != nil {
				return nil, err
			}
			reply["id"] = message["id"]
			initialized, err = json.Marshal(reply)
			if err != nil {
				return nil, err
			}
			return jsonResponse(initialized), nil
		}
		if method == methodInitialized {
			return &http.Response{StatusCode: http.StatusAccepted, Status: "202 Accepted", Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil))}, nil
		}
		originalID, ok := message["id"]
		if !ok || len(originalID) == 0 || bytes.Equal(bytes.TrimSpace(originalID), []byte("null")) {
			return nil, errors.New("stdio upstream requires a JSON-RPC request id")
		}
		response, err := process.call(ctx, body, originalID, &u.nextID)
		if errors.Is(err, errProcessClosed) {
			continue
		}
		return response, err
	}
}

func (p *stdioProcess) cancelOriginal(ctx context.Context, message map[string]json.RawMessage) error {
	var params map[string]json.RawMessage
	if json.Unmarshal(message["params"], &params) != nil {
		return errors.New("invalid cancellation notification")
	}
	requestID := bytes.TrimSpace(params["requestId"])
	if len(requestID) == 0 {
		return errors.New("cancellation requestId is required")
	}
	p.mu.Lock()
	internal := ""
	var pending stdioPending
	matches := 0
	for id, candidate := range p.pending {
		if sameID(candidate.originalID, requestID) {
			internal, pending, matches = id, candidate, matches+1
		}
	}
	if matches == 1 {
		delete(p.pending, internal)
	}
	p.mu.Unlock()
	if matches != 1 {
		return nil
	}
	err := context.Canceled
	if pending.stream != nil {
		pending.finish(err)
	} else {
		pending.result <- stdioResult{err: err}
		close(pending.finished)
	}
	return p.cancel(ctx, internal)
}

var errProcessClosed = errors.New("stdio MCP server is not running")

func (p *stdioProcess) ensureInitialized(ctx context.Context) ([]byte, error) {
	p.initMu.Lock()
	defer p.initMu.Unlock()
	if p.init != nil {
		return append([]byte(nil), p.init...), nil
	}
	response, err := p.call(ctx, initializeRequest("gateway-initialize"), json.RawMessage(`"gateway-initialize"`), &p.owner.nextID)
	if err != nil {
		return nil, err
	}
	body, err := readBounded(response.Body)
	if closeErr := response.Body.Close(); closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	if err != nil {
		return nil, err
	}
	request := rpcRequest{Method: methodInitialize, ID: json.RawMessage(`"gateway-initialize"`)}
	body, err = filterResponse(body, request, &Config{})
	if err != nil {
		return nil, err
	}
	err = p.write(ctx, initializedNotification())
	if err != nil {
		return nil, err
	}
	p.init = append([]byte(nil), body...)
	return append([]byte(nil), p.init...), nil
}

func jsonResponse(body []byte) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}
}

func (p *stdioProcess) call(ctx context.Context, body, originalID json.RawMessage, sequence *atomic.Uint64) (*http.Response, error) {
	message := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &message); err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC message: %w", err)
	}
	internalID := "mcp-gateway-" + strconv.FormatUint(sequence.Add(1), 10)
	response := make(chan stdioResult, 1)
	pending := stdioPending{originalID: originalID, result: response, progress: progressToken(message), finished: make(chan struct{})}
	var reader *io.PipeReader
	if len(pending.progress) > 0 {
		reader, pending.stream = io.Pipe()
	}
	if err := p.register(internalID, pending); err != nil {
		return nil, err
	}

	message["id"] = json.RawMessage(strconv.Quote(internalID))
	outbound, err := json.Marshal(message)
	if err != nil {
		p.remove(internalID)
		return nil, err
	}
	if err := p.write(ctx, outbound); err != nil {
		if taken, ok := p.take(internalID); ok {
			taken.finish(err)
		}
		return nil, err
	}
	if reader != nil {
		go p.cancelWhenDone(ctx, internalID, pending.finished)
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{eventStream}, "Cache-Control": []string{"no-cache"}, "X-Accel-Buffering": []string{"no"}}, Body: reader}, nil
	}

	select {
	case result := <-response:
		if result.err != nil {
			return nil, result.err
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(result.body))}, nil
	case <-ctx.Done():
		if pending, ok := p.take(internalID); ok {
			pending.finish(ctx.Err())
			cancel, stop := context.WithTimeout(context.WithoutCancel(ctx), 100*time.Millisecond)
			_ = p.cancel(cancel, internalID)
			stop()
		}
		return nil, ctx.Err()
	}
}

func progressToken(message map[string]json.RawMessage) json.RawMessage {
	var params, meta map[string]json.RawMessage
	if json.Unmarshal(message["params"], &params) != nil || json.Unmarshal(params["_meta"], &meta) != nil {
		return nil
	}
	token := bytes.TrimSpace(meta["progressToken"])
	if len(token) == 0 || bytes.Equal(token, []byte("null")) {
		return nil
	}
	var scalar any
	if json.Unmarshal(token, &scalar) != nil {
		return nil
	}
	switch scalar.(type) {
	case string, float64:
		return append(json.RawMessage(nil), token...)
	default:
		return nil
	}
}

func (p *stdioProcess) cancelWhenDone(ctx context.Context, id string, finished <-chan struct{}) {
	select {
	case <-finished:
		return
	case <-ctx.Done():
		if pending, ok := p.take(id); ok {
			pending.finish(ctx.Err())
			cancel, stop := context.WithTimeout(context.WithoutCancel(ctx), 100*time.Millisecond)
			_ = p.cancel(cancel, id)
			stop()
		}
	}
}

func (u *stdioUpstream) getProcess() (*stdioProcess, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return nil, errors.New("stdio MCP upstream is closed")
	}
	if u.process != nil && !u.process.isClosed() {
		return u.process, nil
	}

	// #nosec G204 G702 -- permitted: running the command named in mcpconfig.json is the stdio transport
	cmd := exec.Command(u.connection.Command, u.connection.Args...) //nolint:gosec // permitted: running the command named in mcpconfig.json is the stdio transport
	cmd.Env = environment(u.connection.Env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	err = cmd.Start()
	if err != nil {
		return nil, err
	}
	p := &stdioProcess{owner: u, cmd: cmd, stdin: stdin, pending: map[string]stdioPending{}, done: make(chan struct{}), exited: make(chan struct{})}
	u.process = p
	go p.read(stdout)
	go p.log(stderr)
	go p.wait()
	return p, nil
}

func environment(overrides map[string]string) []string {
	base := os.Environ()
	index := make(map[string]int, len(base))
	for i, value := range base {
		for j := 0; j < len(value); j++ {
			if value[j] == '=' {
				index[value[:j]] = i
				break
			}
		}
	}
	for key, value := range overrides {
		entry := key + "=" + value
		if i, ok := index[key]; ok {
			base[i] = entry
		} else {
			index[key] = len(base)
			base = append(base, entry)
		}
	}
	return base
}

func (p *stdioProcess) register(id string, pending stdioPending) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errProcessClosed
	}
	if len(pending.progress) > 0 {
		for _, existing := range p.pending {
			if len(existing.progress) > 0 && bytes.Equal(existing.progress, pending.progress) {
				return errors.New("progress token is already in use by an in-flight request")
			}
		}
	}
	p.pending[id] = pending
	return nil
}

func (p *stdioProcess) remove(id string) bool {
	_, ok := p.take(id)
	return ok
}

func (p *stdioProcess) take(id string) (stdioPending, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pending, ok := p.pending[id]
	if !ok {
		return stdioPending{}, false
	}
	delete(p.pending, id)
	return pending, true
}

func (p stdioPending) finish(err error) {
	if p.stream != nil {
		if err == nil {
			_ = p.stream.Close()
		} else {
			_ = p.stream.CloseWithError(err)
		}
	}
	close(p.finished)
}

func (p *stdioProcess) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (p *stdioProcess) write(ctx context.Context, body []byte) error {
	result := make(chan error, 1)
	go func() {
		p.writeMu.Lock()
		defer p.writeMu.Unlock()
		if p.isClosed() {
			result <- errProcessClosed
			return
		}
		if _, err := p.stdin.Write(append(body, '\n')); err != nil {
			p.fail(fmt.Errorf("writing to stdio MCP server: %w", err))
			result <- err
			return
		}
		result <- nil
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		// A child that stops reading stdin can otherwise strand this write.
		p.abort(ctx.Err())
		return ctx.Err()
	}
}

func (p *stdioProcess) cancel(ctx context.Context, id string) error {
	message, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  methodCancelled,
		"params":  map[string]any{"requestId": id},
	})
	if err != nil {
		return err
	}
	return p.write(ctx, message)
}

func (p *stdioProcess) read(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxStdioMessage)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var message stdioMessage
		if err := json.Unmarshal(line, &message); err != nil {
			continue
		}
		if len(message.ID) == 0 {
			p.progress(line, message)
			continue
		}
		var id string
		if err := json.Unmarshal(message.ID, &id); err != nil {
			continue
		}
		pending, ok := p.take(id)
		if !ok {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			if pending.stream != nil {
				pending.finish(err)
			} else {
				pending.result <- stdioResult{err: err}
				close(pending.finished)
			}
			continue
		}
		raw["id"] = pending.originalID
		rewritten, err := json.Marshal(raw)
		if pending.stream != nil {
			if err == nil {
				err = writeSSE(pending.stream, rewritten)
			}
			pending.finish(err)
			continue
		}
		pending.result <- stdioResult{body: rewritten, err: err}
		close(pending.finished)
	}
	if err := scanner.Err(); err != nil {
		p.fail(fmt.Errorf("reading stdio MCP server: %w", err))
	}
}

func (p *stdioProcess) progress(line []byte, message stdioMessage) {
	if message.Method != "notifications/progress" {
		return
	}
	token := bytes.TrimSpace(message.Params["progressToken"])
	if len(token) == 0 {
		return
	}
	p.mu.Lock()
	var target *stdioPending
	for _, pending := range p.pending {
		if pending.stream != nil && bytes.Equal(pending.progress, token) {
			match := pending
			target = &match
			break
		}
	}
	p.mu.Unlock()
	if target != nil {
		_ = writeSSE(target.stream, line)
	}
}

func writeSSE(w io.Writer, message []byte) error {
	_, err := fmt.Fprintf(w, "data: %s\n\n", message)
	return err
}

func (p *stdioProcess) log(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxStdioMessage)
	for scanner.Scan() {
		log.Printf("mcp-gateway stdio %s: %s", p.cmd.Path, scanner.Text())
	}
}

func (p *stdioProcess) wait() {
	defer close(p.exited)
	err := p.cmd.Wait()
	if err == nil {
		err = errors.New("stdio MCP server exited")
	} else {
		err = fmt.Errorf("stdio MCP server exited: %w", err)
	}
	p.fail(err)
}

func (p *stdioProcess) abort(err error) {
	_ = p.stdin.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	p.fail(err)
}

func (p *stdioProcess) fail(err error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	pending := p.pending
	p.pending = map[string]stdioPending{}
	p.mu.Unlock()

	p.owner.mu.Lock()
	if p.owner.process == p {
		p.owner.process = nil
	}
	p.owner.mu.Unlock()
	for _, request := range pending {
		if request.stream != nil {
			request.finish(err)
		} else {
			request.result <- stdioResult{err: err}
			close(request.finished)
		}
	}
	close(p.done)
}

func (p *stdioProcess) stop() error {
	err := p.stdin.Close()
	if err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	select {
	case <-p.exited:
		return nil
	case <-time.After(2 * time.Second):
		if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		<-p.exited
		return nil
	}
}

func (u *stdioUpstream) Close() error {
	u.mu.Lock()
	u.closed = true
	p := u.process
	u.process = nil
	u.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.stop()
}

func (u *stdioUpstream) Delete(context.Context, http.Header) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusMethodNotAllowed, Status: "405 Method Not Allowed", Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil))}, nil
}
