# `(*Scope).Go` の error/panic ハンドリング

## 経緯

もともと `Go` は `fn` が返した error を `e.logger.Error(...)` に固定でログ出力するだけで、
`fn` が panic した場合は `recover()` が無く goroutine の panic がそのまま伝播していた。
`recover` は同一 goroutine の deferred function でしか効かないため、`aws-lambda-go` の
ランタイムが持つ通常の panic recover では拾えず、Lambda 実行環境(コンテナ)ごとクラッシュ
し、同じコンテナで処理中の他のリクエストも巻き込んでいた。加えて、`fn` が error を返した
場合はログに残るが panic の場合は(slog経由の構造化ログとしては)残らないという観測性の
非対称もあった。この非対称は、このライブラリの利用側プロジェクトからの報告で発覚した。

対応として、`fn` の error と panic の両方を1つの `BackgroundErrorHandler`(`WithErrorHandler`
で差し替え可能、デフォルトは `WithLogger` の logger へ `Error` 出力)に合流させる形にした。
専用の panic 用ログ行は増やしていない。

## デフォルトで panic を揉み消す設計にした理由

`Go` の goroutine は必ず `recover()` するため、デフォルトでは **panic はプロセスを
クラッシュさせなくなる**(以前の「panic → コンテナごと落ちる → 他のリクエストを巻き込む」
という挙動からの意図的な変更)。crash-on-panic の挙動を維持したい呼び出し側は、
`WithErrorHandler` に渡す自分の handler の中で recover された値を再度 `panic()` すればよい
(`err` が `*PanicError` かどうかは `errors.As` で判別できる)。「recover して握りつぶすのは
Go の一般的な規範から外れる」という意見もあり得るが、Lambda 実行環境を道連れにする実害の
方を優先した。

## `Go` 内で `recover` を置く位置、`errorHandler` の呼び出しを1箇所にまとめた理由

```go
go func() {
    defer func() { /* inflight-- と cond.Broadcast() */ }()
    bgCtx := context.WithValue(e.rootCtx, scopeContextKey{}, s)
    err := func() (err error) {
        defer func() {
            if r := recover(); r != nil {
                err = &PanicError{Recovered: r, Stack: debug.Stack()}
            }
        }()
        return fn(bgCtx)
    }()
    if err != nil {
        e.errorHandler(bgCtx, err)
    }
}()
```

最初の実装は `fn` の呼び出しと `if err := fn(bgCtx); err != nil { e.errorHandler(bgCtx, err) }`
を同じ関数本体に並べ、その手前に `recover` 用の defer を1つ置くだけだった。これだと
`e.errorHandler(bgCtx, err)`(error 由来の呼び出し)自体もその defer のスコープに入って
しまい、次の非対称が生じた(review-dispatch で3つの独立したレビューエージェントが別々の
角度から発見し、再現スクリプトで実際に確認した):

- `fn` が **panic** し、`errorHandler` がその中で(crash-on-panic を復元する目的で)
  再度 panic する → 呼び出し時点で recover 用 defer は既に recover() を使い切っているため
  この再 panic は捕まらず、そのまま伝播してプロセスがクラッシュする(意図通り)
- `fn` が **error を返し**、`errorHandler` がその処理中に(同じ意図で)panic する →
  この呼び出しはまだ recover 用 defer のスコープ内にあるため、その panic は同じ defer に
  **捕まってしまい**、`*PanicError` として `errorHandler` が**もう一度**呼ばれる。プロセスは
  クラッシュしない

同じ「`errorHandler` が panic する」状況なのに、`fn` が panic したかどうかで挙動(クラッシュ
するか、握りつぶされて2重呼び出しになるか)が変わってしまい、「error と panic を同じ hook に
合流させる」という設計方針と矛盾していた。

対策として、`fn` の呼び出しと recover を無名関数に閉じ込め、`err`(通常の error か
`*PanicError` のどちらか)として受け取ってから `errorHandler` を呼ぶ形にした。これにより
`errorHandler` の呼び出し箇所は(error 由来・panic 由来を問わず)常にこの1箇所だけになり、
`errorHandler` 自身が panic した場合はどちらの経路でも同じく(このコードのどの defer にも
捕まらず)伝播してプロセスをクラッシュさせる。「`errorHandler` の中で panic すれば
crash-on-panic を復元できる」という godoc の説明は、この構造があって初めて両経路で
一貫して成り立つ。**`errorHandler` の呼び出しを、`fn` の recover 用 defer のスコープの
外に出す**という制約を変更するときは、この非対称が再発することをまず疑う。

`inflight--` の defer は今まで通り一番外側にあるため、`errorHandler` が panic しても
(検証用の再現コードで確認済みで)`inflight--`/`cond.Broadcast()` は必ず実行される。

### `errorHandler` の実行時間は待ち合わせ時間に直結する(タイミングの契約)

`errorHandler` の呼び出しは `inflight--` の defer より**先に**完了する(defer は LIFO で
`inflight--` の defer が最後に実行されるため)。つまり `errorHandler` の実行時間は、
Lambda 実行時は `loop()` が `/next` を呼ぶまでの遅延、ローカルモードでは `End()` が
`waitInflight()` でブロックする時間に直結する。`errorHandler` に重い外部 I/O(Slack 通知・
外部監視サービスへの送信等)を書くと、`WithScopeTimeout` のような安全網なしにこの待ち時間が
伸びる。この関係(defer の実行順序が正しさだけでなくタイミングにも影響する)を壊さないこと。

## `WithLogger` と `WithErrorHandler` の役割分担

`WithLogger` は extension 自身の failure(`register`/`event/next` のリクエスト失敗等)と
scope-timeout warning のみを報告する。background task(`fn`)の error/panic の報告は
`WithErrorHandler` の責任であり、そのデフォルト実装が内部で `WithLogger` の logger を
使っているだけ、という関係。`WithLogger` の godoc を変更する際は、この役割分担を保つこと。
