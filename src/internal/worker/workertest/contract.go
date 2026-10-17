package workertest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compression"
	"overseer/internal/worker"
)

// Target is an adapter under contract test.
type Target struct {
	// New returns a fresh, unconnected worker.
	New func(t *testing.T) worker.Worker
	// DelayNext makes the next send reply only after d (timeout and
	// cancellation checks).
	DelayNext func(d time.Duration)
	// Dir is a directory the worker accepts.
	Dir string
}

// Prompt returns a valid immutable worker prompt.
func Prompt(content string) compression.WorkerPrompt {
	return compression.WorkerPrompt{ID: uuid.New(), Content: content, Source: compression.SourceCanonical,
		Hash: compression.WorkerPromptHash(content, compression.SourceCanonical), CreatedAt: time.Now().UTC()}
}

// Request returns a valid request for project.
func Request(project uuid.UUID, content string) worker.WorkerRequest {
	p := Prompt(content)
	return worker.WorkerRequest{RequestID: uuid.New(), ProjectID: project, Prompt: p,
		Metadata: worker.WorkerRequestMetadata{RuntimeStateID: uuid.New(), StateKey: "IMPLEMENT", RoleKey: "ENGINEER", PromptHash: p.Hash, Attempt: 1}}
}

func code(err error) worker.WorkerErrorCode {
	var we *worker.WorkerError
	if errors.As(err, &we) {
		return we.Code
	}
	return ""
}

func delivery(err error) worker.DeliveryState {
	var we *worker.WorkerError
	if errors.As(err, &we) {
		return we.Delivery
	}
	return ""
}

// ready connects (when supported) and opens a session (when supported).
func ready(t *testing.T, tg Target) (worker.Worker, worker.WorkerCapabilities) {
	t.Helper()
	ctx := context.Background()
	w := tg.New(t)
	t.Cleanup(func() { _ = w.Close(context.Background()) })
	if c, ok := w.(worker.Connector); ok {
		if err := c.Connect(ctx); err != nil {
			t.Fatalf("connect: %v", err)
		}
	}
	caps, err := w.Capabilities(ctx)
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if caps.NewSession {
		if _, err := w.NewSession(ctx, worker.NewSessionRequest{ProjectID: uuid.New()}); err != nil {
			t.Fatalf("new session: %v", err)
		}
	}
	return w, caps
}

// RunContract runs the generic worker contract (WBS 12.69); checks for
// unsupported capabilities are skipped, never failed.
func RunContract(t *testing.T, tg Target) {
	ctx := context.Background()
	t.Run("connect", func(t *testing.T) {
		w := tg.New(t)
		defer w.Close(ctx)
		c, ok := w.(worker.Connector)
		if !ok {
			t.Skip("no explicit connection")
		}
		if c.ConnectionState() == worker.ConnConnected {
			t.Fatal("connected before Connect")
		}
		if err := c.Connect(ctx); err != nil || c.ConnectionState() != worker.ConnConnected {
			t.Fatalf("connect: %v state %s", err, c.ConnectionState())
		}
	})
	t.Run("capabilities", func(t *testing.T) {
		w, caps := ready(t, tg)
		again, _ := w.Capabilities(ctx)
		if caps.Provider == "" || caps != again {
			t.Fatalf("capabilities not deterministic: %+v vs %+v", caps, again)
		}
	})
	t.Run("new_session", func(t *testing.T) {
		w, caps := ready(t, tg)
		if !caps.NewSession {
			t.Skip("no session capability")
		}
		a, _ := w.Session(ctx)
		b, err := w.NewSession(ctx, worker.NewSessionRequest{ProjectID: uuid.New()})
		if err != nil || b.ID == "" || b.ID == a.ID {
			t.Fatalf("new session %q vs %q: %v", b.ID, a.ID, err)
		}
		if b.WorkingDirectory != nil {
			t.Fatal("new session inherited a working directory")
		}
	})
	t.Run("change_directory", func(t *testing.T) {
		w, caps := ready(t, tg)
		if !caps.ChangeDirectory {
			t.Skip("no cwd capability")
		}
		if err := w.ChangeDirectory(ctx, worker.ChangeDirectoryRequest{Path: ""}); err == nil {
			t.Fatal("empty directory accepted")
		}
		if s, _ := w.Session(ctx); s.WorkingDirectory != nil {
			t.Fatal("failed cd changed the confirmed directory")
		}
		if err := w.ChangeDirectory(ctx, worker.ChangeDirectoryRequest{Path: tg.Dir}); err != nil {
			t.Fatalf("cd: %v", err)
		}
		if s, _ := w.Session(ctx); s.WorkingDirectory == nil || *s.WorkingDirectory != tg.Dir {
			t.Fatal("cd not confirmed")
		}
	})
	t.Run("send_and_usage", func(t *testing.T) {
		w, caps := ready(t, tg)
		req := Request(uuid.New(), "implement unit 12.1")
		resp, err := w.Send(ctx, req)
		if err != nil || resp.RequestID != req.RequestID || resp.Content == "" {
			t.Fatalf("send: %+v %v", resp, err)
		}
		if caps.Sessions {
			s, _ := w.Session(ctx)
			if resp.Session.ID != s.ID {
				t.Fatal("response from another session")
			}
		}
		if caps.ContextTelemetry && (!resp.Usage.HasContext() || resp.Usage.Estimated) {
			t.Fatalf("context telemetry advertised but not reported: %+v", resp.Usage)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		if tg.DelayNext == nil {
			t.Skip("no delay control")
		}
		w, _ := ready(t, tg)
		tg.DelayNext(2 * time.Second)
		tctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := w.Send(tctx, Request(uuid.New(), "slow"))
		if code(err) != worker.CodeTimeout || time.Since(start) > 1500*time.Millisecond {
			t.Fatalf("timeout not enforced: %v", err)
		}
		if delivery(err) == worker.DeliveryNotSent {
			t.Fatal("transmitted request reported as definitely not sent")
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		w, _ := ready(t, tg)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := w.Send(cctx, Request(uuid.New(), "never"))
		if code(err) != worker.CodeCancelled || delivery(err) != worker.DeliveryNotSent {
			t.Fatalf("pre-send cancellation: %v", err)
		}
	})
	t.Run("close", func(t *testing.T) {
		w, _ := ready(t, tg)
		if err := w.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if c, ok := w.(worker.Connector); ok && c.ConnectionState() != worker.ConnClosed {
			t.Fatalf("state after close: %s", c.ConnectionState())
		}
		if _, err := w.Send(ctx, Request(uuid.New(), "late")); code(err) != worker.CodeClosed {
			t.Fatalf("send after close: %v", err)
		}
	})
}
