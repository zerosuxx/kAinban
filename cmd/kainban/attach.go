package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// runAttach runs argv in a pod container with this terminal attached (TTY,
// raw mode, resize), like `kubectl exec -it`, using client-go so the image
// needs no kubectl. The board's `t`/`T` call it.
func runAttach(ctx context.Context, cf commonFlags, pod, container string, argv []string, stderr io.Writer) int {
	t, err := cf.connect()
	if err != nil {
		fmt.Fprintf(stderr, "kainban: %v\n", err)
		return 2
	}
	req := t.Client.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(t.Namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   argv,
			Stdin:     true,
			Stdout:    true,
			TTY:       true,
		}, scheme.ParameterCodec)

	// WebSocket first (current API servers), SPDY as the fallback.
	ws, err := remotecommand.NewWebSocketExecutor(t.Config, "GET", req.URL().String())
	if err != nil {
		fmt.Fprintf(stderr, "kainban: %v\n", err)
		return 1
	}
	spdy, err := remotecommand.NewSPDYExecutor(t.Config, "POST", req.URL())
	if err != nil {
		fmt.Fprintf(stderr, "kainban: %v\n", err)
		return 1
	}
	executor, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		fmt.Fprintf(stderr, "kainban: %v\n", err)
		return 1
	}

	fd := int(os.Stdin.Fd())
	var sizes remotecommand.TerminalSizeQueue
	if term.IsTerminal(fd) {
		state, err := term.MakeRaw(fd)
		if err != nil {
			fmt.Fprintf(stderr, "kainban: %v\n", err)
			return 1
		}
		defer term.Restore(fd, state)
		q := newSizeQueue(fd)
		defer q.stop()
		sizes = q
	}
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             os.Stdin,
		Stdout:            os.Stdout,
		Tty:               true,
		TerminalSizeQueue: sizes,
	})
	if err != nil {
		term.Restore(fd, nil) // best effort before printing
		fmt.Fprintf(stderr, "\r\nkainban: %v\r\n", err)
		return 1
	}
	return 0
}

// sizeQueue reports the terminal size at start and on every SIGWINCH.
type sizeQueue struct {
	fd    int
	ch    chan remotecommand.TerminalSize
	sig   chan os.Signal
	done  chan struct{}
	first bool
}

func newSizeQueue(fd int) *sizeQueue {
	q := &sizeQueue{fd: fd, ch: make(chan remotecommand.TerminalSize, 1),
		sig: make(chan os.Signal, 1), done: make(chan struct{})}
	signal.Notify(q.sig, syscall.SIGWINCH)
	go func() {
		for {
			select {
			case <-q.sig:
				if s, ok := q.size(); ok {
					select {
					case q.ch <- s:
					default: // a newer size will follow
					}
				}
			case <-q.done:
				return
			}
		}
	}()
	return q
}

func (q *sizeQueue) size() (remotecommand.TerminalSize, bool) {
	w, h, err := term.GetSize(q.fd)
	if err != nil {
		return remotecommand.TerminalSize{}, false
	}
	return remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}, true
}

// Next blocks until the next size; nil ends the stream of sizes.
func (q *sizeQueue) Next() *remotecommand.TerminalSize {
	if !q.first {
		q.first = true
		if s, ok := q.size(); ok {
			return &s
		}
	}
	select {
	case s := <-q.ch:
		return &s
	case <-q.done:
		return nil
	}
}

func (q *sizeQueue) stop() {
	signal.Stop(q.sig)
	close(q.done)
}
