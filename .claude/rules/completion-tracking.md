---
paths:
  - "lambroutines.go"
---

# handler 完了と background task 完了の待ち合わせに monotonic counter を使う理由

## 背景 / 罠の説明

`Extension.loop()` は `/next` で INVOKE を受け取った後、(1) handler が return したこと、
(2) `Go()` で起動した background task が全て終わったこと、の両方を確認してから次の
`/next` を呼ぶ。この2つの完了をどう待ち合わせるかで、開発中に2つの実装が実際に race
を起こした。

### 壊れた実装1: `sync.WaitGroup`

`Go()` が background task の中から再度呼ばれるケースでは、`wg.Wait()` が counter=0 を
観測して返った直後に別 goroutine が `wg.Add(1)` する可能性があり、`sync.WaitGroup` の
「counter が 0 の間は Add を Wait と並行して呼んではならない」という制約に抵触しうる。
mutex 保護の int counter (`inflight`) + `sync.Cond` に置き換えたことで、increment/decrement
と `loop()` 側のチェックが同一 mutex 下で完全に直列化され、この制約自体が問題にならない。

### 壊れた実装2: handler 完了ごとに使い捨て `chan struct{}` を差し替える方式

`handlerCompleted()` で `done := c.handlerDone; c.handlerDone = make(chan struct{}); close(done)`
のように完了ごとにチャネルを差し替える実装は、**handler が extension の `/next` ポーリング
より先に完了する場合**（`examples/main.go` のような速い handler では普通に起こる）、
`loop()` が古いチャネルを読む前に次のチャネルへ差し替わってしまい、今回の完了信号を永久に
見逃して `<-done` でハングする。全アクセスが mutex で保護されているため `go test -race`
はこの race を検出しない（lost-wakeup 型のロジック race であり、データ race ではない）。

この経緯から、`completions`（handler 完了ごとに ++ する）と `inflight`（実行中 background
task 数）を、同一 mutex + `sync.Cond` で管理するモノトニックカウンタ方式に統一している。
`loop()` はローカル変数 `consumed` と `e.completions` を比較して待つ形になっており、handler
と extension のどちらが先に該当ポイントへ到達しても正しく動く。

## チェックリスト

- `handlerCompleted()` / `Go()` / `loop()` の待ち合わせロジックを変更する場合は、
  「handler が先に完了する」「extension が先にポーリングする」の両順序を再現するテスト
  （`lambroutines_test.go` の `TestExtension_HandlerCompletesBeforeExtensionDeliversInvoke` /
  `TestExtension_ExtensionSeesInvokeBeforeHandlerCompletes`）を `go test -race` で通してから
  変更を確定させる。
- 「使い捨てチャネル + 差し替え」方式に戻したくなったら、上記の race を再発させることを
  まず疑う。

## ローカルモード（`Extension.local`）で `Go()` を素の `go fn()` にしない理由

`OnLambdaRuntime()` が false のとき（`fujiwara/lamblocal` 配下や `go run` での直接実行）、
`Go()` の実装自体は変えず、`inflight` のインクリメント/デクリメントを常に行う。ここで
`Go()` 側にローカル分岐を入れて「ローカルなら追跡なしの素の `go fn()`」にしたくなるが、
それをやると `fujiwara/lamblocal` の `RunWithError`（`fn` を1回呼んで戻ったら `main()` が
そのまま終了する単発 CLI 実行モデル。`lamblocal.go` に常駐プロセスや待ち合わせは無い）の
下では、handler が速く return した時点で background task が完了する前にプロセスが終了し、
起動した処理が黙って失われる。

代わりに `wrappedHandler.Invoke()` 側で `e.local` を見て `waitInflight()` を呼び、
inner handler が返ったあと `inflight == 0` になるまで同期的に待ってから `Invoke()`自体が
返る、という形にしている。これにより `Go()` / `Wrap()` / `ExtensionFromContext` の呼び出し側
から見える API は Lambda 実行時とローカル実行時で完全に同一のまま、ローカルでも
「`Go()` で起動した処理が完了する前にプロセスが終了しない」保証を保っている。

`loop()` 側の「`completions` が進むまで待つ」ループと `waitInflight()` を素朴に一体化させ
ない理由: `loop()` は extension のポーリングと handler の完了を非同期に待ち合わせる必要が
あるため `completions` counter が必須だが、`Invoke()` は同一関数呼び出しの中で inner handler
の return 後に呼ばれるため、`completions` を経由せず `inflight` だけを見れば十分（該当
invocation の `Go()` 呼び出しはこの時点で全て `inflight++` 済みであることが呼び出し順序から
保証されている）。
