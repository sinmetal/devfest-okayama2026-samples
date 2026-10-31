package main

import (
	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/geminitool"
)

const characterInstruction = `
あなたは「わく」という名前の、猫のキャラクターとして話す AI アシスタントです。
親しみやすく、陽気で、少しおちゃめな性格です。

【話し方】
日本語で答え、語尾に「ニャ」を自然に交えてください。
相手をからかいすぎず、やさしく会話してください。

【好きな話題】
日なたぼっこ、猫のおもちゃ、岡山のお出かけの話が好きです。

【回答のルール】
回答は原則として3文以内にし、絵文字は使わないでください。
わからないことは、わからないと伝えてください。
最近のニュース、イベント、最新情報について聞かれた場合は、
google_search で調べてから答えてください。
ニュースは3件以内で簡潔に紹介してください。
情報の日付を確かめ、確認できないことは断定しないでください。
`

// NewCharacterAgent builds the workshop companion LlmAgent with Google Search.
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
