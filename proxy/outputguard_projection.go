package proxy

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/codex2api/security/outputguard"
	"github.com/tidwall/gjson"
)

func extractJSONStringPayload(res gjson.Result, data []byte) []byte {
	if res.Index > 0 && len(res.Raw) >= 2 && res.Raw[0] == '"' && res.Raw[len(res.Raw)-1] == '"' {
		rawInner := data[res.Index+1 : res.Index+len(res.Raw)-1]
		if bytes.IndexByte(rawInner, '\\') == -1 {
			return rawInner
		}
	}
	return []byte(res.String())
}

type ObservationAction string

const (
	ActionAppend   ObservationAction = "append"
	ActionSnapshot ObservationAction = "snapshot"
	ActionEnd      ObservationAction = "end"
)

type Observation struct {
	ChannelID     string
	AliasID       string
	Kind          outputguard.ChannelKind
	Action        ObservationAction
	Payload       []byte
	TerminalStage string // 严格限制为 3 种合法语义终态阶段: "arguments.done", "item.done", "response.completed"，其余普通帧置空
}

type ProtocolProjector interface {
	Project(eventType string, data []byte) ([]Observation, error)
}

// -------------------------------------------------------------
// 1. ResponsesProjector (HTTP SSE, WebSocket, Realtime Text)
// -------------------------------------------------------------

type ResponsesProjector struct {
	aliasMap map[string]string
}

func NewResponsesProjector() *ResponsesProjector {
	return &ResponsesProjector{
		aliasMap: make(map[string]string),
	}
}

func (p *ResponsesProjector) Project(eventType string, data []byte) ([]Observation, error) {
	if len(data) == 0 {
		return nil, nil
	}
	t := eventType
	var parsed gjson.Result
	if t == "" {
		parsed = gjson.ParseBytes(data)
		t = parsed.Get("type").String()
	}

	var observations []Observation

	switch t {
	case "response.output_text.delta":
		res := gjson.GetManyBytes(data, "item_id", "delta", "content_index", "output_index")
		itemID := res[0].String()
		cIdx := res[2].Int()
		if itemID == "" {
			itemID = "output_" + strconv.FormatInt(res[3].Int(), 10)
		}
		cid := itemID + ":content:" + strconv.FormatInt(cIdx, 10)
		payload := extractJSONStringPayload(res[1], data)
		observations = append(observations, Observation{
			ChannelID: cid,
			AliasID:   itemID,
			Kind:      outputguard.KindText,
			Action:    ActionAppend,
			Payload:   payload,
		})

	case "response.reasoning.delta", "response.thinking.delta",
		"response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		res := gjson.GetManyBytes(data, "item_id", "delta", "content_index", "output_index", "summary_index")
		itemID := res[0].String()
		cIdx := res[2].Int()
		if itemID == "" {
			itemID = "output_" + strconv.FormatInt(res[3].Int(), 10)
		}
		cid := itemID + ":thinking:" + strconv.FormatInt(cIdx, 10)
		aliasID := itemID + ".thinking"
		// reasoning_summary_text.delta 的 part 分隔符是 summary_index（不携带 content_index），
		// 必须按 summary_index 独立分通道，且 alias 也按 part 隔离：共享 alias 会在
		// RequestGuard.resolveChannelLocked 中把后续 part 合并回首通道，使独立 part 的
		// 字节/重复检查失去意义。
		if t == "response.reasoning_summary_text.delta" {
			sIdx := strconv.FormatInt(res[4].Int(), 10)
			cid = itemID + ":thinking:summary:" + sIdx
			aliasID = itemID + ".thinking.summary." + sIdx
		}
		payload := extractJSONStringPayload(res[1], data)
		observations = append(observations, Observation{
			ChannelID: cid,
			AliasID:   aliasID,
			Kind:      outputguard.KindThinking,
			Action:    ActionAppend,
			Payload:   payload,
		})

	case "response.function_call_arguments.delta":
		res := gjson.GetManyBytes(data, "call_id", "item_id", "delta", "output_index")
		callID := res[0].String()
		itemID := res[1].String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		if targetID == "" {
			targetID = "output_" + strconv.FormatInt(res[3].Int(), 10)
		}
		payload := extractJSONStringPayload(res[2], data)
		observations = append(observations, Observation{
			ChannelID: targetID,
			AliasID:   itemID,
			Kind:      outputguard.KindToolArguments,
			Action:    ActionAppend,
			Payload:   payload,
		})

	case "response.custom_tool_call_input.delta":
		res := gjson.GetManyBytes(data, "call_id", "item_id", "delta", "output_index")
		callID := res[0].String()
		itemID := res[1].String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		if targetID == "" {
			targetID = "output_" + strconv.FormatInt(res[3].Int(), 10)
		}
		payload := extractJSONStringPayload(res[2], data)
		observations = append(observations, Observation{
			ChannelID: targetID,
			AliasID:   itemID,
			Kind:      outputguard.KindToolArguments,
			Action:    ActionAppend,
			Payload:   payload,
		})

	case "response.function_call_arguments.done":
		res := gjson.GetManyBytes(data, "call_id", "item_id", "arguments")
		callID := res[0].String()
		itemID := res[1].String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		payload := extractJSONStringPayload(res[2], data)
		observations = append(observations, Observation{
			ChannelID:     targetID,
			AliasID:       itemID,
			Kind:          outputguard.KindToolArguments,
			Action:        ActionSnapshot,
			Payload:       payload,
			TerminalStage: "arguments.done",
		})

	case "response.custom_tool_call_input.done":
		res := gjson.GetManyBytes(data, "call_id", "item_id", "input")
		callID := res[0].String()
		itemID := res[1].String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		payload := extractJSONStringPayload(res[2], data)
		observations = append(observations, Observation{
			ChannelID:     targetID,
			AliasID:       itemID,
			Kind:          outputguard.KindToolArguments,
			Action:        ActionSnapshot,
			Payload:       payload,
			TerminalStage: "arguments.done",
		})

	case "response.output_item.done":
		if !parsed.Exists() {
			parsed = gjson.ParseBytes(data)
		}
		item := parsed.Get("item")
		itemType := item.Get("type").String()
		itemID := item.Get("id").String()
		if itemID == "" {
			itemID = parsed.Get("item_id").String()
		}

		if itemType == "function_call" {
			callID := item.Get("call_id").String()
			args := item.Get("arguments").String()
			targetID := callID
			if targetID == "" {
				targetID = itemID
			}
			observations = append(observations, Observation{
				ChannelID:     targetID,
				AliasID:       itemID,
				Kind:          outputguard.KindToolArguments,
				Action:        ActionSnapshot,
				Payload:       []byte(args),
				TerminalStage: "item.done",
			})
		} else if itemType == "custom_tool_call" {
			callID := item.Get("call_id").String()
			inputVal := item.Get("input").String()
			targetID := callID
			if targetID == "" {
				targetID = itemID
			}
			observations = append(observations, Observation{
				ChannelID:     targetID,
				AliasID:       itemID,
				Kind:          outputguard.KindToolArguments,
				Action:        ActionSnapshot,
				Payload:       []byte(inputVal),
				TerminalStage: "item.done",
			})
		} else if itemType == "message" {
			contentArray := item.Get("content").Array()
			for idx, cBlock := range contentArray {
				cType := cBlock.Get("type").String()
				text := cBlock.Get("text").String()
				if cType == "output_text" || (cType == "" && text != "") {
					if text != "" {
						observations = append(observations, Observation{
							ChannelID:     fmt.Sprintf("%s:content:%d", itemID, idx),
							AliasID:       itemID,
							Kind:          outputguard.KindText,
							Action:        ActionSnapshot,
							Payload:       []byte(text),
							TerminalStage: "item.done",
						})
					}
				} else if cType == "reasoning" || cType == "thinking" {
					thinkingText := cBlock.Get("thinking").String()
					if thinkingText == "" {
						thinkingText = cBlock.Get("text").String()
					}
					if thinkingText != "" {
						observations = append(observations, Observation{
							ChannelID:     fmt.Sprintf("%s:thinking:%d", itemID, idx),
							AliasID:       fmt.Sprintf("%s.thinking", itemID),
							Kind:          outputguard.KindThinking,
							Action:        ActionSnapshot,
							Payload:       []byte(thinkingText),
							TerminalStage: "item.done",
						})
					}
				}
			}
		}

	case "response.completed", "response.incomplete":
		if !parsed.Exists() {
			parsed = gjson.ParseBytes(data)
		}
		// SSE 终态事件的 output 通常内嵌于 response；非流式完整 body 则位于顶层。
		outputs := parsed.Get("response.output")
		if !outputs.Exists() {
			outputs = parsed.Get("output")
		}
		for _, item := range outputs.Array() {
			observations = append(observations, p.projectResponsesOutputItem(item, "response.completed")...)
		}

	default:
		// 兼容非流式完整响应 JSON（completed 与 incomplete 均为语义终态）
		if !parsed.Exists() {
			parsed = gjson.ParseBytes(data)
		}
		status := parsed.Get("status").String()
		if (status == "completed" || status == "incomplete") && parsed.Get("output").Exists() {
			for _, item := range parsed.Get("output").Array() {
				observations = append(observations, p.projectResponsesOutputItem(item, "response.completed")...)
			}
		}
	}

	return observations, nil
}

func (p *ResponsesProjector) projectResponsesOutputItem(item gjson.Result, stage string) []Observation {
	var observations []Observation
	itemType := item.Get("type").String()
	itemID := item.Get("id").String()

	if itemType == "function_call" {
		callID := item.Get("call_id").String()
		args := item.Get("arguments").String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		observations = append(observations, Observation{
			ChannelID:     targetID,
			AliasID:       itemID,
			Kind:          outputguard.KindToolArguments,
			Action:        ActionSnapshot,
			Payload:       []byte(args),
			TerminalStage: stage,
		})
	} else if itemType == "custom_tool_call" {
		callID := item.Get("call_id").String()
		inputVal := item.Get("input").String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		observations = append(observations, Observation{
			ChannelID:     targetID,
			AliasID:       itemID,
			Kind:          outputguard.KindToolArguments,
			Action:        ActionSnapshot,
			Payload:       []byte(inputVal),
			TerminalStage: stage,
		})
	} else if itemType == "message" {
		contentArray := item.Get("content").Array()
		for idx, cBlock := range contentArray {
			cType := cBlock.Get("type").String()
			text := cBlock.Get("text").String()
			if cType == "output_text" || (cType == "" && text != "") {
				if text != "" {
					observations = append(observations, Observation{
						ChannelID:     fmt.Sprintf("%s:content:%d", itemID, idx),
						AliasID:       itemID,
						Kind:          outputguard.KindText,
						Action:        ActionSnapshot,
						Payload:       []byte(text),
						TerminalStage: stage,
					})
				}
			} else if cType == "reasoning" || cType == "thinking" {
				thinkingText := cBlock.Get("thinking").String()
				if thinkingText == "" {
					thinkingText = cBlock.Get("text").String()
				}
				if thinkingText != "" {
					observations = append(observations, Observation{
						ChannelID:     fmt.Sprintf("%s:thinking:%d", itemID, idx),
						AliasID:       fmt.Sprintf("%s.thinking", itemID),
						Kind:          outputguard.KindThinking,
						Action:        ActionSnapshot,
						Payload:       []byte(thinkingText),
						TerminalStage: stage,
					})
				}
			}
		}
	}
	return observations
}

// -------------------------------------------------------------
// 2. ChatProjector (OpenAI /v1/chat/completions Stream & JSON)
// -------------------------------------------------------------

type ChatProjector struct {
	aliasMap    map[string]string
	choiceTools map[int]map[string]struct{}
}

func NewChatProjector() *ChatProjector {
	return &ChatProjector{
		aliasMap:    make(map[string]string),
		choiceTools: make(map[int]map[string]struct{}),
	}
}

func (p *ChatProjector) Project(eventType string, data []byte) ([]Observation, error) {
	if len(data) == 0 {
		return nil, nil
	}
	parsed := gjson.ParseBytes(data)
	var observations []Observation

	choices := parsed.Get("choices").Array()
	for _, choice := range choices {
		choiceIdx := choice.Get("index").Int()
		finishReason := choice.Get("finish_reason").String()

		// 流式 delta
		delta := choice.Get("delta")
		if delta.Exists() {
			if content := delta.Get("content").String(); content != "" {
				observations = append(observations, Observation{
					ChannelID: fmt.Sprintf("choice_%d.text", choiceIdx),
					Kind:      outputguard.KindText,
					Action:    ActionAppend,
					Payload:   []byte(content),
				})
			}
			reasoning := delta.Get("reasoning_content").String()
			if reasoning == "" {
				reasoning = delta.Get("reasoning").String()
			}
			if reasoning != "" {
				observations = append(observations, Observation{
					ChannelID: fmt.Sprintf("choice_%d.thinking", choiceIdx),
					Kind:      outputguard.KindThinking,
					Action:    ActionAppend,
					Payload:   []byte(reasoning),
				})
			}

			// 标准 tool_calls
			toolCalls := delta.Get("tool_calls").Array()
			for _, tc := range toolCalls {
				toolIdx := int(tc.Get("index").Int())
				callID := tc.Get("id").String()
				channelID := fmt.Sprintf("choice_%d.tool_%d", choiceIdx, toolIdx)
				if callID != "" {
					p.aliasMap[callID] = channelID
				}
				if p.choiceTools[int(choiceIdx)] == nil {
					p.choiceTools[int(choiceIdx)] = make(map[string]struct{})
				}
				p.choiceTools[int(choiceIdx)][channelID] = struct{}{}
				args := tc.Get("function.arguments").String()
				if args != "" {
					observations = append(observations, Observation{
						ChannelID: channelID,
						AliasID:   callID,
						Kind:      outputguard.KindToolArguments,
						Action:    ActionAppend,
						Payload:   []byte(args),
					})
				}
			}

			// 兼容 legacy delta.function_call (Spec §4.1)
			if fnCall := delta.Get("function_call"); fnCall.Exists() {
				args := fnCall.Get("arguments").String()
				channelID := fmt.Sprintf("choice_%d.function_call", choiceIdx)
				if args != "" {
					observations = append(observations, Observation{
						ChannelID: channelID,
						Kind:      outputguard.KindToolArguments,
						Action:    ActionAppend,
						Payload:   []byte(args),
					})
				}
			}
		}

		// 非流式 message
		msg := choice.Get("message")
		if msg.Exists() {
			if content := msg.Get("content").String(); content != "" {
				observations = append(observations, Observation{
					ChannelID:     fmt.Sprintf("choice_%d.text", choiceIdx),
					Kind:          outputguard.KindText,
					Action:        ActionSnapshot,
					Payload:       []byte(content),
					TerminalStage: "response.completed",
				})
			}
			reasoning := msg.Get("reasoning_content").String()
			if reasoning == "" {
				reasoning = msg.Get("reasoning").String()
			}
			if reasoning != "" {
				observations = append(observations, Observation{
					ChannelID:     fmt.Sprintf("choice_%d.thinking", choiceIdx),
					Kind:          outputguard.KindThinking,
					Action:        ActionSnapshot,
					Payload:       []byte(reasoning),
					TerminalStage: "response.completed",
				})
			}

			// 标准 tool_calls
			toolCalls := msg.Get("tool_calls").Array()
			for idx, tc := range toolCalls {
				callID := tc.Get("id").String()
				channelID := fmt.Sprintf("choice_%d.tool_%d", choiceIdx, idx)
				args := tc.Get("function.arguments").String()
				observations = append(observations, Observation{
					ChannelID:     channelID,
					AliasID:       callID,
					Kind:          outputguard.KindToolArguments,
					Action:        ActionSnapshot,
					Payload:       []byte(args),
					TerminalStage: "response.completed",
				})
			}

			// 兼容 legacy message.function_call (Spec §4.1)
			if fnCall := msg.Get("function_call"); fnCall.Exists() {
				args := fnCall.Get("arguments").String()
				channelID := fmt.Sprintf("choice_%d.function_call", choiceIdx)
				observations = append(observations, Observation{
					ChannelID:     channelID,
					Kind:          outputguard.KindToolArguments,
					Action:        ActionSnapshot,
					Payload:       []byte(args),
					TerminalStage: "response.completed",
				})
			}
		}

		// finish_reason 尾检触发 ActionEnd
		if finishReason != "" {
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("choice_%d.text", choiceIdx),
				Kind:      outputguard.KindText,
				Action:    ActionEnd,
			})
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("choice_%d.thinking", choiceIdx),
				Kind:      outputguard.KindThinking,
				Action:    ActionEnd,
			})
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("choice_%d.function_call", choiceIdx),
				Kind:      outputguard.KindToolArguments,
				Action:    ActionEnd,
			})
			if tools, exists := p.choiceTools[int(choiceIdx)]; exists {
				for toolKey := range tools {
					observations = append(observations, Observation{
						ChannelID: toolKey,
						Kind:      outputguard.KindToolArguments,
						Action:    ActionEnd,
					})
				}
			}
		}
	}

	return observations, nil
}

// -------------------------------------------------------------
// 3. ObserveEvent Dispatcher (单上游事件严格计 1 次，流水线批调度)
// -------------------------------------------------------------

func ObserveEvent(guard *outputguard.RequestGuard, projector ProtocolProjector, eventType string, data []byte) error {
	// 心跳/注释帧直接放行，不计入业务事件预算
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte(":")) || eventType == "ping" {
		return nil
	}

	// 步骤 1: 严格每上游业务事件计 1 次 requestEvents（即使是 control/unknown 帧）
	if err := guard.RecordEvent(); err != nil {
		return err
	}

	obs, err := projector.Project(eventType, data)
	if err != nil {
		return err
	}
	if len(obs) == 0 {
		return nil
	}

	// 依次将 observations 推入 guard，执行判定流水线
	// 单 event 所有 obs 检查通过再允许 consumer，一 obs 命中整 event 不可见
	for _, o := range obs {
		switch o.Action {
		case ActionAppend:
			if err := guard.ObserveAppendNoEvent(o.ChannelID, o.AliasID, o.Kind, o.Payload); err != nil {
				return err
			}
		case ActionSnapshot:
			if err := guard.ObserveSnapshotNoEvent(o.ChannelID, o.AliasID, o.Kind, o.Payload, o.TerminalStage); err != nil {
				return err
			}
		case ActionEnd:
			// ActionEnd 目标解析：当 Observation 携带 AliasID（如 tool call_id）时优先使用；
			// 依赖 RequestGuard.aliasMap 内部双向索引反查至对应 primary channel，保持解耦契约。
			target := o.ChannelID
			if o.AliasID != "" {
				target = o.AliasID
			}
			if err := guard.EndChannel(target); err != nil {
				return err
			}
		}
	}

	return nil
}
