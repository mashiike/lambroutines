package lambroutines

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeExtensionsAPI is a minimal stand-in for the Lambda Extensions API.
// event/next blocks until the test sends an event type on respond, and
// reports each poll on polled so the test can observe when loop() advances.
type fakeExtensionsAPI struct {
	polled  chan struct{}
	respond chan string

	// registeredExtensionName is written by the register handler before it
	// writes the HTTP response, and read by tests only after the client call
	// that triggered registration (register(), via Start()) has returned; the
	// HTTP round trip provides the necessary synchronization.
	registeredExtensionName string
}

func newFakeExtensionsAPI() *fakeExtensionsAPI {
	return &fakeExtensionsAPI{
		polled:  make(chan struct{}),
		respond: make(chan string),
	}
}

func (f *fakeExtensionsAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/2020-01-01/extension/register", func(w http.ResponseWriter, r *http.Request) {
		f.registeredExtensionName = r.Header.Get("Lambda-Extension-Name")
		w.Header().Set("Lambda-Extension-Identifier", "test-extension-id")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	})
	mux.HandleFunc("/2020-01-01/extension/event/next", func(w http.ResponseWriter, r *http.Request) {
		f.polled <- struct{}{}
		eventType := <-f.respond
		_ = json.NewEncoder(w).Encode(map[string]string{"eventType": eventType})
	})
	return mux
}

func newTestExtension(t *testing.T, opts ...Option) (*Extension, *fakeExtensionsAPI) {
	t.Helper()
	api := newFakeExtensionsAPI()
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)

	t.Setenv("AWS_LAMBDA_RUNTIME_API", strings.TrimPrefix(server.URL, "http://"))

	e, err := Start(opts...)
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	if e.local {
		t.Fatal("expected Extension to not be in local mode when AWS_LAMBDA_RUNTIME_API is set")
	}
	return e, api
}

// awaitPoll waits for loop() to reach its next /next call, failing the test
// if it doesn't happen in time. It distinguishes "loop() advanced" from
// "loop() is stuck waiting on a stale completion signal".
func awaitPoll(t *testing.T, api *fakeExtensionsAPI) {
	t.Helper()
	select {
	case <-api.polled:
	case <-time.After(2 * time.Second):
		t.Fatal("loop() never issued the next event/next call")
	}
}

func TestExtension_HandlerCompletesBeforeExtensionDeliversInvoke(t *testing.T) {
	e, api := newTestExtension(t)

	awaitPoll(t, api) // initial poll right after registration

	var bgDone atomic.Bool
	e.Go(func(context.Context) error {
		time.Sleep(100 * time.Millisecond)
		bgDone.Store(true)
		return nil
	})
	e.handlerCompleted() // handler returns before the extension even sees INVOKE

	api.respond <- "INVOKE"

	awaitPoll(t, api) // loop() must still notice completion and wait for inflight work
	if !bgDone.Load() {
		t.Error("loop() polled again before the background task finished")
	}
	api.respond <- "SHUTDOWN"
}

func TestExtension_ExtensionSeesInvokeBeforeHandlerCompletes(t *testing.T) {
	e, api := newTestExtension(t)

	awaitPoll(t, api)
	api.respond <- "INVOKE" // loop() now waits for a completion that hasn't happened yet

	time.Sleep(50 * time.Millisecond) // give loop() a chance to start waiting

	var bgDone atomic.Bool
	e.Go(func(context.Context) error {
		time.Sleep(100 * time.Millisecond)
		bgDone.Store(true)
		return nil
	})
	e.handlerCompleted()

	awaitPoll(t, api)
	if !bgDone.Load() {
		t.Error("loop() polled again before the background task finished")
	}
	api.respond <- "SHUTDOWN"
}

func TestExtension_MultipleInvocationsInSequence(t *testing.T) {
	e, api := newTestExtension(t)

	awaitPoll(t, api) // initial poll right after registration

	for i := range 3 {
		var bgDone atomic.Bool
		e.Go(func(context.Context) error {
			time.Sleep(30 * time.Millisecond)
			bgDone.Store(true)
			return nil
		})
		e.handlerCompleted()
		api.respond <- "INVOKE"

		awaitPoll(t, api)
		if !bgDone.Load() {
			t.Errorf("invocation %d: loop() polled again before the background task finished", i)
		}
	}
	api.respond <- "SHUTDOWN"
}

func TestExtension_ShutdownStopsPolling(t *testing.T) {
	e, api := newTestExtension(t)
	_ = e

	awaitPoll(t, api)
	api.respond <- "SHUTDOWN"

	select {
	case <-api.polled:
		t.Fatal("loop() polled again after receiving SHUTDOWN")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestExtensionFromContext_AvailableInsideWrappedHandler(t *testing.T) {
	e, api := newTestExtension(t)
	awaitPoll(t, api)

	var bgDone atomic.Bool
	handler := func(ctx context.Context) error {
		got, ok := ExtensionFromContext(ctx)
		if !ok {
			t.Error("ExtensionFromContext: Extension not found in handler's context")
		}
		if got != e {
			t.Error("ExtensionFromContext: returned a different Extension than the one that wrapped the handler")
		}
		return Go(ctx, func(context.Context) error {
			time.Sleep(100 * time.Millisecond)
			bgDone.Store(true)
			return nil
		})
	}

	wrapped := e.Wrap(handler)
	if _, err := wrapped.Invoke(context.Background(), []byte("null")); err != nil {
		t.Fatalf("Invoke() failed: %v", err)
	}

	api.respond <- "INVOKE"
	awaitPoll(t, api)
	if !bgDone.Load() {
		t.Error("loop() polled again before the background task started via Go(ctx, fn) finished")
	}
	api.respond <- "SHUTDOWN"
}

func TestExtension_Go_PropagatesExtensionToNestedGo(t *testing.T) {
	e, api := newTestExtension(t)
	awaitPoll(t, api)

	var nestedDone atomic.Bool
	e.Go(func(bgCtx context.Context) error {
		return Go(bgCtx, func(context.Context) error {
			time.Sleep(50 * time.Millisecond)
			nestedDone.Store(true)
			return nil
		})
	})
	e.handlerCompleted()
	api.respond <- "INVOKE"

	awaitPoll(t, api)
	if !nestedDone.Load() {
		t.Error("loop() polled again before the nested Go(bgCtx, ...) task finished")
	}
	api.respond <- "SHUTDOWN"
}

func TestExtensionFromContext_NotFoundOutsideWrap(t *testing.T) {
	if _, ok := ExtensionFromContext(context.Background()); ok {
		t.Error("expected no Extension in a bare context.Background()")
	}
}

func TestGo_ReturnsErrorWhenContextHasNoExtension(t *testing.T) {
	err := Go(context.Background(), func(context.Context) error { return nil })
	if !errors.Is(err, ErrNoExtensionInContext) {
		t.Errorf("Go() = %v, want ErrNoExtensionInContext", err)
	}
}

func TestOnLambdaRuntime(t *testing.T) {
	tests := map[string]struct {
		executionEnv string
		runtimeAPI   string
		want         bool
	}{
		"どちらも未設定なら false": {
			executionEnv: "",
			runtimeAPI:   "",
			want:         false,
		},
		"AWS_EXECUTION_ENV が AWS_Lambda で始まれば true": {
			executionEnv: "AWS_Lambda_go1.x",
			runtimeAPI:   "",
			want:         true,
		},
		"AWS_LAMBDA_RUNTIME_API が非空なら true": {
			executionEnv: "",
			runtimeAPI:   "127.0.0.1:9001",
			want:         true,
		},
		"AWS_EXECUTION_ENV が別の値なら false": {
			executionEnv: "CloudFunctions",
			runtimeAPI:   "",
			want:         false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AWS_EXECUTION_ENV", tt.executionEnv)
			t.Setenv("AWS_LAMBDA_RUNTIME_API", tt.runtimeAPI)
			if got := OnLambdaRuntime(); got != tt.want {
				t.Errorf("OnLambdaRuntime() = %v, want %v", got, tt.want)
			}
		})
	}
}

func newLocalTestExtension(t *testing.T, opts ...Option) *Extension {
	t.Helper()
	t.Setenv("AWS_EXECUTION_ENV", "")
	t.Setenv("AWS_LAMBDA_RUNTIME_API", "")

	e, err := Start(opts...)
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	return e
}

func TestExtension_LocalMode_StartIsNoOp(t *testing.T) {
	var logBuf bytes.Buffer
	origOutput := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(origOutput) })

	e := newLocalTestExtension(t)
	if !e.local {
		t.Fatal("expected Extension to be in local mode")
	}

	// If Start() mistakenly started loop() in local mode, it would call
	// next() against an empty runtimeAPI, fail immediately, and log the
	// failure before returning. Give it a chance to do so.
	time.Sleep(50 * time.Millisecond)
	if logBuf.Len() > 0 {
		t.Errorf("Start() appears to have started the extension loop in local mode: %s", logBuf.String())
	}
}

func TestExtension_LocalMode_InvokeWaitsForGo(t *testing.T) {
	e := newLocalTestExtension(t)

	var bgDone atomic.Bool
	handler := func(ctx context.Context) error {
		return Go(ctx, func(context.Context) error {
			time.Sleep(100 * time.Millisecond)
			bgDone.Store(true)
			return nil
		})
	}

	wrapped := e.Wrap(handler)
	if _, err := wrapped.Invoke(context.Background(), []byte("null")); err != nil {
		t.Fatalf("Invoke() failed: %v", err)
	}
	if !bgDone.Load() {
		t.Error("Invoke() returned before the background task started via Go(ctx, fn) finished")
	}
}

func TestExtension_LocalMode_InvokeReturnsImmediatelyWithoutGo(t *testing.T) {
	e := newLocalTestExtension(t)

	wrapped := e.Wrap(func(context.Context) error { return nil })

	start := time.Now()
	if _, err := wrapped.Invoke(context.Background(), []byte("null")); err != nil {
		t.Fatalf("Invoke() failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("Invoke() took %s with no Go() calls; want a near-immediate return", elapsed)
	}
}

func TestExtension_LocalMode_InvokeWaitsForAllGoCalls(t *testing.T) {
	e := newLocalTestExtension(t)

	var fastDone, slowDone atomic.Bool
	handler := func(ctx context.Context) error {
		if err := Go(ctx, func(context.Context) error {
			time.Sleep(30 * time.Millisecond)
			fastDone.Store(true)
			return nil
		}); err != nil {
			return err
		}
		return Go(ctx, func(context.Context) error {
			time.Sleep(150 * time.Millisecond)
			slowDone.Store(true)
			return nil
		})
	}

	wrapped := e.Wrap(handler)
	if _, err := wrapped.Invoke(context.Background(), []byte("null")); err != nil {
		t.Fatalf("Invoke() failed: %v", err)
	}
	if !fastDone.Load() || !slowDone.Load() {
		t.Error("Invoke() returned before all background tasks started via Go(ctx, fn) finished")
	}
}

func TestExtension_LocalMode_InvokeWaitsForNestedGo(t *testing.T) {
	e := newLocalTestExtension(t)

	var nestedDone atomic.Bool
	handler := func(ctx context.Context) error {
		return Go(ctx, func(bgCtx context.Context) error {
			return Go(bgCtx, func(context.Context) error {
				time.Sleep(100 * time.Millisecond)
				nestedDone.Store(true)
				return nil
			})
		})
	}

	wrapped := e.Wrap(handler)
	if _, err := wrapped.Invoke(context.Background(), []byte("null")); err != nil {
		t.Fatalf("Invoke() failed: %v", err)
	}
	if !nestedDone.Load() {
		t.Error("Invoke() returned before the nested Go(bgCtx, ...) task finished")
	}
}

func TestExtension_LocalMode_InvokeWaitsEvenWhenHandlerPanics(t *testing.T) {
	e := newLocalTestExtension(t)

	var bgDone atomic.Bool
	wrapped := e.Wrap(func(ctx context.Context) error {
		_ = Go(ctx, func(context.Context) error {
			time.Sleep(100 * time.Millisecond)
			bgDone.Store(true)
			return nil
		})
		panic("boom")
	})

	recovered := func() (r any) {
		defer func() { r = recover() }()
		_, _ = wrapped.Invoke(context.Background(), []byte("{}"))
		return nil
	}()

	if recovered == nil {
		t.Fatal("expected Invoke() to panic")
	}
	if !bgDone.Load() {
		t.Error("Invoke() returned before the background task finished, even though it panicked")
	}
}

func TestWrap_HandlerCompletedCalledEvenWhenHandlerPanics(t *testing.T) {
	e, api := newTestExtension(t)
	awaitPoll(t, api)

	wrapped := e.Wrap(func(context.Context) error {
		panic("boom")
	})

	recovered := func() (r any) {
		defer func() { r = recover() }()
		_, _ = wrapped.Invoke(context.Background(), []byte("{}"))
		return nil
	}()
	if recovered == nil {
		t.Fatal("expected Invoke() to panic")
	}

	api.respond <- "INVOKE"
	awaitPoll(t, api) // would time out if handlerCompleted() was skipped on panic
	api.respond <- "SHUTDOWN"
}

func TestWithExtensionName_OverridesRegisterHeader(t *testing.T) {
	_, api := newTestExtension(t, WithExtensionName("my-custom-extension"))
	awaitPoll(t, api)

	if got := api.registeredExtensionName; got != "my-custom-extension" {
		t.Errorf("registered extension name = %q, want %q", got, "my-custom-extension")
	}

	api.respond <- "SHUTDOWN"
}

func TestWithContext_DefaultsToBackground(t *testing.T) {
	e := newLocalTestExtension(t)

	var gotCtx context.Context
	e.Go(func(ctx context.Context) error {
		gotCtx = ctx
		return nil
	})
	e.waitInflight()

	if gotCtx.Done() != nil {
		t.Error("expected fn's context to be uncancelable when WithContext is not given")
	}
}

type testCtxKey struct{}

func TestWithContext_PropagatesValueToGo(t *testing.T) {
	rootCtx := context.WithValue(context.Background(), testCtxKey{}, "root-value")
	e := newLocalTestExtension(t, WithContext(rootCtx))

	var got any
	e.Go(func(ctx context.Context) error {
		got = ctx.Value(testCtxKey{})
		return nil
	})
	e.waitInflight()

	if got != "root-value" {
		t.Errorf("fn's context value = %v, want %q", got, "root-value")
	}
}

func TestWithContext_CancelPropagatesToGo(t *testing.T) {
	rootCtx, cancel := context.WithCancel(context.Background())
	e := newLocalTestExtension(t, WithContext(rootCtx))

	done := make(chan struct{})
	e.Go(func(ctx context.Context) error {
		<-ctx.Done()
		close(done)
		return nil
	})

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fn's context was not canceled after the root context passed to WithContext was canceled")
	}
	e.waitInflight()
}
