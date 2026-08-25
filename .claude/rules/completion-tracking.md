---
paths:
  - "lambroutines.go"
---

# handler 完了と background task 完了の待ち合わせに monotonic counter を使う理由

## 背景 / 罠の説明

`Extension.loop()` は `/next` で INVOKE を受け取った後、(1) その invocation の root
`Scope` が `End()` されたこと、(2) その `Scope` 経由で `Go()` した background task が
全て終わったこと、の両方を確認してから次の `/next` を呼ぶ。この2つの完了をどう待ち
合わせるかで、開発中に2つの実装が実際に race を起こした（この節では歴史的経緯を説明
するため、当時の実装に合わせて「handler 完了」「`handlerCompleted()`」と表記する。
現在の対応する概念は「root `Scope` の `End()`」「`noteScopeEnded()`」）。

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

この経緯から、`completions`（root `Scope` が `End()` されるごとに ++ する）と `inflight`
（実行中 background task 数）を、同一 mutex + `sync.Cond` で管理するモノトニックカウンタ
方式に統一している。`loop()` はローカル変数 `consumed` と `e.completions` を比較して待つ
形になっており、handler と extension のどちらが先に該当ポイントへ到達しても正しく動く。

## チェックリスト

- `noteScopeEnded()` / `Go()` / `loop()` の待ち合わせロジックを変更する場合は、
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

代わりに `(*Scope).End()` 側で `e.local` を見て `waitInflight()` を呼び（root Scope の
場合のみ）、inner handler が返ったあと `inflight == 0` になるまで同期的に待ってから
`End()` 自体が返る、という形にしている（`wrappedHandler.Invoke()` は `StartScope` して
`defer scope.End()` するだけの薄いラッパー）。これにより `Go()` / `Wrap()` /
`ScopeFromContext` の呼び出し側から見える API は Lambda 実行時とローカル実行時で完全に
同一のまま、ローカルでも「`Go()` で起動した処理が完了する前にプロセスが終了しない」
保証を保っている。

`loop()` 側の「`completions` が進むまで待つ」ループと `waitInflight()` を素朴に一体化させ
ない理由: `loop()` は extension のポーリングと handler の完了を非同期に待ち合わせる必要が
あるため `completions` counter が必須だが、`End()` は同一関数呼び出しの中で inner handler
の return 後に呼ばれるため、`completions` を経由せず `inflight` だけを見れば十分（該当
invocation の `Go()` 呼び出しはこの時点で全て `inflight++` 済みであることが呼び出し順序から
保証されている）。

## `Scope` 開始の検知に `starts` という別カウンタを使う理由

`Extension` には `WithScopeTimeout`（デフォルト30秒）という安全網があり、INVOKE イベント
を受け取ってから一定時間内に `Scope` が全く開始されなければ（`Wrap` し忘れ等）警告を出す。
この実装でも、上記の「使い捨てオブジェクトの差し替え」に似た罠を設計中に実際に踏んだ。

### 壊れた実装: タイムアウトを `completions`（Scope の終了）にひも付ける

最初の実装は、既存の `for e.completions == consumed { e.cond.Wait() }` にそのままデッド
ラインを付けるものだった。これは Lambda の handler が最大900秒まで正常に動くという事実
と衝突する。30秒を超える正常な handler では次が起きる:

1. デッドラインに達し、`loop()` は「Scope が完了しなかった」と誤って警告を出し、先に
   `/next` を呼んでしまう（`consumed` は更新しない）。
2. handler は40秒後に正常に終了して `completions` が進むが、`loop()` は既に次の `/next`
   を呼んでポーリング中なのでこれに気づかない。
3. 次の INVOKE が来たとき、`loop()` はこの取りこぼした `completions` の前進を「今回の
   Scope が完了した」と誤認し、現在の handler がまだ実行中のうちに `/next` を呼んで
   しまう。

この off-by-one は一度発生すると自己修復せず、以後全 invocation で background work が
保護されないまま固定化する。しかも2回目以降は警告も出ない。

### 正しい実装: タイムアウトは `starts`（Scope の開始）にだけ付ける

`StartScope` が root Scope を作るたびに `starts` というモノトニックカウンタを進める。
`loop()` はまず `starts` の前進をタイムアウト付きで待ち（`waitForStart`）、一度でも
`starts` が観測できたら（`Wrap`/`StartScope` が機能している証拠が取れたら）、その後の
`completions` の前進は**タイムアウトなしで**待つ（`waitForCompletion`）。`StartScope` は
`Wrap` の `Invoke` 冒頭、あるいは呼び出し側が invocation の最初の位置で呼ぶ前提なので、
handler がどれだけ長くかかっても `starts` は数十ミリ秒で進み、上記の誤動作は起きない。

この設計を変更する際は、「Scope の**開始**を検知する」と「Scope の**終了**を待つ」を
分離したまま保つこと。1つのカウンタと1つのタイムアウトに戻したくなったら、上記の
off-by-one を再発させることをまず疑う。テストは
`TestExtension_LoopWarnsWhenNoScopeStartedWithinTimeout` /
`TestExtension_LoopDoesNotTimeOutOnSlowHandlerOnceScopeStarted` を必ず通すこと。

なお、`StartScope` を一度も呼ばない invocation（ヘルスチェック等、部分的にしか計装され
ていないルート）は、この設計でも毎回タイムアウト秒数分だけ `/next` が遅れる。これは
タイムアウト方式そのものに残る既知のコストで、`starts` 方式でも解決されない。

## nested `Scope` が独立した `inflight` を持たない理由

`StartScope` を、既に `Scope` を含む `ctx` から呼ぶと nested `Scope` になる。検討時に
「nested `Scope` も自分専用の `mu`/`cond`/`inflight` を持つ」設計を一度提案したが、実際
に使うのは常に「一番外側（root）の `Scope` が完了したかどうか」だけであり、nested
`Scope` 自身の区間の長さを追跡する必要はないとわかった。nested の `Go()` は root の
`inflight` を直接操作し、nested の `End()` は no-op にしている。

これにより、以下のコードは「最後に呼ばれた root の `End()`」だけが完了通知として意味を
持ち、`scope2`（nested）がいつ `End()` を呼んでも root の完了判定には影響しない:

```go
scope1, ctx := ext.StartScope(ctx)
defer scope1.End()
scope1.Go(fn1)

scope2, ctx := ext.StartScope(ctx) // nested
defer scope2.End()                  // no-op
scope2.Go(fn2)
```

nested に `mu`/`cond`/`inflight` を持たせる設計に戻したくなったら、「ローカルモードで
同一 `*Extension` に対して複数の root `Scope` が並行に走るケースを分離したい」という
明示的な新要件が無い限り、それは不要な複雑化であることをまず疑う（現状は Lambda の
実行モデル通り、同時に処理される invocation は1つという前提に依存している）。

## `(*Scope).Go` が `s.extension` ではなく `s.rootOrSelf().extension` を使う理由

`StartScope(ctx)` は `nested := &Scope{extension: e, root: parent.rootOrSelf()}` の形で
nested Scope を作る。ここで `e` は「nested を作るのに使った `*Extension`」であり、
`parent`（実際の root）が属する `*Extension` と一致するとは限らない（複数の `*Extension`
が同一プロセスに存在し、片方で開いた Scope を含む `ctx` をもう片方の `StartScope` に渡す、
という誤用に近いケースでのみ発生する）。`Go()` が `s.extension` を直接見てしまうと、この
場合 nested の `inflight` は「渡した側」の `*Extension` でカウントされ、root の
`End()`/`waitInflight()`（root が属する別の `*Extension` 側）はそれを待たない。
`s.rootOrSelf().extension` を経由すれば、必ず root と同じ `*Extension` の `inflight` が
更新されるため、この非対称性が起きない。テストは
`TestScope_Go_TracksAgainstRootAcrossExtensions` を参照。

## `Go` の `fn` の中から、その Scope（や祖先）の `End` を呼んではならない理由

`(*Scope).Go(fn)` が起動する goroutine には、`context.WithValue(e.rootCtx, scopeContextKey{}, s)`
で `s`（`Go` を呼んだ Scope）自身が埋め込まれた `bgCtx` が渡る。もし `fn` がこの `bgCtx` から
`ScopeFromContext` で `s`（や `s` の祖先）を取り出し、それが root であるものの `End()` を
呼ぶと、ローカルモードで自己デッドロックする: `Go` は `fn` の実行前に `inflight++` し、
`fn` の `defer` でしか `inflight--` しないため、`fn` の中で呼ばれた `End()` は
`waitInflight()` に入り、`fn` 自身の分の `inflight` が減るのを待って永久にブロックする。
`fn` が戻れないので、以後同じ `sync.Once` を待つ他の `End()` 呼び出し（handler 側の
`defer scope.End()` 等）も連鎖してブロックする。

この制約は API では強制できない（`ScopeFromContext` は公開 API で、`fn` はそれを自由に
呼べる）ため、`(*Scope).Go` と `(*Scope).End` の GoDoc に明記して事前条件として扱っている。
`Go`/`End` を変更する際は、この GoDoc の警告を消さないこと。
