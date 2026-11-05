// Command fakechild is a tiny stand-in for a runtime binary in the pool
// worker's tests: it speaks the worker frame protocol without a Collector.
// Its runtime identity is the digest of "fake runtime <variant>", variant
// being set at build time, and what it does
// with a request is named by the request's inline bundle (execute) or input
// (inspect and ottl).
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

type wireError struct {
	Kind    string `json:"kind,omitempty"`
	Message string `json:"message"`
}

type message struct {
	ID               uint64          `json:"id,omitempty"`
	Type             string          `json:"type"`
	Hello            map[string]any  `json:"hello,omitempty"`
	Bundle           []byte          `json:"bundle,omitempty"`
	BundleURL        string          `json:"bundle_url,omitempty"`
	ResultURL        string          `json:"result_url,omitempty"`
	RunID            string          `json:"run_id,omitempty"`
	Binary           json.RawMessage `json:"binary,omitempty"`
	Input            json.RawMessage `json:"input,omitempty"`
	Event            json.RawMessage `json:"event,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
	Error            *wireError      `json:"error,omitempty"`
	DeadlineExceeded bool            `json:"deadline_exceeded,omitempty"`
}

// variant is set at build time so each variant is a different binary.
var variant string

var (
	outMu sync.Mutex
	out   = os.Stdout
)

func write(m *message) {
	body, _ := json.Marshal(m)
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	outMu.Lock()
	defer outMu.Unlock()
	_, _ = out.Write(frame)
}

func read(r io.Reader) (*message, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return nil, err
	}
	body := make([]byte, binary.BigEndian.Uint32(size[:]))
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var m message
	return &m, json.Unmarshal(body, &m)
}

func main() {
	sum := sha256.Sum256([]byte("fake runtime " + variant))
	identity := "sha256:" + hex.EncodeToString(sum[:])
	switch os.Getenv("FAKE_CHILD_MODE") {
	case "die":
		fmt.Fprintln(os.Stderr, "fake runtime failed to load")
		os.Exit(3)
	}
	protocol := 2
	if os.Getenv("FAKE_CHILD_PROTOCOL") == "99" {
		protocol = 99
	}
	fmt.Fprintln(os.Stderr, "fake runtime ready")
	write(&message{Type: "hello", Hello: map[string]any{"protocol": protocol, "runtime": identity, "product_version": "fake-" + variant}})

	var mu sync.Mutex
	cancelled := map[uint64]chan bool{}
	var wg sync.WaitGroup
	for {
		m, err := read(os.Stdin)
		if err != nil {
			break
		}
		if m.Type == "cancel" {
			mu.Lock()
			ch := cancelled[m.ID]
			mu.Unlock()
			if ch != nil {
				ch <- m.DeadlineExceeded
			}
			continue
		}
		ch := make(chan bool, 1)
		mu.Lock()
		cancelled[m.ID] = ch
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle(m, ch)
		}()
	}
	wg.Wait()
}

func handle(m *message, cancel chan bool) {
	what := string(m.Bundle)
	if m.Type != "execute" {
		what = strings.Trim(string(m.Input), `"`)
	}
	switch what {
	case "crash":
		write(&message{ID: m.ID, Type: "event", Event: json.RawMessage(`{"n":"before-crash"}`)})
		os.Exit(7)
	case "slow":
		write(&message{ID: m.ID, Type: "event", Event: json.RawMessage(`{"n":"started"}`)})
		exceeded := <-cancel
		kind, text := "cancelled", "context canceled"
		if exceeded {
			kind, text = "deadline_exceeded", "context deadline exceeded"
		}
		write(&message{ID: m.ID, Type: "result", Error: &wireError{Kind: kind, Message: text}})
	case "env":
		var env []string
		for _, kv := range os.Environ() {
			if strings.HasPrefix(strings.ToUpper(kv), "LINEA_") || strings.HasPrefix(kv, "POOL_TEST_") {
				env = append(env, kv)
			}
		}
		raw, _ := json.Marshal(map[string]any{"env": env})
		write(&message{ID: m.ID, Type: "result", Result: raw})
	case "report":
		write(&message{ID: m.ID, Type: "result", Result: json.RawMessage(`{}`)})
	case "fail":
		write(&message{ID: m.ID, Type: "result", Error: &wireError{Kind: "bundle", Message: "the fake runtime refuses this"}})
	default:
		write(&message{ID: m.ID, Type: "event", Event: json.RawMessage(`{"n":1}`)})
		write(&message{ID: m.ID, Type: "event", Event: json.RawMessage(`{"n":2}`)})
		raw, _ := json.Marshal(map[string]any{
			"type":       m.Type,
			"what":       what,
			"id":         m.ID,
			"run_id":     m.RunID,
			"bundle_url": m.BundleURL,
			"result_url": m.ResultURL,
			"has_binary": len(m.Binary) > 0,
		})
		write(&message{ID: m.ID, Type: "result", Result: raw})
	}
}
