# lambroutines

[![CI](https://github.com/mashiike/lambroutines/actions/workflows/ci.yml/badge.svg)](https://github.com/mashiike/lambroutines/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/mashiike/lambroutines.svg)](https://pkg.go.dev/github.com/mashiike/lambroutines)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

English README is [README.md](README.md).

`lambroutines` は、AWS Lambda の handler から素の `go fn()` と同じ感覚でバックグラウンド処理を起動しつつ、その処理が完了するまで実行環境がフリーズしないことを保証するパッケージです。

## なぜ必要か

Lambda の handler 内で素の `go fn()` を起動しても、その完了は保証されません。handler が return した後、Lambda は任意のタイミングで実行環境をフリーズさせる可能性があり、実行中のgoroutineは一時停止(場合によっては二度と再開されない)します。

`lambroutines` は [internal Lambda extension](https://docs.aws.amazon.com/lambda/latest/dg/runtimes-extensions-api.html) を登録し、その extension 自身の `/next` 呼び出しを、handler が return し **かつ** `Go` で起動した処理が全て完了するまで遅延させることでこれを解決します。extension が `/next` 呼び出し中は Lambda が実行環境をフリーズさせないため、handler 自身のレスポンスをブロックすることなく、バックグラウンド処理を完了まで走らせることができます。

## インストール

```sh
go get github.com/mashiike/lambroutines
```

## 使い方

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
		// バックグラウンドで実行される。handler はこの完了を待たないが、
		// これが完了するまで実行環境はフリーズしない。
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

- `lambroutines.Start` は `*Extension` を作成し、Lambda 実行環境上では internal extension として登録します。
- `ext.Wrap(handle)` は(`lambda.Start` が受け付けるあらゆる形式の)handler をラップし、その完了が `ext` に通知されるようにするとともに、handler 自身の `context.Context` から `ext` を取得できるようにします。
- `lambroutines.Go(ctx, fn)` は `ctx` から `*Extension` を取得し、`fn` を新しい goroutine で即座に起動します——素の `go fn()` 文と同じ即時実行のセマンティクスに、完了追跡が加わっただけです。すでに `ext` への参照を持っている場合は `ext.Go(fn)` を直接呼ぶこともできます。

デプロイ可能な例は [`examples/`](examples/) を参照してください。[fujiwara/lambroll](https://github.com/fujiwara/lambroll) でビルドしています。

## ローカルモード

Lambda 実行環境外(`lambroutines.OnLambdaRuntime` を参照)——例えば [`fujiwara/lamblocal`](https://github.com/fujiwara/lamblocal) 配下や、単なる `go run` での実行時——には Extensions API が存在しません。`Start` はこれを検知し、代わりにローカルモードで動作する `*Extension` を返します: `Go` は依然として `fn` を即座に起動しますが、`Wrap` でラップした handler は `fn` の完了を待ってから return するため、handler が return した直後にプロセスが終了してもバックグラウンド処理が黙って失われることはありません。

ローカルモードは開発時の利便性のためのものです。同一の `*Extension` に対して同時に実行中の invocation は最大1つという前提を置いており、これは実際の Lambda 実行環境(1つの実行環境が同時に処理する invocation は1つ)の挙動と一致します。Extensions API によるフリーズ遅延の厳密な再現ではありません。

## オプション

`Start` は関数オプションを受け付けます:

- `lambroutines.WithContext(ctx)`: `Go` が `fn` に渡す context のベースとなる root context、および Extension 自身の Extensions API リクエストに使う context を設定します。キャンセルするとポーリングループが停止し、`fn` にも伝播します。
- `lambroutines.WithExtensionName(name)`: Extension が Lambda Extensions API に登録する名前を上書きします(デフォルトは `"lambroutines"`)。

## ライセンス

MIT。[LICENSE](LICENSE) を参照してください。
