# lambroutines

[![CI](https://github.com/mashiike/lambroutines/actions/workflows/ci.yml/badge.svg)](https://github.com/mashiike/lambroutines/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/mashiike/lambroutines.svg)](https://pkg.go.dev/github.com/mashiike/lambroutines)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

日本語版 README は [README.ja.md](README.ja.md) を参照してください。

`lambroutines` lets an AWS Lambda handler spawn background work with a plain
`go fn()`-like call, while guaranteeing that the work finishes before the
execution environment is allowed to freeze.

## Why

A plain `go fn()` started inside a Lambda handler is not guaranteed to run to
completion: once the handler returns, Lambda may freeze the execution
environment at any point, pausing (and possibly never resuming) any
goroutines still in flight.

`lambroutines` solves this by registering an [internal Lambda
extension](https://docs.aws.amazon.com/lambda/latest/dg/runtimes-extensions-api.html)
that defers its own call to `/next` until the handler has returned **and**
all work started via `Go` has completed. Lambda does not freeze the execution
environment while an extension has an outstanding `/next` call, so the
background work gets to run to completion — without blocking the handler's
own response.

## Installation

```sh
go get github.com/mashiike/lambroutines
```

## Usage

```go
package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/mashiike/lambroutines"
)

func handle(ctx context.Context, payload json.RawMessage) (string, error) {
	err := lambroutines.Go(ctx, func(context.Context) error {
		// runs in the background; the handler doesn't wait for this to finish,
		// but the execution environment won't freeze until it does.
		time.Sleep(3 * time.Second)
		log.Println("background work done")
		return nil
	})
	if err != nil {
		return "", err
	}
	return "ok", nil
}

func main() {
	ext, err := lambroutines.Start()
	if err != nil {
		log.Fatalf("lambroutines: start failed: %v", err)
	}
	lambda.Start(ext.Wrap(handle))
}
```

- `lambroutines.Start` creates an `*Extension` and, on the Lambda runtime,
  registers it as an internal extension.
- `ext.Wrap(handle)` adapts your handler (in any form accepted by
  `lambda.Start`) so a `Scope` is started for each invocation, embedded in
  the handler's own `context.Context`, and ended once the handler returns.
- `lambroutines.Go(ctx, fn)` looks up the `*Scope` from `ctx` and starts `fn`
  in a new goroutine immediately — the same start-immediately semantics as a
  plain `go fn()` statement, just tracked.

See [`examples/`](examples/) for a deployable example built with
[fujiwara/lambroll](https://github.com/fujiwara/lambroll).

## Handlers Wrap doesn't cover

Some Lambda integrations, such as [fujiwara/ridge](https://github.com/fujiwara/ridge),
call `lambda.Start` internally and never expose a `lambda.Handler` for you to
pass to `Wrap`. For these, call `ext.StartScope` directly at the start of
your own `http.Handler` (or whatever unit of work should be tracked), and
defer its `End`:

```go
func handler(w http.ResponseWriter, r *http.Request) {
	scope, ctx := ext.StartScope(r.Context())
	defer scope.End()

	scope.Go(func(ctx context.Context) error {
		// background work
		return nil
	})
	// ... normal HTTP handling ...
}

ridge.Run(":8080", "", http.HandlerFunc(handler))
```

Call `StartScope` as early as possible in the invocation — see its GoDoc for
why, and see `WithScopeTimeout` below for what happens if you don't.

## Local mode

Off the Lambda runtime (see `lambroutines.OnLambdaRuntime`) — for example
under [`fujiwara/lamblocal`](https://github.com/fujiwara/lamblocal), or a
plain `go run` — there is no Extensions API to register with. `Start`
detects this and returns an `*Extension` running in local mode instead:
`Go` still starts `fn` immediately, and ending a root Scope's `End`
(automatically via `Wrap`, or directly via `StartScope`) waits for it to
finish before returning, so background work isn't silently dropped when the
process exits right after the invocation ends.

Local mode is meant as a development convenience. It assumes at most one
invocation is in flight on a given `*Extension` at a time (that is, at most
one root Scope open at once), matching how a single Lambda execution
environment actually operates; it is not a strict reproduction of the
Extensions API's freeze-deferral behavior.

## Options

`Start` accepts functional options:

- `lambroutines.WithContext(ctx)` sets the root `context.Context` used as the
  base for the context passed to `fn` by `Go`, and for the Extension's own
  Extensions API requests. Canceling it stops the polling loop and is
  propagated to `fn`.
- `lambroutines.WithExtensionName(name)` overrides the name the Extension
  registers itself under with the Lambda Extensions API (defaults to
  `"lambroutines"`).
- `lambroutines.WithScopeTimeout(d)` overrides how long, on the Lambda
  runtime, the extension waits for a `Scope` to start (see `StartScope`)
  after an `INVOKE` event before giving up and logging a warning (defaults
  to 30 seconds; `d <= 0` disables the timeout). This only guards against
  never starting a `Scope` at all (for example, forgetting to wrap the
  handler with `Wrap`); it does not protect against starting one later than
  this timeout, nor against invocations that never start a `Scope`, which
  pay the timeout on every such invocation.
- `lambroutines.WithLogger(logger)` overrides the `*slog.Logger` used to
  report background task errors, extension failures, and scope-timeout
  warnings (defaults to `slog.Default()`).

## License

MIT. See [LICENSE](LICENSE).
