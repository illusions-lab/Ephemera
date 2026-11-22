// Package control provides an owner-only drain lease over a local socket.
package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/illusions-lab/Ephemera/internal/status"
)

type Identity struct {
	Instance string
	Request  int64
}

type Job struct {
	Identity
	Repository, Name, Runner string
	Run                      int64
	Started                  time.Time
}

type Snapshot struct {
	Jobs    []Job
	Pending bool
	Ready   bool
	Changed bool
}

type Request struct {
	Abort []Identity
	View  []Identity
}

func Path(root string) string { return filepath.Join(root, "ctl.sock") }

// Listen allows one lease at a time. Closing it resumes normal intake.
func Listen(ctx context.Context, root string, serve func(context.Context, <-chan Request, chan<- Snapshot)) (func(), error) {
	path := Path(root)
	if err := status.RemoveStale(path, root); err != nil {
		return nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		l.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		lease := make(chan struct{}, 1)
		for {
			conn, err := l.AcceptUnix()
			if err != nil {
				return
			}
			select {
			case lease <- struct{}{}:
				go func() {
					defer func() { <-lease }()
					session(ctx, conn, serve)
				}()
			default:
				conn.Close()
			}
		}
	}()
	return func() { cancel(); l.Close() }, nil
}

func session(ctx context.Context, conn net.Conn, serve func(context.Context, <-chan Request, chan<- Snapshot)) {
	defer conn.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	requests, replies := make(chan Request), make(chan Snapshot)
	go func() {
		defer cancel()
		lines := bufio.NewScanner(conn)
		lines.Buffer(make([]byte, 4096), 4<<20)
		for lines.Scan() {
			var req Request
			if json.Unmarshal(lines.Bytes(), &req) != nil {
				return
			}
			select {
			case requests <- req:
			case <-ctx.Done():
				return
			}
		}
	}()
	served := make(chan struct{})
	go func() {
		defer close(served)
		defer cancel()
		serve(ctx, requests, replies)
	}()
	defer func() {
		cancel()
		<-served
	}()
	encoder := json.NewEncoder(conn)
	for {
		select {
		case <-ctx.Done():
			return
		case reply := <-replies:
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if encoder.Encode(reply) != nil {
				return
			}
		}
	}
}

// Drain holds the lease until the caller returns from the service operation.
func Drain(ctx context.Context, root string, input *os.File, output *os.File, operation func() error) error {
	return drain(ctx, root, input, output, operation)
}

// DrainWait waits for active jobs without offering forced aborts.
func DrainWait(ctx context.Context, root string, output *os.File, operation func() error) error {
	return drain(ctx, root, nil, output, operation)
}

func drain(ctx context.Context, root string, input *os.File, output *os.File, operation func() error) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", Path(root))
	if err != nil {
		return fmt.Errorf("open drain socket: %w. Keep the candidate staged and stop the old service in a maintenance window after confirming its jobs have finished", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	encoder, decoder := json.NewEncoder(conn), json.NewDecoder(conn)
	exchange := func(req Request) (Snapshot, error) {
		var snapshot Snapshot
		if err := encoder.Encode(req); err != nil {
			return snapshot, err
		}
		err := decoder.Decode(&snapshot)
		return snapshot, err
	}
	interactive := input != nil && term.IsTerminal(input.Fd()) && term.IsTerminal(output.Fd())
	reader := bufio.NewReader(input)
	var previous []Identity
	printed := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		snapshot, err := exchange(Request{})
		if err != nil {
			return fmt.Errorf("read drain state: %w", err)
		}
		if snapshot.Pending {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
				continue
			}
		}
		view := identities(snapshot.Jobs)
		if !printed || !slices.Equal(view, previous) {
			printed = true
			previous = view
			for _, job := range snapshot.Jobs {
				elapsed := "unknown"
				if !job.Started.IsZero() {
					elapsed = time.Since(job.Started).Round(time.Second).String()
				}
				fmt.Fprintf(output, "repository=%q run=%d job=%q runner=%s duration=%s instance=%s request=%d\n",
					job.Repository, job.Run, job.Name, job.Runner, elapsed, job.Instance, job.Request)
			}
			var abort []Identity
			for _, id := range view {
				if id.Request > 0 {
					abort = append(abort, id)
				}
			}
			if interactive && len(view) > 0 {
				if len(abort) > 0 {
					fmt.Fprint(output, "Press Enter to wait, type abort to reclaim the listed jobs with request IDs, or type quit to resume intake: ")
				} else {
					fmt.Fprint(output, "Press Enter to wait or type quit to resume intake: ")
				}
				line := make(chan string, 1)
				go func() { answer, _ := reader.ReadString('\n'); line <- answer }()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case answer := <-line:
					switch answer {
					case "quit\n":
						return errors.New("service operation cancelled")
					case "abort\n":
						if len(abort) == 0 {
							break
						}
						snapshot, err = exchange(Request{View: view, Abort: abort})
						if err != nil {
							return err
						}
						if snapshot.Changed {
							fmt.Fprintln(output, "The job list changed. Review the current jobs again.")
							printed = false
							continue
						}
					}
				}
			}
		}
		if snapshot.Ready {
			if err := ctx.Err(); err != nil {
				return err
			}
			return operation()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func identities(jobs []Job) []Identity {
	ids := make([]Identity, 0, len(jobs))
	for _, job := range jobs {
		ids = append(ids, job.Identity)
	}
	return ids
}
