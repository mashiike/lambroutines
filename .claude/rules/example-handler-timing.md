---
paths:
  - "examples/main.go"
---

# handler 内の 1 秒 sleep は検証用の意図的なコード（削除禁止）

## 背景 / 罠の説明

`handle()` は `lambroutines.Go(ctx, ...)` の呼び出し直後に `time.Sleep(1 * time.Second)` を入れている。
これは実装として必要な処理ではなく、`Go()` の実行タイミングを実機の Billed Duration で
検証するための仕掛け。

`Go()` は呼ばれた瞬間に即座に goroutine を起動する設計（handler の完了を待たずに開始する）
だが、ログの出力順序（`[bg] background task started` が `[handler] returning response` より
先に出るか）は goroutine スケジューリング次第で確定せず、証拠にならない。

handler に `Go()` の後にも測定可能な作業（1 秒 sleep）を入れることで:

- `Go()` が即時実行なら、background task（3 秒 sleep）は handler の 1 秒 sleep と並行して
  進むため、Billed Duration は概ね background task の長さ（約 3 秒）になる
- `Go()` が handler 完了まで開始を遅延する実装（過去に実際に混入したバグ）だと、直列に
  実行されるため Billed Duration は概ね両者の合計（約 4 秒）になる

この 1 秒 sleep を削除すると、実機デプロイでこの区別ができなくなる。

## チェックリスト

- この handler の sleep 時間や background task の sleep 時間を変更する場合は、
  「Billed Duration が直列なら合計、並行なら概ね長い方に近づく」という関係を保ったまま
  変更する。
