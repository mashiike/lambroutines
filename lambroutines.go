// Package lambroutines lets an AWS Lambda handler spawn background work via
// Go, with the same start-immediately semantics as a plain go statement,
// while guaranteeing that work finishes before the execution environment is
// allowed to freeze. It does this by registering an internal Lambda
// extension that defers the extension's own /next call until the handler
// has returned and all work started via Go has completed.
//
// Off the Lambda runtime (see OnLambdaRuntime) — for example under
// fujiwara/lamblocal, or a plain `go run` — there is no Extensions API to
// register with, so Extension runs in local mode instead: Go still starts fn
// immediately, and the handler wrapped with Wrap waits for it to finish
// before returning.
package lambroutines

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-lambda-go/lambda"
)

const (
	defaultExtensionName      = "lambroutines"
	eventTypeInvoke           = "INVOKE"
	extensionIdentifierHeader = "Lambda-Extension-Identifier"
)

// OnLambdaRuntime reports whether the current process is running inside an
// AWS Lambda execution environment, using the same detection as
// fujiwara/ridge and fujiwara/lamblocal:
//   - AWS_EXECUTION_ENV is set on the managed Lambda runtime (go1.x)
//   - AWS_LAMBDA_RUNTIME_API is set on custom runtimes (provided.*)
func OnLambdaRuntime() bool {
	return strings.HasPrefix(os.Getenv("AWS_EXECUTION_ENV"), "AWS_Lambda") || os.Getenv("AWS_LAMBDA_RUNTIME_API") != ""
}

// Extension is a Lambda internal extension: it delays the execution
// environment freeze until background work started via Go has finished, by
// deferring its own /next call until the handler has returned and that work
// has completed. It also tracks that work, similar in spirit to
// golang.org/x/sync/errgroup.Group. The zero value is not usable; create one
// with Start.
type Extension struct {
	client        *http.Client
	runtimeAPI    string
	local         bool
	rootCtx       context.Context
	extensionName string

	mu          sync.Mutex
	cond        *sync.Cond
	inflight    int
	completions int

	extensionID string
}

// Option configures an Extension constructed by Start.
type Option func(*Extension)

// WithContext sets the root context.Context used as the base for the
// context passed to fn by Go and, on the Lambda runtime, for the requests
// this Extension makes to the Lambda Extensions API. It defaults to
// context.Background(). Canceling the provided context stops the polling
// loop and is propagated to fn.
func WithContext(ctx context.Context) Option {
	return func(e *Extension) {
		e.rootCtx = ctx
	}
}

// WithExtensionName overrides the name this Extension registers itself
// under with the Lambda Extensions API. It defaults to "lambroutines".
func WithExtensionName(name string) Option {
	return func(e *Extension) {
		e.extensionName = name
	}
}

// Start creates an Extension and, on the Lambda runtime (see
// OnLambdaRuntime), registers it with the Lambda Extensions API and starts
// the background loop that keeps the execution environment from freezing
// until the handler has returned and all in-flight work started via Go has
// finished. Start blocks until registration completes.
//
// Off the Lambda runtime, such as under fujiwara/lamblocal or a plain
// `go run`, the returned Extension runs in local mode instead: Start does
// not register with the Extensions API, Go still starts fn immediately, and
// the handler wrapped with Wrap waits for it to finish instead of relying on
// the polling loop.
func Start(opts ...Option) (*Extension, error) {
	e := &Extension{
		client:        http.DefaultClient,
		runtimeAPI:    os.Getenv("AWS_LAMBDA_RUNTIME_API"),
		local:         !OnLambdaRuntime(),
		rootCtx:       context.Background(),
		extensionName: defaultExtensionName,
	}
	e.cond = sync.NewCond(&e.mu)
	for _, opt := range opts {
		opt(e)
	}

	if e.local {
		return e, nil
	}
	if e.runtimeAPI == "" {
		return nil, errors.New("lambroutines: AWS_LAMBDA_RUNTIME_API is not set")
	}
	if err := e.register(e.rootCtx); err != nil {
		return nil, err
	}
	go e.loop()
	return e, nil
}

// Go runs fn in a new goroutine, starting immediately just like a plain go
// statement, but tracks it so completion can be waited for: on the Lambda
// runtime, e's polling loop (started by Start) delays freezing the
// execution environment until fn returns; in local mode, the handler wrapped
// with Wrap waits for fn before Invoke returns. If fn returns a non-nil
// error, the error is logged.
//
// Go must be called synchronously from the handler's own call stack (or from
// a running fn, to launch further work). Calling it from an unrelated
// goroutine started by the handler is not tracked reliably: the handler may
// be observed as complete before that goroutine's Go call registers.
func (e *Extension) Go(fn func(context.Context) error) {
	e.mu.Lock()
	e.inflight++
	e.mu.Unlock()

	go func() {
		defer func() {
			e.mu.Lock()
			e.inflight--
			if e.inflight == 0 {
				e.cond.Broadcast()
			}
			e.mu.Unlock()
		}()
		bgCtx := context.WithValue(e.rootCtx, extensionContextKey{}, e)
		if err := fn(bgCtx); err != nil {
			log.Printf("lambroutines: background task returned error: %v", err)
		}
	}()
}

// Wrap adapts handler, in any form accepted by lambda.Start, so that its
// completion is signaled to e and e becomes retrievable from the handler's
// context via ExtensionFromContext (and, through it, Go). The returned
// Handler must be passed to lambda.Start (or lambda.StartWithOptions) in
// place of handler. Because the returned Handler already satisfies the
// lambda.Handler interface, lambda's JSON-decoding Options (WithUseNumber,
// WithDisallowUnknownFields, WithSetEscapeHTML, WithSetIndent) are bypassed if
// passed alongside it; apply them to handler via lambda.NewHandlerWithOptions
// before calling Wrap if needed.
//
// In local mode, the returned Handler's Invoke waits for e's in-flight work
// to finish before returning, using the same inflight count that Go tracks.
// This assumes at most one invocation is in flight on e at a time, matching
// how a single Lambda execution environment (and thus a single Extension)
// actually operates; calling Invoke concurrently on Handlers sharing the
// same Extension is not supported and will make one invocation wait on
// another's unrelated background work.
func (e *Extension) Wrap(handler any) lambda.Handler {
	return &wrappedHandler{extension: e, inner: lambda.NewHandler(handler)}
}

type wrappedHandler struct {
	extension *Extension
	inner     lambda.Handler
}

func (w *wrappedHandler) Invoke(ctx context.Context, payload []byte) ([]byte, error) {
	defer func() {
		w.extension.handlerCompleted()
		if w.extension.local {
			w.extension.waitInflight()
		}
	}()
	return w.inner.Invoke(context.WithValue(ctx, extensionContextKey{}, w.extension), payload)
}

type extensionContextKey struct{}

// ExtensionFromContext returns the Extension embedded in ctx by
// (*Extension).Wrap, and reports whether one was found.
func ExtensionFromContext(ctx context.Context) (*Extension, bool) {
	e, ok := ctx.Value(extensionContextKey{}).(*Extension)
	return e, ok
}

// ErrNoExtensionInContext is returned by Go when ctx was not derived from a
// handler wrapped with (*Extension).Wrap.
var ErrNoExtensionInContext = errors.New("lambroutines: no Extension in context; handler must be wrapped with (*Extension).Wrap")

// Go is a convenience wrapper around (*Extension).Go for handlers wrapped
// with (*Extension).Wrap: it looks up the Extension via
// ExtensionFromContext(ctx) and calls Go on it, so the handler doesn't need
// to hold onto the Extension itself. It returns ErrNoExtensionInContext if
// ctx was not derived from a handler wrapped with (*Extension).Wrap.
func Go(ctx context.Context, fn func(context.Context) error) error {
	e, ok := ExtensionFromContext(ctx)
	if !ok {
		return ErrNoExtensionInContext
	}
	e.Go(fn)
	return nil
}

// handlerCompleted records that the handler for the current invocation has
// returned. In non-local mode, loop waits for this before checking inflight;
// in local mode nothing reads completions, so this is a harmless no-op signal.
func (e *Extension) handlerCompleted() {
	e.mu.Lock()
	e.completions++
	e.cond.Broadcast()
	e.mu.Unlock()
}

func (e *Extension) loop() {
	consumed := 0
	for {
		eventType, err := e.next(e.rootCtx)
		if err != nil {
			log.Printf("lambroutines: extension event/next failed, stopping: %v", err)
			return
		}
		if eventType != eventTypeInvoke {
			return
		}

		e.mu.Lock()
		for e.completions == consumed {
			e.cond.Wait()
		}
		consumed = e.completions
		e.mu.Unlock()

		e.waitInflight()
	}
}

// waitInflight blocks until all work started via Go has finished.
func (e *Extension) waitInflight() {
	e.mu.Lock()
	for e.inflight > 0 {
		e.cond.Wait()
	}
	e.mu.Unlock()
}

func (e *Extension) endpoint(path string) string {
	return fmt.Sprintf("http://%s/2020-01-01/extension/%s", e.runtimeAPI, path)
}

func (e *Extension) register(ctx context.Context) error {
	body, err := json.Marshal(struct {
		Events []string `json:"events"`
	}{Events: []string{eventTypeInvoke}})
	if err != nil {
		return fmt.Errorf("lambroutines: marshal register request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint("register"), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("lambroutines: build register request: %w", err)
	}
	req.Header.Set("Lambda-Extension-Name", e.extensionName)

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("lambroutines: register request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("lambroutines: close register response body: %v", err)
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("lambroutines: read register response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("lambroutines: register failed: status=%d body=%s", resp.StatusCode, respBody)
	}

	id := resp.Header.Get(extensionIdentifierHeader)
	if id == "" {
		return fmt.Errorf("lambroutines: register response missing %s: body=%s", extensionIdentifierHeader, respBody)
	}
	e.extensionID = id
	return nil
}

func (e *Extension) next(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.endpoint("event/next"), nil)
	if err != nil {
		return "", fmt.Errorf("lambroutines: build event/next request: %w", err)
	}
	req.Header.Set(extensionIdentifierHeader, e.extensionID)

	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("lambroutines: event/next request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("lambroutines: close event/next response body: %v", err)
		}
	}()

	var event struct {
		EventType string `json:"eventType"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&event); err != nil {
		return "", fmt.Errorf("lambroutines: decode event/next response: %w", err)
	}
	return event.EventType, nil
}
