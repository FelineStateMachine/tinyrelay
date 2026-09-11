package agentrunner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestACPHandshakeAndStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := RunProcess(ctx, ProcessOptions{Command: os.Args[0], Args: []string{"-test.run=TestACPHelper"}, Env: append(os.Environ(), "TINY_ACP_HELPER=1"), CWD: t.TempDir(), Protocol: "acp"}, "hello")
	if err != nil || result != "hello from agent" {
		t.Fatalf("result=%q err=%v", result, err)
	}
}

func TestACPCancellationNotifiesAgentBeforeExit(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cancelled")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := RunProcess(ctx, ProcessOptions{Command: os.Args[0], Args: []string{"-test.run=TestACPHelper"}, Env: append(os.Environ(), "TINY_ACP_HELPER=1", "TINY_ACP_CANCEL_HELPER=1", "TINY_ACP_CANCEL_MARKER="+marker), CWD: t.TempDir(), Protocol: "acp"}, "hello")
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("agent did not receive cancellation: %v", err)
	}
}

func TestCommandCleansUpDescendantProcess(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-survived")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := RunProcess(ctx, ProcessOptions{Command: "sh", Args: []string{"-c", "(sleep 0.5; touch \"$1\") & wait", "descendant", marker}, CWD: t.TempDir(), Protocol: "command"}, "hello")
	if err == nil {
		t.Fatal("expected cancellation")
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("descendant survived")
	}
}
func TestACPHelper(t *testing.T) {
	if os.Getenv("TINY_ACP_HELPER") != "1" && os.Getenv("TINY_ACP_CANCEL_HELPER") != "1" {
		return
	}
	scan := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scan.Scan() {
		var message map[string]any
		if json.Unmarshal(scan.Bytes(), &message) != nil {
			os.Exit(2)
		}
		params, _ := message["params"].(map[string]any)
		switch message["method"] {
		case "initialize":
			if params["protocolVersion"] != float64(1) {
				os.Exit(3)
			}
			encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": message["id"], "result": map[string]any{"protocolVersion": 1}})
		case "session/new":
			if params["cwd"] == nil || params["mcpServers"] == nil {
				os.Exit(4)
			}
			encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": message["id"], "result": map[string]any{"sessionId": "test-session"}})
		case "session/prompt":
			if params["sessionId"] != "test-session" || params["prompt"] == nil {
				os.Exit(5)
			}
			if os.Getenv("TINY_ACP_CANCEL_HELPER") == "1" {
				continue
			}
			encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "test-session", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "hello from agent"}}}})
			encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": message["id"], "result": map[string]any{"stopReason": "end_turn"}})
		case "session/cancel":
			if marker := os.Getenv("TINY_ACP_CANCEL_MARKER"); marker != "" {
				_ = os.WriteFile(marker, []byte("cancelled"), 0600)
			}
			os.Exit(0)
		}
	}
	os.Exit(0)
}
