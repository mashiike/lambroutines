# CLAUDE.md

このファイルは Claude Code (claude.ai/code) がこのリポジトリで作業する際の指針です。

## プロジェクト概要

`lambroutines` は、AWS Lambda の handler から素の `go fn()` と同じ感覚でバックグラウンド処理を起動しつつ、その処理が完了するまで実行環境がフリーズしないことを保証する Go パッケージ。internal Lambda extension を登録し、invocation ごとに開始される `Scope` の終了 + その `Scope` 経由で `Go()` を呼んで起動した処理の完了の両方を確認してから extension 自身の `/next` 呼び出しを行うことで実現している。`Wrap` は `lambda.Handler` を受け取るハンドラ向けに `Scope` の開始・終了を自動化する薄いラッパーで、`fujiwara/ridge` のように `lambda.Start` を内部で直接呼んでしまう統合には `StartScope` を直接使う。Lambda 実行環境外(`fujiwara/lamblocal` 配下や `go run` 等)ではローカルモードにフォールバックする。

詳細は [README.md](README.md) / [README.ja.md](README.ja.md) を参照。

## 言語ポリシー

- **対話セッション(このファイルを含む)は日本語。**
- **コミットメッセージ・PRのタイトルと本文は英語。**
- **`README.md` は英語、`README.ja.md` は日本語。** 両ファイルを常に同期させる(片方だけ更新して終わらない)。
- コード中のコメント・GoDoc は英語(既存コードに準拠)。

## 開発コマンド

```sh
go build ./...
go vet ./...
gofmt -l .            # 差分が出たら gofmt -w . で整形すること。空出力が正
go test -race ./...
```

lint は `golangci-lint`(CI と同一設定)を使う。ローカルに無ければ `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1 run` 等で実行するか、CI の結果を確認する。

`examples/` は独立した Taskfile(`examples/Taskfile.yaml`)と aqua(`examples/aqua.yaml`)でツール管理された、lambroll によるデプロイ可能な実例。ルートの `go test`/`go build` の対象には含まれるが、デプロイ操作(`task deploy` 等)はユーザーの承認を得てから実行する。

## CI / リリース

- `.github/workflows/ci.yml`: push/PR で `go build` / `go vet` / `gofmt` チェック / `go test -race` と `golangci-lint` を実行
- `.github/workflows/tagpr.yml`: [Songmu/tagpr](https://github.com/Songmu/tagpr) による リリース PR の自動作成・タグ付け。`main` へのマージが起点
- `.github/dependabot.yml`: `gomod` / `github-actions` の依存更新
- GitHub Actions の `uses:` はすべて commit SHA 固定(`# vX.Y.Z` コメント付き)。dependabot がこのコメント込みで更新する前提を崩さない

## 設計上の重要な制約

- `(*Scope).Go(fn)` は**呼ばれた瞬間に即座に goroutine を起動する**(呼び出し側から見て素の `go fn()` と同じ見た目)。「invocation 完了まで開始を遅延する」ような変更は設計の核を破壊する
- ローカルモードとLambda実行時で `Go()`/`Wrap()`/`StartScope()`/`ScopeFromContext` の外部から見える挙動は同一に保つ(内部の待ち合わせ方法だけが異なる)
- nested `Scope`(既に `Scope` を含む `ctx` から `StartScope` を呼んだ場合)は root `Scope` の `inflight` に合算されるだけで、独立した完了通知を持たない。`End()` は root でのみ意味を持ち、冪等(`sync.Once`)。この非対称性を「nested にも通知させる」方向へ変更しない
- `WithScopeTimeout` は `Scope` が**開始**されるまでの猶予であり、`Scope` の**終了**(`completions`)を待つ側にタイムアウトを付けてはならない。終了側に付けると、猶予時間を超える正常な handler で誤警告・恒久的なズレが起きる(設計中に実際にこの誤りを作った実績がある)。理由・待ち合わせの詳細は `.claude/rules/completion-tracking.md` を参照。変更前に必ず読むこと
- `examples/main.go` の handler 内の sleep は実機での動作検証用の意図的なコードで、削除・変更禁止。理由は `.claude/rules/example-handler-timing.md` を参照

## テスト方針

- `go test -race` を必ず通す(このパッケージは goroutine 完了待ち合わせがコアロジックであり、race 検出が最重要の回帰チェック)
- ローカルモード / Lambda 実行時モードの両方に対応するテストを追加する(`lambroutines_test.go` の `newTestExtension` / `newLocalTestExtension` ヘルパー参照)
