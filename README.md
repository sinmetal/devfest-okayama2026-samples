# DevFest Okayama 2026 - ADK Go (`google/adk-go`) で作る AI アシスタント

[Harappa80/devfest-okayama-ai-assistant](https://github.com/Harappa80/devfest-okayama-ai-assistant)（Python / FastAPI / `google-adk`）と同じ AI コンパニオン Web アプリケーションを、**[Agent Development Kit (ADK) for Go v2](https://github.com/google/adk-go)**（`google.golang.org/adk/v2`）と Go 標準ライブラリ `net/http` で実装した完成版サンプルです。

- **会話エージェント**: `google.golang.org/adk/v2/agent/llmagent`（モデル: `gemini-3.8-flash`）
- **Google 検索グラウンディング**: `google.golang.org/adk/v2/tool/geminitool`（`geminitool.GoogleSearch{}`）
- **音声読み上げ（TTS）**: `google.golang.org/genai`（モデル: `gemini-3.8-flash-tts` / WAV 出力）
- **アバター表示・リップシンク**: 音声再生に合わせた口パクアニメーション・出典リンク表示

---

## ファイル構成

| ファイル・フォルダー | 役割 |
| :--- | :--- |
| `main.go` | Web サーバー（`GET /`, `GET /config`, `POST /chat`, `POST /tts`, `/static/*`）、入力検証、Gemini TTS 呼び出し |
| `companion_agent.go` | ADK `Runner` の初期化、リクエストごとの `InMemoryService` セッション生成・会話履歴リプレイ・破棄 |
| `character.go` | ADK `llmagent`（猫のキャラクター「わく」）および `geminitool.GoogleSearch{}` ツールの定義 |
| `main_test.go` | Web API・バリデーション・TTS・ADC 認証・エラーサニタイズの単体テスト |
| `companion_agent_test.go` | ADK エージェント設定・履歴リプレイ・Grounding 抽出の単体テスト |
| `.env.template` | 環境変数設定ファイルのひな形 |
| `templates/index.html` | アプリの画面 HTML |
| `static/app.js` | メッセージ送信、返答表示、音声読み上げ、口のアニメーション制御 |
| `static/style.css` | スタイルシート |
| `static/images/` | キャラクターのアバター画像（口を閉じた画像・開いた画像） |
| `docs/implementation_plan.md` | Python 版からの移植設計・実装計画書 |

---

## セットアップと起動方法

### 1. 前提条件

- Go 1.26.10 以上
- 課金および Vertex AI (Agent Platform) API が有効化された Google Cloud プロジェクト
- Application Default Credentials (ADC) の設定（Cloud Shell または `gcloud auth application-default login`）

### 2. `.env` の作成

`.env.template` をコピーして `.env` を作成し、`your-project-id` を自分の Google Cloud プロジェクト ID に置き換えます。

```bash
cp .env.template .env
sed -i.bak 's/^GOOGLE_CLOUD_PROJECT=.*/GOOGLE_CLOUD_PROJECT=your-project-id/' .env && rm -f .env.bak
chmod 600 .env
```

カスタム音声（`voice_...`）を使う場合は、`.env` の `GEMINI_TTS_VOICE` と `GEMINI_TTS_VOICE_NAME` を設定してください。

### 3. サーバーの起動

```bash
go run .
```

起動すると `http://127.0.0.1:8000` で待ち受けます（`PORT` 環境変数でポート変更可能）。ブラウザーまたは Cloud Shell の「ウェブでプレビュー（ポート 8000）」から開いてください。

---

## キャラクターやツールのカスタマイズ

`character.go` の `characterInstruction` や `NewCharacterAgent` を編集することで、性格・口調・利用するツールを自由に変更できます。

```go
func NewCharacterAgent(m model.LLM) (agent.Agent, error) {
	return llmagent.New(llmagent.Config{
		Name:        "companion_agent",
		Model:       m,
		Instruction: characterInstruction,
		Tools: []tool.Tool{
			geminitool.GoogleSearch{},
		},
		GenerateContentConfig: &genai.GenerateContentConfig{
			MaxOutputTokens: 2048,
			ThinkingConfig: &genai.ThinkingConfig{
				ThinkingLevel: genai.ThinkingLevelLow,
			},
		},
	})
}
```

---

## テストの実行

### Go 単体テスト & フロントエンド単体テスト

```bash
go test -v ./...
node tests/test_ui.cjs
```

### Playwright ブラウザ E2E テスト

サーバー（`go run .`）を起動した状態で実行します。

```bash
npm ci
npx playwright install chromium
npm run test:e2e
# 実際の Vertex AI / Gemini を使う検証（モデル利用料金が発生）
LIVE_GEMINI=1 npm run test:e2e
```
