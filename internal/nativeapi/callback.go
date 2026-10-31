package nativeapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

type CallbackResult struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// Callback supervises the Rust loopback OAuth API. Authorization state and
// results travel only through private inherited pipes, never process argv.
type Callback struct {
	command *exec.Cmd
	Results chan CallbackResult
	done    chan struct{}
	once    sync.Once
}

func StartCallback(ctx context.Context, port int, state string) (*Callback, error) {
	path, cleanup, err := Prepare()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	command := exec.Command(path, "oauth-callback", "--port", strconv.Itoa(port), "--parent-pid", strconv.Itoa(os.Getpid()))
	command.Stderr = os.Stderr
	input, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		input.Close()
		return nil, err
	}
	if err := command.Start(); err != nil {
		input.Close()
		output.Close()
		return nil, err
	}
	cleanup()
	callback := &Callback{command: command, Results: make(chan CallbackResult, 1), done: make(chan struct{})}
	ready := make(chan error, 1)
	go func() {
		defer close(callback.done)
		defer output.Close()
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 32<<10)
		if !scanner.Scan() {
			ready <- errors.New("native OAuth callback did not start")
			_ = command.Wait()
			return
		}
		var acknowledgement struct {
			Ready bool `json:"ready"`
		}
		if json.Unmarshal(scanner.Bytes(), &acknowledgement) != nil || !acknowledgement.Ready {
			ready <- errors.New("native OAuth callback returned invalid readiness")
			_ = command.Process.Kill()
			_ = command.Wait()
			return
		}
		ready <- nil
		var result CallbackResult
		if !scanner.Scan() || json.Unmarshal(scanner.Bytes(), &result) != nil {
			result.Error = "native OAuth callback ended without a result"
		}
		callback.Results <- result
		_ = command.Wait()
	}()
	if err := json.NewEncoder(input).Encode(map[string]string{"state": state}); err != nil {
		input.Close()
		callback.Close()
		return nil, err
	}
	input.Close()
	select {
	case err := <-ready:
		if err != nil {
			callback.Close()
			return nil, err
		}
		return callback, nil
	case <-ctx.Done():
		callback.Close()
		return nil, ctx.Err()
	case <-time.After(5 * time.Second):
		callback.Close()
		return nil, fmt.Errorf("native OAuth callback did not become ready")
	}
}
func (c *Callback) Close() {
	c.once.Do(func() {
		_ = c.command.Process.Signal(os.Interrupt)
		select {
		case <-c.done:
		case <-time.After(4 * time.Second):
			_ = c.command.Process.Kill()
			<-c.done
		}
	})
}
