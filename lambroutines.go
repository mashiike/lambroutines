// Package lambroutines lets an AWS Lambda handler spawn background work via
// Go, with the same start-immediately semantics as a plain go statement,
// while guaranteeing that work finishes before the execution environment is
// allowed to freeze. It does this by registering an internal Lambda
// extension that defers the extension's own /next call until a Scope opened
// for the invocation has ended and all work started via that Scope's Go has
// completed.
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
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
)

const (
	defaultExtensionName      = "lambroutines"
	defaultScopeTimeout       = 30 * time.Second
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
// environment freeze until background work started via a Scope's Go has
// finished, by deferring its own /next call until a Scope opened for the
// invocation has ended and that work has completed. The zero value is not
// usable; create one with Start.
type Extension struct {
	client        *http.Client
	runtimeAPI    string
	local         bool
	rootCtx       context.Context
	extensionName string
	scopeTimeout  time.Duration
	logger        *slog.Logger

	mu          sync.Mutex
	cond        *sync.Cond
	starts      int
	completions int
	inflight    int

	extensionID string
}

// Option configures an Extension constructed by Start.
type Option func(*Extension)

// WithContext sets the root context.Context used as the base for the
// context passed to fn by Go and, on the Lambda runtime, for the requests
// this Extension makes to the Lambda Extensions API. It defaults to
// context.Background(). Canceling the provided context is propagated to fn
// and stops the extension's own /next request from blocking indefinitely;
// it is not observed while the polling loop is waiting for a Scope to start
// or end, or for in-flight Go work to finish, so canceling it does not by
// itself unblock those waits.
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

// WithScopeTimeout overrides how long, on the Lambda runtime, the extension
// waits for a Scope to be started (see (*Extension).StartScope) after
// receiving an INVOKE event before giving up and logging a warning. It
// defaults to 30 seconds. A value <= 0 disables the timeout, and the
// extension waits indefinitely instead.
//
// This guards against forgetting to wrap the handler with Wrap or call
// StartScope: without it, a missing Scope would make the extension's
// polling loop wait forever. It does not protect against a Scope started
// later than this timeout (see StartScope for the guidance to start Scopes
// as early as possible), nor against invocations that never start a Scope
// at all (for example, requests routed to handlers that don't call
// StartScope), which pay the timeout on every such invocation.
func WithScopeTimeout(d time.Duration) Option {
	return func(e *Extension) {
		e.scopeTimeout = d
	}
}

// WithLogger overrides the *slog.Logger used to report background task
// errors, extension failures, and scope-timeout warnings. It defaults to
// slog.Default().
func WithLogger(logger *slog.Logger) Option {
	return func(e *Extension) {
		e.logger = logger
	}
}

// Start creates an Extension and, on the Lambda runtime (see
// OnLambdaRuntime), registers it with the Lambda Extensions API and starts
// the background loop that keeps the execution environment from freezing
// until a Scope started for the invocation has ended and all its in-flight
// work started via Go has finished. Start blocks until registration
// completes.
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
		scopeTimeout:  defaultScopeTimeout,
		logger:        slog.Default(),
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

// Scope represents the span of a single Lambda invocation — or an
// http.Handler request, or any other unit of work in which background tasks
// should be tracked. Background work started via Go while a Scope is active
// is guaranteed to finish before the execution environment freezes. Create
// one with (*Extension).StartScope; the zero value is not usable.
//
// A Scope started while ctx already carries another Scope (for example,
// nested StartScope calls, or a call from inside a Go fn) is a nested Scope:
// its Go calls are tracked against the outermost (root) Scope, and its End
// is a no-op — only the root Scope's End signals completion. This lets code
// call StartScope defensively without needing to know whether it's already
// inside one.
type Scope struct {
	extension *Extension
	root      *Scope
	endOnce   sync.Once
}

func (s *Scope) rootOrSelf() *Scope {
	if s.root != nil {
		return s.root
	}
	return s
}

// StartScope opens a Scope for the current invocation and embeds it in the
// returned context.Context, retrievable via ScopeFromContext. Call End
// (typically via defer) when the invocation is done.
//
// Wrap calls StartScope automatically; call it directly only when you can't
// use Wrap, for example when adapting a handler shape Wrap doesn't cover
// (such as fujiwara/ridge's http.Handler-based integration). Call
// StartScope as early as possible in the invocation — ideally as the first
// statement — and always pair it with a deferred End. On the Lambda
// runtime, the extension only waits up to WithScopeTimeout for some Scope to
// start after each INVOKE event; starting one later than that risks it
// being attributed to the wrong invocation, silently breaking the
// freeze-delay guarantee (see WithScopeTimeout).
//
// Call it at most once per invocation with a ctx that doesn't already carry
// a Scope. A second such call within the same invocation starts an
// unrelated root Scope that the extension's polling loop isn't guaranteed to
// wait for; pass the ctx returned by the first call (directly, or via a
// nested call to StartScope) instead.
func (e *Extension) StartScope(ctx context.Context) (*Scope, context.Context) {
	if parent, ok := ScopeFromContext(ctx); ok {
		nested := &Scope{extension: e, root: parent.rootOrSelf()}
		return nested, context.WithValue(ctx, scopeContextKey{}, nested)
	}

	e.mu.Lock()
	e.starts++
	e.cond.Broadcast()
	e.mu.Unlock()

	root := &Scope{extension: e}
	return root, context.WithValue(ctx, scopeContextKey{}, root)
}

// Go runs fn in a new goroutine, starting immediately just like a plain go
// statement, but tracks it against s's root Scope so completion can be
// waited for: on the Lambda runtime, the extension's polling loop delays
// freezing the execution environment until fn returns; in local mode, the
// code that calls End on the root Scope waits for fn before returning. If fn
// returns a non-nil error, the error is logged.
//
// Go must be called synchronously from the scope holder's own call stack (or
// from a running fn, to launch further work). Calling it from an unrelated
// goroutine is not tracked reliably: the root Scope's End may be observed
// before that goroutine's Go call registers.
//
// fn must not call End on the Scope it receives via bgCtx (or on any
// ancestor of that Scope): in local mode, End waits for this same Go call to
// finish, so calling it from inside fn deadlocks.
func (s *Scope) Go(fn func(context.Context) error) {
	e := s.rootOrSelf().extension
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
		bgCtx := context.WithValue(e.rootCtx, scopeContextKey{}, s)
		if err := fn(bgCtx); err != nil {
			e.logger.Error("lambroutines: background task returned error", "error", err)
		}
	}()
}

// End marks s as finished. For a root Scope (one returned by StartScope when
// ctx carried no other Scope), this signals the extension that the
// invocation is done, and in local mode blocks until all work started via Go
// against this Scope has finished. For a nested Scope, End is a no-op — only
// the root Scope's End matters.
//
// End is idempotent: calling it more than once on the same root Scope has no
// effect beyond the first call. In local mode, a second, concurrent call
// blocks until the first call's wait for in-flight work has finished; it
// does not return immediately.
//
// End must not be called from inside a fn passed to this Scope's (or one of
// its descendants') Go: see Go's documentation for why that deadlocks in
// local mode.
func (s *Scope) End() {
	if s.root != nil {
		return
	}
	s.endOnce.Do(func() {
		s.extension.noteScopeEnded()
		if s.extension.local {
			s.extension.waitInflight()
		}
	})
}

// Wrap adapts handler, in any form accepted by lambda.Start, so that a Scope
// is started for each invocation and embedded in the handler's context via
// ScopeFromContext (and, through it, Go), and the Scope is ended once
// handler returns. The returned Handler must be passed to lambda.Start (or
// lambda.StartWithOptions) in place of handler. Because the returned Handler
// already satisfies the lambda.Handler interface, lambda's JSON-decoding
// Options (WithUseNumber, WithDisallowUnknownFields, WithSetEscapeHTML,
// WithSetIndent) are bypassed if passed alongside it; apply them to handler
// via lambda.NewHandlerWithOptions before calling Wrap if needed.
//
// In local mode, the returned Handler's Invoke waits for the Scope's
// in-flight work to finish before returning. This assumes at most one
// invocation is in flight on e at a time, matching how a single Lambda
// execution environment (and thus a single Extension) actually operates;
// calling Invoke concurrently on Handlers sharing the same Extension is not
// supported and will make one invocation wait on another's unrelated
// background work.
func (e *Extension) Wrap(handler any) lambda.Handler {
	return &wrappedHandler{extension: e, inner: lambda.NewHandler(handler)}
}

type wrappedHandler struct {
	extension *Extension
	inner     lambda.Handler
}

func (w *wrappedHandler) Invoke(ctx context.Context, payload []byte) ([]byte, error) {
	scope, ctx := w.extension.StartScope(ctx)
	defer scope.End()
	return w.inner.Invoke(ctx, payload)
}

type scopeContextKey struct{}

// ScopeFromContext returns the Scope embedded in ctx by
// (*Extension).StartScope (including via Wrap), and reports whether one was
// found.
func ScopeFromContext(ctx context.Context) (*Scope, bool) {
	s, ok := ctx.Value(scopeContextKey{}).(*Scope)
	return s, ok
}

// ErrNoScopeInContext is returned by Go when ctx was not derived from a
// Scope started by (*Extension).StartScope.
var ErrNoScopeInContext = errors.New("lambroutines: no Scope in context; call (*Extension).StartScope (or wrap the handler with (*Extension).Wrap)")

// Go is a convenience wrapper around (*Scope).Go for code holding only a
// context.Context: it looks up the Scope via ScopeFromContext(ctx) and calls
// Go on it. It returns ErrNoScopeInContext if ctx was not derived from a
// Scope started by (*Extension).StartScope.
func Go(ctx context.Context, fn func(context.Context) error) error {
	s, ok := ScopeFromContext(ctx)
	if !ok {
		return ErrNoScopeInContext
	}
	s.Go(fn)
	return nil
}

func (e *Extension) noteScopeEnded() {
	e.mu.Lock()
	e.completions++
	e.cond.Broadcast()
	e.mu.Unlock()
}

func (e *Extension) loop() {
	consumedStarts := 0
	consumedCompletions := 0
	for {
		eventType, err := e.next(e.rootCtx)
		if err != nil {
			e.logger.Error("lambroutines: extension event/next failed, stopping", "error", err)
			return
		}
		if eventType != eventTypeInvoke {
			return
		}

		newStarts, started := e.waitForStart(consumedStarts)
		if !started {
			e.logger.Warn("lambroutines: no Scope was started within the timeout; did you forget to call StartScope or wrap the handler with Wrap?")
			continue
		}
		consumedStarts = newStarts

		consumedCompletions = e.waitForCompletion(consumedCompletions)
		e.waitInflight()
	}
}

func (e *Extension) waitForStart(consumed int) (int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.scopeTimeout <= 0 {
		for e.starts == consumed {
			e.cond.Wait()
		}
		return e.starts, true
	}

	deadline := time.Now().Add(e.scopeTimeout)
	timer := time.AfterFunc(e.scopeTimeout, func() {
		e.mu.Lock()
		e.cond.Broadcast()
		e.mu.Unlock()
	})
	defer timer.Stop()

	for e.starts == consumed && time.Now().Before(deadline) {
		e.cond.Wait()
	}
	return e.starts, e.starts != consumed
}

func (e *Extension) waitForCompletion(consumed int) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	for e.completions == consumed {
		e.cond.Wait()
	}
	return e.completions
}

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
			e.logger.Error("lambroutines: close register response body", "error", err)
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
			e.logger.Error("lambroutines: close event/next response body", "error", err)
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
