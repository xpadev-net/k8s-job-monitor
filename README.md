# Kubernetes Job Monitor

Kubernetes Job Monitor は、Kubernetes クラスター内の Job リソースの状態を監視し、Job の成功または失敗を Discord Webhook を通じて通知するカスタムコントローラーです。

## 機能

- Kubernetes Job リソースの監視
- Job の成功時に Discord に通知
- Job の失敗時に Discord に通知
- コンテナ化されており、Kubernetes クラスター内で簡単に実行可能

## インストール

### 前提条件

- Kubernetes クラスター
- kubectl がインストールされ、クラスターにアクセス可能
- Discord ウェブフック URL（通知を受け取るチャンネル用）

### Discord Webhook の設定

1. Discord サーバーで、通知を受け取りたいテキストチャンネルを開く
2. チャンネル名の横にある歯車アイコン（チャンネル設定）をクリック
3. 「連携サービス」を選択
4. 「ウェブフック」をクリック
5. 「新しいウェブフック」を選択
6. ウェブフックに名前を付け、アイコンを選択（任意）
7. 「ウェブフックURLをコピー」をクリック
8. コピーした URL を Kubernetes Job Monitor の `--webhook-url` パラメータに使用

## 設定オプション

コントローラーは以下のコマンドラインオプションをサポートしています：

- `--webhook-url`: Discord Webhook URL（必須）
- `--kubeconfig`: クラスター外で実行する場合の kubeconfig へのパス
- `--master`: Kubernetes API サーバーのアドレス（kubeconfig の値を上書き）

## 使用例

### テスト用の Job を作成

以下のサンプル Job マニフェストを使用して、通知をテストできます：

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: test-job-success
spec:
  template:
    spec:
      containers:
      - name: test
        image: busybox
        command: ["sh", "-c", "echo 'Job completed successfully!' && exit 0"]
      restartPolicy: Never
  backoffLimit: 1
```

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: test-job-failure
spec:
  template:
    spec:
      containers:
      - name: test
        image: busybox
        command: ["sh", "-c", "echo 'Job failed!' && exit 1"]
      restartPolicy: Never
  backoffLimit: 1
```

### 通知の確認

上記のテスト Job を適用すると、Discord チャンネルに以下のような通知が送信されます：

- Job 成功時: 緑色のメッセージで成功通知
- Job 失敗時: 赤色のメッセージで失敗通知

## ライセンス

MIT License
