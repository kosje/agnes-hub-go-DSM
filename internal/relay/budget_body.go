package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
)

func responseNeurons(raw []byte) float64 {
	var value struct {
		Usage struct {
			Neurons float64 `json:"neurons"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Usage.Neurons <= 0 {
		return 0
	}
	return value.Usage.Neurons
}

// Each upstream round owns its budget accounting, including a continuation
// routed to a different account. SSE usage is cumulative within that round.
type budgetBody struct {
	io.ReadCloser
	pending []byte
	data    []string
	used    float64
	charge  func(float64)
	once    sync.Once
}

func (b *budgetBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.pending = append(b.pending, p[:n]...)
	for {
		line, rest, ok := bytes.Cut(b.pending, []byte{'\n'})
		if !ok {
			break
		}
		b.pending = rest
		text := strings.TrimSuffix(string(line), "\r")
		if text == "" {
			if used := responseNeurons([]byte(strings.Join(b.data, "\n"))); used > b.used {
				b.used = used
			}
			b.data = nil
		} else if strings.HasPrefix(text, "data:") {
			b.data = append(b.data, strings.TrimSpace(strings.TrimPrefix(text, "data:")))
		}
	}
	if len(b.pending) > 4<<20 {
		b.pending = nil
		b.data = nil
	}
	return n, err
}

func (b *budgetBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() {
		if strings.HasPrefix(string(b.pending), "data:") {
			b.data = append(b.data, strings.TrimSpace(strings.TrimPrefix(string(b.pending), "data:")))
		}
		if used := responseNeurons([]byte(strings.Join(b.data, "\n"))); used > b.used {
			b.used = used
		}
		if b.used > 0 {
			b.charge(b.used)
		}
	})
	return err
}
