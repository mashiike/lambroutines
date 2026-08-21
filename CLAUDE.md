# CLAUDE.md

このファイルは Claude Code (claude.ai/code) がこのリポジトリで作業する際の指針です。

## プロジェクト概要

`lambroutines` は、AWS Lambda の handler から素の `go fn()` と同じ感覚でバックグラウンド処理を起動しつつ、その処理が完了するまで実行環境がフリーズしないことを保証する Go パッケージ。internal Lambda extension を登録し、handler 完了 + `Go()` で起動した処理の完了の両方を確認してから extension 自身の `/next` 呼び出しを行うことで実現している。Lambda 実行環境外(`fujiwara/lamblocal` 配下や `go run` 等)ではローカルモードにフォールバックする。

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

- `(*Extension).Go(fn)` は**呼ばれた瞬間に即座に goroutine を起動する**(呼び出し側から見て素の `go fn()` と同じ見た目)。「handler 完了まで開始を遅延する」ような変更は設計の核を破壊する
- ローカルモードとLambda実行時で `Go()`/`Wrap()`/`ExtensionFromContext` の外部から見える挙動は同一に保つ(内部の待ち合わせ方法だけが異なる)
- `handlerCompleted()` と `inflight` の待ち合わせに monotonic counter (`completions`/`inflight` + `sync.Cond`) を使っている理由、ローカルモードで `Go()` 自体を分岐させない理由は `.claude/rules/completion-tracking.md` を参照。変更前に必ず読むこと
- `examples/main.go` の handler 内の sleep は実機での動作検証用の意図的なコードで、削除・変更禁止。理由は `.claude/rules/example-handler-timing.md` を参照

## テスト方針

- `go test -race` を必ず通す(このパッケージは goroutine 完了待ち合わせがコアロジックであり、race 検出が最重要の回帰チェック)
- ローカルモード / Lambda 実行時モードの両方に対応するテストを追加する(`lambroutines_test.go` の `newTestExtension` / `newLocalTestExtension` ヘルパー参照)
