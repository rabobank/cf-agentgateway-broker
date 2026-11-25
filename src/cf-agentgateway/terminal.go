package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

func readHiddenLine(ctx context.Context, input *os.File, prompt func()) (line []byte, err error) {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd := int(input.Fd())
	previous, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	defer func() {
		if restore := term.Restore(fd, previous); restore != nil && err == nil {
			line, err = nil, restore
		}
	}()
	prompt()
	type result struct {
		line []byte
		err  error
	}
	done := make(chan result, 1)
	// The command exits on cancellation. A pending terminal read may remain
	// blocked until then, but it never owns or changes the terminal mode.
	go func() {
		var line []byte
		var b [1]byte
		for {
			if err := ctx.Err(); err != nil {
				done <- result{err: err}
				return
			}
			if _, err := io.ReadFull(input, b[:]); err != nil {
				done <- result{err: err}
				return
			}
			switch b[0] {
			case '\r', '\n':
				done <- result{line: line}
				return
			case 3, 4: // Ctrl-C, Ctrl-D in raw mode.
				done <- result{err: context.Canceled}
				return
			case 8, 127:
				if len(line) > 0 {
					line = line[:len(line)-1]
				}
			default:
				if len(line) == 16<<10 {
					done <- result{err: errors.New("OAuth callback exceeds input limit")}
					return
				}
				line = append(line, b[0])
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return res.line, res.err
	}
}
