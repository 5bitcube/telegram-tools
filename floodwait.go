package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// maxFloodWait is the longest FLOOD_WAIT we sleep through before giving up.
const maxFloodWait = 30 * time.Minute

// floodWaitMiddleware retries a request after sleeping when Telegram answers
// FLOOD_WAIT_N (built on tgerr.FloodWait, which does the waiting).
func floodWaitMiddleware() telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			for attempt := 0; ; attempt++ {
				err := next.Invoke(ctx, input, output)
				if err == nil {
					return nil
				}
				wait, ok := telegram.AsFloodWait(err)
				if !ok {
					return err
				}
				if wait > maxFloodWait || attempt >= 5 {
					return fmt.Errorf("telegram asks to wait %s (FLOOD_WAIT); try again later: %w", wait, err)
				}
				fmt.Fprintf(os.Stderr, "FLOOD_WAIT: sleeping %s before retrying\n", wait)
				if retry, werr := tgerr.FloodWait(ctx, err); !retry {
					return werr
				}
			}
		}
	})
}

// newFlagSet creates a flag set whose --help prints the given header plus the flags.
func newFlagSet(name, header string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), header)
		fmt.Fprintln(fs.Output())
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
	}
	return fs
}
