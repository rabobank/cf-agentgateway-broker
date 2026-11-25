//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

func TestHiddenInputTimeoutRestoresTerminalWithoutInput(t *testing.T) {
	master, input, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer input.Close()
	fd := int(input.Fd())
	before, err := term.GetState(fd)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := readHiddenLine(ctx, input, func() {})
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected the login deadline, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal read did not return after deadline")
	}
	after, err := term.GetState(fd)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("terminal settings not restored: %v", err)
	}
}
