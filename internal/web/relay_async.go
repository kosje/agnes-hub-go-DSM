package web

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"

	"agneshub/internal/relay"
)

type relayOutcome struct {
	result *relay.Result
	err    error
}

// A response is owned by either the handler or the canceled worker, never left
// in an unread buffered channel holding an account's concurrency token.
func (s *Server) relayAsync(ctx context.Context, opts relay.Options) <-chan relayOutcome {
	ch := make(chan relayOutcome)
	go func() {
		out := relayOutcome{}
		func() {
			defer func() {
				if e := recover(); e != nil {
					if s.Hub != nil {
						s.Hub.Metrics.PanicsTotal.Add(1)
					}
					log.Printf("[panic-recovered] async relay: %v\n%s", e, debug.Stack())
					out.err = fmt.Errorf("网关内部错误：请求处理失败")
				}
			}()
			out.result, out.err = relay.Do(ctx, s.Hub, s.Client, opts)
		}()
		select {
		case ch <- out:
		case <-ctx.Done():
			if out.result != nil {
				out.result.Close()
			}
		}
	}()
	return ch
}
